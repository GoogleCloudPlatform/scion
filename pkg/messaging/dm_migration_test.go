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
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock DMMigrationStore
// ---------------------------------------------------------------------------

type mockMigrationStore struct {
	conversations map[string]*store.Conversation
	participants  map[string][]store.ConversationParticipant // key: conversationID
	users         map[string]*store.User                     // key: user ID
	agents        map[string]*store.Agent                    // key: agent ID
	messages      map[string]*store.Message                  // key: message ID
}

func newMockMigrationStore() *mockMigrationStore {
	return &mockMigrationStore{
		conversations: make(map[string]*store.Conversation),
		participants:  make(map[string][]store.ConversationParticipant),
		users:         make(map[string]*store.User),
		agents:        make(map[string]*store.Agent),
		messages:      make(map[string]*store.Message),
	}
}

func (m *mockMigrationStore) GetConversation(_ context.Context, id string) (*store.Conversation, error) {
	conv, ok := m.conversations[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return conv, nil
}

func (m *mockMigrationStore) UpdateConversation(_ context.Context, conv *store.Conversation) error {
	if _, ok := m.conversations[conv.ID]; !ok {
		return store.ErrNotFound
	}
	m.conversations[conv.ID] = conv
	return nil
}

func (m *mockMigrationStore) DeleteConversation(_ context.Context, id string) error {
	conv, ok := m.conversations[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now()
	conv.DeletedAt = &now
	return nil
}

func (m *mockMigrationStore) ListConversations(_ context.Context, filter store.ConversationFilter, opts store.ListOptions) (*store.ListResult[store.Conversation], error) {
	var items []store.Conversation
	for _, conv := range m.conversations {
		// Skip soft-deleted conversations.
		if conv.DeletedAt != nil {
			continue
		}
		if filter.Kind != "" && conv.Kind != filter.Kind {
			continue
		}
		if filter.Surface != "" && conv.Surface != filter.Surface {
			continue
		}
		items = append(items, *conv)
	}

	// Sort by ID for deterministic pagination.
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	// Apply cursor.
	if opts.Cursor != "" {
		idx := 0
		for i, it := range items {
			if it.ID == opts.Cursor {
				idx = i + 1
				break
			}
		}
		items = items[idx:]
	}

	if opts.Limit > 0 && len(items) > opts.Limit {
		nextCursor := items[opts.Limit-1].ID
		return &store.ListResult[store.Conversation]{
			Items:      items[:opts.Limit],
			TotalCount: len(items),
			NextCursor: nextCursor,
		}, nil
	}

	return &store.ListResult[store.Conversation]{Items: items, TotalCount: len(items)}, nil
}

func (m *mockMigrationStore) GetConversationByExternalRef(_ context.Context, surface, externalRef string) (*store.Conversation, error) {
	for _, conv := range m.conversations {
		if conv.DeletedAt != nil {
			continue
		}
		if conv.Surface == surface && conv.ExternalRef == externalRef {
			return conv, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockMigrationStore) AddParticipant(_ context.Context, p *store.ConversationParticipant) error {
	for _, existing := range m.participants[p.ConversationID] {
		if existing.PrincipalKind == p.PrincipalKind && existing.PrincipalID == p.PrincipalID {
			return store.ErrAlreadyExists
		}
	}
	m.participants[p.ConversationID] = append(m.participants[p.ConversationID], *p)
	return nil
}

func (m *mockMigrationStore) ListParticipants(_ context.Context, conversationID string) ([]store.ConversationParticipant, error) {
	return m.participants[conversationID], nil
}

func (m *mockMigrationStore) GetUser(_ context.Context, id string) (*store.User, error) {
	user, ok := m.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return user, nil
}

func (m *mockMigrationStore) GetAgent(_ context.Context, id string) (*store.Agent, error) {
	agent, ok := m.agents[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return agent, nil
}

func (m *mockMigrationStore) ListMessages(_ context.Context, filter store.MessageFilter, opts store.ListOptions) (*store.ListResult[store.Message], error) {
	var items []store.Message
	for _, msg := range m.messages {
		if filter.ConversationID != "" && msg.ConversationID != filter.ConversationID {
			continue
		}
		items = append(items, *msg)
	}

	// Sort by ID for deterministic pagination.
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	// Apply cursor.
	if opts.Cursor != "" {
		idx := 0
		for i, it := range items {
			if it.ID == opts.Cursor {
				idx = i + 1
				break
			}
		}
		items = items[idx:]
	}

	if opts.Limit > 0 && len(items) > opts.Limit {
		nextCursor := items[opts.Limit-1].ID
		return &store.ListResult[store.Message]{
			Items:      items[:opts.Limit],
			TotalCount: len(items),
			NextCursor: nextCursor,
		}, nil
	}

	return &store.ListResult[store.Message]{Items: items, TotalCount: len(items)}, nil
}

func (m *mockMigrationStore) SetMessageConversationID(_ context.Context, messageID, conversationID string) error {
	msg, ok := m.messages[messageID]
	if !ok {
		return store.ErrNotFound
	}
	msg.ConversationID = conversationID
	return nil
}

// addConv is a helper to add a conversation with optional participants.
func (m *mockMigrationStore) addConv(conv *store.Conversation, participants ...store.ConversationParticipant) {
	m.conversations[conv.ID] = conv
	m.participants[conv.ID] = participants
}

// addMessage is a helper to add a message.
func (m *mockMigrationStore) addMessage(msg *store.Message) {
	m.messages[msg.ID] = msg
}

// ---------------------------------------------------------------------------
// Step 2: Listing-index rebuild tests
// ---------------------------------------------------------------------------

// TestStep2_KindEncodedRowAddsParticipants verifies that a kind-encoded row
// with no participants gets both principals added after migration.
func TestStep2_KindEncodedRowAddsParticipants(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	userID := uuid.NewString()
	agentID := uuid.NewString()
	convID := uuid.NewString()

	// Register both principals in their tables.
	ms.users[userID] = &store.User{ID: userID, Email: "test@example.com"}
	ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "test-agent"}

	// Kind-encoded row with NO participants.
	extRef := mustDMKey("user", userID, "agent", agentID)
	ms.addConv(&store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
	})

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	// Verify participants were added.
	parts := ms.participants[convID]
	require.Len(t, parts, 2, "expected 2 participants, got %d", len(parts))

	var hasUser, hasAgent bool
	for _, p := range parts {
		if p.PrincipalKind == "user" && p.PrincipalID == userID {
			hasUser = true
		}
		if p.PrincipalKind == "agent" && p.PrincipalID == agentID {
			hasAgent = true
		}
	}
	assert.True(t, hasUser, "user participant should be added")
	assert.True(t, hasAgent, "agent participant should be added")
	assert.Equal(t, 2, result.ParticipantsAdded, "ParticipantsAdded should be 2")
	assert.Equal(t, 1, result.TotalScanned, "TotalScanned should be 1")
}

// TestStep2_SkipsWhenPrincipalNotFound verifies that when one principal doesn't
// exist, the entire row is skipped (all-or-nothing).
func TestStep2_SkipsWhenPrincipalNotFound(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	userID := uuid.NewString()
	agentID := uuid.NewString()
	convID := uuid.NewString()

	// Only register the user — agent does NOT exist.
	ms.users[userID] = &store.User{ID: userID, Email: "test@example.com"}

	extRef := mustDMKey("user", userID, "agent", agentID)
	ms.addConv(&store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
	})

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	// No participants should be added (all-or-nothing skip).
	parts := ms.participants[convID]
	assert.Len(t, parts, 0, "no participants should be added when one principal is missing")
	assert.Equal(t, 0, result.ParticipantsAdded)
	assert.Equal(t, 1, result.Unparseable, "should be counted as unparseable")
}

// TestStep2_IdempotentExistingParticipants verifies that re-running migration
// on a row that already has participants doesn't error.
func TestStep2_IdempotentExistingParticipants(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	userID := uuid.NewString()
	agentID := uuid.NewString()
	convID := uuid.NewString()

	ms.users[userID] = &store.User{ID: userID, Email: "test@example.com"}
	ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "test-agent"}

	extRef := mustDMKey("user", userID, "agent", agentID)
	ms.addConv(&store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
	},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: userID},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "agent", PrincipalID: agentID},
	)

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	assert.Equal(t, 0, result.ParticipantsAdded, "no new participants should be added")
	assert.Len(t, result.Errors, 0, "no errors on idempotent run")
}

// ---------------------------------------------------------------------------
// Step 3a: Empty-ref merge/re-key tests
// ---------------------------------------------------------------------------

// TestStep3a_MergeEmptyRefWithExisting verifies that an empty-ref row is merged
// with an existing kind-encoded row — messages re-stamped, old row deleted.
func TestStep3a_MergeEmptyRefWithExisting(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	userID := uuid.NewString()
	agentID := uuid.NewString()
	oldConvID := uuid.NewString()
	newConvID := uuid.NewString()

	ms.users[userID] = &store.User{ID: userID, Email: "test@example.com"}
	ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "test-agent"}

	// Old empty-ref row with participants.
	ms.addConv(&store.Conversation{
		ID:          oldConvID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "",
	},
		store.ConversationParticipant{ConversationID: oldConvID, PrincipalKind: "user", PrincipalID: userID},
		store.ConversationParticipant{ConversationID: oldConvID, PrincipalKind: "agent", PrincipalID: agentID},
	)

	// Existing kind-encoded row for the same pair.
	extRef := mustDMKey("user", userID, "agent", agentID)
	ms.addConv(&store.Conversation{
		ID:          newConvID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
	},
		store.ConversationParticipant{ConversationID: newConvID, PrincipalKind: "user", PrincipalID: userID},
		store.ConversationParticipant{ConversationID: newConvID, PrincipalKind: "agent", PrincipalID: agentID},
	)

	// Add a message to the old conversation.
	msgID := uuid.NewString()
	ms.addMessage(&store.Message{
		ID:             msgID,
		ConversationID: oldConvID,
		Msg:            "hello",
	})

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	// Old row should be soft-deleted.
	assert.NotNil(t, ms.conversations[oldConvID].DeletedAt, "old row should be soft-deleted")

	// Message should be re-stamped to the new conversation.
	assert.Equal(t, newConvID, ms.messages[msgID].ConversationID,
		"message should be re-stamped to new conversation")

	assert.Equal(t, 1, result.EmptyRefMerged, "EmptyRefMerged should be 1")
}

// TestStep3a_RekeyEmptyRefInPlace verifies that an empty-ref row with no
// existing kind-encoded counterpart gets re-keyed in place.
func TestStep3a_RekeyEmptyRefInPlace(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	userID := uuid.NewString()
	agentID := uuid.NewString()
	convID := uuid.NewString()
	projectID := "some-project"

	ms.users[userID] = &store.User{ID: userID, Email: "test@example.com"}
	ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "test-agent"}

	// Empty-ref row with a ProjectID that should be cleared.
	ms.addConv(&store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "",
		ProjectID:   &projectID,
	},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: userID},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "agent", PrincipalID: agentID},
	)

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	// External ref should be set to the kind-encoded key.
	expectedKey := mustDMKey("user", userID, "agent", agentID)
	conv := ms.conversations[convID]
	assert.Equal(t, expectedKey, conv.ExternalRef, "ExternalRef should be set to kind-encoded key")
	assert.Nil(t, conv.ProjectID, "ProjectID should be nil (DMs are global)")
	assert.Equal(t, 1, result.EmptyRefRekeyed, "EmptyRefRekeyed should be 1")
}

// TestStep3a_SkipsWhenParticipantsNot2 verifies that empty-ref rows with != 2
// active participants are skipped.
func TestStep3a_SkipsWhenParticipantsNot2(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	convID := uuid.NewString()
	userID := uuid.NewString()

	// Only 1 participant.
	ms.addConv(&store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "",
	},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: userID},
	)

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Unparseable, "should be unparseable with 1 participant")
	assert.Equal(t, 0, result.EmptyRefRekeyed)
	assert.Equal(t, 0, result.EmptyRefMerged)
}

// ---------------------------------------------------------------------------
// Step 3b: Old-format re-key tests
// ---------------------------------------------------------------------------

// TestStep3b_OldFormatRekey verifies that an old dm:id1:id2 row is re-keyed
// to the kind-encoded format when both IDs resolve unambiguously.
func TestStep3b_OldFormatRekey(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	userID := uuid.NewString()
	agentID := uuid.NewString()
	convID := uuid.NewString()
	projectID := "old-project"

	ms.users[userID] = &store.User{ID: userID, Email: "test@example.com"}
	ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "test-agent"}

	// Old format key: dm:{sorted(id1,id2)}.
	oldKey := directMessageExternalRef(userID, agentID)
	ms.addConv(&store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
		ProjectID:   &projectID,
	})

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	// Should be re-keyed to kind-encoded format.
	expectedKey := mustDMKey("user", userID, "agent", agentID)
	conv := ms.conversations[convID]
	assert.Equal(t, expectedKey, conv.ExternalRef, "should be re-keyed to kind-encoded format")
	assert.Nil(t, conv.ProjectID, "ProjectID should be nil (DMs are global)")
	assert.Equal(t, 1, result.OldFormatRekeyed, "OldFormatRekeyed should be 1")
}

// TestStep3b_AmbiguousIDInNeither verifies that when an ID is found in neither
// the user nor agent table, it's counted as ambiguous and skipped.
func TestStep3b_AmbiguousIDInNeither(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	id1 := uuid.NewString()
	id2 := uuid.NewString()
	convID := uuid.NewString()

	// Neither ID exists in any table.
	oldKey := directMessageExternalRef(id1, id2)
	ms.addConv(&store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	})

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Ambiguous, "should be counted as ambiguous")
	assert.Equal(t, 0, result.OldFormatRekeyed)
}

// TestStep3b_AmbiguousIDInBoth verifies that when an ID exists in both user
// and agent tables, it's counted as ambiguous.
func TestStep3b_AmbiguousIDInBoth(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	sharedID := uuid.NewString()
	otherID := uuid.NewString()
	convID := uuid.NewString()

	// Same ID in both tables — ambiguous.
	ms.users[sharedID] = &store.User{ID: sharedID, Email: "ambig@example.com"}
	ms.agents[sharedID] = &store.Agent{ID: sharedID, Slug: "ambig-agent"}
	ms.users[otherID] = &store.User{ID: otherID, Email: "other@example.com"}

	oldKey := directMessageExternalRef(sharedID, otherID)
	ms.addConv(&store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	})

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Ambiguous, "should be counted as ambiguous")
	assert.Equal(t, 0, result.OldFormatRekeyed)
}

// TestStep3b_OldFormatMerge verifies that an old-format row is merged with
// an existing kind-encoded row when both exist for the same pair.
func TestStep3b_OldFormatMerge(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	userID := uuid.NewString()
	agentID := uuid.NewString()
	oldConvID := uuid.NewString()
	newConvID := uuid.NewString()

	ms.users[userID] = &store.User{ID: userID, Email: "test@example.com"}
	ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "test-agent"}

	// Old-format row.
	oldKey := directMessageExternalRef(userID, agentID)
	ms.addConv(&store.Conversation{
		ID:          oldConvID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	},
		store.ConversationParticipant{ConversationID: oldConvID, PrincipalKind: "user", PrincipalID: userID},
		store.ConversationParticipant{ConversationID: oldConvID, PrincipalKind: "agent", PrincipalID: agentID},
	)

	// Existing kind-encoded row.
	extRef := mustDMKey("user", userID, "agent", agentID)
	ms.addConv(&store.Conversation{
		ID:          newConvID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
	},
		store.ConversationParticipant{ConversationID: newConvID, PrincipalKind: "user", PrincipalID: userID},
		store.ConversationParticipant{ConversationID: newConvID, PrincipalKind: "agent", PrincipalID: agentID},
	)

	// Add a message to the old conversation.
	msgID := uuid.NewString()
	ms.addMessage(&store.Message{
		ID:             msgID,
		ConversationID: oldConvID,
		Msg:            "old message",
	})

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	// Old row soft-deleted, message re-stamped.
	assert.NotNil(t, ms.conversations[oldConvID].DeletedAt, "old row should be soft-deleted")
	assert.Equal(t, newConvID, ms.messages[msgID].ConversationID, "message re-stamped")
	assert.Equal(t, 1, result.OldFormatRekeyed)
}

// ---------------------------------------------------------------------------
// DryRun test
// ---------------------------------------------------------------------------

// TestDryRun_NoWrites verifies that DryRun=true computes statistics without
// making any changes.
func TestDryRun_NoWrites(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	userID := uuid.NewString()
	agentID := uuid.NewString()

	ms.users[userID] = &store.User{ID: userID, Email: "test@example.com"}
	ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "test-agent"}

	// Kind-encoded row with no participants (step 2 candidate).
	convID1 := uuid.NewString()
	extRef := mustDMKey("user", userID, "agent", agentID)
	ms.addConv(&store.Conversation{
		ID:          convID1,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
	})

	// Empty-ref row (step 3a candidate).
	user2ID := uuid.NewString()
	agent2ID := uuid.NewString()
	ms.users[user2ID] = &store.User{ID: user2ID, Email: "test2@example.com"}
	ms.agents[agent2ID] = &store.Agent{ID: agent2ID, Slug: "test-agent-2"}

	convID2 := uuid.NewString()
	ms.addConv(&store.Conversation{
		ID:          convID2,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "",
	},
		store.ConversationParticipant{ConversationID: convID2, PrincipalKind: "user", PrincipalID: user2ID},
		store.ConversationParticipant{ConversationID: convID2, PrincipalKind: "agent", PrincipalID: agent2ID},
	)

	// Old-format row (step 3b candidate).
	user3ID := uuid.NewString()
	agent3ID := uuid.NewString()
	ms.users[user3ID] = &store.User{ID: user3ID, Email: "test3@example.com"}
	ms.agents[agent3ID] = &store.Agent{ID: agent3ID, Slug: "test-agent-3"}

	convID3 := uuid.NewString()
	oldKey := directMessageExternalRef(user3ID, agent3ID)
	ms.addConv(&store.Conversation{
		ID:          convID3,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	})

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{DryRun: true})
	require.NoError(t, err)

	// Statistics should be computed.
	assert.Equal(t, 3, result.TotalScanned, "should scan all 3 conversations")
	assert.Equal(t, 2, result.ParticipantsAdded, "should count 2 missing participants")
	assert.Equal(t, 1, result.EmptyRefRekeyed, "should count 1 re-key")
	assert.Equal(t, 1, result.OldFormatRekeyed, "should count 1 old-format re-key")

	// No actual changes should be made.
	assert.Len(t, ms.participants[convID1], 0, "no participants should be added in dry run")
	assert.Equal(t, "", ms.conversations[convID2].ExternalRef, "external ref unchanged in dry run")
	assert.Equal(t, oldKey, ms.conversations[convID3].ExternalRef, "old key unchanged in dry run")
}

// ---------------------------------------------------------------------------
// Guard tests (permanent post-migration invariant assertions)
// ---------------------------------------------------------------------------

// TestGuardA_Migration_NoEmptyRefDirectRows asserts that after migration,
// zero non-deleted direct conversations have an empty external_ref.
// Floor: the test creates at least 2 such rows before migration (rule 14).
func TestGuardA_Migration_NoEmptyRefDirectRows(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	// Create 2 empty-ref direct rows.
	for i := 0; i < 2; i++ {
		userID := uuid.NewString()
		agentID := uuid.NewString()
		convID := uuid.NewString()
		ms.users[userID] = &store.User{ID: userID, Email: "guard-a@example.com"}
		ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "guard-a-agent"}

		ms.addConv(&store.Conversation{
			ID:          convID,
			Kind:        "direct",
			Surface:     "native",
			ExternalRef: "",
		},
			store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: userID},
			store.ConversationParticipant{ConversationID: convID, PrincipalKind: "agent", PrincipalID: agentID},
		)
	}

	// Also create a kind-encoded row that's fine already.
	userOK := uuid.NewString()
	agentOK := uuid.NewString()
	ms.users[userOK] = &store.User{ID: userOK, Email: "ok@example.com"}
	ms.agents[agentOK] = &store.Agent{ID: agentOK, Slug: "ok-agent"}
	okRef := mustDMKey("user", userOK, "agent", agentOK)
	ms.addConv(&store.Conversation{
		ID:          uuid.NewString(),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: okRef,
	},
		store.ConversationParticipant{ConversationID: "", PrincipalKind: "user", PrincipalID: userOK},
	)

	// Run migration.
	svc := NewDMMigrationService(ms)
	_, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	// Guard assertion: zero non-deleted direct conversations with empty ref.
	var directCount, emptyRefCount int
	for _, conv := range ms.conversations {
		if conv.Kind != "direct" || conv.DeletedAt != nil {
			continue
		}
		directCount++
		if conv.ExternalRef == "" {
			emptyRefCount++
		}
	}

	// Rule 14: floor — at least 3 rows examined.
	require.GreaterOrEqual(t, directCount, 3,
		"floor violation: expected at least 3 direct conversations, found %d", directCount)
	assert.Equal(t, 0, emptyRefCount,
		"found %d non-deleted direct conversations with empty external_ref", emptyRefCount)
}

// TestGuardB_Migration_EveryDMRowHasTwoParticipants asserts that after migration,
// every non-deleted dm: row has exactly two participants.
// Floor: >= 3 dm: rows examined.
func TestGuardB_Migration_EveryDMRowHasTwoParticipants(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	// Create 3 kind-encoded rows with no participants.
	for i := 0; i < 3; i++ {
		userID := uuid.NewString()
		agentID := uuid.NewString()
		ms.users[userID] = &store.User{ID: userID, Email: "guard-b@example.com"}
		ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "guard-b-agent"}

		extRef := mustDMKey("user", userID, "agent", agentID)
		ms.addConv(&store.Conversation{
			ID:          uuid.NewString(),
			Kind:        "direct",
			Surface:     "native",
			ExternalRef: extRef,
		})
	}

	// Run migration (should add participants).
	svc := NewDMMigrationService(ms)
	_, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	// Guard assertion.
	var dmCount int
	for _, conv := range ms.conversations {
		if conv.DeletedAt != nil {
			continue
		}
		if !strings.HasPrefix(conv.ExternalRef, "dm:") {
			continue
		}
		dmCount++
		parts := ms.participants[conv.ID]
		assert.Len(t, parts, 2,
			"dm: conversation %s has %d participants, expected 2", conv.ID, len(parts))
	}

	require.GreaterOrEqual(t, dmCount, 3,
		"floor violation: expected at least 3 dm: conversations, found %d", dmCount)
}

// TestGuardC_Migration_AllDMKeysAreParseable asserts that after migration,
// every non-deleted dm: row with participants has a key that ParseDMKey accepts.
// Floor: >= 3 such rows.
func TestGuardC_Migration_AllDMKeysAreParseable(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	// Create 3 kind-encoded rows with no participants (step 2 will add them).
	for i := 0; i < 3; i++ {
		userID := uuid.NewString()
		agentID := uuid.NewString()
		ms.users[userID] = &store.User{ID: userID, Email: "guard-c@example.com"}
		ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "guard-c-agent"}

		extRef := mustDMKey("user", userID, "agent", agentID)
		ms.addConv(&store.Conversation{
			ID:          uuid.NewString(),
			Kind:        "direct",
			Surface:     "native",
			ExternalRef: extRef,
		})
	}

	// Also create an old-format row with participants (step 3b will re-key it,
	// and step 2 won't touch it in this pass — but it already has participants).
	userOld := uuid.NewString()
	agentOld := uuid.NewString()
	ms.users[userOld] = &store.User{ID: userOld, Email: "old@example.com"}
	ms.agents[agentOld] = &store.Agent{ID: agentOld, Slug: "old-agent"}
	oldKey := directMessageExternalRef(userOld, agentOld)
	oldConvID := uuid.NewString()
	ms.addConv(&store.Conversation{
		ID:          oldConvID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	},
		store.ConversationParticipant{ConversationID: oldConvID, PrincipalKind: "user", PrincipalID: userOld},
		store.ConversationParticipant{ConversationID: oldConvID, PrincipalKind: "agent", PrincipalID: agentOld},
	)

	// Run migration.
	svc := NewDMMigrationService(ms)
	_, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	// Guard assertion.
	var dmWithPartsCount int
	for _, conv := range ms.conversations {
		if conv.DeletedAt != nil {
			continue
		}
		if !strings.HasPrefix(conv.ExternalRef, "dm:") {
			continue
		}
		parts := ms.participants[conv.ID]
		if len(parts) == 0 {
			continue
		}
		dmWithPartsCount++
		_, _, _, _, parseErr := messages.ParseDMKey(conv.ExternalRef)
		assert.NoError(t, parseErr,
			"dm: conversation %s has unparseable key %q", conv.ID, conv.ExternalRef)
	}

	require.GreaterOrEqual(t, dmWithPartsCount, 3,
		"floor violation: expected at least 3 dm: rows with participants, found %d", dmWithPartsCount)
}

// ---------------------------------------------------------------------------
// Mixed scenario test
// ---------------------------------------------------------------------------

// TestMigration_MixedScenarios verifies that all three categories of rows
// are processed correctly in a single migration run.
func TestMigration_MixedScenarios(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()

	// Category 1: Kind-encoded row with no participants.
	user1 := uuid.NewString()
	agent1 := uuid.NewString()
	ms.users[user1] = &store.User{ID: user1}
	ms.agents[agent1] = &store.Agent{ID: agent1}

	conv1ID := uuid.NewString()
	ref1 := mustDMKey("user", user1, "agent", agent1)
	ms.addConv(&store.Conversation{
		ID: conv1ID, Kind: "direct", Surface: "native", ExternalRef: ref1,
	})

	// Category 2: Empty-ref row.
	user2 := uuid.NewString()
	agent2 := uuid.NewString()
	ms.users[user2] = &store.User{ID: user2}
	ms.agents[agent2] = &store.Agent{ID: agent2}

	conv2ID := uuid.NewString()
	ms.addConv(&store.Conversation{
		ID: conv2ID, Kind: "direct", Surface: "native", ExternalRef: "",
	},
		store.ConversationParticipant{ConversationID: conv2ID, PrincipalKind: "user", PrincipalID: user2},
		store.ConversationParticipant{ConversationID: conv2ID, PrincipalKind: "agent", PrincipalID: agent2},
	)

	// Category 3: Old-format row.
	user3 := uuid.NewString()
	agent3 := uuid.NewString()
	ms.users[user3] = &store.User{ID: user3}
	ms.agents[agent3] = &store.Agent{ID: agent3}

	conv3ID := uuid.NewString()
	oldKey := directMessageExternalRef(user3, agent3)
	ms.addConv(&store.Conversation{
		ID: conv3ID, Kind: "direct", Surface: "native", ExternalRef: oldKey,
	})

	svc := NewDMMigrationService(ms)
	result, err := svc.Run(ctx, DMMigrationConfig{})
	require.NoError(t, err)

	assert.Equal(t, 3, result.TotalScanned)
	assert.Equal(t, 2, result.ParticipantsAdded, "2 participants from kind-encoded row")
	assert.Equal(t, 1, result.EmptyRefRekeyed, "1 empty-ref re-keyed")
	assert.Equal(t, 1, result.OldFormatRekeyed, "1 old-format re-keyed")
	assert.Equal(t, 0, result.Unparseable)
	assert.Equal(t, 0, result.Ambiguous)
	assert.Len(t, result.Errors, 0)
}

// ---------------------------------------------------------------------------
// B2: Atomicity — re-stamp failure must not delete the old row
// ---------------------------------------------------------------------------

// failingRestamp wraps the mock and makes message re-stamping always fail.
type failingRestamp struct{ *mockMigrationStore }

func (f *failingRestamp) SetMessageConversationID(_ context.Context, _, _ string) error {
	return errors.New("simulated DB failure during re-stamp")
}

// TestB2_MergeAbortsOnRestampFailure verifies that mergeConversation does NOT
// soft-delete the old conversation row when re-stamping messages fails.
//
// Mutation contract: removing the restampFailed abort check causes this test
// to fail because the old row gets soft-deleted despite re-stamp failure.
func TestB2_MergeAbortsOnRestampFailure(t *testing.T) {
	ms := newMockMigrationStore()
	ctx := context.Background()
	userID, agentID := uuid.NewString(), uuid.NewString()
	oldConvID, newConvID := uuid.NewString(), uuid.NewString()

	ms.users[userID] = &store.User{ID: userID, Email: "t@e.com"}
	ms.agents[agentID] = &store.Agent{ID: agentID, Slug: "a"}

	// Old empty-ref row with a message.
	ms.addConv(&store.Conversation{ID: oldConvID, Kind: "direct", Surface: "native", ExternalRef: ""},
		store.ConversationParticipant{ConversationID: oldConvID, PrincipalKind: "user", PrincipalID: userID},
		store.ConversationParticipant{ConversationID: oldConvID, PrincipalKind: "agent", PrincipalID: agentID})

	// Target kind-encoded row.
	ms.addConv(&store.Conversation{ID: newConvID, Kind: "direct", Surface: "native",
		ExternalRef: mustDMKey("user", userID, "agent", agentID)},
		store.ConversationParticipant{ConversationID: newConvID, PrincipalKind: "user", PrincipalID: userID},
		store.ConversationParticipant{ConversationID: newConvID, PrincipalKind: "agent", PrincipalID: agentID})

	msgID := uuid.NewString()
	ms.addMessage(&store.Message{ID: msgID, ConversationID: oldConvID, Msg: "important history"})

	svc := NewDMMigrationService(&failingRestamp{ms})
	result, _ := svc.Run(ctx, DMMigrationConfig{})

	// (a) Old row must NOT be soft-deleted.
	assert.Nil(t, ms.conversations[oldConvID].DeletedAt,
		"old row must not be soft-deleted when re-stamp fails")

	// (b) Message must still point at old conversation.
	assert.Equal(t, oldConvID, ms.messages[msgID].ConversationID,
		"message must still reference old conversation after re-stamp failure")

	// (c) Errors must be reported.
	assert.NotEmpty(t, result.Errors,
		"result.Errors must be non-empty after re-stamp failure")
}
