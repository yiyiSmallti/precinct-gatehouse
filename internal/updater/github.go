package updater

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const defaultGitHubAPIURL = "https://api.github.com"

// GitHubSource checks and downloads Gatehouse release tarballs from GitHub Releases.
type GitHubSource struct {
	Repo       string
	APIURL     string
	Token      string
	Prerelease bool
	HTTPClient *http.Client
	PubKey     ed25519.PublicKey
	RequireSig bool
	TargetOS   string
	TargetArch string
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt string    `json:"published_at"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Assets      []ghAsset `json:"assets"`
}

type ghAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest,omitempty"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func (g *GitHubSource) client() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return NewHTTPClient(60*time.Second, nil)
}

func (g *GitHubSource) apiBase() string {
	b := strings.TrimRight(strings.TrimSpace(g.APIURL), "/")
	if b == "" {
		return defaultGitHubAPIURL
	}
	return b
}

func (g *GitHubSource) targetPlatform() (string, string) {
	osName := g.TargetOS
	if osName == "" {
		osName = runtime.GOOS
	}
	arch := g.TargetArch
	if arch == "" {
		arch = runtime.GOARCH
	}
	return osName, arch
}

func (g *GitHubSource) doGet(ctx context.Context, rawURL, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "precinct-gatehouse-updater/1.0")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if strings.TrimSpace(g.Token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(g.Token))
	}
	return g.client().Do(req)
}

func (g *GitHubSource) fetchRelease(ctx context.Context) (*ghRelease, error) {
	repo := strings.TrimSpace(g.Repo)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("invalid github_repo %q", g.Repo)
	}
	base := g.apiBase()
	if !g.Prerelease {
		u := fmt.Sprintf("%s/repos/%s/%s/releases/latest", base, owner, name)
		resp, err := g.doGet(ctx, u, "application/vnd.github+json")
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("github releases/latest HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var rel ghRelease
		if err := json.Unmarshal(body, &rel); err != nil {
			return nil, err
		}
		return &rel, nil
	}

	u := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=10", base, owner, name)
	resp, err := g.doGet(ctx, u, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github releases list HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list []ghRelease
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	for i := range list {
		if !list[i].Draft {
			return &list[i], nil
		}
	}
	return nil, errors.New("no published GitHub releases found")
}

func (g *GitHubSource) findAssets(rel *ghRelease) (tarball *ghAsset, extras map[string]*ghAsset) {
	osName, arch := g.targetPlatform()
	exact := fmt.Sprintf("gatehouse_%s_%s_%s.tar.gz", rel.TagName, osName, arch)
	suffix := fmt.Sprintf("_%s_%s.tar.gz", osName, arch)
	extras = map[string]*ghAsset{}

	for i := range rel.Assets {
		a := &rel.Assets[i]
		switch a.Name {
		case exact:
			tarball = a
		case "SHA256SUMS", "RELEASE_META", "SIGNATURE":
			extras[a.Name] = a
		default:
			if tarball == nil && strings.HasPrefix(a.Name, "gatehouse_") && strings.HasSuffix(a.Name, suffix) {
				tarball = a
			}
		}
	}
	return tarball, extras
}

func (g *GitHubSource) downloadAsset(ctx context.Context, asset *ghAsset, destPath string, maxSize int64) error {
	dlURL := asset.BrowserDownloadURL
	accept := ""
	if strings.TrimSpace(g.Token) != "" && asset.URL != "" {
		dlURL = asset.URL
		accept = "application/octet-stream"
	} else if dlURL == "" {
		dlURL = asset.URL
		accept = "application/octet-stream"
	}
	if dlURL == "" {
		return fmt.Errorf("asset %q has no download URL", asset.Name)
	}

	resp, err := g.doGet(ctx, dlURL, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download asset %s HTTP %d: %s", asset.Name, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxSize+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxSize {
		return fmt.Errorf("asset %s exceeds maximum size (%d)", asset.Name, maxSize)
	}
	return nil
}

// Check queries GitHub Releases for the latest matching release metadata.
func (g *GitHubSource) Check(ctx context.Context) (*RemoteInfo, error) {
	rel, err := g.fetchRelease(ctx)
	if err != nil {
		return nil, err
	}
	tarball, extras := g.findAssets(rel)
	if tarball == nil {
		osName, arch := g.targetPlatform()
		return nil, fmt.Errorf("release %s has no tarball asset for %s/%s", rel.TagName, osName, arch)
	}
	digest := strings.TrimSpace(tarball.Digest)
	if digest == "" {
		digest = fmt.Sprintf("github-asset:%d", tarball.ID)
	}
	return &RemoteInfo{
		Source:      "github",
		Version:     strings.TrimSpace(rel.TagName),
		Digest:      digest,
		Tag:         strings.TrimSpace(rel.TagName),
		PublishedAt: rel.PublishedAt,
		Notes:       rel.Body,
		URL:         rel.HTMLURL,
		Signed:      extras["SIGNATURE"] != nil,
		AssetName:   tarball.Name,
	}, nil
}

// Download fetches and extracts the matching GitHub Release tarball into outDir
// and verifies SHA-256 and optional Ed25519 signature.
func (g *GitHubSource) Download(ctx context.Context, outDir string) (*DownloadedArtifact, error) {
	rel, err := g.fetchRelease(ctx)
	if err != nil {
		return nil, err
	}
	tarball, extras := g.findAssets(rel)
	if tarball == nil {
		osName, arch := g.targetPlatform()
		return nil, fmt.Errorf("release %s has no tarball asset for %s/%s", rel.TagName, osName, arch)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	tarPath := filepath.Join(outDir, ".release.tar.gz")
	if err := g.downloadAsset(ctx, tarball, tarPath, maxBinarySize); err != nil {
		_ = os.Remove(tarPath)
		return nil, err
	}
	binName := binaryFilename()
	wanted := map[string]os.FileMode{
		binName:        0o755,
		"gatehouse":    0o755,
		"SHA256SUMS":   0o644,
		"RELEASE_META": 0o644,
		"SIGNATURE":    0o644,
	}
	found, exErr := ExtractArtifactTar(tarPath, outDir, wanted)
	_ = os.Remove(tarPath)
	if exErr != nil {
		return nil, exErr
	}

	// Fetch standalone metadata/signature assets if they were not bundled inside the tarball.
	for _, name := range []string{"SHA256SUMS", "RELEASE_META", "SIGNATURE"} {
		if found[name] == "" && extras[name] != nil {
			dst := filepath.Join(outDir, name)
			if err := g.downloadAsset(ctx, extras[name], dst, maxMetaSize); err != nil {
				return nil, err
			}
			found[name] = dst
		}
	}

	verified, err := VerifyExtractedArtifact(found, binName, g.PubKey, g.RequireSig)
	if err != nil {
		return nil, err
	}
	ver := verified.Version
	if ver == "" {
		ver = strings.TrimSpace(rel.TagName)
	}
	digest := strings.TrimSpace(tarball.Digest)
	if digest == "" {
		digest = fmt.Sprintf("github-asset:%d", tarball.ID)
	}
	var commit string
	if verified.Meta != nil {
		commit = verified.Meta.Commit
	}
	return &DownloadedArtifact{
		Version:  ver,
		Verified: verified,
		Remote: RemoteInfo{
			Source:      "github",
			Version:     ver,
			Digest:      digest,
			Tag:         strings.TrimSpace(rel.TagName),
			PublishedAt: rel.PublishedAt,
			Notes:       rel.Body,
			URL:         rel.HTMLURL,
			Commit:      commit,
			Signed:      verified.Signed,
			AssetName:   tarball.Name,
		},
	}, nil
}
