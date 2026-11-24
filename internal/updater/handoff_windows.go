//go:build windows

package updater

import (
	"errors"
	"net"
	"os/exec"
)

func supportsHandoff(string) bool {
	return false
}

type handoffSocket struct{}

func listenHandoffSocket(string) (*handoffSocket, error) {
	return nil, errors.New("socket handoff is unsupported on windows")
}

func (*handoffSocket) Addr() string      { return "" }
func (*handoffSocket) BoundAddr() string { return "" }
func (*handoffSocket) Close() error      { return nil }

func isAddrInUse(error) bool { return false }

type supervisorHandoff struct {
	events chan HandoffEvent
}

func startSupervisorHandoff(*exec.Cmd, string, string) (*supervisorHandoff, error) {
	return nil, errors.New("socket handoff is unsupported on windows")
}

func (*supervisorHandoff) afterStart(bool)               {}
func (s *supervisorHandoff) Events() <-chan HandoffEvent { return s.events }
func (*supervisorHandoff) SendActivate(*handoffSocket, *handoffSocket) error {
	return errors.New("unsupported on windows")
}
func (*supervisorHandoff) SendCmd(string) error { return nil }
func (*supervisorHandoff) Close()               {}

// WorkerHandoff is unavailable on Windows; legacy restart flow is used instead.
type WorkerHandoff struct {
	cmds chan HandoffCommand
}

// NewWorkerHandoffFromEnv always returns an error on Windows.
func NewWorkerHandoffFromEnv() (*WorkerHandoff, error) {
	return nil, errors.New("socket handoff is unsupported on windows")
}

func (w *WorkerHandoff) Commands() <-chan HandoffCommand { return w.cmds }
func (*WorkerHandoff) SendEvent(HandoffEvent) error      { return errors.New("unsupported on windows") }
func (*WorkerHandoff) RecvListeners(bool) (net.Listener, net.Listener, error) {
	return nil, nil, errors.New("unsupported on windows")
}
func (*WorkerHandoff) Close() {}
