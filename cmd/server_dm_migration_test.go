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
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedOldFormatDMConversation creates a direct conversation with an old-format
// key (dm:<uuidA>:<uuidB>) and the corresponding user and agent rows.
// Returns the conversation ID, user ID, and agent ID.
func seedOldFormatDMConversation(t *testing.T, ctx context.Context, s store.Store) (convID, userID, agentID string) {
	t.Helper()

	userID = uuid.NewString()
	agentID = uuid.NewString()

	// Create user and agent so kind resolution works.
	err := s.CreateUser(ctx, &store.User{
		ID:          userID,
		DisplayName: "test-user",
		Email:       "test-user-" + userID[:8] + "@example.com",
	})
	require.NoError(t, err)

	// Create a project for the agent (required by store).
	projectID := uuid.NewString()
	err = s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "dm-test-project",
		Slug: "dm-test-" + projectID[:8],
	})
	require.NoError(t, err)

	err = s.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		ProjectID: projectID,
		Name:      "test-agent",
		Slug:      "test-agent-" + agentID[:8],
	})
	require.NoError(t, err)

	// Sort the IDs to build the old-format key.
	id1, id2 := userID, agentID
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

	return convID, userID, agentID
}

// TestDMMigrationDryRunMutatesNothing verifies that dry-run mode reports
// what would change without modifying any database rows.
func TestDMMigrationDryRunMutatesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	convID, _, _ := seedOldFormatDMConversation(t, ctx, s)

	// Run in dry-run mode.
	result, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{
		DryRun: true,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalScanned, "should scan the one conversation")
	assert.Equal(t, 1, result.OldFormatRekeyed, "should report 1 re-key in dry-run")

	// Verify the conversation was NOT modified.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	assert.Contains(t, conv.ExternalRef, "dm:",
		"dry-run should not modify the external_ref")
	// Old-format key should still have exactly 3 segments (dm:uuid:uuid).
	_, _, _, _, parseErr := messages.ParseDMKey(conv.ExternalRef)
	assert.Error(t, parseErr, "old-format key should still fail ParseDMKey in dry-run")
}

// TestDMMigrationExecuteRekeysOldFormat verifies that execute mode re-keys
// old-format conversations to kind-encoded format.
func TestDMMigrationExecuteRekeysOldFormat(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	convID, userID, agentID := seedOldFormatDMConversation(t, ctx, s)

	// Run in execute mode.
	result, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{
		DryRun: false,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalScanned)
	assert.Equal(t, 1, result.OldFormatRekeyed)
	assert.Empty(t, result.Errors, "no errors expected")

	// Verify the key was re-keyed to kind-encoded format.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)

	kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(conv.ExternalRef)
	require.NoError(t, parseErr, "re-keyed conversation should parse as a kind-encoded key")

	// Verify both principals are present in the parsed key.
	principals := map[string]string{idA: kindA, idB: kindB}
	assert.Equal(t, "user", principals[userID], "user should be in the key")
	assert.Equal(t, "agent", principals[agentID], "agent should be in the key")
}

// TestDMMigrationIdempotent verifies that running the migration twice
// produces no changes on the second run.
func TestDMMigrationIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seedOldFormatDMConversation(t, ctx, s)

	// First run: re-keys the old-format conversation.
	result1, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{
		DryRun: false,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result1.OldFormatRekeyed)

	// Second run: everything is already kind-encoded.
	result2, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{
		DryRun: false,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result2.TotalScanned, "should still scan")
	assert.Equal(t, 0, result2.OldFormatRekeyed, "nothing to re-key on second run")
}

// TestDMMigrationKindEncodedNoOp verifies that a conversation already in
// kind-encoded format is scanned but not re-keyed (only participants may
// be added if missing).
func TestDMMigrationKindEncodedNoOp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	userID := uuid.NewString()
	agentID := uuid.NewString()

	// Create user and agent.
	err := s.CreateUser(ctx, &store.User{
		ID:          userID,
		DisplayName: "test-user",
		Email:       "test-user-" + userID[:8] + "@example.com",
	})
	require.NoError(t, err)

	projectID := uuid.NewString()
	err = s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "dm-test-project",
		Slug: "dm-test-" + projectID[:8],
	})
	require.NoError(t, err)

	err = s.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		ProjectID: projectID,
		Name:      "test-agent",
		Slug:      "test-agent-" + agentID[:8],
	})
	require.NoError(t, err)

	// Create a conversation with an already-kind-encoded key.
	newKey, err := messages.DMConversationKey("user", userID, "agent", agentID)
	require.NoError(t, err)

	convID := uuid.NewString()
	err = s.CreateConversation(ctx, &store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: newKey,
	})
	require.NoError(t, err)

	result, err := runDMMigrationWithStore(ctx, s, messaging.DMMigrationConfig{
		DryRun: false,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalScanned)
	assert.Equal(t, 0, result.OldFormatRekeyed, "kind-encoded key should not be re-keyed")
	assert.Empty(t, result.Errors)

	// Verify the key is unchanged.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	assert.Equal(t, newKey, conv.ExternalRef, "key must remain unchanged")
}

// TestDMMigrationDefaultIsDryRun_FlagWiring exercises the flag-to-config wiring
// in runServerDMMigration. Verifies that when dmMigrationExecute is false
// (the default), the config sent to the migration engine has DryRun=true.
func TestDMMigrationDefaultIsDryRun_FlagWiring(t *testing.T) {
	// Save and restore all global flags.
	origExecute := dmMigrationExecute
	origDB := dmMigrationDB
	origBatch := dmMigrationBatchSize
	origConfigPath := serverConfigPath
	defer func() {
		dmMigrationExecute = origExecute
		dmMigrationDB = origDB
		dmMigrationBatchSize = origBatch
		serverConfigPath = origConfigPath
	}()

	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Seed data through a direct store connection.
	dmMigrationDB = dbPath
	serverConfigPath = filepath.Join(tmpDir, "nonexistent.yaml")
	seedStore, err := openDMMigrationStore(ctx)
	require.NoError(t, err)

	seedOldFormatDMConversation(t, ctx, seedStore)
	require.NoError(t, seedStore.Close())

	// Set global flags to their defaults (no --execute).
	dmMigrationExecute = false
	dmMigrationDB = dbPath
	dmMigrationBatchSize = 0

	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	err = runServerDMMigration(cmd, nil)
	require.NoError(t, err)

	// Verify the report says dry-run.
	assert.Contains(t, buf.String(), "dry-run")

	// Re-open and verify conversations were NOT modified.
	dmMigrationDB = dbPath
	verifyStore, err := openDMMigrationStore(ctx)
	require.NoError(t, err)
	defer func() { _ = verifyStore.Close() }()

	convs, err := verifyStore.ListConversations(ctx, store.ConversationFilter{Kind: "direct"}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	for _, conv := range convs.Items {
		if conv.ExternalRef == "" {
			continue
		}
		_, _, _, _, parseErr := messages.ParseDMKey(conv.ExternalRef)
		assert.Error(t, parseErr,
			"default (no --execute) must be dry-run: old-format key should not be re-keyed")
	}
}

// Tests that do not need SQLite are in server_dm_migration_safety_test.go
// so the blocking make test-fast gate (go test -tags no_sqlite) can see them.
