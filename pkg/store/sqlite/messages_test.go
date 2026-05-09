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

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMessage(projectID, agentID string) *store.Message {
	return &store.Message{
		ID:          api.NewUUID(),
		ProjectID:   projectID,
		Sender:      "user:alice",
		SenderID:    "user-uuid-alice",
		Recipient:   "agent:coder",
		RecipientID: agentID,
		Msg:         "Please fix the auth module.",
		Type:        "instruction",
		AgentID:     agentID,
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
	}
}

func TestMessageCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	projectID, agentID := createTestProjectAndAgent(t, s)
	msg := newTestMessage(projectID, agentID)

	// Create
	require.NoError(t, s.CreateMessage(ctx, msg))

	// Get
	got, err := s.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	assert.Equal(t, msg.ID, got.ID)
	assert.Equal(t, msg.ProjectID, got.ProjectID)
	assert.Equal(t, msg.Sender, got.Sender)
	assert.Equal(t, msg.Recipient, got.Recipient)
	assert.Equal(t, msg.Msg, got.Msg)
	assert.Equal(t, msg.Type, got.Type)
	assert.Equal(t, msg.AgentID, got.AgentID)
	assert.False(t, got.Read)

	// Duplicate create returns ErrAlreadyExists
	err = s.CreateMessage(ctx, msg)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)

	// Not found
	_, err = s.GetMessage(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestMessageMarkRead(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	projectID, agentID := createTestProjectAndAgent(t, s)
	msg := newTestMessage(projectID, agentID)
	require.NoError(t, s.CreateMessage(ctx, msg))

	// Mark single message as read
	require.NoError(t, s.MarkMessageRead(ctx, msg.ID))
	got, err := s.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	assert.True(t, got.Read)

	// Mark not-found returns ErrNotFound
	assert.ErrorIs(t, s.MarkMessageRead(ctx, "nonexistent"), store.ErrNotFound)
}

func TestMessageMarkAllRead(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	projectID, agentID := createTestProjectAndAgent(t, s)

	// Create two messages for the same recipient
	recipientID := agentID
	msg1 := newTestMessage(projectID, agentID)
	msg1.RecipientID = recipientID
	msg2 := newTestMessage(projectID, agentID)
	msg2.ID = api.NewUUID()
	msg2.RecipientID = recipientID
	require.NoError(t, s.CreateMessage(ctx, msg1))
	require.NoError(t, s.CreateMessage(ctx, msg2))

	require.NoError(t, s.MarkAllMessagesRead(ctx, recipientID))

	got1, err := s.GetMessage(ctx, msg1.ID)
	require.NoError(t, err)
	assert.True(t, got1.Read)

	got2, err := s.GetMessage(ctx, msg2.ID)
	require.NoError(t, err)
	assert.True(t, got2.Read)
}

func TestListMessages(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	projectID, agentID := createTestProjectAndAgent(t, s)

	// Create unread message
	unread := newTestMessage(projectID, agentID)
	require.NoError(t, s.CreateMessage(ctx, unread))

	// Create read message
	read := newTestMessage(projectID, agentID)
	read.ID = api.NewUUID()
	require.NoError(t, s.CreateMessage(ctx, read))
	require.NoError(t, s.MarkMessageRead(ctx, read.ID))

	// List all
	result, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)
	assert.Len(t, result.Items, 2)

	// List unread only
	result, err = s.ListMessages(ctx, store.MessageFilter{ProjectID: projectID, OnlyUnread: true}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Len(t, result.Items, 1)
	assert.Equal(t, unread.ID, result.Items[0].ID)

	// Filter by agent
	result, err = s.ListMessages(ctx, store.MessageFilter{AgentID: agentID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)

	// Filter by type
	result, err = s.ListMessages(ctx, store.MessageFilter{Type: "instruction"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)

	result, err = s.ListMessages(ctx, store.MessageFilter{Type: "input-needed"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.TotalCount)
}

func TestListMessages_ParticipantID(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	projectID, agentID := createTestProjectAndAgent(t, s)
	userID := "user-uuid-alice"

	// Inbound: user → agent. Sender=user, recipient=agent.
	inbound := newTestMessage(projectID, agentID)
	inbound.SenderID = userID
	inbound.RecipientID = agentID
	require.NoError(t, s.CreateMessage(ctx, inbound))

	// Outbound: agent → user. Sender=agent, recipient=user.
	outbound := &store.Message{
		ID:          api.NewUUID(),
		ProjectID:   projectID,
		Sender:      "agent:coder",
		SenderID:    agentID,
		Recipient:   "user:alice",
		RecipientID: userID,
		Msg:         "Done — here's the patch.",
		Type:        "assistant-reply",
		AgentID:     agentID,
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, s.CreateMessage(ctx, outbound))

	// Unrelated message in the same project/agent with a different user.
	other := &store.Message{
		ID:          api.NewUUID(),
		ProjectID:   projectID,
		Sender:      "user:bob",
		SenderID:    "user-uuid-bob",
		Recipient:   "agent:coder",
		RecipientID: agentID,
		Msg:         "Bob's message",
		Type:        "instruction",
		AgentID:     agentID,
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, s.CreateMessage(ctx, other))

	// ParticipantID + AgentID returns both sides of the alice↔agent chat
	// but not bob's message.
	result, err := s.ListMessages(ctx, store.MessageFilter{
		AgentID:       agentID,
		ParticipantID: userID,
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)
	gotIDs := map[string]bool{}
	for _, m := range result.Items {
		gotIDs[m.ID] = true
	}
	assert.True(t, gotIDs[inbound.ID], "inbound (user→agent) should match")
	assert.True(t, gotIDs[outbound.ID], "outbound (agent→user) should match")
	assert.False(t, gotIDs[other.ID], "bob's message should not match alice's participant filter")
}

func TestPurgeOldMessages(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	projectID, agentID := createTestProjectAndAgent(t, s)

	old := newTestMessage(projectID, agentID)
	old.CreatedAt = time.Now().Add(-40 * 24 * time.Hour)
	require.NoError(t, s.CreateMessage(ctx, old))
	require.NoError(t, s.MarkMessageRead(ctx, old.ID))

	recent := newTestMessage(projectID, agentID)
	recent.ID = api.NewUUID()
	require.NoError(t, s.CreateMessage(ctx, recent))

	readCutoff := time.Now().Add(-30 * 24 * time.Hour)
	unreadCutoff := time.Now().Add(-90 * 24 * time.Hour)
	n, err := s.PurgeOldMessages(ctx, readCutoff, unreadCutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = s.GetMessage(ctx, old.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.GetMessage(ctx, recent.ID)
	assert.NoError(t, err)
}
