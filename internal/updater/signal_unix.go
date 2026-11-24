//go:build !windows

package updater

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyReloadSignals() chan os.Signal {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGHUP)
	return ch
}

func stopReloadSignals(ch chan os.Signal) {
	if ch != nil {
		signal.Stop(ch)
	}
}

func forwardSignal(p *os.Process, sig os.Signal) {
	if p == nil {
		return
	}
	_ = p.Signal(sig)
}

func terminateProcess(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
}
