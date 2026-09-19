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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/require"
)

// This exercises the production PTY bridge with a real tmux client and kernel
// PTY. Only runtime exec is adapted to a private local socket; this is NOT a
// Docker, Apple container, Kubernetes or sandbox integration test.
func TestPTYLifecycle_PrivateTmuxSurvivesDetachAndStreamClose(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("isolated tmux lifecycle requires tmux in PATH")
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
	// No default socket, config, real agent or container is used, even in cleanup.
	_, err = tmuxCommand("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })
	panePID, err := tmuxCommand("display-message", "-p", "-t", "scion", "#{pane_pid}")
	require.NoError(t, err)
	require.NotEmpty(t, panePID)

	// Match the runtime exec boundary used by startDockerExec, readiness and
	// active-window queries. Reject unexpected targets instead of running them.
	adapter := filepath.Join(dir, "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(`#!/bin/sh
set -eu
[ "$1" = exec ]; shift
if [ "$1" = -it ]; then shift; fi
[ "$1" = --user ]; shift 2
[ "$1" = lifecycle-fixture ]; shift
[ "$1" = tmux ]; shift
exec "$TW_LIFECYCLE_TMUX" -S "$TW_LIFECYCLE_SOCKET" "$@"
`), 0700))
	t.Setenv("TW_LIFECYCLE_TMUX", tmux)
	t.Setenv("TW_LIFECYCLE_SOCKET", socket)

	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()
	client := &ControlChannelClient{conn: brokerConn, connected: true, streams: make(map[string]*StreamHandler)}
	// Drain actual control-channel output so the PTY cannot block on transport.
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

	// Each round attaches to the SAME pane, proving new attachment remains
	// possible after graceful detach, browser-like immediate close, and EOF.
	for round, mode := range []string{"detach", "detach-and-close", "stream-close", "reattach"} {
		t.Run(mode, func(t *testing.T) {
			streamID := fmt.Sprintf("lifecycle-%d", round)
			handler := &StreamHandler{streamID: streamID, slug: "fixture", dataCh: make(chan []byte, 4), resizeCh: make(chan [2]int, 4), closeCh: make(chan struct{})}
			client.streamMu.Lock()
			client.streams[streamID] = handler
			client.streamMu.Unlock()
			bridge := NewStreamPTYHandler(client, handler, "lifecycle-fixture", adapter, "scion", "", 80, 24, nil, nil)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = bridge.Run() // EOF/EIO/cancellation are normal detach outcomes.
				_ = client.CloseStream(streamID, "session ended", 0)
			}()
			// Cancel through the stream to avoid racing initialization of the PTY.
			t.Cleanup(func() {
				payload, _ := json.Marshal(wsprotocol.NewStreamCloseMessage(streamID, "test cleanup", 0))
				_ = client.handleStreamClose(payload)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("PTY bridge did not exit")
				}
			})
			require.Eventually(t, func() bool {
				out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
				return err == nil && out == "80x24"
			}, 5*time.Second, 20*time.Millisecond, "one real tmux client should attach")

			resize, err := json.Marshal(wsprotocol.NewStreamResizeMessage(streamID, 100, 30))
			require.NoError(t, err)
			require.NoError(t, client.handleStreamResize(resize))
			require.Eventually(t, func() bool {
				out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
				return err == nil && out == "100x30"
			}, 5*time.Second, 20*time.Millisecond, "kernel PTY resize reaches tmux client")

			if mode != "stream-close" {
				data, err := json.Marshal(wsprotocol.NewStreamFrame(streamID, []byte{2, 'd'}))
				require.NoError(t, err)
				require.NoError(t, client.handleStreamData(data))
			}
			if mode == "stream-close" || mode == "detach-and-close" {
				payload, err := json.Marshal(wsprotocol.NewStreamCloseMessage(streamID, "browser closed", 0))
				require.NoError(t, err)
				require.NoError(t, client.handleStreamClose(payload))
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("PTY bridge did not release on close")
			}
			require.ErrorIs(t, bridge.ctx.Err(), context.Canceled)
			require.NotNil(t, bridge.cmd.ProcessState, "runtime exec process must be reaped")
			_, err = bridge.ptyMaster.Stat()
			require.Error(t, err, "PTY file descriptor must be closed")
			client.streamMu.RLock()
			remaining := len(client.streams)
			client.streamMu.RUnlock()
			require.Zero(t, remaining, "broker stream registry must release attachment")
			select {
			case id := <-closedStreams:
				require.Equal(t, streamID, id)
			case <-time.After(5 * time.Second):
				t.Fatal("Hub did not receive stream close")
			}
			require.Eventually(t, func() bool {
				out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
				return err == nil && out == ""
			}, 5*time.Second, 20*time.Millisecond)
			out, err := tmuxCommand("display-message", "-p", "-t", "scion", "#{pane_pid}:#{pane_dead}")
			require.NoError(t, err)
			require.Equal(t, panePID+":0", out, "original agent surrogate must remain alive")
		})
	}
}
