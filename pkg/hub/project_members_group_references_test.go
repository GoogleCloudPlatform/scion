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

package hub

// Project members groups are system-managed: they are not valid role-binding
// principals or child groups. The store refuses new writes of either kind;
// these tests pin the startup pass that removes existing references.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// membersGroupRefsFixture holds the groups, bindings and references seeded by
// setupMembersGroupRefsFixture.
type membersGroupRefsFixture struct {
	s store.Store

	canonical *store.Group // members group, canonical marker key, project X
	legacy    *store.Group // members group, legacy marker key only, project Y
	unrelated *store.Group // ordinary project-scoped group, no marker
	parent    *store.Group // direct parent of canonical, legacy and unrelated
	ancestor  *store.Group // parent of parent (transitive ancestor only)

	// bindings maps group ID to the IDs of the role bindings naming it.
	bindings map[string][]string

	constraintID  string
	entitlementID string
}

// captureWarnLogs routes the default slog logger to a buffer for the test.
func captureWarnLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// setupMembersGroupRefsFixture seeds, for a canonical-key members group and a
// legacy-key-only members group, role bindings in project X (project scope),
// in project Y (project scope) and at system scope, plus a child edge into
// another group. An unrelated unmarked group gets the same references.
//
// The store refuses role bindings and child edges that name a project members
// group, so each group is created unmarked, the references are written, and
// only then is the marker applied with s.UpdateGroup (the store does not guard
// marker annotations on update). This models rows written before the store
// guard existed.
func setupMembersGroupRefsFixture(t *testing.T) *membersGroupRefsFixture {
	t.Helper()
	_, s := testServer(t)
	ctx := context.Background()
	now := time.Now()

	creator := createStaleOwnerUser(t, s, tid("mgref-creator"), "mgref-creator@test.com")
	projectX := &store.Project{
		ID: tid("mgref-project-x"), Name: "MGRef X", Slug: "mgref-project-x",
		OwnerID: creator.ID, CreatedBy: creator.ID, Created: now, Updated: now,
	}
	projectY := &store.Project{
		ID: tid("mgref-project-y"), Name: "MGRef Y", Slug: "mgref-project-y",
		OwnerID: creator.ID, CreatedBy: creator.ID, Created: now, Updated: now,
	}
	require.NoError(t, s.CreateProject(ctx, projectX))
	require.NoError(t, s.CreateProject(ctx, projectY))

	newGroup := func(name, projectID string) *store.Group {
		g := &store.Group{
			ID: tid(name), Name: name, Slug: name,
			GroupType: store.GroupTypeExplicit, ProjectID: projectID,
		}
		require.NoError(t, s.CreateGroup(ctx, g))
		return g
	}
	f := &membersGroupRefsFixture{
		s:         s,
		canonical: newGroup("mgref-canonical", projectX.ID),
		legacy:    newGroup("mgref-legacy", projectY.ID),
		unrelated: newGroup("mgref-unrelated", projectX.ID),
		parent:    newGroup("mgref-parent", ""),
		ancestor:  newGroup("mgref-ancestor", ""),
		bindings:  map[string][]string{},
	}

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	viewerRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)

	addEdge := func(parentID, childID string) {
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: parentID, MemberType: store.GroupMemberTypeGroup, MemberID: childID,
			Role: store.GroupMemberRoleMember, AddedBy: creator.ID,
		}))
	}
	addEdge(f.ancestor.ID, f.parent.ID)

	for _, g := range []*store.Group{f.canonical, f.legacy, f.unrelated} {
		for _, b := range []struct{ rd, scopeType, scopeID string }{
			{memberRD.ID, store.RoleScopeProject, projectX.ID},
			{memberRD.ID, store.RoleScopeProject, projectY.ID},
			{viewerRD.ID, store.RoleScopeSystem, ""},
		} {
			rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: b.rd, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: g.ID,
				ScopeType: b.scopeType, ScopeID: b.scopeID, CreatedBy: creator.ID,
			})
			require.NoError(t, err)
			f.bindings[g.ID] = append(f.bindings[g.ID], rb.ID)
		}
		addEdge(f.parent.ID, g.ID)
	}

	// References that are logged only and left in place.
	constraint, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name: "mgref-constraint", SubjectKind: store.ConstraintSubjectGroupClosure,
		SubjectGroupID: &f.canonical.ID, ScopeType: "system",
		MaximumPermissions: []string{"agent.read"}, Purpose: "test", CreatedBy: creator.ID,
	})
	require.NoError(t, err)
	f.constraintID = constraint.ID
	limit, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		ID: tid("mgref-limit"), Name: "mgref-limit", ResourceType: "agent", Unit: "count",
		DefaultValue: 10, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	ent, err := s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		ID: tid("mgref-entitlement"), LimitDefinitionID: limit.ID,
		SubjectType: store.EntitlementSubjectGroup, SubjectID: f.legacy.ID,
		ScopeType: "system", Value: 5, CreatedBy: creator.ID, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	f.entitlementID = ent.ID

	// Mark the members groups only now, after their references exist.
	f.canonical.Annotations = map[string]string{store.AnnotationProjectMembersGroup: "true"}
	require.NoError(t, s.UpdateGroup(ctx, f.canonical))
	f.legacy.Annotations = map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"}
	require.NoError(t, s.UpdateGroup(ctx, f.legacy))

	for _, g := range []*store.Group{f.canonical, f.legacy} {
		stored, err := s.GetGroup(ctx, g.ID)
		require.NoError(t, err)
		require.True(t, store.IsProjectMembersGroup(stored), "precondition: %s must be marked", g.Slug)
	}
	return f
}

func (f *membersGroupRefsFixture) groupBindingIDs(t *testing.T, groupID string) []string {
	t.Helper()
	rbs, err := f.s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalGroup, groupID)
	require.NoError(t, err)
	ids := make([]string, 0, len(rbs))
	for _, rb := range rbs {
		ids = append(ids, rb.ID)
	}
	return ids
}

func (f *membersGroupRefsFixture) hasEdge(t *testing.T, parentID, childID string) bool {
	t.Helper()
	_, err := f.s.GetGroupMembership(context.Background(), parentID, store.GroupMemberTypeGroup, childID)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	require.NoError(t, err)
	return true
}

func (f *membersGroupRefsFixture) referenceAudits(t *testing.T) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: membersGroupReferenceRemovedOp, Limit: 1000,
	})
	require.NoError(t, err)
	return recs
}

// TestBackfillRoleBindings_RemovesProjectMembersGroupReferences pins the
// startup pass: role bindings at every scope and child edges naming a
// members group (either marker key) are removed with audit records and WARN
// logs, unrelated references and WARN-only references are left in place, and
// a second run is a no-op.
func TestBackfillRoleBindings_RemovesProjectMembersGroupReferences(t *testing.T) {
	f := setupMembersGroupRefsFixture(t)
	ctx := context.Background()
	logs := captureWarnLogs(t)

	require.NoError(t, BackfillRoleBindings(ctx, f.s))

	for _, g := range []*store.Group{f.canonical, f.legacy} {
		assert.Empty(t, f.groupBindingIDs(t, g.ID), "role bindings naming %s must be removed", g.Slug)
		assert.False(t, f.hasEdge(t, f.parent.ID, g.ID), "child edge naming %s must be removed", g.Slug)
	}
	assert.ElementsMatch(t, f.bindings[f.unrelated.ID], f.groupBindingIDs(t, f.unrelated.ID),
		"unrelated group's bindings are untouched")
	assert.True(t, f.hasEdge(t, f.parent.ID, f.unrelated.ID), "unrelated group's edge is untouched")
	assert.True(t, f.hasEdge(t, f.ancestor.ID, f.parent.ID), "edge between unmarked groups is untouched")

	// Audit records: one per removed binding (3 per group) and edge (1 per group).
	audits := f.referenceAudits(t)
	require.Len(t, audits, 8)
	byTarget := map[string]*store.MutationAuditRecord{}
	for _, a := range audits {
		assert.Equal(t, "system", a.ActorPrincipalKind)
		assert.Equal(t, store.SystemReconcileCreatedBy, a.ActorPrincipalID)
		byTarget[a.TargetID] = a
	}
	for _, g := range []*store.Group{f.canonical, f.legacy} {
		for _, id := range f.bindings[g.ID] {
			a, ok := byTarget[id]
			require.True(t, ok, "audit record for binding %s", id)
			assert.Equal(t, "role_binding", a.TargetType)
			assert.Contains(t, a.BeforeSummary, `"created_by":"`+tid("mgref-creator")+`"`)
			assert.Contains(t, a.BeforeSummary, `"scope_type"`)
			assert.Contains(t, a.BeforeSummary, `"role":"`)
		}
		a, ok := byTarget[f.parent.ID+"/"+g.ID]
		require.True(t, ok, "audit record for edge into %s", g.Slug)
		assert.Equal(t, "group_child_group", a.TargetType)
	}

	// WARN-only references are left in place.
	_, err := f.s.GetAccessConstraint(ctx, f.constraintID)
	assert.NoError(t, err, "access constraint is left in place")
	_, err = f.s.GetEntitlementBinding(ctx, f.entitlementID)
	assert.NoError(t, err, "entitlement binding is left in place")

	out := logs.String()
	assert.Equal(t, 6, strings.Count(out, "msg=\"removed role binding that names a project members group\""))
	assert.Equal(t, 2, strings.Count(out, "msg=\"removed child group edge that names a project members group\""))
	assert.Contains(t, out, "access_constraints=1")
	assert.Contains(t, out, "entitlement_bindings=1")

	// Second run: nothing to remove.
	logs.Reset()
	require.NoError(t, BackfillRoleBindings(ctx, f.s))
	assert.Len(t, f.referenceAudits(t), 8, "second run writes no audit records")
	assert.NotContains(t, logs.String(), "removed role binding")
	assert.NotContains(t, logs.String(), "removed child group edge")
	assert.ElementsMatch(t, f.bindings[f.unrelated.ID], f.groupBindingIDs(t, f.unrelated.ID))
}

// referenceFailingStore fails DeleteRoleBinding for one binding ID, including
// inside WithTx.
type referenceFailingStore struct {
	store.Store
	failBindingID string
}

func (r *referenceFailingStore) DeleteRoleBinding(ctx context.Context, id string) error {
	if id == r.failBindingID {
		return errors.New("injected DeleteRoleBinding failure")
	}
	return r.Store.DeleteRoleBinding(ctx, id)
}

func (r *referenceFailingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return r.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&referenceFailingStore{Store: tx, failBindingID: r.failBindingID})
	})
}

// TestBackfillRoleBindings_ProjectMembersGroupReferenceErrorIsSkipped pins
// that a per-item store error is logged, does not abort the other removals
// and does not fail startup.
func TestBackfillRoleBindings_ProjectMembersGroupReferenceErrorIsSkipped(t *testing.T) {
	f := setupMembersGroupRefsFixture(t)
	ctx := context.Background()
	logs := captureWarnLogs(t)

	failing := f.bindings[f.canonical.ID][0]
	require.NoError(t, BackfillRoleBindings(ctx, &referenceFailingStore{Store: f.s, failBindingID: failing}))

	assert.Equal(t, []string{failing}, f.groupBindingIDs(t, f.canonical.ID),
		"only the failing binding remains")
	assert.Empty(t, f.groupBindingIDs(t, f.legacy.ID))
	assert.False(t, f.hasEdge(t, f.parent.ID, f.canonical.ID))
	assert.False(t, f.hasEdge(t, f.parent.ID, f.legacy.ID))
	assert.Len(t, f.referenceAudits(t), 7, "no audit record for the failed removal")
	assert.Contains(t, logs.String(), "failed to remove role binding that names a project members group")
	assert.Contains(t, logs.String(), "injected DeleteRoleBinding failure")

	// The next startup removes the remaining binding.
	require.NoError(t, BackfillRoleBindings(ctx, f.s))
	assert.Empty(t, f.groupBindingIDs(t, f.canonical.ID))
}

// TestBackfillRoleBindings_ReferenceRemovalRunsWhenEarlierStepFails pins that
// the reference removal runs even when an earlier backfill step fails.
func TestBackfillRoleBindings_ReferenceRemovalRunsWhenEarlierStepFails(t *testing.T) {
	f := setupMembersGroupRefsFixture(t)
	ctx := context.Background()
	_ = captureWarnLogs(t)

	err := BackfillRoleBindings(ctx, &backfillFailingStore{Store: f.s, failListUsers: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected ListUsers failure")
	assert.NotContains(t, err.Error(), "remove project members group references")

	assert.Empty(t, f.groupBindingIDs(t, f.canonical.ID))
	assert.Empty(t, f.groupBindingIDs(t, f.legacy.ID))
	assert.False(t, f.hasEdge(t, f.parent.ID, f.canonical.ID))
}
