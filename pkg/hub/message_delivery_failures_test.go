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

// Tests for ptone/scion#1820: agent→agent messages to a non-running agent
// must not be silently dropped while marked "dispatched".

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Admission gate: deliverToAgent (pub/sub, conversation, group delivery)
// ---------------------------------------------------------------------------

func TestDeliverToAgent_RejectsNonRunningAgent(t *testing.T) {
	for _, phase := range []string{"stopped", "suspended", "error", "provisioning", "starting", "stopping"} {
		t.Run(phase, func(t *testing.T) {
			s := newBrokerTestStore(t)
			projectID := setupBrokerTestProject(t, s)
			sender := setupBrokerTestAgent(t, s, projectID, "sender-agent", "running")
			target := setupBrokerTestAgent(t, s, projectID, "target-agent", phase)

			events := NewChannelEventPublisher()
			defer events.Close()
			b := eventbus.NewInProcessEventBus(slog.Default())
			defer func() { _ = b.Close() }()
			dispatcher := &brokerMockDispatcher{}
			proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

			msg := messages.NewInstruction("agent:sender-agent", "agent:target-agent", "hello")
			msg.SenderID = sender.ID
			msg.RecipientID = target.ID
			proxy.deliverToAgent(context.Background(), projectID, target.Slug, msg)

			// Nothing may be dispatched to the target and nothing persisted
			// for it — no silent "dispatched" row.
			for _, d := range dispatcher.getMessages() {
				assert.NotEqual(t, target.Slug, d.agentSlug, "message must not be dispatched to a %s agent", phase)
			}
			rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: target.ID}, store.ListOptions{})
			require.NoError(t, err)
			assert.Empty(t, rows.Items, "no message row may be persisted for a %s agent", phase)

			// The agent sender is told, mirroring the 409 a human sender gets.
			var notice *brokerDispatchedMsg
			for _, d := range dispatcher.getMessages() {
				if d.agentSlug == sender.Slug {
					d := d
					notice = &d
				}
			}
			require.NotNil(t, notice, "agent sender must receive a DELIVERY_FAILED notice")
			assert.Equal(t, "DELIVERY_FAILED", notice.structured.Status)
			assert.Contains(t, notice.msg, "target-agent")
		})
	}
}

func TestDeliverToAgent_RunningAgentCarriesMessageID(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	target := setupBrokerTestAgent(t, s, projectID, "target-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	dispatcher := &brokerMockDispatcher{}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:target-agent", "hello")
	msg.SenderID = tid("user-alice")
	msg.RecipientID = target.ID
	proxy.deliverToAgent(context.Background(), projectID, target.Slug, msg)

	dispatched := dispatcher.getMessages()
	require.Len(t, dispatched, 1)
	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: target.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.Equal(t, rows.Items[0].ID, dispatched[0].messageID,
		"dispatch must carry the persisted message ID so broker failures can be reported")
}

// ---------------------------------------------------------------------------
// Honest dispatch state: broker-reported buffered delivery failures
// ---------------------------------------------------------------------------

func postMessageFailures(t *testing.T, srv *Server, brokerID string, identityBrokerID string, failures ...messageDeliveryFailure) (*httptest.ResponseRecorder, messageDeliveryFailuresResponse) {
	t.Helper()
	body, err := json.Marshal(messageDeliveryFailuresRequest{Failures: failures})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/message-failures", bytes.NewReader(body))
	if identityBrokerID != "" {
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(identityBrokerID)))
	}
	rr := httptest.NewRecorder()
	srv.handleRuntimeBrokerByIDInternal(rr, req, brokerID, "message-failures")
	var resp messageDeliveryFailuresResponse
	if rr.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	}
	return rr, resp
}

// TestBufferedFlushFailure_MarksAgentDMFailed covers the full hub-side path:
// an agent DM is accepted and marked dispatched (the broker answered 200
// after buffering), then the broker's buffer flush fails and it reports the
// message ID back. The row must end up "failed", and the sending agent is
// told.
func TestBufferedFlushFailure_MarksAgentDMFailed(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "will be lost in buffer"))
	require.Nil(t, dmErr)
	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, result.MessageID, calls[0].MessageID,
		"ExecuteAgentDM must carry the persisted message ID to the broker")

	row, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	require.Equal(t, store.MessageDispatchDispatched, row.DispatchState)

	// Wire a broker proxy so the sender notice can be observed.
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	events := NewChannelEventPublisher()
	defer events.Close()
	srv.SetMessageBrokerProxy(NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default()))

	rr, resp := postMessageFailures(t, srv, target.RuntimeBrokerID, target.RuntimeBrokerID, messageDeliveryFailure{
		MessageID: result.MessageID,
		AgentID:   target.Slug,
		ProjectID: target.ProjectID,
		Reason:    "broker delivery failed: agent 'delivery-target' not found or not running",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, 1, resp.Marked)

	row, err = s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, row.DispatchState,
		"a buffered flush failure must leave the row failed, not dispatched")
	require.NotNil(t, row.DispatchFailureReason)
	assert.Contains(t, *row.DispatchFailureReason, "not found or not running")

	var notice *dispatchCall
	for _, c := range dispatcher.getCalls() {
		if c.Agent.ID == sender.ID {
			c := c
			notice = &c
		}
	}
	require.NotNil(t, notice, "sending agent must be notified of the failure")
	assert.Equal(t, "DELIVERY_FAILED", notice.StructuredMessage.Status)
	assert.Empty(t, notice.MessageID, "the notice itself must not be reportable (no feedback loop)")
}

// A broker-supplied reason is untrusted: it is bounded in size and stripped
// of terminal control sequences before being stored or echoed into the
// sender's terminal (which may run on a different broker).
func TestBufferedFlushFailure_SanitizesHostileReason(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "hostile reason"))
	require.Nil(t, dmErr)

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	events := NewChannelEventPublisher()
	defer events.Close()
	srv.SetMessageBrokerProxy(NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default()))

	hostile := "boom\x1b]0;pwned\x07\x1b[2J\x00\r\nnext" + strings.Repeat("A", 10*1024)
	rr, resp := postMessageFailures(t, srv, target.RuntimeBrokerID, target.RuntimeBrokerID, messageDeliveryFailure{
		MessageID: result.MessageID,
		Reason:    hostile,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, 1, resp.Marked)

	row, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	require.NotNil(t, row.DispatchFailureReason)
	stored := *row.DispatchFailureReason
	assert.LessOrEqual(t, len(stored), maxFailureReasonBytes)
	assert.True(t, strings.HasPrefix(stored, "boom]0;pwned[2J  next"), "got %q", stored[:32])
	assertNoControlChars(t, stored)

	var notice *dispatchCall
	for _, c := range dispatcher.getCalls() {
		if c.Agent.ID == sender.ID {
			c := c
			notice = &c
		}
	}
	require.NotNil(t, notice)
	assert.Less(t, len(notice.StructuredMessage.Msg), 2*maxFailureReasonBytes)
	assertNoControlChars(t, notice.StructuredMessage.Msg)
}

func assertNoControlChars(t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		if r == '\n' {
			continue // notice formatting may add its own line breaks
		}
		require.False(t, unicode.IsControl(r), "unexpected control character %U in %q", r, s)
	}
}

func TestSanitizeFailureReason(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"plain reason":           "plain reason",
		"a\x1b[31mred\x1b[0m":    "a[31mred[0m",
		"bell\x07nul\x00del\x7f": "bellnuldel",
		"c1\u009bcsi":            "c1csi",
		"line1\r\nline2\ttab":    "line1  line2 tab",
		"  \n trimmed \r ":       "trimmed",
		"bad\xffutf8":            "badutf8",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitizeFailureReason(in), "input %q", in)
	}

	long := strings.Repeat("é", maxFailureReasonBytes) // 2 bytes per rune
	got := sanitizeFailureReason(long)
	assert.LessOrEqual(t, len(got), maxFailureReasonBytes)
	assert.True(t, utf8.ValidString(got), "truncation must respect rune boundaries")
}

func TestHandleBrokerMessageFailures_RejectsOtherIdentities(t *testing.T) {
	srv, _, _, sender, target, _, _ := deliverySetup(t)
	result, dmErr := srv.ExecuteAgentDM(context.Background(), deliveryDMInput(sender, target, "x"))
	require.Nil(t, dmErr)
	f := messageDeliveryFailure{MessageID: result.MessageID}

	// No identity.
	rr, _ := postMessageFailures(t, srv, target.RuntimeBrokerID, "", f)
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// A different broker's identity for this broker's endpoint.
	rr, _ = postMessageFailures(t, srv, target.RuntimeBrokerID, tid("some-other-broker"), f)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestHandleBrokerMessageFailures_IgnoresUnownedAndTerminalRows(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "owned-by-other-broker"))
	require.Nil(t, dmErr)

	// A second broker that does not host the recipient cannot fail its rows.
	otherBroker := tid("other-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: otherBroker, Name: "other-broker", Slug: "other-broker", Status: store.BrokerStatusOnline,
	}))
	rr, resp := postMessageFailures(t, srv, otherBroker, otherBroker, messageDeliveryFailure{MessageID: result.MessageID})
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 0, resp.Marked)
	assert.Equal(t, 1, resp.Ignored)
	row, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, row.DispatchState)

	// Unknown IDs are ignored.
	rr, resp = postMessageFailures(t, srv, target.RuntimeBrokerID, target.RuntimeBrokerID,
		messageDeliveryFailure{MessageID: api.NewUUID()}, messageDeliveryFailure{})
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 0, resp.Marked)
	assert.Equal(t, 2, resp.Ignored)

	// Already-failed rows keep their original reason.
	require.NoError(t, s.MarkMessageFailed(ctx, result.MessageID, "original reason"))
	rr, resp = postMessageFailures(t, srv, target.RuntimeBrokerID, target.RuntimeBrokerID,
		messageDeliveryFailure{MessageID: result.MessageID, Reason: "late report"})
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 0, resp.Marked)
	row, err = s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	require.NotNil(t, row.DispatchFailureReason)
	assert.Equal(t, "original reason", *row.DispatchFailureReason)
}

func TestHandleBrokerMessageFailures_RejectsOversizedReport(t *testing.T) {
	srv, _, _, _, target, _, _ := deliverySetup(t)
	failures := make([]messageDeliveryFailure, maxMessageFailuresPerReport+1)
	for i := range failures {
		failures[i] = messageDeliveryFailure{MessageID: api.NewUUID()}
	}
	rr, _ := postMessageFailures(t, srv, target.RuntimeBrokerID, target.RuntimeBrokerID, failures...)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestDispatchMessageIDContext(t *testing.T) {
	ctx := context.Background()
	assert.Empty(t, dispatchMessageIDFromContext(ctx))
	assert.Equal(t, ctx, withDispatchMessageID(ctx, ""), "empty ID must not wrap the context")
	assert.Equal(t, "m-1", dispatchMessageIDFromContext(withDispatchMessageID(ctx, "m-1")))
}

// Guard: the wire builders must include message_id only when the context
// carries one. Exercised through brokerHTTPTransport against a stub broker.
func TestBrokerHTTPTransport_MessageAgentCarriesMessageID(t *testing.T) {
	var bodies []map[string]any
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		bodies = append(bodies, m)
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	tr := newBrokerHTTPTransport(false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, tr.MessageAgent(ctx, "b1", stub.URL, "a1", "p1", "hi", false, nil))
	require.NoError(t, tr.MessageAgent(withDispatchMessageID(ctx, "msg-123"), "b1", stub.URL, "a1", "p1", "hi", false, nil))
	require.Len(t, bodies, 2)
	_, has := bodies[0]["message_id"]
	assert.False(t, has, "message_id must be omitted without a dispatch message ID")
	assert.Equal(t, "msg-123", bodies[1]["message_id"])
}
