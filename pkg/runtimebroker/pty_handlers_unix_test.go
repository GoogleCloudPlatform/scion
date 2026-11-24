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

package runtimebroker

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
)

// readOSC0 reads from the given reader, scanning for an OSC 0 title sequence
// (\033]0;<title>\007 or \033]0;<title>\033\\). Returns (title, rawBytes) on
// success. Times out after the given duration.
func readOSC0(t *testing.T, r *os.File, timeout time.Duration) (string, string) {
	t.Helper()

	type result struct {
		title string
		raw   string
	}
	ch := make(chan result, 1)

	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Split(bufio.ScanBytes)
		var buf []byte
		inOSC := false
		for scanner.Scan() {
			b := scanner.Bytes()[0]
			buf = append(buf, b)

			if !inOSC {
				// Look for \033]0; sequence start
				if len(buf) >= 4 {
					tail := string(buf[len(buf)-4:])
					if tail == "\033]0;" {
						inOSC = true
						buf = nil // reset to capture title only
					}
				}
			} else {
				// Look for BEL (\007) or ST (\033\\) terminator
				if b == '\007' {
					title := string(buf[:len(buf)-1]) // strip BEL
					ch <- result{title: title, raw: string(buf)}
					return
				}
				if len(buf) >= 2 && buf[len(buf)-2] == '\033' && buf[len(buf)-1] == '\\' {
					title := string(buf[:len(buf)-2]) // strip ST
					ch <- result{title: title, raw: string(buf)}
					return
				}
			}
		}
	}()

	select {
	case r := <-ch:
		return r.title, r.raw
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for OSC 0 title (waited %v)", timeout)
		return "", ""
	}
}

// TestActivateTmuxSetTitles_WindowSwitchOSC0 verifies that after activating
// set-titles, switching tmux windows emits OSC 0 title updates through the PTY.
// This is the end-to-end proof that the toolbar sync mechanism works.
func TestActivateTmuxSetTitles_WindowSwitchOSC0(t *testing.T) {
	if !tmuxAvailable(t) {
		t.Skip("tmux not available")
	}

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "tmux.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create tmux server with session "scion", first window "agent"
	startTmuxServer(t, socketPath, "scion", "agent")

	// Add a second window "shell"
	cmd := exec.CommandContext(ctx, "tmux", "-S", socketPath,
		"new-window", "-t", "scion", "-n", "shell")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "new-window failed: %s", string(out))

	// Switch back to window 0 (agent) so we start from a known state
	cmd = exec.CommandContext(ctx, "tmux", "-S", socketPath,
		"select-window", "-t", "scion:0")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "select-window failed: %s", string(out))

	// Enable set-titles via activateTmuxSetTitles using the fake docker wrapper
	fakeDocker := writeFakeDockerScript(t, socketPath)
	activateTmuxSetTitles(ctx, fakeDocker, "fake-container", "scion")

	// Attach to the tmux session with a real PTY so we receive OSC 0 output.
	// Use TERM=xterm-256color (same as production attach).
	attachCmd := exec.CommandContext(ctx, "tmux", "-S", socketPath,
		"attach-session", "-t", "scion")
	attachCmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptyMaster, err := startPTYForTest(t, attachCmd)
	require.NoError(t, err, "failed to start PTY for tmux attach")

	// Give tmux a moment to initialize the attached client
	time.Sleep(500 * time.Millisecond)

	// Drain the initial OSC 0 emitted on attach (shows current window "agent").
	// This prevents confusing the initial title with the window-switch title.
	initTitle, _ := readOSC0(t, ptyMaster, 5*time.Second)
	t.Logf("initial OSC 0 title on attach: %q", initTitle)

	// Switch to window "shell" — should emit OSC 0 with title "shell"
	cmd = exec.CommandContext(ctx, "tmux", "-S", socketPath,
		"select-window", "-t", "scion:shell")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "select-window shell failed: %s", string(out))

	title1, raw1 := readOSC0(t, ptyMaster, 5*time.Second)
	require.NotEmpty(t, title1, "OSC 0 title after switch to shell window was empty (raw: %q)", raw1)
	require.Contains(t, title1, "shell",
		"expected OSC 0 title to contain 'shell', got %q", title1)

	// Switch to window "agent" — should emit OSC 0 with title "agent"
	cmd = exec.CommandContext(ctx, "tmux", "-S", socketPath,
		"select-window", "-t", "scion:agent")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "select-window agent failed: %s", string(out))

	title2, raw2 := readOSC0(t, ptyMaster, 5*time.Second)
	require.NotEmpty(t, title2, "OSC 0 title after switch to agent window was empty (raw: %q)", raw2)
	require.Contains(t, title2, "agent",
		"expected OSC 0 title to contain 'agent', got %q", title2)
}

// startPTYForTest starts a command with a PTY and returns the master file
// descriptor. The command is killed on test cleanup.
func startPTYForTest(t *testing.T, cmd *exec.Cmd) (*os.File, error) {
	t.Helper()

	master, slave, err := openPTYPair(t)
	if err != nil {
		return nil, err
	}

	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, err
	}

	// Close slave in parent — only the child uses it
	_ = slave.Close()

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = master.Close()
	})

	return master, nil
}

// openPTYPair opens a PTY master/slave pair using the creack/pty package.
func openPTYPair(t *testing.T) (master, slave *os.File, err error) {
	t.Helper()
	m, s, e := pty.Open()
	if e != nil {
		return nil, nil, fmt.Errorf("pty.Open: %w", e)
	}
	return m, s, nil
}
