//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyReload() chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	return ch
}
