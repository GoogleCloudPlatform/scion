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

package cmd

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Adversarial fixture-class tests — sqlite-backed authorization assertions
//
// These tests exercise the must-NOT-repair and must-repair authorization
// outcomes against a real store. Mock-level tests prove counter and format
// correctness; these prove that isDMParticipant (the actual ACL check)
// grants or denies after migration runs.
//
// F-4 is already covered by TestBootDMKeyMigration_FailClosed in
// boot_data_migrations_test.go (same file shape, same assertions).
// ---------------------------------------------------------------------------

// isDMParticipantCheck is duplicated from boot_data_migrations_test.go.
// It replicates the isDMParticipant logic from handlers_chat_v2.go
// without importing pkg/hub (circular dependency).
//
// Note: this function is already defined in boot_data_migrations_test.go
// in the same package, so we reference it directly here.

// seedTwoUsersOldFormatDM creates a direct conversation with an old-format
// key where both principals resolve as users. Returns convID, user1ID, user2ID.
func seedTwoUsersOldFormatDM(t *testing.T, ctx context.Context, s store.Store) (convID, user1ID, user2ID string) {
	t.Helper()

	user1ID = uuid.NewString()
	user2ID = uuid.NewString()

	err := s.CreateUser(ctx, &store.User{
		ID:    user1ID,
		Email: "f8-user1-" + user1ID[:8] + "@example.com",
	})
	require.NoError(t, err)

	err = s.CreateUser(ctx, &store.User{
		ID:    user2ID,
		Email: "f8-user2-" + user2ID[:8] + "@example.com",
	})
	require.NoError(t, err)

	// Sort IDs for old-format key.
	id1, id2 := user1ID, user2ID
	if id1 > id2 {
		id1, id2 = id2, id1
	}
	oldKey := "dm:" + id1 + ":" + id2

	convID = uuid.NewString()
	err = s.CreateConversation(ctx, &store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	})
	require.NoError(t, err)

	return convID, user1ID, user2ID
}

// seedTwoAgentsOldFormatDM creates a direct conversation with an old-format
// key where both principals resolve as agents. Returns convID, agent1ID, agent2ID.
func seedTwoAgentsOldFormatDM(t *testing.T, ctx context.Context, s store.Store) (convID, agent1ID, agent2ID string) {
	t.Helper()

	agent1ID = uuid.NewString()
	agent2ID = uuid.NewString()

	// Create a project for agents.
	projectID := uuid.NewString()
	err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "f9-project",
		Slug: "f9-proj-" + projectID[:8],
	})
	require.NoError(t, err)

	err = s.CreateAgent(ctx, &store.Agent{
		ID:        agent1ID,
		ProjectID: projectID,
		Name:      "f9-agent1",
		Slug:      "f9-agent1-" + agent1ID[:8],
	})
	require.NoError(t, err)

	err = s.CreateAgent(ctx, &store.Agent{
		ID:        agent2ID,
		ProjectID: projectID,
		Name:      "f9-agent2",
		Slug:      "f9-agent2-" + agent2ID[:8],
	})
	require.NoError(t, err)

	// Sort IDs for old-format key.
	id1, id2 := agent1ID, agent2ID
	if id1 > id2 {
		id1, id2 = id2, id1
	}
	oldKey := "dm:" + id1 + ":" + id2

	convID = uuid.NewString()
	err = s.CreateConversation(ctx, &store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	})
	require.NoError(t, err)

	return convID, agent1ID, agent2ID
}

// ---------------------------------------------------------------------------
// F-1: Third-principal denial (sqlite-backed)
// ---------------------------------------------------------------------------

// TestF1_SQLite_ThirdPrincipalDenied verifies the F-1 security property
// against a real store: after rekey, isDMParticipant denies a stranger.
func TestF1_SQLite_ThirdPrincipalDenied(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	convID, userID, agentID := seedOldFormatDMConversation(t, ctx, s)
	strangerID := uuid.NewString()

	// Run the migration.
	_, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{})
	require.NoError(t, err)

	// Read back the conversation.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)

	// Both named principals must be granted.
	assert.True(t, isDMParticipantCheck(conv.ExternalRef, userID),
		"F-1 sqlite: user must be granted by rekeyed ACL")

	// Parse the key to check the agent. isDMParticipantCheck only checks
	// for "user" kind, so we use CheckDMParticipantKey for the agent.
	assert.NoError(t, messages.CheckDMParticipantKey("direct", conv.ExternalRef, "agent", agentID),
		"F-1 sqlite: agent must be granted by rekeyed ACL")

	// THE LOAD-BEARING ASSERTION: stranger must be denied.
	assert.False(t, isDMParticipantCheck(conv.ExternalRef, strangerID),
		"F-1 sqlite: stranger must be denied by rekeyed ACL")
	assert.Error(t, messages.CheckDMParticipantKey("direct", conv.ExternalRef, "user", strangerID),
		"F-1 sqlite: stranger must be denied by CheckDMParticipantKey")
}

// ---------------------------------------------------------------------------
// F-3: One resolves, one doesn't — still denied (sqlite-backed)
// ---------------------------------------------------------------------------

// TestF3_SQLite_OneResolves_StillDenied verifies the F-3 security property
// against a real store: when one principal doesn't resolve, the key is
// unchanged and isDMParticipant denies both principals.
func TestF3_SQLite_OneResolves_StillDenied(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	userID := uuid.NewString()
	unresolvedID := uuid.NewString()

	// Create only the user — unresolvedID is in neither table.
	err := s.CreateUser(ctx, &store.User{
		ID:    userID,
		Email: "f3-user-" + userID[:8] + "@example.com",
	})
	require.NoError(t, err)

	// Sort IDs for old-format key.
	id1, id2 := userID, unresolvedID
	if id1 > id2 {
		id1, id2 = id2, id1
	}
	oldKey := "dm:" + id1 + ":" + id2

	convID := uuid.NewString()
	err = s.CreateConversation(ctx, &store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	})
	require.NoError(t, err)

	// Run the migration.
	result, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Ambiguous, "F-3 sqlite: should be ambiguous")

	// Read back the conversation.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)

	// Key must be unchanged.
	assert.Equal(t, oldKey, conv.ExternalRef,
		"F-3 sqlite: key must be unchanged")

	// SECURITY: both principals must be denied.
	assert.False(t, isDMParticipantCheck(conv.ExternalRef, userID),
		"F-3 sqlite: resolved principal must be denied (old-format key)")
	assert.False(t, isDMParticipantCheck(conv.ExternalRef, unresolvedID),
		"F-3 sqlite: unresolved principal must be denied (old-format key)")
}

// ---------------------------------------------------------------------------
// F-5: Identical UUIDs — degenerate self-DM (sqlite-backed)
// ---------------------------------------------------------------------------

// TestF5_SQLite_IdenticalUUIDs_Rekeyed verifies F-5 against a real store:
// the migration rekeyes identical UUIDs into a self-DM key. The named
// principal is granted, a stranger is denied.
func TestF5_SQLite_IdenticalUUIDs_Rekeyed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sameID := uuid.NewString()
	strangerID := uuid.NewString()

	// The UUID exists as a user.
	err := s.CreateUser(ctx, &store.User{
		ID:    sameID,
		Email: "f5-user-" + sameID[:8] + "@example.com",
	})
	require.NoError(t, err)

	// Old-format key with identical UUIDs.
	oldKey := "dm:" + sameID + ":" + sameID
	convID := uuid.NewString()
	err = s.CreateConversation(ctx, &store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	})
	require.NoError(t, err)

	// Run the migration.
	result, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{})
	require.NoError(t, err)

	assert.Equal(t, 1, result.OldFormatRekeyed, "F-5 sqlite: should rekey")
	assert.Equal(t, 1, result.DegeneratePairs, "F-5 sqlite: should count degenerate pair")

	// Read back the conversation.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)

	// The named principal IS granted.
	assert.True(t, isDMParticipantCheck(conv.ExternalRef, sameID),
		"F-5 sqlite: named principal must be granted by self-DM ACL")

	// A stranger IS denied.
	assert.False(t, isDMParticipantCheck(conv.ExternalRef, strangerID),
		"F-5 sqlite: stranger must be denied by self-DM ACL")
}

// ---------------------------------------------------------------------------
// F-8: Both users — rekeyed, both granted, third denied (sqlite-backed)
// ---------------------------------------------------------------------------

// TestF8_SQLite_BothUsers_Granted_ThirdDenied verifies the F-8 authorization
// property against a real store: after rekey, both user principals are granted
// and a third party is denied.
func TestF8_SQLite_BothUsers_Granted_ThirdDenied(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	convID, user1ID, user2ID := seedTwoUsersOldFormatDM(t, ctx, s)
	strangerID := uuid.NewString()

	// Run the migration.
	result, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{})
	require.NoError(t, err)

	assert.Equal(t, 1, result.OldFormatRekeyed, "F-8 sqlite: should rekey")

	// Read back the conversation.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)

	// Both user principals must be granted.
	assert.True(t, isDMParticipantCheck(conv.ExternalRef, user1ID),
		"F-8 sqlite: user1 must be granted")
	assert.True(t, isDMParticipantCheck(conv.ExternalRef, user2ID),
		"F-8 sqlite: user2 must be granted")

	// Third party denied.
	assert.False(t, isDMParticipantCheck(conv.ExternalRef, strangerID),
		"F-8 sqlite: stranger must be denied")
}

// ---------------------------------------------------------------------------
// F-9: Both agents — rekeyed, both granted, third denied (sqlite-backed)
// ---------------------------------------------------------------------------

// TestF9_SQLite_BothAgents_Granted_ThirdDenied verifies the F-9 authorization
// property against a real store: after rekey, both agent principals are granted
// and a third party is denied.
func TestF9_SQLite_BothAgents_Granted_ThirdDenied(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	convID, agent1ID, agent2ID := seedTwoAgentsOldFormatDM(t, ctx, s)
	strangerID := uuid.NewString()

	// Run the migration.
	result, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{})
	require.NoError(t, err)

	assert.Equal(t, 1, result.OldFormatRekeyed, "F-9 sqlite: should rekey")

	// Read back the conversation.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)

	// Both agent principals must be granted.
	assert.NoError(t, messages.CheckDMParticipantKey("direct", conv.ExternalRef, "agent", agent1ID),
		"F-9 sqlite: agent1 must be granted")
	assert.NoError(t, messages.CheckDMParticipantKey("direct", conv.ExternalRef, "agent", agent2ID),
		"F-9 sqlite: agent2 must be granted")

	// Third party denied.
	assert.Error(t, messages.CheckDMParticipantKey("direct", conv.ExternalRef, "agent", strangerID),
		"F-9 sqlite: stranger agent must be denied")
	assert.False(t, isDMParticipantCheck(conv.ExternalRef, strangerID),
		"F-9 sqlite: stranger must be denied by isDMParticipant")
}
