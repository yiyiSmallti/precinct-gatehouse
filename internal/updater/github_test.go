package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
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
