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
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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
	srv *Server
	s   store.Store

	// creator created project X and is a member of the canonical members
	// group. Project Y was created by another user, so the creator's only
	// path to project Y is the canonical group's role binding there.
	creator  *store.User
	projectY *store.Project

	canonical *store.Group // members group, canonical marker key, project X; also nested in itself
	legacy    *store.Group // members group, legacy marker key only, project Y
	unrelated *store.Group // ordinary project-scoped group, no marker
	parent    *store.Group // direct parent of canonical, legacy and unrelated
	ancestor  *store.Group // parent of parent (transitive ancestor only)

	// bindings maps group ID to the IDs of the role bindings naming it.
	bindings map[string][]string

	constraintIDs []string // group_closure and legacy principal/group rows naming legacy
	entitlementID string   // names canonical
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
// another group. An unrelated unmarked group gets the same references. The
// canonical group is also nested in itself, and the creator is a user member
// of it.
//
// The store refuses role bindings and child edges that name a project members
// group, so each group is created unmarked, the references are written, and
// only then is the marker applied with s.UpdateGroup (the store does not guard
// marker annotations on update). This models rows written before the store
// guard existed.
func setupMembersGroupRefsFixture(t *testing.T) *membersGroupRefsFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	now := time.Now()

	creator := createStaleOwnerUser(t, s, tid("mgref-creator"), "mgref-creator@test.com")
	otherOwner := createStaleOwnerUser(t, s, tid("mgref-other-owner"), "mgref-other-owner@test.com")
	projectX := &store.Project{
		ID: tid("mgref-project-x"), Name: "MGRef X", Slug: "mgref-project-x",
		OwnerID: creator.ID, CreatedBy: creator.ID, Created: now, Updated: now,
	}
	projectY := &store.Project{
		ID: tid("mgref-project-y"), Name: "MGRef Y", Slug: "mgref-project-y",
		OwnerID: otherOwner.ID, CreatedBy: otherOwner.ID, Created: now, Updated: now,
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
		srv:       srv,
		s:         s,
		creator:   creator,
		projectY:  projectY,
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
	addEdge(f.canonical.ID, f.canonical.ID)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: f.canonical.ID, MemberType: store.GroupMemberTypeUser, MemberID: creator.ID,
		Role: store.GroupMemberRoleMember, AddedBy: creator.ID,
	}))

	// References that are logged only and left in place. The constraints
	// name the legacy group, which has no members, so they do not limit the
	// creator's access checked by the tests.
	for _, name := range []string{"mgref-constraint", "mgref-constraint-legacy-form"} {
		constraint, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
			Name: name, SubjectKind: store.ConstraintSubjectGroupClosure,
			SubjectGroupID: &f.legacy.ID, ScopeType: "system",
			MaximumPermissions: []string{"agent.read"}, Purpose: "test", CreatedBy: creator.ID,
		})
		require.NoError(t, err)
		f.constraintIDs = append(f.constraintIDs, constraint.ID)
	}
	// The store no longer accepts an exact-group principal subject, so the
	// second row is rewritten to that legacy form directly.
	dbs, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB")
	res, err := dbs.DB().ExecContext(ctx,
		`UPDATE access_constraints SET subject_kind = 'principal', subject_principal_type = ?, `+
			`subject_principal_id = ?, subject_group_id = NULL WHERE name = ?`,
		store.ConstraintPrincipalTypeGroup, f.legacy.ID, "mgref-constraint-legacy-form") //nolint:staticcheck // SA1019: seeds a legacy row.
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	legacyForm, err := s.GetAccessConstraint(ctx, f.constraintIDs[1])
	require.NoError(t, err)
	require.Equal(t, store.ConstraintSubjectPrincipal, legacyForm.SubjectKind)
	require.NotNil(t, legacyForm.SubjectPrincipalID)
	require.Equal(t, f.legacy.ID, *legacyForm.SubjectPrincipalID)
	limit, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		ID: tid("mgref-limit"), Name: "mgref-limit", ResourceType: "agent", Unit: "count",
		DefaultValue: 10, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	ent, err := s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		ID: tid("mgref-entitlement"), LimitDefinitionID: limit.ID,
		SubjectType: store.EntitlementSubjectGroup, SubjectID: f.canonical.ID,
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

// getProjectY returns the HTTP status of GET project Y as the creator.
func (f *membersGroupRefsFixture) getProjectY(t *testing.T) int {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.creator, http.MethodGet, "/api/v1/projects/"+f.projectY.ID, nil)
	return rec.Code
}

// edgeAudits returns the child-group edge audit records keyed by
// "<parent>/<child>" from their summaries.
func edgeAudits(t *testing.T, audits []*store.MutationAuditRecord) map[string]*store.MutationAuditRecord {
	t.Helper()
	out := map[string]*store.MutationAuditRecord{}
	for _, a := range audits {
		if a.TargetType != "group_membership" {
			continue
		}
		var summary map[string]string
		require.NoError(t, json.Unmarshal([]byte(a.BeforeSummary), &summary))
		assert.Equal(t, a.TargetID, summary["groupId"], "edge audit target is the parent group")
		assert.Equal(t, store.GroupMemberTypeGroup, summary["memberType"])
		out[summary["groupId"]+"/"+summary["memberId"]] = a
	}
	return out
}

// TestBackfillRoleBindings_RemovesProjectMembersGroupReferences pins the
// startup pass: role bindings at every scope and child edges naming a
// members group (either marker key, self-edge included) are removed with
// audit records and WARN logs, unrelated references and WARN-only references
// are left in place, access through the group goes away, and a second run is
// a no-op.
func TestBackfillRoleBindings_RemovesProjectMembersGroupReferences(t *testing.T) {
	f := setupMembersGroupRefsFixture(t)
	ctx := context.Background()
	logs := captureWarnLogs(t)

	require.Equal(t, http.StatusOK, f.getProjectY(t),
		"precondition: the creator reads project Y through the canonical members group")

	require.NoError(t, BackfillRoleBindings(ctx, f.s))

	assert.Equal(t, http.StatusNotFound, f.getProjectY(t),
		"after the backfill the creator has no access to project Y")

	for _, g := range []*store.Group{f.canonical, f.legacy} {
		assert.Empty(t, f.groupBindingIDs(t, g.ID), "role bindings naming %s must be removed", g.Slug)
		assert.False(t, f.hasEdge(t, f.parent.ID, g.ID), "child edge naming %s must be removed", g.Slug)
	}
	assert.False(t, f.hasEdge(t, f.canonical.ID, f.canonical.ID), "self-edge must be removed")
	assert.ElementsMatch(t, f.bindings[f.unrelated.ID], f.groupBindingIDs(t, f.unrelated.ID),
		"unrelated group's bindings are untouched")
	assert.True(t, f.hasEdge(t, f.parent.ID, f.unrelated.ID), "unrelated group's edge is untouched")
	assert.True(t, f.hasEdge(t, f.ancestor.ID, f.parent.ID), "edge between unmarked groups is untouched")

	// Audit records: one per removed binding (3 per group) and edge
	// (parent and self for canonical, parent for legacy). The transitive
	// ancestor gets none.
	audits := f.referenceAudits(t)
	require.Len(t, audits, 9)
	byTarget := map[string]*store.MutationAuditRecord{}
	for _, a := range audits {
		assert.Equal(t, "system", a.ActorPrincipalKind)
		assert.Equal(t, store.SystemReconcileCreatedBy, a.ActorPrincipalID)
		if a.TargetType == "role_binding" {
			byTarget[a.TargetID] = a
		}
	}
	for _, g := range []*store.Group{f.canonical, f.legacy} {
		for _, id := range f.bindings[g.ID] {
			a, ok := byTarget[id]
			require.True(t, ok, "audit record for binding %s", id)
			assert.Contains(t, a.BeforeSummary, `"created_by":"`+tid("mgref-creator")+`"`)
			assert.Contains(t, a.BeforeSummary, `"scope_type"`)
			assert.Contains(t, a.BeforeSummary, `"role":"`)
		}
	}
	edges := edgeAudits(t, audits)
	assert.Len(t, edges, 3)
	for _, key := range []string{
		f.parent.ID + "/" + f.canonical.ID,
		f.canonical.ID + "/" + f.canonical.ID,
		f.parent.ID + "/" + f.legacy.ID,
	} {
		a, ok := edges[key]
		require.True(t, ok, "audit record for edge %s", key)
		assert.NotContains(t, a.BeforeSummary, "member_role")
		assert.NotContains(t, a.BeforeSummary, "added_by")
	}

	// WARN-only references are left in place.
	for _, id := range f.constraintIDs {
		_, err := f.s.GetAccessConstraint(ctx, id)
		assert.NoError(t, err, "access constraint is left in place")
	}
	_, err := f.s.GetEntitlementBinding(ctx, f.entitlementID)
	assert.NoError(t, err, "entitlement binding is left in place")

	out := logs.String()
	assert.Equal(t, 6, strings.Count(out, "msg=\"removed role binding that names a project members group\""))
	assert.Equal(t, 3, strings.Count(out, "msg=\"removed child group edge that names a project members group\""))
	assert.NotContains(t, out, "member_role=")
	assert.NotContains(t, out, "added_by=")
	// Both constraint forms (group_closure and the legacy principal subject
	// of type group) are counted for the legacy group.
	assert.Contains(t, out, "group_id="+f.legacy.ID+" access_constraints=2 entitlement_bindings=0")
	assert.Contains(t, out, "group_id="+f.canonical.ID+" access_constraints=0 entitlement_bindings=1")

	// Second run: nothing to remove.
	logs.Reset()
	require.NoError(t, BackfillRoleBindings(ctx, f.s))
	assert.Len(t, f.referenceAudits(t), 9, "second run writes no audit records")
	assert.NotContains(t, logs.String(), "removed role binding")
	assert.NotContains(t, logs.String(), "removed child group edge")
	assert.ElementsMatch(t, f.bindings[f.unrelated.ID], f.groupBindingIDs(t, f.unrelated.ID))
}

// TestBackfillRoleBindings_ProjectMembersGroupReferencesPaginates pins the
// group listing and access constraint scan loops: with a page size of 1,
// members groups past the first page are still cleaned and constraints past
// the first page are still counted.
func TestBackfillRoleBindings_ProjectMembersGroupReferencesPaginates(t *testing.T) {
	f := setupMembersGroupRefsFixture(t)
	ctx := context.Background()
	s := f.s
	logs := captureWarnLogs(t)

	orig := projectMembersGroupOwnerBackfillPageSize
	projectMembersGroupOwnerBackfillPageSize = 1
	t.Cleanup(func() { projectMembersGroupOwnerBackfillPageSize = orig })

	// A members group created last, so it sorts onto a later page, with a
	// role binding and a child edge.
	late := &store.Group{
		ID: tid("mgref-late"), Name: "mgref-late", Slug: "mgref-late",
		GroupType: store.GroupTypeExplicit, ProjectID: f.projectY.ID, Created: time.Now().Add(time.Hour),
	}
	require.NoError(t, s.CreateGroup(ctx, late))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: late.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectY.ID, CreatedBy: f.creator.ID,
	})
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: f.parent.ID, MemberType: store.GroupMemberTypeGroup, MemberID: late.ID,
		Role: store.GroupMemberRoleMember,
	}))
	late.Annotations = map[string]string{store.AnnotationProjectMembersGroup: "true"}
	require.NoError(t, s.UpdateGroup(ctx, late))

	// Precondition: more than one page, and the late group is not on page 1.
	first, err := s.ListGroups(ctx, store.GroupFilter{}, store.ListOptions{
		Limit: projectMembersGroupOwnerBackfillPageSize, SkipTotalCount: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, first.NextCursor, "precondition: more than one page")
	for _, item := range first.Items {
		require.NotEqual(t, late.ID, item.ID, "precondition: late group must not be on page 1")
	}

	require.NoError(t, BackfillRoleBindings(ctx, s))

	for _, g := range []*store.Group{f.canonical, f.legacy, late} {
		assert.Empty(t, f.groupBindingIDs(t, g.ID), "role bindings naming %s must be removed", g.Slug)
		assert.False(t, f.hasEdge(t, f.parent.ID, g.ID), "child edge naming %s must be removed", g.Slug)
	}
	assert.Len(t, f.referenceAudits(t), 11)
	assert.Contains(t, logs.String(), "group_id="+f.legacy.ID+" access_constraints=2",
		"constraints past the first page are counted")
}

// listGroupsFailAfterMarkedStore fails every ListGroups call after the first
// page that contains a project members group.
type listGroupsFailAfterMarkedStore struct {
	store.Store
	firstMarkedID string // the members group on the last page returned
	calls         int
}

func (l *listGroupsFailAfterMarkedStore) ListGroups(ctx context.Context, f store.GroupFilter, o store.ListOptions) (*store.ListResult[store.Group], error) {
	l.calls++
	if l.firstMarkedID != "" {
		return nil, errors.New("injected ListGroups page failure")
	}
	page, err := l.Store.ListGroups(ctx, f, o)
	if err != nil {
		return nil, err
	}
	for i := range page.Items {
		if store.IsProjectMembersGroup(&page.Items[i]) {
			l.firstMarkedID = page.Items[i].ID
			break
		}
	}
	return page, nil
}

// TestBackfillRoleBindings_ProjectMembersGroupListingFailsPartWay pins that a
// ListGroups error on a later page does not discard the members groups
// already listed: their owner is cleared and their references are removed,
// the members groups on the unread pages are left for the next startup, and
// the listing error is still returned.
func TestBackfillRoleBindings_ProjectMembersGroupListingFailsPartWay(t *testing.T) {
	f := setupMembersGroupRefsFixture(t)
	ctx := context.Background()
	s := f.s
	_ = captureWarnLogs(t)

	orig := projectMembersGroupOwnerBackfillPageSize
	projectMembersGroupOwnerBackfillPageSize = 1
	t.Cleanup(func() { projectMembersGroupOwnerBackfillPageSize = orig })

	for _, g := range []*store.Group{f.canonical, f.legacy} {
		stored, err := s.GetGroup(ctx, g.ID)
		require.NoError(t, err)
		stored.OwnerID = f.creator.ID
		require.NoError(t, s.UpdateGroup(ctx, stored))
	}

	ls := &listGroupsFailAfterMarkedStore{Store: s}
	err := BackfillRoleBindings(ctx, ls)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list project members groups")
	assert.Contains(t, err.Error(), "injected ListGroups page failure")
	require.NotEmpty(t, ls.firstMarkedID, "precondition: a members group was listed")
	require.Greater(t, ls.calls, 1, "precondition: the listing failed on a later page")

	listed, unread := f.canonical, f.legacy
	if ls.firstMarkedID == f.legacy.ID {
		listed, unread = f.legacy, f.canonical
	}
	require.Equal(t, listed.ID, ls.firstMarkedID)

	stored, err := s.GetGroup(ctx, listed.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.OwnerID, "owner of the listed members group is cleared")
	assert.Empty(t, f.groupBindingIDs(t, listed.ID), "role bindings naming the listed members group are removed")
	assert.False(t, f.hasEdge(t, f.parent.ID, listed.ID), "child edge naming the listed members group is removed")

	stored, err = s.GetGroup(ctx, unread.ID)
	require.NoError(t, err)
	assert.Equal(t, f.creator.ID, stored.OwnerID, "members group on an unread page is left for the next startup")
	assert.ElementsMatch(t, f.bindings[unread.ID], f.groupBindingIDs(t, unread.ID))
	assert.True(t, f.hasEdge(t, f.parent.ID, unread.ID))

	// The next startup, with a working listing, handles the rest.
	require.NoError(t, BackfillRoleBindings(ctx, s))
	stored, err = s.GetGroup(ctx, unread.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.OwnerID)
	assert.Empty(t, f.groupBindingIDs(t, unread.ID))
	assert.False(t, f.hasEdge(t, f.parent.ID, unread.ID))
}

// concurrentRemovalStore removes one row after it has been listed and before
// its removal transaction runs, as a second replica running the same pass
// would.
type concurrentRemovalStore struct {
	store.Store
	bindingGroupID string // remove the first binding listed for this group
	edgeParentID   string // remove this parent's edge to edgeChildID once listed
	edgeChildID    string
	done           bool
}

func (c *concurrentRemovalStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	rbs, err := c.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
	if err == nil && !c.done && principalType == store.RoleBindingPrincipalGroup &&
		principalID == c.bindingGroupID && len(rbs) > 0 {
		if err := c.DeleteRoleBinding(ctx, rbs[0].ID); err != nil {
			return nil, err
		}
		c.done = true
	}
	return rbs, err
}

func (c *concurrentRemovalStore) GetDirectParentGroupIDs(ctx context.Context, groupID string) ([]string, error) {
	ids, err := c.Store.GetDirectParentGroupIDs(ctx, groupID)
	if err == nil && !c.done && groupID == c.edgeChildID {
		if err := c.RemoveGroupMember(ctx, c.edgeParentID, store.GroupMemberTypeGroup, c.edgeChildID); err != nil {
			return nil, err
		}
		c.done = true
	}
	return ids, err
}

// TestBackfillRoleBindings_ProjectMembersGroupBindingAlreadyRemoved pins
// that a role binding removed between the listing and its transaction is
// skipped quietly: no failure WARN, no audit record, not counted.
func TestBackfillRoleBindings_ProjectMembersGroupBindingAlreadyRemoved(t *testing.T) {
	f := setupMembersGroupRefsFixture(t)
	ctx := context.Background()
	logs := captureWarnLogs(t)

	cs := &concurrentRemovalStore{Store: f.s, bindingGroupID: f.canonical.ID}
	require.NoError(t, BackfillRoleBindings(ctx, cs))
	require.True(t, cs.done, "precondition: the binding was removed after listing")

	assert.Empty(t, f.groupBindingIDs(t, f.canonical.ID))
	assert.Len(t, f.referenceAudits(t), 8, "no audit record for the binding removed elsewhere")
	out := logs.String()
	assert.NotContains(t, out, "failed to remove role binding")
	assert.Equal(t, 5, strings.Count(out, "msg=\"removed role binding that names a project members group\""))
	assert.Contains(t, out, "role_bindings_removed=5")
}

// TestBackfillRoleBindings_ProjectMembersGroupEdgeAlreadyRemoved pins that a
// child-group edge removed between the listing and its transaction is
// skipped quietly: no WARN, no audit record, not counted.
func TestBackfillRoleBindings_ProjectMembersGroupEdgeAlreadyRemoved(t *testing.T) {
	f := setupMembersGroupRefsFixture(t)
	ctx := context.Background()
	logs := captureWarnLogs(t)

	cs := &concurrentRemovalStore{Store: f.s, edgeParentID: f.parent.ID, edgeChildID: f.canonical.ID}
	require.NoError(t, BackfillRoleBindings(ctx, cs))
	require.True(t, cs.done, "precondition: the edge was removed after listing")

	assert.False(t, f.hasEdge(t, f.parent.ID, f.canonical.ID))
	audits := f.referenceAudits(t)
	assert.Len(t, audits, 8, "no audit record for the edge removed elsewhere")
	_, ok := edgeAudits(t, audits)[f.parent.ID+"/"+f.canonical.ID]
	assert.False(t, ok)
	out := logs.String()
	assert.NotContains(t, out, "failed to remove child group edge")
	assert.Equal(t, 2, strings.Count(out, "msg=\"removed child group edge that names a project members group\""))
	assert.Contains(t, out, "child_group_edges_removed=2")
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
	assert.Len(t, f.referenceAudits(t), 8, "no audit record for the failed removal")
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
	assert.NotContains(t, err.Error(), "list project members groups")

	assert.Empty(t, f.groupBindingIDs(t, f.canonical.ID))
	assert.Empty(t, f.groupBindingIDs(t, f.legacy.ID))
	assert.False(t, f.hasEdge(t, f.parent.ID, f.canonical.ID))
}
