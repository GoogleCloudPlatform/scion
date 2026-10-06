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
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const artifactListPath = "/api/v1/artifacts?mine=1"

// seedListedArtifact writes an artifact homed in project, owned by
// (ownerKind, ownerRef), with the home-scope read grant publish writes plus
// extra grants. It has no file bytes: the list never reads them.
func seedListedArtifact(t *testing.T, st artifacts.Store, name, project, ownerKind, ownerRef string, extra ...artifacts.Grant) string {
	t.Helper()
	now := time.Now()
	id := tid("list-" + name)
	a := &artifacts.Artifact{ID: id, ScopeKind: artifacts.ScopeKindProject, ScopeRef: project,
		OwnerKind: ownerKind, OwnerRef: ownerRef, Title: name, CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: tid("list-" + name + "-v1"), ArtifactID: id, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: name + ".md", TotalBytes: 1, FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	grants := []artifacts.Grant{{ID: tid("list-" + name + "-home"), ArtifactID: id, SubjectKind: artifacts.SubjectScope,
		SubjectRef: project, Permission: artifacts.GrantRead, CreatedAt: now}}
	for i, g := range extra {
		g.ID, g.ArtifactID, g.CreatedAt = tid("list-"+name+"-g")+string(rune('a'+i)), id, now
		grants = append(grants, g)
	}
	require.NoError(t, st.CreatePublished(context.Background(), a, v, nil, grants))
	return id
}

func principalGrant(kind, ref string) artifacts.Grant {
	return artifacts.Grant{SubjectKind: artifacts.SubjectPrincipal, SubjectRef: artifacts.PrincipalRef(kind, ref), Permission: artifacts.GrantRead}
}

// decodeArtifactList decodes a 200 list response.
func decodeArtifactList(t *testing.T, rec *httptest.ResponseRecorder) artifacts.ArtifactListResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp artifacts.ArtifactListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func listedIDs(resp artifacts.ArtifactListResponse) []string {
	out := make([]string, 0, len(resp.Artifacts))
	for _, a := range resp.Artifacts {
		out = append(out, a.ID)
	}
	slices.Sort(out)
	return out
}

func sortedIDs(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// TestArtifactsListMine: through the real routes and authz, a user lists
// what it owns, what it was granted, and what is homed in projects it is a
// member of; another user's artifact in a project it is not in never
// appears, and leaving a project removes that project's artifacts unless
// owned or directly granted. Agents list their project's artifacts and
// their grants, within their token scope and delegation chain.
func TestArtifactsListMine(t *testing.T) {
	srv, s := testServer(t)
	st, _ := enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "list-p1")
	p2 := artifactProject(t, s, "list-p2")
	p3 := artifactProject(t, s, "list-p3")
	userID := tid("list-user")
	createTestUserWithProjectRole(t, s, userID, "list-user@test.com", p1.ID, store.ProjectRoleMember)
	otherID := tid("list-other")
	createTestUserWithProjectRole(t, s, otherID, "list-other@test.com", p2.ID, store.ProjectRoleMember)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	other, err := s.GetUser(ctx, otherID)
	require.NoError(t, err)
	agent1, tok1 := artifactAgent(t, srv, s, p1.ID, "list-agent-1", AgentRoleBaseline)
	agent2, tok2 := artifactAgent(t, srv, s, p2.ID, "list-agent-2", AgentRoleBaseline)

	own := seedListedArtifact(t, st, "own", p3.ID, artifacts.PrincipalKindUser, user.ID)
	project := seedListedArtifact(t, st, "project", p1.ID, artifacts.PrincipalKindAgent, agent1.ID)
	granted := seedListedArtifact(t, st, "granted", p2.ID, artifacts.PrincipalKindAgent, agent2.ID,
		principalGrant(artifacts.PrincipalKindUser, user.ID), principalGrant(artifacts.PrincipalKindAgent, agent1.ID))
	foreign := seedListedArtifact(t, st, "foreign", p2.ID, artifacts.PrincipalKindUser, other.ID)

	list := func(u *store.User, target string) []string {
		t.Helper()
		return listedIDs(decodeArtifactList(t, userArtifactRequest(t, srv, u, http.MethodGet, target, nil)))
	}
	assert.Equal(t, sortedIDs(own, project, granted), list(user, artifactListPath), "user")
	assert.Equal(t, sortedIDs(granted, foreign), list(other, artifactListPath), "other user")
	assert.NotContains(t, list(user, artifactListPath), foreign, "another user's artifact never appears")
	assert.Equal(t, sortedIDs(own), list(user, artifactListPath+"&owner=me"), "owned only")

	// Every listed artifact is readable by GET for the same caller.
	for _, id := range list(user, artifactListPath) {
		assert.Equal(t, http.StatusOK, userArtifactRequest(t, srv, user, http.MethodGet, "/api/v1/artifacts/"+id, nil).Code, id)
	}

	// Agents: own project's artifacts plus grants.
	agentList := func(tok string) []string {
		t.Helper()
		return listedIDs(decodeArtifactList(t, doRawAgentRequest(t, srv, http.MethodGet, artifactListPath, nil, tok)))
	}
	assert.Equal(t, sortedIDs(project, granted), agentList(tok1), "p1 agent")
	assert.Equal(t, sortedIDs(granted, foreign), agentList(tok2), "p2 agent")

	// An agent token without project:artifact:read lists nothing.
	var noRead []AgentTokenScope
	for _, sc := range ScopesForRole(AgentRoleBaseline) {
		if sc != ScopeProjectArtifactRead {
			noRead = append(noRead, sc)
		}
	}
	noReadTok, err := srv.GetAgentTokenService().GenerateAgentToken(agent1.ID, p1.ID, noRead, nil)
	require.NoError(t, err)
	assert.Empty(t, agentList(noReadTok), "agent without project:artifact:read")

	// The user leaves p1: the p1 artifact drops out; owned and granted stay.
	_, err = s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	require.NoError(t, err)
	assert.Equal(t, sortedIDs(own, granted), list(user, artifactListPath), "after leaving p1")

	// The p1 agent's delegator loses its role: the chain no longer allows
	// reads, so neither the project artifact nor the grant is listed.
	_, err = s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, tid("art-delegator-"+p1.ID))
	require.NoError(t, err)
	assert.Empty(t, agentList(tok1), "agent with a narrowed chain")

	// No mine: 400. Experiment off: 404 like every artifact route.
	rec := userArtifactRequest(t, srv, user, http.MethodGet, "/api/v1/artifacts", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	srv.experiments = experiments.Default() // hub.artifacts defaults off
	rec = userArtifactRequest(t, srv, user, http.MethodGet, artifactListPath, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// TestArtifactsListUserAccessTokensAreBounded: a user access token lists
// only rows its ceiling and boundary allow reading. A token without
// artifact read, or bounded to another project, gets an empty 200, the same
// answer as having no artifacts. Cursors are sealed, carry no artifact id,
// and are bound to the credential that received them.
func TestArtifactsListUserAccessTokensAreBounded(t *testing.T) {
	srv, s := testServer(t)
	st, _ := enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "listuat-p1")
	p2 := artifactProject(t, s, "listuat-p2")
	userID := tid("listuat-user")
	createTestUserWithProjectRole(t, s, userID, "listuat-user@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, userID, "listuat-user@test.com", p2.ID, store.ProjectRoleMember)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	session := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")

	var inP1 []string
	for _, n := range []string{"a", "b", "c"} {
		inP1 = append(inP1, seedListedArtifact(t, st, "uat-"+n, p1.ID, artifacts.PrincipalKindUser, user.ID))
	}
	inP2 := seedListedArtifact(t, st, "uat-p2", p2.ID, artifacts.PrincipalKindAgent, tid("listuat-agent"))

	list := func(identity Identity, target string) artifacts.ArtifactListResponse {
		t.Helper()
		return decodeArtifactList(t, identityArtifactRequest(t, srv, identity, http.MethodGet, target, nil))
	}
	assert.Equal(t, sortedIDs(append(slices.Clone(inP1), inP2)...), listedIDs(list(session, artifactListPath)), "session")
	assert.Empty(t, list(artifactTestUAT(t, session, p1.ID, "agent:read"), artifactListPath).Artifacts, "UAT without artifact:read")
	assert.Equal(t, sortedIDs(inP2), listedIDs(list(artifactTestUAT(t, session, p2.ID, "artifact:read"), artifactListPath)), "UAT bounded to p2")
	assert.Equal(t, sortedIDs(inP1...), listedIDs(list(artifactTestUAT(t, session, p1.ID, "artifact:read"), artifactListPath)), "UAT bounded to p1")
	hubUAT := newScopedUserIdentity(session, TokenBoundary{Kind: BoundaryKindHub}, []string{"artifact:read"}, "uat-hub",
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: uatCeilingFromSelectors(t, "artifact:read").PermissionIDs}, nil)
	assert.Len(t, list(hubUAT, artifactListPath).Artifacts, 4, "hub-boundary UAT with artifact:read")

	// Walk p1 a page at a time with a sealed cursor.
	p1Token := artifactTestUAT(t, session, p1.ID, "artifact:read")
	page := list(p1Token, artifactListPath+"&limit=2")
	require.Len(t, page.Artifacts, 2)
	require.NotEmpty(t, page.NextCursor)
	assert.True(t, strings.HasPrefix(page.NextCursor, listCursorPrefix), "cursor is sealed: %q", page.NextCursor)
	for _, id := range append(slices.Clone(inP1), inP2) {
		assert.NotContains(t, page.NextCursor, id)
	}
	cursor := url.QueryEscape(page.NextCursor)
	rest := list(p1Token, artifactListPath+"&limit=2&cursor="+cursor)
	assert.Len(t, rest.Artifacts, 1)
	assert.Empty(t, rest.NextCursor)

	// The cursor is bound to the credential and the query.
	for name, tc := range map[string]struct {
		identity Identity
		target   string
	}{
		"session":         {session, artifactListPath + "&cursor=" + cursor},
		"other boundary":  {artifactTestUAT(t, session, p2.ID, "artifact:read"), artifactListPath + "&cursor=" + cursor},
		"different query": {p1Token, artifactListPath + "&q=uat&cursor=" + cursor},
	} {
		rec := identityArtifactRequest(t, srv, tc.identity, http.MethodGet, tc.target, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
	}
}

// TestArtifactHostMemberScopes: projects come from live project-scoped role
// bindings (direct or through a group); agents get their own project.
func TestArtifactHostMemberScopes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	p1 := artifactProject(t, s, "members-scope-p1")
	p2 := artifactProject(t, s, "members-scope-p2")
	userID := tid("members-scope-user")
	createTestUserWithProjectRole(t, s, userID, "members-scope@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, userID, "members-scope@test.com", p2.ID, store.ProjectRoleOwner)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	host := newArtifactHost(srv)

	got, err := host.MemberScopes(contextWithIdentity(ctx, NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{p1.ID, p2.ID}, got)

	_, err = s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	require.NoError(t, err)
	got, err = host.MemberScopes(contextWithIdentity(ctx, NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")))
	require.NoError(t, err)
	assert.Empty(t, got, "membership is read live")

	got, err = host.MemberScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, got, "no identity")
}
