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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPreArtifactCeilingKeepsProjectRead is the regression test for adding
// an artifact permission to the agent roles: a ceiling frozen before it must
// keep admitting the scopes and roles it admitted, and must not gain the new
// capability.
func TestPreArtifactCeilingKeepsProjectRead(t *testing.T) {
	c := preArtifactReadCeiling()

	assert.True(t, ceilingAllowsScope(c, ScopeProjectRead), "project:read must still be issuable")
	assert.False(t, ceilingAllowsScope(c, ScopeProjectArtifactRead), "project:artifact:read must not be issuable")
	assert.False(t, EffectCeilingAllows(c, "artifact.read", false), "artifact.read stays denied at use")

	role, cause, ok := childRoleWithinCeiling(c, AgentRoleFull, false)
	require.True(t, ok, "deny cause %q", cause)
	assert.Equal(t, AgentRoleBaseline, role, "a defaulted full role still caps to baseline")
	for _, explicit := range []AgentRole{AgentRoleReadOnly, AgentRoleBaseline} {
		got, cause, ok := childRoleWithinCeiling(c, explicit, true)
		assert.True(t, ok, "explicit %s must still fit (cause %q)", explicit, cause)
		assert.Equal(t, explicit, got)
	}

	issued := filterScopes(ScopesForRole(AgentRoleBaseline), c, ScopeCeilings{})
	assert.Contains(t, issued, ScopeProjectRead)
	assert.NotContains(t, issued, ScopeProjectArtifactRead, "the mint filter drops the ceiling-optional scope")
	assert.NotContains(t, issued, ScopeProjectArtifactWrite, "the mint filter drops the write scope too")
	issuedFull := filterScopes(ScopesForRole(AgentRoleFull), c, ScopeCeilings{})
	assert.NotContains(t, issuedFull, ScopeProjectArtifactRead)
	assert.NotContains(t, issuedFull, ScopeProjectArtifactWrite)
}

// TestReadOnlyArtifactCeilingIssuesReadNotWrite: a ceiling holding the
// pre-artifact set plus artifact.read (but not artifact.create) issues the
// read scope and withholds the write scope.
func TestReadOnlyArtifactCeilingIssuesReadNotWrite(t *testing.T) {
	c := boundedCeiling(
		"harness_config.list", "harness_config.read", "project.read",
		"skill.list", "skill.read", "template.list", "template.read",
		"agent.create", "artifact.read",
	)
	for _, role := range []AgentRole{AgentRoleBaseline, AgentRoleFull} {
		issued := filterScopes(ScopesForRole(role), c, ScopeCeilings{})
		assert.Contains(t, issued, ScopeProjectArtifactRead, role)
		assert.NotContains(t, issued, ScopeProjectArtifactWrite, role)
	}
}

// TestPreArtifactCeilingChildCannotReadArtifacts follows the token a child
// under a pre-artifact ceiling is issued through to the artifact host: it
// keeps project reads but is not served by the artifact service.
func TestPreArtifactCeilingChildCannotReadArtifacts(t *testing.T) {
	srv, s := testServer(t)
	host := newArtifactHost(srv)
	agent := createTestAgent(t, s)

	issued := filterScopes(ScopesForRole(AgentRoleBaseline), preArtifactReadCeiling(), ScopeCeilings{})
	ctx := contextWithIdentity(context.Background(), artifactTestAgent(agent.ID, agent.ProjectID, issued...))

	_, _, _, ok := host.Principal(ctx)
	assert.False(t, ok, "the artifact service must not serve the child")
	assert.False(t, host.Authorize(ctx, agent.ProjectID, artifacts.PermissionRead))
	assert.False(t, host.Authorize(ctx, agent.ProjectID, artifacts.PermissionCreate))
	assert.False(t, host.Permits(ctx, agent.ProjectID, artifacts.PermissionCreate))
}

// TestCeilingOptionalScopesDoNotDecideDelegation: an agent without the
// artifact scopes may still delegate a role that carries them (the child's
// mint drops them), but asking for one explicitly still requires holding it.
func TestCeilingOptionalScopesDoNotDecideDelegation(t *testing.T) {
	authz, _ := authzTestSetup(t)
	required := []AgentTokenScope{}
	for _, sc := range ScopesForRole(AgentRoleBaseline) {
		if !ceilingOptionalRoleScopes[sc] {
			required = append(required, sc)
		}
	}
	actor := artifactTestAgent(tid("optional-delegator"), tid("optional-project"), required...)

	byRole := authz.canAgentDelegateToAgent(actor, GrantDescriptor{
		Type: GrantTypeAgentDelegation, AgentRole: string(AgentRoleBaseline), ProjectID: tid("optional-project"),
	})
	assert.True(t, byRole.Allowed, "delegating baseline must not require the optional scopes: %q", byRole.Reason)

	explicit := authz.canAgentDelegateToAgent(actor, GrantDescriptor{
		Type: GrantTypeAgentDelegation, AgentRole: string(AgentRoleBaseline), ProjectID: tid("optional-project"),
		AgentScopes: []AgentTokenScope{ScopeProjectArtifactRead},
	})
	assert.False(t, explicit.Allowed, "an explicitly requested optional scope stays required")
}

func checkCeilingOptionalRoleScopesReviewed(optional, reviewed map[AgentTokenScope]bool, bundles func(AgentRole) []AgentTokenScope) error {
	inAnyRole := map[AgentTokenScope]bool{}
	for _, role := range []AgentRole{AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull} {
		for _, s := range bundles(role) {
			inAnyRole[s] = true
		}
	}
	for s, enabled := range optional {
		if !enabled {
			return fmt.Errorf("ceilingOptionalRoleScopes[%q] is false; omit absent scopes rather than storing false (sortedOptionalRoleScopes iterates map keys)", s)
		}
		if !inAnyRole[s] {
			return fmt.Errorf("ceilingOptionalRoleScopes[%q] does not appear in any ScopesForRole bundle", s)
		}
		if !reviewed[s] {
			return fmt.Errorf(
				"ceilingOptionalRoleScopes contains unreviewed scope %q: "+
					"ceilingOptionalRoleScopes IS the required classification for additive role scopes (ptone/scion#8324); "+
					"before adding %q to the reviewed map in TestCeilingOptionalRoleScopesAreReviewed, confirm: "+
					"(1) its permission coverage is pinned in TestAgentTokenScopesMapToRegistry and safe to exclude from unrecorded legacy chains, "+
					"(2) handlers/CLI fail gracefully (e.g. 403 missing_scope) when pre-deploy bounded chains lack it until 'scion reset-auth --reissue-scopes', and "+
					"(3) skipping it in canAgentDelegateToAgent's JWT check cannot widen authority beyond the delegator's live chain ceiling",
				s, s,
			)
		}
	}
	for s := range reviewed {
		if !optional[s] {
			return fmt.Errorf("reviewed optional scope %q was removed from ceilingOptionalRoleScopes; update TestCeilingOptionalRoleScopesAreReviewed if intentional", s)
		}
	}
	return nil
}

// TestCeilingOptionalRoleScopesAreReviewed ensures additions to
// ceilingOptionalRoleScopes receive explicit security review rather than
// pinning the set closed against future additive role scopes
// (ptone/scion#8324).
func TestCeilingOptionalRoleScopesAreReviewed(t *testing.T) {
	reviewed := map[AgentTokenScope]bool{
		ScopeProjectArtifactRead:          true,
		ScopeProjectArtifactWrite:         true,
		ScopeProjectArtifactRequestReview: true,
		ScopeProjectSkillWrite:            true,
	}
	require.NoError(t, checkCeilingOptionalRoleScopesReviewed(ceilingOptionalRoleScopes, reviewed, ScopesForRole))

	t.Run("unreviewed addition prompts review checklist", func(t *testing.T) {
		candidate := map[AgentTokenScope]bool{
			ScopeProjectArtifactRead:          true,
			ScopeProjectArtifactWrite:         true,
			ScopeProjectArtifactRequestReview: true,
			ScopeProjectSkillWrite:            true,
			ScopeProjectTemplateWrite:         true,
		}
		err := checkCeilingOptionalRoleScopesReviewed(candidate, reviewed, ScopesForRole)
		require.Error(t, err)
		assert.Contains(t, err.Error(), string(ScopeProjectTemplateWrite))
		assert.Contains(t, err.Error(), "TestCeilingOptionalRoleScopesAreReviewed")
	})

	t.Run("false entry rejected", func(t *testing.T) {
		bad := map[AgentTokenScope]bool{
			ScopeProjectArtifactRead:          true,
			ScopeProjectArtifactWrite:         false,
			ScopeProjectArtifactRequestReview: true,
			ScopeProjectSkillWrite:            true,
		}
		err := checkCeilingOptionalRoleScopesReviewed(bad, reviewed, ScopesForRole)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "false")
	})
}

// assertNoArtifactAccess checks that an agent holding scopes has neither
// artifact scope and that the artifact host neither serves it nor allows it
// to read, publish or update artifacts in its project.
func assertNoArtifactAccess(t *testing.T, host *artifactHost, agentID, projectID string, scopes []AgentTokenScope) {
	t.Helper()
	assert.NotContains(t, scopes, ScopeProjectArtifactRead)
	assert.NotContains(t, scopes, ScopeProjectArtifactWrite)
	ctx := contextWithIdentity(context.Background(), artifactTestAgent(agentID, projectID, scopes...))
	_, _, _, ok := host.Principal(ctx)
	assert.False(t, ok, "the artifact service must not serve the agent (it answers 404)")
	for _, p := range []string{artifacts.PermissionRead, artifacts.PermissionCreate, artifacts.PermissionUpdate} {
		assert.False(t, host.Authorize(ctx, projectID, p), p)
	}
}

// TestLegacyUATChildHasNoArtifactAccess: a UAT minted before artifacts
// existed (unspecified ceiling version, backfill-normalized permissions)
// still creates a child agent, and that child's tokens carry no artifact
// scope, now or once write joins the role bundles, so it can neither read
// nor publish artifacts.
func TestLegacyUATChildHasNoArtifactAccess(t *testing.T) {
	f := newUATCreateFixture(t, "uat-legacy-artifacts")
	uat := NewScopedUserIdentity(authUser(f.creator), f.proj.ID, minimalSelectors(t))
	require.Equal(t, permissions.CeilingVersionUnspecified, uat.Ceiling().Version)

	rec := f.create(t, uat, CreateAgentRequest{Name: "uat-legacy-artifacts"})
	child, edge := f.createdAgent(t, rec, "uat-legacy-artifacts")
	require.Equal(t, store.EffectCeilingBounded, edge.Kind)

	ctx := context.Background()
	authz := f.srv.authzService
	candidates := append(authz.mintCandidateScopes(child), ScopeProjectArtifactWrite)
	issued, err := authz.ceilingFilteredAgentScopes(ctx, child, candidates)
	require.NoError(t, err)
	assert.Contains(t, issued, ScopeProjectRead, "the child keeps project reads")
	assertNoArtifactAccess(t, newArtifactHost(f.srv), child.ID, child.ProjectID, issued)
}

// TestPreArtifactCeilingChildHasNoArtifactAccess: the same for a child under
// a bounded ceiling recorded before artifacts, for every role, including a
// write scope offered as a candidate.
func TestPreArtifactCeilingChildHasNoArtifactAccess(t *testing.T) {
	srv, s := testServer(t)
	agent := createTestAgent(t, s)
	for _, role := range []AgentRole{AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull} {
		t.Run(string(role), func(t *testing.T) {
			candidates := append(ScopesForRole(role), ScopeProjectArtifactWrite)
			issued := filterScopes(candidates, preArtifactReadCeiling(), ScopeCeilings{})
			assertNoArtifactAccess(t, newArtifactHost(srv), agent.ID, agent.ProjectID, issued)
		})
	}
}

// TestReadonlyAgentNeverHoldsArtifactWrite: the readonly role carries no
// write scope, under any ceiling.
func TestReadonlyAgentNeverHoldsArtifactWrite(t *testing.T) {
	assert.NotContains(t, ScopesForRole(AgentRoleReadOnly), ScopeProjectArtifactWrite)
	a := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleReadOnly)}}
	candidates := (&AuthzService{}).mintCandidateScopes(a)
	assert.NotContains(t, candidates, ScopeProjectArtifactWrite)
	principal := store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	assert.NotContains(t, filterScopes(candidates, principal, ScopeCeilings{}), ScopeProjectArtifactWrite,
		"not even under an unbounded principal ceiling")
}

// TestPreArtifactAgentParentChildHasNoArtifactAccess goes through the real
// create handler with an agent parent: a full-role parent created from a
// token that covers the role's required scopes but no artifact permission
// can still create a child, and that child's frozen ceiling and issued
// scopes carry no artifact permission or scope.
func TestPreArtifactAgentParentChildHasNoArtifactAccess(t *testing.T) {
	f := newChainFixture(t, "artifact-agent-parent")
	var required []AgentTokenScope
	for _, sc := range ScopesForRole(AgentRoleFull) {
		if !ceilingOptionalRoleScopes[sc] {
			required = append(required, sc)
		}
	}
	// A current-version ceiling holding exactly the full role's required
	// coverage: everything the role needs, and no artifact permission.
	ceilingIDs := agentScopeCoverage(required)
	var selectors []string
	for _, permID := range ceilingIDs {
		require.NotContains(t, permID, "artifact.")
		if p, _ := registryPermission(permID); p.UATScope != "" {
			selectors = append(selectors, p.UATScope)
		}
	}
	uat := NewScopedUserIdentityWithCeiling(authUser(f.creator), f.proj.ID, sortedUniqueIDs(selectors), "uat-artifact-parent",
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: ceilingIDs})

	parentRec := f.create(t, uat, CreateAgentRequest{Name: "artifact-parent", AgentRole: string(AgentRoleFull)})
	parent, parentEdge := f.createdAgent(t, parentRec, "artifact-parent")
	require.Equal(t, store.EffectCeilingBounded, parentEdge.Kind)
	parentScopes, err := f.srv.authzService.ceilingFilteredAgentScopes(context.Background(), parent, f.srv.authzService.mintCandidateScopes(parent))
	require.NoError(t, err)
	require.Contains(t, parentScopes, ScopeAgentCreate)
	require.NotContains(t, parentScopes, ScopeProjectArtifactRead, "the parent's own token lacks the read scope")

	// Both create responses warn that the non-demoted ceiling-optional scopes
	// were left out (ptone/scion#4569); project:skill:write is demoted to Hub
	// debug logging (ptone/scion#9452).
	assertDroppedScopesWarning(t, parentRec, "artifact-parent", ScopeProjectArtifactRead, ScopeProjectArtifactWrite, ScopeProjectArtifactRequestReview)
	childRec := f.createAsParent(t, f.agentToken(t, parent.ID), CreateAgentRequest{Name: "artifact-child"})
	child, childEdge := f.createdAgent(t, childRec, "artifact-child")
	assertDroppedScopesWarning(t, childRec, "artifact-child", ScopeProjectArtifactRead, ScopeProjectArtifactWrite, ScopeProjectArtifactRequestReview)
	assert.NotContains(t, childEdge.PermissionIDs, "artifact.read")
	assert.NotContains(t, childEdge.PermissionIDs, "artifact.create")
	assert.NotContains(t, childEdge.PermissionIDs, "artifact.update")

	authz := f.srv.authzService
	issued, err := authz.ceilingFilteredAgentScopes(context.Background(), child, append(authz.mintCandidateScopes(child), ScopeProjectArtifactWrite))
	require.NoError(t, err)
	assertNoArtifactAccess(t, newArtifactHost(f.srv), child.ID, child.ProjectID, issued)
}

// createResponseWarnings decodes the warnings of a create or lifecycle
// response.
func createResponseWarnings(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var body struct {
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Warnings
}

// assertDroppedScopesWarning asserts that rec carries exactly the
// dropped-scope warning for slug naming scopes.
func assertDroppedScopesWarning(t *testing.T, rec *httptest.ResponseRecorder, slug string, scopes ...AgentTokenScope) {
	t.Helper()
	want := droppedScopesWarning(&store.Agent{Slug: slug}, &AgentTokenGrant{DroppedScopes: scopes})
	require.NotEmpty(t, want)
	assert.Contains(t, createResponseWarnings(t, rec), want, rec.Body.String())
}

// TestDroppedScopesWarningText: the warning names every dropped scope, the
// creator's scope ceiling as the reason, and the re-issue path; there is
// none without a dropped scope.
func TestDroppedScopesWarningText(t *testing.T) {
	both := droppedScopesWarning(&store.Agent{Slug: "worker-1", Name: "Worker 1"},
		&AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectArtifactRead, ScopeProjectArtifactWrite}})
	assert.Contains(t, both, "without the project:artifact:read and project:artifact:write scopes")
	assert.Contains(t, both, "scope ceiling of the agent's creator does not allow them")
	assert.Contains(t, both, "missing_scope")
	assert.Contains(t, both, "same parent gives it the same scopes")
	assert.Contains(t, both, "hub super-admin can run 'scion reset-auth worker-1 --reissue-scopes'")
	assert.NotContains(t, strings.ToLower(both), "artifact commands")

	one := droppedScopesWarning(&store.Agent{Name: "Worker 2"}, &AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectArtifactRead}})
	assert.Contains(t, one, "without the project:artifact:read scope:")
	assert.Contains(t, one, "does not allow it,")
	assert.Contains(t, one, "'scion reset-auth <agent> --reissue-scopes'", "a display name is never used in the command")
	assert.NotContains(t, one, "Worker 2")

	nonArtifact := droppedScopesWarning(&store.Agent{Slug: "worker-4"}, &AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectTemplateWrite}})
	assert.Contains(t, nonArtifact, "without the project:template:write scope:")
	assert.Contains(t, nonArtifact, "does not allow it,")
	assert.Contains(t, nonArtifact, "missing_scope")
	assert.Contains(t, nonArtifact, "'scion reset-auth worker-4 --reissue-scopes'")
	assert.NotContains(t, strings.ToLower(nonArtifact), "artifact", "warning text must be generic across scopes")

	skillOnly := droppedScopesWarning(&store.Agent{Slug: "worker-skill"}, &AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectSkillWrite}})
	assert.Empty(t, skillOnly, "project:skill:write alone is demoted to Hub debug logging (ptone/scion#9452)")
	assert.Equal(t, []AgentTokenScope{ScopeProjectSkillWrite},
		debugOnlyDroppedScopesFromGrant(&AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectSkillWrite}}))

	combined := droppedScopesWarning(&store.Agent{Slug: "worker-all"},
		&AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectSkillWrite, ScopeProjectArtifactRead, ScopeProjectArtifactWrite}})
	assert.Contains(t, combined, "without the project:artifact:read and project:artifact:write scopes:")
	assert.NotContains(t, combined, string(ScopeProjectSkillWrite))
	assert.Contains(t, combined, "so operations requiring them will fail with missing_scope")
	assert.Equal(t, []AgentTokenScope{ScopeProjectSkillWrite},
		debugOnlyDroppedScopesFromGrant(&AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectSkillWrite, ScopeProjectArtifactRead, ScopeProjectArtifactWrite}}))

	slugged := droppedScopesWarning(&store.Agent{Slug: "worker-3", Name: "Worker 3 (docs site)"}, &AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectArtifactWrite}})
	assert.Contains(t, slugged, "'scion reset-auth worker-3 --reissue-scopes'")
	assert.NotContains(t, slugged, "Worker 3")

	assert.Empty(t, droppedScopesWarning(&store.Agent{Slug: "w"}, &AgentTokenGrant{}))
	assert.Empty(t, droppedScopesWarning(&store.Agent{Slug: "w"}, nil))
}

// TestDroppedCeilingOptionalScopes: only ceiling-optional role scopes the
// issued set lacks are reported, once each and in candidate order; a
// required scope the ceiling dropped is not (that denies the role instead).
func TestDroppedCeilingOptionalScopes(t *testing.T) {
	candidates := []AgentTokenScope{ScopeProjectRead, ScopeProjectArtifactWrite, ScopeAgentCreate, ScopeProjectArtifactRead, ScopeProjectArtifactWrite}
	assert.Equal(t, []AgentTokenScope{ScopeProjectArtifactWrite, ScopeProjectArtifactRead},
		droppedCeilingOptionalScopes(candidates, []AgentTokenScope{ScopeProjectRead}))
	assert.Equal(t, []AgentTokenScope{ScopeProjectArtifactWrite},
		droppedCeilingOptionalScopes(candidates, []AgentTokenScope{ScopeProjectArtifactRead}))
	assert.Empty(t, droppedCeilingOptionalScopes(candidates, candidates))
	assert.Empty(t, droppedCeilingOptionalScopes(ScopesForRole(AgentRoleNone), nil))
}

// TestMintWarnsWhenCeilingDropsArtifactScopes: create, start and restart of
// an agent whose chain ceiling does not allow the artifact scopes each
// raise the dropped-scope warning, and the start response carries it; an
// agent whose ceiling allows them gets no such warning (ptone/scion#4569).
func TestMintWarnsWhenCeilingDropsArtifactScopes(t *testing.T) {
	f := newMintFixture(t, "drop-warn")
	capped := f.agent(t, "drop-warn-capped", AgentRoleBaseline, state.PhaseStopped)
	f.edge(t, store.DelegationPrincipalUser, f.userID, capped.ID, preArtifactReadCeiling(), provSession)
	want := droppedScopesWarning(capped, &AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectArtifactRead, ScopeProjectArtifactWrite, ScopeProjectArtifactRequestReview}})

	for name, dispatch := range map[string]func(context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := f.disp.DispatchAgentCreate(ctx, capped)
			return err
		},
		"start":   func(ctx context.Context) error { return f.disp.DispatchAgentStart(ctx, capped, "", false) },
		"restart": func(ctx context.Context) error { return f.disp.DispatchAgentRestart(ctx, capped) },
	} {
		ctx, warns := withDispatchWarnings(context.Background())
		require.NoError(t, dispatch(ctx), name)
		assert.Contains(t, warns.Warnings(), want, name)
	}

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+capped.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, createResponseWarnings(t, rec), want, "the start response carries the warning")

	open := f.agent(t, "drop-warn-open", AgentRoleBaseline, state.PhaseStopped)
	f.edge(t, store.DelegationPrincipalUser, f.userID, open.ID, ceilPrincip, provSession)
	ctx, warns := withDispatchWarnings(context.Background())
	_, err := f.disp.DispatchAgentCreate(ctx, open)
	require.NoError(t, err)
	require.NoError(t, f.disp.DispatchAgentStart(ctx, open, "", false))
	for _, w := range warns.Warnings() {
		assert.NotContains(t, w, "issued without the", "no warning when the ceiling allows the artifact scopes")
	}
	assert.Contains(t, f.tokenClaims(t, f.client.lastResolvedEnv["SCION_AUTH_TOKEN"]).Scopes, ScopeProjectArtifactWrite)
}

// TestMintDemotesDroppedSkillWriteScopeToDebug verifies ptone/scion#9452:
//   - When only project:skill:write is dropped by the creator's ceiling, no
//     user-facing dispatch/HTTP warning is emitted; the omission is logged at
//     Hub debug level instead, while grant.DroppedScopes and minted scopes are
//     unchanged.
//   - When both project:skill:write and another ceiling-optional scope (such as
//     project:artifact:write) are dropped, the user-facing warning lists only
//     the other scope, and project:skill:write is logged at Hub debug.
func TestMintDemotesDroppedSkillWriteScopeToDebug(t *testing.T) {
	f := newMintFixture(t, "drop-skill-debug")

	// Capture Hub debug logs from the dispatcher.
	var logBuf bytes.Buffer
	f.disp.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Ceiling allowing everything in AgentRoleFull EXCEPT skill.create/skill.update.
	allExceptSkillWrite := agentScopeCoverage(slices.DeleteFunc(
		slices.Clone(ScopesForRole(AgentRoleFull)),
		func(s AgentTokenScope) bool { return s == ScopeProjectSkillWrite },
	))
	noSkillWriteCeiling := store.EffectCeiling{
		Kind:          store.EffectCeilingBounded,
		Version:       permissions.CeilingVersionV1,
		PermissionIDs: allExceptSkillWrite,
	}

	skillOnlyAgent := f.agent(t, "drop-skill-only", AgentRoleFull, state.PhaseStopped)
	f.edge(t, store.DelegationPrincipalUser, f.userID, skillOnlyAgent.ID, noSkillWriteCeiling, provSession)

	// Minting and grant.DroppedScopes are unchanged.
	grant, err := f.srv.AuthorizeAgentToken(context.Background(), skillOnlyAgent)
	require.NoError(t, err)
	assert.NotContains(t, grant.Scopes, ScopeProjectSkillWrite)
	assert.Contains(t, grant.Scopes, ScopeProjectArtifactWrite)
	assert.Equal(t, []AgentTokenScope{ScopeProjectSkillWrite}, grant.DroppedScopes)

	for name, dispatch := range map[string]func(context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := f.disp.DispatchAgentCreate(ctx, skillOnlyAgent)
			return err
		},
		"start":   func(ctx context.Context) error { return f.disp.DispatchAgentStart(ctx, skillOnlyAgent, "", false) },
		"restart": func(ctx context.Context) error { return f.disp.DispatchAgentRestart(ctx, skillOnlyAgent) },
	} {
		logBuf.Reset()
		ctx, warns := withDispatchWarnings(context.Background())
		require.NoError(t, dispatch(ctx), name)
		for _, w := range warns.Warnings() {
			assert.NotContains(t, w, "issued without the", "%s: no user-facing warning when only project:skill:write is dropped", name)
		}
		assert.Contains(t, logBuf.String(), "agent token issued without ceiling-optional scopes", name)
		assert.Contains(t, logBuf.String(), string(ScopeProjectSkillWrite), name)
	}

	// HTTP /start response also carries no user-facing dropped-scope warning.
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+skillOnlyAgent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, w := range createResponseWarnings(t, rec) {
		assert.NotContains(t, w, "issued without the")
	}

	// When both project:skill:write and project:artifact:write are dropped, the
	// user-facing warning names only project:artifact:write, and
	// project:skill:write is logged at Hub debug.
	withoutSkillAndArtifactWrite := agentScopeCoverage(slices.DeleteFunc(
		slices.Clone(ScopesForRole(AgentRoleFull)),
		func(s AgentTokenScope) bool {
			return s == ScopeProjectSkillWrite || s == ScopeProjectArtifactWrite
		},
	))
	bothCeiling := store.EffectCeiling{
		Kind:          store.EffectCeilingBounded,
		Version:       permissions.CeilingVersionV1,
		PermissionIDs: withoutSkillAndArtifactWrite,
	}
	bothAgent := f.agent(t, "drop-skill-and-artifact", AgentRoleFull, state.PhaseStopped)
	f.edge(t, store.DelegationPrincipalUser, f.userID, bothAgent.ID, bothCeiling, provSession)

	logBuf.Reset()
	ctx, warns := withDispatchWarnings(context.Background())
	require.NoError(t, f.disp.DispatchAgentStart(ctx, bothAgent, "", false))
	wantArtifactOnly := droppedScopesWarning(bothAgent, &AgentTokenGrant{DroppedScopes: []AgentTokenScope{ScopeProjectArtifactWrite}})
	require.NotEmpty(t, wantArtifactOnly)
	assert.Contains(t, warns.Warnings(), wantArtifactOnly)
	for _, w := range warns.Warnings() {
		assert.NotContains(t, w, string(ScopeProjectSkillWrite))
	}
	assert.Contains(t, logBuf.String(), string(ScopeProjectSkillWrite))
}

// withholdRoleScopesForTest simulates a pre-deploy binary whose ScopesForRole
// did not yet include scopes. Calling deploy restores the post-deploy bundle.
func withholdRoleScopesForTest(t *testing.T, scopes ...AgentTokenScope) (deploy func()) {
	t.Helper()
	prev := roleScopesWithheldForTest
	m := make(map[AgentTokenScope]bool, len(scopes))
	for _, s := range scopes {
		m[s] = true
	}
	roleScopesWithheldForTest = m
	restored := false
	restore := func() {
		if !restored {
			roleScopesWithheldForTest = prev
			restored = true
		}
	}
	t.Cleanup(restore)
	return restore
}

// TestAdditiveCeilingOptionalRoleScope_NoDelegation403OrWidening reproduces a
// Hub deploy that adds a scope (shaped like project:template:write) to
// AgentRoleFull after a principal-rooted coordinator and an agent-created lead
// are already running (ptone/scion#8324, parent ptone/scion#1515):
//   - Without ceilingOptionalRoleScopes, both stale-JWT and bounded-chain
//     delegation fail with 403.
//   - With ceilingOptionalRoleScopes, both parents create full-role children
//     with 201 (no 403 and no silent role downgrade), the bounded chain's
//     child does not gain the new scope or permissions (no privilege
//     widening) and emits the generic dropped-scope warning, and top-down
//     scope re-issue grants the new scope cleanly.
func TestAdditiveCeilingOptionalRoleScope_NoDelegation403OrWidening(t *testing.T) {
	const addedScope = ScopeProjectTemplateWrite
	deploy := withholdRoleScopesForTest(t, addedScope)

	f := newChainFixture(t, "additive-optional")
	f.srv.authzService.mintDevAuthOverride = false
	ctx := context.Background()

	// Pre-deploy binary: session user creates coord (principal ceiling), and
	// coord creates lead (bounded ceiling frozen before addedScope existed).
	coord, coordEdge := f.createdAgent(t,
		f.create(t, authUser(f.creator), CreateAgentRequest{Name: "coord", AgentRole: string(AgentRoleFull)}),
		"coord")
	require.Equal(t, store.EffectCeilingPrincipal, coordEdge.Kind)
	coordTokOld := f.agentToken(t, coord.ID)
	require.NotContains(t, f.agentIdentityFor(t, coordTokOld).Scopes(), addedScope)

	lead, leadEdge := f.createdAgent(t,
		f.createAsParent(t, coordTokOld, CreateAgentRequest{Name: "lead", AgentRole: string(AgentRoleFull)}),
		"lead")
	require.Equal(t, store.EffectCeilingBounded, leadEdge.Kind)
	require.NotContains(t, leadEdge.PermissionIDs, "template.create")
	require.NotContains(t, leadEdge.PermissionIDs, "template.update")
	leadTokOld := f.agentToken(t, lead.ID)
	require.NotContains(t, f.agentIdentityFor(t, leadTokOld).Scopes(), addedScope)

	// Deploy: AgentRoleFull now carries addedScope.
	deploy()
	require.Contains(t, ScopesForRole(AgentRoleFull), addedScope)

	// Control: if addedScope were NOT ceiling-optional, both stale-JWT and
	// refreshed bounded-chain creates fail with 403.
	recDenied := f.createAsParent(t, coordTokOld, CreateAgentRequest{Name: "denied-coord"})
	require.Equal(t, http.StatusForbidden, recDenied.Code)
	assert.Contains(t, recDenied.Body.String(), "agent lacks scope for delegation: "+string(addedScope))
	leadTokRefreshedBeforeOptional := f.agentToken(t, lead.ID)
	require.NotContains(t, f.agentIdentityFor(t, leadTokRefreshedBeforeOptional).Scopes(), addedScope)
	recDeniedLead := f.createAsParent(t, leadTokRefreshedBeforeOptional, CreateAgentRequest{Name: "denied-lead"})
	require.Equal(t, http.StatusForbidden, recDeniedLead.Code)

	// Enforced rule: addedScope is classed ceiling-optional.
	ceilingOptionalRoleScopes[addedScope] = true
	t.Cleanup(func() { delete(ceilingOptionalRoleScopes, addedScope) })

	// 1. Principal-rooted parent with stale pre-deploy JWT creates full child (201, no 403).
	c1Rec := f.createAsParent(t, coordTokOld, CreateAgentRequest{Name: "coord-child"})
	c1, _ := f.createdAgent(t, c1Rec, "coord-child")
	assert.Equal(t, string(AgentRoleFull), c1.AppliedConfig.AgentRole)
	c1Grant, err := f.srv.AuthorizeAgentToken(ctx, c1)
	require.NoError(t, err)
	assert.Contains(t, c1Grant.Scopes, addedScope, "principal-rooted child derives live authority from user creator")
	assert.Empty(t, c1Grant.DroppedScopes)

	// 2. Bounded-chain parent (both stale JWT and refreshed JWT, defaulted and
	// explicit full) creates full child with 201 and no privilege widening.
	l1Rec := f.createAsParent(t, leadTokOld, CreateAgentRequest{Name: "lead-child-default"})
	l1, l1Edge := f.createdAgent(t, l1Rec, "lead-child-default")
	assert.Equal(t, string(AgentRoleFull), l1.AppliedConfig.AgentRole, "role must stay full, not silently downgrade")
	assertDroppedScopesWarning(t, l1Rec, "lead-child-default", addedScope)
	for _, w := range createResponseWarnings(t, l1Rec) {
		assert.NotContains(t, strings.ToLower(w), "artifact", "dropped-scope warning must be generic")
	}

	leadTokRefreshed := f.agentToken(t, lead.ID)
	assert.NotContains(t, f.agentIdentityFor(t, leadTokRefreshed).Scopes(), addedScope,
		"refreshing a bounded-chain parent must not widen its scopes")
	l2Rec := f.createAsParent(t, leadTokRefreshed, CreateAgentRequest{Name: "lead-child-explicit", AgentRole: string(AgentRoleFull)})
	l2, _ := f.createdAgent(t, l2Rec, "lead-child-explicit")
	assert.Equal(t, string(AgentRoleFull), l2.AppliedConfig.AgentRole)

	// 3. No privilege widening on bounded-chain children: frozen edge, minted
	// token, ceiling evaluator, and CheckAccess all deny the new capability.
	assert.NotContains(t, l1Edge.PermissionIDs, "template.create")
	assert.NotContains(t, l1Edge.PermissionIDs, "template.update")
	l1Grant, err := f.srv.AuthorizeAgentToken(ctx, l1)
	require.NoError(t, err)
	assert.NotContains(t, l1Grant.Scopes, addedScope)
	assert.Equal(t, []AgentTokenScope{addedScope}, l1Grant.DroppedScopes)
	l1Ceiling, err := f.srv.authzService.agentRowEffectCeiling(ctx, l1)
	require.NoError(t, err)
	assert.False(t, EffectCeilingAllows(l1Ceiling, "template.create", false))
	assert.False(t, EffectCeilingAllows(l1Ceiling, "template.update", false))
	l1Identity := f.agentIdentityFor(t, f.agentToken(t, l1.ID))
	dec := f.srv.authzService.CheckAccess(ctx, l1Identity, Resource{Type: "template", ParentID: f.proj.ID}, ActionCreate)
	assert.False(t, dec.Allowed, "bounded child must not be allowed template.create: %s", dec.Reason)

	// 4. Top-down super-admin scope re-issue repairs lead and then l1.
	op := reissueOperator{UserID: DevUserID, CredentialKind: store.InitiatorCredentialKindSession}
	leadReissue, err := f.srv.runScopeReissue(ctx, lead, op, false, "")
	require.NoError(t, err)
	assert.True(t, slices.Contains(leadReissue.Added, string(addedScope)), "added=%v", leadReissue.Added)

	l1Reloaded, err := f.store.GetAgent(ctx, l1.ID)
	require.NoError(t, err)
	l1Reissue, err := f.srv.runScopeReissue(ctx, l1Reloaded, op, false, "")
	require.NoError(t, err)
	assert.True(t, slices.Contains(l1Reissue.Added, string(addedScope)), "added=%v", l1Reissue.Added)
	l1After, err := f.store.GetAgent(ctx, l1.ID)
	require.NoError(t, err)
	l1GrantAfter, err := f.srv.AuthorizeAgentToken(ctx, l1After)
	require.NoError(t, err)
	assert.Contains(t, l1GrantAfter.Scopes, addedScope)
}

// TestLegacyChainAndPreRequestReviewCeilingOptionalScope verifies design §4.8
// and §10 Q2: project:artifact:request_review is optional under ceilings. A
// legacy unrecorded delegation chain or a pre-Phase-4 bounded ceiling lacking
// artifact.request_review still fits baseline/full without a ceiling violation;
// the scope is treated as absent (dropped at mint and denied at use).
func TestLegacyChainAndPreRequestReviewCeilingOptionalScope(t *testing.T) {
	// 1. Legacy unrecorded delegation chain (EffectCeilingUnrecorded).
	legacy := store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
	assert.True(t, roleFitsCeiling(legacy, AgentRoleBaseline), "legacy unrecorded chain must still fit baseline")
	assert.True(t, roleFitsCeiling(legacy, AgentRoleFull), "legacy unrecorded chain must still fit full")
	assert.False(t, ceilingAllowsScope(legacy, ScopeProjectArtifactRequestReview), "legacy chain must not be issued project:artifact:request_review")
	assert.False(t, EffectCeilingAllows(legacy, "artifact.request_review", false), "legacy chain must be denied artifact.request_review at use")
	legacyIssued := filterScopes(ScopesForRole(AgentRoleBaseline), legacy, ScopeCeilings{})
	assert.NotContains(t, legacyIssued, ScopeProjectArtifactRequestReview)
	assert.Contains(t, legacyIssued, ScopeProjectRead)

	// 2. Bounded ceiling that predates artifact.request_review (carries
	//    artifact.read + artifact.create, lacks artifact.request_review).
	preReqReview := boundedCeiling(
		"harness_config.list", "harness_config.read", "project.read",
		"skill.list", "skill.read", "template.list", "template.read",
		"agent.create", "artifact.read", "artifact.create",
	)
	assert.True(t, roleFitsCeiling(preReqReview, AgentRoleBaseline), "pre-request_review ceiling must still fit baseline")
	role, cause, ok := childRoleWithinCeiling(preReqReview, AgentRoleBaseline, true)
	require.True(t, ok, "explicit baseline must not fail ceiling check: %q", cause)
	assert.Equal(t, AgentRoleBaseline, role)

	issued := filterScopes(ScopesForRole(AgentRoleBaseline), preReqReview, ScopeCeilings{})
	assert.Contains(t, issued, ScopeProjectArtifactRead)
	assert.Contains(t, issued, ScopeProjectArtifactWrite)
	assert.NotContains(t, issued, ScopeProjectArtifactRequestReview, "mint filter must drop optional scope absent from ceiling")
	assert.False(t, EffectCeilingAllows(preReqReview, "artifact.request_review", false))
}
