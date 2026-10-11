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

package sshd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// ptyDrainGrace is how long a pty session's output is still delivered
// after the command exits (background jobs may keep the pty open).
const ptyDrainGrace = time.Second

// errPrivilegeDrop refuses to run anything as this process's identity
// when the configuration requires a privilege drop.
var errPrivilegeDrop = errors.New("privilege drop required but no container user is set")

// sysProcAttr returns the process attributes for a child run as u: its
// own process group (or session, with a pty) and u's credentials.
func sysProcAttr(u User, withPTY bool) (*syscall.SysProcAttr, error) {
	if u.RequirePrivilegeDrop && !u.drops() {
		return nil, errPrivilegeDrop
	}
	a := &syscall.SysProcAttr{Setpgid: !withPTY, Setsid: withPTY, Setctty: withPTY}
	if u.drops() {
		a.Credential = suppgroups.Credential(uint32(u.UID), uint32(u.GID))
	}
	return a, nil
}

// workDir is u's home when it exists, else /.
func workDir(u User) string {
	home := u.Home()
	if home == "" {
		home = os.Getenv("HOME")
	}
	if fi, err := os.Stat(home); err == nil && fi.IsDir() {
		return home
	}
	return "/"
}

// startManaged starts cmd registered with the PID 1 reaper, so its exit
// status stays with cmd.Wait, and returns a function that waits for and
// unregisters it.
func startManaged(cmd *exec.Cmd) (func() error, error) {
	var tok *procreap.Token
	err := procreap.Gated(func() error {
		if err := cmd.Start(); err != nil {
			return err
		}
		tok = procreap.RegisterManagedPID(cmd.Process.Pid)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return func() error {
		err := cmd.Wait()
		procreap.UnregisterManagedPID(cmd.Process.Pid, tok)
		return err
	}, nil
}

// localProcess is a session command started by start.
type localProcess struct {
	cmd    *exec.Cmd
	master *os.File // nil without a pty
	done   chan struct{}
	res    exitResult

	mu     sync.Mutex
	exited bool
}

// start runs cmd as the container user with the channel as its terminal
// (with a pty) or its stdin, stdout and stderr (without).
func (c *Config) start(ch ssh.Channel, cmd command) (process, error) {
	attr, err := sysProcAttr(c.User, cmd.PTY != nil)
	if err != nil {
		return nil, err
	}
	ec := &exec.Cmd{Path: cmd.Path, Args: cmd.Args, Env: c.Env(), Dir: workDir(c.User), SysProcAttr: attr}
	p := &localProcess{cmd: ec, done: make(chan struct{})}
	if cmd.PTY != nil {
		return p, p.startPTY(ch, cmd.PTY, c.User)
	}
	return p, p.startPipes(ch)
}

func (p *localProcess) startPipes(ch ssh.Channel) error {
	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		return err
	}
	p.cmd.Stdout = ch
	p.cmd.Stderr = ch.Stderr()
	wait, err := startManaged(p.cmd)
	if err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(stdin, ch)
		_ = stdin.Close()
	}()
	go func() {
		err := wait()
		p.markExited()
		p.finish(err)
	}()
	return nil
}

func (p *localProcess) startPTY(ch ssh.Channel, req *ptyRequest, u User) error {
	term := req.Term
	if term == "" {
		term = "xterm"
	}
	p.cmd.Env = setEnv(p.cmd.Env, "TERM", term)
	master, tty, err := pty.Open()
	if err != nil {
		return fmt.Errorf("open pty: %w", err)
	}
	defer func() { _ = tty.Close() }()
	if err := pty.Setsize(master, &pty.Winsize{Cols: req.Cols, Rows: req.Rows}); err != nil {
		_ = master.Close()
		return fmt.Errorf("set pty size: %w", err)
	}
	if u.drops() {
		if err := tty.Chown(u.UID, u.GID); err != nil {
			_ = master.Close()
			return fmt.Errorf("chown pty: %w", err)
		}
	}
	p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = tty, tty, tty
	wait, err := startManaged(p.cmd)
	if err != nil {
		_ = master.Close()
		return err
	}
	p.master = master
	out := make(chan struct{})
	go func() {
		_, _ = io.Copy(ch, master)
		close(out)
	}()
	go func() { _, _ = io.Copy(master, ch) }()
	go func() {
		err := wait()
		p.markExited()
		select {
		case <-out:
		case <-time.After(ptyDrainGrace):
		}
		_ = master.Close()
		p.finish(err)
	}()
	return nil
}

// finish records how the command ended and closes done.
func (p *localProcess) finish(err error) {
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			p.res.Signal = strings.TrimPrefix(unix.SignalName(ws.Signal()), "SIG")
		} else {
			p.res.Code = ee.ExitCode()
		}
	default:
		p.res.Code = 255
	}
	close(p.done)
}

// markExited stops Hangup from signalling once the command is reaped.
func (p *localProcess) markExited() {
	p.mu.Lock()
	p.exited = true
	p.mu.Unlock()
}

func (p *localProcess) Done() <-chan struct{} { return p.done }
func (p *localProcess) Result() exitResult    { return p.res }

// Resize sets the pty window size.
func (p *localProcess) Resize(cols, rows uint16) error {
	if p.master == nil {
		return errors.New("no pty")
	}
	return pty.Setsize(p.master, &pty.Winsize{Cols: cols, Rows: rows})
}

// Hangup sends SIGHUP to the command's process group (the command leads
// its own group or session), and only that group.
func (p *localProcess) Hangup() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited || p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGHUP)
}
