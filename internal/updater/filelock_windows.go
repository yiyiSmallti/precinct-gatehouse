//go:build windows

package updater

import (
	"fmt"
	"os"
	"time"
)

func acquireFileLock(path string) (func(), error) {
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) > 30*time.Minute {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("another deployment is already in progress (lock: %s): %w", path, err)
	}
	_ = f.Close()
	return func() {
		_ = os.Remove(path)
	}, nil
}
