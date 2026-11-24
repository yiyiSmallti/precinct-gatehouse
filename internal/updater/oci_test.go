package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func buildTestTar(t *testing.T, useGzip bool, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var tw *tar.Writer
	var gw *gzip.Writer
	if useGzip {
		gw = gzip.NewWriter(&buf)
		tw = tar.NewWriter(gw)
	} else {
		tw = tar.NewWriter(&buf)
	}
	for name, data := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(data)),
		}
		if name == "gatehouse" || name == "gatehouse.exe" {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gw != nil {
		if err := gw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestParseImageRefAndAuthChallenge(t *testing.T) {
	reg, repo, tag, err := ParseImageRef("crpi.example.com/smallti/gatehouse:v0.1.2", "latest")
	if err != nil || reg != "crpi.example.com" || repo != "smallti/gatehouse" || tag != "v0.1.2" {
		t.Fatalf("ParseImageRef got (%q,%q,%q,%v)", reg, repo, tag, err)
	}
	reg, repo, tag, err = ParseImageRef("crpi.example.com/smallti/gatehouse:v0.1.2", "v0.1.3")
	if err != nil || tag != "v0.1.3" {
		t.Fatalf("explicit non-latest tag override failed: %q %v", tag, err)
	}
	scheme, params := ParseWWWAuthenticate(`Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:a/b:pull"`)
	if scheme != "bearer" || params["service"] != "registry.example.com" || params["realm"] != "https://auth.example.com/token" {
		t.Fatalf("ParseWWWAuthenticate got (%q,%v)", scheme, params)
	}
}

func TestExtractArtifactTarRejectsTraversalAndSymlink(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("bad"))
	_ = tw.Close()
	travPath := filepath.Join(dir, "trav.tar")
	_ = os.WriteFile(travPath, buf.Bytes(), 0o644)
	if _, err := ExtractArtifactTar(travPath, dir, map[string]os.FileMode{"gatehouse": 0o755}); err == nil {
		t.Fatal("expected traversal tar to fail")
	}

	buf.Reset()
	tw = tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "gatehouse", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.Close()
	symPath := filepath.Join(dir, "sym.tar")
	_ = os.WriteFile(symPath, buf.Bytes(), 0o644)
	if _, err := ExtractArtifactTar(symPath, dir, map[string]os.FileMode{"gatehouse": 0o755}); err == nil {
		t.Fatal("expected symlink tar member to be rejected")
	}
}

func TestOCISourceCheckAndDownloadWithRedirectAndSignature(t *testing.T) {
	pubStr, privStr, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := ParsePublicKey(pubStr)
	priv, _ := ParsePrivateKey(privStr)

	binName := binaryFilename()
	binBytes := []byte("#!/bin/sh\necho gatehouse v0.2.0\n")
	binSum := sha256.Sum256(binBytes)
	binHex := hex.EncodeToString(binSum[:])
	sumsBytes := []byte(binHex + "  " + binName + "\n")
	sigBytes := []byte(SignBytes(priv, sumsBytes) + "\n")
	metaBytes, _ := json.Marshal(ReleaseMeta{
		Version:      "v0.2.0",
		Commit:       "deadbeef",
		BinarySHA256: binHex,
		Signed:       true,
	})

	layerBytes := buildTestTar(t, true, map[string][]byte{
		"./" + binName: binBytes,
		"SHA256SUMS":   sumsBytes,
		"RELEASE_META": metaBytes,
		"SIGNATURE":    sigBytes,
	})
	layerSum := sha256.Sum256(layerBytes)
	layerDigest := "sha256:" + hex.EncodeToString(layerSum[:])

	cfgBytes, _ := json.Marshal(map[string]any{
		"created": "2026-11-24T08:00:00Z",
		"config": map[string]any{
			"Labels": map[string]string{
				"org.opencontainers.image.version":  "v0.2.0",
				"org.opencontainers.image.revision": "deadbeef",
				"org.opencontainers.image.created":  "2026-11-24T08:00:00Z",
			},
		},
	})
	cfgSum := sha256.Sum256(cfgBytes)
	cfgDigest := "sha256:" + hex.EncodeToString(cfgSum[:])

	childManifestBytes, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    cfgDigest,
			"size":      len(cfgBytes),
		},
		"layers": []map[string]any{
			{
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
				"digest":    layerDigest,
				"size":      len(layerBytes),
			},
		},
	})
	childSum := sha256.Sum256(childManifestBytes)
	childDigest := "sha256:" + hex.EncodeToString(childSum[:])

	indexBytes, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []map[string]any{
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
				"annotations": map[string]string{
					"vnd.docker.reference.type": "attestation-manifest",
				},
			},
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    childDigest,
				"platform": map[string]string{
					"os":           runtime.GOOS,
					"architecture": runtime.GOARCH,
				},
			},
		},
	})

	// Separate OSS server that fails if Authorization header leaks across redirect.
	var ossSawAuth bool
	ossSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			ossSawAuth = true
			http.Error(w, "leaked auth header", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(layerBytes)
	}))
	defer ossSrv.Close()

	var regSrv *httptest.Server
	regSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			user, pass, ok := r.BasicAuth()
			if !ok || user != "aliuser" || pass != "alipass" {
				http.Error(w, "bad basic auth", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-123"})
		case "/v2/smallti/gatehouse/manifests/latest":
			if r.Header.Get("Authorization") != "Bearer tok-123" {
				w.Header().Set("Www-Authenticate", `Bearer realm="`+regSrv.URL+`/token",service="reg",scope="repository:smallti/gatehouse:pull"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:indexdigest")
			_, _ = w.Write(indexBytes)
		case "/v2/smallti/gatehouse/manifests/" + childDigest:
			w.Header().Set("Docker-Content-Digest", childDigest)
			_, _ = w.Write(childManifestBytes)
		case "/v2/smallti/gatehouse/blobs/" + cfgDigest:
			_, _ = w.Write(cfgBytes)
		case "/v2/smallti/gatehouse/blobs/" + layerDigest:
			http.Redirect(w, r, ossSrv.URL+"/signed-blob", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	defer regSrv.Close()

	host := strings.TrimPrefix(regSrv.URL, "http://")
	src := &OCISource{
		Image:        host + "/smallti/gatehouse",
		Tag:          "latest",
		Username:     "aliuser",
		Password:     "alipass",
		InsecureHTTP: true,
		HTTPClient:   NewHTTPClient(0, nil),
		PubKey:       pub,
		RequireSig:   true,
	}

	remote, err := src.Check(context.Background())
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if remote.Version != "v0.2.0" || remote.Digest != childDigest || remote.Commit != "deadbeef" {
		t.Fatalf("unexpected remote info: %+v", remote)
	}

	outDir := t.TempDir()
	art, err := src.Download(context.Background(), outDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if ossSawAuth {
		t.Fatal("Authorization header leaked across cross-origin redirect to OSS")
	}
	if art.Version != "v0.2.0" || !art.Verified.Signed || art.Verified.BinarySHA256 != binHex {
		t.Fatalf("unexpected downloaded artifact: %+v", art)
	}
}

func TestOCISourceRejectsMismatchedPinnedManifestDigest(t *testing.T) {
	goodManifest := []byte(`{"schemaVersion":2,"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size":10}]}`)
	goodSum := sha256.Sum256(goodManifest)
	expectedDigest := "sha256:" + hex.EncodeToString(goodSum[:])

	tamperedManifest := []byte(`{"schemaVersion":2,"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","size":10}]}`)

	indexBytes, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []map[string]any{
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    expectedDigest,
				"platform": map[string]string{
					"os":           runtime.GOOS,
					"architecture": runtime.GOARCH,
				},
			},
		},
	})

	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/smallti/gatehouse/manifests/" + expectedDigest:
			// Even with a spoofed Docker-Content-Digest header matching expectedDigest,
			// tampered manifest bytes must be rejected.
			w.Header().Set("Docker-Content-Digest", expectedDigest)
			_, _ = w.Write(tamperedManifest)
		case "/v2/smallti/gatehouse/manifests/latest":
			_, _ = w.Write(indexBytes)
		case "/v2/smallti/gatehouse/manifests/sha512:deadbeef":
			_, _ = w.Write(tamperedManifest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer regSrv.Close()

	host := strings.TrimPrefix(regSrv.URL, "http://")

	// 1. Direct pinned @sha256:... image ref mismatch must fail Check and Download.
	pinned := &OCISource{
		Image:        host + "/smallti/gatehouse@" + expectedDigest,
		InsecureHTTP: true,
		HTTPClient:   NewHTTPClient(0, nil),
	}
	if _, err := pinned.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "manifest digest mismatch") {
		t.Fatalf("expected manifest digest mismatch on pinned Check, got: %v", err)
	}
	if _, err := pinned.Download(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "manifest digest mismatch") {
		t.Fatalf("expected manifest digest mismatch on pinned Download, got: %v", err)
	}

	// 2. Unsupported digest algorithm must be rejected.
	badAlgo := &OCISource{
		Image:        host + "/smallti/gatehouse@sha512:deadbeef",
		InsecureHTTP: true,
		HTTPClient:   NewHTTPClient(0, nil),
	}
	if _, err := badAlgo.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported manifest digest algorithm") {
		t.Fatalf("expected unsupported manifest digest algorithm error, got: %v", err)
	}

	// 3. Tag index pointing to child manifest digest must also reject tampered child bytes.
	tagged := &OCISource{
		Image:        host + "/smallti/gatehouse:latest",
		InsecureHTTP: true,
		HTTPClient:   NewHTTPClient(0, nil),
	}
	if _, err := tagged.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "manifest digest mismatch") {
		t.Fatalf("expected manifest digest mismatch on index child Check, got: %v", err)
	}
	if _, err := tagged.Download(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "manifest digest mismatch") {
		t.Fatalf("expected manifest digest mismatch on index child Download, got: %v", err)
	}
}

func TestOCISourceRejectsOversizedLayerBeforeExhaustingDisk(t *testing.T) {
	smallPayload := bytes.Repeat([]byte("a"), 256)
	smallSum := sha256.Sum256(smallPayload)
	smallDigest := "sha256:" + hex.EncodeToString(smallSum[:])

	oversizedPayload := bytes.Repeat([]byte("b"), 4096)
	oversizedSum := sha256.Sum256(oversizedPayload)
	oversizedDigest := "sha256:" + hex.EncodeToString(oversizedSum[:])

	mkManifest := func(layerDigest string, layerSize int64) []byte {
		b, _ := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"layers": []map[string]any{
				{
					"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
					"digest":    layerDigest,
					"size":      layerSize,
				},
			},
		})
		return b
	}

	var blobHits atomic.Int32
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/smallti/gatehouse/manifests/declared-too-big":
			_, _ = w.Write(mkManifest(smallDigest, 2048))
		case "/v2/smallti/gatehouse/manifests/stream-too-big":
			_, _ = w.Write(mkManifest(oversizedDigest, 0))
		case "/v2/smallti/gatehouse/manifests/size-mismatch":
			_, _ = w.Write(mkManifest(smallDigest, 256))
		case "/v2/smallti/gatehouse/blobs/" + smallDigest:
			blobHits.Add(1)
			_, _ = w.Write(bytes.Repeat([]byte("c"), 1024))
		case "/v2/smallti/gatehouse/blobs/" + oversizedDigest:
			blobHits.Add(1)
			_, _ = w.Write(oversizedPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer regSrv.Close()

	host := strings.TrimPrefix(regSrv.URL, "http://")

	// 1. Declared size exceeding MaxLayerSize fails before even fetching the blob.
	out1 := t.TempDir()
	src1 := &OCISource{
		Image:        host + "/smallti/gatehouse:declared-too-big",
		InsecureHTTP: true,
		HTTPClient:   NewHTTPClient(0, nil),
		MaxLayerSize: 1024,
	}
	if _, err := src1.Download(context.Background(), out1); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected declared oversized layer error, got: %v", err)
	}
	if blobHits.Load() != 0 {
		t.Fatalf("expected 0 blob HTTP hits when manifest declared size exceeds limit, got %d", blobHits.Load())
	}
	if entries, _ := os.ReadDir(out1); len(entries) != 0 {
		t.Fatalf("expected no temp blobs left behind in %s, got %d entries", out1, len(entries))
	}

	// 2. Omitted manifest size with streaming body exceeding MaxLayerSize stops bounded copy cleanly.
	out2 := t.TempDir()
	src2 := &OCISource{
		Image:        host + "/smallti/gatehouse:stream-too-big",
		InsecureHTTP: true,
		HTTPClient:   NewHTTPClient(0, nil),
		MaxLayerSize: 1024,
	}
	if _, err := src2.Download(context.Background(), out2); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected streamed oversized layer error, got: %v", err)
	}
	if entries, _ := os.ReadDir(out2); len(entries) != 0 {
		t.Fatalf("expected no temp blobs left behind in %s, got %d entries", out2, len(entries))
	}

	// 3. Body exceeding declared manifest size (within MaxLayerSize) rejects size mismatch.
	out3 := t.TempDir()
	src3 := &OCISource{
		Image:        host + "/smallti/gatehouse:size-mismatch",
		InsecureHTTP: true,
		HTTPClient:   NewHTTPClient(0, nil),
		MaxLayerSize: 4096,
	}
	if _, err := src3.Download(context.Background(), out3); err == nil || (!strings.Contains(err.Error(), "size mismatch") && !strings.Contains(err.Error(), "does not match manifest size")) {
		t.Fatalf("expected layer size mismatch error, got: %v", err)
	}
	if entries, _ := os.ReadDir(out3); len(entries) != 0 {
		t.Fatalf("expected no temp blobs left behind in %s, got %d entries", out3, len(entries))
	}
}
