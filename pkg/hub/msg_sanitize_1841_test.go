//go:build !no_sqlite

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

package hub

// Tests for ptone/scion#1841: broker-supplied error text is sanitized and
// bounded in every DELIVERY_FAILED notice and in the persisted
// dispatch_failure_reason, and the broker error body read is capped.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"unicode"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostileBrokerBody carries ESC/CSI, OSC, BEL and NUL sequences followed by an
// oversized tail.
var hostileBrokerBody = "boom\x1b[31mred\x1b[0m\x07bell\x1b]0;pwned\x07\x00nul\r\nnext" + strings.Repeat("A", 100_000)

func hostileBrokerError() error {
	return &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: hostileBrokerBody}
}

// assertSanitized checks that text carries no control characters other than
// plain spaces and is bounded.
func assertSanitized(t *testing.T, text string, maxLen int) {
	t.Helper()
	for _, r := range text {
		if unicode.IsControl(r) {
			t.Fatalf("control character %U survived sanitization in %q", r, text[:min(len(text), 120)])
		}
	}
	assert.NotContains(t, text, "\x1b")
	assert.LessOrEqual(t, len(text), maxLen, "text must be truncated")
	assert.Contains(t, text, "boom", "the readable part of the reason should survive")
}

func TestDeliverToAgent_HostileBrokerErrorSanitized(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "sender-agent", "running")
	target := setupBrokerTestAgent(t, s, projectID, "target-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	dispatcher := &ctxHonoringDispatcher{
		fail:   map[string]bool{target.Slug: true},
		onFail: func(context.Context) error { return hostileBrokerError() },
	}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	msg := messages.NewInstruction("agent:sender-agent", "agent:target-agent", "hello")
	msg.SenderID = sender.ID
	msg.RecipientID = target.ID
	proxy.deliverToAgent(context.Background(), projectID, target.Slug, msg)

	row := requireSingleRowState(t, s, target.ID, store.MessageDispatchFailed)
	require.NotNil(t, row.DispatchFailureReason)
	assertSanitized(t, *row.DispatchFailureReason, maxFailureReasonBytes)

	notices := dispatcher.noticesTo(sender.Slug)
	require.Len(t, notices, 1)
	// The notice is a fixed prefix plus the sanitized reason.
	assertSanitized(t, notices[0].msg, maxFailureReasonBytes+128)
	assert.Equal(t, notices[0].msg, notices[0].structured.Msg)
}

func TestPublishBroadcastDeliveryFailed_HostileBrokerErrorSanitized(t *testing.T) {
	srv, s := testServer(t)
	_, agents := setupGroupTest(t, s, "1841-bcast", map[string]string{
		"bcast-sender": string(state.PhaseRunning),
		"bcast-target": string(state.PhaseRunning),
	})
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	msg := &messages.StructuredMessage{
		Sender:   "agent:bcast-sender",
		SenderID: agents["bcast-sender"].ID,
		Msg:      "hello all",
		Type:     messages.TypeInstruction,
	}
	srv.publishBroadcastDeliveryFailed(context.Background(), agents["bcast-target"], msg, hostileBrokerError())

	got := mockDispatchesTo(dispatcher, "bcast-sender")
	require.Len(t, got, 1)
	assert.Equal(t, "DELIVERY_FAILED", got[0].structured.Status)
	assertSanitized(t, got[0].msg, maxFailureReasonBytes+128)
	assert.Equal(t, got[0].msg, got[0].structured.Msg)
}

func TestHandleAgentMessage_HostileBrokerErrorSanitizedInPersistedReason(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseRunning))
	srv.SetDispatcher(&ctxHonoringDispatcher{
		fail:   map[string]bool{"msg-agent": true},
		onFail: func(context.Context) error { return hostileBrokerError() },
	})

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	})
	require.GreaterOrEqual(t, rec.Code, 400)

	row := requireSingleRowState(t, s, agentID, store.MessageDispatchFailed)
	require.NotNil(t, row.DispatchFailureReason)
	assertSanitized(t, *row.DispatchFailureReason, maxFailureReasonBytes)
}

func TestBrokerHTTPError_BoundsBodyRead(t *testing.T) {
	body := strings.Repeat("x", 4*maxBrokerErrorBodyBytes)
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
	err := brokerHTTPError(resp)
	var se *brokerStatusError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, http.StatusBadGateway, se.StatusCode)
	assert.Len(t, se.Body, maxBrokerErrorBodyBytes, "broker error body read must be capped")
}
