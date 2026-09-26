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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1954: skill list/resolve returned HTTP 500 for some callers
// since the #1901/#1912 pagination-and-scope change wired listSkills into
// AuthzService.ResolveListScopes and treated every error from it as a 500.
//
// This file covers the three concrete gaps found while root-causing #1954:
//  1. A malformed cursor must return 400, not 500 (skill_store.go's
//     decodeListCursor error now wraps store.ErrInvalidInput).
//  2. A principal whose group-membership lookup fails for a reason other
//     than "not found" (e.g. a federated identity whose ID isn't a bare
//     UUID, tripping parseUUID) must fail closed to the narrow, no-extra-
//     authority scope instead of 500ing the whole endpoint.
//  3. An agent principal's list-query predicate must agree with the per-row
//     capability filter: skill.read carries no AgentScopes, so an agent's
//     per-row filter can never let a hub-scoped row through — the predicate
//     must not offer one either, or totalCount disagrees with the page.
// ============================================================================

// TestListSkills_MalformedCursorReturns400 proves that a garbled ?cursor on
// GET /api/v1/skills is reported as 400, not 500. Before the fix,
// pkg/store/entadapter/skill_store.go's decodeListCursor error had no
// store.Err* sentinel, so writeErrorFromErr fell through to its default
// (500) branch for any caller — user, agent or broker.
func TestListSkills_MalformedCursorReturns400(t *testing.T) {
	srv, _, alice, _, _ := setupSkillAuthzTest(t)

	for _, cursor := range []string{
		"not-base64-!!!",
		"====",
		"c29tZS1nYXJiYWdl", // valid base64, wrong internal shape
	} {
		t.Run(cursor, func(t *testing.T) {
			path := "/api/v1/skills?" + url.Values{"cursor": {cursor}}.Encode()
			rec := doRequestAsUser(t, srv, alice, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"malformed cursor must be a 400, not a 500; body: %s", rec.Body.String())
		})
	}
}

// TestListSkills_FederatedUserFailsClosedNotServerError injects a
// FederatedUserIdentity whose ID ("issuer:subject") is not a bare UUID.
// authorizationPrincipals normalizes "federated_user" to "user" and calls
// GetEffectiveGroups(ctx, identity.ID()), which calls parseUUID first and
// returns store.ErrInvalidInput (not store.ErrNotFound) for a non-UUID ID.
// Before the fix, ResolveListScopes propagated that error and listSkills
// turned it into a bare 500 for every caller of this shape. The fixed
// behavior must return 200 with a genuinely empty result (totalCount zero,
// no nextCursor): the same principal-resolution error also fails Decide()'s
// per-row capability check closed for every row (see authz.go), so this
// principal can never actually read anything -- giving it a nonzero
// totalCount over an empty page would both repeat the count/page mismatch
// this fix closes for agents and needlessly disclose the hub-wide skill
// count to a principal that can't read any of it.
func TestListSkills_FederatedUserFailsClosedNotServerError(t *testing.T) {
	srv, s, alice, _, _ := setupSkillAuthzTest(t)

	privateSkill := createTestSkill(t, s, "fed-user-private", store.SkillScopeUser, alice.ID, alice.ID)
	hubSkill := createTestSkill(t, s, "fed-user-hub", store.SkillScopeGlobal, "", alice.ID)

	fedIdentity := NewFederatedUserIdentity("https://issuer.example", "remote-subject-123",
		"fed-user@example.com", "Fed User", "member", nil)
	require.NotEqual(t, fedIdentity.ID(), privateSkill.OwnerID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills?status=active", nil)
	req = req.WithContext(contextWithIdentity(context.Background(), fedIdentity))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"a federated identity with a non-UUID ID must fail closed, not 500; body: %s", rec.Body.String())

	resp := decodeSkillsPageFromRecorder(t, rec)
	ids := skillIDSet(resp.Skills)
	assert.False(t, ids[privateSkill.ID], "alice's private skill must not leak to an unresolvable federated principal")
	assert.False(t, ids[hubSkill.ID], "a hub-scoped skill must not be disclosed either")
	assert.Zero(t, resp.TotalCount,
		"totalCount must not be inflated by rows this principal can never actually read; body: %s", rec.Body.String())
	assert.Empty(t, resp.Skills)
	assert.Empty(t, resp.NextCursor, "a zero-total result must not advertise a further page")
}

// TestListSkills_FederatedAgentFailsClosedNotServerError is the
// federated_agent counterpart: GetEffectiveGroupsForAgent hits the same
// parseUUID gate as GetEffectiveGroups.
func TestListSkills_FederatedAgentFailsClosedNotServerError(t *testing.T) {
	srv, s, alice, _, _ := setupSkillAuthzTest(t)

	privateSkill := createTestSkill(t, s, "fed-agent-private", store.SkillScopeUser, alice.ID, alice.ID)
	hubSkill := createTestSkill(t, s, "fed-agent-hub", store.SkillScopeGlobal, "", alice.ID)

	fedIdentity := NewFederatedAgentIdentity("https://issuer.example", "remote-agent-456",
		"", "Fed Agent", "", nil, []AgentTokenScope{ScopeProjectRead})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills?status=active", nil)
	req = req.WithContext(contextWithIdentity(context.Background(), fedIdentity))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"a federated agent with a non-UUID ID must fail closed, not 500; body: %s", rec.Body.String())

	resp := decodeSkillsPageFromRecorder(t, rec)
	ids := skillIDSet(resp.Skills)
	assert.False(t, ids[privateSkill.ID], "alice's private skill must not leak to an unresolvable federated agent")
	assert.False(t, ids[hubSkill.ID], "a hub-scoped skill must not be disclosed either")
	assert.Zero(t, resp.TotalCount,
		"totalCount must not be inflated by rows this principal can never actually read; body: %s", rec.Body.String())
	assert.Empty(t, resp.Skills)
	assert.Empty(t, resp.NextCursor, "a zero-total result must not advertise a further page")
}

// TestListSkills_AgentPredicateAndFilterAgree is the ptone/scion#1954 N-B
// regression: skill.read/skill.list carry no AgentScopes, so an agent's
// per-row capability filter can never allow any skill through -- there is
// no visibility bypass any more (ptone/scion#1903), so a plain agent's
// effective access to every skill read surface is exactly nothing. Before
// this fix, the query predicate offered hub-scoped and own-project rows
// anyway (IncludeHubScope: true, ProjectIDs from the caller's resolved
// scopes), so totalCount counted them while the per-row filter silently
// dropped every one of them from the page -- an agent paging with a small
// limit could see an empty page with a nonzero total and a nextCursor. The
// fixed predicate must agree with what the per-row filter actually allows
// today (nothing): totalCount must be zero, not just the returned page.
func TestListSkills_AgentPredicateAndFilterAgree(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()

	// Hub-scoped (global) skills and a project-scoped skill in the agent's
	// own project: rows the pre-fix predicate would have offered, that the
	// per-row filter can never actually grant an agent.
	const globalCount = 6
	for i := 0; i < globalCount; i++ {
		createTestSkill(t, s, fmt.Sprintf("agent-agree-global-%02d", i), store.SkillScopeGlobal, "", alice.ID)
	}
	createTestSkill(t, s, "agent-agree-own-project", store.SkillScopeProject, project.ID, alice.ID)

	agent := &store.Agent{
		ID: tid("agent-agree-agent"), Slug: tid("agent-agree-agent"), Name: "Agree Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)
	token, err := tokenSvc.GenerateAgentToken(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)

	// Default limit: totalCount must equal the number of items actually
	// returned -- zero, since an agent has no AgentScopes grant for
	// skill.read/list and no more visibility bypass exists to widen it.
	rec := doAgentTokenRequestSkills(t, srv, "/api/v1/skills?status=active", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeSkillsPageFromRecorder(t, rec)
	assert.Equal(t, len(resp.Skills), resp.TotalCount,
		"totalCount must agree with the page for an agent caller; body: %s", rec.Body.String())
	assert.Zero(t, resp.TotalCount,
		"an agent has no read grant on any of the hub-scoped or own-project skills seeded here; body: %s", rec.Body.String())

	// A small limit must not produce an empty page with a nonzero total and
	// a cursor: with a genuinely zero total there must be exactly one
	// (empty) page and no nextCursor.
	rec = doAgentTokenRequestSkills(t, srv, "/api/v1/skills?status=active&limit=1", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page := decodeSkillsPageFromRecorder(t, rec)
	assert.Zero(t, page.TotalCount)
	assert.Empty(t, page.Skills)
	assert.Empty(t, page.NextCursor, "a zero-total result must not advertise a further page")
}

// TestListSkills_AgentWithIsAllScopeStillGetsEmptyScope covers a gemini
// review comment on the initial fix: the isAgentIdentity case in listSkills'
// switch must be checked BEFORE scopeResult.Scopes.IsAll(), not after. An
// agent whose resolved scope happens to be IsAll (here: a hub-admin role
// bound directly to the agent's own principal, which the kernel does not
// forbid) must still get the empty, per-row-filter-agreeing scope -- not an
// unfiltered query, which would reintroduce the exact totalCount/page
// mismatch this fix exists to close, just triggered a different way.
func TestListSkills_AgentWithIsAllScopeStillGetsEmptyScope(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()

	// A hub-scoped skill the pre-fix (or wrongly-ordered) code would have
	// disclosed via an unfiltered query.
	createTestSkill(t, s, "agent-isall-hub", store.SkillScopeGlobal, "", alice.ID)

	agent := &store.Agent{
		ID: tid("agent-isall-agent"), Slug: tid("agent-isall-agent"), Name: "IsAll Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Bind hub-admin directly to the agent's own principal (not via a group)
	// so ResolveListScopes resolves an unrestricted (IsAll) scope for it.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "hub-admin role should have been seeded")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      agent.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// applyCredentialCaveats (authz_list.go) intersects an agent's resolved
	// scope with ScopeSetExplicit(agentIdent.ProjectID()) whenever the token
	// carries a project ID, which would collapse IsAll back down to just
	// that project before this test could ever observe the IsAll case. Mint
	// the token with an empty project ID so the caveat does not apply and
	// the hub-admin binding's IsAll survives, exercising the exact ordering
	// this test is pinning.
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)
	token, err := tokenSvc.GenerateAgentToken(agent.ID, "", []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)

	rec := doAgentTokenRequestSkills(t, srv, "/api/v1/skills?status=active", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeSkillsPageFromRecorder(t, rec)
	assert.Zero(t, resp.TotalCount,
		"an agent must get the empty scope even when its resolved scope is IsAll; body: %s", rec.Body.String())
	assert.Empty(t, resp.Skills)
	assert.Empty(t, resp.NextCursor)
}

// doAgentTokenRequestSkills issues a GET to path authenticated as an agent
// token. Local helper (rather than reusing doAgentTokenRequest from
// port_forward_handlers_test.go) to keep this file self-contained.
func doAgentTokenRequestSkills(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	// Handler(), not mux directly: the agent-token auth middleware that
	// turns X-Scion-Agent-Token into an AgentIdentity in context wraps mux,
	// so bypassing it here would silently test the nil-identity path
	// instead of the agent path.
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeSkillsPageFromRecorder decodes a ListSkillsResponse from an
// already-issued response recorder (used where the request needed a
// hand-built context, so decodeSkillsPage's own request issuance doesn't
// apply).
func decodeSkillsPageFromRecorder(t *testing.T, rec *httptest.ResponseRecorder) ListSkillsResponse {
	t.Helper()
	var resp ListSkillsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}
