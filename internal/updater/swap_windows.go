//go:build windows

package updater

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func binaryFilename() string {
	return "gatehouse.exe"
}

func atomicSwapBinary(binLink, _, absTarget string) error {
	dir := filepath.Dir(binLink)
	tmp := filepath.Join(dir, fmt.Sprintf(".gatehouse.next.%d.exe", os.Getpid()))
	src, err := os.Open(absTarget)
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		_ = src.Close()
		return err
	}
	_, copyErr := io.Copy(dst, src)
	_ = src.Close()
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}

	oldPath := binLink + ".old"
	_ = os.Remove(oldPath)
	if _, err := os.Stat(binLink); err == nil {
		_ = os.Rename(binLink, oldPath)
	}
	if err := os.Rename(tmp, binLink); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func currentLinkVersion(_ string) string {
	return ""
}

func isSymlinkPath(_ string) bool {
	return false
}
