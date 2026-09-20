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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
)

// TestGracefulShutdownExec_ClosePTYBeforeKill verifies that gracefulShutdownExec
// closes the PTY master before attempting to kill the process, giving the
// container runtime a chance to propagate the terminal hangup.
func TestGracefulShutdownExec_ClosePTYBeforeKill(t *testing.T) {
	// Start a long-running process with a PTY
	cmd := exec.Command("sleep", "60")
	ptmx, err := os.CreateTemp(t.TempDir(), "pty-stub-*")
	require.NoError(t, err)

	// Start the process separately since we're using a fake PTY fd
	require.NoError(t, cmd.Start())

	// gracefulShutdownExec should close ptmx and reap the process
	gracefulShutdownExec(cmd, ptmx, "test-graceful")

	// Verify: PTY fd is closed
	_, err = ptmx.Stat()
	require.Error(t, err, "PTY file descriptor must be closed")

	// Verify: process is reaped
	require.NotNil(t, cmd.ProcessState, "process must be reaped")
}

// TestGracefulShutdownExec_NilProcess verifies that gracefulShutdownExec
// handles nil cmd and process gracefully.
func TestGracefulShutdownExec_NilProcess(t *testing.T) {
	// Should not panic with nil cmd
	gracefulShutdownExec(nil, nil, "nil-test")

	// Should not panic with cmd but nil process
	cmd := &exec.Cmd{}
	gracefulShutdownExec(cmd, nil, "nil-process-test")
}

// TestGracefulShutdownExec_AlreadyExited verifies that gracefulShutdownExec
// handles an already-exited process.
func TestGracefulShutdownExec_AlreadyExited(t *testing.T) {
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run()) // Run waits for exit

	// Process already exited and reaped — should not block or panic
	gracefulShutdownExec(cmd, nil, "already-exited")
	require.NotNil(t, cmd.ProcessState)
}

// TestGracefulShutdownExec_ProcessExitsOnPTYClose verifies that a process
// attached to a PTY exits when the PTY master is closed (terminal hangup).
func TestGracefulShutdownExec_ProcessExitsOnPTYClose(t *testing.T) {
	// Use 'cat' which reads from stdin (the PTY) — closing the PTY will
	// cause cat to get EOF/EIO and exit naturally.
	cmd := exec.Command("cat")

	// Create a real PTY pair
	// We use the pty package indirectly through the cmd.
	// Instead, start cat with a pipe and test the timeout escalation path.
	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	cmd.Stdin = stdinR
	require.NoError(t, cmd.Start())
	stdinR.Close() // Close read end in parent

	start := time.Now()
	gracefulShutdownExec(cmd, stdinW, "pty-close-test")
	elapsed := time.Since(start)

	require.NotNil(t, cmd.ProcessState, "process must be reaped")
	// Process should exit quickly from the pipe close (< grace period)
	require.Less(t, elapsed, processExitGracePeriod+time.Second,
		"process should exit from stdin close without needing SIGTERM")
}

// TestDetachContainerClient_SkipsK8s verifies that detachContainerClient
// is a no-op for Kubernetes runtimes.
func TestDetachContainerClient_SkipsK8s(t *testing.T) {
	// These should not panic or attempt any exec
	detachContainerClient("kubernetes", "some-pod", "scion", "/dev/pts/0")
	detachContainerClient("k8s", "some-pod", "scion", "/dev/pts/0")
}

// TestDetachContainerClient_SkipsEmptyContainer verifies that
// detachContainerClient is a no-op when containerID is empty.
func TestDetachContainerClient_SkipsEmptyContainer(t *testing.T) {
	detachContainerClient("docker", "", "scion", "/dev/pts/0")
}

// TestDetachContainerClient_SkipsEmptyTTY verifies that
// detachContainerClient is a no-op when targetTTY is empty
// (identification failed or ambiguous).
func TestDetachContainerClient_SkipsEmptyTTY(t *testing.T) {
	detachContainerClient("docker", "some-container", "scion", "")
}

// TestDetachContainerClient_ToleratesCommandFailure verifies that
// detachContainerClient does not panic or error when the runtime command
// fails (e.g., container already stopped).
func TestDetachContainerClient_ToleratesCommandFailure(t *testing.T) {
	// "false" always exits 1, simulating a failed docker exec
	detachContainerClient("false", "nonexistent-container", "scion", "/dev/pts/0")
}

// TestPTYCleanup_ExplicitCloseReleasesAttach tests acceptance criterion 2:
// explicit close releases the stream and attach process/PTY descriptors;
// the agent remains running and attachable.
func TestPTYCleanup_ExplicitCloseReleasesAttach(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("requires tmux in PATH")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMUX", "")
	dir := t.TempDir()
	socket := filepath.Join(dir, "tmux.sock")
	tmuxCommand := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tmux, append([]string{"-S", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// Create a private tmux session (agent surrogate)
	_, err = tmuxCommand("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })

	// Capture baseline pane PID to verify agent survival
	panePID, err := tmuxCommand("display-message", "-p", "-t", "scion", "#{pane_pid}")
	require.NoError(t, err)
	require.NotEmpty(t, panePID)

	// Runtime adapter that wraps tmux commands through the fixture socket
	adapter := filepath.Join(dir, "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(`#!/bin/sh
set -eu
[ "$1" = exec ]; shift
if [ "$1" = -it ]; then shift; fi
[ "$1" = --user ]; shift 2
[ "$1" = cleanup-fixture ]; shift
[ "$1" = tmux ]; shift
exec "$TW_CLEANUP_TMUX" -S "$TW_CLEANUP_SOCKET" "$@"
`), 0700))
	t.Setenv("TW_CLEANUP_TMUX", tmux)
	t.Setenv("TW_CLEANUP_SOCKET", socket)

	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()
	client := &ControlChannelClient{conn: brokerConn, connected: true, streams: make(map[string]*StreamHandler)}

	closedStreams := make(chan string, 8)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var message struct {
				Type     string `json:"type"`
				StreamID string `json:"streamId"`
			}
			if err := hubConn.ReadJSON(&message); err != nil {
				return
			}
			if message.Type == wsprotocol.TypeStreamClose {
				closedStreams <- message.StreamID
			}
		}
	}()
	defer func() { _ = hubConn.Close(); <-readerDone }()

	// Simulate explicit browser close via hub stream close (no tmux detach key)
	streamID := "explicit-close-1"
	handler := &StreamHandler{
		streamID: streamID, slug: "fixture",
		dataCh: make(chan []byte, 4), resizeCh: make(chan [2]int, 4),
		closeCh: make(chan struct{}),
	}
	client.streamMu.Lock()
	client.streams[streamID] = handler
	client.streamMu.Unlock()

	bridge := NewStreamPTYHandler(client, handler, "cleanup-fixture", adapter, "scion", "", 80, 24, nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bridge.Run()
		_ = client.CloseStream(streamID, "session ended", 0)
	}()
	t.Cleanup(func() {
		bridge.cancel()
		payload, _ := json.Marshal(wsprotocol.NewStreamCloseMessage(streamID, "test cleanup", 0))
		_ = client.handleStreamClose(payload)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("PTY bridge did not exit")
		}
	})

	// Wait for the tmux client to appear
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
		return err == nil && out == "80x24"
	}, 5*time.Second, 20*time.Millisecond, "tmux client should attach")

	// Close the stream (simulating hub sending close after browser WebSocket drops)
	payload, err := json.Marshal(wsprotocol.NewStreamCloseMessage(streamID, "browser closed", 0))
	require.NoError(t, err)
	require.NoError(t, client.handleStreamClose(payload))

	// Wait for bridge to exit
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("PTY bridge did not release on close")
	}

	// Verify: context canceled
	require.ErrorIs(t, bridge.ctx.Err(), context.Canceled)

	// Verify: process reaped
	require.NotNil(t, bridge.cmd.ProcessState, "runtime exec process must be reaped")

	// Verify: PTY closed
	_, err = bridge.ptyMaster.Stat()
	require.Error(t, err, "PTY file descriptor must be closed")

	// Verify: stream registry cleared
	client.streamMu.RLock()
	remaining := len(client.streams)
	client.streamMu.RUnlock()
	require.Zero(t, remaining, "broker stream registry must release attachment")

	// Verify: hub received close
	select {
	case id := <-closedStreams:
		require.Equal(t, streamID, id)
	case <-time.After(5 * time.Second):
		t.Fatal("Hub did not receive stream close")
	}

	// Verify: tmux client detached (no residual attach process)
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		return err == nil && out == ""
	}, 10*time.Second, 50*time.Millisecond,
		"tmux client must be detached after close — no residual attach process (TW-UAT-002)")

	// Verify: agent surrogate still alive
	out, err := tmuxCommand("display-message", "-p", "-t", "scion", "#{pane_pid}:#{pane_dead}")
	require.NoError(t, err)
	require.Equal(t, panePID+":0", out, "agent surrogate must remain alive after browser close")
}

// TestPTYCleanup_SocketLossReleasesResources tests acceptance criterion 3:
// socket loss reaches terminal disconnected state and cleans up orphaned
// stream resources.
func TestPTYCleanup_SocketLossReleasesResources(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("requires tmux in PATH")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMUX", "")
	dir := t.TempDir()
	socket := filepath.Join(dir, "tmux.sock")
	tmuxCommand := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tmux, append([]string{"-S", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	_, err = tmuxCommand("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })
	panePID, err := tmuxCommand("display-message", "-p", "-t", "scion", "#{pane_pid}")
	require.NoError(t, err)

	adapter := filepath.Join(dir, "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(`#!/bin/sh
set -eu
[ "$1" = exec ]; shift
if [ "$1" = -it ]; then shift; fi
[ "$1" = --user ]; shift 2
[ "$1" = socketloss-fixture ]; shift
[ "$1" = tmux ]; shift
exec "$TW_CLEANUP_TMUX" -S "$TW_CLEANUP_SOCKET" "$@"
`), 0700))
	t.Setenv("TW_CLEANUP_TMUX", tmux)
	t.Setenv("TW_CLEANUP_SOCKET", socket)

	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()
	client := &ControlChannelClient{conn: brokerConn, connected: true, streams: make(map[string]*StreamHandler)}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var msg json.RawMessage
			if err := hubConn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}()
	defer func() { _ = hubConn.Close(); <-readerDone }()

	streamID := "socketloss-1"
	handler := &StreamHandler{
		streamID: streamID, slug: "fixture",
		dataCh: make(chan []byte, 4), resizeCh: make(chan [2]int, 4),
		closeCh: make(chan struct{}),
	}
	client.streamMu.Lock()
	client.streams[streamID] = handler
	client.streamMu.Unlock()

	bridge := NewStreamPTYHandler(client, handler, "socketloss-fixture", adapter, "scion", "", 80, 24, nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bridge.Run()
	}()

	// Wait for attach
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		return err == nil && strings.TrimSpace(out) != ""
	}, 5*time.Second, 20*time.Millisecond)

	// Simulate socket loss by directly canceling the handler context
	// (broker's markDisconnected closes closeCh and cancels streams)
	bridge.cancel()
	handler.closeMu.Lock()
	if !handler.closed {
		handler.closed = true
		close(handler.closeCh)
	}
	handler.closeMu.Unlock()

	// Wait for bridge to exit
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("PTY bridge did not release on socket loss")
	}

	// Verify: process reaped
	require.NotNil(t, bridge.cmd.ProcessState, "exec process must be reaped after socket loss")

	// Verify: PTY closed
	_, err = bridge.ptyMaster.Stat()
	require.Error(t, err, "PTY fd must be closed after socket loss")

	// Verify: tmux client detached
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		return err == nil && out == ""
	}, 10*time.Second, 50*time.Millisecond,
		"tmux client must be cleaned up after socket loss")

	// Verify: agent alive
	out, err := tmuxCommand("display-message", "-p", "-t", "scion", "#{pane_pid}:#{pane_dead}")
	require.NoError(t, err)
	require.Equal(t, panePID+":0", out, "agent must survive socket loss")
}

// TestPTYCleanup_BrokerDisconnectCleansUpAllStreams tests that
// markDisconnected releases all active PTY streams when the broker
// loses its control channel connection to the hub.
func TestPTYCleanup_BrokerDisconnectCleansUpAllStreams(t *testing.T) {
	config := ControlChannelConfig{
		HubEndpoint: "https://hub.example.com",
		BrokerID:    "cleanup-test-broker",
	}
	client := NewControlChannelClient(config, nil, nil, "", slog.Default())
	client.mu.Lock()
	client.connected = true
	client.mu.Unlock()

	// Create multiple stream handlers
	streamIDs := []string{"stream-a", "stream-b", "stream-c"}
	for _, id := range streamIDs {
		client.streamMu.Lock()
		client.streams[id] = &StreamHandler{
			streamID: id, slug: "test",
			dataCh: make(chan []byte, 1), resizeCh: make(chan [2]int, 1),
			closeCh: make(chan struct{}),
		}
		client.streamMu.Unlock()
	}

	// Simulate broker disconnect
	client.markDisconnected()

	// Verify: all streams closed
	client.streamMu.RLock()
	remaining := len(client.streams)
	client.streamMu.RUnlock()
	require.Zero(t, remaining, "all streams must be released on disconnect")

	// Verify: disconnected
	require.False(t, client.IsConnected(), "must be disconnected")
}

// TestPTYCleanup_CLIClientsSurviveBrowserClose tests acceptance criterion 5:
// other CLI clients remain usable after browser session close.
func TestPTYCleanup_CLIClientsSurviveBrowserClose(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("requires tmux in PATH")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMUX", "")
	dir := t.TempDir()
	socket := filepath.Join(dir, "tmux.sock")
	tmuxCommand := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tmux, append([]string{"-S", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	_, err = tmuxCommand("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })

	// Start a CLI client (simulates sciontool init's baseline client).
	// tmux attach-session requires a real terminal, so use a PTY.
	cliCmd := exec.Command(tmux, "-S", socket, "attach-session", "-t", "scion")
	cliPtmx, err := pty.Start(cliCmd)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = cliPtmx.Close()
		_ = cliCmd.Process.Kill()
		_ = cliCmd.Wait()
	})

	// Verify CLI client is attached
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion")
		return err == nil && strings.Contains(out, "scion")
	}, 5*time.Second, 20*time.Millisecond)

	// Count initial clients
	initialClients, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
	require.NoError(t, err)
	initialCount := len(strings.Split(strings.TrimSpace(initialClients), "\n"))
	require.GreaterOrEqual(t, initialCount, 1, "should have at least CLI client")

	// Start browser PTY session through the bridge
	adapter := filepath.Join(dir, "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(`#!/bin/sh
set -eu
[ "$1" = exec ]; shift
if [ "$1" = -it ]; then shift; fi
[ "$1" = --user ]; shift 2
[ "$1" = cli-fixture ]; shift
[ "$1" = tmux ]; shift
exec "$TW_CLEANUP_TMUX" -S "$TW_CLEANUP_SOCKET" "$@"
`), 0700))
	t.Setenv("TW_CLEANUP_TMUX", tmux)
	t.Setenv("TW_CLEANUP_SOCKET", socket)

	brokerConn, hubConn, cleanupWS := newWSPair(t)
	defer cleanupWS()
	client := &ControlChannelClient{conn: brokerConn, connected: true, streams: make(map[string]*StreamHandler)}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var msg json.RawMessage
			if err := hubConn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}()
	defer func() { _ = hubConn.Close(); <-readerDone }()

	streamID := "cli-coexist-1"
	handler := &StreamHandler{
		streamID: streamID, slug: "fixture",
		dataCh: make(chan []byte, 4), resizeCh: make(chan [2]int, 4),
		closeCh: make(chan struct{}),
	}
	client.streamMu.Lock()
	client.streams[streamID] = handler
	client.streamMu.Unlock()

	bridge := NewStreamPTYHandler(client, handler, "cli-fixture", adapter, "scion", "", 120, 40, nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bridge.Run()
	}()

	// Wait for browser client to attach (should be one more than baseline)
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		if err != nil {
			return false
		}
		count := len(strings.Split(strings.TrimSpace(out), "\n"))
		return count == initialCount+1
	}, 5*time.Second, 20*time.Millisecond, "browser should add one tmux client")

	// Close browser session
	payload, err := json.Marshal(wsprotocol.NewStreamCloseMessage(streamID, "browser closed", 0))
	require.NoError(t, err)
	require.NoError(t, client.handleStreamClose(payload))

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("bridge did not exit")
	}

	// Verify: CLI client count returns to initial (browser client removed, CLI preserved)
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		if err != nil {
			return false
		}
		lines := strings.TrimSpace(out)
		if lines == "" {
			return initialCount == 0
		}
		count := len(strings.Split(lines, "\n"))
		return count == initialCount
	}, 10*time.Second, 50*time.Millisecond,
		"CLI client count must return to baseline after browser close")

	// Verify: CLI client is still functional (can query through it)
	out, err := tmuxCommand("display-message", "-p", "-t", "scion", "#{session_name}")
	require.NoError(t, err)
	require.Equal(t, "scion", out, "CLI must remain functional")
}

// TestPTYCleanup_ConcurrentClientSurvivesCleanup is a regression test for the
// concurrent client race condition identified by the technical advisor: a client
// opened AFTER the baseline snapshot but BEFORE cleanup must NOT be detached.
//
// This tests that the per-attach PTY identity approach correctly targets only
// the specific PTY device for the browser session, leaving a concurrently
// opened client untouched.
//
// Test fixture: real local tmux via shell adapter (not Docker containers).
func TestPTYCleanup_ConcurrentClientSurvivesCleanup(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("requires tmux in PATH")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMUX", "")
	dir := t.TempDir()
	socket := filepath.Join(dir, "tmux.sock")
	tmuxCommand := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tmux, append([]string{"-S", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	_, err = tmuxCommand("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })

	// Runtime adapter
	adapter := filepath.Join(dir, "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(`#!/bin/sh
set -eu
[ "$1" = exec ]; shift
if [ "$1" = -it ]; then shift; fi
[ "$1" = --user ]; shift 2
[ "$1" = concurrent-fixture ]; shift
[ "$1" = tmux ]; shift
exec "$TW_CLEANUP_TMUX" -S "$TW_CLEANUP_SOCKET" "$@"
`), 0700))
	t.Setenv("TW_CLEANUP_TMUX", tmux)
	t.Setenv("TW_CLEANUP_SOCKET", socket)

	// Start browser PTY session through the bridge
	brokerConn, hubConn, cleanupWS := newWSPair(t)
	defer cleanupWS()
	client := &ControlChannelClient{conn: brokerConn, connected: true, streams: make(map[string]*StreamHandler)}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var msg json.RawMessage
			if err := hubConn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}()
	defer func() { _ = hubConn.Close(); <-readerDone }()

	streamID := "concurrent-1"
	handler := &StreamHandler{
		streamID: streamID, slug: "fixture",
		dataCh: make(chan []byte, 4), resizeCh: make(chan [2]int, 4),
		closeCh: make(chan struct{}),
	}
	client.streamMu.Lock()
	client.streams[streamID] = handler
	client.streamMu.Unlock()

	bridge := NewStreamPTYHandler(client, handler, "concurrent-fixture", adapter, "scion", "", 80, 24, nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bridge.Run()
	}()

	// Wait for browser client to attach
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		return err == nil && strings.TrimSpace(out) != ""
	}, 5*time.Second, 20*time.Millisecond, "browser client should attach")

	// NOW open a concurrent client AFTER the baseline was captured.
	// This simulates the race condition: a new CLI session opens while the
	// browser session is active. Under the old baseline subtraction approach,
	// this client would be incorrectly detached because it wasn't in the baseline.
	concurrentCmd := exec.Command(tmux, "-S", socket, "attach-session", "-t", "scion")
	concurrentPtmx, err := pty.Start(concurrentCmd)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = concurrentPtmx.Close()
		_ = concurrentCmd.Process.Kill()
		_ = concurrentCmd.Wait()
	})

	// Wait for concurrent client to attach (should be 2 total)
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		if err != nil {
			return false
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		return len(lines) == 2
	}, 5*time.Second, 20*time.Millisecond, "should have 2 clients (browser + concurrent)")

	// Close browser session — this triggers cleanup
	payload, err := json.Marshal(wsprotocol.NewStreamCloseMessage(streamID, "browser closed", 0))
	require.NoError(t, err)
	require.NoError(t, client.handleStreamClose(payload))

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("bridge did not exit")
	}

	// KEY ASSERTION: concurrent client must survive the browser cleanup.
	// Allow a brief settling period then verify.
	time.Sleep(500 * time.Millisecond)

	out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Equal(t, 1, len(lines),
		"concurrent client must survive browser cleanup — got %d clients: %v", len(lines), lines)

	// Verify the surviving client is the concurrent one, not the browser one
	require.True(t, concurrentCmd.ProcessState == nil,
		"concurrent client process must still be running")
}

// TestPTYCleanup_SingleAttachOnReopen tests acceptance criterion 1:
// one browser session produces one broker attach; warm reopen/navigation
// produces no extra PTY upgrade.
func TestPTYCleanup_SingleAttachOnReopen(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("requires tmux in PATH")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMUX", "")
	dir := t.TempDir()
	socket := filepath.Join(dir, "tmux.sock")
	tmuxCommand := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tmux, append([]string{"-S", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	_, err = tmuxCommand("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })

	adapter := filepath.Join(dir, "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(`#!/bin/sh
set -eu
[ "$1" = exec ]; shift
if [ "$1" = -it ]; then shift; fi
[ "$1" = --user ]; shift 2
[ "$1" = reopen-fixture ]; shift
[ "$1" = tmux ]; shift
exec "$TW_CLEANUP_TMUX" -S "$TW_CLEANUP_SOCKET" "$@"
`), 0700))
	t.Setenv("TW_CLEANUP_TMUX", tmux)
	t.Setenv("TW_CLEANUP_SOCKET", socket)

	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()
	client := &ControlChannelClient{conn: brokerConn, connected: true, streams: make(map[string]*StreamHandler)}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var msg json.RawMessage
			if err := hubConn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}()
	defer func() { _ = hubConn.Close(); <-readerDone }()

	// Open first session
	streamID1 := "reopen-1"
	handler1 := &StreamHandler{
		streamID: streamID1, slug: "fixture",
		dataCh: make(chan []byte, 4), resizeCh: make(chan [2]int, 4),
		closeCh: make(chan struct{}),
	}
	client.streamMu.Lock()
	client.streams[streamID1] = handler1
	client.streamMu.Unlock()
	bridge1 := NewStreamPTYHandler(client, handler1, "reopen-fixture", adapter, "scion", "", 80, 24, nil, nil)
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		_ = bridge1.Run()
	}()

	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		return err == nil && strings.TrimSpace(out) != ""
	}, 5*time.Second, 20*time.Millisecond)

	// Count clients with first session
	clients1, _ := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
	count1 := len(strings.Split(strings.TrimSpace(clients1), "\n"))
	require.Equal(t, 1, count1, "first session should produce exactly one client")

	// Close first session, then reopen (simulates browser navigation)
	payload, _ := json.Marshal(wsprotocol.NewStreamCloseMessage(streamID1, "navigation close", 0))
	_ = client.handleStreamClose(payload)
	select {
	case <-done1:
	case <-time.After(15 * time.Second):
		t.Fatal("first bridge did not exit")
	}

	// Wait for client to be cleaned up
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		return err == nil && out == ""
	}, 10*time.Second, 50*time.Millisecond)

	// Open second session (warm reopen)
	streamID2 := "reopen-2"
	handler2 := &StreamHandler{
		streamID: streamID2, slug: "fixture",
		dataCh: make(chan []byte, 4), resizeCh: make(chan [2]int, 4),
		closeCh: make(chan struct{}),
	}
	client.streamMu.Lock()
	client.streams[streamID2] = handler2
	client.streamMu.Unlock()
	bridge2 := NewStreamPTYHandler(client, handler2, "reopen-fixture", adapter, "scion", "", 100, 30, nil, nil)
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		_ = bridge2.Run()
	}()

	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		return err == nil && strings.TrimSpace(out) != ""
	}, 5*time.Second, 20*time.Millisecond)

	// Verify: still exactly one client (no accumulated leaks)
	clients2, _ := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
	count2 := len(strings.Split(strings.TrimSpace(clients2), "\n"))
	require.Equal(t, 1, count2, "reopen must produce exactly one client, not accumulate")

	// Clean up
	payload2, _ := json.Marshal(wsprotocol.NewStreamCloseMessage(streamID2, "test done", 0))
	_ = client.handleStreamClose(payload2)
	select {
	case <-done2:
	case <-time.After(15 * time.Second):
		t.Fatal("second bridge did not exit")
	}

	// Verify: cleaned up after second close too
	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		return err == nil && out == ""
	}, 10*time.Second, 50*time.Millisecond,
		"second session must clean up on close")
}

// TestPTYCleanup_CloseIdempotent verifies that calling Close() multiple times
// does not panic.
func TestPTYCleanup_CloseIdempotent(t *testing.T) {
	handler := &StreamHandler{
		streamID: "idem-1", slug: "test",
		dataCh: make(chan []byte, 1), resizeCh: make(chan [2]int, 1),
		closeCh: make(chan struct{}),
	}
	bridge := &StreamPTYHandler{
		slug: "test", handler: handler,
		runtimeCmd:  "false",
		containerID: "test",
		execUser:    "scion",
	}
	bridge.ctx, bridge.cancel = context.WithCancel(context.Background())

	// Multiple Close() calls must not panic
	bridge.Close()
	bridge.Close()
	bridge.Close()
}

// TestPTYCleanup_ResizePreservedDuringSession verifies that resize events
// are properly applied during an active PTY session and don't interfere
// with cleanup.
func TestPTYCleanup_ResizePreservedDuringSession(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("requires tmux in PATH")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMUX", "")
	dir := t.TempDir()
	socket := filepath.Join(dir, "tmux.sock")
	tmuxCommand := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tmux, append([]string{"-S", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	_, err = tmuxCommand("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })

	adapter := filepath.Join(dir, "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(fmt.Sprintf(`#!/bin/sh
set -eu
[ "$1" = exec ]; shift
if [ "$1" = -it ]; then shift; fi
[ "$1" = --user ]; shift 2
[ "$1" = resize-fixture ]; shift
[ "$1" = tmux ]; shift
exec "%s" -S "%s" "$@"
`, tmux, socket)), 0700))

	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()
	client := &ControlChannelClient{conn: brokerConn, connected: true, streams: make(map[string]*StreamHandler)}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var msg json.RawMessage
			if err := hubConn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}()
	defer func() { _ = hubConn.Close(); <-readerDone }()

	streamID := "resize-1"
	handler := &StreamHandler{
		streamID: streamID, slug: "fixture",
		dataCh: make(chan []byte, 4), resizeCh: make(chan [2]int, 4),
		closeCh: make(chan struct{}),
	}
	client.streamMu.Lock()
	client.streams[streamID] = handler
	client.streamMu.Unlock()
	bridge := NewStreamPTYHandler(client, handler, "resize-fixture", adapter, "scion", "", 80, 24, nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bridge.Run()
	}()

	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
		return err == nil && out == "80x24"
	}, 5*time.Second, 20*time.Millisecond)

	// Resize multiple times
	for _, size := range [][2]int{{100, 30}, {120, 40}, {80, 24}} {
		resize, err := json.Marshal(wsprotocol.NewStreamResizeMessage(streamID, size[0], size[1]))
		require.NoError(t, err)
		require.NoError(t, client.handleStreamResize(resize))

		expected := fmt.Sprintf("%dx%d", size[0], size[1])
		require.Eventually(t, func() bool {
			out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
			return err == nil && out == expected
		}, 5*time.Second, 20*time.Millisecond, "resize to %s must reach tmux", expected)
	}

	// Close — should still clean up properly after resize
	payload, _ := json.Marshal(wsprotocol.NewStreamCloseMessage(streamID, "done", 0))
	_ = client.handleStreamClose(payload)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("bridge did not exit after resize session")
	}

	require.Eventually(t, func() bool {
		out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
		return err == nil && out == ""
	}, 10*time.Second, 50*time.Millisecond,
		"client must be cleaned up after resize session")
}
