package updater

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// SignaturePrefix is prepended to encoded Ed25519 keys and signatures.
const SignaturePrefix = "ed25519:"

// ReleaseMeta describes metadata stored alongside a release binary.
type ReleaseMeta struct {
	Version      string `json:"version"`
	Commit       string `json:"commit,omitempty"`
	ShortSHA     string `json:"short_sha,omitempty"`
	BuiltAt      string `json:"built_at,omitempty"`
	Arch         string `json:"arch,omitempty"`
	BinarySHA256 string `json:"binary_sha256,omitempty"`
	Signed       bool   `json:"signed,omitempty"`
	Note         string `json:"note,omitempty"`
}

// VerifiedArtifact holds the validated paths and metadata for an extracted update.
type VerifiedArtifact struct {
	Version      string
	BinaryPath   string
	BinarySHA256 string
	SumsPath     string
	MetaPath     string
	Meta         *ReleaseMeta
	SigPath      string
	Signed       bool
}

// GenerateKeypair generates a fresh Ed25519 keypair encoded with the "ed25519:" prefix.
func GenerateKeypair() (pubEncoded, privEncoded string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return SignaturePrefix + base64.StdEncoding.EncodeToString(pub),
		SignaturePrefix + base64.StdEncoding.EncodeToString(priv),
		nil
}

func decodeKeyBytes(raw string, wantLens ...int) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("empty key or signature")
	}
	s = strings.TrimPrefix(s, SignaturePrefix)
	s = strings.TrimSpace(s)

	matchesWant := func(n int) bool {
		for _, w := range wantLens {
			if n == w {
				return true
			}
		}
		return false
	}

	var wrongLen int
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		hex.DecodeString,
	} {
		if b, err := dec(s); err == nil {
			if len(wantLens) == 0 || matchesWant(len(b)) {
				return b, nil
			}
			wrongLen = len(b)
		}
	}
	if wrongLen > 0 {
		return nil, fmt.Errorf("unexpected decoded length %d bytes", wrongLen)
	}
	return nil, errors.New("invalid base64 or hex encoding")
}

// ParsePublicKey decodes an Ed25519 public key (32 bytes).
func ParsePublicKey(raw string) (ed25519.PublicKey, error) {
	b, err := decodeKeyBytes(raw, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	return ed25519.PublicKey(b), nil
}

// ParsePrivateKey decodes an Ed25519 private key (64 bytes) or 32-byte seed.
func ParsePrivateKey(raw string) (ed25519.PrivateKey, error) {
	b, err := decodeKeyBytes(raw, ed25519.SeedSize, ed25519.PrivateKeySize)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	switch len(b) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	default:
		return nil, fmt.Errorf("private key: expected %d or %d bytes, got %d", ed25519.SeedSize, ed25519.PrivateKeySize, len(b))
	}
}

// SignBytes signs data with priv and returns an "ed25519:<base64>" string.
func SignBytes(priv ed25519.PrivateKey, data []byte) string {
	sig := ed25519.Sign(priv, data)
	return SignaturePrefix + base64.StdEncoding.EncodeToString(sig)
}

// VerifyBytes checks sigText against data using pub.
func VerifyBytes(pub ed25519.PublicKey, data []byte, sigText string) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("invalid public key")
	}
	sig, err := decodeKeyBytes(sigText, ed25519.SignatureSize)
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	if !ed25519.Verify(pub, data, sig) {
		return errors.New("ed25519 signature verification failed")
	}
	return nil
}

// SHA256File computes the lowercase hex SHA-256 digest of path.
func SHA256File(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ParseSHA256SUMS parses standard sha256sum output mapping base filename -> lowercase hex.
func ParseSHA256SUMS(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		digest := strings.ToLower(fields[0])
		name := strings.TrimPrefix(fields[1], "*")
		name = path.Base(strings.ReplaceAll(name, "\\", "/"))
		if digest != "" && name != "" {
			out[name] = digest
		}
	}
	return out
}

// VerifyExtractedArtifact validates binary SHA-256 against SHA256SUMS and RELEASE_META,
// and verifies SIGNATURE over SHA256SUMS when public key / requirement are configured.
func VerifyExtractedArtifact(found map[string]string, binaryName string, pub ed25519.PublicKey, requireSig bool) (VerifiedArtifact, error) {
	if requireSig && len(pub) != ed25519.PublicKeySize {
		return VerifiedArtifact{}, errors.New("signature verification required, but no valid public_key is configured")
	}

	binPath := found[binaryName]
	if binPath == "" && binaryName != "gatehouse" {
		binPath = found["gatehouse"]
	}
	if binPath == "" {
		return VerifiedArtifact{}, fmt.Errorf("artifact missing %q binary", binaryName)
	}
	sumsPath := found["SHA256SUMS"]
	if sumsPath == "" {
		return VerifiedArtifact{}, errors.New("artifact missing SHA256SUMS")
	}

	sumsData, err := os.ReadFile(sumsPath)
	if err != nil {
		return VerifiedArtifact{}, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	sums := ParseSHA256SUMS(sumsData)
	expectedSHA := sums[binaryName]
	if expectedSHA == "" {
		expectedSHA = sums["gatehouse"]
	}
	if expectedSHA == "" {
		return VerifiedArtifact{}, fmt.Errorf("SHA256SUMS missing entry for %q", binaryName)
	}

	actualSHA, err := SHA256File(binPath)
	if err != nil {
		return VerifiedArtifact{}, fmt.Errorf("hash binary: %w", err)
	}
	if actualSHA != expectedSHA {
		return VerifiedArtifact{}, fmt.Errorf("binary SHA256 mismatch against SHA256SUMS: expected %s, got %s", expectedSHA, actualSHA)
	}

	var meta *ReleaseMeta
	metaPath := found["RELEASE_META"]
	if metaPath != "" {
		rawMeta, err := os.ReadFile(metaPath)
		if err != nil {
			return VerifiedArtifact{}, fmt.Errorf("read RELEASE_META: %w", err)
		}
		var m ReleaseMeta
		if err := json.Unmarshal(rawMeta, &m); err != nil {
			return VerifiedArtifact{}, fmt.Errorf("parse RELEASE_META: %w", err)
		}
		if m.BinarySHA256 != "" && !strings.EqualFold(m.BinarySHA256, actualSHA) {
			return VerifiedArtifact{}, fmt.Errorf("binary SHA256 mismatch against RELEASE_META: expected %s, got %s", strings.ToLower(m.BinarySHA256), actualSHA)
		}
		meta = &m
	}

	sigPath := found["SIGNATURE"]
	signed := false
	if sigPath != "" {
		sigData, err := os.ReadFile(sigPath)
		if err != nil {
			return VerifiedArtifact{}, fmt.Errorf("read SIGNATURE: %w", err)
		}
		if len(pub) == ed25519.PublicKeySize {
			if err := VerifyBytes(pub, sumsData, string(sigData)); err != nil {
				return VerifiedArtifact{}, err
			}
			signed = true
		}
	} else if requireSig {
		return VerifiedArtifact{}, errors.New("artifact missing SIGNATURE file while signature verification is required")
	}

	ver := ""
	if meta != nil {
		ver = strings.TrimSpace(meta.Version)
	}
	return VerifiedArtifact{
		Version:      ver,
		BinaryPath:   binPath,
		BinarySHA256: actualSHA,
		SumsPath:     sumsPath,
		MetaPath:     metaPath,
		Meta:         meta,
		SigPath:      sigPath,
		Signed:       signed,
	}, nil
}
