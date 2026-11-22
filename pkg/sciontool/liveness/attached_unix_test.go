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

package liveness

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/creack/pty"
)

func TestBuildTmuxListClientsCmd_AgentCredentialAndTrustedPath(t *testing.T) {
	expectedTmux, err := rootexec.Resolve("tmux")
	if err != nil {
		t.Skipf("tmux not in trusted rootexec dirs: %v", err)
	}

	t.Setenv("TMUX_TMPDIR", "/tmp/scion-tmux-test")
	t.Setenv("TMUX", "/tmp/ambient-tmux,123,0")

	cmd, err := buildTmuxListClientsCmd(context.Background(), "scion", "", 1000, 1001)
	if err != nil {
		t.Fatalf("buildTmuxListClientsCmd: %v", err)
	}

	if cmd.Path != expectedTmux {
		t.Errorf("cmd.Path = %q, want trusted rootexec path %q", cmd.Path, expectedTmux)
	}
	wantArgs := []string{expectedTmux, "list-clients", "-t", "scion"}
	if !slices.Equal(cmd.Args, wantArgs) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, wantArgs)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("expected SysProcAttr.Credential to be set for uid/gid > 0")
	}
	if cmd.SysProcAttr.Credential.Uid != 1000 {
		t.Errorf("Credential.Uid = %d, want 1000", cmd.SysProcAttr.Credential.Uid)
	}
	if cmd.SysProcAttr.Credential.Gid != 1001 {
		t.Errorf("Credential.Gid = %d, want 1001", cmd.SysProcAttr.Credential.Gid)
	}
	if !slices.Contains(cmd.Env, "TMUX_TMPDIR=/tmp/scion-tmux-test") {
		t.Errorf("cmd.Env missing TMUX_TMPDIR: %v", cmd.Env)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TMUX=") {
			t.Errorf("cmd.Env leaked ambient TMUX variable: %q", kv)
		}
	}

	// uid=0, gid=0 leaves SysProcAttr nil.
	rootCmd, err := buildTmuxListClientsCmd(context.Background(), "scion", "privsock", 0, 0)
	if err != nil {
		t.Fatalf("buildTmuxListClientsCmd (root): %v", err)
	}
	if rootCmd.SysProcAttr != nil {
		t.Errorf("expected nil SysProcAttr when uid/gid == 0, got %+v", rootCmd.SysProcAttr)
	}
}

func TestTmuxAttachedChecker_PrivateSocket(t *testing.T) {
	if _, err := rootexec.Resolve("tmux"); err != nil {
		t.Skipf("tmux not in trusted rootexec dirs: %v", err)
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

	check := tmuxAttachedCheckerOnSocket("scion", sock, 0, 0)
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
