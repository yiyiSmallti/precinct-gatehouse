//go:build windows

package main

import "os"

func notifyReload() chan os.Signal {
	return nil
}
