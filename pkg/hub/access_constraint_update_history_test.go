// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite

package hub

// P6 (#2404 remainder): boundary UPDATE writes exactly one
// access_constraint_history row in the same transaction as the mutation.
// Every test synchronizes on returned values and store reads; none asserts
// on timing. Shared P6 fixtures live here.

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func p6RequireUUID(t *testing.T, value string) {
	t.Helper()
	parsed, err := uuid.Parse(value)
	require.NoError(t, err, "%q must be a UUID", value)
	require.Equal(t, parsed.String(), value, "%q must be a canonical UUID", value)
	require.NotEqual(t, uuid.Nil, parsed)
}

func p6OperationContext(t *testing.T, correlationID string) context.Context {
	t.Helper()
	operation, err := auditevent.NewOperationContext(correlationID)
	require.NoError(t, err)
	return auditevent.ContextWithOperation(context.Background(), operation)
}

// p6SeedUpdatableConstraint creates, through governance, a system constraint
// on a dedicated target user (so no update can lock out the admin).
func p6SeedUpdatableConstraint(t *testing.T, gs *GovernanceService, ps *PreviewService, s store.Store, name string) (string, *store.AccessConstraint) {
	t.Helper()
	adminID := govSeedAdminUser(t, s, name+"-admin")
	targetID := pvSeedUser(t, s, name+"-target")
	created := govCreateAndCommit(t, gs, ps, &store.AccessConstraint{
		Name:                 name,
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(targetID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create"},
		Purpose:              "p6 purpose " + name,
		CreatedBy:            adminID,
	}, pvTestActor(adminID))
	require.NotNil(t, created)
	return adminID, created
}

// p6NarrowedDraft removes agent.create: a tighten update.
func p6NarrowedDraft(created *store.AccessConstraint) *store.AccessConstraint {
	draft := *created
	draft.MaximumPermissions = []string{"agent.read"}
	draft.Purpose = "narrowed " + created.Purpose
	return &draft
}

func p6CommitUpdate(t *testing.T, ctx context.Context, gs *GovernanceService, ps *PreviewService, created, draft *store.AccessConstraint, actor PrincipalContext) (*CommitResult, *PreviewResult, error) {
	t.Helper()
	preview, err := ps.GeneratePreview(context.Background(), PreviewRequest{
		Operation:    "update",
		Draft:        draft,
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		Actor:        actor,
	})
	require.NoError(t, err)
	result, err := gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "update",
		Draft:        draft,
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		PreviewToken: preview.PreviewToken,
		PreviewID:    preview.PreviewID,
		DraftHash:    preview.DraftHash,
		Actor:        actor,
	})
	return result, preview, err
}

// p6AssertUnchanged asserts the constraint still has the given state and
// exactly the given number of history rows.
func p6AssertUnchanged(t *testing.T, s store.Store, want *store.AccessConstraint, wantRows int) {
	t.Helper()
	current, err := s.GetAccessConstraint(context.Background(), want.ID)
	require.NoError(t, err)
	assert.Equal(t, want.Revision, current.Revision, "revision must be unchanged")
	assert.ElementsMatch(t, want.MaximumPermissions, current.MaximumPermissions, "permissions must be unchanged")
	assert.Equal(t, want.Disabled, current.Disabled, "disabled flag must be unchanged")
	rows, err := s.ListConstraintHistory(context.Background(), want.ID)
	require.NoError(t, err)
	assert.Len(t, rows, wantRows, "history rows")
}

// p6UpdateFaultStore fails UpdateAccessConstraint inside the transaction.
type p6UpdateFaultStore struct {
	store.Store
	err error
}

func (s *p6UpdateFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&p6UpdateFaultStore{Store: tx, err: s.err})
	})
}

func (s *p6UpdateFaultStore) UpdateAccessConstraint(context.Context, *store.AccessConstraint, int64) (*store.AccessConstraint, error) {
	return nil, s.err
}

// ---------------------------------------------------------------------------
// T1: UPDATE writes its history row in the mutation's transaction
// ---------------------------------------------------------------------------

func TestGovernanceUpdateHistory_SuccessWritesOneRow(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	adminID, created := p6SeedUpdatableConstraint(t, gs, ps, s, "p6-update-success")
	capture := auditevent.NewCaptureSink()
	gs.auditSink = capture
	ctx := p6OperationContext(t, "request-p6-update")

	result, preview, err := p6CommitUpdate(t, ctx, gs, ps, created, p6NarrowedDraft(created), pvTestActor(adminID))
	require.NoError(t, err)
	require.NotNil(t, result.Constraint)
	p6RequireUUID(t, result.AuditID)
	assert.Equal(t, "update", result.Operation)
	assert.Equal(t, ClassificationTighten, result.Classification)

	stored, err := s.GetAccessConstraint(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.Revision+1, stored.Revision)
	assert.Equal(t, []string{"agent.read"}, stored.MaximumPermissions)

	rows, err := s.ListConstraintHistory(context.Background(), created.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2, "the CREATE row plus exactly one UPDATE row")
	var updateRows []*store.AccessConstraintHistory
	for _, row := range rows {
		if row.Operation == "update" {
			updateRows = append(updateRows, row)
		}
	}
	require.Len(t, updateRows, 1)
	row := updateRows[0]
	assert.Equal(t, result.AuditID, row.EventID, "auditId is the history row's event_id")
	assert.Equal(t, created.ID, row.ConstraintID)
	assert.Equal(t, ClassificationTighten, row.Classification)
	require.NotNil(t, row.BeforeRevision)
	require.NotNil(t, row.AfterRevision)
	assert.Equal(t, created.Revision, *row.BeforeRevision)
	assert.Equal(t, stored.Revision, *row.AfterRevision)
	assert.Equal(t, "request-p6-update", row.CorrelationID, "correlation comes from the audit operation")
	assert.Equal(t, "user", row.ActorKind)
	assert.Equal(t, adminID, row.ActorID)
	assert.Equal(t, preview.PreviewID, row.PreviewID)
	assert.Equal(t, preview.DraftHash, row.DraftHash)

	assert.Empty(t, capture.Records(), "UPDATE is history-only: nothing is emitted to a log sink")
}

func TestGovernanceUpdateHistory_StoreFaultLeavesNoMutationOrRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode string
	}{
		{name: "store-error", err: errors.New("injected update store failure")},
		{name: "revision-conflict", err: store.ErrRevisionConflict, wantCode: ErrCodeStaleAuthorizationPreview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs, ps, authz, realStore := govTestSetup(t)
			adminID, created := p6SeedUpdatableConstraint(t, gs, ps, realStore, "p6-update-store-fault-"+tc.name)
			faulty := NewGovernanceService(&p6UpdateFaultStore{Store: realStore, err: tc.err}, ps, authz, gs.logger)
			faulty.nowFunc = gs.nowFunc
			capture := auditevent.NewCaptureSink()
			faulty.auditSink = capture

			result, _, err := p6CommitUpdate(t, p6OperationContext(t, "request-p6-update-fault"), faulty, ps, created, p6NarrowedDraft(created), pvTestActor(adminID))
			require.Error(t, err)
			assert.Nil(t, result)
			if tc.wantCode != "" {
				var governanceErr *GovernanceError
				require.ErrorAs(t, err, &governanceErr)
				assert.Equal(t, tc.wantCode, governanceErr.Code)
			} else {
				require.ErrorIs(t, err, tc.err)
			}
			p6AssertUnchanged(t, realStore, created, 1)
			assert.Empty(t, capture.Records())
		})
	}
}

func TestGovernanceUpdateHistory_HistoryFailuresRollBack(t *testing.T) {
	for _, tc := range []struct {
		name        string
		afterAppend bool
	}{
		{name: "insert"},
		{name: "post-insert-prune", afterAppend: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs, ps, authz, realStore := govTestSetup(t)
			adminID, created := p6SeedUpdatableConstraint(t, gs, ps, realStore, "p6-update-history-fault-"+tc.name)
			injected := errors.New("injected " + tc.name + " failure")
			faulty := NewGovernanceService(&constraintHistoryFailureStore{Store: realStore, err: injected, afterAppend: tc.afterAppend}, ps, authz, gs.logger)
			faulty.nowFunc = gs.nowFunc
			capture := auditevent.NewCaptureSink()
			faulty.auditSink = capture

			result, _, err := p6CommitUpdate(t, p6OperationContext(t, "request-p6-update-history-fault"), faulty, ps, created, p6NarrowedDraft(created), pvTestActor(adminID))
			require.ErrorIs(t, err, injected)
			assert.Nil(t, result)
			p6AssertUnchanged(t, realStore, created, 1)
			assert.Empty(t, capture.Records())
		})
	}
}

func TestGovernanceUpdateHistory_BuilderFailureRollsBack(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	adminID, created := p6SeedUpdatableConstraint(t, gs, ps, s, "p6-update-builder-fault")
	capture := auditevent.NewCaptureSink()
	gs.auditSink = capture
	// A malformed trusted correlation fails envelope validation inside the
	// update transaction.
	ctx := auditevent.ContextWithOperation(context.Background(), auditevent.AuditOperationContext{CorrelationID: "bad\ncorrelation"})

	result, _, err := p6CommitUpdate(t, ctx, gs, ps, created, p6NarrowedDraft(created), pvTestActor(adminID))
	require.Error(t, err)
	assert.Nil(t, result)
	var validationErr *auditevent.ValidationError
	require.ErrorAs(t, err, &validationErr)
	p6AssertUnchanged(t, s, created, 1)
	assert.Empty(t, capture.Records())
}

// ---------------------------------------------------------------------------
// T2: history written by one Server is read by a fresh Server
// ---------------------------------------------------------------------------

func p6FileBackedBoundaryServer(t *testing.T, dsn string) (*Server, store.Store) {
	t.Helper()
	st, err := newTestStoreAt(t, dsn)
	require.NoError(t, err)
	srv, s := testServerWithStore(t, st)
	if srv.previewService == nil {
		srv.initBoundaryServices()
	}
	require.NotNil(t, srv.governanceService)
	return srv, s
}

func TestGovernanceUpdateHistory_RowsReadableByFreshServer(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "p6-history.db") + "?_pragma=busy_timeout(10000)"
	first, firstStore := p6FileBackedBoundaryServer(t, dsn)
	rd := createTestRoleDefinition(t, firstStore, "p6-restart-boundary-admin", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, PermissionConstraintRead, "agent.read", "agent.create", "agent.delete", "project.read"})
	pvSeedRoleBinding(t, firstStore, rd.ID, "user", DevUserID, store.RoleScopeSystem, "")
	targetID := pvSeedUser(t, firstStore, "p6-restart-target")

	createCtx := p6OperationContext(t, "request-audit-create")
	createResult, err := commitAuditedConstraint(t, createCtx, first.governanceService, first.previewService, &store.AccessConstraint{
		Name:                 "p6-restart-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   &targetID,
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create"},
		Purpose:              "p6 restart",
		CreatedBy:            DevUserID,
	}, pvTestActor(DevUserID))
	require.NoError(t, err)
	created := createResult.Constraint
	updateResult, _, err := p6CommitUpdate(t, p6OperationContext(t, "request-p6-restart-update"),
		first.governanceService, first.previewService, created, p6NarrowedDraft(created), pvTestActor(DevUserID))
	require.NoError(t, err)

	// A second Server on the same database has no process memory of the
	// first; it can only answer from the persisted history table.
	second, _ := p6FileBackedBoundaryServer(t, dsn)
	require.NotSame(t, first, second)
	response := doRequest(t, second, http.MethodGet,
		"/api/v1/admin/access-constraints/"+created.ID+"/audit", nil)
	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
	page := decodeAuditPage(t, response.Body.Bytes())
	require.Len(t, page.Items, 2)
	byID := map[string]auditEventResponse{}
	for _, item := range page.Items {
		byID[item.ID] = item
	}
	updateItem, ok := byID[updateResult.AuditID]
	require.True(t, ok, "the fresh server lists the UPDATE row by its auditId")
	assert.Equal(t, "update", updateItem.Operation)
	assert.Equal(t, "request-p6-restart-update", updateItem.CorrelationID)
	assert.Equal(t, created.Revision, mustParseRevision(t, updateItem.BeforeRevision))
	assert.Equal(t, updateResult.Constraint.Revision, mustParseRevision(t, updateItem.AfterRevision))
	createItem, ok := byID[createResult.AuditID]
	require.True(t, ok)
	assert.Equal(t, "create", createItem.Operation)
}
