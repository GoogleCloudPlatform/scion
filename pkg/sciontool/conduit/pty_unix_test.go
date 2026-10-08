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
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// fakeTmux is a stand-in tmux client: it prints its arguments, then for
// each input line prints the terminal size and the line.
const fakeTmux = `#!/bin/sh
echo "ARGS:$*"
while IFS= read -r line; do
  echo "SIZE:$(stty size) GOT:$line"
done
`

// installFakeTmux puts fakeTmux first on PATH.
func installFakeTmux(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(fakeTmux), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// capturingSpawner wraps the default spawner and hands out each
// *localPTY it starts.
func capturingSpawner(t *testing.T) (PTYSpawner, <-chan *localPTY) {
	t.Helper()
	def := defaultPTYSpawner(PTYUser{})
	if def == nil {
		t.Fatal("default spawner unavailable with tmux on PATH")
	}
	procs := make(chan *localPTY, 4)
	return func(ctx context.Context, req PTYRequest) (PTYProcess, error) {
		p, err := def(ctx, req)
		if err == nil {
			procs <- p.(*localPTY)
		}
		return p, err
	}, procs
}

func nextLocalPTY(t *testing.T, procs <-chan *localPTY) *localPTY {
	t.Helper()
	select {
	case p := <-procs:
		return p
	case <-time.After(waitTimeout):
		t.Fatal("no pty spawned")
		return nil
	}
}

// readLine reads lines from r until one contains want, and returns it.
func readLine(t *testing.T, r *bufio.Reader, want string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if strings.Contains(line, want) {
				got <- line
				return
			}
			if err != nil {
				got <- "error: " + err.Error()
				return
			}
		}
	}()
	select {
	case line := <-got:
		if !strings.Contains(line, want) {
			t.Fatalf("no line containing %q: %s", want, line)
		}
		return line
	case <-time.After(waitTimeout):
		t.Fatalf("no line containing %q", want)
		return ""
	}
}

// assertTornDown checks that p's client has been reaped and its pty
// master closed.
func assertTornDown(t *testing.T, p *localPTY) {
	t.Helper()
	select {
	case <-p.exited:
	case <-time.After(waitTimeout):
		t.Fatal("tmux client still running")
	}
	if p.cmd.ProcessState == nil {
		t.Fatal("tmux client not reaped")
	}
	if err := syscall.Kill(p.cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("tmux client pid %d still exists (kill 0: %v)", p.cmd.Process.Pid, err)
	}
	if _, _, err := p.size(); !errors.Is(err, errPTYClosed) {
		t.Fatalf("pty master still open (size: %v)", err)
	}
}

// TestAgentPTYLocalClientResizeAndClose runs the default spawner against
// a fake tmux: the client runs `attach-session -t scion` on a pty of the
// granted size, the pty follows resize frames, and closing the stream
// reaps the client and closes the pty.
func TestAgentPTYLocalClientResizeAndClose(t *testing.T) {
	installFakeTmux(t)
	key := newTestKey(t, "k1")
	spawn, procs := capturingSpawner(t)
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) { o.SpawnPTY = spawn })
	s := h.nextSession(t)
	st, err := s.OpenStream(context.Background(), ptyOpen(t, key, s.Info(), ptyParams()))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	p := nextLocalPTY(t, procs)
	r := bufio.NewReader(st)
	readLine(t, r, "ARGS:attach-session -t scion")
	if _, err := st.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	readLine(t, r, "SIZE:30 100 GOT:one")

	if err := st.Resize(120, 50); err != nil {
		t.Fatal(err)
	}
	// The resize and the data travel separately; each round trip shows
	// the size the client sees, until the resize has been applied.
	resized := false
	for i := 0; i < 100 && !resized; i++ {
		if _, err := st.Write([]byte("probe\n")); err != nil {
			t.Fatal(err)
		}
		resized = strings.Contains(readLine(t, r, "GOT:probe"), "SIZE:50 120 ")
	}
	if !resized {
		t.Fatal("pty did not follow the resize")
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	assertTornDown(t, p)
}

// TestAgentPTYLocalClientCancelDuringOpening: with a real client spawned,
// cancelling the open before the accept reaps the client and closes its
// pty.
func TestAgentPTYLocalClientCancelDuringOpening(t *testing.T) {
	installFakeTmux(t)
	key := newTestKey(t, "k1")
	spawn, procs := capturingSpawner(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) {
		o.SpawnPTY = spawn
		o.ptyBeforeAccept = func(hctx context.Context) {
			cancel()
			<-hctx.Done()
		}
	})
	s := h.nextSession(t)
	if st, err := s.OpenStream(ctx, ptyOpen(t, key, s.Info(), ptyParams())); err == nil {
		_ = st.Close()
		t.Fatal("OpenStream succeeded after cancel")
	}
	assertTornDown(t, nextLocalPTY(t, procs))
}

// TestAgentAdvertisesPTYWithTmuxOnPath: the default spawner, and so the
// pty kind, is present exactly when tmux is on PATH.
func TestAgentAdvertisesPTYWithTmuxOnPath(t *testing.T) {
	installFakeTmux(t)
	h := newFakeHub(t)
	startAgent(t, h, nil)
	if got := h.nextHello(t).GetCapabilities().GetStreamKinds(); !slices.Contains(got, "pty") {
		t.Fatalf("stream kinds = %v, want pty", got)
	}
}

// TestPTYEnv: the client gets TERM, the agent user's HOME/USER/LOGNAME
// and the passthrough variables, and nothing else from this process.
func TestPTYEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SCION_AUTH_TOKEN", "secret")
	t.Setenv("LANG", "C.UTF-8")
	env := ptyEnv(PTYUser{UID: 1000, GID: 1000, Username: "scion"})
	for _, want := range []string{"TERM=xterm-256color", "PATH=/usr/bin", "LANG=C.UTF-8", "HOME=/home/scion", "USER=scion", "LOGNAME=scion"} {
		if !slices.Contains(env, want) {
			t.Errorf("env %v lacks %s", env, want)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "SCION_") {
			t.Errorf("env leaks %s", kv)
		}
	}
}

// TestLocalPTYRequiresPrivilegeDrop: with a privilege drop required, no
// client starts as this process's identity.
func TestLocalPTYRequiresPrivilegeDrop(t *testing.T) {
	_, err := startLocalPTY(PTYUser{RequirePrivilegeDrop: true}, PTYRequest{Cols: 80, Rows: 24, Session: "scion"}, "/bin/true")
	if err == nil {
		t.Fatal("started a client without the required privilege drop")
	}
}

// TestAgentPTYLocalClientSessionEnd: a real client is reaped and its pty
// closed when the session ends under its stream.
func TestAgentPTYLocalClientSessionEnd(t *testing.T) {
	installFakeTmux(t)
	key := newTestKey(t, "k1")
	spawn, procs := capturingSpawner(t)
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) { o.SpawnPTY = spawn })
	s := h.nextSession(t)
	if _, err := s.OpenStream(context.Background(), ptyOpen(t, key, s.Info(), ptyParams())); err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	p := nextLocalPTY(t, procs)
	if err := s.CloseWithCode(core.CloseRelayRestart, "relay_restart"); err != nil {
		t.Fatal(err)
	}
	assertTornDown(t, p)
}

// TestLocalPTYKillsAfterHangupGrace: a client that ignores the hangup is
// killed, with the children in its process group, once the grace ends.
func TestLocalPTYKillsAfterHangupGrace(t *testing.T) {
	// Become the subreaper so the orphaned child can be reaped here.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("cannot become a subreaper: %v", err)
	}
	t.Cleanup(func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0) })

	p, err := startLocalPTY(PTYUser{}, PTYRequest{Cols: 80, Rows: 24, Session: "scion"},
		"/bin/sh", "-c", `trap '' HUP; sleep 1000 & echo "CHILD:$!"; wait`)
	if err != nil {
		t.Fatal(err)
	}
	p.grace = 50 * time.Millisecond // before Close, on this instance only
	line := readLine(t, bufio.NewReader(p), "CHILD:")
	child, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "CHILD:")))
	if err != nil {
		t.Fatalf("child pid in %q: %v", line, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	assertTornDown(t, p)
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("client ended with %v, want SIGKILL", p.cmd.ProcessState)
	}
	reaped := make(chan error, 1)
	go func() {
		var ws unix.WaitStatus
		_, err := unix.Wait4(child, &ws, 0, nil)
		if err == nil && ws.Signal() != unix.SIGKILL {
			err = errors.New("child not killed: " + ws.Signal().String())
		}
		reaped <- err
	}()
	select {
	case err := <-reaped:
		if err != nil {
			t.Fatalf("child %d: %v", child, err)
		}
	case <-time.After(waitTimeout):
		t.Fatalf("child %d still running", child)
	}
	if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d still exists (kill 0: %v)", child, err)
	}
}

// TestLocalPTYChownFailureFailsSpawn: when the pty cannot be handed to
// the agent user, no client starts.
func TestLocalPTYChownFailureFailsSpawn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can chown the tty")
	}
	_, err := startLocalPTY(PTYUser{UID: 65534, GID: 65534}, PTYRequest{Cols: 80, Rows: 24, Session: "scion"}, "/bin/true")
	if err == nil || !strings.Contains(err.Error(), "chown") {
		t.Fatalf("startLocalPTY = %v, want a chown error", err)
	}
}

// TestLocalPTYRunsAsAgentUser (root only): the client runs as the agent
// user and owns its terminal.
func TestLocalPTYRunsAsAgentUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to drop privileges")
	}
	p, err := startLocalPTY(PTYUser{UID: 65534, GID: 65534}, PTYRequest{Cols: 80, Rows: 24, Session: "scion"},
		"/bin/sh", "-c", `echo "UID:$(id -u) TTY:$(stat -c %u "$(tty)")"; read x`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if line := readLine(t, bufio.NewReader(p), "UID:"); !strings.Contains(line, "UID:65534 TTY:65534") {
		t.Fatalf("client reports %q, want uid and tty owner 65534", line)
	}
}
