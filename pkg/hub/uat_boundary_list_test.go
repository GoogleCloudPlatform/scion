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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListCaveats_HubBoundaryKeepsLiveScopeSet pins that a hub-boundary UAT
// whose ceiling carries the list permission keeps the grant-derived scope set
// with no project intersection.
func TestListCaveats_HubBoundaryKeepsLiveScopeSet(t *testing.T) {
	scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser("list-hub-user"), hubBoundary(), []string{"agent:list"}, "list-hub-cred", bearerCeiling(t, "agent:list"), nil)
	for _, in := range []ScopeSet{ScopeSetAll(), ScopeSetExplicit("proj-1", "proj-2"), ScopeSetNone()} {
		got := applyCredentialCaveats(scoped, "agent.list", in)
		assert.True(t, got.Equal(in), "hub boundary must keep %v, got %v", in, got)
	}
}

// TestListCaveats_HubBoundaryRequiresListPermissionInCeiling pins that a
// hub-boundary UAT lists nothing for a permission its ceiling does not carry.
func TestListCaveats_HubBoundaryRequiresListPermissionInCeiling(t *testing.T) {
	scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser("list-hub-user"), hubBoundary(), []string{"agent:read"}, "list-hub-cred", bearerCeiling(t, "agent:read"), nil)
	for _, in := range []ScopeSet{ScopeSetAll(), ScopeSetExplicit("proj-1")} {
		got := applyCredentialCaveats(scoped, "agent.list", in)
		assert.True(t, got.IsNone(), "a ceiling without agent.list must list nothing, got %v", got)
	}
}

// TestListCaveats_ProjectBoundaryIntersectsWithItsProject pins that a
// project-boundary UAT's scope set is confined to the boundary project.
func TestListCaveats_ProjectBoundaryIntersectsWithItsProject(t *testing.T) {
	scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser("list-proj-user"), projectBoundary("proj-1"), []string{"agent:list"}, "list-proj-cred", bearerCeiling(t, "agent:list"), nil)
	assert.True(t, applyCredentialCaveats(scoped, "agent.list", ScopeSetAll()).Equal(ScopeSetExplicit("proj-1")))
	assert.True(t, applyCredentialCaveats(scoped, "agent.list", ScopeSetExplicit("proj-1", "proj-2")).Equal(ScopeSetExplicit("proj-1")))
	assert.True(t, applyCredentialCaveats(scoped, "agent.list", ScopeSetExplicit("proj-2")).IsNone())
}

// TestListCaveats_InvalidBoundaryYieldsNone pins that a UAT identity whose
// boundary is invalid lists nothing, whatever its grants or ceiling.
func TestListCaveats_InvalidBoundaryYieldsNone(t *testing.T) {
	ceiling := bearerCeiling(t, "agent:list")
	for name, boundary := range map[string]TokenBoundary{
		"project without ID": {Kind: BoundaryKindProject},
		"hub with project":   {Kind: BoundaryKindHub, ProjectID: "proj-1"},
		"unknown kind":       {Kind: "org", ProjectID: "proj-1"},
		"empty":              {},
	} {
		scoped := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser("list-invalid-user"), boundary, []string{"agent:list"}, "list-invalid-cred", ceiling, nil)
		got := applyCredentialCaveats(scoped, "agent.list", ScopeSetAll())
		assert.True(t, got.IsNone(), "%s: got %v", name, got)
	}
	var typedNil *ScopedUserIdentity
	assert.True(t, applyCredentialCaveats(typedNil, "agent.list", ScopeSetAll()).IsNone(), "a typed-nil UAT identity lists nothing")
}

// TestResolveListScopes_HubBoundaryListsOnlyProjectsWithAccess pins that a
// hub-boundary UAT's list scope is the holder's live project access: the
// project it is a member of, and not a project it cannot access. A
// project-boundary UAT for the same user sees only its boundary project.
func TestResolveListScopes_HubBoundaryListsOnlyProjectsWithAccess(t *testing.T) {
	f := newBearerFixture(t, "list-scope")
	ctx := context.Background()
	projectC := tid("bearer-list-scope-project-c")
	createRS1Project(t, f.store, projectC, tid("bearer-list-scope-owner-c"))
	uatpMember(t, f.store, projectC, f.ownerA)

	hub := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerA), hubBoundary(), []string{"agent:list"}, tid("list-scope-hub-cred"), bearerCeiling(t, "agent:list"), nil)
	result, err := f.srv.authzService.ResolveListScopes(ctx, hub, "agent.list")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{f.projectA, projectC}, result.Scopes.ProjectIDs(), "hub boundary lists every project with live access, and only those")

	project := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerA), projectBoundary(f.projectA), []string{"agent:list"}, tid("list-scope-proj-cred"), bearerCeiling(t, "agent:list"), nil)
	result, err = f.srv.authzService.ResolveListScopes(ctx, project, "agent.list")
	require.NoError(t, err)
	assert.Equal(t, []string{f.projectA}, result.Scopes.ProjectIDs(), "project boundary lists only its project")

	uatpDeleteProjectBinding(t, f.store, f.ownerA, projectC)
	result, err = f.srv.authzService.ResolveListScopes(ctx, hub, "agent.list")
	require.NoError(t, err)
	assert.Equal(t, []string{f.projectA}, result.Scopes.ProjectIDs(), "removing membership removes the project from the next list")
}

// TestListAgents_HubTokenReturnsOnlyAccessibleProjects pins the list rule
// end to end: a member's hub token lists agents of the member's project and
// none from a project the member cannot access.
func TestListAgents_HubTokenReturnsOnlyAccessibleProjects(t *testing.T) {
	f := newBearerFixture(t, "list-http")
	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.ownerA), CreateTokenParams{
		UserID: f.ownerA, Name: "list-http", Boundary: hubBoundary(), Scopes: []string{"agent:list"},
	})
	require.NoError(t, err)

	rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, "/api/v1/agents", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Agents []struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	var ids []string
	for _, a := range body.Agents {
		assert.Equal(t, f.projectA, a.ProjectID, "agent %s is outside the holder's live access", a.ID)
		ids = append(ids, a.ID)
	}
	assert.Contains(t, ids, f.agentA.ID)
	assert.NotContains(t, ids, f.agentB.ID)
}

// TestListCursorBinding_KeyedOnBoundaryKindProjectAndCredential pins that a
// list cursor minted under a hub-boundary UAT does not bind for a
// project-boundary UAT or a session of the same user, and that two
// credentials with the same boundary do not share a binding.
func TestListCursorBinding_KeyedOnBoundaryKindProjectAndCredential(t *testing.T) {
	filter := store.AgentFilter{Phase: "running"}
	user := bearerUser("cursor-user")
	ceiling := bearerCeiling(t, "agent:list")
	hub := NewScopedUserIdentityWithBoundaryAndDecoration(user, hubBoundary(), []string{"agent:list"}, "cursor-cred", ceiling, nil)
	hubOther := NewScopedUserIdentityWithBoundaryAndDecoration(user, hubBoundary(), []string{"agent:list"}, "cursor-cred-2", ceiling, nil)
	projectP := NewScopedUserIdentityWithBoundaryAndDecoration(user, projectBoundary("proj-p"), []string{"agent:list"}, "cursor-cred", ceiling, nil)
	projectQ := NewScopedUserIdentityWithBoundaryAndDecoration(user, projectBoundary("proj-q"), []string{"agent:list"}, "cursor-cred", ceiling, nil)

	bindings := map[string]string{
		"hub":       scopedCursorBinding("agents", filter, hub),
		"hub other": scopedCursorBinding("agents", filter, hubOther),
		"project P": scopedCursorBinding("agents", filter, projectP),
		"project Q": scopedCursorBinding("agents", filter, projectQ),
		"session":   scopedCursorBinding("agents", filter, user),
		"unauthed":  scopedCursorBinding("agents", filter, nil),
	}
	assert.Equal(t, bindings["hub"], scopedCursorBinding("agents", filter, hub), "the binding is stable for one credential")

	seen := map[string]string{}
	for name, b := range bindings {
		if prev, dup := seen[b]; dup {
			t.Errorf("%s and %s share a cursor binding", name, prev)
		}
		seen[b] = name
	}

	cursor := authorizedListCursor(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), "00000000-0000-4000-8000-000000000001", bindings["hub"])
	assert.NoError(t, validateAuthorizedListCursor(cursor, bindings["hub"]))
	assert.Error(t, validateAuthorizedListCursor(cursor, bindings["project P"]), "a hub cursor must not open for a project token")
	assert.Error(t, validateAuthorizedListCursor(cursor, bindings["session"]), "a hub cursor must not open for a session")
}
