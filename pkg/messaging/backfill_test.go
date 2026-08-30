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

package messaging

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// In-memory mock stores for backfill tests
// ---------------------------------------------------------------------------

// mockMessageStore is an in-memory MessageStore used by backfill tests.
type mockMessageStore struct {
	mu       sync.Mutex
	messages []store.Message
}

func (m *mockMessageStore) CreateMessage(_ context.Context, msg *store.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}
	m.messages = append(m.messages, *msg)
	return nil
}

func (m *mockMessageStore) GetMessage(_ context.Context, id string) (*store.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.messages {
		if m.messages[i].ID == id {
			cp := m.messages[i]
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockMessageStore) GetMessagesByIDs(_ context.Context, ids []string) (map[string]*store.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]*store.Message)
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	for i := range m.messages {
		if idSet[m.messages[i].ID] {
			cp := m.messages[i]
			result[cp.ID] = &cp
		}
	}
	return result, nil
}

func (m *mockMessageStore) ListMessages(_ context.Context, filter store.MessageFilter, opts store.ListOptions) (*store.ListResult[store.Message], error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Filter messages.
	var filtered []store.Message
	for _, msg := range m.messages {
		if filter.ProjectID != "" && msg.ProjectID != filter.ProjectID {
			continue
		}
		if !filter.After.IsZero() && !msg.CreatedAt.After(filter.After) {
			continue
		}
		filtered = append(filtered, msg)
	}

	// Sort by created DESC (matching real store behaviour).
	for i := 0; i < len(filtered); i++ {
		for j := i + 1; j < len(filtered); j++ {
			if filtered[j].CreatedAt.After(filtered[i].CreatedAt) ||
				(filtered[j].CreatedAt.Equal(filtered[i].CreatedAt) && filtered[j].ID > filtered[i].ID) {
				filtered[i], filtered[j] = filtered[j], filtered[i]
			}
		}
	}

	// Apply cursor — skip items at or before the cursor message (by created DESC).
	if opts.Cursor != "" {
		// Find the cursor message's position and skip everything before it
		// (inclusive), simulating keyset pagination in DESC order.
		var cursorIdx = -1
		for i, msg := range filtered {
			if msg.ID == opts.Cursor {
				cursorIdx = i
				break
			}
		}
		if cursorIdx >= 0 {
			// In DESC order, the cursor is the last item of the previous page.
			// Everything after it (higher index) is the next page.
			filtered = filtered[cursorIdx+1:]
		}
	}

	// Paginate.
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	result := &store.ListResult[store.Message]{
		TotalCount: len(filtered),
	}

	if len(filtered) > limit {
		result.Items = filtered[:limit]
		result.NextCursor = filtered[limit-1].ID
	} else {
		result.Items = filtered
	}

	return result, nil
}

func (m *mockMessageStore) MarkMessageRead(_ context.Context, _ string) error { return nil }

func (m *mockMessageStore) MarkAllMessagesRead(_ context.Context, _ string) error { return nil }

func (m *mockMessageStore) PurgeOldMessages(_ context.Context, _, _ time.Time) (int, error) {
	return 0, nil
}

func (m *mockMessageStore) SetMessageConversationID(_ context.Context, messageID, conversationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.messages {
		if m.messages[i].ID == messageID {
			m.messages[i].ConversationID = conversationID
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *mockMessageStore) CountUnbackfilledMessages(_ context.Context, projectID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, msg := range m.messages {
		if msg.ConversationID == "" {
			if projectID == "" || msg.ProjectID == projectID {
				count++
			}
		}
	}
	return count, nil
}

// mockConversationStore is an in-memory ConversationStore used by backfill tests.
type mockConversationStore struct {
	mu            sync.Mutex
	conversations []store.Conversation
	participants  []store.ConversationParticipant
	addressees    []store.MessageAddressee
}

func (m *mockConversationStore) CreateConversation(_ context.Context, conv *store.Conversation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conversations {
		if c.ID == conv.ID {
			return store.ErrAlreadyExists
		}
	}
	m.conversations = append(m.conversations, *conv)
	return nil
}

func (m *mockConversationStore) GetConversation(_ context.Context, id string) (*store.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.conversations {
		if m.conversations[i].ID == id && m.conversations[i].DeletedAt == nil {
			cp := m.conversations[i]
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockConversationStore) UpdateConversation(_ context.Context, conv *store.Conversation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.conversations {
		if m.conversations[i].ID == conv.ID {
			m.conversations[i] = *conv
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *mockConversationStore) DeleteConversation(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for i := range m.conversations {
		if m.conversations[i].ID == id {
			m.conversations[i].DeletedAt = &now
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *mockConversationStore) ListConversations(_ context.Context, filter store.ConversationFilter, _ store.ListOptions) (*store.ListResult[store.Conversation], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var items []store.Conversation
	for _, c := range m.conversations {
		if c.DeletedAt != nil {
			continue
		}
		if filter.ProjectID != "" && (c.ProjectID == nil || *c.ProjectID != filter.ProjectID) {
			continue
		}
		if filter.Kind != "" && c.Kind != filter.Kind {
			continue
		}
		items = append(items, c)
	}
	return &store.ListResult[store.Conversation]{Items: items, TotalCount: len(items)}, nil
}

func (m *mockConversationStore) UpsertConversationByExternalRef(_ context.Context, conv *store.Conversation) (*store.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if conv.ExternalRef == "" {
		return nil, store.ErrInvalidInput
	}
	// Find existing by external ref.
	for i := range m.conversations {
		c := &m.conversations[i]
		if c.ExternalRef == conv.ExternalRef && c.DeletedAt == nil {
			// Update mutable fields.
			c.DriftState = conv.DriftState
			c.DefaultAgentID = conv.DefaultAgentID
			cp := *c
			return &cp, nil
		}
	}
	// Create new.
	m.conversations = append(m.conversations, *conv)
	cp := *conv
	return &cp, nil
}

func (m *mockConversationStore) GetConversationByExternalRef(_ context.Context, surface, externalRef string) (*store.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.conversations {
		c := &m.conversations[i]
		if c.ExternalRef == externalRef && c.Surface == surface && c.DeletedAt == nil {
			cp := *c
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockConversationStore) AddParticipant(_ context.Context, p *store.ConversationParticipant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.participants {
		if existing.ConversationID == p.ConversationID &&
			existing.PrincipalKind == p.PrincipalKind &&
			existing.PrincipalID == p.PrincipalID &&
			existing.LeftAt == nil {
			return store.ErrAlreadyExists
		}
	}
	m.participants = append(m.participants, *p)
	return nil
}

func (m *mockConversationStore) EnsureParticipant(_ context.Context, p *store.ConversationParticipant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// If any row exists (active or soft-removed), leave it untouched.
	for _, existing := range m.participants {
		if existing.ConversationID == p.ConversationID &&
			existing.PrincipalKind == p.PrincipalKind &&
			existing.PrincipalID == p.PrincipalID {
			return nil
		}
	}
	m.participants = append(m.participants, *p)
	return nil
}

func (m *mockConversationStore) RemoveParticipant(_ context.Context, conversationID, principalKind, principalID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for i := range m.participants {
		p := &m.participants[i]
		if p.ConversationID == conversationID && p.PrincipalKind == principalKind && p.PrincipalID == principalID && p.LeftAt == nil {
			p.LeftAt = &now
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *mockConversationStore) ListParticipants(_ context.Context, conversationID string) ([]store.ConversationParticipant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []store.ConversationParticipant
	for _, p := range m.participants {
		if p.ConversationID == conversationID && p.LeftAt == nil {
			result = append(result, p)
		}
	}
	return result, nil
}

func (m *mockConversationStore) GetConversationsForPrincipal(_ context.Context, principalKind, principalID string) ([]store.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	convIDs := make(map[string]bool)
	for _, p := range m.participants {
		if p.PrincipalKind == principalKind && p.PrincipalID == principalID && p.LeftAt == nil {
			convIDs[p.ConversationID] = true
		}
	}

	var result []store.Conversation
	for _, c := range m.conversations {
		if convIDs[c.ID] && c.DeletedAt == nil {
			result = append(result, c)
		}
	}
	return result, nil
}

func (m *mockConversationStore) AddAddressee(_ context.Context, a *store.MessageAddressee) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addressees = append(m.addressees, *a)
	return nil
}

func (m *mockConversationStore) ListAddressees(_ context.Context, messageID string) ([]store.MessageAddressee, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []store.MessageAddressee
	for _, a := range m.addressees {
		if a.MessageID == messageID {
			result = append(result, a)
		}
	}
	return result, nil
}

func (m *mockConversationStore) UpdateDeliveryState(_ context.Context, _ string, _ string, _ *string) error {
	return nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newTestMessage(projectID, sender, senderID, recipient, recipientID string, createdAt time.Time) store.Message {
	return store.Message{
		ID:          uuid.NewString(),
		ProjectID:   projectID,
		Sender:      sender,
		SenderID:    senderID,
		Recipient:   recipient,
		RecipientID: recipientID,
		Msg:         "test message",
		Type:        "instruction",
		CreatedAt:   createdAt,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestBackfill_NormalDirectMessages(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	now := time.Now()

	// Two messages between the same user and agent — should form one conversation.
	msg1 := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, now.Add(-2*time.Minute))
	msg2 := newTestMessage(projectID, "agent:bot", agentID, "user:alice", userID, now.Add(-1*time.Minute))

	// A third message between different participants — separate conversation.
	otherUserID := uuid.NewString()
	msg3 := newTestMessage(projectID, "user:bob", otherUserID, "agent:bot", agentID, now)

	msgStore := &mockMessageStore{messages: []store.Message{msg1, msg2, msg3}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Equal(t, 3, result.TotalProcessed)
	assert.Equal(t, 3, result.Attributed)
	assert.Equal(t, 0, result.Skipped)
	assert.Equal(t, 0, result.Inferred)
	assert.Equal(t, 2, result.ConversationsCreated, "two distinct pairs = two conversations")

	// Verify messages were stamped.
	stamped1, _ := msgStore.GetMessage(ctx, msg1.ID)
	stamped2, _ := msgStore.GetMessage(ctx, msg2.ID)
	stamped3, _ := msgStore.GetMessage(ctx, msg3.ID)

	assert.NotEmpty(t, stamped1.ConversationID)
	assert.NotEmpty(t, stamped2.ConversationID)
	assert.NotEmpty(t, stamped3.ConversationID)

	// msg1 and msg2 should share a conversation.
	assert.Equal(t, stamped1.ConversationID, stamped2.ConversationID,
		"messages between same pair should share a conversation")

	// msg3 should be in a different conversation.
	assert.NotEqual(t, stamped1.ConversationID, stamped3.ConversationID,
		"messages between different pairs should be in different conversations")
}

func TestBackfill_ThreadBasedGrouping(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	user1 := uuid.NewString()
	user2 := uuid.NewString()
	agentID := uuid.NewString()

	now := time.Now()

	// Two messages with the same thread_id — should form one group conversation.
	msg1 := newTestMessage(projectID, "user:alice", user1, "agent:bot", agentID, now.Add(-2*time.Minute))
	msg1.ThreadID = "deploy-thread"

	msg2 := newTestMessage(projectID, "user:bob", user2, "agent:bot", agentID, now.Add(-1*time.Minute))
	msg2.ThreadID = "deploy-thread"

	msgStore := &mockMessageStore{messages: []store.Message{msg1, msg2}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Equal(t, 2, result.TotalProcessed)
	assert.Equal(t, 2, result.Attributed)
	assert.Equal(t, 1, result.ConversationsCreated)

	stamped1, _ := msgStore.GetMessage(ctx, msg1.ID)
	stamped2, _ := msgStore.GetMessage(ctx, msg2.ID)
	assert.Equal(t, stamped1.ConversationID, stamped2.ConversationID)

	// Verify the conversation is of kind "group".
	conv, err := convStore.GetConversation(ctx, stamped1.ConversationID)
	require.NoError(t, err)
	assert.Equal(t, "group", conv.Kind)
}

func TestBackfill_BroadcastedMessagesSkipped(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	msg := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now())
	msg.Broadcasted = true

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalProcessed)
	assert.Equal(t, 1, result.Skipped)
	assert.Equal(t, 0, result.Attributed)
	assert.Equal(t, 0, result.ConversationsCreated)

	// Message should NOT be stamped.
	original, _ := msgStore.GetMessage(ctx, msg.ID)
	assert.Empty(t, original.ConversationID)
}

func TestBackfill_HazardA_EmailBasedDMKeys(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	agentID := uuid.NewString()

	// Sender has an email address instead of a UUID.
	msg := newTestMessage(projectID, "user:alice@example.com", "alice@example.com", "agent:bot", agentID, time.Now())

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalProcessed)
	// After DeriveConversationKey repoint, email-based IDs fail key derivation
	// (DMConversationKey requires valid UUIDs). The message is skipped with an
	// error rather than being assigned to a hazard-A group.
	assert.Equal(t, 0, result.Attributed, "key derivation fails for email IDs")
	assert.Equal(t, 0, result.Inferred, "message is skipped, not inferred")
	assert.Equal(t, 0, result.ConversationsCreated, "no conversation created for failed derivation")
	assert.NotEmpty(t, result.Errors, "derivation failure should be recorded as an error")

	// Message should NOT be stamped — derivation failed.
	stamped, _ := msgStore.GetMessage(ctx, msg.ID)
	assert.Empty(t, stamped.ConversationID)
}

func TestBackfill_HazardB_SlugResolution(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	// Agent referenced by slug in AgentID field.
	msg := newTestMessage(projectID, "user:alice", userID, "agent:code-reviewer", agentID, time.Now())
	msg.AgentID = "code-reviewer" // slug, not UUID

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{
		agents: map[string]*store.Agent{
			projectID + "/code-reviewer": {ID: uuid.NewString(), Slug: "code-reviewer", ProjectID: projectID},
		},
	}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalProcessed)
	assert.Equal(t, 1, result.Attributed)
	assert.Equal(t, 1, result.HazardBSlugCount, "slug reference should be counted")
}

func TestBackfill_HazardB_DeletedAgent(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	msg := newTestMessage(projectID, "user:alice", userID, "agent:deleted-agent", agentID, time.Now())
	msg.AgentID = "deleted-agent" // slug that no longer exists

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{} // empty — slug lookup will return ErrNotFound

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalProcessed)
	assert.Equal(t, 1, result.Attributed)
	assert.Equal(t, 1, result.HazardBSlugCount)
	assert.Equal(t, 1, result.ConversationsCreated)

	// Verify the conversation has orphaned drift state.
	stamped, _ := msgStore.GetMessage(ctx, msg.ID)
	conv, err := convStore.GetConversation(ctx, stamped.ConversationID)
	require.NoError(t, err)
	assert.Equal(t, DriftStateOrphaned, conv.DriftState)
}

func TestBackfill_DryRun(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	msg1 := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now().Add(-time.Minute))
	msg2 := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now())

	msgStore := &mockMessageStore{messages: []store.Message{msg1, msg2}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{
		ProjectID: projectID,
		DryRun:    true,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, result.TotalProcessed)
	assert.Equal(t, 2, result.Attributed)
	assert.Equal(t, 1, result.ConversationsCreated)

	// Verify NO actual changes were made.
	stamped1, _ := msgStore.GetMessage(ctx, msg1.ID)
	stamped2, _ := msgStore.GetMessage(ctx, msg2.ID)
	assert.Empty(t, stamped1.ConversationID, "dry-run should not stamp messages")
	assert.Empty(t, stamped2.ConversationID, "dry-run should not stamp messages")

	convs, _ := convStore.ListConversations(ctx, store.ConversationFilter{}, store.ListOptions{})
	assert.Empty(t, convs.Items, "dry-run should not create conversations")
}

func TestBackfill_Idempotent(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	msg := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now())

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)

	// First run.
	result1, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)
	assert.Equal(t, 1, result1.Attributed)
	assert.Equal(t, 1, result1.ConversationsCreated)

	// Second run — message already has conversation_id, should be skipped.
	result2, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)
	assert.Equal(t, 1, result2.TotalProcessed)
	assert.Equal(t, 1, result2.Skipped)
	assert.Equal(t, 0, result2.Attributed)
	assert.Equal(t, 0, result2.ConversationsCreated, "no new conversations on re-run")
}

func TestBackfill_Resumable(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	now := time.Now()

	// Four messages at distinct times.
	msg1 := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, now.Add(-4*time.Minute))
	msg2 := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, now.Add(-3*time.Minute))
	msg3 := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, now.Add(-2*time.Minute))
	msg4 := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, now.Add(-1*time.Minute))

	msgStore := &mockMessageStore{messages: []store.Message{msg1, msg2, msg3, msg4}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)

	// Full run with batch size 2 → forces 2 pages in DESC order:
	//   Page 1: [msg4, msg3]  → cursor = msg3.ID, LastCheckpoint = cursor
	//   Page 2: [msg2, msg1]  → NextCursor="" → break
	result1, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID, BatchSize: 2})
	require.NoError(t, err)
	assert.Equal(t, 4, result1.TotalProcessed)
	assert.Equal(t, 4, result1.Attributed)
	assert.NotEmpty(t, result1.LastCheckpoint, "multi-page run should record a checkpoint")

	// Reset all messages to simulate an interrupted run that only completed page 1.
	for i := range msgStore.messages {
		msgStore.messages[i].ConversationID = ""
	}
	convStore.mu.Lock()
	convStore.conversations = nil
	convStore.participants = nil
	convStore.mu.Unlock()

	// Resume from checkpoint — should process only page 2 (the remaining older messages).
	result2, err := svc.Run(ctx, BackfillConfig{
		ProjectID:  projectID,
		Checkpoint: result1.LastCheckpoint,
		BatchSize:  10,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, result2.TotalProcessed,
		"resume should process only messages after the checkpoint cursor position")
	assert.Equal(t, 2, result2.Attributed)

	// Verify only the older messages (msg1, msg2) were stamped.
	stamped1, _ := msgStore.GetMessage(ctx, msg1.ID)
	stamped2, _ := msgStore.GetMessage(ctx, msg2.ID)
	stamped3, _ := msgStore.GetMessage(ctx, msg3.ID)
	stamped4, _ := msgStore.GetMessage(ctx, msg4.ID)
	assert.NotEmpty(t, stamped1.ConversationID, "msg1 after cursor — should be stamped")
	assert.NotEmpty(t, stamped2.ConversationID, "msg2 after cursor — should be stamped")
	assert.Empty(t, stamped3.ConversationID, "msg3 before cursor — should not be stamped")
	assert.Empty(t, stamped4.ConversationID, "msg4 before cursor — should not be stamped")
}

func TestBackfill_DefaultBatchSize(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()

	msgStore := &mockMessageStore{}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)
	assert.Equal(t, 0, result.TotalProcessed)
}

func TestBackfill_HazardA_BothSidesEmail(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()

	// Both sender and recipient have non-UUID identifiers.
	msg := newTestMessage(projectID, "user:alice@example.com", "alice@example.com",
		"user:bob@example.com", "bob@example.com", time.Now())

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	// After DeriveConversationKey repoint, email-based IDs fail key derivation.
	// The message is skipped with an error, not assigned to a hazard-A group.
	assert.Equal(t, 0, result.HazardAEmailCount, "key derivation fails before hazard detection")
	assert.Equal(t, 0, result.Inferred, "message is skipped, not inferred")
	assert.Equal(t, 0, result.Attributed)
	assert.NotEmpty(t, result.Errors, "derivation failure should be recorded as an error")
}

func TestBackfill_ConversationParticipants(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	msg := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now())

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	_, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	stamped, _ := msgStore.GetMessage(ctx, msg.ID)
	participants, err := convStore.ListParticipants(ctx, stamped.ConversationID)
	require.NoError(t, err)

	assert.Len(t, participants, 2, "should have both sender and recipient as participants")

	// Verify participant kinds.
	kinds := map[string]string{}
	for _, p := range participants {
		kinds[p.PrincipalID] = p.PrincipalKind
	}
	assert.Equal(t, "user", kinds[userID])
	assert.Equal(t, "agent", kinds[agentID])
}

func TestBackfill_ConversationExternalRef(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	msg := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now())

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	_, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	stamped, _ := msgStore.GetMessage(ctx, msg.ID)
	conv, err := convStore.GetConversation(ctx, stamped.ConversationID)
	require.NoError(t, err)

	assert.NotEmpty(t, conv.ExternalRef, "conversation should have an external ref for upsert idempotency")
	// DM external_ref must match DMConversationKey format (kind-prefixed, canonical).
	// The backfill now uses DeriveConversationKey which delegates to DMConversationKey,
	// producing dm:<kind>:<uuid>:<kind>:<uuid> — not the old DirectMessageExternalRef
	// format dm:<sorted-id>:<sorted-id>. The old format predates the kind-safe
	// convergence in DEF-8 and no longer matches what dual-write produces.
	wantRef, err := messages.DMConversationKey("user", userID, "agent", agentID)
	require.NoError(t, err)
	assert.Equal(t, wantRef, conv.ExternalRef,
		"DM external_ref must match DMConversationKey (dual-write format)")
}

func TestBackfill_MixedDirectAndThread(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	now := time.Now()

	// Direct message.
	dm := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, now.Add(-time.Minute))

	// Thread message with the same participants.
	threadMsg := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, now)
	threadMsg.ThreadID = "build-thread"

	msgStore := &mockMessageStore{messages: []store.Message{dm, threadMsg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Equal(t, 2, result.Attributed)
	assert.Equal(t, 2, result.ConversationsCreated, "direct and thread should be separate conversations")

	stampedDM, _ := msgStore.GetMessage(ctx, dm.ID)
	stampedThread, _ := msgStore.GetMessage(ctx, threadMsg.ID)
	assert.NotEqual(t, stampedDM.ConversationID, stampedThread.ConversationID)
}

func TestParsePrincipal(t *testing.T) {
	tests := []struct {
		label    string
		id       string
		wantKind string
		wantID   string
	}{
		{"user:alice", "abc-123", "user", "abc-123"},
		{"agent:bot", "def-456", "agent", "def-456"},
		{"user:alice@example.com", "", "user", "alice@example.com"},
		{"agent:code-reviewer", "", "agent", "code-reviewer"},
		{"nocolon", "", "user", "nocolon"}, // no colon defaults to user
		{"user:alice", "", "user", "alice"},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			kind, id := parsePrincipal(tt.label, tt.id)
			assert.Equal(t, tt.wantKind, kind)
			assert.Equal(t, tt.wantID, id)
		})
	}
}

func TestIsValidUUID(t *testing.T) {
	assert.True(t, isValidUUID(uuid.NewString()))
	assert.False(t, isValidUUID("not-a-uuid"))
	assert.False(t, isValidUUID("alice@example.com"))
	assert.False(t, isValidUUID(""))
}

func TestBackfill_HazardB_InvalidAgentRef(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	// Agent ref that is neither a UUID nor a valid slug (contains @).
	msg := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now())
	msg.AgentID = "invalid@ref" // neither UUID nor valid slug

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Equal(t, 1, result.HazardBSlugCount)
	assert.Equal(t, 1, result.Inferred, "invalid agent ref should mark messages as inferred")
}

// TestBackfill_UnrecognizedCheckpointServiceLayer verifies the backfill service
// passes an unrecognized cursor through to the store's ListMessages. The mock
// store does not validate cursors — an unknown cursor silently returns all
// messages (restart-from-zero). In production, the real store
// (entadapter/message_store.go) validates cursors via decodeCursor and returns
// "invalid cursor" on malformed input.
//
// COVERAGE GAP: the "bad checkpoint → loud error" property is NOT tested here
// because the mock cannot produce cursor-validation errors. That property is
// tested in cmd/server_backfill_test.go (TestBackfillPreUpgradeCheckpointRejected)
// against the real SQLite store.
func TestBackfill_UnrecognizedCheckpointServiceLayer(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	msg := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now())

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	// The mock ignores unrecognized cursors (returns all messages).
	// This only tests that the service does not crash — the validation
	// property is in the store layer.
	result, err := svc.Run(ctx, BackfillConfig{
		ProjectID:  projectID,
		Checkpoint: "bogus-cursor-value",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalProcessed)
}

func TestBackfill_EmptyProjectID(t *testing.T) {
	ctx := context.Background()

	msgStore := &mockMessageStore{}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	_, err := svc.Run(ctx, BackfillConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ProjectID is required")
}

func TestBackfill_LastCheckpointTracked(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	now := time.Now()
	msg1 := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, now.Add(-time.Minute))
	msg2 := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, now)

	msgStore := &mockMessageStore{messages: []store.Message{msg1, msg2}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)

	// Use BatchSize=1 to force multi-page pagination and checkpoint tracking.
	// With default batch size both messages fit on a single page and
	// LastCheckpoint would remain empty (single-page run).
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID, BatchSize: 1})
	require.NoError(t, err)

	assert.NotEmpty(t, result.LastCheckpoint, "multi-page run should track a checkpoint")

	// LastCheckpoint is an opaque page cursor, not a message ID.
	// Verify it can be used for a resume run without error.
	result2, err := svc.Run(ctx, BackfillConfig{
		ProjectID:  projectID,
		Checkpoint: result.LastCheckpoint,
	})
	require.NoError(t, err)
	// The remaining message was already attributed, so resume should skip it.
	assert.Equal(t, 1, result2.TotalProcessed)
	assert.Equal(t, 1, result2.Skipped)
}

func TestBackfill_SinglePageNoCheckpoint(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	msg := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now())

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Empty(t, result.LastCheckpoint,
		"single-page run should have empty checkpoint (all data processed)")
}

// TestBackfill_SameTimestampMessages is the service-level regression test for
// the cursor-based checkpoint fix. It uses a manually constructed checkpoint
// (a message ID, which is the mock store's cursor format) on FRESH data — no
// prior backfill — so the checkpoint's behaviour is the ONLY thing determining
// whether same-timestamp messages get processed.
//
// On FIXED code: checkpoint is used as cursor → mock skips to after that
// position → remaining same-timestamp messages are processed.
//
// On BUGGY code (fda9977f): checkpoint is resolved via GetMessage, then
// filter.After = CreatedAt → "created > T" excludes ALL same-timestamp
// messages → TotalProcessed = 0.
//
// NOTE: This test guards the SERVICE-LAYER mutation site (the filter.After
// form of the bug) against a mock store. The companion test
// TestBackfillResumeViaCheckpoint_SameTimestamp in cmd/ guards the
// STORE-LAYER mutation site (the (created, id) tuple comparison in
// message_store.go). Neither is redundant — they guard different layers.
// Do not remove one believing the other covers it.
func TestBackfill_SameTimestampMessages(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	senderID := uuid.NewString()

	// 5 messages at IDENTICAL timestamp, different recipients.
	same := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var msgs []store.Message
	var ids []string
	for i := 0; i < 5; i++ {
		msg := newTestMessage(projectID, "user:alice", senderID,
			"agent:bot", uuid.NewString(), same)
		msgs = append(msgs, msg)
		ids = append(ids, msg.ID)
	}

	// Sort IDs to get deterministic ordering. The mock sorts DESC by (created, id).
	// Sorted ascending: ids[0] < ids[1] < ids[2] < ids[3] < ids[4]
	// DESC order: ids[4], ids[3], ids[2], ids[1], ids[0]
	sort.Strings(ids)

	msgStore := &mockMessageStore{messages: msgs}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)

	// Use ids[2] as the checkpoint — the mock's cursor format is a message ID.
	// This simulates resuming after processing ids[4] and ids[3] (the first
	// page in DESC order), with the cursor positioned at ids[2].
	// Messages after the cursor in DESC: ids[1], ids[0] — these should be processed.
	//
	// NO prior backfill run — all messages still have empty conversation_id.
	result, err := svc.Run(ctx, BackfillConfig{
		ProjectID:  projectID,
		Checkpoint: ids[2],
	})
	require.NoError(t, err)

	// EXACT counts — this is the regression guard.
	// Fixed code (cursor-based): processes ids[1] and ids[0] → TotalProcessed = 2.
	// Buggy code (filter.After = T): "created > T" matches 0 messages → TotalProcessed = 0.
	assert.Equal(t, 2, result.TotalProcessed,
		"cursor-based resume must process same-timestamp messages after cursor position")
	assert.Equal(t, 2, result.Attributed,
		"should attribute the 2 same-timestamp messages after cursor")
	assert.Equal(t, 0, result.Skipped,
		"no messages should be skipped (none have conversation_id)")
}

// ---------------------------------------------------------------------------
// AC-DEF15-6: dm:-prefixed ThreadID backfill produces kind=direct
// ---------------------------------------------------------------------------

func TestBackfill_DMPrefixedThreadID_ProducesDirectConversation(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()

	// Build a canonical dm: key.
	dmKey, err := messages.DMConversationKey("agent", agentID, "user", userID)
	require.NoError(t, err)

	// Create a message whose ThreadID is dm:-prefixed.
	msg := newTestMessage(projectID, "user:alice", userID, "agent:bot", agentID, time.Now())
	msg.ThreadID = dmKey

	msgStore := &mockMessageStore{messages: []store.Message{msg}}
	convStore := &mockConversationStore{}
	agents := &mockAgentLookup{}

	svc := NewBackfillService(convStore, msgStore, agents)
	result, err := svc.Run(ctx, BackfillConfig{ProjectID: projectID})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Attributed, "dm:-prefixed ThreadID should be attributed")
	assert.Equal(t, 1, result.ConversationsCreated)

	// Verify the created conversation.
	stamped, err := msgStore.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, stamped.ConversationID)

	conv, err := convStore.GetConversation(ctx, stamped.ConversationID)
	require.NoError(t, err)

	// AC-DEF15-6: kind must be "direct", not "group".
	assert.Equal(t, "direct", conv.Kind,
		"AC-DEF15-6: dm:-prefixed ThreadID must produce kind=direct")

	// AC-DEF15-6: external_ref must NOT match "thread:%:dm:%" pattern.
	assert.False(t, strings.HasPrefix(conv.ExternalRef, "thread:"),
		"AC-DEF15-6: external_ref must not have thread: prefix for dm: keys")

	// AC-DEF15-6: external_ref must equal the dm: key verbatim.
	assert.Equal(t, dmKey, conv.ExternalRef,
		"AC-DEF15-6: external_ref must equal the dm: key verbatim")
}
