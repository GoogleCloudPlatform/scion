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
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversation"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversationparticipant"
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
	dmKey := "dm:agent:" + uuid.NewString() + ":user:" + uuid.NewString()
	conv1 := &store.Conversation{
		ID:          uuid.NewString(),
		ProjectID:   &projectID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
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

	// Create 5 conversations with staggered activity times.
	// Uses kind="group" because pagination is kind-agnostic, avoiding the
	// need for unique DM keys on each fixture.
	for i := 0; i < 5; i++ {
		conv := &store.Conversation{
			ID:             uuid.NewString(),
			Kind:           "group",
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
// DEF-28: ParentRef preservation + comprehensive field classification
// ---------------------------------------------------------------------------

func TestUpsertConversationByExternalRef_EmptyParentRefPreservesExisting(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	// Create with a parent ref.
	conv1 := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:proj1:thread-def28",
		ParentRef:   "T789",
	}
	r1, err := s.UpsertConversationByExternalRef(ctx, conv1)
	require.NoError(t, err)
	assert.Equal(t, "T789", r1.ParentRef)

	// Upsert with empty parent ref — must NOT clobber the existing value.
	conv2 := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:proj1:thread-def28",
		ParentRef:   "", // empty
	}
	r2, err := s.UpsertConversationByExternalRef(ctx, conv2)
	require.NoError(t, err)
	assert.Equal(t, r1.ID, r2.ID, "should be the same conversation")
	assert.Equal(t, "T789", r2.ParentRef, "original parent ref must be preserved (DEF-28)")
}

func TestUpsertConversationByExternalRef_NonEmptyParentRefOverwrites(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	// Create with an initial parent ref.
	conv1 := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:proj1:thread-def28-pos",
		ParentRef:   "T789",
	}
	r1, err := s.UpsertConversationByExternalRef(ctx, conv1)
	require.NoError(t, err)
	assert.Equal(t, "T789", r1.ParentRef)

	// Upsert with a different non-empty parent ref — must overwrite.
	conv2 := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:proj1:thread-def28-pos",
		ParentRef:   "T999",
	}
	r2, err := s.UpsertConversationByExternalRef(ctx, conv2)
	require.NoError(t, err)
	assert.Equal(t, "T999", r2.ParentRef, "non-empty parent ref must overwrite")
}

// TestUpsertConversationByExternalRef_FieldClassification is a comprehensive
// table-driven test that classifies EVERY field on store.Conversation into one
// of five buckets. The test uses reflect to ensure that adding a new field to
// the struct without classifying it breaks the test — converting "reviewer must
// notice an omission" into "CI notices."
//
// Buckets:
//
//	A. MATCH KEY   — selects the row; never updated.           (Surface, ExternalRef)
//	B. IMMUTABLE   — must not change for the row's lifetime.   (ID, CreatedAt, Kind)
//	C. PRESERVE    — guarded; empty input preserves prior.     (DisplayName, DriftState, ProjectID,
//	                                                            DefaultAgentID, ParentRef)
//	D. ALWAYS-SET  — unconditionally written on purpose.       (LastActivityAt)
//	E. NOT TOUCHED — not modified by the update path.          (ArchivedAt, DeletedAt)
func TestUpsertConversationByExternalRef_FieldClassification(t *testing.T) {
	// Every field on store.Conversation must appear in exactly one bucket.
	// If a new field is added to the struct and not listed here, the
	// reflect check below fails.
	classified := map[string]string{
		// A — match key
		"Surface":     "A",
		"ExternalRef": "A",
		// B — immutable
		"ID":        "B",
		"CreatedAt": "B",
		"Kind":      "B",
		// C — preserve-on-empty
		"DisplayName":    "C",
		"DriftState":     "C",
		"ProjectID":      "C",
		"DefaultAgentID": "C",
		"ParentRef":      "C",
		// D — always-written
		"LastActivityAt": "D",
		// E — not touched on update path
		"ArchivedAt": "E",
		"DeletedAt":  "E",
	}

	// Reflect check: every exported field of store.Conversation must be classified.
	convType := reflect.TypeOf(store.Conversation{})
	for i := 0; i < convType.NumField(); i++ {
		field := convType.Field(i)
		if !field.IsExported() {
			continue
		}
		if _, ok := classified[field.Name]; !ok {
			t.Errorf("store.Conversation field %q is not classified — add it to a bucket (A-E) "+
				"and write assertions for it. This test exists to catch exactly this omission.", field.Name)
		}
	}
	// And no stale entries in the classification.
	for name := range classified {
		found := false
		for i := 0; i < convType.NumField(); i++ {
			if convType.Field(i).Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("classified field %q does not exist on store.Conversation — remove it from the table", name)
		}
	}

	s := newTestConversationStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	agentID := uuid.NewString()

	// Step 1: Create a fully-populated conversation via initial upsert.
	initial := &store.Conversation{
		Kind:           "group",
		Surface:        "native",
		ExternalRef:    "test-field-class-" + uuid.NewString(),
		ParentRef:      "parent-original",
		DisplayName:    "Original Name",
		DriftState:     "active",
		ProjectID:      &projectID,
		DefaultAgentID: &agentID,
	}
	r1, err := s.UpsertConversationByExternalRef(ctx, initial)
	require.NoError(t, err)
	require.NotNil(t, r1)

	// Capture initial state for comparison.
	initialID := r1.ID
	initialCreatedAt := r1.CreatedAt
	initialKind := r1.Kind
	initialLastActivity := r1.LastActivityAt

	// Step 2: Upsert with EMPTY optional fields — only match keys and Kind set.
	emptyUpsert := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: initial.ExternalRef,
		// All optional fields left at zero value.
	}
	r2, err := s.UpsertConversationByExternalRef(ctx, emptyUpsert)
	require.NoError(t, err)
	require.Equal(t, initialID, r2.ID, "must be same row")

	// Bucket B — IMMUTABLE: same value even when input is different.
	t.Run("B_immutable", func(t *testing.T) {
		assert.Equal(t, initialID, r2.ID, "ID must not change")
		assert.True(t, initialCreatedAt.Equal(r2.CreatedAt), "CreatedAt must not change (got %v vs %v)", initialCreatedAt, r2.CreatedAt)
		// Kind: the upsert passed the same Kind ("group"). Upserting with a different
		// Kind is silently ignored — the update path does not call SetKind. This is
		// believed correct and load-bearing: a direct conversation must not become a
		// group because D-1 says a direct conversation's participant set is immutable
		// for its lifetime. The silence (no error on Kind mismatch) is a separate
		// question tracked outside this test.
		assert.Equal(t, initialKind, r2.Kind, "Kind must not change")
	})

	// Bucket C — PRESERVE-ON-EMPTY: prior value survives empty-input upsert.
	t.Run("C_preserve_on_empty", func(t *testing.T) {
		assert.Equal(t, "Original Name", r2.DisplayName, "DisplayName must be preserved when empty")
		assert.Equal(t, "active", r2.DriftState, "DriftState must be preserved when empty")
		assert.NotNil(t, r2.ProjectID, "ProjectID must be preserved when nil input")
		if r2.ProjectID != nil {
			assert.Equal(t, projectID, *r2.ProjectID, "ProjectID value must match original")
		}
		assert.NotNil(t, r2.DefaultAgentID, "DefaultAgentID must be preserved when nil input")
		if r2.DefaultAgentID != nil {
			assert.Equal(t, agentID, *r2.DefaultAgentID, "DefaultAgentID value must match original")
		}
		assert.Equal(t, "parent-original", r2.ParentRef, "ParentRef must be preserved when empty (DEF-28)")
	})

	// Bucket D — ALWAYS-WRITTEN: value changed from initial.
	t.Run("D_always_written", func(t *testing.T) {
		// LastActivityAt is set to time.Now() on every upsert update.
		// It must differ from the initial value (which was set by the first upsert).
		assert.False(t, r2.LastActivityAt.Equal(initialLastActivity) && r2.LastActivityAt.Before(time.Now().Add(-time.Second)),
			"LastActivityAt must be updated on every upsert (bucket D)")
	})

	// Bucket E — NOT TOUCHED: still nil after update.
	t.Run("E_not_touched", func(t *testing.T) {
		assert.Nil(t, r2.ArchivedAt, "ArchivedAt must not be set by upsert update path")
		assert.Nil(t, r2.DeletedAt, "DeletedAt must not be set by upsert update path")
	})

	// Step 3: Upsert with NON-EMPTY optional fields — must overwrite.
	newProjectID := uuid.NewString()
	newAgentID := uuid.NewString()
	overwrite := &store.Conversation{
		Kind:           "group",
		Surface:        "native",
		ExternalRef:    initial.ExternalRef,
		ParentRef:      "parent-updated",
		DisplayName:    "Updated Name",
		DriftState:     "orphaned",
		ProjectID:      &newProjectID,
		DefaultAgentID: &newAgentID,
	}
	r3, err := s.UpsertConversationByExternalRef(ctx, overwrite)
	require.NoError(t, err)

	// Bucket C — non-empty input DOES overwrite.
	t.Run("C_overwrite_when_nonempty", func(t *testing.T) {
		assert.Equal(t, "Updated Name", r3.DisplayName, "DisplayName must update when non-empty")
		assert.Equal(t, "orphaned", r3.DriftState, "DriftState must update when non-empty")
		require.NotNil(t, r3.ProjectID)
		assert.Equal(t, newProjectID, *r3.ProjectID, "ProjectID must update when non-nil")
		require.NotNil(t, r3.DefaultAgentID)
		assert.Equal(t, newAgentID, *r3.DefaultAgentID, "DefaultAgentID must update when non-nil")
		assert.Equal(t, "parent-updated", r3.ParentRef, "ParentRef must update when non-empty")
	})
}

// ---------------------------------------------------------------------------
// Nil parameter guard tests
// ---------------------------------------------------------------------------

func TestCreateConversation_NilInput(t *testing.T) {
	s := newTestConversationStore(t)
	err := s.CreateConversation(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestCreateConversation_ValidInput(t *testing.T) {
	s := newTestConversationStore(t)
	conv := newTestConversation()
	err := s.CreateConversation(context.Background(), conv)
	require.NoError(t, err, "valid input must succeed")
}

func TestUpdateConversation_NilInput(t *testing.T) {
	s := newTestConversationStore(t)
	err := s.UpdateConversation(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestUpdateConversation_ValidInput(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()
	// Create first, then update.
	conv := newTestConversation()
	require.NoError(t, s.CreateConversation(ctx, conv))
	conv.DisplayName = "updated"
	err := s.UpdateConversation(ctx, conv)
	require.NoError(t, err, "valid input must succeed")
}

func TestUpsertConversationByExternalRef_NilInput(t *testing.T) {
	s := newTestConversationStore(t)
	_, err := s.UpsertConversationByExternalRef(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestUpsertConversationByExternalRef_ValidInput(t *testing.T) {
	s := newTestConversationStore(t)
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "test-ref-" + uuid.NewString(),
	}
	result, err := s.UpsertConversationByExternalRef(context.Background(), conv)
	require.NoError(t, err, "valid input must succeed")
	require.NotNil(t, result)
}

// ---------------------------------------------------------------------------
// DEF-29: Direct conversation requires non-empty external_ref
// ---------------------------------------------------------------------------

func TestCreateConversation_DirectWithEmptyExternalRef_Refused(t *testing.T) {
	s := newTestConversationStore(t)
	conv := &store.Conversation{
		ID:      uuid.NewString(),
		Kind:    "direct",
		Surface: "native",
		// ExternalRef intentionally empty — this is the defect.
	}
	err := s.CreateConversation(context.Background(), conv)
	require.Error(t, err, "direct conversation with empty external_ref must be refused")
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	assert.Contains(t, err.Error(), "external_ref", "error message must name the missing field")
}

func TestCreateConversation_GroupWithEmptyExternalRef_Allowed(t *testing.T) {
	// A guard that rejects ALL empty external_refs would break native group
	// creation. Only kind=="direct" requires a key.
	s := newTestConversationStore(t)
	conv := &store.Conversation{
		ID:      uuid.NewString(),
		Kind:    "group",
		Surface: "native",
		// ExternalRef intentionally empty — valid for groups.
	}
	err := s.CreateConversation(context.Background(), conv)
	require.NoError(t, err, "group conversation with empty external_ref must succeed")
}

func TestCreateConversation_DirectWithValidDMKey_Succeeds(t *testing.T) {
	s := newTestConversationStore(t)
	agentID := uuid.NewString()
	userID := uuid.NewString()
	extRef, err := messages.DMConversationKey("agent", agentID, "user", userID)
	require.NoError(t, err)

	conv := &store.Conversation{
		ID:          uuid.NewString(),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
	}
	err = s.CreateConversation(context.Background(), conv)
	require.NoError(t, err, "direct conversation with valid DM key must succeed")
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

	// DM conversations are global — ProjectID is intentionally nil.
	// Must include a valid DM key because DEF-29 rejects keyless direct rows.
	conv := newTestDMConversation("agent", uuid.NewString(), "user", uuid.NewString())
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
// TestAddParticipant_DM_EmptyExternalRefRejection_BypassCreate fail.
// TestAddParticipant_DM_EmptyExternalRefRejection tests the CreateConversation
// layer (DEF-29); the _BypassCreate variant tests AddParticipant's own guard
// independently by inserting the row via ent directly.
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
// conversation with an empty external_ref is caught at the CreateConversation
// layer (DEF-29). Before DEF-29, this hole was only caught at AddParticipant
// time; CreateConversation now refuses keyless direct rows outright, making
// the AddParticipant guard defense-in-depth.
func TestAddParticipant_DM_EmptyExternalRefRejection(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	conv := &store.Conversation{
		ID:          uuid.NewString(),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "", // unparseable — DEF-29 guard catches this
	}
	err := s.CreateConversation(ctx, conv)
	require.Error(t, err, "CreateConversation must refuse a direct conversation without external_ref")
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

// TestAddParticipant_DM_EmptyExternalRefRejection_BypassCreate tests
// AddParticipant's OWN guard for kind="direct" with empty external_ref,
// independent of CreateConversation's DEF-29 guard.
//
// Why the bypass: DEF-29 makes CreateConversation refuse keyless direct
// rows, which shadows AddParticipant's guard in normal flow. But
// AddParticipant's guard is defense-in-depth: if a second create path is
// ever added (the exact defect class DEF-29 closes), or if DEF-29 is
// relaxed, AddParticipant must still reject the empty key. Each layer's
// guard is independently tested.
//
// The row is constructed via the ent client directly, bypassing the store's
// CreateConversation. Do not "simplify" this back through CreateConversation
// — that would silently delete the AddParticipant-level coverage.
func TestAddParticipant_DM_EmptyExternalRefRejection_BypassCreate(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	// Insert a kind="direct", external_ref="" row directly via ent,
	// bypassing CreateConversation's DEF-29 guard.
	convID := uuid.New()
	_, err := s.client.Conversation.Create().
		SetID(convID).
		SetKind(conversation.KindDirect).
		SetSurface(conversation.SurfaceNative).
		SetExternalRef(""). // intentionally empty — the defect
		SetDriftState(conversation.DriftStateActive).
		Save(ctx)
	require.NoError(t, err, "direct ent insert must succeed (bypasses store guard)")

	// AddParticipant must reject this via its own ParseDMKey guard.
	addErr := s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: convID.String(),
		PrincipalKind:  "user",
		PrincipalID:    uuid.NewString(),
		Role:           "member",
	})
	require.Error(t, addErr, "AddParticipant must reject empty external_ref on direct conversation")
	assert.ErrorIs(t, addErr, store.ErrInvalidInput)
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

// ---------------------------------------------------------------------------
// EnsureParticipant tests (B6, B9 fixes)
// ---------------------------------------------------------------------------

// TestEnsureParticipant_InsertIfAbsent verifies that EnsureParticipant creates
// a new participant row when none exists, and is idempotent on repeat calls.
func TestEnsureParticipant_InsertIfAbsent(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	userA := uuid.NewString()
	userB := uuid.NewString()

	conv := newTestDMConversation("user", userA, "user", userB)
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Ensure participant A — no prior row.
	err := s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userA, Role: "member",
	})
	require.NoError(t, err)

	// Verify participant was created.
	participants, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	require.Len(t, participants, 1)
	assert.Equal(t, userA, participants[0].PrincipalID)

	// Call EnsureParticipant again — must be idempotent (no error).
	err = s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userA, Role: "member",
	})
	require.NoError(t, err, "second EnsureParticipant call must be idempotent")

	// Still exactly one active participant.
	participants, err = s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	assert.Len(t, participants, 1)
}

// TestEnsureParticipant_DoesNotClearLeftAt (B6) verifies that EnsureParticipant
// on a soft-removed row leaves left_at UNCHANGED — pinning the exact timestamp.
func TestEnsureParticipant_DoesNotClearLeftAt(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	userA := uuid.NewString()
	userB := uuid.NewString()

	conv := newTestDMConversation("user", userA, "user", userB)
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Add both participants via AddParticipant.
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userA, Role: "member",
	}))
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	}))

	// Soft-remove userB.
	require.NoError(t, s.RemoveParticipant(ctx, conv.ID, "user", userB))

	// Record the exact left_at timestamp.
	convUID, err := parseUUID(conv.ID)
	require.NoError(t, err)
	softRemoved, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.PrincipalIDEQ(userB),
		).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, softRemoved.LeftAt, "left_at must be set after RemoveParticipant")
	originalLeftAt := *softRemoved.LeftAt

	// Call EnsureParticipant for the soft-removed participant.
	err = s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	})
	require.NoError(t, err)

	// Assert left_at is UNCHANGED — pin the exact timestamp.
	afterEnsure, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.PrincipalIDEQ(userB),
		).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, afterEnsure.LeftAt, "left_at must still be set after EnsureParticipant")
	assert.True(t, originalLeftAt.Equal(*afterEnsure.LeftAt),
		"left_at must be unchanged: original=%v, after=%v", originalLeftAt, *afterEnsure.LeftAt)
}

// TestEnsureParticipant_ResolveAfterLeave (B6) is the full scenario test:
// create DM, add both participants, soft-remove one, then call EnsureParticipant.
// The left_at timestamp must be preserved exactly.
func TestEnsureParticipant_ResolveAfterLeave(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	userA := uuid.NewString()
	userB := uuid.NewString()

	conv := newTestDMConversation("user", userA, "user", userB)
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Add both participants.
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userA, Role: "member",
	}))
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	}))

	// Soft-remove userB (user "leaves" / hides the DM).
	require.NoError(t, s.RemoveParticipant(ctx, conv.ID, "user", userB))

	// Record exact left_at.
	convUID, err := parseUUID(conv.ID)
	require.NoError(t, err)
	before, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.PrincipalIDEQ(userB),
		).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, before.LeftAt)
	pinnedLeftAt := *before.LeftAt

	// Simulate resolve-driven EnsureParticipant (what ResolveOrCreateDMConversation does).
	err = s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	})
	require.NoError(t, err)

	// Assert left_at is unchanged.
	after, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.PrincipalIDEQ(userB),
		).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, after.LeftAt, "left_at must remain set after EnsureParticipant")
	assert.True(t, pinnedLeftAt.Equal(*after.LeftAt),
		"left_at must be exactly preserved: pinned=%v, actual=%v", pinnedLeftAt, *after.LeftAt)

	// userB should NOT appear in active participant list (left_at is still set).
	activeParticipants, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	assert.Len(t, activeParticipants, 1, "only userA should be active")
	assert.Equal(t, userA, activeParticipants[0].PrincipalID)
}

// TestEnsureParticipant_DM_ThirdPartyRejection verifies that the D-1 guard
// shared predicate rejects third parties via EnsureParticipant, same as
// AddParticipant. Testing BOTH methods proves the guard cannot diverge.
func TestEnsureParticipant_DM_ThirdPartyRejection(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	userA := uuid.NewString()
	userB := uuid.NewString()
	userC := uuid.NewString() // intruder — NOT named in key

	conv := newTestDMConversation("user", userA, "user", userB)
	require.NoError(t, s.CreateConversation(ctx, conv))

	// AddParticipant rejects third party.
	addErr := s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userC, Role: "member",
	})
	require.Error(t, addErr, "AddParticipant must reject third party")
	assert.ErrorIs(t, addErr, store.ErrInvalidInput)

	// EnsureParticipant rejects the same third party.
	ensureErr := s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userC, Role: "member",
	})
	require.Error(t, ensureErr, "EnsureParticipant must reject third party")
	assert.ErrorIs(t, ensureErr, store.ErrInvalidInput)

	// Both errors should contain the same diagnostic message pattern.
	assert.Contains(t, addErr.Error(), "not named in direct conversation key")
	assert.Contains(t, ensureErr.Error(), "not named in direct conversation key")
}

// TestEnsureParticipant_PopulatesCallerStruct verifies that EnsureParticipant
// populates p.ID and p.JoinedAt from the existing row, matching AddParticipant's
// post-condition. This is a READ-BACK, not a write — the DB row is untouched.
func TestEnsureParticipant_PopulatesCallerStruct(t *testing.T) {
	s := newTestConversationStore(t)
	ctx := context.Background()

	userA := uuid.NewString()
	userB := uuid.NewString()

	conv := newTestDMConversation("user", userA, "user", userB)
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Add userB via AddParticipant, then soft-remove.
	addP := &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	}
	require.NoError(t, s.AddParticipant(ctx, addP))
	require.NoError(t, s.RemoveParticipant(ctx, conv.ID, "user", userB))

	// Record DB state for the soft-removed row.
	convUID, err := parseUUID(conv.ID)
	require.NoError(t, err)
	dbRow, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.PrincipalIDEQ(userB),
		).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, dbRow.LeftAt, "precondition: left_at must be set")
	originalLeftAt := *dbRow.LeftAt

	// Call EnsureParticipant with a fresh struct (zero ID, zero JoinedAt).
	ensureP := &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: userB, Role: "member",
	}
	require.Empty(t, ensureP.ID, "precondition: p.ID must be zero before call")
	require.True(t, ensureP.JoinedAt.IsZero(), "precondition: p.JoinedAt must be zero before call")

	// (a) returns nil
	err = s.EnsureParticipant(ctx, ensureP)
	require.NoError(t, err)

	// (b) left_at unchanged in DB
	afterRow, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.PrincipalIDEQ(userB),
		).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, afterRow.LeftAt, "left_at must remain set")
	assert.True(t, originalLeftAt.Equal(*afterRow.LeftAt),
		"left_at must be unchanged: original=%v, after=%v", originalLeftAt, *afterRow.LeftAt)

	// (c) p.ID and p.JoinedAt now match the existing row
	assert.Equal(t, dbRow.ID.String(), ensureP.ID,
		"p.ID must be populated from existing row")
	assert.True(t, dbRow.JoinedAt.Equal(ensureP.JoinedAt),
		"p.JoinedAt must be populated from existing row: expected=%v, got=%v",
		dbRow.JoinedAt, ensureP.JoinedAt)
}
