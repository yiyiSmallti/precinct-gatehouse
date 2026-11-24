//go:build !windows

package updater

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type handoffCapKey struct {
	realPath string
	size     int64
	modNano  int64
	helper   string
}

var (
	handoffCapMu    sync.Mutex
	handoffCapCache = map[handoffCapKey]bool{}
)

func supportsHandoff(binPath string) bool {
	if strings.TrimSpace(binPath) == "" {
		return false
	}
	st, err := os.Stat(binPath)
	if err != nil || st.IsDir() {
		return false
	}
	realPath, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		realPath = binPath
	}
	key := handoffCapKey{
		realPath: realPath,
		size:     st.Size(),
		modNano:  st.ModTime().UnixNano(),
		helper:   os.Getenv("GATEHOUSE_HANDOFF_HELPER"),
	}
	handoffCapMu.Lock()
	cached, ok := handoffCapCache[key]
	handoffCapMu.Unlock()
	if ok {
		return cached
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "serve", "-h")
	cmd.Env = append(os.Environ(), "GATEHOUSE_HANDOFF_PROBE=1")
	out, _ := cmd.CombinedOutput()
	supported := bytes.Contains(out, []byte("-handoff"))

	handoffCapMu.Lock()
	handoffCapCache[key] = supported
	handoffCapMu.Unlock()
	return supported
}

type handoffSocket struct {
	addr string
	ln   net.Listener
}

func listenHandoffSocket(addr string) (*handoffSocket, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &handoffSocket{addr: addr, ln: ln}, nil
}

func (s *handoffSocket) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

func (s *handoffSocket) BoundAddr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *handoffSocket) Close() error {
	if s == nil || s.ln == nil {
		return nil
	}
	return s.ln.Close()
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

type supervisorHandoff struct {
	supConn   *net.UnixConn
	childSock *os.File
	ctrlR     *os.File
	ctrlW     *os.File
	cmdR      *os.File
	cmdW      *os.File

	mu     sync.Mutex
	closed bool
	events chan HandoffEvent
}

func startSupervisorHandoff(cmd *exec.Cmd, dataAddr, adminAddr string) (*supervisorHandoff, error) {
	ctrlR, ctrlW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("ctrl pipe: %w", err)
	}
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		_ = ctrlR.Close()
		_ = ctrlW.Close()
		return nil, fmt.Errorf("cmd pipe: %w", err)
	}
	fdPair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		_ = ctrlR.Close()
		_ = ctrlW.Close()
		_ = cmdR.Close()
		_ = cmdW.Close()
		return nil, fmt.Errorf("fd socketpair: %w", err)
	}
	syscall.CloseOnExec(fdPair[0])
	syscall.CloseOnExec(fdPair[1])

	supFile := os.NewFile(uintptr(fdPair[0]), "handoff-sup-sock")
	conn, err := net.FileConn(supFile)
	_ = supFile.Close()
	if err != nil {
		_ = syscall.Close(fdPair[1])
		_ = ctrlR.Close()
		_ = ctrlW.Close()
		_ = cmdR.Close()
		_ = cmdW.Close()
		return nil, fmt.Errorf("wrap supervisor unix conn: %w", err)
	}
	supConn, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		_ = syscall.Close(fdPair[1])
		_ = ctrlR.Close()
		_ = ctrlW.Close()
		_ = cmdR.Close()
		_ = cmdW.Close()
		return nil, errors.New("unexpected non-unix handoff socket")
	}
	childSock := os.NewFile(uintptr(fdPair[1]), "handoff-child-sock")

	baseFD := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, childSock, ctrlW, cmdR)
	cmd.Args = append(cmd.Args, "--handoff")
	cmd.Env = append(cmd.Env,
		EnvHandoff+"=1",
		EnvFDSockFD+"="+strconv.Itoa(baseFD),
		EnvCtrlWriteFD+"="+strconv.Itoa(baseFD+1),
		EnvCmdReadFD+"="+strconv.Itoa(baseFD+2),
		EnvListenAddr+"="+dataAddr,
		EnvAdminAddr+"="+adminAddr,
	)

	return &supervisorHandoff{
		supConn:   supConn,
		childSock: childSock,
		ctrlR:     ctrlR,
		ctrlW:     ctrlW,
		cmdR:      cmdR,
		cmdW:      cmdW,
		events:    make(chan HandoffEvent, 16),
	}, nil
}

func (s *supervisorHandoff) afterStart(started bool) {
	if s.childSock != nil {
		_ = s.childSock.Close()
		s.childSock = nil
	}
	if s.ctrlW != nil {
		_ = s.ctrlW.Close()
		s.ctrlW = nil
	}
	if s.cmdR != nil {
		_ = s.cmdR.Close()
		s.cmdR = nil
	}
	if !started {
		if s.ctrlR != nil {
			_ = s.ctrlR.Close()
		}
		s.Close()
		close(s.events)
		return
	}
	go s.readEventsLoop()
}

func (s *supervisorHandoff) readEventsLoop() {
	defer func() {
		_ = s.ctrlR.Close()
		close(s.events)
	}()
	sc := bufio.NewScanner(s.ctrlR)
	sc.Buffer(make([]byte, 4096), 256*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev HandoffEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		s.events <- ev
	}
}

func (s *supervisorHandoff) Events() <-chan HandoffEvent {
	return s.events
}

func (s *supervisorHandoff) SendActivate(dataSock, adminSock *handoffSocket) error {
	if dataSock == nil || dataSock.ln == nil {
		return errors.New("missing data listener socket for handoff")
	}
	if err := sendSocketFDs(s.supConn, dataSock.ln, adminSock); err != nil {
		return err
	}
	return s.SendCmd("activate")
}

func (s *supervisorHandoff) SendCmd(cmdStr string) error {
	b, err := json.Marshal(HandoffCommand{Cmd: cmdStr})
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.cmdW == nil {
		return errors.New("handoff command channel closed")
	}
	_, err = s.cmdW.Write(b)
	return err
}

func (s *supervisorHandoff) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.supConn != nil {
		_ = s.supConn.Close()
	}
	if s.cmdW != nil {
		_ = s.cmdW.Close()
	}
	s.mu.Unlock()
}

func dupListenerFDNoBlockTouch(ln net.Listener) (int, error) {
	sc, ok := ln.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return -1, errors.New("listener does not implement SyscallConn")
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return -1, err
	}
	dupFD := -1
	var dupErr error
	if err := rc.Control(func(fd uintptr) {
		syscall.ForkLock.RLock()
		d, err := syscall.Dup(int(fd))
		if err == nil {
			syscall.CloseOnExec(d)
		}
		syscall.ForkLock.RUnlock()
		dupFD, dupErr = d, err
	}); err != nil {
		return -1, err
	}
	if dupErr != nil {
		return -1, dupErr
	}
	return dupFD, nil
}

func sendSocketFDs(conn *net.UnixConn, dataLn net.Listener, adminSock *handoffSocket) error {
	if conn == nil {
		return errors.New("nil handoff unix socket")
	}
	fds := make([]int, 0, 2)
	cleanup := func() {
		for _, fd := range fds {
			if fd >= 0 {
				_ = syscall.Close(fd)
			}
		}
	}
	dataFD, err := dupListenerFDNoBlockTouch(dataLn)
	if err != nil {
		return fmt.Errorf("dup data fd: %w", err)
	}
	fds = append(fds, dataFD)

	if adminSock != nil && adminSock.ln != nil {
		admFD, err := dupListenerFDNoBlockTouch(adminSock.ln)
		if err != nil {
			cleanup()
			return fmt.Errorf("dup admin fd: %w", err)
		}
		fds = append(fds, admFD)
	}
	defer cleanup()

	rights := syscall.UnixRights(fds...)
	_, _, err = conn.WriteMsgUnix([]byte("F"), rights, nil)
	if err != nil {
		return fmt.Errorf("write unix rights: %w", err)
	}
	return nil
}

// WorkerHandoff is the worker-process side of supervisor FD handoff.
type WorkerHandoff struct {
	fdSock *net.UnixConn
	ctrlW  *os.File
	cmdR   *os.File

	mu     sync.Mutex
	closed bool
	cmds   chan HandoffCommand
}

// NewWorkerHandoffFromEnv opens the inherited handoff FDs and starts reading commands.
func NewWorkerHandoffFromEnv() (*WorkerHandoff, error) {
	if os.Getenv(EnvHandoff) != "1" {
		return nil, errors.New("not running in handoff mode")
	}
	parseFD := func(env string) (int, error) {
		raw := strings.TrimSpace(os.Getenv(env))
		if raw == "" {
			return -1, fmt.Errorf("missing %s", env)
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < 3 {
			return -1, fmt.Errorf("invalid %s=%q", env, raw)
		}
		syscall.CloseOnExec(v)
		return v, nil
	}
	sockFD, err := parseFD(EnvFDSockFD)
	if err != nil {
		return nil, err
	}
	ctrlFD, err := parseFD(EnvCtrlWriteFD)
	if err != nil {
		return nil, err
	}
	cmdFD, err := parseFD(EnvCmdReadFD)
	if err != nil {
		return nil, err
	}

	sockFile := os.NewFile(uintptr(sockFD), "worker-fd-sock")
	c, err := net.FileConn(sockFile)
	_ = sockFile.Close()
	if err != nil {
		return nil, fmt.Errorf("open fd socket: %w", err)
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		_ = c.Close()
		return nil, errors.New("handoff socket is not AF_UNIX")
	}

	wh := &WorkerHandoff{
		fdSock: uc,
		ctrlW:  os.NewFile(uintptr(ctrlFD), "worker-ctrl-w"),
		cmdR:   os.NewFile(uintptr(cmdFD), "worker-cmd-r"),
		cmds:   make(chan HandoffCommand, 8),
	}
	go wh.readCommandsLoop()
	return wh, nil
}

func (w *WorkerHandoff) readCommandsLoop() {
	defer func() {
		_ = w.cmdR.Close()
		close(w.cmds)
	}()
	sc := bufio.NewScanner(w.cmdR)
	sc.Buffer(make([]byte, 4096), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var cmd HandoffCommand
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			continue
		}
		w.cmds <- cmd
	}
}

func (w *WorkerHandoff) Commands() <-chan HandoffCommand {
	return w.cmds
}

func (w *WorkerHandoff) SendEvent(ev HandoffEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.ctrlW == nil {
		return errors.New("worker handoff ctrl pipe closed")
	}
	_, err = w.ctrlW.Write(b)
	return err
}

// RecvListeners receives the handed-off TCP listeners over the Unix SCM_RIGHTS socket.
func (w *WorkerHandoff) RecvListeners(expectAdmin bool) (dataLn, adminLn net.Listener, err error) {
	if w.fdSock == nil {
		return nil, nil, errors.New("fd socket closed")
	}
	_ = w.fdSock.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer func() { _ = w.fdSock.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, 32)
	oob := make([]byte, syscall.CmsgSpace(4*4))
	n, oobn, _, _, err := w.fdSock.ReadMsgUnix(buf, oob)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("read handoff fds: %w", err)
	}
	if n == 0 && oobn == 0 {
		return nil, nil, errors.New("handoff socket closed before delivering listeners")
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, nil, fmt.Errorf("parse control message: %w", err)
	}
	var fds []int
	for _, m := range msgs {
		pfds, perr := syscall.ParseUnixRights(&m)
		if perr != nil {
			continue
		}
		for _, fd := range pfds {
			syscall.CloseOnExec(fd)
			fds = append(fds, fd)
		}
	}
	want := 1
	if expectAdmin {
		want = 2
	}
	if len(fds) < want {
		for _, fd := range fds {
			_ = syscall.Close(fd)
		}
		return nil, nil, fmt.Errorf("expected %d listener FDs, got %d", want, len(fds))
	}
	// Close any unexpected trailing FDs cleanly.
	for _, extra := range fds[want:] {
		_ = syscall.Close(extra)
	}

	df := os.NewFile(uintptr(fds[0]), "handoff-data-ln")
	dataLn, err = net.FileListener(df)
	_ = df.Close()
	if err != nil {
		for _, fd := range fds[1:want] {
			_ = syscall.Close(fd)
		}
		return nil, nil, fmt.Errorf("wrap data listener: %w", err)
	}

	if expectAdmin {
		af := os.NewFile(uintptr(fds[1]), "handoff-admin-ln")
		adminLn, err = net.FileListener(af)
		_ = af.Close()
		if err != nil {
			_ = dataLn.Close()
			return nil, nil, fmt.Errorf("wrap admin listener: %w", err)
		}
	}
	return dataLn, adminLn, nil
}

func (w *WorkerHandoff) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	if w.fdSock != nil {
		_ = w.fdSock.Close()
	}
	if w.ctrlW != nil {
		_ = w.ctrlW.Close()
	}
	w.mu.Unlock()
}
