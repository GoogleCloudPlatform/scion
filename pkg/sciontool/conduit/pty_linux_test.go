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

//go:build linux

package conduit

// Linux-only pty tests: they use PR_SET_CHILD_SUBREAPER and GNU stat.

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
