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

package liveness

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/creack/pty"
)

func TestSampler_AttachedTerminalGuard(t *testing.T) {
	attached := false
	s := NewSampler(Config{
		AgentUID:      1 << 30,
		IsManagedPID:  func(int) bool { return false },
		CheckAttached: func() bool { return attached },
	})
	s.procRoot = t.TempDir()

	// Not attached -> nil.
	if got := s.sample(time.Now(), state.ActivityIdle, time.Now()); got != nil {
		t.Fatalf("expected nil when not attached, got %+v", got)
	}

	// Attached -> KindAttached, SuppressStall=false, Detail="terminal attached".
	attached = true
	got := s.sample(time.Now(), state.ActivityIdle, time.Now())
	if got == nil {
		t.Fatal("expected LiveWork when attached, got nil")
	}
	if got.Kind != KindAttached {
		t.Errorf("Kind = %q, want %q", got.Kind, KindAttached)
	}
	if got.SuppressStall {
		t.Error("SuppressStall = true, want false")
	}
	if got.Detail != "terminal attached" {
		t.Errorf("Detail = %q, want %q", got.Detail, "terminal attached")
	}

	// Detached again -> nil.
	attached = false
	if got := s.sample(time.Now(), state.ActivityIdle, time.Now()); got != nil {
		t.Fatalf("expected nil after detach, got %+v", got)
	}
}

func TestTmuxAttachedChecker_PrivateSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	sock := fmt.Sprintf("scion-test-liveness-%d", time.Now().UnixNano())
	cleanEnv := func() []string {
		env := make([]string, 0, len(os.Environ())+1)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TERM=") {
				env = append(env, kv)
			}
		}
		env = append(env, "TERM=xterm-256color")
		return env
	}
	runTmux := func(args ...string) error {
		cmd := exec.Command("tmux", append([]string{"-L", sock}, args...)...)
		cmd.Env = cleanEnv()
		return cmd.Run()
	}

	if err := runTmux("new-session", "-d", "-s", "scion", "sleep", "30"); err != nil {
		t.Fatalf("tmux new-session: %v", err)
	}
	t.Cleanup(func() {
		_ = runTmux("kill-server")
	})

	check := tmuxAttachedCheckerOnSocket("scion", sock)
	if check() {
		t.Fatal("expected false before any client attaches")
	}

	// Attach a client via a PTY so tmux registers an attached client.
	attachCmd := exec.Command("tmux", "-L", sock, "attach-session", "-t", "scion")
	attachCmd.Env = cleanEnv()
	ptmx, err := pty.Start(attachCmd)
	if err != nil {
		t.Fatalf("pty.Start tmux attach: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()
	defer func() {
		_ = ptmx.Close()
		_ = attachCmd.Process.Kill()
		_, _ = attachCmd.Process.Wait()
	}()

	deadline := time.Now().Add(3 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("expected true while client is attached")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Detach by closing the PTY and terminating the attach client.
	_ = ptmx.Close()
	_ = attachCmd.Process.Kill()
	_, _ = attachCmd.Process.Wait()

	deadline = time.Now().Add(3 * time.Second)
	for check() {
		if time.Now().After(deadline) {
			t.Fatal("expected false after client detached")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
