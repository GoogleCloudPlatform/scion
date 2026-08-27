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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestConversationStore(t *testing.T) *ConversationStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewConversationStore(client)
}

func newTestConversation() *store.Conversation {
	return &store.Conversation{
		ID:      uuid.NewString(),
		Kind:    "group",
		Surface: "native",
	}
}

// newTestDMConversation creates a direct conversation with a valid kind-encoded
// DM key. Returns the conversation and the two participant identities (kindA/idA,
// kindB/idB) that are named in the key.
func newTestDMConversation(kindA, idA, kindB, idB string) *store.Conversation {
	extRef, err := messages.DMConversationKey(kindA, idA, kindB, idB)
	if err != nil {
		panic("newTestDMConversation: " + err.Error())
	}
	return &store.Conversation{
		ID:          uuid.NewString(),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
	}
}

// ---------------------------------------------------------------------------
// Conversation CRUD
// ---------------------------------------------------------------------------

func TestConversationCRUD(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	agentID := uuid.NewString()
	conv := &store.Conversation{
		ID:             uuid.NewString(),
		ProjectID:      &projectID,
		Kind:           "group",
		Surface:        "slack",
		ExternalRef:    "C123456",
		ParentRef:      "T789",
		DisplayName:    "Design discussion",
		DefaultAgentID: &agentID,
		DriftState:     "active",
	}
	require.NoError(t, s.CreateConversation(ctx, conv))
	assert.False(t, conv.CreatedAt.IsZero())
	assert.False(t, conv.LastActivityAt.IsZero())

	// Get
	got, err := s.GetConversation(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, conv.ID, got.ID)
	assert.Equal(t, &projectID, got.ProjectID)
	assert.Equal(t, "group", got.Kind)
	assert.Equal(t, "slack", got.Surface)
	assert.Equal(t, "C123456", got.ExternalRef)
	assert.Equal(t, "T789", got.ParentRef)
	assert.Equal(t, "Design discussion", got.DisplayName)
	assert.Equal(t, &agentID, got.DefaultAgentID)
	assert.Equal(t, "active", got.DriftState)

	// Update
	got.DisplayName = "Updated discussion"
	got.DriftState = "orphaned"
	require.NoError(t, s.UpdateConversation(ctx, got))
	updated, err := s.GetConversation(ctx, got.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated discussion", updated.DisplayName)
	assert.Equal(t, "orphaned", updated.DriftState)

	// Delete (soft)
	require.NoError(t, s.DeleteConversation(ctx, conv.ID))

	// Get after soft-delete should return ErrNotFound
	_, err = s.GetConversation(ctx, conv.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestConversationGetNotFound(t *testing.T) {
	s := newTestConversationStore(t)
	_, err := s.GetConversation(context.Background(), uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestConversationDeleteNotFound(t *testing.T) {
	s := newTestConversationStore(t)
	err := s.DeleteConversation(context.Background(), uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestConversationSoftDeleteExcludedFromList(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv1 := newTestConversation()
	conv2 := newTestConversation()
	require.NoError(t, s.CreateConversation(ctx, conv1))
	require.NoError(t, s.CreateConversation(ctx, conv2))

	// Soft-delete conv1
	require.NoError(t, s.DeleteConversation(ctx, conv1.ID))

	// List should only return conv2
	result, err := s.ListConversations(ctx, store.ConversationFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, result.Items, 1)
	assert.Equal(t, conv2.ID, result.Items[0].ID)
}

func TestConversationListFilters(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	conv1 := &store.Conversation{
		ID:        uuid.NewString(),
		ProjectID: &projectID,
		Kind:      "direct",
		Surface:   "native",
	}
	conv2 := &store.Conversation{
		ID:      uuid.NewString(),
		Kind:    "group",
		Surface: "slack",
	}
	require.NoError(t, s.CreateConversation(ctx, conv1))
	require.NoError(t, s.CreateConversation(ctx, conv2))

	// Filter by kind
	result, err := s.ListConversations(ctx, store.ConversationFilter{Kind: "direct"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, result.Items, 1)
	assert.Equal(t, conv1.ID, result.Items[0].ID)

	// Filter by surface
	result, err = s.ListConversations(ctx, store.ConversationFilter{Surface: "slack"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, result.Items, 1)
	assert.Equal(t, conv2.ID, result.Items[0].ID)

	// Filter by project
	result, err = s.ListConversations(ctx, store.ConversationFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, result.Items, 1)
	assert.Equal(t, conv1.ID, result.Items[0].ID)
}

func TestConversationListPagination(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	// Create 5 conversations with staggered activity times
	for i := 0; i < 5; i++ {
		conv := &store.Conversation{
			ID:             uuid.NewString(),
			Kind:           "direct",
			Surface:        "native",
			LastActivityAt: time.Now().Add(time.Duration(i) * time.Second),
		}
		require.NoError(t, s.CreateConversation(ctx, conv))
	}

	// First page
	result, err := s.ListConversations(ctx, store.ConversationFilter{}, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, result.Items, 2)
	assert.NotEmpty(t, result.NextCursor)
	assert.Equal(t, 5, result.TotalCount)

	// Second page
	result2, err := s.ListConversations(ctx, store.ConversationFilter{}, store.ListOptions{Limit: 2, Cursor: result.NextCursor})
	require.NoError(t, err)
	assert.Len(t, result2.Items, 2)
	assert.NotEmpty(t, result2.NextCursor)

	// Third page
	result3, err := s.ListConversations(ctx, store.ConversationFilter{}, store.ListOptions{Limit: 2, Cursor: result2.NextCursor})
	require.NoError(t, err)
	assert.Len(t, result3.Items, 1)
	assert.Empty(t, result3.NextCursor)
}

// ---------------------------------------------------------------------------
// DefaultAgentID validation
// ---------------------------------------------------------------------------

func TestConversationDefaultAgentIDValidation(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	slug := "builder"
	conv := &store.Conversation{
		ID:             uuid.NewString(),
		Kind:           "direct",
		Surface:        "native",
		DefaultAgentID: &slug,
	}
	err := s.CreateConversation(ctx, conv)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

// ---------------------------------------------------------------------------
// UpsertConversationByExternalRef
// ---------------------------------------------------------------------------

func TestUpsertConversationByExternalRef_CreateIfNotExists(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: "channel-123",
		DisplayName: "general",
	}
	result, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)
	assert.NotEmpty(t, result.ID)
	assert.Equal(t, "group", result.Kind)
	assert.Equal(t, "discord", result.Surface)
	assert.Equal(t, "channel-123", result.ExternalRef)
	assert.Equal(t, "general", result.DisplayName)
}

func TestUpsertConversationByExternalRef_UpdateIfExists(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	// First upsert creates
	conv1 := &store.Conversation{
		Kind:        "group",
		Surface:     "slack",
		ExternalRef: "C99999",
		DisplayName: "original-name",
	}
	r1, err := s.UpsertConversationByExternalRef(ctx, conv1)
	require.NoError(t, err)

	// Second upsert with same (surface, external_ref) updates
	conv2 := &store.Conversation{
		Kind:        "group",
		Surface:     "slack",
		ExternalRef: "C99999",
		DisplayName: "updated-name",
	}
	r2, err := s.UpsertConversationByExternalRef(ctx, conv2)
	require.NoError(t, err)

	// Same conversation
	assert.Equal(t, r1.ID, r2.ID)
	assert.Equal(t, "updated-name", r2.DisplayName)
}

func TestUpsertConversationByExternalRef_EmptyDisplayNamePreservesExisting(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	// Create with a display name.
	conv1 := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "dm:agent:aaa:user:bbb",
		DisplayName: "Alice ↔ Builder",
	}
	r1, err := s.UpsertConversationByExternalRef(ctx, conv1)
	require.NoError(t, err)
	assert.Equal(t, "Alice ↔ Builder", r1.DisplayName)

	// Upsert with empty display name — must NOT clobber the existing name.
	conv2 := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "dm:agent:aaa:user:bbb",
		DisplayName: "", // empty
	}
	r2, err := s.UpsertConversationByExternalRef(ctx, conv2)
	require.NoError(t, err)
	assert.Equal(t, r1.ID, r2.ID, "should be the same conversation")
	assert.Equal(t, "Alice ↔ Builder", r2.DisplayName, "original display name must be preserved")
}

func TestUpsertConversationByExternalRef_RequiresExternalRef(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:    "direct",
		Surface: "native",
	}
	_, err := s.UpsertConversationByExternalRef(ctx, conv)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestUpsertConversationByExternalRef_ConcurrentUpsert(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	const goroutines = 5
	var wg sync.WaitGroup
	results := make([]*store.Conversation, goroutines)
	errors := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conv := &store.Conversation{
				Kind:        "group",
				Surface:     "telegram",
				ExternalRef: "concurrent-test-ref",
				DisplayName: "concurrent",
			}
			results[idx], errors[idx] = s.UpsertConversationByExternalRef(ctx, conv)
		}(i)
	}
	wg.Wait()

	// All should succeed
	var ids []string
	for i, err := range errors {
		require.NoError(t, err, "goroutine %d failed", i)
		ids = append(ids, results[i].ID)
	}

	// All should get the same conversation ID
	for _, id := range ids {
		assert.Equal(t, ids[0], id, "concurrent upserts should converge on one conversation")
	}
}

func TestUpsertConversationByExternalRef_DifferentExternalRefsSameSurface(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv1 := &store.Conversation{
		Kind:        "group",
		Surface:     "slack",
		ExternalRef: "C111",
		DisplayName: "channel-1",
	}
	conv2 := &store.Conversation{
		Kind:        "group",
		Surface:     "slack",
		ExternalRef: "C222",
		DisplayName: "channel-2",
	}
	r1, err := s.UpsertConversationByExternalRef(ctx, conv1)
	require.NoError(t, err)
	r2, err := s.UpsertConversationByExternalRef(ctx, conv2)
	require.NoError(t, err)

	// Different conversations
	assert.NotEqual(t, r1.ID, r2.ID)
}

// ---------------------------------------------------------------------------
// Participant operations
// ---------------------------------------------------------------------------

func TestParticipantAddRemoveList(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := newTestConversation()
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Add participant
	p := &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    "user-alice",
		Role:           "member",
	}
	require.NoError(t, s.AddParticipant(ctx, p))
	assert.NotEmpty(t, p.ID)
	assert.False(t, p.JoinedAt.IsZero())

	// Add second participant
	p2 := &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    "agent-coder",
		Role:           "observer",
	}
	require.NoError(t, s.AddParticipant(ctx, p2))

	// List participants
	participants, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	assert.Len(t, participants, 2)

	// Remove participant
	require.NoError(t, s.RemoveParticipant(ctx, conv.ID, "user", "user-alice"))

	// List should now return only 1
	participants, err = s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	assert.Len(t, participants, 1)
	assert.Equal(t, "agent-coder", participants[0].PrincipalID)
}

func TestParticipantAddDuplicate(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := newTestConversation()
	require.NoError(t, s.CreateConversation(ctx, conv))

	p := &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    "user-alice",
	}
	require.NoError(t, s.AddParticipant(ctx, p))

	// Duplicate add should fail
	p2 := &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    "user-alice",
	}
	err := s.AddParticipant(ctx, p2)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestParticipantRemoveNotFound(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := newTestConversation()
	require.NoError(t, s.CreateConversation(ctx, conv))

	err := s.RemoveParticipant(ctx, conv.ID, "user", "nobody")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// ---------------------------------------------------------------------------
// GetConversationsForPrincipal
// ---------------------------------------------------------------------------

func TestGetConversationsForPrincipal(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv1 := newTestConversation()
	conv2 := newTestConversation()
	conv3 := newTestConversation()
	require.NoError(t, s.CreateConversation(ctx, conv1))
	require.NoError(t, s.CreateConversation(ctx, conv2))
	require.NoError(t, s.CreateConversation(ctx, conv3))

	// Add alice to conv1 and conv2
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv1.ID,
		PrincipalKind:  "user",
		PrincipalID:    "alice",
	}))
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv2.ID,
		PrincipalKind:  "user",
		PrincipalID:    "alice",
	}))
	// Add alice to conv3 then remove
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv3.ID,
		PrincipalKind:  "user",
		PrincipalID:    "alice",
	}))
	require.NoError(t, s.RemoveParticipant(ctx, conv3.ID, "user", "alice"))

	// Alice should be in conv1 and conv2 only
	convs, err := s.GetConversationsForPrincipal(ctx, "user", "alice")
	require.NoError(t, err)
	assert.Len(t, convs, 2)

	ids := map[string]bool{}
	for _, c := range convs {
		ids[c.ID] = true
	}
	assert.True(t, ids[conv1.ID])
	assert.True(t, ids[conv2.ID])
	assert.False(t, ids[conv3.ID])
}

func TestGetConversationsForPrincipal_ExcludesSoftDeletedConversations(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := newTestConversation()
	require.NoError(t, s.CreateConversation(ctx, conv))

	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    "alice",
	}))

	// Soft-delete the conversation
	require.NoError(t, s.DeleteConversation(ctx, conv.ID))

	convs, err := s.GetConversationsForPrincipal(ctx, "user", "alice")
	require.NoError(t, err)
	assert.Empty(t, convs)
}

// ---------------------------------------------------------------------------
// Addressee operations
// ---------------------------------------------------------------------------

func TestAddresseeAddListUpdateDeliveryState(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	msgID := uuid.NewString()
	a := &store.MessageAddressee{
		MessageID:     msgID,
		PrincipalKind: "agent",
		PrincipalID:   "agent-coder",
		Via:           "explicit",
		DeliveryState: "pending",
	}
	require.NoError(t, s.AddAddressee(ctx, a))
	assert.NotEmpty(t, a.ID)

	// List
	addrs, err := s.ListAddressees(ctx, msgID)
	require.NoError(t, err)
	assert.Len(t, addrs, 1)
	assert.Equal(t, "pending", addrs[0].DeliveryState)
	assert.Equal(t, "explicit", addrs[0].Via)

	// Update delivery state
	require.NoError(t, s.UpdateDeliveryState(ctx, a.ID, "delivered", nil))
	addrs, err = s.ListAddressees(ctx, msgID)
	require.NoError(t, err)
	assert.Equal(t, "delivered", addrs[0].DeliveryState)

	// Update to failed with reason
	reason := "agent offline"
	require.NoError(t, s.UpdateDeliveryState(ctx, a.ID, "failed", &reason))
	addrs, err = s.ListAddressees(ctx, msgID)
	require.NoError(t, err)
	assert.Equal(t, "failed", addrs[0].DeliveryState)
	assert.Equal(t, &reason, addrs[0].FailureReason)
}

func TestAddresseeDuplicate(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	msgID := uuid.NewString()
	a := &store.MessageAddressee{
		MessageID:     msgID,
		PrincipalKind: "user",
		PrincipalID:   "user-alice",
		Via:           "direct",
		DeliveryState: "pending",
	}
	require.NoError(t, s.AddAddressee(ctx, a))

	// Duplicate should fail
	a2 := &store.MessageAddressee{
		MessageID:     msgID,
		PrincipalKind: "user",
		PrincipalID:   "user-alice",
		Via:           "direct",
		DeliveryState: "pending",
	}
	err := s.AddAddressee(ctx, a2)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestAddresseeUpdateDeliveryStateNotFound(t *testing.T) {
	s := newTestConversationStore(t)
	err := s.UpdateDeliveryState(context.Background(), uuid.NewString(), "delivered", nil)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAddresseeMultiplePerMessage(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	msgID := uuid.NewString()
	require.NoError(t, s.AddAddressee(ctx, &store.MessageAddressee{
		MessageID:     msgID,
		PrincipalKind: "user",
		PrincipalID:   "user-alice",
		Via:           "explicit",
		DeliveryState: "pending",
	}))
	require.NoError(t, s.AddAddressee(ctx, &store.MessageAddressee{
		MessageID:     msgID,
		PrincipalKind: "agent",
		PrincipalID:   "agent-coder",
		Via:           "default-agent",
		DeliveryState: "pending",
	}))

	addrs, err := s.ListAddressees(ctx, msgID)
	require.NoError(t, err)
	assert.Len(t, addrs, 2)
}

// ---------------------------------------------------------------------------
// Partial unique index: soft-deleted conversations allow reuse
// ---------------------------------------------------------------------------

func TestPartialUniqueIndex_SoftDeletedAllowsReuse(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	// Create conversation
	conv1 := &store.Conversation{
		ID:          uuid.NewString(),
		Kind:        "group",
		Surface:     "slack",
		ExternalRef: "REUSE-TEST",
	}
	require.NoError(t, s.CreateConversation(ctx, conv1))

	// Soft-delete it
	require.NoError(t, s.DeleteConversation(ctx, conv1.ID))

	// Creating a new conversation with the same (surface, external_ref) should succeed
	// because the index only covers non-deleted rows.
	conv2 := &store.Conversation{
		ID:          uuid.NewString(),
		Kind:        "group",
		Surface:     "slack",
		ExternalRef: "REUSE-TEST",
	}
	require.NoError(t, s.CreateConversation(ctx, conv2))

	// Verify conv2 is accessible
	got, err := s.GetConversation(ctx, conv2.ID)
	require.NoError(t, err)
	assert.Equal(t, conv2.ID, got.ID)
}

// ---------------------------------------------------------------------------
// Conversation with nil ProjectID (direct conversations)
// ---------------------------------------------------------------------------

func TestConversationNilProjectID(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := &store.Conversation{
		ID:      uuid.NewString(),
		Kind:    "direct",
		Surface: "native",
	}
	require.NoError(t, s.CreateConversation(ctx, conv))

	got, err := s.GetConversation(ctx, conv.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ProjectID)
}

// ---------------------------------------------------------------------------
// Default role for participants
// ---------------------------------------------------------------------------

func TestParticipantReJoinAfterRemove(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := newTestConversation()
	require.NoError(t, s.CreateConversation(ctx, conv))

	// 1. Add a participant.
	p := &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    "user-alice",
		Role:           "member",
	}
	require.NoError(t, s.AddParticipant(ctx, p))
	assert.NotEmpty(t, p.ID)

	// 2. Remove them (soft-remove sets left_at).
	require.NoError(t, s.RemoveParticipant(ctx, conv.ID, "user", "user-alice"))

	// Verify they are gone from active list.
	participants, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	assert.Len(t, participants, 0)

	// 3. Re-add them — should succeed, not ErrAlreadyExists.
	p2 := &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    "user-alice",
		Role:           "observer",
	}
	require.NoError(t, s.AddParticipant(ctx, p2))

	// 4. Verify the participant is active again with left_at = nil.
	participants, err = s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	require.Len(t, participants, 1)
	assert.Equal(t, "user-alice", participants[0].PrincipalID)
	assert.Equal(t, "observer", participants[0].Role)
	assert.Nil(t, participants[0].LeftAt)
}

func TestParticipantDefaultRole(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := newTestConversation()
	require.NoError(t, s.CreateConversation(ctx, conv))

	p := &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    "user-bob",
	}
	require.NoError(t, s.AddParticipant(ctx, p))

	participants, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	require.Len(t, participants, 1)
	assert.Equal(t, "member", participants[0].Role)
}

// ---------------------------------------------------------------------------
// AddParticipant immutability guard tests (DEF-8, WS-1c)
//
// Rule 10: Removing the guard from AddParticipant should make tests
// TestAddParticipant_DM_SoftRemoveThenSubstitute,
// TestAddParticipant_DM_ThirdPartyRejection, and
// TestAddParticipant_DM_EmptyExternalRefRejection fail.
// ---------------------------------------------------------------------------

// TestAddParticipant_DM_SoftRemoveThenSubstitute is THE test that discriminates
// between the correct key-derived guard and the wrong count-based guard.
// Sequence: create DM(A,B), soft-remove B, attempt AddParticipant(C) where C is
// NOT named in the key. A count>=2 guard would pass (count is 1 after remove),
// but the key-derived guard correctly rejects C.
func TestAddParticipant_DM_SoftRemoveThenSubstitute(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	userA := uuid.NewString()
	userB := uuid.NewString()
	userC := uuid.NewString() // intruder — NOT named in key

	conv := newTestDMConversation("user", userA, "user", userB)
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Add both named participants.
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userA, Role: "member",
	}))
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	}))

	// Soft-remove B.
	require.NoError(t, s.RemoveParticipant(ctx, conv.ID, "user", userB))

	// Attempt to add C — must be rejected by key-derived guard.
	err := s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userC, Role: "member",
	})
	require.Error(t, err, "adding participant not named in DM key must fail")
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	// Rule 13: Assert effects — active participant set is exactly {A}, not {A, C}.
	participants, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	assert.Len(t, participants, 1, "active participant set must be exactly {A}")
	if len(participants) == 1 {
		assert.Equal(t, userA, participants[0].PrincipalID)
	}
}

// TestAddParticipant_DM_ThirdPartyRejection verifies that a third party cannot
// be added to a direct conversation that already has 2 active participants.
func TestAddParticipant_DM_ThirdPartyRejection(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	userA := uuid.NewString()
	userB := uuid.NewString()
	userC := uuid.NewString()

	conv := newTestDMConversation("user", userA, "user", userB)
	require.NoError(t, s.CreateConversation(ctx, conv))

	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userA, Role: "member",
	}))
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	}))

	// Add C — not named in key.
	err := s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userC, Role: "member",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	// Rule 13: Assert participant count is still 2.
	participants, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	assert.Len(t, participants, 2, "participant count must still be 2")
}

// TestAddParticipant_DM_EmptyExternalRefRejection verifies that a direct
// conversation with an empty/unparseable external_ref rejects all AddParticipant
// calls.
func TestAddParticipant_DM_EmptyExternalRefRejection(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := &store.Conversation{
		ID:          uuid.NewString(),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "", // unparseable
	}
	require.NoError(t, s.CreateConversation(ctx, conv))

	err := s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: uuid.NewString(), Role: "member",
	})
	require.Error(t, err, "direct conversation with empty external_ref must reject AddParticipant")
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

// TestAddParticipant_DM_NamedParticipantAllowed verifies that a principal named
// in the DM key can be added successfully.
func TestAddParticipant_DM_NamedParticipantAllowed(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	userA := uuid.NewString()
	agentB := uuid.NewString()

	conv := newTestDMConversation("user", userA, "agent", agentB)
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Add userA — named in key.
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userA, Role: "member",
	}))

	// Add agentB — also named in key.
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "agent", PrincipalID: agentB, Role: "member",
	}))

	// Rule 13: Assert effects.
	participants, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	assert.Len(t, participants, 2)
}

// TestAddParticipant_DM_ReAddAfterSoftRemove verifies that a principal named
// in the DM key can be re-added after soft-remove. This proves the guard does
// not block legitimate re-adds.
func TestAddParticipant_DM_ReAddAfterSoftRemove(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	userA := uuid.NewString()
	userB := uuid.NewString()

	conv := newTestDMConversation("user", userA, "user", userB)
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Add both.
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userA, Role: "member",
	}))
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	}))

	// Soft-remove B.
	require.NoError(t, s.RemoveParticipant(ctx, conv.ID, "user", userB))

	// Re-add B — named in key, should succeed.
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	}))

	// Rule 13: Assert effects — both participants active.
	participants, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	assert.Len(t, participants, 2, "both named participants should be active after re-add")

	// Rule 14: Assert non-zero floor — at least 2 participants examined.
	require.GreaterOrEqual(t, len(participants), 2,
		"floor violation: expected at least 2 participants")
}
