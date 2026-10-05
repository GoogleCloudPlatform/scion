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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preArtifactReadCeiling is a bounded ceiling frozen before artifacts
// existed: exactly project:read's coverage at that time, plus agent.create,
// as a UAT minted with the read selectors and agent:create carries.
func preArtifactReadCeiling() store.EffectCeiling {
	return boundedCeiling(
		"harness_config.list", "harness_config.read", "project.read",
		"skill.list", "skill.read", "template.list", "template.read",
		"agent.create",
	)
}

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

// TestCeilingOptionalRoleScopesAreExactlyTheArtifactScopes keeps the set of
// role scopes that do not decide role fit or delegation to exactly the two
// artifact scopes; any addition needs its own review.
func TestCeilingOptionalRoleScopesAreExactlyTheArtifactScopes(t *testing.T) {
	assert.Equal(t, map[AgentTokenScope]bool{
		ScopeProjectArtifactRead:  true,
		ScopeProjectArtifactWrite: true,
	}, ceilingOptionalRoleScopes)
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

	parent, parentEdge := f.createdAgent(t, f.create(t, uat, CreateAgentRequest{Name: "artifact-parent", AgentRole: string(AgentRoleFull)}), "artifact-parent")
	require.Equal(t, store.EffectCeilingBounded, parentEdge.Kind)
	parentScopes, err := f.srv.authzService.ceilingFilteredAgentScopes(context.Background(), parent, f.srv.authzService.mintCandidateScopes(parent))
	require.NoError(t, err)
	require.Contains(t, parentScopes, ScopeAgentCreate)
	require.NotContains(t, parentScopes, ScopeProjectArtifactRead, "the parent's own token lacks the read scope")

	child, childEdge := f.childOf(t, parent, "artifact-child")
	assert.NotContains(t, childEdge.PermissionIDs, "artifact.read")
	assert.NotContains(t, childEdge.PermissionIDs, "artifact.create")
	assert.NotContains(t, childEdge.PermissionIDs, "artifact.update")

	authz := f.srv.authzService
	issued, err := authz.ceilingFilteredAgentScopes(context.Background(), child, append(authz.mintCandidateScopes(child), ScopeProjectArtifactWrite))
	require.NoError(t, err)
	assertNoArtifactAccess(t, newArtifactHost(f.srv), child.ID, child.ProjectID, issued)
}
