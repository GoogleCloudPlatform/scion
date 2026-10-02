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

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type postCommitAuditSink struct {
	store  store.Store
	err    error
	calls  int
	events []auditevent.EnvelopeV1
}

func (s *postCommitAuditSink) Emit(ctx context.Context, event auditevent.EnvelopeV1) error {
	s.calls++
	s.events = append(s.events, event)
	if event.Resource != nil {
		if _, err := s.store.GetAccessConstraint(ctx, event.Resource.ID); err != nil {
			return fmt.Errorf("live row not committed before sink: %w", err)
		}
		rows, err := s.store.ListConstraintHistory(ctx, event.Resource.ID)
		if err != nil {
			return fmt.Errorf("history not committed before sink: %w", err)
		}
		if len(rows) != 1 || rows[0].EventID != event.EventID {
			return fmt.Errorf("committed history does not match sink event")
		}
	}
	return s.err
}

type constraintHistoryFailureStore struct {
	store.Store
	err         error
	afterAppend bool
}

func (s *constraintHistoryFailureStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&constraintHistoryFailureStore{Store: tx, err: s.err, afterAppend: s.afterAppend})
	})
}

func (s *constraintHistoryFailureStore) AppendConstraintHistoryTx(ctx context.Context, entry *store.AccessConstraintHistory) error {
	if !s.afterAppend {
		return s.err
	}
	if err := s.Store.AppendConstraintHistoryTx(ctx, entry); err != nil {
		return err
	}
	return s.err
}

func commitAuditedConstraint(
	t *testing.T,
	ctx context.Context,
	gs *GovernanceService,
	ps *PreviewService,
	draft *store.AccessConstraint,
	actor PrincipalContext,
) (*CommitResult, error) {
	t.Helper()
	preview, err := ps.GeneratePreview(context.Background(), PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)
	return gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: preview.PreviewToken,
		PreviewID:    preview.PreviewID,
		DraftHash:    preview.DraftHash,
		Actor:        actor,
		AuditRequest: &auditevent.RequestRef{
			ID:      "request-audit-create",
			Method:  "POST",
			Route:   "/api/v1/admin/access-constraints",
			Surface: "api",
		},
	})
}

func TestGovernanceCreateAudit_OneEnvelopeTransactionAndScope(t *testing.T) {
	for _, tc := range []struct {
		name         string
		scopeType    string
		projectScope bool
	}{
		{name: "system", scopeType: store.RoleScopeSystem},
		{name: "project", scopeType: store.RoleScopeProject, projectScope: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs, ps, _, s := govTestSetup(t)
			adminID := govSeedAdminUser(t, s, "audit-envelope-admin-"+tc.name)
			draft := &store.AccessConstraint{
				Name:               "audit-envelope-" + tc.name,
				SubjectKind:        store.ConstraintSubjectAllPrincipals,
				ScopeType:          tc.scopeType,
				MaximumPermissions: []string{"agent.read", PermissionConstraintAdmin},
				Purpose:            "transactional audit envelope",
				CreatedBy:          adminID,
			}
			if tc.projectScope {
				draft.ScopeID = pvSeedProject(t, s, "audit-envelope-project")
				projectAdminRole := createTestRoleDefinition(t, s, "audit-envelope-project-admin", store.RoleScopeProject,
					[]string{PermissionConstraintAdmin, "agent.read"})
				pvSeedRoleBinding(t, s, projectAdminRole.ID, "user", adminID, store.RoleScopeProject, draft.ScopeID)
			}
			sink := &postCommitAuditSink{store: s}
			gs.auditSink = sink
			operation, err := auditevent.NewOperationContext("request-audit-create")
			require.NoError(t, err)
			ctx := auditevent.ContextWithOperation(context.Background(), operation)

			result, err := commitAuditedConstraint(t, ctx, gs, ps, draft, pvTestActor(adminID))
			require.NoError(t, err)
			require.NotNil(t, result.Constraint)
			require.Equal(t, 1, sink.calls)
			require.Len(t, sink.events, 1)
			event := sink.events[0]
			assert.Equal(t, result.AuditID, event.EventID)
			assert.Equal(t, result.Constraint.ID, event.Resource.ID)
			assert.Equal(t, "request-audit-create", event.CorrelationID)
			assert.Equal(t, tc.projectScope, event.Resource.ProjectID != "")
			if tc.projectScope {
				assert.Equal(t, auditevent.ResourceScopeProject, event.Resource.Scope)
				assert.Equal(t, draft.ScopeID, event.Resource.ProjectID)
			} else {
				assert.Equal(t, auditevent.ResourceScopeSystem, event.Resource.Scope)
				assert.Empty(t, event.Resource.ProjectID)
			}

			history, err := s.ListConstraintHistory(ctx, result.Constraint.ID)
			require.NoError(t, err)
			require.Len(t, history, 1)
			assert.Equal(t, event.EventID, history[0].EventID)
			assert.Equal(t, event.OccurredAt, history[0].OccurredAt)
			assert.Equal(t, event.CorrelationID, history[0].CorrelationID)
			assert.Equal(t, event.Principal.ID, history[0].ActorID)
			assert.Equal(t, result.Constraint.Revision, *history[0].AfterRevision)

			rendered, err := auditevent.Render(event)
			require.NoError(t, err)
			var record map[string]any
			require.NoError(t, json.Unmarshal(rendered, &record))
			resource := record["resource"].(map[string]any)
			_, hasProjectID := resource["project_id"]
			assert.Equal(t, tc.projectScope, hasProjectID)
		})
	}
}

func TestGovernanceCreateAudit_BuilderFailureRollsBack(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	adminID := govSeedAdminUser(t, s, "audit-builder-failure-admin")
	sink := &postCommitAuditSink{store: s}
	gs.auditSink = sink
	draft := &store.AccessConstraint{
		Name:               "audit-builder-failure",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read", PermissionConstraintAdmin},
		CreatedBy:          adminID,
	}
	ctx := auditevent.ContextWithOperation(context.Background(), auditevent.AuditOperationContext{CorrelationID: "bad\ncorrelation"})

	result, err := commitAuditedConstraint(t, ctx, gs, ps, draft, pvTestActor(adminID))
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Zero(t, sink.calls)
	constraints, listErr := s.ListAccessConstraints(context.Background(), 100, 0)
	require.NoError(t, listErr)
	for _, constraint := range constraints {
		assert.NotEqual(t, draft.Name, constraint.Name)
	}
}

func TestGovernanceCreateAudit_HistoryFailuresRollBack(t *testing.T) {
	for _, tc := range []struct {
		name        string
		afterAppend bool
	}{
		{name: "insert"},
		{name: "post-insert-prune", afterAppend: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs, ps, authz, realStore := govTestSetup(t)
			adminID := govSeedAdminUser(t, realStore, "audit-history-failure-admin-"+tc.name)
			injected := errors.New("injected " + tc.name + " failure")
			failingStore := &constraintHistoryFailureStore{Store: realStore, err: injected, afterAppend: tc.afterAppend}
			gs = NewGovernanceService(failingStore, ps, authz, gs.logger)
			sink := &postCommitAuditSink{store: realStore}
			gs.auditSink = sink
			draft := &store.AccessConstraint{
				Name:               "audit-history-failure-" + tc.name,
				SubjectKind:        store.ConstraintSubjectAllPrincipals,
				ScopeType:          store.RoleScopeSystem,
				MaximumPermissions: []string{"agent.read", PermissionConstraintAdmin},
				CreatedBy:          adminID,
			}
			operation, err := auditevent.NewOperationContext("request-audit-create")
			require.NoError(t, err)
			ctx := auditevent.ContextWithOperation(context.Background(), operation)

			result, err := commitAuditedConstraint(t, ctx, gs, ps, draft, pvTestActor(adminID))
			require.ErrorIs(t, err, injected)
			assert.Nil(t, result)
			assert.Zero(t, sink.calls)
			constraints, listErr := realStore.ListAccessConstraints(ctx, 100, 0)
			require.NoError(t, listErr)
			for _, constraint := range constraints {
				assert.NotEqual(t, draft.Name, constraint.Name)
			}
		})
	}
}

func TestGovernanceCreateAudit_SinkFailurePreservesCommit(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	adminID := govSeedAdminUser(t, s, "audit-sink-failure-admin")
	sinkFailure := errors.New("injected sink failure")
	sink := &postCommitAuditSink{store: s, err: sinkFailure}
	gs.auditSink = sink
	draft := &store.AccessConstraint{
		Name:               "audit-sink-failure",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read", PermissionConstraintAdmin},
		CreatedBy:          adminID,
	}
	operation, err := auditevent.NewOperationContext("request-audit-create")
	require.NoError(t, err)
	ctx := auditevent.ContextWithOperation(context.Background(), operation)

	result, err := commitAuditedConstraint(t, ctx, gs, ps, draft, pvTestActor(adminID))
	require.NoError(t, err)
	require.NotNil(t, result.Constraint)
	assert.Equal(t, 1, sink.calls)
	rows, err := s.ListConstraintHistory(ctx, result.Constraint.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, result.AuditID, rows[0].EventID)
}
