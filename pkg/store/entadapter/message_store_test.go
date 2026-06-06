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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMessageStore(t *testing.T) *MessageStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewMessageStore(client)
}

func newTestMessage(projectID, recipientID string) *store.Message {
	return &store.Message{
		ID:          uuid.NewString(),
		ProjectID:   projectID,
		Sender:      "user:alice",
		SenderID:    "sender-1",
		Recipient:   "agent:coder",
		RecipientID: recipientID,
		Msg:         "Please fix the auth module.",
		Type:        "instruction",
		AgentID:     recipientID,
	}
}

func TestMessageCRUD(t *testing.T) {
	s := newTestMessageStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	msg := newTestMessage(projectID, "agent-1")
	require.NoError(t, s.CreateMessage(ctx, msg))
	assert.False(t, msg.CreatedAt.IsZero())

	got, err := s.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	assert.Equal(t, msg.ID, got.ID)
	assert.Equal(t, projectID, got.ProjectID)
	assert.Equal(t, "user:alice", got.Sender)
	assert.Equal(t, "Please fix the auth module.", got.Msg)
	assert.Equal(t, "instruction", got.Type)
	assert.False(t, got.Read)
}

func TestMessageGetNotFound(t *testing.T) {
	s := newTestMessageStore(t)
	_, err := s.GetMessage(context.Background(), uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestMessageInvalidInput(t *testing.T) {
	s := newTestMessageStore(t)
	err := s.CreateMessage(context.Background(), &store.Message{ID: uuid.NewString()})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestMarkMessageRead(t *testing.T) {
	s := newTestMessageStore(t)
	ctx := context.Background()
	msg := newTestMessage(uuid.NewString(), "agent-1")
	require.NoError(t, s.CreateMessage(ctx, msg))

	require.NoError(t, s.MarkMessageRead(ctx, msg.ID))
	got, err := s.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	assert.True(t, got.Read)

	assert.ErrorIs(t, s.MarkMessageRead(ctx, uuid.NewString()), store.ErrNotFound)
}

func TestMarkAllMessagesRead(t *testing.T) {
	s := newTestMessageStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	recipient := "agent-1"

	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateMessage(ctx, newTestMessage(projectID, recipient)))
	}
	// A message for a different recipient must stay unread.
	other := newTestMessage(projectID, "agent-2")
	require.NoError(t, s.CreateMessage(ctx, other))

	require.NoError(t, s.MarkAllMessagesRead(ctx, recipient))

	res, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: recipient, OnlyUnread: true}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, res.TotalCount)

	got, err := s.GetMessage(ctx, other.ID)
	require.NoError(t, err)
	assert.False(t, got.Read)
}

func TestListMessagesFilters(t *testing.T) {
	s := newTestMessageStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	m1 := newTestMessage(projectID, "agent-1")
	m1.SenderID = "user-x"
	require.NoError(t, s.CreateMessage(ctx, m1))

	m2 := newTestMessage(projectID, "agent-2")
	m2.SenderID = "user-x"
	require.NoError(t, s.CreateMessage(ctx, m2))

	// Filter by recipient.
	res, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: "agent-1"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount)

	// ParticipantID matches sender or recipient.
	res, err = s.ListMessages(ctx, store.MessageFilter{ParticipantID: "user-x"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, res.TotalCount)

	res, err = s.ListMessages(ctx, store.MessageFilter{ParticipantID: "agent-2"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount)
}

func TestPurgeOldMessages(t *testing.T) {
	s := newTestMessageStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	oldRead := newTestMessage(projectID, "agent-1")
	oldRead.Read = true
	oldRead.CreatedAt = time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, s.CreateMessage(ctx, oldRead))

	oldUnread := newTestMessage(projectID, "agent-1")
	oldUnread.CreatedAt = time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, s.CreateMessage(ctx, oldUnread))

	recent := newTestMessage(projectID, "agent-1")
	require.NoError(t, s.CreateMessage(ctx, recent))

	// readCutoff 24h ago purges oldRead; unreadCutoff 96h ago keeps oldUnread.
	readCutoff := time.Now().Add(-24 * time.Hour)
	unreadCutoff := time.Now().Add(-96 * time.Hour)
	n, err := s.PurgeOldMessages(ctx, readCutoff, unreadCutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = s.GetMessage(ctx, oldRead.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetMessage(ctx, oldUnread.ID)
	require.NoError(t, err)
	_, err = s.GetMessage(ctx, recent.ID)
	require.NoError(t, err)
}

// fakePublisher records PublishUserMessage calls to verify the LISTEN/NOTIFY
// design-in hook fires on create.
type fakePublisher struct {
	published []*store.Message
}

func (f *fakePublisher) PublishUserMessage(_ context.Context, msg *store.Message) error {
	f.published = append(f.published, msg)
	return nil
}

func TestCreateMessagePublishesEvent(t *testing.T) {
	base := newTestMessageStore(t)
	pub := &fakePublisher{}
	s := base.WithPublisher(pub)
	ctx := context.Background()

	msg := newTestMessage(uuid.NewString(), "agent-1")
	require.NoError(t, s.CreateMessage(ctx, msg))

	require.Len(t, pub.published, 1)
	assert.Equal(t, msg.ID, pub.published[0].ID)
}
