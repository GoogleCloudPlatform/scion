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
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// TestExecForcedUser: exec runs without a pty as the container user,
// whatever user name the client logs in with (here: root, while the
// container user is this process's identity).
func TestExecForcedUser(t *testing.T) {
	client := serve(t, testConfig(t), "root")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("id -u; test -t 0 && echo tty || echo notty")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	want := strconv.Itoa(os.Getuid()) + "\nnotty"
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("exec output %q, want %q", out, want)
	}
}

// TestExecExitStatusAndStderr: the exit status and stderr reach the
// client.
func TestExecExitStatusAndStderr(t *testing.T) {
	client := serve(t, testConfig(t), "scion")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	err = sess.Run("echo oops >&2; exit 3")
	var ee *ssh.ExitError
	if !errors.As(err, &ee) || ee.ExitStatus() != 3 {
		t.Fatalf("Run = %v, want exit status 3", err)
	}
	if strings.TrimSpace(stderr.String()) != "oops" {
		t.Fatalf("stderr %q", stderr.String())
	}
}

// TestEnvBuiltPerSession: the environment is obtained from Config.Env
// for every session, not once per server or connection.
func TestEnvBuiltPerSession(t *testing.T) {
	cfg := testConfig(t)
	var mu sync.Mutex
	calls := 0
	cfg.Env = func() ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return append(os.Environ(), "SESSION_N="+strconv.Itoa(calls)), nil
	}
	client := serve(t, cfg, "scion")
	for want := 1; want <= 2; want++ {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		out, err := sess.Output("echo $SESSION_N")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(out)); got != strconv.Itoa(want) {
			t.Fatalf("session %d saw SESSION_N=%q", want, got)
		}
	}
}

// ptyShell starts a login shell on a pty and returns its stdin and a
// buffer collecting its output.
func ptyShell(t *testing.T, client *ssh.Client, rows, cols int) (*ssh.Session, io.WriteCloser, *syncBuffer) {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.RequestPty("xterm-test", rows, cols, ssh.TerminalModes{}); err != nil {
		t.Fatalf("pty-req: %v", err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	sess.Stdout = out
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	return sess, in, out
}

// syncBuffer is a goroutine-safe bytes.Buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestPTYLoginShell: pty-req plus shell runs the login shell on a fresh
// pty with TERM from pty-req and the requested size; window-change
// resizes it.
func TestPTYLoginShell(t *testing.T) {
	client := serve(t, testConfig(t), "scion")
	sess, in, out := ptyShell(t, client, 30, 100)
	if _, err := io.WriteString(in, "echo \"argv0=$0 term=$TERM\"; tty; stty size\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the login shell's answers", func() bool {
		s := out.String()
		return strings.Contains(s, "argv0=-sh term=xterm-test") && strings.Contains(s, "/dev/pts/") && strings.Contains(s, "30 100")
	})
	if err := sess.WindowChange(50, 120); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the resized pty", func() bool {
		_, _ = io.WriteString(in, "stty size\n")
		time.Sleep(50 * time.Millisecond)
		return strings.Contains(out.String(), "50 120")
	})
	_, _ = io.WriteString(in, "exit 4\n")
	var ee *ssh.ExitError
	if err := sess.Wait(); !errors.As(err, &ee) || ee.ExitStatus() != 4 {
		t.Fatalf("Wait = %v, want exit status 4", err)
	}
}

// TestPTYCreatesNoTmuxSession: a pty-req session is the login shell
// itself; no tmux client, session or window is created. A recording tmux
// is first on the session's PATH, also after the login profile (the
// session's HOME/.profile puts it there), and must never run.
func TestPTYCreatesNoTmuxSession(t *testing.T) {
	bin := t.TempDir()
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "tmux-called")
	script := "#!/bin/sh\necho \"$@\" >> " + marker + "\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := "PATH=" + bin + ":$PATH\nexport PATH\n"
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	env := setEnv(setEnv(os.Environ(), "PATH", bin+":"+os.Getenv("PATH")), "HOME", home)
	env = setEnv(env, "ENV", "")
	cfg.Env = func() ([]string, error) { return env, nil }
	client := serve(t, cfg, "scion")
	sess, in, out := ptyShell(t, client, 24, 80)
	_, _ = io.WriteString(in, "echo \"tmux=$(command -v tmux) argv0=$0\"; exit\n")
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v (output %q)", err, out.String())
	}
	if !strings.Contains(out.String(), "tmux="+filepath.Join(bin, "tmux")) {
		t.Fatalf("the recording tmux is not first on the session's PATH: %q", out.String())
	}
	if !strings.Contains(out.String(), "argv0=-") {
		t.Fatalf("the session is not a login shell: %q", out.String())
	}
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("tmux was invoked: %q", data)
	}
}

// TestSFTPSubsystem: the sftp subsystem writes and reads a file, as the
// container user.
func TestSFTPSubsystem(t *testing.T) {
	client := serve(t, testConfig(t), "scion")
	sc, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("sftp: %v", err)
	}
	defer func() { _ = sc.Close() }()
	path := filepath.Join(t.TempDir(), "f.txt")
	f, err := sc.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("sftp-data")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "sftp-data" {
		t.Fatalf("file = %q, %v", data, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != os.Getuid() {
		t.Fatalf("file owner %d, want the container user %d", st.Uid, os.Getuid())
	}
	r, err := sc.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(got) != "sftp-data" {
		t.Fatalf("read back %q, %v", got, err)
	}
}

// TestSFTPRefusedWithoutContainerUser: with a required privilege drop
// and no container user, the sftp subsystem is refused.
func TestSFTPRefusedWithoutContainerUser(t *testing.T) {
	cfg := testConfig(t)
	cfg.User = User{RequirePrivilegeDrop: true}
	client := serve(t, cfg, "scion")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if err := sess.RequestSubsystem(SFTPSubsystem); err == nil {
		t.Fatal("sftp accepted")
	}
}

// TestOtherSubsystemRefused: only sftp is served.
func TestOtherSubsystemRefused(t *testing.T) {
	client := serve(t, testConfig(t), "scion")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if err := sess.RequestSubsystem("netconf"); err == nil {
		t.Fatal("subsystem netconf accepted")
	}
}

// TestSessionEndHangsUpProcessGroup: when the client closes the session,
// every process in the command's process group gets SIGHUP (the leader
// and a background child), and a process that started its own session
// (setsid) does not.
func TestSessionEndHangsUpProcessGroup(t *testing.T) {
	setsid, err := lookPath("setsid")
	if err != nil {
		t.Skip("setsid not available")
	}
	dir := t.TempDir()
	hup := filepath.Join(dir, "group-hup")
	detachedHup := filepath.Join(dir, "detached-hup")
	detachedPID := filepath.Join(dir, "detached-pid")
	bgHup := filepath.Join(dir, "background-hup")
	bgPID := filepath.Join(dir, "background-pid")
	client := serve(t, testConfig(t), "scion")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	sess.Stdout = out
	cmd := setsid + " sh -c 'trap \"echo HUP > " + detachedHup + "\" HUP; echo $$ > " + detachedPID + "; sleep 30 & wait' </dev/null >/dev/null 2>&1 & " +
		"sh -c 'trap \"echo HUP > " + bgHup + "; exit 0\" HUP; echo $$ > " + bgPID + "; while :; do sleep 0.1; done' </dev/null >/dev/null 2>&1 & " +
		"trap 'echo HUP > " + hup + "; exit 0' HUP; echo ready; while :; do sleep 0.1; done"
	if err := sess.Start(cmd); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command to start", func() bool {
		_, err1 := os.Stat(detachedPID)
		_, err2 := os.Stat(bgPID)
		return strings.Contains(out.String(), "ready") && err1 == nil && err2 == nil
	})
	_ = sess.Close()
	waitFor(t, "SIGHUP to the group leader", func() bool {
		_, err := os.Stat(hup)
		return err == nil
	})
	waitFor(t, "SIGHUP to a background process in the same group", func() bool {
		_, err := os.Stat(bgHup)
		return err == nil
	})
	raw, err := os.ReadFile(detachedPID)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(detachedHup); err == nil {
		t.Fatal("the setsid process received SIGHUP")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the setsid process did not survive: %v", err)
	}
}

// lookPath finds name in PATH.
func lookPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

// listeningFDs returns this process's descriptors that are listening
// sockets.
func listeningFDs(t *testing.T) map[int]bool {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	out := map[int]bool{}
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err == nil && v == 1 {
			out[fd] = true
		}
	}
	return out
}

// TestServerCreatesNoListener: serving a connection with sessions and
// forwarded channels opens no listening socket; the server is reached
// only through the connection it is given.
func TestServerCreatesNoListener(t *testing.T) {
	before := listeningFDs(t)
	client := serve(t, testConfig(t), "scion")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Output("true"); err != nil {
		t.Fatal(err)
	}
	_, in, _ := ptyShell(t, client, 24, 80)
	defer func() { _ = in.Close() }()
	for fd := range listeningFDs(t) {
		if !before[fd] {
			t.Fatalf("descriptor %d is a new listening socket", fd)
		}
	}
}
