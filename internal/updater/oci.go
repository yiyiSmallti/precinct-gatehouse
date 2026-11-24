package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const acceptManifests = "application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json"

var authParamRe = regexp.MustCompile(`([A-Za-z0-9_]+)="([^"]*)"`)

// ParseImageRef splits a registry image reference into (registry, repository, tag).
func ParseImageRef(image, defaultTag string) (registry, repo, tag string, err error) {
	s := strings.TrimSpace(image)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	slash := strings.IndexByte(s, '/')
	if slash <= 0 {
		return "", "", "", fmt.Errorf("image reference must include registry host and repository: %q", image)
	}
	registry = s[:slash]
	repo = s[slash+1:]
	if defaultTag == "" {
		defaultTag = "latest"
	}
	tag = defaultTag
	if at := strings.IndexByte(repo, '@'); at >= 0 {
		tag = repo[at+1:]
		repo = repo[:at]
	} else {
		lastSlash := strings.LastIndexByte(repo, '/')
		tail := repo[lastSlash+1:]
		if colon := strings.LastIndexByte(tail, ':'); colon >= 0 {
			embedded := tail[colon+1:]
			repo = repo[:lastSlash+1+colon]
			if defaultTag == "latest" && embedded != "" {
				tag = embedded
			}
		}
	}
	repo = strings.Trim(repo, "/")
	if registry == "" || repo == "" {
		return "", "", "", fmt.Errorf("invalid image reference: %q", image)
	}
	return registry, repo, tag, nil
}

// ParseWWWAuthenticate parses a Bearer or Basic Www-Authenticate challenge header.
func ParseWWWAuthenticate(header string) (scheme string, params map[string]string) {
	s := strings.TrimSpace(header)
	if s == "" {
		return "", nil
	}
	parts := strings.SplitN(s, " ", 2)
	scheme = strings.ToLower(strings.TrimSpace(parts[0]))
	params = map[string]string{}
	if len(parts) > 1 {
		for _, m := range authParamRe.FindAllStringSubmatch(parts[1], -1) {
			params[strings.ToLower(m[1])] = m[2]
		}
	}
	return scheme, params
}

// OCISource pulls Gatehouse artifact images from an OCI / Docker Registry v2.
type OCISource struct {
	Image        string
	Tag          string
	Username     string
	Password     string
	InsecureHTTP bool
	HTTPClient   *http.Client
	PubKey       ed25519.PublicKey
	RequireSig   bool
	TargetOS     string
	TargetArch   string

	tokMu  sync.Mutex
	tokens map[string]string
}

type ociManifest struct {
	SchemaVersion int              `json:"schemaVersion"`
	MediaType     string           `json:"mediaType,omitempty"`
	Config        *ociDescriptor   `json:"config,omitempty"`
	Layers        []ociDescriptor  `json:"layers,omitempty"`
	Manifests     []ociManifestRef `json:"manifests,omitempty"`
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType,omitempty"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ociManifestRef struct {
	MediaType   string            `json:"mediaType,omitempty"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

type ociImageConfig struct {
	Created string `json:"created,omitempty"`
	Config  struct {
		Labels map[string]string `json:"Labels,omitempty"`
	} `json:"config"`
}

func (o *OCISource) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return NewHTTPClient(60*time.Second, nil)
}

func (o *OCISource) scheme() string {
	if o.InsecureHTTP {
		return "http"
	}
	return "https"
}

func (o *OCISource) targetPlatform() (string, string) {
	osName := o.TargetOS
	if osName == "" {
		osName = runtime.GOOS
	}
	arch := o.TargetArch
	if arch == "" {
		arch = runtime.GOARCH
	}
	return osName, arch
}

func (o *OCISource) basicHeader() string {
	if o.Username == "" && o.Password == "" {
		return ""
	}
	raw := base64.StdEncoding.EncodeToString([]byte(o.Username + ":" + o.Password))
	return "Basic " + raw
}

func (o *OCISource) fetchBearerToken(ctx context.Context, params map[string]string, defaultScope string) (string, error) {
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("bearer challenge missing realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("invalid bearer realm %q: %w", realm, err)
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	scope := params["scope"]
	if scope == "" {
		scope = defaultScope
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "precinct-gatehouse-updater/1.0")
	if b := o.basicHeader(); b != "" {
		req.Header.Set("Authorization", b)
	}

	resp, err := o.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("token endpoint HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	tok := payload.Token
	if tok == "" {
		tok = payload.AccessToken
	}
	if tok == "" {
		return "", errors.New("token endpoint did not return token or access_token")
	}
	return tok, nil
}

func (o *OCISource) doRegistryRequest(ctx context.Context, registry, repo, path string, headers map[string]string) (*http.Response, error) {
	fullURL := fmt.Sprintf("%s://%s%s", o.scheme(), registry, path)
	scope := "repository:" + repo + ":pull"

	buildReq := func(auth string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "precinct-gatehouse-updater/1.0")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return req, nil
	}

	o.tokMu.Lock()
	cachedAuth := o.tokens[scope]
	o.tokMu.Unlock()

	req, err := buildReq(cachedAuth)
	if err != nil {
		return nil, err
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challenge := resp.Header.Get("Www-Authenticate")
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()

	scheme, params := ParseWWWAuthenticate(challenge)
	var authHeader string
	switch scheme {
	case "bearer":
		tok, err := o.fetchBearerToken(ctx, params, scope)
		if err != nil {
			return nil, err
		}
		authHeader = "Bearer " + tok
	case "basic":
		authHeader = o.basicHeader()
		if authHeader == "" {
			return nil, errors.New("registry requires Basic authentication credentials")
		}
	default:
		return nil, fmt.Errorf("unsupported registry 401 Www-Authenticate challenge: %q", challenge)
	}

	o.tokMu.Lock()
	if o.tokens == nil {
		o.tokens = map[string]string{}
	}
	o.tokens[scope] = authHeader
	o.tokMu.Unlock()

	retryReq, err := buildReq(authHeader)
	if err != nil {
		return nil, err
	}
	return o.client().Do(retryReq)
}

func (o *OCISource) fetchManifest(ctx context.Context, registry, repo, ref string) (*ociManifest, string, error) {
	escaped := url.PathEscape(ref)
	// Preserve ":" and "@" inside digest or tag references.
	escaped = strings.ReplaceAll(strings.ReplaceAll(escaped, "%3A", ":"), "%40", "@")
	p := fmt.Sprintf("/v2/%s/manifests/%s", repo, escaped)

	resp, err := o.doRegistryRequest(ctx, registry, repo, p, map[string]string{"Accept": acceptManifests})
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("manifest %s HTTP %d: %s", ref, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	digest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		sum := sha256.Sum256(raw)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	var doc ociManifest
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", fmt.Errorf("parse manifest %s: %w", ref, err)
	}
	return &doc, digest, nil
}

func (o *OCISource) resolveTargetManifest(ctx context.Context, registry, repo, tag string) (*ociManifest, string, error) {
	doc, digest, err := o.fetchManifest(ctx, registry, repo, tag)
	if err != nil {
		return nil, "", err
	}
	if len(doc.Manifests) == 0 {
		return doc, digest, nil
	}
	targetOS, targetArch := o.targetPlatform()
	for _, item := range doc.Manifests {
		if item.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
			continue
		}
		if item.Platform != nil && item.Platform.OS == targetOS && item.Platform.Architecture == targetArch && item.Digest != "" {
			return o.fetchManifest(ctx, registry, repo, item.Digest)
		}
	}
	return nil, "", fmt.Errorf("no %s/%s manifest found in index for tag %q", targetOS, targetArch, tag)
}

func (o *OCISource) fetchConfigLabels(ctx context.Context, registry, repo string, doc *ociManifest) ociImageConfig {
	var out ociImageConfig
	if doc == nil || doc.Config == nil || !strings.HasPrefix(doc.Config.Digest, "sha256:") || doc.Config.Size > maxMetaSize {
		return out
	}
	p := fmt.Sprintf("/v2/%s/blobs/%s", repo, doc.Config.Digest)
	resp, err := o.doRegistryRequest(ctx, registry, repo, p, nil)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetaSize))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (o *OCISource) downloadBlob(ctx context.Context, registry, repo, digest, destPath string) error {
	if !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("unsupported blob digest algorithm: %q", digest)
	}
	expected := strings.ToLower(strings.TrimPrefix(digest, "sha256:"))
	p := fmt.Sprintf("/v2/%s/blobs/%s", repo, digest)
	resp, err := o.doRegistryRequest(ctx, registry, repo, p, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("blob %s HTTP %d: %s", digest, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("blob digest mismatch for %s: expected %s, got %s", digest, expected, actual)
	}
	return nil
}

// Check queries the remote OCI manifest digest and optional OCI config labels.
func (o *OCISource) Check(ctx context.Context) (*RemoteInfo, error) {
	registry, repo, tag, err := ParseImageRef(o.Image, o.Tag)
	if err != nil {
		return nil, err
	}
	doc, digest, err := o.resolveTargetManifest(ctx, registry, repo, tag)
	if err != nil {
		return nil, err
	}
	cfg := o.fetchConfigLabels(ctx, registry, repo, doc)
	ver := strings.TrimSpace(cfg.Config.Labels["org.opencontainers.image.version"])
	rev := strings.TrimSpace(cfg.Config.Labels["org.opencontainers.image.revision"])
	created := strings.TrimSpace(cfg.Config.Labels["org.opencontainers.image.created"])
	if created == "" {
		created = cfg.Created
	}
	return &RemoteInfo{
		Source:      "oci",
		Version:     ver,
		Digest:      digest,
		Tag:         tag,
		Commit:      rev,
		PublishedAt: created,
		URL:         fmt.Sprintf("%s/%s:%s", registry, repo, tag),
	}, nil
}

// Download pulls and extracts all layers from the target OCI artifact image,
// then verifies SHA-256 checksums and optional Ed25519 signature.
func (o *OCISource) Download(ctx context.Context, outDir string) (*DownloadedArtifact, error) {
	registry, repo, tag, err := ParseImageRef(o.Image, o.Tag)
	if err != nil {
		return nil, err
	}
	doc, digest, err := o.resolveTargetManifest(ctx, registry, repo, tag)
	if err != nil {
		return nil, err
	}
	if len(doc.Layers) == 0 {
		return nil, fmt.Errorf("manifest %s contains no layers", digest)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
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

	found := map[string]string{}
	for idx, layer := range doc.Layers {
		tmpBlob := filepath.Join(outDir, fmt.Sprintf(".blob-%d.tar", idx))
		err := o.downloadBlob(ctx, registry, repo, layer.Digest, tmpBlob)
		if err != nil {
			_ = os.Remove(tmpBlob)
			return nil, err
		}
		extracted, exErr := ExtractArtifactTar(tmpBlob, outDir, wanted)
		_ = os.Remove(tmpBlob)
		if exErr != nil {
			return nil, exErr
		}
		for k, v := range extracted {
			found[k] = v
		}
	}

	verified, err := VerifyExtractedArtifact(found, binName, o.PubKey, o.RequireSig)
	if err != nil {
		return nil, err
	}
	ver := verified.Version
	if ver == "" || ver == "latest" {
		short := strings.TrimPrefix(digest, "sha256:")
		if len(short) > 12 {
			short = short[:12]
		}
		ver = tag + "-" + short
	}
	var commit, builtAt string
	if verified.Meta != nil {
		commit = verified.Meta.Commit
		builtAt = verified.Meta.BuiltAt
	}
	return &DownloadedArtifact{
		Version:  ver,
		Verified: verified,
		Remote: RemoteInfo{
			Source:      "oci",
			Version:     ver,
			Digest:      digest,
			Tag:         tag,
			Commit:      commit,
			PublishedAt: builtAt,
			Signed:      verified.Signed,
			URL:         fmt.Sprintf("%s/%s:%s", registry, repo, tag),
		},
	}, nil
}
