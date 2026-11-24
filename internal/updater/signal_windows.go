//go:build windows

package updater

import "os"

func notifyReloadSignals() chan os.Signal {
	return nil
}

func stopReloadSignals(_ chan os.Signal) {}

func forwardSignal(_ *os.Process, _ os.Signal) {}

func terminateProcess(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Kill()
}
