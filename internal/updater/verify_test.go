package updater

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestKeypairSignVerify(t *testing.T) {
	pubStr, privStr, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	pub, err := ParsePublicKey(pubStr)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	priv, err := ParsePrivateKey(privStr)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}

	// Verify seed-only and hex decoding also work.
	seedHex := hex.EncodeToString(priv.Seed())
	privFromSeed, err := ParsePrivateKey(seedHex)
	if err != nil {
		t.Fatalf("ParsePrivateKey(seed hex): %v", err)
	}
	pubRaw := base64.RawStdEncoding.EncodeToString(pub)
	pubParsed, err := ParsePublicKey(pubRaw)
	if err != nil {
		t.Fatalf("ParsePublicKey(raw b64): %v", err)
	}

	msg := []byte("abc123  gatehouse\n")
	sig := SignBytes(privFromSeed, msg)
	if err := VerifyBytes(pubParsed, msg, sig); err != nil {
		t.Fatalf("VerifyBytes failed: %v", err)
	}
	if err := VerifyBytes(pubParsed, []byte("tampered"), sig); err == nil {
		t.Fatal("expected VerifyBytes to fail on tampered data")
	}

	otherPub, _, _ := ed25519.GenerateKey(nil)
	if err := VerifyBytes(otherPub, msg, sig); err == nil {
		t.Fatal("expected VerifyBytes to fail with wrong public key")
	}
}

func TestVerifyExtractedArtifact(t *testing.T) {
	pubStr, privStr, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := ParsePublicKey(pubStr)
	priv, _ := ParsePrivateKey(privStr)

	makeBundle := func(t *testing.T, withSig bool) map[string]string {
		t.Helper()
		dir := t.TempDir()
		binName := binaryFilename()
		binPath := filepath.Join(dir, binName)
		content := []byte("binary-payload-v0.1.2")
		if err := os.WriteFile(binPath, content, 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		shaHex := hex.EncodeToString(sum[:])

		sumsData := []byte(shaHex + "  " + binName + "\n")
		sumsPath := filepath.Join(dir, "SHA256SUMS")
		if err := os.WriteFile(sumsPath, sumsData, 0o644); err != nil {
			t.Fatal(err)
		}

		metaBytes, _ := json.Marshal(ReleaseMeta{Version: "v0.1.2", BinarySHA256: shaHex, Signed: withSig})
		metaPath := filepath.Join(dir, "RELEASE_META")
		if err := os.WriteFile(metaPath, metaBytes, 0o644); err != nil {
			t.Fatal(err)
		}

		found := map[string]string{
			binName:        binPath,
			"SHA256SUMS":   sumsPath,
			"RELEASE_META": metaPath,
		}
		if withSig {
			sigPath := filepath.Join(dir, "SIGNATURE")
			if err := os.WriteFile(sigPath, []byte(SignBytes(priv, sumsData)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			found["SIGNATURE"] = sigPath
		}
		return found
	}

	t.Run("signed_valid", func(t *testing.T) {
		found := makeBundle(t, true)
		res, err := VerifyExtractedArtifact(found, binaryFilename(), pub, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.Signed || res.Version != "v0.1.2" {
			t.Fatalf("unexpected result: %+v", res)
		}
	})

	t.Run("missing_sig_required_fails", func(t *testing.T) {
		found := makeBundle(t, false)
		if _, err := VerifyExtractedArtifact(found, binaryFilename(), pub, true); err == nil {
			t.Fatal("expected error when signature is missing and required")
		}
	})

	t.Run("missing_sig_optional_ok", func(t *testing.T) {
		found := makeBundle(t, false)
		res, err := VerifyExtractedArtifact(found, binaryFilename(), pub, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Signed {
			t.Fatal("expected Signed=false")
		}
	})

	t.Run("require_sig_without_pubkey_fails_closed", func(t *testing.T) {
		found := makeBundle(t, true)
		if _, err := VerifyExtractedArtifact(found, binaryFilename(), nil, true); err == nil {
			t.Fatal("expected error when requireSig=true and pubkey is nil")
		}
	})

	t.Run("tampered_binary_rejected", func(t *testing.T) {
		found := makeBundle(t, true)
		if err := os.WriteFile(found[binaryFilename()], []byte("evil-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyExtractedArtifact(found, binaryFilename(), pub, true); err == nil {
			t.Fatal("expected SHA-256 mismatch error")
		}
	})
}
