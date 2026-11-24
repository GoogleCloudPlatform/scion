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
	"errors"
	"fmt"
	"io"
	"log/slog"
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
		{
			// A well-behaved AgentLookup never returns (nil, nil), but a
			// test double or a future implementation might. This must not
			// dereference the nil result (a panic), and must not guess the
			// agent is gone (4404) — treat it as a retriable failure, same
			// as list-unavailable.
			name:       "nil result with nil error gives 4503, never dereferenced or treated as not-found",
			lookup:     &fixedAgentLookup{result: nil, err: nil},
			wantCode:   wsprotocol.ClosePTYUpstreamUnavailable,
			wantReason: wsprotocol.CloseReasonRuntimeUnavailable,
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

// TestHandlePTYStream_OpenTimeStoppedContainer covers ptone/scion#2088's
// early-detection follow-up on the control-channel attach path: an open-time
// lookup that already reports the container as definitively stopped ends the
// stream immediately with the terminal 4410 agent_stopped, instead of
// dispatching to handlePTYStreamWithAgent and waiting out the full
// waitForTmuxSession timeout before classifyAttachEnd's post-hoc probe
// reaches the same conclusion. Like TestHandlePTYStream_OpenTimeLookupFailure,
// this returns from handlePTYStream synchronously, before any PTY/tmux exec
// starts.
func TestHandlePTYStream_OpenTimeStoppedContainer(t *testing.T) {
	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()
	client := &ControlChannelClient{
		conn:      brokerConn,
		connected: true,
		streams:   make(map[string]*StreamHandler),
		agentLookup: &fixedAgentLookup{result: &AgentLookupResult{
			ContainerID: "stopped-container",
			RuntimeName: "docker",
			Phase:       "stopped",
		}},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ctx: context.Background(),
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

	handler := &StreamHandler{streamID: "open-time-stopped-test", slug: "stopped-agent", dataCh: make(chan []byte, 1), resizeCh: make(chan [2]int, 1), closeCh: make(chan struct{})}
	client.streamMu.Lock()
	client.streams[handler.streamID] = handler
	client.streamMu.Unlock()

	start := time.Now()
	client.handlePTYStream(handler, 80, 24)
	elapsed := time.Since(start)

	select {
	case msg := <-closedCh:
		require.Equal(t, wsprotocol.ClosePTYSessionGone, msg.Code)
		require.Equal(t, wsprotocol.CloseReasonAgentStopped, msg.Reason)
	case <-time.After(2 * time.Second):
		t.Fatal("no stream_close received")
	}
	require.Less(t, elapsed, tmuxSessionWaitTimeout,
		"a definitively stopped container must not wait out the full tmux readiness timeout")
}
