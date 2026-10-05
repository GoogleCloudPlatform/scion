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

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parentGroupFixture is a hub server with a parent group (created by the
// dev admin) and a spare group that can be offered as a group member.
type parentGroupFixture struct {
	srv    *Server
	store  store.Store
	parent *store.Group
	spare  *store.Group
}

func newParentGroupFixture(t *testing.T, name string) *parentGroupFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	mk := func(slug string) *store.Group {
		g := &store.Group{
			ID:        api.NewUUID(),
			Name:      slug,
			Slug:      slug,
			GroupType: store.GroupTypeExplicit,
			OwnerID:   DevUserID,
			CreatedBy: DevUserID,
		}
		require.NoError(t, s.CreateGroup(ctx, g))
		return g
	}
	return &parentGroupFixture{
		srv:    srv,
		store:  s,
		parent: mk(name + "-parent"),
		spare:  mk(name + "-spare"),
	}
}

// newSystemRoleUser creates a member user holding a custom system-scope role
// with exactly the given permissions.
func (f *parentGroupFixture) newSystemRoleUser(t *testing.T, name string, perms []string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          tid(name),
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, u))
	ensureHubMembership(ctx, f.store, u.ID)

	rd, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        name + "-role",
		Description: "test role",
		ScopeType:   store.RoleScopeSystem,
		Permissions: perms,
	})
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      u.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return u
}

func (f *parentGroupFixture) addMember(t *testing.T, groupID, userID, role string) {
	t.Helper()
	require.NoError(t, f.store.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       role,
	}))
}

// bindHubAdminToParent gives the parent group a system-scope hub-admin
// binding, so a membership of the parent carries authority the actor lacks.
func (f *parentGroupFixture) bindHubAdminToParent(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      f.parent.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

func (f *parentGroupFixture) createChild(t *testing.T, actor *store.User, slug, parentID string) (int, string) {
	t.Helper()
	body := map[string]interface{}{"name": slug, "slug": slug}
	if parentID != "" {
		body["parentId"] = parentID
	}
	rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, "/api/v1/groups", body)
	return rec.Code, rec.Body.String()
}

// addSpareAsMember adds the spare group to the parent through the members
// endpoint: the reference response for the same caller and parent.
func (f *parentGroupFixture) addSpareAsMember(t *testing.T, actor *store.User) (int, string) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, "/api/v1/groups/"+f.parent.ID+"/members",
		map[string]interface{}{"memberType": store.GroupMemberTypeGroup, "memberId": f.spare.ID})
	return rec.Code, rec.Body.String()
}

func apiErrorCode(t *testing.T, body string) string {
	t.Helper()
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(body), &errResp), "body: %s", body)
	return errResp.Error.Code
}

// assertNothingCreated checks that a refused create left no group, no
// creator membership and no child edge under the parent.
func (f *parentGroupFixture) assertNothingCreated(t *testing.T, actor *store.User, slug string) {
	t.Helper()
	ctx := context.Background()
	_, err := f.store.GetGroupBySlug(ctx, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "refused create must not create the group")

	children, err := f.store.ListGroups(ctx, store.GroupFilter{ParentID: f.parent.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, children.Items, "refused create must not add a child under the parent")

	memberships, err := f.store.GetUserGroups(ctx, actor.ID)
	require.NoError(t, err)
	for _, m := range memberships {
		assert.NotEqual(t, store.GroupMemberRoleOwner, m.Role,
			"refused create must not add the caller as owner of any group (group %s)", m.GroupID)
	}
}

// assertRefusedLikeAddMember runs a create with parentId that must be refused with the same
// status and error code as adding a group member to that parent.
func (f *parentGroupFixture) assertRefusedLikeAddMember(t *testing.T, actor *store.User, slug, wantMsg string) {
	t.Helper()
	code, body := f.createChild(t, actor, slug, f.parent.ID)
	assert.Equal(t, http.StatusForbidden, code, "create with parentId: %s", body)
	assert.Contains(t, body, wantMsg)
	f.assertNothingCreated(t, actor, slug)

	refCode, refBody := f.addSpareAsMember(t, actor)
	assert.Equal(t, refCode, code, "status must match addGroupMember (ref body: %s)", refBody)
	assert.Equal(t, apiErrorCode(t, refBody), apiErrorCode(t, body), "error code must match addGroupMember")
	assert.Contains(t, refBody, wantMsg, "addGroupMember refuses for the same reason")
}

// TestCreateGroup_ParentID_RequiresAddMemberOnParent: a caller with only
// group.create cannot create a group under a parent they cannot add members
// to.
func TestCreateGroup_ParentID_RequiresAddMemberOnParent(t *testing.T) {
	f := newParentGroupFixture(t, "cg-addmember")
	actor := f.newSystemRoleUser(t, "cg-create-only", []string{"group.create"})

	f.assertRefusedLikeAddMember(t, actor, "cg-addmember-child", `"denied_action":"addMember"`)
}

// TestCreateGroup_ParentID_AppliesRoleHierarchy: a caller holding
// group.create and group.addMember through a custom role, but who is neither
// owner nor admin of the parent, is refused by the group role hierarchy.
func TestCreateGroup_ParentID_AppliesRoleHierarchy(t *testing.T) {
	f := newParentGroupFixture(t, "cg-hierarchy")
	actor := f.newSystemRoleUser(t, "cg-create-add", []string{"group.create", "group.addMember"})

	f.assertRefusedLikeAddMember(t, actor, "cg-hierarchy-child", "Only group owners or admins can add members")
}

// TestCreateGroup_ParentID_AppliesCanDelegate: the owner of the parent group
// who is not a member of it, and so lacks the authority bound to the parent,
// cannot create a group under it.
func TestCreateGroup_ParentID_AppliesCanDelegate(t *testing.T) {
	f := newParentGroupFixture(t, "cg-delegate")
	actor := f.newSystemRoleUser(t, "cg-delegate-actor", []string{"group.create"})
	f.parent.OwnerID = actor.ID
	require.NoError(t, f.store.UpdateGroup(context.Background(), f.parent))
	f.bindHubAdminToParent(t)

	f.assertRefusedLikeAddMember(t, actor, "cg-delegate-child", "Cannot grant authority you do not hold")
}

// TestCreateGroup_NoParentID_Unchanged: without parentId, group.create alone
// is enough, as before.
func TestCreateGroup_NoParentID_Unchanged(t *testing.T) {
	f := newParentGroupFixture(t, "cg-noparent")
	actor := f.newSystemRoleUser(t, "cg-noparent-actor", []string{"group.create"})

	code, body := f.createChild(t, actor, "cg-noparent-group", "")
	require.Equal(t, http.StatusCreated, code, body)

	g, err := f.store.GetGroupBySlug(context.Background(), "cg-noparent-group")
	require.NoError(t, err)
	m, err := f.store.GetGroupMembership(context.Background(), g.ID, store.GroupMemberTypeUser, actor.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberRoleOwner, m.Role)
}

// TestCreateGroup_ParentID_UnknownParent: a parentId that names no group is
// a validation error and creates nothing, both for the dev admin and for a
// caller holding only group.create.
func TestCreateGroup_ParentID_UnknownParent(t *testing.T) {
	f := newParentGroupFixture(t, "cg-unknown")
	ctx := context.Background()

	listGroupIDs := func() []string {
		t.Helper()
		res, err := f.store.ListGroups(ctx, store.GroupFilter{}, store.ListOptions{Limit: 1000})
		require.NoError(t, err)
		ids := make([]string, 0, len(res.Items))
		for _, g := range res.Items {
			ids = append(ids, g.ID)
		}
		return ids
	}

	t.Run("dev admin", func(t *testing.T) {
		before := listGroupIDs()
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups",
			map[string]interface{}{"name": "cg-unknown-child", "slug": "cg-unknown-child", "parentId": api.NewUUID()})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeValidationError, apiErrorCode(t, rec.Body.String()))
		_, err := f.store.GetGroupBySlug(ctx, "cg-unknown-child")
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.ElementsMatch(t, before, listGroupIDs(), "no group may be created")
	})

	t.Run("create-only caller", func(t *testing.T) {
		actor := f.newSystemRoleUser(t, "cg-unknown-create-only", []string{"group.create"})
		before := listGroupIDs()
		code, body := f.createChild(t, actor, "cg-unknown-child-2", api.NewUUID())
		assert.Equal(t, http.StatusBadRequest, code, body)
		assert.Equal(t, ErrCodeValidationError, apiErrorCode(t, body))
		assert.ElementsMatch(t, before, listGroupIDs(), "no group may be created")
		f.assertNothingCreated(t, actor, "cg-unknown-child-2")
	})
}

// listAllGroupIDs returns the IDs of every group in the store.
func (f *parentGroupFixture) listAllGroupIDs(t *testing.T) []string {
	t.Helper()
	res, err := f.store.ListGroups(context.Background(), store.GroupFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	ids := make([]string, 0, len(res.Items))
	for _, g := range res.Items {
		ids = append(ids, g.ID)
	}
	return ids
}

// groupMembershipAudits returns the group_membership mutation audit records
// for groupID.
func (f *parentGroupFixture) groupMembershipAudits(t *testing.T, groupID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.store.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		TargetType: "group_membership", TargetID: groupID,
	})
	require.NoError(t, err)
	return recs
}

// setMaxMembersPerGroup sets the hub-wide max_members_per_group limit.
func (f *parentGroupFixture) setMaxMembersPerGroup(t *testing.T, limit int64) {
	t.Helper()
	ctx := context.Background()
	def, err := f.store.GetLimitDefinitionByName(ctx, store.LimitMaxMembersPerGroup)
	if errors.Is(err, store.ErrNotFound) {
		_, err = f.store.CreateLimitDefinition(ctx, &store.LimitDefinition{
			Name: store.LimitMaxMembersPerGroup, ResourceType: "group", Unit: "count", DefaultValue: limit,
		})
		require.NoError(t, err)
		return
	}
	require.NoError(t, err)
	def.DefaultValue = limit
	_, err = f.store.UpdateLimitDefinition(ctx, def)
	require.NoError(t, err)
}

// TestCreateGroup_ParentID_AllowedForParentAdmin: a caller who can add
// members to the parent (group.addMember and admin of the parent) can create
// a group under it, and the new parent membership is audited the same way
// addGroupMember audits one.
func TestCreateGroup_ParentID_AllowedForParentAdmin(t *testing.T) {
	f := newParentGroupFixture(t, "cg-allowed")
	actor := f.newSystemRoleUser(t, "cg-parent-admin", []string{"group.create", "group.addMember"})
	f.addMember(t, f.parent.ID, actor.ID, store.GroupMemberRoleAdmin)

	code, body := f.createChild(t, actor, "cg-allowed-child", f.parent.ID)
	require.Equal(t, http.StatusCreated, code, body)

	child, err := f.store.GetGroupBySlug(context.Background(), "cg-allowed-child")
	require.NoError(t, err)
	children, err := f.store.ListGroups(context.Background(), store.GroupFilter{ParentID: f.parent.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, children.Items, 1)
	assert.Equal(t, child.ID, children.Items[0].ID, "the new group is a child of the parent")

	var recs []*store.MutationAuditRecord
	require.Eventually(t, func() bool {
		recs = f.groupMembershipAudits(t, f.parent.ID)
		return len(recs) > 0
	}, 2*time.Second, 10*time.Millisecond, "create with parentId must write a group_membership audit record")
	time.Sleep(50 * time.Millisecond)
	recs = f.groupMembershipAudits(t, f.parent.ID)
	require.Len(t, recs, 1, "exactly one audit record for the parent membership")

	rec := recs[0]
	assert.Equal(t, "group_member_add", rec.MutationType)
	assert.Equal(t, "group_membership", rec.TargetType)
	assert.Equal(t, f.parent.ID, rec.TargetID)
	assert.JSONEq(t,
		`{"groupId":"`+f.parent.ID+`","memberType":"group","memberId":"`+child.ID+`","role":"member"}`,
		rec.AfterSummary)
	assert.Equal(t, "allow", rec.CanDelegateResult)
	assert.NotEmpty(t, rec.CanDelegateReason)
	assert.Equal(t, actor.ID, rec.ActorPrincipalID)

	// Reference: the same caller may add a group member to the parent, and
	// addGroupMember writes a record with the same shape.
	refCode, refBody := f.addSpareAsMember(t, actor)
	require.Equal(t, http.StatusCreated, refCode, refBody)
	var ref *store.MutationAuditRecord
	require.Eventually(t, func() bool {
		for _, r := range f.groupMembershipAudits(t, f.parent.ID) {
			if r.ID != rec.ID {
				ref = r
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, ref.MutationType, rec.MutationType)
	assert.Equal(t, ref.TargetType, rec.TargetType)
	assert.Equal(t, ref.CanDelegateResult, rec.CanDelegateResult)
	assert.Equal(t, ref.CanDelegateReason, rec.CanDelegateReason)
}

// inMemoryReservationStore is a test-only store wrapper that keeps usage
// reservations in memory, with advisory locks that always succeed. The SQLite
// test store cannot hold a reservation for a group scope yet
// (ptone/scion#3082), so the tests that need a parent group to actually reach
// its member limit, or need to observe a reservation being released, use this
// wrapper as the quota service's store.
//
// It models the reservation methods of entadapter.QuotaStore for any number
// of limits: an active reservation is identified by its limit definition ID
// and resource ID, HasActiveReservation and ReleaseReservation match on both,
// and CountActiveReservations filters on limit definition ID, subject ID,
// scope type and scope ID. Every other store method goes to the wrapped store.
type inMemoryReservationStore struct {
	store.Store
	mu       sync.Mutex
	active   map[string]store.UsageReservation // reservationKey -> reservation
	created  []string                          // resource IDs, in order
	released []string                          // resource IDs, in order
}

// reservationKey identifies an active reservation by limit and resource.
func reservationKey(limitDefinitionID, resourceID string) string {
	return limitDefinitionID + "\x00" + resourceID
}

func (m *inMemoryReservationStore) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

func (m *inMemoryReservationStore) TryAdvisoryLockObject(ctx context.Context, class store.AdvisoryLockKey, obj int32) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

func (m *inMemoryReservationStore) CreateUsageReservation(ctx context.Context, r *store.UsageReservation) (*store.UsageReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active[reservationKey(r.LimitDefinitionID, r.ResourceID)] = *r
	m.created = append(m.created, r.ResourceID)
	return r, nil
}

func (m *inMemoryReservationStore) CountActiveReservations(ctx context.Context, limitDefinitionID, subjectID, scopeType, scopeID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, r := range m.active {
		if r.LimitDefinitionID == limitDefinitionID && r.SubjectID == subjectID &&
			r.ScopeType == scopeType && r.ScopeID == scopeID {
			n++
		}
	}
	return n, nil
}

func (m *inMemoryReservationStore) HasActiveReservation(ctx context.Context, limitDefinitionID, resourceID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[reservationKey(limitDefinitionID, resourceID)]
	return ok, nil
}

func (m *inMemoryReservationStore) ReleaseReservation(ctx context.Context, limitDefinitionID, resourceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.released = append(m.released, resourceID)
	key := reservationKey(limitDefinitionID, resourceID)
	if _, ok := m.active[key]; !ok {
		return store.ErrNotFound
	}
	delete(m.active, key)
	return nil
}

// snapshot returns copies of the active reservations and of the resource IDs
// of the created and released reservations.
func (m *inMemoryReservationStore) snapshot() (active []store.UsageReservation, created, released []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.active {
		active = append(active, r)
	}
	return active, append([]string(nil), m.created...), append([]string(nil), m.released...)
}

// useInMemoryReservations sets the hub-wide max_members_per_group limit and
// points the quota service at an in-memory reservation store.
func (f *parentGroupFixture) useInMemoryReservations(t *testing.T, limit int64) *inMemoryReservationStore {
	t.Helper()
	f.setMaxMembersPerGroup(t, limit)
	mem := &inMemoryReservationStore{Store: f.store, active: map[string]store.UsageReservation{}}
	f.srv.quotaService = &QuotaService{store: mem, logger: slog.Default()}
	return mem
}

// TestCreateGroup_ParentID_EnforcesParentMemberLimit: a parent at its
// max_members_per_group limit refuses a new child with 429 quota_exceeded,
// the same way it refuses a new group member, and nothing is created.
func TestCreateGroup_ParentID_EnforcesParentMemberLimit(t *testing.T) {
	f := newParentGroupFixture(t, "cg-quota")
	ctx := context.Background()
	mem := f.useInMemoryReservations(t, 1)

	// Take the parent's only member slot through the members endpoint.
	fill := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups/"+f.parent.ID+"/members",
		map[string]interface{}{"memberType": store.GroupMemberTypeGroup, "memberId": f.spare.ID})
	require.Equal(t, http.StatusCreated, fill.Code, "filling the parent's member slot: %s", fill.Body.String())

	// Reference: adding another group member to the parent is refused.
	other := &store.Group{
		ID: api.NewUUID(), Name: "cg-quota-other", Slug: "cg-quota-other",
		GroupType: store.GroupTypeExplicit, OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, f.store.CreateGroup(ctx, other))
	refRec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups/"+f.parent.ID+"/members",
		map[string]interface{}{"memberType": store.GroupMemberTypeGroup, "memberId": other.ID})
	require.Equal(t, http.StatusTooManyRequests, refRec.Code, refRec.Body.String())
	require.Equal(t, ErrCodeQuotaExceeded, apiErrorCode(t, refRec.Body.String()))

	before := f.listAllGroupIDs(t)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups",
		map[string]interface{}{"name": "cg-quota-child", "slug": "cg-quota-child", "parentId": f.parent.ID})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeQuotaExceeded, apiErrorCode(t, rec.Body.String()))
	assert.ElementsMatch(t, before, f.listAllGroupIDs(t), "no group may be created")
	_, err := f.store.GetGroupBySlug(ctx, "cg-quota-child")
	assert.ErrorIs(t, err, store.ErrNotFound)

	active, _, _ := mem.snapshot()
	require.Len(t, active, 1, "only the filling member holds a reservation")
	assert.Equal(t, groupMembershipQuotaID(f.parent.ID, store.GroupMemberTypeGroup, f.spare.ID), active[0].ResourceID)
	assert.Equal(t, f.parent.ID, active[0].ScopeID)
}

// TestCreateGroup_ParentID_MemberLimitCheckMatchesAddMember: with the default
// test store, create with parentId applies the parent's member-limit check
// the same way addGroupMember does: same status and error code.
//
// The test compares against addGroupMember's live response rather than a
// fixed status: until ptone/scion#3082 is fixed, the reservation for a group
// scope fails in this store and both paths answer 500 runtime_error; once it
// is fixed, both answer 429 quota_exceeded.
func TestCreateGroup_ParentID_MemberLimitCheckMatchesAddMember(t *testing.T) {
	f := newParentGroupFixture(t, "cg-quota-real")
	ctx := context.Background()
	f.setMaxMembersPerGroup(t, 1)

	// Ask for the parent's only member slot through the members endpoint.
	// Under ptone/scion#3082 this request is refused like the ones below.
	fill := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups/"+f.parent.ID+"/members",
		map[string]interface{}{"memberType": store.GroupMemberTypeGroup, "memberId": f.spare.ID})
	t.Logf("filling the parent's member slot: %d %s", fill.Code, fill.Body.String())

	// Reference: adding another group member to the parent.
	other := &store.Group{
		ID: api.NewUUID(), Name: "cg-quota-real-other", Slug: "cg-quota-real-other",
		GroupType: store.GroupTypeExplicit, OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, f.store.CreateGroup(ctx, other))
	refRec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups/"+f.parent.ID+"/members",
		map[string]interface{}{"memberType": store.GroupMemberTypeGroup, "memberId": other.ID})
	require.GreaterOrEqual(t, refRec.Code, http.StatusBadRequest,
		"addGroupMember must refuse a member over the parent's limit: %s", refRec.Body.String())
	refCode := apiErrorCode(t, refRec.Body.String())

	before := f.listAllGroupIDs(t)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups",
		map[string]interface{}{"name": "cg-quota-real-child", "slug": "cg-quota-real-child", "parentId": f.parent.ID})
	assert.Equal(t, refRec.Code, rec.Code, "status must match addGroupMember (body: %s)", rec.Body.String())
	assert.Equal(t, refCode, apiErrorCode(t, rec.Body.String()), "error code must match addGroupMember")
	assert.ElementsMatch(t, before, f.listAllGroupIDs(t), "no group may be created")
	_, err := f.store.GetGroupBySlug(ctx, "cg-quota-real-child")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestCreateGroup_ParentID_ReleasesReservationOnCreateFailure: when the
// group cannot be created after the parent's member slot was reserved (here
// the slug is taken), the reservation is released.
func TestCreateGroup_ParentID_ReleasesReservationOnCreateFailure(t *testing.T) {
	f := newParentGroupFixture(t, "cg-release")
	mem := f.useInMemoryReservations(t, 5)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups",
		map[string]interface{}{"name": "cg-release-dup", "slug": f.spare.Slug, "parentId": f.parent.ID})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	active, created, released := mem.snapshot()
	require.Len(t, created, 1, "the parent's member slot is reserved before the create")
	assert.Equal(t, created, released, "the same reservation is released")
	assert.Empty(t, active, "no reservation is held after the failed create")
}

// TestCreateGroup_ParentID_SlugConflict_NoAudit: a create with parentId that
// fails because the slug is taken returns 409, creates nothing and writes no
// group_membership audit record for the parent.
func TestCreateGroup_ParentID_SlugConflict_NoAudit(t *testing.T) {
	f := newParentGroupFixture(t, "cg-dup")
	ctx := context.Background()

	before := f.listAllGroupIDs(t)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups",
		map[string]interface{}{"name": "cg-dup-child", "slug": f.spare.Slug, "parentId": f.parent.ID})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	assert.ElementsMatch(t, before, f.listAllGroupIDs(t), "no group may be created")
	children, err := f.store.ListGroups(ctx, store.GroupFilter{ParentID: f.parent.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, children.Items, "the parent has no child group")
	spare, err := f.store.GetGroup(ctx, f.spare.ID)
	require.NoError(t, err)
	assert.Empty(t, spare.ParentID, "the group holding the slug is unchanged")

	// Audit records are written asynchronously: give a record time to land.
	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, f.groupMembershipAudits(t, f.parent.ID), "a failed create writes no audit record")
}

// failingParentLookupStore fails GetGroup for one group ID with an error
// other than not-found.
type failingParentLookupStore struct {
	store.Store
	groupID string
}

var errParentLookup = errors.New("parent lookup unavailable")

func (f *failingParentLookupStore) GetGroup(ctx context.Context, id string) (*store.Group, error) {
	if id == f.groupID {
		return nil, errParentLookup
	}
	return f.Store.GetGroup(ctx, id)
}

// TestCreateGroup_ParentID_ParentLookupError: a store failure loading the
// parent returns an error and creates nothing.
func TestCreateGroup_ParentID_ParentLookupError(t *testing.T) {
	f := newParentGroupFixture(t, "cg-lookup")
	before := f.listAllGroupIDs(t)
	f.srv.store = &failingParentLookupStore{Store: f.store, groupID: f.parent.ID}

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups",
		map[string]interface{}{"name": "cg-lookup-child", "slug": "cg-lookup-child", "parentId": f.parent.ID})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), errParentLookup.Error())

	f.srv.store = f.store
	assert.ElementsMatch(t, before, f.listAllGroupIDs(t), "no group may be created")
	_, err := f.store.GetGroupBySlug(context.Background(), "cg-lookup-child")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestCreateGroup_ParentID_AgentCaller: an agent caller creating a group
// under a parent is held to the same checks as an agent adding a group
// member to that parent (agent tokens carry no group permissions, so both
// are refused, as in the addGroupMember agent role cap tests), and a refusal
// creates nothing.
func TestCreateGroup_ParentID_AgentCaller(t *testing.T) {
	type agentFixture struct {
		srv    *Server
		store  store.Store
		agent  *store.Agent
		parent *store.Group
		spare  *store.Group
	}
	setup := func(t *testing.T) *agentFixture {
		t.Helper()
		srv, s := testServer(t)
		ctx := context.Background()
		owner := &store.User{
			ID: tid("cg-agent-owner"), Email: "cg-agent-owner@test.com", DisplayName: "cg-agent-owner",
			Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, owner))
		proj := &store.Project{ID: tid("cg-agent-proj"), Name: "cg-agent-proj", Slug: "cg-agent-proj", OwnerID: owner.ID}
		require.NoError(t, s.CreateProject(ctx, proj))
		agent := &store.Agent{
			ID: tid("cg-agent-caller"), Slug: tid("cg-agent-caller"), Name: "cg-agent-caller",
			ProjectID: proj.ID, Phase: string(state.PhaseRunning),
			CreatedBy: owner.ID, OwnerID: owner.ID, Ancestry: []string{owner.ID},
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
		mk := func(slug string) *store.Group {
			g := &store.Group{
				ID: api.NewUUID(), Name: slug, Slug: slug, GroupType: store.GroupTypeExplicit,
				ProjectID: proj.ID, OwnerID: owner.ID,
			}
			require.NoError(t, s.CreateGroup(ctx, g))
			return g
		}
		return &agentFixture{srv: srv, store: s, agent: agent, parent: mk("cg-agent-parent"), spare: mk("cg-agent-spare")}
	}

	asAgent := func(t *testing.T, f *agentFixture, method, path string, body interface{}) *httptest.ResponseRecorder {
		t.Helper()
		svc := f.srv.GetAgentTokenService()
		require.NotNil(t, svc)
		tok, err := svc.GenerateAgentToken(f.agent.ID, f.agent.ProjectID, []AgentTokenScope{ScopeProjectRead}, nil)
		require.NoError(t, err)
		return doRequestWithAgentToken(t, f.srv, method, path, body, tok)
	}

	listIDs := func(t *testing.T, s store.Store) []string {
		t.Helper()
		res, err := s.ListGroups(context.Background(), store.GroupFilter{}, store.ListOptions{Limit: 1000})
		require.NoError(t, err)
		ids := make([]string, 0, len(res.Items))
		for _, g := range res.Items {
			ids = append(ids, g.ID)
		}
		return ids
	}

	assertRefused := func(t *testing.T, f *agentFixture) {
		t.Helper()
		before := listIDs(t, f.store)
		rec := asAgent(t, f, http.MethodPost, "/api/v1/groups",
			CreateGroupRequest{Name: "cg-agent-child", Slug: "cg-agent-child", ParentID: f.parent.ID})
		assert.Equal(t, http.StatusForbidden, rec.Code, "agent create with parentId: %s", rec.Body.String())
		assert.ElementsMatch(t, before, listIDs(t, f.store), "no group may be created")
		_, err := f.store.GetGroupBySlug(context.Background(), "cg-agent-child")
		assert.ErrorIs(t, err, store.ErrNotFound)

		refRec := asAgent(t, f, http.MethodPost, "/api/v1/groups/"+f.parent.ID+"/members",
			AddGroupMemberRequest{MemberType: store.GroupMemberTypeGroup, MemberID: f.spare.ID, Role: store.GroupMemberRoleMember})
		assert.Equal(t, refRec.Code, rec.Code, "status must match addGroupMember (ref body: %s)", refRec.Body.String())
		assert.Equal(t, apiErrorCode(t, refRec.Body.String()), apiErrorCode(t, rec.Body.String()))
	}

	t.Run("agent without role binding is refused", func(t *testing.T) {
		assertRefused(t, setup(t))
	})

	t.Run("agent with a group role binding is refused", func(t *testing.T) {
		f := setup(t)
		rd := createTestRoleDefinition(t, f.store, "cg-agent-group-role", store.RoleScopeSystem,
			[]string{"group.create", "group.addMember"})
		_, err := f.store.CreateRoleBinding(context.Background(), &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalAgent,
			PrincipalID:      f.agent.ID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
		assertRefused(t, f)
	})
}
