//go:build !windows

package updater

import (
	"fmt"
	"os"
	"path/filepath"
)

func binaryFilename() string {
	return "gatehouse"
}

func atomicSwapBinary(binLink, relTarget, _ string) error {
	tmp := filepath.Join(filepath.Dir(binLink), fmt.Sprintf(".gatehouse.next.%d", os.Getpid()))
	_ = os.Remove(tmp)
	if err := os.Symlink(relTarget, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, binLink); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func currentLinkVersion(binLink string) string {
	target, err := os.Readlink(binLink)
	if err != nil {
		return ""
	}
	return filepath.Base(filepath.Dir(target))
}

func isSymlinkPath(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0
}
