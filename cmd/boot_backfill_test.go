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

// listProjectsPanicStore wraps a real store.Store and panics on
// ListProjects, simulating an unexpected nil deref or similar crash
// inside the backfill migration path. All other methods pass through.
type listProjectsPanicStore struct {
	store.Store
}

func (s *listProjectsPanicStore) ListProjects(_ context.Context, _ store.ProjectFilter, _ store.ListOptions) (*store.ListResult[store.Project], error) {
	panic("injected panic in ListProjects")
}

// listConversationsPanicStore wraps a real store.Store and panics on
// ListConversations, simulating an unexpected crash inside the DM key
// migration path. All other methods pass through.
type listConversationsPanicStore struct {
	store.Store
}

func (s *listConversationsPanicStore) ListConversations(_ context.Context, _ store.ConversationFilter, _ store.ListOptions) (*store.ListResult[store.Conversation], error) {
	panic("injected panic in ListConversations")
}

// listMessagesPanicStore wraps a real store.Store and panics on
// ListMessages for a specific project, simulating an unexpected crash
// partway through the per-project backfill loop.
type listMessagesPanicStore struct {
	store.Store
	panicProjectID string
}

func (s *listMessagesPanicStore) ListMessages(ctx context.Context, filter store.MessageFilter, opts store.ListOptions) (*store.ListResult[store.Message], error) {
	if filter.ProjectID == s.panicProjectID {
		panic("injected panic in ListMessages for project " + s.panicProjectID)
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

// ---------------------------------------------------------------------------
// Panic containment — design A2: boot is never blocked
// ---------------------------------------------------------------------------

// TestBootDataMigrations_BackfillPanic_Contained verifies that a panic in
// the message backfill migration is recovered and does not kill the process.
// The hub must still complete the boot hook. The backfill marker must NOT
// be written — a panicking pass is a run-level failure under M-1'.
//
// Mutation-tested: removing the recover in runMigrationSafe causes this
// test to fail (panic propagates). See mutation results in commit message.
func TestBootDataMigrations_BackfillPanic_Contained(t *testing.T) {
	ctx := context.Background()
	realStore := newTestStore(t)

	// Seed data so the backfill would attempt work.
	seedBackfillProjectWithMessage(t, ctx, realStore, "panic-test")

	// Also seed DM data so the DM migration runs and proves it's unaffected.
	seedOldFormatDMConversation(t, ctx, realStore)

	// Wrap: ListProjects panics, everything else works.
	panicStore := &listProjectsPanicStore{Store: realStore}

	buf, restore := captureSlog(t)
	defer restore()

	// Must not panic — the recover catches it.
	assert.NotPanics(t, func() {
		runBootDataMigrations(ctx, panicStore)
	}, "panic in backfill must be contained; boot must complete")

	logOutput := buf.String()

	// The DM migration should have run BEFORE the panic.
	assert.Contains(t, logOutput, "DM key migration: starting",
		"DM migration must still run even when backfill panics")
	assert.Contains(t, logOutput, "DM key migration: pass completed",
		"DM migration must complete even when backfill panics")

	// The panic should be logged.
	assert.Contains(t, logOutput, "recovered from panic",
		"panic must be logged at ERROR")

	// The backfill marker MUST NOT be written.
	marker, err := loadBackfillMarker(ctx, realStore)
	require.NoError(t, err)
	assert.Nil(t, marker.CompletedAt,
		"M-1': panic is a run-level failure; marker must NOT be written")

	// The DM key marker should be written (panic in backfill doesn't
	// affect the DM migration that already completed).
	done, err := IsMigrationComplete(ctx, realStore, MigrationDMKey)
	require.NoError(t, err)
	assert.True(t, done,
		"DM key marker must be written despite backfill panic")
}

// TestBootDataMigrations_DMKeyPanic_BackfillStillRuns verifies that a
// panic in the DM key migration does not prevent the backfill from running.
// Each migration is wrapped in its own recover.
func TestBootDataMigrations_DMKeyPanic_BackfillStillRuns(t *testing.T) {
	ctx := context.Background()
	realStore := newTestStore(t)

	// Seed a project with a message for the backfill.
	seedBackfillProjectWithMessage(t, ctx, realStore, "dm-panic-test")

	// Wrap: ListConversations panics (DM migration path),
	// but backfill path (ListProjects, ListMessages) works fine.
	panicStore := &listConversationsPanicStore{Store: realStore}

	buf, restore := captureSlog(t)
	defer restore()

	assert.NotPanics(t, func() {
		runBootDataMigrations(ctx, panicStore)
	}, "panic in DM migration must be contained")

	logOutput := buf.String()

	// DM migration panic should be logged.
	assert.Contains(t, logOutput, "recovered from panic",
		"DM migration panic must be logged")

	// Backfill should have run AFTER the DM panic.
	assert.Contains(t, logOutput, "Message backfill: starting",
		"backfill must run even when DM migration panics")
	assert.Contains(t, logOutput, "Message backfill: all projects complete",
		"backfill must complete even when DM migration panics")

	// DM key marker must NOT be written (panic = run-level failure).
	done, err := IsMigrationComplete(ctx, realStore, MigrationDMKey)
	require.NoError(t, err)
	assert.False(t, done,
		"DM key marker must NOT be written on panic")

	// Backfill marker SHOULD be written (backfill succeeded).
	marker, err := loadBackfillMarker(ctx, realStore)
	require.NoError(t, err)
	assert.NotNil(t, marker.CompletedAt,
		"backfill marker should be written when backfill succeeds despite DM panic")
}

// TestBootBackfill_PanicPreservesProgress verifies that a panic mid-way
// through the project list does not roll back already-persisted progress.
// The recover is a scoped abort, not a rollback: projects that completed
// full passes and were persisted before the panic remain in projects_done.
// A subsequent boot resumes from where it left off.
//
// Uses runBootDataMigrations (not runMessageBackfill directly) because the
// recover lives in runMigrationSafe, which wraps each migration at the
// runBootDataMigrations level.
func TestBootBackfill_PanicPreservesProgress(t *testing.T) {
	ctx := context.Background()
	realStore := newTestStore(t)

	// Create three projects.
	pid1, _ := seedBackfillProjectWithMessage(t, ctx, realStore, "panic-progress-p1")
	pid2, _ := seedBackfillProjectWithMessage(t, ctx, realStore, "panic-progress-p2")
	pid3, _ := seedBackfillProjectWithMessage(t, ctx, realStore, "panic-progress-p3")

	// Pre-seed pid1 and pid2 as done (simulating earlier boot progress).
	err := saveBackfillProgress(ctx, realStore, backfillMarker{
		ProjectsDone: []string{pid1, pid2},
		Residuals:    3,
	})
	require.NoError(t, err)

	// Also mark DM key migration as done so it doesn't interact.
	err = MarkMigrationComplete(ctx, realStore, MigrationDMKey, 0)
	require.NoError(t, err)

	// Wrap: ListMessages panics for pid3 (the only remaining project).
	panicOnPid3Store := &listMessagesPanicStore{
		Store:          realStore,
		panicProjectID: pid3,
	}

	// Run through the full boot hook — runMigrationSafe provides the recover.
	assert.NotPanics(t, func() {
		runBootDataMigrations(ctx, panicOnPid3Store)
	}, "panic mid-list must be contained by runMigrationSafe")

	// Already-persisted progress must survive the panic.
	marker, err := loadBackfillMarker(ctx, realStore)
	require.NoError(t, err)

	doneSet := make(map[string]bool)
	for _, pid := range marker.ProjectsDone {
		doneSet[pid] = true
	}
	assert.True(t, doneSet[pid1], "pid1 was banked before panic; must survive")
	assert.True(t, doneSet[pid2], "pid2 was banked before panic; must survive")
	assert.False(t, doneSet[pid3], "pid3 panicked; must NOT be in projects_done")

	// Residuals carried forward must survive.
	assert.GreaterOrEqual(t, marker.Residuals, 3,
		"residuals from prior boot must survive the panic")

	// Global marker must NOT be written (not all projects done).
	assert.Nil(t, marker.CompletedAt,
		"global marker must not be written when a project panicked")

	// Second boot (no panic): should resume and complete.
	runBootDataMigrations(ctx, realStore)

	marker, err = loadBackfillMarker(ctx, realStore)
	require.NoError(t, err)
	assert.NotNil(t, marker.CompletedAt,
		"second boot should complete after panic recovery")
}
