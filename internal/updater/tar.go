package updater

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxBinarySize int64 = 256 << 20
	maxMetaSize   int64 = 2 << 20
)

func normalizeTarMemberName(raw string) (string, error) {
	if strings.ContainsRune(raw, '\x00') {
		return "", fmt.Errorf("unsafe null byte in tar member %q", raw)
	}
	if strings.HasPrefix(raw, "../") || strings.Contains(raw, "/../") || raw == ".." {
		return "", fmt.Errorf("path traversal detected in tar member %q", raw)
	}
	s := strings.ReplaceAll(raw, "\\", "/")
	parts := strings.Split(s, "/")
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			return "", fmt.Errorf("path traversal detected in tar member %q", raw)
		}
		cleaned = append(cleaned, p)
	}
	return strings.Join(cleaned, "/"), nil
}

// ExtractArtifactTar extracts wanted files from a plain or gzip-compressed tar
// archive into outDir while rejecting symlinks, hardlinks, special files, and
// path traversal attempts.
func ExtractArtifactTar(archivePath, outDir string, wanted map[string]os.FileMode) (map[string]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br := bufio.NewReader(f)
	magic, err := br.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	var r io.Reader = br
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("open gzip stream: %w", err)
		}
		defer gz.Close()
		r = gz
	}

	tr := tar.NewReader(r)
	extracted := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return nil, fmt.Errorf("unsafe special/link tar member rejected: %q", hdr.Name)
		}

		rel, err := normalizeTarMemberName(hdr.Name)
		if err != nil {
			return nil, err
		}
		if rel == "" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		mode, ok := wanted[rel]
		if !ok {
			continue
		}

		limit := maxMetaSize
		if rel == "gatehouse" || rel == "gatehouse.exe" {
			limit = maxBinarySize
		}
		if hdr.Size > limit {
			return nil, fmt.Errorf("tar member %q exceeds max size (%d > %d)", rel, hdr.Size, limit)
		}

		dst := filepath.Join(outDir, rel)
		tmp := dst + ".tmp"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return nil, err
		}
		written, copyErr := io.Copy(out, io.LimitReader(tr, limit+1))
		closeErr := out.Close()
		if copyErr != nil {
			_ = os.Remove(tmp)
			return nil, copyErr
		}
		if closeErr != nil {
			_ = os.Remove(tmp)
			return nil, closeErr
		}
		if written > limit {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("tar member %q exceeds max size %d", rel, limit)
		}
		if err := os.Chmod(tmp, mode); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		extracted[rel] = dst
	}
	return extracted, nil
}
