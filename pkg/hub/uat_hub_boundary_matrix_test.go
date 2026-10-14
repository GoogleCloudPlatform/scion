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
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubUAT_MemberDeletesOnlyAgentsInProjectsWithAccess pins that a
// project owner's hub token carrying agent:delete deletes an agent in the
// owner's project and is denied on an agent in a project the owner cannot
// access.
func TestHubUAT_MemberDeletesOnlyAgentsInProjectsWithAccess(t *testing.T) {
	f := newBearerFixture(t, "matrix-delete")
	ctx := context.Background()
	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.ownerA), CreateTokenParams{
		UserID: f.ownerA, Name: "matrix-delete", Boundary: hubBoundary(), Scopes: []string{"agent:delete"},
	})
	require.NoError(t, err)

	rec := doRequestWithUAT(t, f.srv, key, http.MethodDelete, "/api/v1/agents/"+f.agentB.ID, nil)
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
	_, err = f.store.GetAgent(ctx, f.agentB.ID)
	assert.NoError(t, err, "the agent outside the holder's access is not deleted")

	rec = doRequestWithUAT(t, f.srv, key, http.MethodDelete, "/api/v1/agents/"+f.agentA.ID, nil)
	assert.Less(t, rec.Code, 300, "delete in the holder's project: %d %s", rec.Code, rec.Body.String())
}

// TestHubUAT_UserWithoutProjectAccessCannotMintOrRead pins that a hub
// boundary grants nothing by itself: a user who is a hub member with no
// project access cannot mint a hub token carrying agent:read.
func TestHubUAT_UserWithoutProjectAccessCannotMintOrRead(t *testing.T) {
	f := newBearerFixture(t, "matrix-nonmember")
	ctx := context.Background()
	user := tid("matrix-nonmember-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: user, Email: user + "@test.com", DisplayName: "Non Member", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, user)

	_, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(user), CreateTokenParams{
		UserID: user, Name: "matrix-nonmember", Boundary: hubBoundary(), Scopes: []string{"agent:read"},
	})
	require.Error(t, err, "a user with no project access cannot mint a hub token for agent:read")
	assert.ErrorIs(t, err, ErrUATProjectForbidden)

	// The same holder's token, had it been issued, reaches no project
	// target: current access decides, not the boundary.
	token := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(user), hubBoundary(), []string{"agent:read"}, tid("matrix-nonmember-cred"), bearerCeiling(t, "agent:read"), nil)
	assert.False(t, f.srv.authzService.CheckAccess(ctx, token, agentResource(f.agentA), ActionRead).Allowed)
	result, err := f.srv.authzService.ResolveListScopes(ctx, token, "agent.list")
	require.NoError(t, err)
	assert.Empty(t, result.Scopes.ProjectIDs(), "a holder with no project access lists no project")
}

// TestHubUAT_GroupMembershipChangeRecheckedOnNextRequest pins that a hub
// token's project access follows the holder's group-derived membership on
// every request: access granted through a group admits the token, and
// removing the holder from the group denies the next request.
func TestHubUAT_GroupMembershipChangeRecheckedOnNextRequest(t *testing.T) {
	f := newBearerFixture(t, "matrix-group")
	ctx := context.Background()
	user := tid("matrix-group-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: user, Email: user + "@test.com", DisplayName: "Group User", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, user)

	groupID := tid("matrix-group")
	require.NoError(t, f.store.CreateGroup(ctx, &store.Group{ID: groupID, Name: "Matrix Group", Slug: "matrix-group"}))
	require.NoError(t, f.store.AddGroupMember(ctx, &store.GroupMember{GroupID: groupID, MemberType: "user", MemberID: user, Role: "member"}))
	memberRD, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectB,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(user), CreateTokenParams{
		UserID: user, Name: "matrix-group", Boundary: hubBoundary(), Scopes: []string{"agent:read"},
	})
	require.NoError(t, err, "group-derived project access lets the user mint a hub token")

	rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, "/api/v1/agents/"+f.agentB.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "group-derived access admits the token: %s", rec.Body.String())

	require.NoError(t, f.store.RemoveGroupMember(ctx, groupID, "user", user))
	rec = doRequestWithUAT(t, f.srv, key, http.MethodGet, "/api/v1/agents/"+f.agentB.ID, nil)
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, "leaving the group denies the next request: %s", rec.Body.String())
}

// TestHubUAT_RoleChangeRecheckedOnNextRequest pins that a hub token's
// authority follows the holder's current project role: a project admin's
// token runs lifecycle actions on another member's agent, and after the
// holder is demoted to member the same token is denied that action.
func TestHubUAT_RoleChangeRecheckedOnNextRequest(t *testing.T) {
	f := newBearerFixture(t, "matrix-role")
	ctx := context.Background()
	admin := tid("matrix-role-admin")
	createTestUserWithProjectRole(t, f.store, admin, admin+"@test.com", f.projectB, store.ProjectRoleAdmin)
	ensureHubMembership(ctx, f.store, admin)
	first := uatpAgent(t, f.store, f.projectB, f.ownerB, "matrix-role-1", f.ownerB)
	second := uatpAgent(t, f.store, f.projectB, f.ownerB, "matrix-role-2", f.ownerB)

	token := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(admin), hubBoundary(), []string{"agent:lifecycle"}, tid("matrix-role-cred"), bearerCeiling(t, "agent:lifecycle"), nil)
	d := f.srv.authzService.CheckAccess(ctx, token, agentResource(first), ActionLifecycle)
	require.True(t, d.Allowed, "a project admin's hub token may run lifecycle actions on another member's agent: %s", d.Reason)

	uatpDeleteProjectBinding(t, f.store, admin, f.projectB)
	uatpMember(t, f.store, f.projectB, admin)
	assert.False(t, f.srv.authzService.CheckAccess(ctx, token, agentResource(second), ActionLifecycle).Allowed,
		"after demotion to member the same token is denied the lifecycle action")
}

// TestCreateToken_HubBoundaryAuditFailureStoresNoToken pins that a hub
// token mint whose audit write fails stores no token row.
func TestCreateToken_HubBoundaryAuditFailureStoresNoToken(t *testing.T) {
	f := newBearerFixture(t, "matrix-audit-fail")
	ctx := context.Background()
	before, err := f.store.CountUserAccessTokens(ctx, f.ownerA)
	require.NoError(t, err)

	orig := f.srv.uatService.store
	f.srv.uatService.store = &rs4FailingStore{Store: f.store, createMutationAuditErr: errors.New("injected: audit write failure")}
	defer func() { f.srv.uatService.store = orig }()

	_, _, err = f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.ownerA), CreateTokenParams{
		UserID: f.ownerA, Name: "matrix-audit-fail", Boundary: hubBoundary(), Scopes: []string{"agent:read"},
	})
	require.Error(t, err, "an audit write failure fails the mint")

	after, err := f.store.CountUserAccessTokens(ctx, f.ownerA)
	require.NoError(t, err)
	assert.Equal(t, before, after, "no token row is stored when the audit write fails")
}

// TestCreateToken_ProjectAndHubTokensShareOneCap pins that project and hub
// tokens count against the same per-user cap, and that concurrent hub
// mints for the last slot never exceed it.
func TestCreateToken_ProjectAndHubTokensShareOneCap(t *testing.T) {
	f := newBearerFixture(t, "matrix-cap")
	ctx := context.Background()
	mintCtx := rs4MintContext(f.ownerA)

	for i := 0; i < store.UATMaxPerUser-1; i++ {
		boundary := projectBoundary(f.projectA)
		if i%2 == 1 {
			boundary = hubBoundary()
		}
		_, _, err := f.srv.uatService.CreateTokenWithParams(mintCtx, CreateTokenParams{
			UserID: f.ownerA, Name: fmt.Sprintf("cap-fill-%d", i), Boundary: boundary, Scopes: []string{"agent:read"},
		})
		require.NoError(t, err, "fill token %d", i)
	}

	const contenders = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.ownerA), CreateTokenParams{
				UserID: f.ownerA, Name: fmt.Sprintf("cap-race-%d", i), Boundary: hubBoundary(), Scopes: []string{"agent:read"},
			})
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			} else {
				assert.ErrorIs(t, err, ErrUATLimitExceeded, "a losing contender fails on the cap")
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, 1, successes, "exactly one concurrent mint takes the last slot")

	count, err := f.store.CountUserAccessTokens(ctx, f.ownerA)
	require.NoError(t, err)
	assert.Equal(t, store.UATMaxPerUser, count, "project and hub tokens together never exceed the cap")

	_, _, err = f.srv.uatService.CreateTokenWithParams(mintCtx, CreateTokenParams{
		UserID: f.ownerA, Name: "cap-over-project", Boundary: projectBoundary(f.projectA), Scopes: []string{"agent:read"},
	})
	assert.ErrorIs(t, err, ErrUATLimitExceeded, "a project token cannot exceed a cap filled partly by hub tokens")
}

// TestAgentCreate_HubUATRecordsHubBoundaryProvenance pins that an agent
// created through a hub-boundary UAT records a bounded effect ceiling with
// boundary kind hub and no boundary project, attributed to the UAT.
func TestAgentCreate_HubUATRecordsHubBoundaryProvenance(t *testing.T) {
	f := newUATCreateFixture(t, "hub-provenance")
	f.withDispatcher(t)
	selectors := []string{"agent:create", "project:read"}
	c := uatCeilingFromSelectors(t, selectors...)
	credID := "uat-hub-" + f.creator.ID
	token := NewScopedUserIdentityWithBoundaryAndDecoration(authUser(f.creator), hubBoundary(), selectors, credID,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: c.PermissionIDs}, nil)

	rec := f.create(t, token, CreateAgentRequest{Name: "hub-provenance", AgentRole: string(AgentRoleNone)})
	_, edge := f.createdAgent(t, rec, "hub-provenance")
	assert.Equal(t, store.EffectCeilingBounded, edge.Kind)
	assert.Equal(t, string(BoundaryKindHub), edge.BoundaryKind)
	assert.Empty(t, edge.BoundaryProjectID, "a hub boundary records no boundary project")
	assert.Equal(t, store.SourceCredentialUAT, edge.SourceCredentialKind)
	assert.Equal(t, credID, edge.SourceCredentialID)
	assert.Equal(t, f.creator.ID, edge.DelegatorID)
}
