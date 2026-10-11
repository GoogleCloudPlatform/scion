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

package sshd

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Terminal size bounds: the size used when pty-req names none, and the
// largest cols or rows applied.
const (
	defaultCols = 80
	defaultRows = 24
	maxDim      = 4096
)

// Session request payloads (RFC 4254 §6).
type ptyReqMsg struct {
	Term          string
	Columns, Rows uint32
	Width, Height uint32
	Modes         string
}

type windowChangeMsg struct {
	Columns, Rows uint32
	Width, Height uint32
}

type execMsg struct{ Command string }

type subsystemMsg struct{ Name string }

type exitStatusMsg struct{ Status uint32 }

type exitSignalMsg struct {
	Signal     string
	CoreDumped bool
	Error      string
	Lang       string
}

// ptyRequest is an accepted pty-req.
type ptyRequest struct {
	Term       string
	Cols, Rows uint16
}

// command is what a session runs.
type command struct {
	Path string   // executable
	Args []string // argv, Args[0] included
	PTY  *ptyRequest
}

// exitResult is how a session's process ended: a signal name (without
// the SIG prefix) or an exit code.
type exitResult struct {
	Code   int
	Signal string
}

// process is a session's running command. Hangup sends SIGHUP to the
// command's process group only, so processes that started their own
// session (setsid) are left running. Done is closed once the command has
// exited and its output has been delivered; Result is valid after that.
type process interface {
	Resize(cols, rows uint16) error
	Hangup()
	Done() <-chan struct{}
	Result() exitResult
}

// SFTPSubsystem is the only subsystem served.
const SFTPSubsystem = "sftp"

// handleSession serves a session channel: pty-req, window-change,
// shell, exec and the sftp subsystem. env, agent forwarding
// (auth-agent-req@openssh.com), X11 (x11-req) and every other request are
// refused. When the client closes the channel or the connection ends
// before the command exits, the command's process group gets SIGHUP.
func (c *Config) handleSession(ctx context.Context, nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	var (
		pty  *ptyRequest
		proc process
		done <-chan struct{}
	)
	end := func() {
		if proc != nil {
			select {
			case <-proc.Done():
			default:
				proc.Hangup()
			}
		}
		_ = ch.Close()
		if reqs != nil {
			go ssh.DiscardRequests(reqs)
		}
	}
	for {
		select {
		case <-ctx.Done():
			end()
			return
		case <-done:
			sendExit(ch, proc.Result())
			_ = ch.CloseWrite()
			end()
			return
		case r, ok := <-reqs:
			if !ok {
				reqs = nil
				end()
				return
			}
			ok = false
			switch r.Type {
			case "pty-req":
				var m ptyReqMsg
				if proc == nil && pty == nil && ssh.Unmarshal(r.Payload, &m) == nil {
					pty = &ptyRequest{Term: m.Term, Cols: dim(m.Columns, defaultCols), Rows: dim(m.Rows, defaultRows)}
					ok = true
				}
			case "window-change":
				var m windowChangeMsg
				if proc != nil && pty != nil && ssh.Unmarshal(r.Payload, &m) == nil {
					ok = proc.Resize(dim(m.Columns, defaultCols), dim(m.Rows, defaultRows)) == nil
				}
			case "shell", "exec", "subsystem":
				if proc != nil {
					break
				}
				cmd, cerr := c.sessionCommand(r, pty)
				if cerr != nil {
					log.Debug("Conduit SSH: refused %s request: %v", r.Type, cerr)
					break
				}
				p, serr := c.start(ch, cmd)
				if serr != nil {
					log.Warn("Conduit SSH: cannot start %s: %v", r.Type, serr)
					break
				}
				proc, done, ok = p, p.Done(), true
			default:
				// env, signal, auth-agent-req@openssh.com, x11-req, ...
				log.Debug("Conduit SSH: refused session request %q", r.Type)
			}
			if r.WantReply {
				_ = r.Reply(ok, nil)
			}
		}
	}
}

// sessionCommand maps a shell, exec or subsystem request to a command.
// shell runs the login shell (on a pty when one was requested), exec runs
// the command through the shell, and the sftp subsystem runs the sftp
// server helper (never on a pty).
func (c *Config) sessionCommand(r *ssh.Request, pty *ptyRequest) (command, error) {
	switch r.Type {
	case "shell":
		return command{Path: c.Shell, Args: []string{"-" + filepath.Base(c.Shell)}, PTY: pty}, nil
	case "exec":
		var m execMsg
		if err := ssh.Unmarshal(r.Payload, &m); err != nil {
			return command{}, err
		}
		return command{Path: c.Shell, Args: []string{c.Shell, "-c", m.Command}, PTY: pty}, nil
	default:
		var m subsystemMsg
		if err := ssh.Unmarshal(r.Payload, &m); err != nil {
			return command{}, err
		}
		if m.Name != SFTPSubsystem || c.HelperPath == "" {
			return command{}, errSubsystem(m.Name)
		}
		return command{Path: c.HelperPath, Args: []string{c.HelperPath, HelperCommand, helperSFTP}}, nil
	}
}

type errSubsystem string

func (e errSubsystem) Error() string { return "subsystem " + string(e) + " is not served" }

// sendExit reports how the command ended (exit-status or exit-signal).
func sendExit(ch ssh.Channel, res exitResult) {
	if res.Signal != "" {
		_, _ = ch.SendRequest("exit-signal", false, ssh.Marshal(exitSignalMsg{Signal: res.Signal}))
		return
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: uint32(res.Code)}))
}

// dim clamps a terminal dimension to 1..maxDim, using def for 0.
func dim(v uint32, def uint16) uint16 {
	switch {
	case v == 0:
		return def
	case v > maxDim:
		return maxDim
	}
	return uint16(v)
}

// passwdFile is where loginShell looks up the user's shell.
const passwdFile = "/etc/passwd"

// loginShell is u's shell from the passwd file at path (looked up by
// u.UID, or this process's uid when u.UID is not positive) when it is an
// absolute path to an executable file; otherwise /bin/bash when present,
// else /bin/sh.
func loginShell(path string, u User) string {
	uid := u.UID
	if uid <= 0 {
		uid = os.Getuid()
	}
	if sh := passwdShell(path, uid); sh != "" && isExecutable(sh) {
		return sh
	}
	if isExecutable("/bin/bash") {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// passwdShell returns the shell field of uid's entry in the passwd file
// at path, or "".
func passwdShell(path string, uid int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	want := strconv.Itoa(uid)
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) == 7 && f[2] == want && filepath.IsAbs(f[6]) {
			return f[6]
		}
	}
	return ""
}

// isExecutable reports whether path is a regular file with an execute bit.
func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// DefaultEnv is the environment used when Config.Env is not set: this
// process's environment with HOME, USER and LOGNAME set for u. sciontool
// sets Config.Env to the harness's environment instead.
func DefaultEnv(u User) []string {
	env := os.Environ()
	if u.Username == "" {
		return env
	}
	return setEnv(setEnv(setEnv(env, "HOME", u.Home()), "USER", u.Username), "LOGNAME", u.Username)
}

// setEnv sets key=value in env, replacing an existing entry.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			continue
		}
		out = append(out, e)
	}
	return append(out, prefix+value)
}
