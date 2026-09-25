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

package runtimebroker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/remotecommand"
)

// newAttachTestRequest builds a WebSocket-upgrade GET request for the given
// agent slug, matching what handleAgentAttach expects before it gets far
// enough to call ptyUpgrader.Upgrade.
func newAttachTestRequest(slug string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+slug+"/attach", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	return req
}

func decodeErrorResponse(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestHandleAgentAttach_AgentNotFound(t *testing.T) {
	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	rec := httptest.NewRecorder()
	srv.handleAgentAttach(rec, newAttachTestRequest("missing-agent"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a genuinely missing agent, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeErrorResponse(t, rec)
	if resp.Error.Code != ErrCodeAgentNotFound {
		t.Errorf("expected code %q, got %q", ErrCodeAgentNotFound, resp.Error.Code)
	}
}

func TestHandleAgentAttach_RuntimeListUnavailable(t *testing.T) {
	mgr := &mockManager{listErr: errors.New("docker ps failed: exit status 1")}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	rec := httptest.NewRecorder()
	srv.handleAgentAttach(rec, newAttachTestRequest("some-agent"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the runtime listing itself fails, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeErrorResponse(t, rec)
	if resp.Error.Code != ErrCodeRuntimeUnavailable {
		t.Errorf("expected code %q, got %q", ErrCodeRuntimeUnavailable, resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "retry") && !strings.Contains(resp.Error.Message, "temporarily") {
		t.Errorf("expected an actionable, retry-oriented message, got %q", resp.Error.Message)
	}
	// The message must not just be "Agent not found" — the whole point is
	// distinguishing a transient runtime failure from a real not-found.
	if strings.Contains(resp.Error.Message, "not found") {
		t.Errorf("runtime-unavailable message should not read like a not-found error, got %q", resp.Error.Message)
	}
}

func TestWaitForTmuxSession_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := waitForTmuxSession(ctx, "false", "nonexistent-container", "", "scion", nil, nil)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

func TestWaitForTmuxSession_TimesOut(t *testing.T) {
	// Use a very short timeout to test the timeout path quickly.
	// "false" always exits with code 1, simulating tmux has-session failure.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := waitForTmuxSession(ctx, "false", "nonexistent-container", "", "scion", nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("expected to wait at least 500ms before timing out, got %v", elapsed)
	}
}

func TestWaitForTmuxSession_SucceedsImmediately(t *testing.T) {
	// "true" always exits with code 0, simulating tmux has-session success.
	// We pass extra args that "true" ignores.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForTmuxSession(ctx, "true", "any-container", "", "scion", nil, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	// First poll is at 500ms, so it should complete around that time
	if elapsed > 2*time.Second {
		t.Errorf("expected quick completion, took %v", elapsed)
	}
}

func TestActiveWindowOSC(t *testing.T) {
	tests := []struct {
		name     string
		window   string
		expected string
	}{
		{"agent window", "agent", "\033]7337;tmuxwindow=agent\007"},
		{"shell window", "shell", "\033]7337;tmuxwindow=shell\007"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(activeWindowOSC(tc.window))
			if got != tc.expected {
				t.Errorf("activeWindowOSC(%q) = %q, want %q", tc.window, got, tc.expected)
			}
		})
	}
}

func TestQueryTmuxActiveWindow_CommandFails(t *testing.T) {
	// "false" always exits 1, simulating tmux not available
	result := queryTmuxActiveWindow(context.Background(), "false", "test-container", "scion")
	if result != "" {
		t.Errorf("expected empty string on failure, got %q", result)
	}
}

func TestQueryTmuxActiveWindow_ContextTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	result := queryTmuxActiveWindow(ctx, "echo", "test-container", "scion")
	if result != "" {
		t.Errorf("expected empty string on cancelled context, got %q", result)
	}
}

func TestK8sSizeQueue_ReturnsInitialSize(t *testing.T) {
	q := &k8sSizeQueue{
		resizeCh: make(chan [2]int, 1),
		closeCh:  make(chan struct{}),
		ctx:      context.Background(),
		initial:  &remotecommand.TerminalSize{Width: 120, Height: 40},
	}

	size := q.Next()
	if size == nil {
		t.Fatal("expected initial size, got nil")
	}
	if size.Width != 120 || size.Height != 40 {
		t.Errorf("expected 120x40, got %dx%d", size.Width, size.Height)
	}

	// initial should be consumed
	if q.initial != nil {
		t.Error("expected initial to be nil after first call")
	}
}

func TestK8sSizeQueue_ReturnsResizeEvents(t *testing.T) {
	resizeCh := make(chan [2]int, 1)
	q := &k8sSizeQueue{
		resizeCh: resizeCh,
		closeCh:  make(chan struct{}),
		ctx:      context.Background(),
		initial:  nil, // no initial size
	}

	resizeCh <- [2]int{200, 50}

	size := q.Next()
	if size == nil {
		t.Fatal("expected resize event, got nil")
	}
	if size.Width != 200 || size.Height != 50 {
		t.Errorf("expected 200x50, got %dx%d", size.Width, size.Height)
	}
}

func TestK8sSizeQueue_ReturnsNilOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	q := &k8sSizeQueue{
		resizeCh: make(chan [2]int),
		closeCh:  make(chan struct{}),
		ctx:      ctx,
		initial:  nil,
	}

	size := q.Next()
	if size != nil {
		t.Errorf("expected nil on cancelled context, got %+v", size)
	}
}

func TestK8sSizeQueue_ReturnsNilOnClose(t *testing.T) {
	closeCh := make(chan struct{})
	close(closeCh)

	q := &k8sSizeQueue{
		resizeCh: make(chan [2]int),
		closeCh:  closeCh,
		ctx:      context.Background(),
		initial:  nil,
	}

	size := q.Next()
	if size != nil {
		t.Errorf("expected nil on close, got %+v", size)
	}
}

func TestSanitizeExecUser(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty falls back", in: "", want: "scion"},
		{name: "valid scion", in: "scion", want: "scion"},
		{name: "valid root", in: "root", want: "root"},
		{name: "valid alphanumeric with hyphen", in: "agent-user_1", want: "agent-user_1"},
		{name: "shell metachar rejected", in: "scion;rm -rf /", want: "scion"},
		{name: "command substitution rejected", in: "$(whoami)", want: "scion"},
		{name: "quote rejected", in: `bad"name`, want: "scion"},
		{name: "space rejected", in: "two words", want: "scion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeExecUser(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeExecUser(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- activateTmuxSetTitles tests ---

// TestActivateTmuxSetTitles_FailureBestEffort verifies that calling
// activateTmuxSetTitles with a non-existent runtime binary does not panic
// or block — the function returns silently after logging a debug message.
func TestActivateTmuxSetTitles_FailureBestEffort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		activateTmuxSetTitles(ctx, "nonexistent-runtime-binary-12345", "fake-container", "scion")
		close(done)
	}()

	select {
	case <-done:
		// Success — returned without panic or hang
	case <-time.After(4 * time.Second):
		t.Fatal("activateTmuxSetTitles blocked on bad runtime — expected prompt return")
	}
}

// TestActivateTmuxSetTitles_CancelledContext verifies that a cancelled context
// causes activateTmuxSetTitles to return promptly without blocking.
func TestActivateTmuxSetTitles_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	done := make(chan struct{})
	go func() {
		activateTmuxSetTitles(ctx, "docker", "fake-container", "scion")
		close(done)
	}()

	select {
	case <-done:
		// Success — returned promptly
	case <-time.After(4 * time.Second):
		t.Fatal("activateTmuxSetTitles blocked on cancelled context — expected prompt return")
	}
}

// tmuxAvailable returns true if tmux is on PATH.
func tmuxAvailable(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("tmux")
	return err == nil
}

// writeFakeDockerScript creates a shell script that strips the docker exec
// prefix (exec --user <user> <container>) and runs the remaining args,
// inserting -S <socket> for tmux commands. Returns the path to the script.
func writeFakeDockerScript(t *testing.T, socketPath string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-docker")
	content := fmt.Sprintf(`#!/bin/sh
# Strip: exec --user <user> <container> → remaining args are the command
shift 4
# Insert tmux socket for tmux commands
case "$1" in
  tmux) shift; exec tmux -S %q "$@" ;;
  *)    exec "$@" ;;
esac
`, socketPath)
	err := os.WriteFile(script, []byte(content), 0755)
	require.NoError(t, err)
	return script
}

// startTmuxServer creates a tmux server with the given socket, session name,
// and initial window. Returns a cleanup function.
func startTmuxServer(t *testing.T, socketPath, sessionName, windowName string) {
	t.Helper()
	cmd := exec.Command("tmux", "-S", socketPath, "new-session", "-d",
		"-s", sessionName, "-n", windowName)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "tmux new-session failed: %s", string(out))
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", socketPath, "kill-server").Run()
	})
}

// TestActivateTmuxSetTitles_RealTmux verifies that activateTmuxSetTitles
// correctly enables set-titles and set-titles-string on a real tmux server.
func TestActivateTmuxSetTitles_RealTmux(t *testing.T) {
	if !tmuxAvailable(t) {
		t.Skip("tmux not available")
	}

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "tmux.sock")

	// Start tmux server with session "scion"
	startTmuxServer(t, socketPath, "scion", "agent")

	// Write a fake docker script that forwards tmux commands with our socket
	fakeDocker := writeFakeDockerScript(t, socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Call activateTmuxSetTitles through the fake docker wrapper
	activateTmuxSetTitles(ctx, fakeDocker, "fake-container", "scion")

	// Verify set-titles is on (session-level option, query with -t)
	out, err := exec.CommandContext(ctx, "tmux", "-S", socketPath,
		"show-option", "-t", "scion", "-v", "set-titles").CombinedOutput()
	require.NoError(t, err, "show-option set-titles failed: %s", string(out))
	require.Equal(t, "on", strings.TrimSpace(string(out)))

	// Verify set-titles-string is #W
	out, err = exec.CommandContext(ctx, "tmux", "-S", socketPath,
		"show-option", "-t", "scion", "-v", "set-titles-string").CombinedOutput()
	require.NoError(t, err, "show-option set-titles-string failed: %s", string(out))
	require.Equal(t, "#W", strings.TrimSpace(string(out)))
}

// TestActivateTmuxSetTitles_Idempotent verifies that calling
// activateTmuxSetTitles twice on the same session is safe (no error, same state).
func TestActivateTmuxSetTitles_Idempotent(t *testing.T) {
	if !tmuxAvailable(t) {
		t.Skip("tmux not available")
	}

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "tmux.sock")

	startTmuxServer(t, socketPath, "scion", "agent")
	fakeDocker := writeFakeDockerScript(t, socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First call
	activateTmuxSetTitles(ctx, fakeDocker, "fake-container", "scion")

	// Second call — should not error
	activateTmuxSetTitles(ctx, fakeDocker, "fake-container", "scion")

	// Verify state is still correct
	out, err := exec.CommandContext(ctx, "tmux", "-S", socketPath,
		"show-option", "-t", "scion", "-v", "set-titles").CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, "on", strings.TrimSpace(string(out)))

	out, err = exec.CommandContext(ctx, "tmux", "-S", socketPath,
		"show-option", "-t", "scion", "-v", "set-titles-string").CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, "#W", strings.TrimSpace(string(out)))
}

// TestActivateTmuxSetTitles_NoSessionFallback verifies that calling
// activateTmuxSetTitles when the "scion" session does not exist returns
// silently without blocking or panicking.
func TestActivateTmuxSetTitles_NoSessionFallback(t *testing.T) {
	if !tmuxAvailable(t) {
		t.Skip("tmux not available")
	}

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "tmux.sock")

	// Start tmux with a DIFFERENT session name — "scion" does not exist
	startTmuxServer(t, socketPath, "other-session", "window0")
	fakeDocker := writeFakeDockerScript(t, socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		activateTmuxSetTitles(ctx, fakeDocker, "fake-container", "scion")
		close(done)
	}()

	select {
	case <-done:
		// Success — returned without blocking
	case <-time.After(5 * time.Second):
		t.Fatal("activateTmuxSetTitles blocked when session missing — expected prompt return")
	}
}

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
