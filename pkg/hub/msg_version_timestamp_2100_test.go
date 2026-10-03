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

// Tests for ptone/scion#2100: every published StructuredMessage built by the
// hub carries Version and an RFC3339 UTC Timestamp.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
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

func assertVersionAndTimestamp(t *testing.T, msg *messages.StructuredMessage) time.Time {
	t.Helper()
	require.NotNil(t, msg)
	assert.Equal(t, messages.Version, msg.Version, "Version must be stamped")
	require.NotEmpty(t, msg.Timestamp, "Timestamp must be stamped")
	ts, err := time.Parse(time.RFC3339, msg.Timestamp)
	require.NoError(t, err, "Timestamp must be RFC3339")
	assert.True(t, strings.HasSuffix(msg.Timestamp, "Z"), "Timestamp must be UTC, got %q", msg.Timestamp)
	assert.WithinDuration(t, time.Now(), ts, time.Minute)
	return ts
}

func newNoticeTestProxy(t *testing.T, s store.Store, dispatcher AgentDispatcher) *MessageBrokerProxy {
	t.Helper()
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	b := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = b.Close() })
	return NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
}

func TestPublishDeliveryFailed_StampsVersionAndTimestamp(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "vt-pdf-sender", "running")
	dispatcher := &brokerMockDispatcher{}
	proxy := newNoticeTestProxy(t, s, dispatcher)

	msg := messages.NewInstruction("agent:"+sender.Slug, "agent:target", "hello")
	msg.SenderID = sender.ID
	proxy.publishDeliveryFailed(context.Background(), projectID, "target-agent", msg, errors.New("boom"))

	got := mockDispatchesTo(dispatcher, sender.Slug)
	require.Len(t, got, 1)
	assert.Equal(t, "DELIVERY_FAILED", got[0].structured.Status)
	assert.Equal(t, sender.ID, got[0].structured.RecipientID)
	assert.Equal(t, messages.SystemCategoryDeliveryFailed, got[0].structured.Metadata["system_category"])
	assertVersionAndTimestamp(t, got[0].structured)
}

func TestPublishDeliveryDeferred_StampsVersionAndTimestamp(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "vt-pdd-sender", "running")
	dispatcher := &brokerMockDispatcher{}
	proxy := newNoticeTestProxy(t, s, dispatcher)

	msg := messages.NewInstruction("agent:"+sender.Slug, "agent:target", "hello")
	msg.SenderID = sender.ID
	proxy.publishDeliveryDeferred(context.Background(), "target-agent", msg)

	got := mockDispatchesTo(dispatcher, sender.Slug)
	require.Len(t, got, 1)
	assert.Equal(t, "DELIVERY_DEFERRED", got[0].structured.Status)
	assert.Equal(t, messages.SystemCategoryDeliveryDeferred, got[0].structured.Metadata["system_category"])
	assertVersionAndTimestamp(t, got[0].structured)
}

func TestPublishBroadcastDeliveryFailed_StampsVersionAndTimestamp(t *testing.T) {
	srv, s := testServer(t)
	_, agents := setupGroupTest(t, s, "2100-bcast", map[string]string{
		"vt-bcast-sender": string(state.PhaseRunning),
		"vt-bcast-target": string(state.PhaseRunning),
	})
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	msg := messages.NewInstruction("agent:vt-bcast-sender", "", "hello")
	msg.SenderID = agents["vt-bcast-sender"].ID
	srv.publishBroadcastDeliveryFailed(context.Background(), agents["vt-bcast-target"], msg, errors.New("boom"))

	got := mockDispatchesTo(dispatcher, "vt-bcast-sender")
	require.Len(t, got, 1)
	assert.Equal(t, "DELIVERY_FAILED", got[0].structured.Status)
	assertVersionAndTimestamp(t, got[0].structured)
}

// capturingBus records every message published through it before handing it
// to the wrapped bus.
type capturingBus struct {
	eventbus.EventBus
	mu   sync.Mutex
	msgs []*messages.StructuredMessage
}

func (b *capturingBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	b.msgs = append(b.msgs, msg)
	b.mu.Unlock()
	return b.EventBus.Publish(ctx, topic, msg)
}

func (b *capturingBus) published() []*messages.StructuredMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*messages.StructuredMessage(nil), b.msgs...)
}

func TestAgentOutboundUserDM_StampsVersionAndTimestamp(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)

	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	inner := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inner.Close() })
	bus := &capturingBus{EventBus: inner}
	proxy := NewMessageBrokerProxy(bus, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	srv.SetMessageBrokerProxy(proxy)

	rr := postOutboundNoConv(t, srv, project.ID, agent.ID, user.Email, "vt agent to user")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var dm *messages.StructuredMessage
	for _, m := range bus.published() {
		if m.Recipient == "user:"+user.Email || m.RecipientID == user.ID {
			dm = m
		}
	}
	require.NotNil(t, dm, "the agent→user DM must be published through the broker")
	assertVersionAndTimestamp(t, dm)
}
