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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/require"
)

// fixedAgentLookup is a minimal AgentLookup that resolves to a fixed result
// or a fixed error, for driving handlePTYStream in tests without a real
// agent manager. A non-nil err takes precedence over result.
type fixedAgentLookup struct {
	result *AgentLookupResult
	err    error
}

func (f *fixedAgentLookup) LookupContainerID(ctx context.Context, slug, projectID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.result.ContainerID, nil
}

func (f *fixedAgentLookup) LookupAgent(ctx context.Context, slug, projectID string) (*AgentLookupResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fixedAgentLookup) RuntimeCommand() string {
	if f.result != nil {
		return f.result.RuntimeName
	}
	return "docker"
}

// ptyClassifierCloseMsg mirrors the fields of wsprotocol.StreamCloseMessage
// that the tests need to assert on.
type ptyClassifierCloseMsg struct {
	Type     string `json:"type"`
	StreamID string `json:"streamId"`
	Code     int    `json:"code"`
	Reason   string `json:"reason"`
}

// TestPTYClassifier_RealTmuxAttachEnd exercises classifyAttachEnd through the
// full production path — handlePTYStream -> handlePTYStreamWithAgent -> Run
// -> classifyAttachEnd -> CloseStream — against a real tmux session and
// kernel PTY. Only the runtime exec is adapted to a private local tmux
// socket, as in TestPTYLifecycle_PrivateTmuxSurvivesDetachAndStreamClose.
//
// Covers: detach-client -> 1000; kill-session -> 4410; a Hub-initiated stream
// close sends no probe and no CloseStream; and a killed exec with the
// session alive gives 4503.
func TestPTYClassifier_RealTmuxAttachEnd(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("real-tmux classifier test requires tmux in PATH")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMUX", "")

	// The adapter script itself is shared: it forwards to whatever socket
	// TW_CLASSIFIER_SOCKET names at exec time, so each subtest below can
	// point it at its own private tmux server (a fresh socket per subtest
	// avoids a create/kill-server race on a shared socket path between fast
	// consecutive subtests). It also appends a marker line to
	// TW_CLASSIFIER_PROBE_LOG every time it is invoked as the post-exit
	// has-session probe (classifyAttachEnd's tmuxHasSession), so tests can
	// assert precisely on the number of probe execs rather than inferring it
	// from the session's state.
	adapter := filepath.Join(t.TempDir(), "runtime-exec")
	require.NoError(t, os.WriteFile(adapter, []byte(`#!/bin/sh
set -eu
[ "$1" = exec ]; shift
if [ "$1" = -it ]; then shift; fi
[ "$1" = --user ]; shift 2
[ "$1" = classifier-fixture ]; shift
[ "$1" = tmux ]; shift
if [ "$1" = has-session ]; then
  echo probe >> "$TW_CLASSIFIER_PROBE_LOG"
fi
exec "$TW_CLASSIFIER_TMUX" -S "$TW_CLASSIFIER_SOCKET" "$@"
`), 0700))
	t.Setenv("TW_CLASSIFIER_TMUX", tmux)

	lookup := &fixedAgentLookup{result: &AgentLookupResult{
		ContainerID: "classifier-fixture",
		RuntimeName: adapter,
		ExecUser:    "scion",
	}}

	// attachHarness holds everything a duringAttach callback needs to poke
	// at a live attach: the handler (to inject data or a Hub-initiated
	// close) and the tmux command runner (to drive the session directly).
	type attachHarness struct {
		handler     *StreamHandler
		client      *ControlChannelClient
		tmuxCommand func(args ...string) (string, error)
	}

	// runAttach starts a fresh tmux session, drives one attach through
	// handlePTYStream, invokes duringAttach once the client has attached,
	// waits for handlePTYStream to return, and reports what (if anything)
	// the broker sent the Hub as the stream close. The returned probeCount
	// reads how many times the adapter's has-session probe actually ran.
	runAttach := func(t *testing.T, streamID string, duringAttach func(t *testing.T, h attachHarness)) (msg ptyClassifierCloseMsg, gotClose bool, tmuxCmd func(args ...string) (string, error), probeCount func() int) {
		t.Helper()

		socket := filepath.Join(t.TempDir(), "tmux.sock")
		tmuxCommand := func(args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, tmux, append([]string{"-S", socket}, args...)...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
		t.Setenv("TW_CLASSIFIER_SOCKET", socket)

		probeLog := filepath.Join(t.TempDir(), "probe.log")
		t.Setenv("TW_CLASSIFIER_PROBE_LOG", probeLog)
		probeCount = func() int {
			data, err := os.ReadFile(probeLog)
			if err != nil {
				return 0 // never created means never invoked
			}
			trimmed := strings.TrimSpace(string(data))
			if trimmed == "" {
				return 0
			}
			return len(strings.Split(trimmed, "\n"))
		}

		out, err := tmuxCommand("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "exec cat")
		require.NoError(t, err, "new-session output: %s", out)
		t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })

		brokerConn, hubConn, cleanup := newWSPair(t)
		defer cleanup()
		client := &ControlChannelClient{
			conn:        brokerConn,
			connected:   true,
			streams:     make(map[string]*StreamHandler),
			agentLookup: lookup,
			log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			// Set explicitly (rather than left nil) so this test exercises
			// the production probeCtx := c.ctx branch in
			// handlePTYStreamWithAgent, not just its nil-ctx test fallback.
			ctx: context.Background(),
		}

		closedCh := make(chan ptyClassifierCloseMsg, 4)
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			for {
				var m ptyClassifierCloseMsg
				if err := hubConn.ReadJSON(&m); err != nil {
					return
				}
				if m.Type == wsprotocol.TypeStreamClose {
					closedCh <- m
				}
			}
		}()
		defer func() { _ = hubConn.Close(); <-readerDone }()

		handler := &StreamHandler{streamID: streamID, slug: "classifier-fixture", dataCh: make(chan []byte, 4), resizeCh: make(chan [2]int, 4), closeCh: make(chan struct{})}
		client.streamMu.Lock()
		client.streams[streamID] = handler
		client.streamMu.Unlock()

		done := make(chan struct{})
		go func() {
			defer close(done)
			client.handlePTYStream(handler, 80, 24)
		}()
		t.Cleanup(func() {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("handlePTYStream did not return during cleanup")
			}
		})

		require.Eventually(t, func() bool {
			out, err := tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
			return err == nil && out == "80x24"
		}, 5*time.Second, 20*time.Millisecond, "one real tmux client should attach")

		// waitForTmuxSession's readiness poll also runs has-session checks
		// before the attach succeeds, and those land in the same probe log.
		// Reset it here so probeCount() reflects only probes that happen
		// after this point — i.e. classifyAttachEnd's post-exit probe, which
		// is what the tests below actually care about.
		require.NoError(t, os.RemoveAll(probeLog))

		duringAttach(t, attachHarness{handler: handler, client: client, tmuxCommand: tmuxCommand})

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("handlePTYStream did not return")
		}

		select {
		case msg := <-closedCh:
			return msg, true, tmuxCommand, probeCount
		case <-time.After(500 * time.Millisecond):
			return ptyClassifierCloseMsg{}, false, tmuxCommand, probeCount
		}
	}

	t.Run("detach-client gives 1000", func(t *testing.T) {
		msg, ok, _, _ := runAttach(t, "classifier-detach", func(t *testing.T, h attachHarness) {
			// C-b d: the tmux detach keystroke, delivered exactly as a real
			// client keypress would arrive over the stream.
			h.handler.dataCh <- []byte{2, 'd'}
		})
		require.True(t, ok, "expected a stream_close for a client-initiated detach")
		require.Equal(t, wsprotocol.ClosePTYNormal, msg.Code)
	})

	t.Run("kill-session gives 4410", func(t *testing.T) {
		msg, ok, _, probeCount := runAttach(t, "classifier-kill-session", func(t *testing.T, h attachHarness) {
			_, err := h.tmuxCommand("kill-session", "-t", "scion")
			require.NoError(t, err)
		})
		require.True(t, ok, "expected a stream_close once the session is gone")
		require.Equal(t, wsprotocol.ClosePTYSessionGone, msg.Code)
		require.Equal(t, wsprotocol.CloseReasonSessionEnded, msg.Reason)

		// Sanity check on the probe-counting mechanism itself: a genuine
		// post-exit classification always runs exactly one has-session probe,
		// so the "no probe" claim in the Hub-close subtest below is backed by
		// a counter that does register real invocations, not one that always
		// reads zero.
		require.Equal(t, 1, probeCount(), "classifying a gone session should run exactly one has-session probe")
	})

	t.Run("killed exec with session alive gives 4503", func(t *testing.T) {
		msg, ok, tmuxCmd, _ := runAttach(t, "classifier-killed-exec", func(t *testing.T, h attachHarness) {
			pidStr, err := h.tmuxCommand("list-clients", "-t", "scion", "-F", "#{client_pid}")
			require.NoError(t, err)
			pid, err := strconv.Atoi(pidStr)
			require.NoError(t, err)
			// Kill the exec transport (the tmux attach client process)
			// directly, simulating a docker/podman exec transport drop or an
			// operator `kill -9` — without touching the tmux session itself.
			require.NoError(t, syscall.Kill(pid, syscall.SIGKILL))
		})
		require.True(t, ok, "expected a stream_close after the exec transport was killed")
		require.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, msg.Code)
		require.Equal(t, wsprotocol.CloseReasonRuntimeStreamDropped, msg.Reason)

		// The session itself must have survived — only the transport died.
		out, err := tmuxCmd("has-session", "-t", "scion")
		require.NoError(t, err, "tmux session must survive a killed exec transport: %s", out)
	})

	t.Run("Hub-initiated close sends no probe and no CloseStream", func(t *testing.T) {
		_, ok, tmuxCmd, probeCount := runAttach(t, "classifier-hub-close", func(t *testing.T, h attachHarness) {
			payload, err := json.Marshal(wsprotocol.NewStreamCloseMessage("classifier-hub-close", "hub closed", 0))
			require.NoError(t, err)
			require.NoError(t, h.client.handleStreamClose(payload))
		})
		require.False(t, ok, "a Hub-initiated close must not be followed by a broker CloseStream")

		// No probe exec ran at all — counted directly from the adapter's own
		// invocation log, not inferred from the session's state.
		require.Equal(t, 0, probeCount(), "a Hub-initiated close must skip the has-session probe entirely")

		// The session must also be untouched.
		out, err := tmuxCmd("has-session", "-t", "scion")
		require.NoError(t, err, "tmux session must still exist: %s", out)
	})
}

// TestHandlePTYStream_OpenTimeLookupFailure covers the other site that must
// distinguish a transient listing failure from a genuinely missing agent:
// the open-time agent lookup in handlePTYStream (controlchannel.go, before
// any PTY session starts), as distinct from the post-exit classifyAttachEnd
// path covered by TestClassifyAttachEnd and TestPTYClassifier_RealTmuxAttachEnd.
// A runtime-listing failure here must give a retriable 4503, never the
// terminal 4404 a plain "not found" would give — this is the only place that
// can otherwise turn a transient `docker ps` hiccup into "session ended" for
// the user. No tmux or PTY is involved: every row here returns from
// handlePTYStream before any exec starts.
func TestHandlePTYStream_OpenTimeLookupFailure(t *testing.T) {
	tests := []struct {
		name       string
		lookup     *fixedAgentLookup
		wantCode   int
		wantReason string
	}{
		{
			name:       "list unavailable gives 4503, not 4404",
			lookup:     &fixedAgentLookup{err: fmt.Errorf("%w: docker ps failed", ErrAgentListUnavailable)},
			wantCode:   wsprotocol.ClosePTYUpstreamUnavailable,
			wantReason: wsprotocol.CloseReasonRuntimeUnavailable,
		},
		{
			name:       "genuine not-found gives 4404",
			lookup:     &fixedAgentLookup{err: errors.New("agent 'ghost' not found")},
			wantCode:   wsprotocol.ClosePTYAgentNotFound,
			wantReason: wsprotocol.CloseReasonAgentNotFound,
		},
		{
			name:       "empty ContainerID gives 4404",
			lookup:     &fixedAgentLookup{result: &AgentLookupResult{ContainerID: "", RuntimeName: "docker"}},
			wantCode:   wsprotocol.ClosePTYAgentNotFound,
			wantReason: wsprotocol.CloseReasonAgentNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			brokerConn, hubConn, cleanup := newWSPair(t)
			defer cleanup()
			client := &ControlChannelClient{
				conn:        brokerConn,
				connected:   true,
				streams:     make(map[string]*StreamHandler),
				agentLookup: tc.lookup,
				log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				ctx:         context.Background(),
			}

			closedCh := make(chan ptyClassifierCloseMsg, 1)
			readerDone := make(chan struct{})
			go func() {
				defer close(readerDone)
				var m ptyClassifierCloseMsg
				if err := hubConn.ReadJSON(&m); err == nil {
					closedCh <- m
				}
			}()
			defer func() { _ = hubConn.Close(); <-readerDone }()

			handler := &StreamHandler{streamID: "open-time-lookup-test", slug: "ghost", dataCh: make(chan []byte, 1), resizeCh: make(chan [2]int, 1), closeCh: make(chan struct{})}
			client.streamMu.Lock()
			client.streams[handler.streamID] = handler
			client.streamMu.Unlock()

			// handlePTYStream returns synchronously in every row here: each
			// lookup fails (or resolves to no container) before any PTY
			// session would start.
			client.handlePTYStream(handler, 80, 24)

			select {
			case msg := <-closedCh:
				require.Equal(t, tc.wantCode, msg.Code)
				require.Equal(t, tc.wantReason, msg.Reason)
			case <-time.After(2 * time.Second):
				t.Fatal("no stream_close received")
			}
		})
	}
}
