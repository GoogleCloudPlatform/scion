// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build unix

package conduit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// ptyHangupGrace is how long Close waits for the tmux client to exit on
// the pty hangup before it kills the client's process group.
const ptyHangupGrace = 2 * time.Second

// ptyPassEnv are the variables of this process passed to the tmux client
// as they are: what it needs to find the server socket and to render.
// Everything else (notably credentials) stays out of its environment.
var ptyPassEnv = []string{"PATH", "TMUX_TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TZ"}

// defaultPTYSpawner returns the tmux spawner for user, or nil when tmux
// is not on PATH (the agent then does not offer the pty kind).
func defaultPTYSpawner(user PTYUser) PTYSpawner {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		log.Debug("Conduit: tmux not found (%v); not offering pty streams", err)
		return nil
	}
	return func(_ context.Context, req PTYRequest) (PTYProcess, error) {
		return startLocalPTY(user, req, tmux, "attach-session", "-t", req.Session)
	}
}

// ptyEnv builds the tmux client's environment.
func ptyEnv(user PTYUser) []string {
	env := []string{"TERM=xterm-256color"}
	for _, k := range ptyPassEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if user.Username != "" {
		env = append(env, "HOME=/home/"+user.Username, "USER="+user.Username, "LOGNAME="+user.Username)
	} else if home, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+home)
	}
	return env
}

// localPTY is a process on a local pty. The master is non-blocking and
// registered with the runtime poller, so closing it interrupts a pending
// Read and actually hangs up the pty.
type localPTY struct {
	cmd    *exec.Cmd
	master *os.File
	exited chan struct{} // closed once cmd has been reaped
	grace  time.Duration // hang-up grace before the kill (ptyHangupGrace)

	mu     sync.Mutex // guards closed and ioctls on master
	closed bool
	once   sync.Once
}

// startLocalPTY starts name with args on a new pty of req's size, as
// user, in a new session with the pty as its controlling terminal.
func startLocalPTY(user PTYUser, req PTYRequest, name string, args ...string) (*localPTY, error) {
	drop := user.UID > 0 && user.GID > 0
	if user.RequirePrivilegeDrop && !drop {
		return nil, errors.New("pty: privilege drop required but no agent user is set")
	}
	m, tty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("pty: open: %w", err)
	}
	defer func() { _ = tty.Close() }()
	if err := pty.Setsize(m, &pty.Winsize{Cols: req.Cols, Rows: req.Rows}); err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("pty: set size: %w", err)
	}
	master, err := pollable(m)
	if err != nil {
		return nil, err
	}
	if drop {
		// The client must own its terminal, as with a login.
		if err := tty.Chown(user.UID, user.GID); err != nil {
			_ = master.Close()
			return nil, fmt.Errorf("pty: chown tty to %d:%d: %w", user.UID, user.GID, err)
		}
	}
	cmd := exec.Command(name, args...)
	cmd.Env = ptyEnv(user)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if drop {
		cmd.SysProcAttr.Credential = suppgroups.Credential(uint32(user.UID), uint32(user.GID))
	}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("pty: start %s: %w", name, err)
	}
	p := &localPTY{cmd: cmd, master: master, exited: make(chan struct{}), grace: ptyHangupGrace}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// pollable returns a non-blocking, poller-registered copy of the pty
// master m and closes m. (pty.Open leaves the master in blocking mode,
// where Close cannot interrupt a Read.)
func pollable(m *os.File) (*os.File, error) {
	fd, err := unix.Dup(int(m.Fd()))
	_ = m.Close()
	if err != nil {
		return nil, fmt.Errorf("pty: dup master: %w", err)
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("pty: set non-blocking: %w", err)
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), nil
}

func (p *localPTY) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *localPTY) Write(b []byte) (int, error) { return p.master.Write(b) }

// Resize sets the pty window size; the kernel signals the client.
func (p *localPTY) Resize(cols, rows uint16) error {
	return p.control(func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	})
}

// size reports the pty window size (tests).
func (p *localPTY) size() (cols, rows uint16, err error) {
	err = p.control(func(fd int) error {
		ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		if err == nil {
			cols, rows = ws.Col, ws.Row
		}
		return err
	})
	return cols, rows, err
}

// control runs fn on the master fd without switching it to blocking
// mode (which os.File.Fd would do).
func (p *localPTY) control(fn func(fd int) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errPTYClosed
	}
	sc, err := p.master.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := sc.Control(func(fd uintptr) { opErr = fn(int(fd)) }); err != nil {
		return err
	}
	return opErr
}

// Close hangs up the pty (closing the master), waits up to
// p.grace for the client to exit, then kills its process group,
// and returns once the client has been reaped.
func (p *localPTY) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		_ = p.master.Close()
		p.mu.Unlock()
		t := time.NewTimer(p.grace)
		defer t.Stop()
		select {
		case <-p.exited:
			return
		case <-t.C:
		}
		// Setsid made the client its own process group leader.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.exited
	})
	return nil
}
