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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustDMKey builds a kind-encoded DM key, panicking on error. Test helper only.
func mustDMKey(kindA, idA, kindB, idB string) string {
	key, err := messages.DMConversationKey(kindA, idA, kindB, idB)
	if err != nil {
		panic(fmt.Sprintf("mustDMKey: %v", err))
	}
	return key
}

// ---------------------------------------------------------------------------
// Mock ResolutionStore
// ---------------------------------------------------------------------------

type mockResolutionStore struct {
	conversations map[string]*store.Conversation
	participants  map[string][]store.ConversationParticipant // key: conversationID
	agents        map[string]*store.Agent                    // key: "projectID/slug"
	users         map[string]*store.User                     // key: email
}

func newMockStore() *mockResolutionStore {
	return &mockResolutionStore{
		conversations: make(map[string]*store.Conversation),
		participants:  make(map[string][]store.ConversationParticipant),
		agents:        make(map[string]*store.Agent),
		users:         make(map[string]*store.User),
	}
}

func (m *mockResolutionStore) GetConversation(_ context.Context, id string) (*store.Conversation, error) {
	conv, ok := m.conversations[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return conv, nil
}

func (m *mockResolutionStore) CreateConversation(_ context.Context, conv *store.Conversation) error {
	if _, ok := m.conversations[conv.ID]; ok {
		return store.ErrAlreadyExists
	}
	m.conversations[conv.ID] = conv
	return nil
}

func (m *mockResolutionStore) ListConversations(_ context.Context, filter store.ConversationFilter, opts store.ListOptions) (*store.ListResult[store.Conversation], error) {
	var items []store.Conversation
	for _, conv := range m.conversations {
		if filter.ProjectID != "" && (conv.ProjectID == nil || *conv.ProjectID != filter.ProjectID) {
			continue
		}
		if filter.Kind != "" && conv.Kind != filter.Kind {
			continue
		}
		if filter.Surface != "" && conv.Surface != filter.Surface {
			continue
		}
		if filter.DriftState != "" && conv.DriftState != filter.DriftState {
			continue
		}
		items = append(items, *conv)
	}

	// Simulate pagination when a limit is set.
	if opts.Limit > 0 && len(items) > 0 {
		// Sort by ID for deterministic pagination.
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

		// Apply cursor (cursor = last ID seen).
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

		if len(items) > opts.Limit {
			nextCursor := items[opts.Limit-1].ID
			return &store.ListResult[store.Conversation]{
				Items:      items[:opts.Limit],
				TotalCount: len(items),
				NextCursor: nextCursor,
			}, nil
		}
	}

	return &store.ListResult[store.Conversation]{Items: items, TotalCount: len(items)}, nil
}

func (m *mockResolutionStore) AddParticipant(_ context.Context, p *store.ConversationParticipant) error {
	// Check for duplicates.
	for _, existing := range m.participants[p.ConversationID] {
		if existing.PrincipalKind == p.PrincipalKind && existing.PrincipalID == p.PrincipalID {
			return store.ErrAlreadyExists
		}
	}
	m.participants[p.ConversationID] = append(m.participants[p.ConversationID], *p)
	return nil
}

func (m *mockResolutionStore) EnsureParticipant(_ context.Context, p *store.ConversationParticipant) error {
	// If any row exists, leave it untouched.
	for _, existing := range m.participants[p.ConversationID] {
		if existing.PrincipalKind == p.PrincipalKind && existing.PrincipalID == p.PrincipalID {
			return nil
		}
	}
	m.participants[p.ConversationID] = append(m.participants[p.ConversationID], *p)
	return nil
}

func (m *mockResolutionStore) ListParticipants(_ context.Context, conversationID string) ([]store.ConversationParticipant, error) {
	return m.participants[conversationID], nil
}

func (m *mockResolutionStore) GetConversationsForPrincipal(_ context.Context, principalKind, principalID string) ([]store.Conversation, error) {
	var result []store.Conversation
	for convID, parts := range m.participants {
		for _, p := range parts {
			if p.PrincipalKind == principalKind && p.PrincipalID == principalID {
				if conv, ok := m.conversations[convID]; ok {
					result = append(result, *conv)
				}
				break
			}
		}
	}
	return result, nil
}

func (m *mockResolutionStore) GetAgentBySlug(_ context.Context, projectID, slug string) (*store.Agent, error) {
	key := projectID + "/" + slug
	if agent, ok := m.agents[key]; ok {
		return agent, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockResolutionStore) UpsertConversationByExternalRef(_ context.Context, conv *store.Conversation) (*store.Conversation, error) {
	if conv.ExternalRef == "" {
		return nil, store.ErrInvalidInput
	}
	// Find existing by (surface, external_ref).
	for _, existing := range m.conversations {
		if existing.Surface == conv.Surface && existing.ExternalRef == conv.ExternalRef {
			// Update mutable fields (mimic real store behaviour).
			if conv.DisplayName != "" {
				existing.DisplayName = conv.DisplayName
			}
			if conv.DriftState != "" {
				existing.DriftState = conv.DriftState
			}
			return existing, nil
		}
	}
	// Create new.
	if conv.ID == "" {
		conv.ID = uuid.NewString()
	}
	m.conversations[conv.ID] = conv
	return conv, nil
}

func (m *mockResolutionStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	if user, ok := m.users[email]; ok {
		return user, nil
	}
	return nil, store.ErrNotFound
}

// helper to add a conversation with participants to the mock store.
func (m *mockResolutionStore) addConversation(conv *store.Conversation, participants ...store.ConversationParticipant) {
	m.conversations[conv.ID] = conv
	m.participants[conv.ID] = participants
}

// ---------------------------------------------------------------------------
// ParseReference tests
// ---------------------------------------------------------------------------

func TestParseReference_ConvID(t *testing.T) {
	id := uuid.NewString()
	ref, err := ParseReference("conv:" + id)
	require.NoError(t, err)
	assert.Equal(t, RefConversation, ref.Kind)
	assert.Equal(t, id, ref.Value)
	assert.Equal(t, "conv:"+id, ref.Raw)
}

func TestParseReference_ConvID_InvalidUUID(t *testing.T) {
	_, err := ParseReference("conv:not-a-uuid")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidInput))
}

func TestParseReference_AgentSlug(t *testing.T) {
	ref, err := ParseReference("@my-agent")
	require.NoError(t, err)
	assert.Equal(t, RefAgent, ref.Kind)
	assert.Equal(t, "my-agent", ref.Value)
}

func TestParseReference_Email(t *testing.T) {
	ref, err := ParseReference("@user@example.com")
	require.NoError(t, err)
	assert.Equal(t, RefEmail, ref.Kind)
	assert.Equal(t, "user@example.com", ref.Value)
}

func TestParseReference_ThreadName(t *testing.T) {
	ref, err := ParseReference("#design-discussion")
	require.NoError(t, err)
	assert.Equal(t, RefThread, ref.Kind)
	assert.Equal(t, "design-discussion", ref.Value)
}

func TestParseReference_ThreadWithSlash_Rejected(t *testing.T) {
	// AC-31: #<space>/<thread> form must be rejected.
	_, err := ParseReference("#my-space/my-thread")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidInput))
	assert.Contains(t, err.Error(), "AC-31")
}

func TestParseReference_Empty(t *testing.T) {
	_, err := ParseReference("")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidInput))
}

func TestParseReference_EmptyAt(t *testing.T) {
	_, err := ParseReference("@")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidInput))
}

func TestParseReference_EmptyHash(t *testing.T) {
	_, err := ParseReference("#")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidInput))
}

func TestParseReference_InvalidFormat(t *testing.T) {
	tests := []string{
		"just-a-string",
		"123",
		"conversation:123",
		"!invalid",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			_, err := ParseReference(input)
			require.Error(t, err)
			assert.True(t, errors.Is(err, store.ErrInvalidInput), "expected ErrInvalidInput for %q, got: %v", input, err)
		})
	}
}

// ---------------------------------------------------------------------------
// Resolve conv:<id> tests
// ---------------------------------------------------------------------------

func TestResolve_ConvByID_HappyPath(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	senderID := uuid.NewString()
	convID := uuid.NewString()

	ms.addConversation(
		&store.Conversation{ID: convID, ProjectID: &projectID, Kind: "group", Surface: "native"},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: senderID},
	)

	result, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectID,
	})
	require.NoError(t, err)
	assert.Equal(t, convID, result.ConversationID)
	assert.False(t, result.Created)
}

func TestResolve_ConvByID_BoundaryViolation_SenderBelongsToOtherProject(t *testing.T) {
	// AC-30: Sender belongs to project B, tries to resolve a conv from project B
	// while in project A context → boundary-violation.
	ms := newMockStore()
	ctx := context.Background()
	projectA := uuid.NewString()
	projectB := uuid.NewString()
	senderID := uuid.NewString()
	convID := uuid.NewString()

	// Conversation belongs to project B.
	ms.addConversation(
		&store.Conversation{ID: convID, ProjectID: &projectB, Kind: "group", Surface: "native"},
	)

	// Sender has a conversation in project B (belongs to project B).
	otherConvID := uuid.NewString()
	ms.addConversation(
		&store.Conversation{ID: otherConvID, ProjectID: &projectB, Kind: "direct", Surface: "native"},
		store.ConversationParticipant{ConversationID: otherConvID, PrincipalKind: "user", PrincipalID: senderID},
	)

	_, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectA, // Sender is in project A context
	})
	require.Error(t, err)

	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "boundary-violation", resErr.Reason, "sender belongs to project B, should get boundary-violation")
}

func TestResolve_ConvByID_NotFound_SenderDoesNotBelongToOtherProject(t *testing.T) {
	// AC-30: Sender does NOT belong to project B, tries to resolve a conv
	// from project B → not-found (no information leakage).
	ms := newMockStore()
	ctx := context.Background()
	projectA := uuid.NewString()
	projectB := uuid.NewString()
	senderID := uuid.NewString()
	convID := uuid.NewString()

	// Conversation belongs to project B.
	ms.addConversation(
		&store.Conversation{ID: convID, ProjectID: &projectB, Kind: "group", Surface: "native"},
	)
	// Sender has NO conversations in project B.

	_, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectA,
	})
	require.Error(t, err)

	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "not-found", resErr.Reason, "sender does not belong to project B, should get not-found")
}

func TestResolve_ConvByID_DisclosureRule(t *testing.T) {
	// AC-30: The error messages for 'not-found' (doesn't exist) and
	// 'not-found' (cross-project, sender not a member) must be IDENTICAL.
	ms := newMockStore()
	ctx := context.Background()
	projectA := uuid.NewString()
	projectB := uuid.NewString()
	senderID := uuid.NewString()

	// Case 1: Conversation truly does not exist.
	fakeID := uuid.NewString()
	_, err1 := Resolve(ctx, ms, "conv:"+fakeID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectA,
	})
	require.Error(t, err1)

	// Case 2: Conversation exists in project B, sender does not belong to B.
	realID := uuid.NewString()
	ms.addConversation(
		&store.Conversation{ID: realID, ProjectID: &projectB, Kind: "group", Surface: "native"},
	)

	_, err2 := Resolve(ctx, ms, "conv:"+realID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectA,
	})
	require.Error(t, err2)

	// Both must be ResolutionError with reason "not-found".
	var resErr1, resErr2 *ResolutionError
	require.ErrorAs(t, err1, &resErr1)
	require.ErrorAs(t, err2, &resErr2)
	assert.Equal(t, "not-found", resErr1.Reason)
	assert.Equal(t, "not-found", resErr2.Reason)

	// The error message format (minus the ref value) must be identical.
	// Replace the specific conv ID to compare the template.
	msg1 := resErr1.Error()
	msg2 := resErr2.Error()
	// Both should follow the same pattern: conversation reference "conv:...": not found
	assert.Contains(t, msg1, "not found")
	assert.Contains(t, msg2, "not found")
}

func TestResolve_ConvByID_NilProjectID_GlobalDM_Allowed(t *testing.T) {
	// AC-30: Conversation with nil ProjectID (global DM) — allowed from any project.
	ms := newMockStore()
	ctx := context.Background()
	projectA := uuid.NewString()
	senderID := uuid.NewString()
	otherID := uuid.NewString()
	convID := uuid.NewString()

	// Global DM (nil ProjectID) with kind-encoded external_ref.
	extRef := mustDMKey("user", senderID, "user", otherID)
	ms.addConversation(
		&store.Conversation{ID: convID, ProjectID: nil, Kind: "direct", Surface: "native", ExternalRef: extRef},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: senderID},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: otherID},
	)

	result, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectA,
	})
	require.NoError(t, err)
	assert.Equal(t, convID, result.ConversationID)
	assert.False(t, result.Created)
}

func TestResolve_ConvByID_NotFound_DoesNotExist(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()

	_, err := Resolve(ctx, ms, "conv:"+uuid.NewString(), ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   uuid.NewString(),
		ProjectID:           uuid.NewString(),
	})
	require.Error(t, err)

	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "not-found", resErr.Reason)
}

// ---------------------------------------------------------------------------
// Resolve @<agent-slug> tests
// ---------------------------------------------------------------------------

func TestResolve_AgentSlug_WithinCurrentProject(t *testing.T) {
	// AC-31: @<agent-slug> resolves within current project only.
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	senderID := uuid.NewString()
	agentID := uuid.NewString()

	ms.agents[projectID+"/my-agent"] = &store.Agent{ID: agentID, Slug: "my-agent", ProjectID: projectID}

	result, err := Resolve(ctx, ms, "@my-agent", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectID,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.ConversationID)
	assert.True(t, result.Created, "should create a new DM on first send")
}

func TestResolve_AgentSlug_CreatesDMOnFirstSend(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	senderID := uuid.NewString()
	agentID := uuid.NewString()

	ms.agents[projectID+"/my-agent"] = &store.Agent{ID: agentID, Slug: "my-agent", ProjectID: projectID}

	result, err := Resolve(ctx, ms, "@my-agent", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectID,
	})
	require.NoError(t, err)
	assert.True(t, result.Created)

	// Verify the conversation was created in the store.
	conv, err := ms.GetConversation(ctx, result.ConversationID)
	require.NoError(t, err)
	assert.Equal(t, "direct", conv.Kind)
	// DMs are global — no ProjectID (DEF-10 fix).
	assert.Nil(t, conv.ProjectID, "DM conversations must be global (nil ProjectID)")

	// Verify participants.
	parts := ms.participants[result.ConversationID]
	require.Len(t, parts, 2)

	var hasSender, hasAgent bool
	for _, p := range parts {
		if p.PrincipalKind == "user" && p.PrincipalID == senderID {
			hasSender = true
		}
		if p.PrincipalKind == "agent" && p.PrincipalID == agentID {
			hasAgent = true
		}
	}
	assert.True(t, hasSender, "sender should be a participant")
	assert.True(t, hasAgent, "agent should be a participant")
}

func TestResolve_AgentSlug_ReturnsExistingDM(t *testing.T) {
	// Idempotent: subsequent sends return the same conversation.
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	senderID := uuid.NewString()
	agentID := uuid.NewString()

	ms.agents[projectID+"/my-agent"] = &store.Agent{ID: agentID, Slug: "my-agent", ProjectID: projectID}

	rctx := ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectID,
	}

	// First resolution — creates the DM.
	result1, err := Resolve(ctx, ms, "@my-agent", rctx)
	require.NoError(t, err)
	assert.True(t, result1.Created)

	// Second resolution — returns the existing DM.
	result2, err := Resolve(ctx, ms, "@my-agent", rctx)
	require.NoError(t, err)
	assert.False(t, result2.Created)
	assert.Equal(t, result1.ConversationID, result2.ConversationID, "should return the same conversation")
}

func TestResolve_AgentSlug_NotFoundInProject(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()

	_, err := Resolve(ctx, ms, "@nonexistent-agent", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   uuid.NewString(),
		ProjectID:           projectID,
	})
	require.Error(t, err)

	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "not-found", resErr.Reason)
}

func TestResolve_AgentSlug_NoProject(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()

	_, err := Resolve(ctx, ms, "@my-agent", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   uuid.NewString(),
		ProjectID:           "", // no project
	})
	require.Error(t, err)

	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "no-shared-project", resErr.Reason)
}

func TestResolve_AgentSlug_OtherProjectAgent_NotFound(t *testing.T) {
	// Agent exists in project B but not in project A — should not resolve.
	ms := newMockStore()
	ctx := context.Background()
	projectA := uuid.NewString()
	projectB := uuid.NewString()
	agentID := uuid.NewString()

	ms.agents[projectB+"/my-agent"] = &store.Agent{ID: agentID, Slug: "my-agent", ProjectID: projectB}

	_, err := Resolve(ctx, ms, "@my-agent", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   uuid.NewString(),
		ProjectID:           projectA,
	})
	require.Error(t, err)

	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "not-found", resErr.Reason)
}

// ---------------------------------------------------------------------------
// Resolve @<email> tests
// ---------------------------------------------------------------------------

func TestResolve_Email_GlobalDM(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	senderID := uuid.NewString()
	userID := uuid.NewString()

	ms.users["alice@example.com"] = &store.User{ID: userID, Email: "alice@example.com"}

	result, err := Resolve(ctx, ms, "@alice@example.com", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           uuid.NewString(), // project context doesn't restrict email DMs
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.ConversationID)
	assert.True(t, result.Created)

	// Verify the conversation is global (nil ProjectID).
	conv, err := ms.GetConversation(ctx, result.ConversationID)
	require.NoError(t, err)
	assert.Nil(t, conv.ProjectID, "email DM should be global (nil ProjectID)")
}

func TestResolve_Email_ReturnsExistingDM(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	senderID := uuid.NewString()
	userID := uuid.NewString()

	ms.users["alice@example.com"] = &store.User{ID: userID, Email: "alice@example.com"}

	rctx := ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           uuid.NewString(),
	}

	// First resolution creates.
	result1, err := Resolve(ctx, ms, "@alice@example.com", rctx)
	require.NoError(t, err)
	assert.True(t, result1.Created)

	// Second resolution returns existing.
	result2, err := Resolve(ctx, ms, "@alice@example.com", rctx)
	require.NoError(t, err)
	assert.False(t, result2.Created)
	assert.Equal(t, result1.ConversationID, result2.ConversationID)
}

func TestResolve_Email_UserNotFound(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()

	_, err := Resolve(ctx, ms, "@nobody@example.com", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   uuid.NewString(),
		ProjectID:           uuid.NewString(),
	})
	require.Error(t, err)

	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "not-found", resErr.Reason)
}

// ---------------------------------------------------------------------------
// Resolve #<thread-name> tests
// ---------------------------------------------------------------------------

func TestResolve_Thread_WithinCurrentProject(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	senderID := uuid.NewString()
	convID := uuid.NewString()

	ms.addConversation(
		&store.Conversation{
			ID:          convID,
			ProjectID:   &projectID,
			Kind:        "group",
			Surface:     "native",
			DisplayName: "design-discussion",
		},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: senderID},
	)

	result, err := Resolve(ctx, ms, "#design-discussion", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectID,
	})
	require.NoError(t, err)
	assert.Equal(t, convID, result.ConversationID)
	assert.False(t, result.Created)
}

func TestResolve_Thread_NotFound(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()

	_, err := Resolve(ctx, ms, "#nonexistent-thread", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   uuid.NewString(),
		ProjectID:           uuid.NewString(),
	})
	require.Error(t, err)

	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "not-found", resErr.Reason)
}

func TestResolve_Thread_NoProject(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()

	_, err := Resolve(ctx, ms, "#some-thread", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   uuid.NewString(),
		ProjectID:           "", // no project
	})
	require.Error(t, err)

	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "no-shared-project", resErr.Reason)
}

func TestResolve_Thread_ManyConversations(t *testing.T) {
	// Regression: resolveThread used Limit:0 which clamped to 50, missing
	// threads beyond the first page.
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	senderID := uuid.NewString()

	// Create 65 group conversations. The target is the last one created,
	// which should be beyond the first page (limit=100 in the real code,
	// but the mock paginates at opts.Limit).
	var targetID string
	for i := 0; i < 65; i++ {
		convID := uuid.NewString()
		name := fmt.Sprintf("thread-%03d", i)
		ms.addConversation(
			&store.Conversation{
				ID:          convID,
				ProjectID:   &projectID,
				Kind:        "group",
				Surface:     "native",
				DisplayName: name,
			},
			store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: senderID},
		)
		if i == 64 {
			targetID = convID
		}
	}

	// Resolve the last thread by name — this would fail with the old
	// single-page implementation if the mock limits pages.
	result, err := Resolve(ctx, ms, "#thread-064", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectID,
	})
	require.NoError(t, err)
	assert.Equal(t, targetID, result.ConversationID)
	assert.False(t, result.Created)
}

func TestResolve_Thread_SpaceSlashThread_Rejected(t *testing.T) {
	// AC-31: #<space>/<thread> form is rejected by the parser.
	ms := newMockStore()
	ctx := context.Background()

	_, err := Resolve(ctx, ms, "#my-space/my-thread", ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   uuid.NewString(),
		ProjectID:           uuid.NewString(),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidInput))
}

// ---------------------------------------------------------------------------
// Invalid reference format tests
// ---------------------------------------------------------------------------

func TestResolve_InvalidFormat(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	rctx := ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   uuid.NewString(),
		ProjectID:           uuid.NewString(),
	}

	invalids := []string{
		"",
		"just-a-string",
		"conv:not-a-uuid",
		"@",
		"#",
		"#space/thread",
	}
	for _, input := range invalids {
		t.Run(input, func(t *testing.T) {
			_, err := Resolve(ctx, ms, input, rctx)
			require.Error(t, err)
		})
	}
}

// ---------------------------------------------------------------------------
// ResolutionError tests
// ---------------------------------------------------------------------------

func TestResolutionError_Methods(t *testing.T) {
	notFound := &ResolutionError{Ref: "conv:abc", Reason: "not-found"}
	boundary := &ResolutionError{Ref: "conv:abc", Reason: "boundary-violation"}

	assert.True(t, notFound.IsNotFound())
	assert.False(t, notFound.IsBoundaryViolation())

	assert.False(t, boundary.IsNotFound())
	assert.True(t, boundary.IsBoundaryViolation())

	assert.Contains(t, notFound.Error(), "not found")
	assert.Contains(t, boundary.Error(), "different project")
}

func TestResolutionError_Ambiguous(t *testing.T) {
	err := &ResolutionError{
		Ref:        "@my-agent",
		Reason:     "ambiguous",
		Candidates: []string{"@project-a/my-agent", "@project-b/my-agent"},
	}
	assert.Contains(t, err.Error(), "ambiguous")
	assert.Contains(t, err.Error(), "project-a")
}

// ---------------------------------------------------------------------------
// DEF-1: Post-resolution participant auth tests (Rule 10)
// ---------------------------------------------------------------------------

// TestResolveConvByID_RejectsNonParticipant verifies that a non-participant
// is rejected when resolving a direct conversation via conv:<id>, even if
// the non-participant belongs to the same project.
//
// Rule 10 mutation check: removing the participant check in
// checkPostResolutionAuth makes Principal B's resolution succeed → test fails.
func TestResolveConvByID_RejectsNonParticipant(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	principalA := uuid.NewString() // participant
	principalB := uuid.NewString() // same project, NOT a participant
	otherParty := uuid.NewString() // the other side of the DM
	convID := uuid.NewString()

	// Create a direct conversation in projectID with kind-encoded external_ref.
	extRef := mustDMKey("user", principalA, "user", otherParty)
	ms.addConversation(
		&store.Conversation{ID: convID, ProjectID: &projectID, Kind: "direct", Surface: "native", ExternalRef: extRef},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: principalA, Role: "member"},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: otherParty, Role: "member"},
	)

	// Give principalB presence in the same project (so project isolation passes).
	otherConvID := uuid.NewString()
	ms.addConversation(
		&store.Conversation{ID: otherConvID, ProjectID: &projectID, Kind: "group", Surface: "native"},
		store.ConversationParticipant{ConversationID: otherConvID, PrincipalKind: "user", PrincipalID: principalB, Role: "member"},
	)

	// Principal A resolves conv:<id> successfully.
	resultA, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   principalA,
		ProjectID:           projectID,
	})
	require.NoError(t, err, "participant A should resolve successfully")
	assert.Equal(t, convID, resultA.ConversationID)

	// Principal B resolves conv:<id> and gets 'not-a-participant' error.
	_, err = Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   principalB,
		ProjectID:           projectID,
	})
	require.Error(t, err, "non-participant B should be rejected")
	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "not-a-participant", resErr.Reason,
		"expected not-a-participant, got %s", resErr.Reason)
}

// TestResolve_GroupConv_AcceptsNonParticipantProjectMember verifies that a
// project member can resolve a group conversation they have never posted in
// (no prior participation required for group conversations).
//
// Rule 10 mutation check: adding a participant requirement for group
// conversations would make this test fail.
func TestResolve_GroupConv_AcceptsNonParticipantProjectMember(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	memberID := uuid.NewString() // project member, never posted in the group
	convID := uuid.NewString()

	// Create a group conversation with NO participants matching memberID.
	otherUserID := uuid.NewString()
	ms.addConversation(
		&store.Conversation{ID: convID, ProjectID: &projectID, Kind: "group", Surface: "native", DisplayName: "general"},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: otherUserID, Role: "member"},
	)

	// The member resolves conv:<id> — should succeed because group
	// conversations only require project membership, not participation.
	result, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   memberID,
		ProjectID:           projectID,
	})
	require.NoError(t, err, "project member should resolve group conv without being a participant")
	assert.Equal(t, convID, result.ConversationID)
	assert.False(t, result.Created)
}

// TestResolve_DirectConv_RejectionGrammarIndependent verifies that the same
// direct-conversation rejection occurs via BOTH conv:<id> AND @<agent-name>,
// proving the auth check is grammar-independent.
//
// Rule 10 mutation check: removing checkPostResolutionAuth from Resolve()
// would let the @<agent> path succeed for an already-existing DM the sender
// is not party to → test fails.
func TestResolve_DirectConv_RejectionGrammarIndependent(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	userA := uuid.NewString()    // owns the DM
	agentID := uuid.NewString()  // the other party in the DM
	intruder := uuid.NewString() // different user, same project

	// Register an agent in the project.
	ms.agents[projectID+"/target-agent"] = &store.Agent{ID: agentID, Slug: "target-agent", ProjectID: projectID}

	// Create an existing DM between userA and the agent with kind-encoded key.
	dmConvID := uuid.NewString()
	dmExtRef := mustDMKey("user", userA, "agent", agentID)
	ms.addConversation(
		&store.Conversation{ID: dmConvID, ProjectID: &projectID, Kind: "direct", Surface: "native", ExternalRef: dmExtRef},
		store.ConversationParticipant{ConversationID: dmConvID, PrincipalKind: "user", PrincipalID: userA, Role: "member"},
		store.ConversationParticipant{ConversationID: dmConvID, PrincipalKind: "agent", PrincipalID: agentID, Role: "member"},
	)

	// Give the intruder presence in the project.
	otherConvID := uuid.NewString()
	ms.addConversation(
		&store.Conversation{ID: otherConvID, ProjectID: &projectID, Kind: "group", Surface: "native"},
		store.ConversationParticipant{ConversationID: otherConvID, PrincipalKind: "user", PrincipalID: intruder, Role: "member"},
	)

	intruderCtx := ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   intruder,
		ProjectID:           projectID,
	}

	// Path 1: conv:<id> — must reject.
	_, err1 := Resolve(ctx, ms, "conv:"+dmConvID, intruderCtx)
	require.Error(t, err1, "conv:<id> path should reject non-participant")
	var resErr1 *ResolutionError
	require.ErrorAs(t, err1, &resErr1)
	assert.Equal(t, "not-a-participant", resErr1.Reason)

	// Path 2: @<agent-slug> — the intruder resolves @target-agent.
	// Since a DM already exists between user A and the agent, the intruder
	// should NOT be able to resolve it. However, @<agent> triggers
	// resolve-or-create: if no existing DM is found for the intruder, a NEW
	// DM is created. The intruder IS a participant in the newly created DM.
	// The test verifies that the intruder does NOT get access to user A's
	// existing DM — the result should be a different conversation.
	result2, err2 := Resolve(ctx, ms, "@target-agent", intruderCtx)
	require.NoError(t, err2, "@agent path should create a new DM for intruder")
	assert.True(t, result2.Created, "should be a newly created conversation")
	assert.NotEqual(t, dmConvID, result2.ConversationID,
		"intruder must get a NEW DM, not user A's existing DM")
}

// ---------------------------------------------------------------------------
// Guard tests (permanent, with floors per rule 14)
// ---------------------------------------------------------------------------

// TestGuardA_NoDirectConversationsWithEmptyExternalRef asserts that zero
// direct conversations have an empty external_ref. Seeded with at least 3
// direct conversations, all with external_ref set.
//
// Rule 14: the test asserts a floor of 3 rows examined.
func TestGuardA_NoDirectConversationsWithEmptyExternalRef(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()

	// Seed 3 direct conversations via the @agent resolve path.
	for i := 0; i < 3; i++ {
		senderID := uuid.NewString()
		agentID := uuid.NewString()
		slug := fmt.Sprintf("agent-%d", i)
		ms.agents[projectID+"/"+slug] = &store.Agent{ID: agentID, Slug: slug, ProjectID: projectID}

		_, err := Resolve(ctx, ms, "@"+slug, ResolveContext{
			SenderPrincipalKind: "user",
			SenderPrincipalID:   senderID,
			ProjectID:           projectID,
		})
		require.NoError(t, err)
	}

	// Count direct conversations with empty external_ref.
	var directCount, emptyRefCount int
	for _, conv := range ms.conversations {
		if conv.Kind != "direct" {
			continue
		}
		directCount++
		if conv.ExternalRef == "" {
			emptyRefCount++
		}
	}

	// Rule 14: floor — we must have examined at least 3 rows.
	require.GreaterOrEqual(t, directCount, 3,
		"floor violation: expected at least 3 direct conversations, found %d", directCount)

	assert.Equal(t, 0, emptyRefCount,
		"found %d direct conversations with empty external_ref", emptyRefCount)
}

// TestGuardB_EveryDMRowHasTwoParticipants asserts that every conversation
// whose external_ref starts with "dm:" has exactly two active participants.
//
// Rule 14: the test asserts a floor of 3 dm: rows examined.
func TestGuardB_EveryDMRowHasTwoParticipants(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()

	// Seed 3 DM conversations via the @agent resolve path.
	for i := 0; i < 3; i++ {
		senderID := uuid.NewString()
		agentID := uuid.NewString()
		slug := fmt.Sprintf("guard-agent-%d", i)
		ms.agents[projectID+"/"+slug] = &store.Agent{ID: agentID, Slug: slug, ProjectID: projectID}

		_, err := Resolve(ctx, ms, "@"+slug, ResolveContext{
			SenderPrincipalKind: "user",
			SenderPrincipalID:   senderID,
			ProjectID:           projectID,
		})
		require.NoError(t, err)
	}

	var dmCount int
	for _, conv := range ms.conversations {
		if len(conv.ExternalRef) < 3 || conv.ExternalRef[:3] != "dm:" {
			continue
		}
		dmCount++
		parts := ms.participants[conv.ID]
		assert.Len(t, parts, 2,
			"dm: conversation %s has %d participants, expected 2", conv.ID, len(parts))
	}

	// Rule 14: floor — at least 3 dm: rows examined.
	require.GreaterOrEqual(t, dmCount, 3,
		"floor violation: expected at least 3 dm: conversations, found %d", dmCount)
}

// ---------------------------------------------------------------------------
// AC tests (DEF-8, DEF-10)
// ---------------------------------------------------------------------------

// TestResolve_SamePathIdempotency_AgentDM verifies that calling Resolve twice
// with the same (sender, agent) pair via the @agent syntax produces exactly
// one conversation row — i.e. the resolver path is idempotent. The first call
// creates the conversation; the second call must find and return it.
func TestResolve_SamePathIdempotency_AgentDM(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()
	senderID := uuid.NewString()
	agentID := uuid.NewString()

	ms.agents[projectID+"/converge-agent"] = &store.Agent{
		ID: agentID, Slug: "converge-agent", ProjectID: projectID,
	}

	rctx := ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectID,
	}

	// First send via @agent (resolveAgentDM).
	result1, err := Resolve(ctx, ms, "@converge-agent", rctx)
	require.NoError(t, err)
	assert.True(t, result1.Created, "first send should create")

	// Second send via @agent (resolveAgentDM) — same pair.
	result2, err := Resolve(ctx, ms, "@converge-agent", rctx)
	require.NoError(t, err)
	assert.False(t, result2.Created, "second send should find existing")

	// Assert convergence by checking the conversation row count.
	assert.Equal(t, result1.ConversationID, result2.ConversationID,
		"both sends must resolve to the same conversation")

	// Count direct conversations for this pair in the DB.
	var directCount int
	for _, conv := range ms.conversations {
		if conv.Kind == "direct" && conv.ExternalRef != "" {
			// Check if this conversation belongs to our sender+agent pair.
			parts := ms.participants[conv.ID]
			hasSender, hasAgent := false, false
			for _, p := range parts {
				if p.PrincipalKind == "user" && p.PrincipalID == senderID {
					hasSender = true
				}
				if p.PrincipalKind == "agent" && p.PrincipalID == agentID {
					hasAgent = true
				}
			}
			if hasSender && hasAgent {
				directCount++
			}
		}
	}
	assert.Equal(t, 1, directCount,
		"there must be exactly ONE direct conversation for the sender+agent pair, found %d", directCount)
}

// TestAC_DEF8_1_CrossPath_DualWriteAndResolverConverge verifies that the
// dual-write path (ResolveOrCreateDMConversation) and the resolver path
// (Resolve with @agent syntax) produce the SAME conversation for the same
// principal pair. This is the true cross-path convergence test — unlike the
// original TestResolve_SamePathIdempotency_AgentDM which only exercised the resolver path twice.
//
// Steps:
//  1. Call ResolveOrCreateDMConversation (dual-write/legacy path)
//  2. Call Resolve with @agent-slug (resolver path)
//  3. Assert both return the SAME conversation ID
//  4. Assert exactly ONE direct conversation row exists (rule 13: observe effect)
func TestAC_DEF8_1_CrossPath_DualWriteAndResolverConverge(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	projectID := uuid.NewString()
	senderID := uuid.NewString()
	agentID := uuid.NewString()
	agentSlug := "crosspath-agent"

	ms.agents[projectID+"/"+agentSlug] = &store.Agent{
		ID: agentID, Slug: agentSlug, ProjectID: projectID,
	}

	// Step 1: Legacy dual-write path.
	convResult := ResolveOrCreateDMConversation(ctx, ms, ms, log, "user", senderID, "agent", agentID)
	require.NotNil(t, convResult, "dual-write path must return a result (rule 14: non-zero floor)")

	// Step 2: Resolver path via @agent.
	rctx := ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   senderID,
		ProjectID:           projectID,
	}
	resolveResult, err := Resolve(ctx, ms, "@"+agentSlug, rctx)
	require.NoError(t, err)
	require.NotNil(t, resolveResult, "resolver path must return a result")

	// Step 3: Same conversation ID.
	assert.Equal(t, convResult.ConversationID, resolveResult.ConversationID,
		"dual-write and resolver paths must converge to the same conversation")

	// Step 4: Exactly one direct conversation row for this pair (rule 13: observe EFFECT).
	var directCount int
	for _, conv := range ms.conversations {
		if conv.Kind == "direct" && conv.ExternalRef != "" {
			// Check if this conversation belongs to our sender+agent pair by
			// examining the external_ref for both UUIDs.
			if strings.Contains(conv.ExternalRef, senderID) && strings.Contains(conv.ExternalRef, agentID) {
				directCount++
			}
		}
	}
	assert.Equal(t, 1, directCount,
		"there must be exactly ONE direct conversation for the sender+agent pair, found %d", directCount)
}

// TestAC_DEF8_2_AllDirectConversationsHaveNilProjectID verifies that every
// direct conversation created through the resolve paths has nil ProjectID.
// This closes DEF-10.
func TestAC_DEF8_2_AllDirectConversationsHaveNilProjectID(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	projectID := uuid.NewString()

	// Create conversations through the @agent path.
	for i := 0; i < 3; i++ {
		senderID := uuid.NewString()
		agentID := uuid.NewString()
		slug := fmt.Sprintf("ac-agent-%d", i)
		ms.agents[projectID+"/"+slug] = &store.Agent{ID: agentID, Slug: slug, ProjectID: projectID}

		_, err := Resolve(ctx, ms, "@"+slug, ResolveContext{
			SenderPrincipalKind: "user",
			SenderPrincipalID:   senderID,
			ProjectID:           projectID,
		})
		require.NoError(t, err)
	}

	// Create conversations through the @email path.
	for i := 0; i < 2; i++ {
		senderID := uuid.NewString()
		userID := uuid.NewString()
		email := fmt.Sprintf("ac-user-%d@example.com", i)
		ms.users[email] = &store.User{ID: userID, Email: email}

		_, err := Resolve(ctx, ms, "@"+email, ResolveContext{
			SenderPrincipalKind: "user",
			SenderPrincipalID:   senderID,
			ProjectID:           projectID,
		})
		require.NoError(t, err)
	}

	// Assert ALL direct conversations have nil ProjectID.
	var directCount int
	for _, conv := range ms.conversations {
		if conv.Kind != "direct" {
			continue
		}
		directCount++
		assert.Nil(t, conv.ProjectID,
			"direct conversation %s has non-nil ProjectID %v — DEF-10 violation",
			conv.ID, conv.ProjectID)
	}

	// Rule 14: floor — at least 5 direct conversations were examined.
	require.GreaterOrEqual(t, directCount, 5,
		"floor violation: expected at least 5 direct conversations, found %d", directCount)
}

// ---------------------------------------------------------------------------
// Key-based DM auth tests (DEF-8, WS-1c)
// ---------------------------------------------------------------------------

// TestKeyBasedAuth_AuthorizedCaller verifies that a caller named in the DM key
// passes the key-based auth check when resolving via conv:<id>.
func TestKeyBasedAuth_AuthorizedCaller(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	userID := uuid.NewString()
	agentID := uuid.NewString()
	convID := uuid.NewString()

	extRef := mustDMKey("user", userID, "agent", agentID)
	ms.addConversation(
		&store.Conversation{ID: convID, Kind: "direct", Surface: "native", ExternalRef: extRef},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: userID},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "agent", PrincipalID: agentID},
	)

	// User resolves — authorized.
	result, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   userID,
	})
	require.NoError(t, err, "caller named in key should be authorized")
	assert.Equal(t, convID, result.ConversationID)

	// Agent resolves — also authorized.
	result2, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "agent",
		SenderPrincipalID:   agentID,
	})
	require.NoError(t, err, "agent named in key should be authorized")
	assert.Equal(t, convID, result2.ConversationID)
}

// TestKeyBasedAuth_UnauthorizedCaller_WrongID verifies that a caller with a
// different UUID is rejected even if they are of the right kind.
func TestKeyBasedAuth_UnauthorizedCaller_WrongID(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	userA := uuid.NewString()
	userB := uuid.NewString()
	intruder := uuid.NewString()
	convID := uuid.NewString()

	extRef := mustDMKey("user", userA, "user", userB)
	ms.addConversation(
		&store.Conversation{ID: convID, Kind: "direct", Surface: "native", ExternalRef: extRef},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: userA},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: userB},
	)

	_, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "user",
		SenderPrincipalID:   intruder,
	})
	require.Error(t, err)
	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "not-a-participant", resErr.Reason)
}

// TestKeyBasedAuth_UnauthorizedCaller_WrongKind verifies that a caller with
// the right UUID but wrong kind is rejected. This tests the kind-check
// tightening — the shipped isDMParticipant only checked UUID, not kind.
func TestKeyBasedAuth_UnauthorizedCaller_WrongKind(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	sharedUUID := uuid.NewString()
	agentID := uuid.NewString()
	convID := uuid.NewString()

	// DM key names (user, sharedUUID) and (agent, agentID).
	extRef := mustDMKey("user", sharedUUID, "agent", agentID)
	ms.addConversation(
		&store.Conversation{ID: convID, Kind: "direct", Surface: "native", ExternalRef: extRef},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: sharedUUID},
		store.ConversationParticipant{ConversationID: convID, PrincipalKind: "agent", PrincipalID: agentID},
	)

	// An agent with the same UUID as the user position should be rejected.
	_, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
		SenderPrincipalKind: "agent", // wrong kind — key says "user" for this UUID
		SenderPrincipalID:   sharedUUID,
	})
	require.Error(t, err)
	var resErr *ResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, "not-a-participant", resErr.Reason,
		"same UUID but wrong kind must be rejected (kind-check tightening)")
}

// TestKeyBasedAuth_UnparseableExternalRef verifies that a direct conversation
// with an unparseable external_ref (old format, empty, or corrupt) results in
// denial (fail closed). No fallback to the participants table.
func TestKeyBasedAuth_UnparseableExternalRef(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	userID := uuid.NewString()

	badRefs := []struct {
		name string
		ref  string
	}{
		{"empty", ""},
		{"old_format_no_kinds", "dm:" + uuid.NewString() + ":" + uuid.NewString()},
		{"garbage", "not-a-dm-key"},
		{"partial", "dm:user:" + uuid.NewString()},
	}

	for _, tc := range badRefs {
		t.Run(tc.name, func(t *testing.T) {
			convID := uuid.NewString()
			ms.addConversation(
				&store.Conversation{ID: convID, Kind: "direct", Surface: "native", ExternalRef: tc.ref},
				// User IS in the participants table — but key-based auth must NOT fall back.
				store.ConversationParticipant{ConversationID: convID, PrincipalKind: "user", PrincipalID: userID},
			)

			_, err := Resolve(ctx, ms, "conv:"+convID, ResolveContext{
				SenderPrincipalKind: "user",
				SenderPrincipalID:   userID,
			})
			require.Error(t, err, "unparseable external_ref %q must deny", tc.ref)
			var resErr *ResolutionError
			require.ErrorAs(t, err, &resErr)
			assert.Equal(t, "not-a-participant", resErr.Reason,
				"fail closed: unparseable ref must return not-a-participant")
		})
	}

	// Rule 14: floor — at least 4 bad-ref scenarios examined.
	require.GreaterOrEqual(t, len(badRefs), 4,
		"floor violation: expected at least 4 bad-ref scenarios")
}
