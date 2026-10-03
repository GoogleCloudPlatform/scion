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

// Tests for ptone/scion#1838: failure finalization (MarkMessageFailed and the
// sender's DELIVERY_FAILED notice) must survive an expired or cancelled
// dispatch context.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ctxHonoringDispatcher behaves like a real transport: a dispatch on a done
// ctx fails with the ctx error and is not recorded. Dispatches to slugs in
// fail are handed to onFail (which decides the error, and may cancel the
// caller's ctx to simulate a client disconnect mid-dispatch); every other
// dispatch is recorded.
type ctxHonoringDispatcher struct {
	brokerMockDispatcher
	mu     sync.Mutex
	fail   map[string]bool
	onFail func(ctx context.Context) error
}

func (d *ctxHonoringDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	failing := d.fail[agent.Slug]
	d.mu.Unlock()
	if failing {
		// Record the attempt (including the message ID carried on ctx) so
		// tests can assert on it, then fail.
		_ = d.brokerMockDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
		return d.onFail(ctx)
	}
	return d.brokerMockDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}

// noticesTo returns DELIVERY_FAILED notices dispatched to slug.
func (d *ctxHonoringDispatcher) noticesTo(slug string) []brokerDispatchedMsg {
	var out []brokerDispatchedMsg
	for _, m := range d.getMessages() {
		if m.agentSlug == slug && m.structured != nil && m.structured.Status == "DELIVERY_FAILED" {
			out = append(out, m)
		}
	}
	return out
}

func requireSingleRowState(t *testing.T, s store.Store, agentID, wantState string) store.Message {
	t.Helper()
	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: agentID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.Equal(t, wantState, rows.Items[0].DispatchState)
	return rows.Items[0]
}

func TestDeliverToAgent_TimedOutDispatchStillFinalizesFailure(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "sender-agent", "running")
	target := setupBrokerTestAgent(t, s, projectID, "target-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	// The broker keeps answering "deferred", so dispatchWithBrokerRetry
	// returns ErrBrokerTimeout exactly when ctx expires.
	dispatcher := &ctxHonoringDispatcher{
		fail:   map[string]bool{target.Slug: true},
		onFail: func(context.Context) error { return ErrMessageDeferred },
	}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	msg := messages.NewInstruction("agent:sender-agent", "agent:target-agent", "hello")
	msg.SenderID = sender.ID
	msg.RecipientID = target.ID

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	proxy.deliverToAgent(ctx, projectID, target.Slug, msg)
	require.Error(t, ctx.Err(), "precondition: the dispatch ctx must have expired")

	row := requireSingleRowState(t, s, target.ID, store.MessageDispatchFailed)
	require.NotNil(t, row.DispatchFailureReason)
	assert.Contains(t, *row.DispatchFailureReason, ErrBrokerTimeout.Error())

	notices := dispatcher.noticesTo(sender.Slug)
	require.Len(t, notices, 1, "agent sender must receive a DELIVERY_FAILED notice despite the expired ctx")
	assert.Contains(t, notices[0].msg, "target-agent")
}

func TestDeliverToAgent_CancelledDispatchStillFinalizesFailure(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "sender-agent", "running")
	target := setupBrokerTestAgent(t, s, projectID, "target-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher := &ctxHonoringDispatcher{
		fail: map[string]bool{target.Slug: true},
		onFail: func(context.Context) error {
			cancel()
			return errors.New("runtime broker returned error 500: boom")
		},
	}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	msg := messages.NewInstruction("agent:sender-agent", "agent:target-agent", "hello")
	msg.SenderID = sender.ID
	msg.RecipientID = target.ID
	proxy.deliverToAgent(ctx, projectID, target.Slug, msg)

	requireSingleRowState(t, s, target.ID, store.MessageDispatchFailed)
	require.Len(t, dispatcher.noticesTo(sender.Slug), 1,
		"agent sender must receive a DELIVERY_FAILED notice despite the cancelled ctx")
}

// TestHandleAgentMessage_ClientDisconnectStillMarksFailed covers the adjacent
// synchronous site: the human broker path marked the row on the request ctx,
// so a client disconnect during dispatch left it "dispatched".
func TestHandleAgentMessage_ClientDisconnectStillMarksFailed(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseRunning))

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.SetDispatcher(&ctxHonoringDispatcher{
		fail: map[string]bool{"msg-agent": true},
		onFail: func(context.Context) error {
			cancel() // client goes away mid-dispatch
			return errors.New("connection reset")
		},
	})

	body, err := json.Marshal(map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), bytes.NewReader(body)).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	requireSingleRowState(t, s, agentID, store.MessageDispatchFailed)
}

// deadlineRecordingDispatcher records the remaining budget on each dispatch.
type deadlineRecordingDispatcher struct {
	ctxHonoringDispatcher
	budgets []time.Duration
}

func (d *deadlineRecordingDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	if dl, ok := ctx.Deadline(); ok {
		d.mu.Lock()
		d.budgets = append(d.budgets, time.Until(dl))
		d.mu.Unlock()
	}
	return d.ctxHonoringDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}

// publishBroadcastDeliveryFailed is called from the broadcast fan-out with
// the dispatch ctx, which may already be done. The sender must still get
// exactly one notice, dispatched on the notice budget rather than the 5s
// row-CAS budget.
func TestPublishBroadcastDeliveryFailed_CancelledCtxStillNotifiesSender(t *testing.T) {
	srv, s := testServer(t)
	_, agents := setupGroupTest(t, s, "1838-bcast", map[string]string{
		"bc-sender": string(state.PhaseRunning),
		"bc-target": string(state.PhaseRunning),
	})
	dispatcher := &deadlineRecordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	msg := messages.NewInstruction("agent:bc-sender", "agent:bc-target", "hello")
	msg.SenderID = agents["bc-sender"].ID
	srv.publishBroadcastDeliveryFailed(ctx, agents["bc-target"], msg, errors.New("runtime broker returned error 500: boom"))

	notices := dispatcher.noticesTo("bc-sender")
	require.Len(t, notices, 1, "agent sender must receive one DELIVERY_FAILED notice despite the cancelled ctx")
	assert.Contains(t, notices[0].msg, "bc-target")
	require.Len(t, dispatcher.budgets, 1)
	assert.Greater(t, dispatcher.budgets[0], finalizationTimeout,
		"the notice must use deliveryNoticeTimeout, not the row-CAS budget")
	assert.LessOrEqual(t, dispatcher.budgets[0], deliveryNoticeTimeout)
}
