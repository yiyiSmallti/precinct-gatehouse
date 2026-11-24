package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestGitHubSourceCheckAndDownload(t *testing.T) {
	pubStr, privStr, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := ParsePublicKey(pubStr)
	priv, _ := ParsePrivateKey(privStr)

	binName := binaryFilename()
	binBytes := []byte("gatehouse-gh-v0.3.0")
	binSum := sha256.Sum256(binBytes)
	binHex := hex.EncodeToString(binSum[:])
	sumsBytes := []byte(binHex + "  " + binName + "\n")
	sigBytes := []byte(SignBytes(priv, sumsBytes) + "\n")
	metaBytes, _ := json.Marshal(ReleaseMeta{Version: "v0.3.0", BinarySHA256: binHex, Signed: true})

	// Deliberately omit SIGNATURE inside tarball to test standalone asset fallback!
	tarBytes := buildTestTar(t, true, map[string][]byte{
		binName:        binBytes,
		"SHA256SUMS":   sumsBytes,
		"RELEASE_META": metaBytes,
	})
	assetName := fmt.Sprintf("gatehouse_v0.3.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	var s3SawAuth bool
	cdnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			s3SawAuth = true
			http.Error(w, "leaked github token", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(tarBytes)
	}))
	defer cdnSrv.Close()

	var apiSrv *httptest.Server
	apiSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-secret" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/yiyiSmallti/precinct-gatehouse/releases/latest":
			_ = json.NewEncoder(w).Encode(ghRelease{
				TagName:     "v0.3.0",
				PublishedAt: "2026-11-24T09:00:00Z",
				Body:        "Release notes",
				HTMLURL:     "https://github.com/yiyiSmallti/precinct-gatehouse/releases/tag/v0.3.0",
				Assets: []ghAsset{
					{
						ID:                 101,
						Name:               assetName,
						URL:                apiSrv.URL + "/assets/tarball",
						BrowserDownloadURL: cdnSrv.URL + "/" + assetName,
					},
					{
						ID:                 102,
						Name:               "SIGNATURE",
						URL:                apiSrv.URL + "/assets/SIGNATURE",
						BrowserDownloadURL: apiSrv.URL + "/dl/SIGNATURE",
					},
				},
			})
		case "/assets/tarball":
			http.Redirect(w, r, cdnSrv.URL+"/redirected", http.StatusFound)
		case "/assets/SIGNATURE":
			_, _ = w.Write(sigBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiSrv.Close()

	src := &GitHubSource{
		Repo:       "yiyiSmallti/precinct-gatehouse",
		APIURL:     apiSrv.URL,
		Token:      "gh-secret",
		HTTPClient: NewHTTPClient(0, nil),
		PubKey:     pub,
		RequireSig: true,
	}

	remote, err := src.Check(context.Background())
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if remote.Version != "v0.3.0" || !remote.Signed || remote.AssetName != assetName {
		t.Fatalf("unexpected remote: %+v", remote)
	}

	art, err := src.Download(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if s3SawAuth {
		t.Fatal("GitHub token leaked across redirect")
	}
	if art.Version != "v0.3.0" || !art.Verified.Signed || art.Verified.BinarySHA256 != binHex {
		t.Fatalf("unexpected downloaded artifact: %+v", art)
	}
}

func TestGitHubSourceExplicitTag(t *testing.T) {
	binName := binaryFilename()
	buildTarFor := func(ver string) ([]byte, string) {
		binBytes := []byte("gatehouse-gh-" + ver)
		binSum := sha256.Sum256(binBytes)
		binHex := hex.EncodeToString(binSum[:])
		sumsBytes := []byte(binHex + "  " + binName + "\n")
		metaBytes, _ := json.Marshal(ReleaseMeta{Version: ver, BinarySHA256: binHex})
		return buildTestTar(t, true, map[string][]byte{
			binName:        binBytes,
			"SHA256SUMS":   sumsBytes,
			"RELEASE_META": metaBytes,
		}), binHex
	}

	tar012, hex012 := buildTarFor("v0.1.2")
	tarLatest, _ := buildTarFor("v0.9.9")

	var hitPaths []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPaths = append(hitPaths, r.URL.Path)
		switch r.URL.Path {
		case "/repos/yiyiSmallti/precinct-gatehouse/releases/tags/v0.1.2":
			_ = json.NewEncoder(w).Encode(ghRelease{
				TagName: "v0.1.2",
				Assets: []ghAsset{{
					ID:                 201,
					Name:               fmt.Sprintf("gatehouse_v0.1.2_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
					BrowserDownloadURL: srv.URL + "/dl/v0.1.2.tar.gz",
				}},
			})
		case "/repos/yiyiSmallti/precinct-gatehouse/releases/latest":
			_ = json.NewEncoder(w).Encode(ghRelease{
				TagName: "v0.9.9",
				Assets: []ghAsset{{
					ID:                 299,
					Name:               fmt.Sprintf("gatehouse_v0.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
					BrowserDownloadURL: srv.URL + "/dl/v0.9.9.tar.gz",
				}},
			})
		case "/dl/v0.1.2.tar.gz":
			_, _ = w.Write(tar012)
		case "/dl/v0.9.9.tar.gz":
			_, _ = w.Write(tarLatest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// 1. Explicit tag on GitHubSource hits /releases/tags/v0.1.2, not /releases/latest.
	src := &GitHubSource{
		Repo:       "yiyiSmallti/precinct-gatehouse",
		Tag:        "v0.1.2",
		APIURL:     srv.URL,
		HTTPClient: NewHTTPClient(0, nil),
	}
	art, err := src.Download(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("explicit tag Download failed: %v", err)
	}
	if art.Version != "v0.1.2" || art.Verified.BinarySHA256 != hex012 {
		t.Fatalf("expected v0.1.2 artifact, got %+v", art)
	}

	// 2. Manager.buildSource propagates tagOverride "v0.1.2" and treats "latest" as default latest lookup.
	mgr := NewManager(Deps{
		CurrentVersion: "v0.1.0",
		StaticConfig: &config.Updater{
			Source:       "github",
			GitHubRepo:   "yiyiSmallti/precinct-gatehouse",
			GitHubAPIURL: srv.URL,
		},
	})
	builtExplicit, err := mgr.buildSource(mgr.currentConfig(), "v0.1.2")
	if err != nil {
		t.Fatalf("buildSource(v0.1.2) failed: %v", err)
	}
	gsExplicit, ok := builtExplicit.(*GitHubSource)
	if !ok || gsExplicit.Tag != "v0.1.2" {
		t.Fatalf("expected GitHubSource with Tag=v0.1.2, got %+v", builtExplicit)
	}

	builtLatest, err := mgr.buildSource(mgr.currentConfig(), "latest")
	if err != nil {
		t.Fatalf("buildSource(latest) failed: %v", err)
	}
	gsLatest := builtLatest.(*GitHubSource)
	if gsLatest.Tag != "" {
		t.Fatalf("expected GitHubSource Tag='' for latest override, got %q", gsLatest.Tag)
	}
	remLatest, err := gsLatest.Check(context.Background())
	if err != nil {
		t.Fatalf("gsLatest.Check failed: %v", err)
	}
	if remLatest.Version != "v0.9.9" {
		t.Fatalf("expected v0.9.9 for latest, got %q", remLatest.Version)
	}
}

func TestGitHubSourceRejectsMismatchedAssetDigest(t *testing.T) {
	binName := binaryFilename()
	binBytes := []byte("gatehouse-gh-digest-test")
	binSum := sha256.Sum256(binBytes)
	binHex := hex.EncodeToString(binSum[:])
	sumsBytes := []byte(binHex + "  " + binName + "\n")
	metaBytes, _ := json.Marshal(ReleaseMeta{Version: "v0.4.0", BinarySHA256: binHex})
	tarBytes := buildTestTar(t, true, map[string][]byte{
		binName:        binBytes,
		"SHA256SUMS":   sumsBytes,
		"RELEASE_META": metaBytes,
	})
	tarSum := sha256.Sum256(tarBytes)
	tarDigest := "sha256:" + hex.EncodeToString(tarSum[:])
	assetName := fmt.Sprintf("gatehouse_v0.4.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	var currentDigest string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/yiyiSmallti/precinct-gatehouse/releases/latest":
			_ = json.NewEncoder(w).Encode(ghRelease{
				TagName: "v0.4.0",
				Assets: []ghAsset{{
					ID:                 301,
					Name:               assetName,
					Digest:             currentDigest,
					BrowserDownloadURL: srv.URL + "/dl/" + assetName,
				}},
			})
		case "/dl/" + assetName:
			_, _ = w.Write(tarBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := &GitHubSource{
		Repo:       "yiyiSmallti/precinct-gatehouse",
		APIURL:     srv.URL,
		HTTPClient: NewHTTPClient(0, nil),
	}

	// 1. Mismatched sha256 digest advertised by API must fail before extraction.
	currentDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	out1 := t.TempDir()
	if _, err := src.Download(context.Background(), out1); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected asset digest mismatch error, got: %v", err)
	}
	if entries, _ := os.ReadDir(out1); len(entries) != 0 {
		t.Fatalf("expected clean outDir after digest mismatch, got %d entries", len(entries))
	}

	// 2. Unsupported digest algorithm must fail cleanly.
	currentDigest = "sha512:deadbeef"
	out2 := t.TempDir()
	if _, err := src.Download(context.Background(), out2); err == nil || !strings.Contains(err.Error(), "unsupported digest algorithm") {
		t.Fatalf("expected unsupported digest algorithm error, got: %v", err)
	}

	// 3. Matching sha256 digest succeeds and reports the digest.
	currentDigest = tarDigest
	art, err := src.Download(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Download with matching asset digest failed: %v", err)
	}
	if art.Version != "v0.4.0" || art.Remote.Digest != tarDigest {
		t.Fatalf("unexpected artifact with matching asset digest: %+v", art)
	}
}
