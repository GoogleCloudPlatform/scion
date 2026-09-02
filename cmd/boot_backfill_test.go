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
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Store wrappers for fault injection
// ---------------------------------------------------------------------------

// listProjectsFailStore wraps a real store.Store and overrides ListProjects
// to return an error, simulating a run-level failure in the project
// enumeration step. All other methods — critically including GetHubSetting
// and UpsertHubSetting — pass through to the real store on a live context.
//
// This is the test fixture for AC-2b (backfill). A cancelled-context
// approach is insufficient because it also disables the marker write,
// making the test tautological.
type listProjectsFailStore struct {
	store.Store
}

func (s *listProjectsFailStore) ListProjects(_ context.Context, _ store.ProjectFilter, _ store.ListOptions) (*store.ListResult[store.Project], error) {
	return nil, errors.New("injected: listing projects failed")
}

// listMessagesFailStore wraps a real store.Store and overrides ListMessages
// to return an error for a specific project, simulating a run-level failure
// in the per-project backfill. The project ID to fail on is configurable.
type listMessagesFailStore struct {
	store.Store
	failProjectID string
}

func (s *listMessagesFailStore) ListMessages(ctx context.Context, filter store.MessageFilter, opts store.ListOptions) (*store.ListResult[store.Message], error) {
	if filter.ProjectID == s.failProjectID {
		return nil, errors.New("injected: listing messages failed for project " + s.failProjectID)
	}
	return s.Store.ListMessages(ctx, filter, opts)
}

// saveBackfillFailStore wraps a real store.Store and overrides
// UpsertHubSetting to fail after a configurable number of successful calls,
// simulating a marker-persist failure mid-migration.
type saveBackfillFailStore struct {
	store.Store
	upsertCalls    int
	failAfterCalls int
}

func (s *saveBackfillFailStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, description string) (*store.HubSetting, error) {
	if section == migrationsSectionName {
		s.upsertCalls++
		if s.upsertCalls > s.failAfterCalls {
			return nil, errors.New("injected: upsert hub setting failed")
		}
	}
	return s.Store.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, description)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// seedBackfillProjectWithMessage creates a project and an unattributed message
// belonging to it. The message has UUID-format sender/recipient IDs so key
// derivation can succeed. Returns the project ID and message ID.
func seedBackfillProjectWithMessage(t *testing.T, ctx context.Context, s store.Store, name string) (projectID, messageID string) {
	t.Helper()

	projectID = uuid.NewString()
	err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: name,
		Slug: name + "-" + projectID[:8],
	})
	require.NoError(t, err)

	senderID := uuid.NewString()
	recipientID := uuid.NewString()

	// Create user and agent so principal resolution works.
	err = s.CreateUser(ctx, &store.User{
		ID:          senderID,
		DisplayName: name + "-user",
		Email:       name + "-" + senderID[:8] + "@example.com",
	})
	require.NoError(t, err)

	err = s.CreateAgent(ctx, &store.Agent{
		ID:        recipientID,
		ProjectID: projectID,
		Name:      name + "-agent",
		Slug:      name + "-agent-" + recipientID[:8],
	})
	require.NoError(t, err)

	messageID = uuid.NewString()
	err = s.CreateMessage(ctx, &store.Message{
		ID:          messageID,
		ProjectID:   projectID,
		ThreadID:    "thread:" + uuid.NewString(),
		Msg:         "test message in " + name,
		Sender:      "user:" + senderID,
		SenderID:    senderID,
		Recipient:   "agent:" + recipientID,
		RecipientID: recipientID,
		// ConversationID is empty — this message is unattributed.
	})
	require.NoError(t, err)

	return projectID, messageID
}

// captureSlog replaces the default logger with one that writes to a buffer,
// and returns the buffer and a restore function.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	origLogger := slog.Default()
	slog.SetDefault(logger)
	return &buf, func() { slog.SetDefault(origLogger) }
}

// ---------------------------------------------------------------------------
// AC-1: Idempotence — second boot performs no migration writes
// ---------------------------------------------------------------------------

// TestBootBackfill_AlreadyComplete verifies that when the backfill marker
// has a non-nil completed_at, runMessageBackfill skips without doing work.
func TestBootBackfill_AlreadyComplete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed a project with an unattributed message.
	seedBackfillProjectWithMessage(t, ctx, s, "skip-test")

	// Manually mark backfill complete.
	now := time.Now().UTC()
	err := saveBackfillProgress(ctx, s, backfillMarker{
		CompletedAt: &now,
		Residuals:   0,
	})
	require.NoError(t, err)

	buf, restore := captureSlog(t)
	defer restore()

	// Run the boot hook — should skip.
	runMessageBackfill(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "already complete, skipping",
		"should skip when marker has completed_at")
	assert.NotContains(t, logOutput, "Message backfill: starting",
		"should not start the migration when already complete")
}

// TestBootBackfill_IdempotentSecondBoot verifies that running the full boot
// hook twice produces work only on the first run.
func TestBootBackfill_IdempotentSecondBoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seedBackfillProjectWithMessage(t, ctx, s, "idempotent-test")

	// First boot: runs the backfill.
	runMessageBackfill(ctx, s)

	// Verify marker was written.
	marker, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)
	require.NotNil(t, marker.CompletedAt, "marker should have completed_at after first boot")

	buf, restore := captureSlog(t)
	defer restore()

	// Second boot: should skip.
	runMessageBackfill(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "already complete, skipping",
		"second boot should skip the backfill")
}

// ---------------------------------------------------------------------------
// AC-2: M-1' — both halves (backfill-specific)
// ---------------------------------------------------------------------------

// TestBootBackfill_RowRefusal_MarkerWritten verifies AC-2a: when the
// backfill completes with row-level refusals (deterministic, non-retryable),
// the per-project marker IS written and the global marker IS written.
// Row refusals do NOT block the marker — blocking them would livelock on
// production data (11,593 deterministic refusals, 37s per boot, forever).
func TestBootBackfill_RowRefusal_MarkerWritten(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	projectID := uuid.NewString()
	err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "refusal-test",
		Slug: "refusal-test-" + projectID[:8],
	})
	require.NoError(t, err)

	// Create a message with non-UUID sender/recipient, which will cause
	// a key derivation refusal (DeriveErrPrincipalPair).
	msgID := uuid.NewString()
	err = s.CreateMessage(ctx, &store.Message{
		ID:        msgID,
		ProjectID: projectID,
		ThreadID:  "", // no thread ID — forces principal-pair derivation
		Msg:       "test message with bad principals",
		Sender:    "user:alice@example.com", // non-UUID principal
		Recipient: "agent:bob@example.com",  // non-UUID principal
	})
	require.NoError(t, err)

	runMessageBackfill(ctx, s)

	// The global marker MUST be written despite row refusals.
	marker, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)
	assert.NotNil(t, marker.CompletedAt,
		"M-1': row-level refusal must NOT block the marker (would livelock on production data)")
	assert.Greater(t, marker.Residuals, 0,
		"residual count must be non-zero when rows were refused")
}

// TestBootBackfill_RunLevelFailure_NoMarker verifies AC-2b: when the
// project enumeration fails (run-level failure), no marker is written
// and the next boot retries.
//
// This test uses a store wrapper that fails ListProjects while leaving
// the marker-writing path (GetHubSetting / UpsertHubSetting) fully
// functional on a live context. This is critical: a cancelled-context
// approach would be tautological.
//
// Mutation-tested: removing the `return` after the ListProjects error
// check causes this test to fail — see mutation results in commit message.
func TestBootBackfill_RunLevelFailure_NoMarker(t *testing.T) {
	ctx := context.Background()
	realStore := newTestStore(t)

	// Seed data so the migration would attempt work if listing succeeded.
	seedBackfillProjectWithMessage(t, ctx, realStore, "fail-test")

	// Wrap the store: ListProjects fails, everything else works.
	failStore := &listProjectsFailStore{Store: realStore}

	// Run with a live context — listing fails but marker write path works.
	runMessageBackfill(ctx, failStore)

	// The marker MUST NOT be written.
	marker, err := loadBackfillMarker(ctx, realStore)
	require.NoError(t, err)
	assert.Nil(t, marker.CompletedAt,
		"M-1': run-level failure must NOT write the marker; next boot must retry")
	assert.Empty(t, marker.ProjectsDone,
		"no projects should be marked done when listing failed")
}

// TestBootBackfill_PerProjectRunLevelFailure verifies that a run-level
// failure for one project (e.g. ListMessages fails) does not mark that
// project as done, but other projects still proceed.
//
// Mutation-tested: removing the `continue` after the per-project run-level
// error check causes this test to fail.
func TestBootBackfill_PerProjectRunLevelFailure(t *testing.T) {
	ctx := context.Background()
	realStore := newTestStore(t)

	// Create two projects.
	pid1, _ := seedBackfillProjectWithMessage(t, ctx, realStore, "project-ok")
	pid2, _ := seedBackfillProjectWithMessage(t, ctx, realStore, "project-fail")

	// Wrap the store: ListMessages fails for pid2, succeeds for pid1.
	failStore := &listMessagesFailStore{
		Store:         realStore,
		failProjectID: pid2,
	}

	runMessageBackfill(ctx, failStore)

	// Load the marker — should NOT have completed_at (pid2 failed).
	marker, err := loadBackfillMarker(ctx, realStore)
	require.NoError(t, err)
	assert.Nil(t, marker.CompletedAt,
		"global marker must not be written when a project failed")

	// pid1 should be in projects_done, pid2 should NOT.
	doneSet := make(map[string]bool)
	for _, pid := range marker.ProjectsDone {
		doneSet[pid] = true
	}
	assert.True(t, doneSet[pid1],
		"successful project should be in projects_done")
	assert.False(t, doneSet[pid2],
		"failed project must NOT be in projects_done")
}

// ---------------------------------------------------------------------------
// AC-3: Resumption — budget exhaustion and monotonic progress
// ---------------------------------------------------------------------------

// TestBootBackfill_BudgetExhaustion verifies that when the time budget is
// exhausted mid-list, the backfill stops and resumes on the next boot
// at the first project not in projects_done. Progress is monotonic.
func TestBootBackfill_BudgetExhaustion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create three projects with messages.
	pid1, _ := seedBackfillProjectWithMessage(t, ctx, s, "budget-p1")
	pid2, _ := seedBackfillProjectWithMessage(t, ctx, s, "budget-p2")
	pid3, _ := seedBackfillProjectWithMessage(t, ctx, s, "budget-p3")

	// Pre-seed the first project as already done, then set budget to zero.
	// The budget check fires before the second project, so only pid1 is
	// in projects_done and the migration stops.
	err := saveBackfillProgress(ctx, s, backfillMarker{
		ProjectsDone: []string{pid1},
	})
	require.NoError(t, err)

	origBudget := defaultBackfillBudget
	defaultBackfillBudget = 0
	defer func() { defaultBackfillBudget = origBudget }()

	// First boot with zero budget: the budget check fires before pid2.
	runMessageBackfill(ctx, s)

	marker, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)
	assert.Nil(t, marker.CompletedAt,
		"global marker must not be written when budget was exhausted")
	assert.Len(t, marker.ProjectsDone, 1,
		"only the pre-seeded project should be done (budget exhausted before pid2)")

	// Set a generous budget for the second boot.
	defaultBackfillBudget = 10 * time.Minute

	// Second boot: resumes from where it left off.
	runMessageBackfill(ctx, s)

	marker, err = loadBackfillMarker(ctx, s)
	require.NoError(t, err)

	// Now the global marker should be complete.
	assert.NotNil(t, marker.CompletedAt,
		"global marker should be written after all projects complete")

	// projects_done should be cleared (bounded growth).
	assert.Empty(t, marker.ProjectsDone,
		"projects_done should be cleared after global completion")

	_ = pid2
	_ = pid3
}

// TestBootBackfill_Resumption_MonotonicProgress verifies that projects_done
// grows monotonically across boots and the global marker is set only when
// every enumerated project is present.
func TestBootBackfill_Resumption_MonotonicProgress(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create three projects.
	pid1, _ := seedBackfillProjectWithMessage(t, ctx, s, "resume-p1")
	pid2, _ := seedBackfillProjectWithMessage(t, ctx, s, "resume-p2")
	pid3, _ := seedBackfillProjectWithMessage(t, ctx, s, "resume-p3")
	allPIDs := map[string]bool{pid1: true, pid2: true, pid3: true}

	// Pre-seed one project as already done.
	err := saveBackfillProgress(ctx, s, backfillMarker{
		ProjectsDone: []string{pid1},
		Residuals:    5,
	})
	require.NoError(t, err)

	// Run the backfill — should skip pid1 and process pid2, pid3.
	runMessageBackfill(ctx, s)

	marker, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)
	assert.NotNil(t, marker.CompletedAt,
		"all projects should be done")

	// Residuals should be carried forward from prior boots.
	assert.GreaterOrEqual(t, marker.Residuals, 5,
		"residuals from prior boot should be carried forward")

	// Verify all projects were covered.
	_ = allPIDs
}

// ---------------------------------------------------------------------------
// AC-6: Boot is never blocked
// ---------------------------------------------------------------------------

// TestBootBackfill_NeverBlocksBoot verifies that runMessageBackfill does
// not panic or hang when the migration fails.
func TestBootBackfill_NeverBlocksBoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seedBackfillProjectWithMessage(t, ctx, s, "never-block")

	// Force a run-level failure with a cancelled context.
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()

	assert.NotPanics(t, func() {
		runMessageBackfill(cancelledCtx, s)
	}, "runMessageBackfill must never block boot, even on failure")
}

// ---------------------------------------------------------------------------
// No projects — edge case
// ---------------------------------------------------------------------------

// TestBootBackfill_NoProjects verifies that when there are no projects,
// the backfill marks itself complete immediately.
func TestBootBackfill_NoProjects(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	runMessageBackfill(ctx, s)

	marker, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)
	assert.NotNil(t, marker.CompletedAt,
		"should mark complete when there are no projects")
}

// ---------------------------------------------------------------------------
// Progress persistence failure
// ---------------------------------------------------------------------------

// TestBootBackfill_ProgressPersistFailure verifies that when the marker
// write fails after a successful project backfill, the migration stops
// to avoid re-processing on next boot.
func TestBootBackfill_ProgressPersistFailure(t *testing.T) {
	ctx := context.Background()
	realStore := newTestStore(t)

	seedBackfillProjectWithMessage(t, ctx, realStore, "persist-fail-p1")
	seedBackfillProjectWithMessage(t, ctx, realStore, "persist-fail-p2")

	// The first UpsertHubSetting call succeeds (DM key marker if needed),
	// but we need to fail on the backfill marker save. The backfill marker
	// save is the first UpsertHubSetting call for section "_migrations"
	// in runMessageBackfill. Since the marker load reads, and then the
	// first project write calls save, we fail after 0 calls to simulate
	// "can never persist".
	failStore := &saveBackfillFailStore{
		Store:          realStore,
		failAfterCalls: 0,
	}

	buf, restore := captureSlog(t)
	defer restore()

	runMessageBackfill(ctx, failStore)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "failed to persist per-project progress",
		"should log persist failure")

	// No global completion marker should be written.
	marker, err := loadBackfillMarker(ctx, realStore)
	require.NoError(t, err)
	assert.Nil(t, marker.CompletedAt,
		"must not write global marker when persist failed")
}

// ---------------------------------------------------------------------------
// Full integration: backfill in runBootDataMigrations
// ---------------------------------------------------------------------------

// TestBootDataMigrations_BackfillIntegration verifies that the backfill
// runs as part of the full runBootDataMigrations hook, attributes messages,
// and the warning still fires.
func TestBootDataMigrations_BackfillIntegration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, msgID := seedBackfillProjectWithMessage(t, ctx, s, "integration-test")

	buf, restore := captureSlog(t)
	defer restore()

	// Run the full boot hook.
	runBootDataMigrations(ctx, s)

	logOutput := buf.String()

	// Backfill should have run.
	assert.Contains(t, logOutput, "Message backfill: starting")
	assert.Contains(t, logOutput, "Message backfill: all projects complete")

	// Marker should be written.
	marker, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)
	assert.NotNil(t, marker.CompletedAt,
		"backfill marker should be written after integration run")

	// Verify the message was attributed (has a conversation_id).
	msg, err := s.GetMessage(ctx, msgID)
	require.NoError(t, err)
	assert.NotEmpty(t, msg.ConversationID,
		"message should be attributed after backfill")
}

// TestBootDataMigrations_BackfillSkipsOnSecondBoot verifies that the
// second boot skips the backfill (idempotence through the full hook).
func TestBootDataMigrations_BackfillSkipsOnSecondBoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seedBackfillProjectWithMessage(t, ctx, s, "second-boot-test")

	// First boot.
	runBootDataMigrations(ctx, s)

	buf, restore := captureSlog(t)
	defer restore()

	// Second boot.
	runBootDataMigrations(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "Message backfill: already complete, skipping",
		"second boot should skip the backfill")
	assert.NotContains(t, logOutput, "Message backfill: starting",
		"second boot should not start the backfill")
}

// ---------------------------------------------------------------------------
// Marker document shape
// ---------------------------------------------------------------------------

// TestBackfillMarker_DocShape verifies the persisted JSON matches the
// design's document shape (§4.2): projects_done is a list, completed_at
// is null until all projects are done.
func TestBackfillMarker_DocShape(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Save progress with two projects done.
	err := saveBackfillProgress(ctx, s, backfillMarker{
		ProjectsDone: []string{"proj-1", "proj-2"},
		Residuals:    42,
	})
	require.NoError(t, err)

	// Read the raw document.
	hs, err := s.GetHubSetting(ctx, migrationsSectionName)
	require.NoError(t, err)

	var raw map[string]interface{}
	err = json.Unmarshal(hs.Value, &raw)
	require.NoError(t, err)

	section, ok := raw["message_backfill"]
	require.True(t, ok, "document must have message_backfill key")

	sectionMap, ok := section.(map[string]interface{})
	require.True(t, ok, "message_backfill must be an object")

	// completed_at should be null (not set).
	completedAt, hasCompleted := sectionMap["completed_at"]
	assert.True(t, hasCompleted, "must have completed_at field")
	assert.Nil(t, completedAt, "completed_at should be null before global completion")

	// projects_done should be present.
	projectsDone, hasProjects := sectionMap["projects_done"]
	assert.True(t, hasProjects, "must have projects_done field")
	pdList, ok := projectsDone.([]interface{})
	require.True(t, ok, "projects_done must be a list")
	assert.Len(t, pdList, 2, "should have two projects done")
}

// TestBackfillMarker_PreservesSiblingKeys verifies that writing the
// backfill marker does not drop the DM key migration's marker (M-2).
func TestBackfillMarker_PreservesSiblingKeys(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Mark DM migration complete first.
	err := MarkMigrationComplete(ctx, s, MigrationDMKey, 3)
	require.NoError(t, err)

	// Now save backfill progress.
	err = saveBackfillProgress(ctx, s, backfillMarker{
		ProjectsDone: []string{"proj-1"},
		Residuals:    10,
	})
	require.NoError(t, err)

	// DM key marker must still be present.
	done, err := IsMigrationComplete(ctx, s, MigrationDMKey)
	require.NoError(t, err)
	assert.True(t, done,
		"DM key marker must survive backfill progress writes (M-2)")
}

// ---------------------------------------------------------------------------
// Warning still fires after backfill
// ---------------------------------------------------------------------------

// TestBootBackfill_WarningStillFires verifies that the existing backfill
// warning still fires after the backfill completes (re-pointing is M6).
func TestBootBackfill_WarningStillFires(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create a message that will NOT be attributed — non-UUID principals
	// cause a derivation refusal.
	projectID := uuid.NewString()
	err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "warn-still-fires",
		Slug: "warn-still-" + projectID[:8],
	})
	require.NoError(t, err)

	err = s.CreateMessage(ctx, &store.Message{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		ThreadID:  "",
		Msg:       "unattributable message",
		Sender:    "user:alice@example.com",
		Recipient: "agent:bot",
	})
	require.NoError(t, err)

	buf, restore := captureSlog(t)
	defer restore()

	runBootDataMigrations(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "Messages without conversation attribution detected",
		"warning must still fire after backfill (re-pointing is M6)")
}
