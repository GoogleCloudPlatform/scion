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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// newPersistIdentityProxy returns a message broker proxy over an in-process
// bus for the def138 fixture.
func newPersistIdentityProxy(t *testing.T, s store.Store) *MessageBrokerProxy {
	t.Helper()
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	inner := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inner.Close() })
	return NewMessageBrokerProxy(inner, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
}

// agentToUserDM is an agent-to-user message for the def138 fixture.
func agentToUserDM(agent *store.Agent, user *store.User, text string) *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender: "agent:" + agent.Slug, SenderID: agent.ID,
		Recipient: "user:" + user.Email, RecipientID: user.ID,
		Msg: text, Type: messages.TypeInstruction,
	}
}

// The persisting subscriber stores the row under the id and creation time
// the sending handler chose (and already reported), not ones of its own.
// The creation time is fixed in the past, so the test cannot pass by a
// clock coincidence.
func TestDeliverToUser_UsesHandlerRowIdentity(t *testing.T) {
	_, s, project, agent, user := def138Setup(t)
	proxy := newPersistIdentityProxy(t, s)
	ctx := context.Background()

	fixed := time.Date(2001, 2, 3, 4, 5, 6, 500_000_000, time.UTC)
	msg := agentToUserDM(agent, user, "pinned identity")
	msg.PersistMessageID = tid("dm-persist-row")
	msg.PersistCreatedAt = fixed

	proxy.deliverToUser(ctx, project.ID, eventbus.TopicUserMessages(project.ID, user.ID), msg)

	row, err := s.GetMessage(ctx, msg.PersistMessageID)
	require.NoError(t, err, "the row is stored under the handler's id")
	assert.Equal(t, "pinned identity", row.Msg)
	assert.WithinDuration(t, fixed, row.CreatedAt, time.Millisecond, "the row carries the handler's creation time")
}

// A message decoded from JSON (how plugin and external traffic reaches the
// hub) cannot carry the hub-internal row identity, so it never pins the row.
func TestDeliverToUser_DecodedMessageDoesNotPinRow(t *testing.T) {
	_, s, project, agent, user := def138Setup(t)
	proxy := newPersistIdentityProxy(t, s)
	ctx := context.Background()

	raw, err := json.Marshal(agentToUserDM(agent, user, "decoded message"))
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	pinnedID := tid("dm-spoofed-row")
	for _, k := range []string{"PersistMessageID", "persistMessageId", "persist_message_id"} {
		fields[k] = pinnedID
	}
	for _, k := range []string{"PersistCreatedAt", "persistCreatedAt", "persist_created_at"} {
		fields[k] = "2001-02-03T04:05:06Z"
	}
	raw, err = json.Marshal(fields)
	require.NoError(t, err)
	var msg messages.StructuredMessage
	require.NoError(t, json.Unmarshal(raw, &msg))
	require.Empty(t, msg.PersistMessageID)
	require.True(t, msg.PersistCreatedAt.IsZero())

	before := time.Now().Add(-time.Minute)
	proxy.deliverToUser(ctx, project.ID, eventbus.TopicUserMessages(project.ID, user.ID), &msg)

	_, err = s.GetMessage(ctx, pinnedID)
	assert.Error(t, err, "no row under the spoofed id")
	res, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: user.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.NotEqual(t, pinnedID, res.Items[0].ID)
	assert.True(t, res.Items[0].CreatedAt.After(before), "the row takes the subscriber's own time")
}
