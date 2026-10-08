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
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArtifactHostPermits checks the credential question per identity type.
// A scoped user access token keeps its own restrictions: it is not treated
// as the plain user behind it.
func TestArtifactHostPermits(t *testing.T) {
	srv, s := testServer(t)
	host := newArtifactHost(srv)
	// A real agent with a recorded delegation edge, so its chain allows the
	// artifact permissions (agentChainAllows).
	p1 := artifactProject(t, s, "permits-p1")
	chained, _ := artifactAgent(t, srv, s, p1.ID, "permits-agent", AgentRoleFull)
	agent := func(scopes ...AgentTokenScope) Identity { return artifactTestAgent(chained.ID, p1.ID, scopes...) }
	alice := NewAuthenticatedUser("user-1", "alice@example.com", "Alice", "member", "api")
	uat := func(project string, scopes ...string) Identity { return artifactTestUAT(t, alice, project, scopes...) }
	synthetic := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: "agent-1"}, ProjectID: "p1", Scopes: ScopesForRole(AgentRoleFull),
	}}

	cases := []struct {
		name     string
		identity Identity
		scope    string
		perm     string
		want     bool
	}{
		{"session user", alice, "p1", artifacts.PermissionRead, true},
		{"session user create", alice, "p1", artifacts.PermissionCreate, true},
		{"dev user", NewDevUser(DevUserConfig{Username: "dev"}), "p1", artifacts.PermissionRead, true},

		{"UAT with artifact:read in its project", uat("p1", "artifact:read"), "p1", artifacts.PermissionRead, true},
		{"UAT without artifact:read", uat("p1", "agent:read"), "p1", artifacts.PermissionRead, false},
		{"UAT bounded to another project", uat("p2", "artifact:read"), "p1", artifacts.PermissionRead, false},
		{"UAT read scope does not create", uat("p1", "artifact:read"), "p1", artifacts.PermissionCreate, false},
		{"UAT with artifact:create", uat("p1", "artifact:create"), "p1", artifacts.PermissionCreate, true},

		{"agent with the read scope", agent(ScopeProjectArtifactRead), p1.ID, artifacts.PermissionRead, true},
		{"agent read scope, artifact homed elsewhere", agent(ScopeProjectArtifactRead), "p2", artifacts.PermissionRead, true},
		{"agent without the read scope", agent(ScopeProjectRead), p1.ID, artifacts.PermissionRead, false},
		{"agent without the write scope", agent(ScopeProjectArtifactRead), p1.ID, artifacts.PermissionCreate, false},
		{"agent with the write scope", agent(ScopeProjectArtifactWrite), p1.ID, artifacts.PermissionCreate, true},
		{"agent delete is never permitted", agent(ScopeProjectArtifactRead, ScopeProjectArtifactWrite), p1.ID, artifacts.PermissionDelete, false},
		{"agent with scopes but no delegation edge", artifactTestAgent("agent-without-edge", p1.ID, ScopeProjectArtifactRead), p1.ID, artifacts.PermissionRead, false},
		{"in-process agent identity without a token", synthetic, "p1", artifacts.PermissionRead, false},

		{"federated agent", NewFederatedAgentIdentity("https://other.example.com", "remote-agent", "remote-proj", "remote", "root", nil, nil), "p1", artifacts.PermissionRead, false},
		{"broker", NewBrokerIdentity("broker-1"), "p1", artifacts.PermissionRead, false},
		{"empty scope", alice, "", artifacts.PermissionRead, false},
		{"non-artifact permission", alice, "p1", "agent.read", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := contextWithIdentity(context.Background(), tc.identity)
			assert.Equal(t, tc.want, host.Permits(ctx, tc.scope, tc.perm))
		})
	}
	assert.False(t, host.Permits(context.Background(), "p1", artifacts.PermissionRead), "no identity")
}

// artifactTestUAT builds a current-version (V1) user access token bounded
// to project, whose ceiling is exactly the registry permissions of scopes.
func artifactTestUAT(t *testing.T, user UserIdentity, project string, scopes ...string) *ScopedUserIdentity {
	t.Helper()
	return NewScopedUserIdentityWithCeiling(user, project, scopes, "uat-"+project,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: uatCeilingFromSelectors(t, scopes...).PermissionIDs})
}

// TestArtifactHostIgnoresInProcessAgentIdentities: identities the hub
// builds in process for its own decisions (no token id, unfiltered role
// scopes) are never served by the artifact service.
func TestArtifactHostIgnoresInProcessAgentIdentities(t *testing.T) {
	host := newArtifactHost(&Server{})
	synthetic := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: "agent-1"}, ProjectID: "p1", Scopes: ScopesForRole(AgentRoleFull),
	}}
	ctx := contextWithIdentity(context.Background(), synthetic)
	_, _, _, ok := host.Principal(ctx)
	assert.False(t, ok)
	assert.False(t, host.Permits(ctx, "p1", artifacts.PermissionRead))
	assert.False(t, host.Permits(ctx, "p1", artifacts.PermissionCreate))
}

var allArtifactPermissions = []string{
	artifacts.PermissionRead, artifacts.PermissionCreate, artifacts.PermissionUpdate,
	artifacts.PermissionDelete, artifacts.PermissionManage,
}

func v1Ceiling(ids ...string) permissions.FrozenPermissionCeiling {
	return permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: ids}
}

func permitsCtx(identity Identity) context.Context {
	return contextWithIdentity(context.Background(), identity)
}

// TestArtifactHostPermits_ScopedUserTokenKeepsItsLimits: a scoped user access
// token is never collapsed into its plain user. Its ceiling bounds every
// permission, including on artifacts its user owns, and its project boundary
// is absolute, including where its user holds a grant elsewhere.
func TestArtifactHostPermits_ScopedUserTokenKeepsItsLimits(t *testing.T) {
	host := newArtifactHost(&Server{})
	user := NewAuthenticatedUser("u1", "u1@example.com", "U1", "member", "web")

	readOnlyP1 := NewScopedUserIdentityWithCeiling(user, "proj-1", []string{"artifact:read"}, "uat-1", v1Ceiling("artifact.read"))
	assert.True(t, host.Permits(permitsCtx(readOnlyP1), "proj-1", artifacts.PermissionRead))
	assert.False(t, host.Permits(permitsCtx(readOnlyP1), "proj-2", artifacts.PermissionRead), "the project boundary is absolute")
	for _, p := range []string{artifacts.PermissionCreate, artifacts.PermissionUpdate, artifacts.PermissionDelete, artifacts.PermissionManage} {
		assert.False(t, host.Permits(permitsCtx(readOnlyP1), "proj-1", p), "the ceiling bounds %s", p)
	}

	noArtifacts := NewScopedUserIdentityWithCeiling(user, "proj-1", []string{"agent:read"}, "uat-2", v1Ceiling("agent.read"))
	for _, p := range allArtifactPermissions {
		assert.False(t, host.Permits(permitsCtx(noArtifacts), "proj-1", p), "no artifact permission in the ceiling: %s", p)
		assert.True(t, host.Permits(permitsCtx(user), "proj-1", p), "the same user's session is not limited: %s", p)
	}
	_, ref, _, ok := host.Principal(permitsCtx(noArtifacts))
	assert.True(t, ok && ref == "u1", "the token is identified as its user")
	assert.False(t, host.Permits(permitsCtx(noArtifacts), "proj-1", artifacts.PermissionRead),
		"but its own limits still apply: no artifact.read, whatever its user owns")

	_, ref, _, ok = host.Principal(permitsCtx(readOnlyP1))
	assert.True(t, ok)
	assert.Equal(t, "u1", ref, "the token is served as its user, with its own limits kept by Permits")

	hubWide := NewScopedUserIdentityWithBoundaryAndDecoration(user, TokenBoundary{Kind: BoundaryKindHub},
		[]string{"artifact:read", "artifact:create"}, "uat-3", v1Ceiling("artifact.create", "artifact.read"), nil)
	for _, project := range []string{"proj-1", "proj-2"} {
		assert.True(t, host.Permits(permitsCtx(hubWide), project, artifacts.PermissionRead), project)
		assert.True(t, host.Permits(permitsCtx(hubWide), project, artifacts.PermissionCreate), project)
		assert.False(t, host.Permits(permitsCtx(hubWide), project, artifacts.PermissionDelete), project)
	}
}

// TestLegacyChainsWithholdOnlyArtifactScopes: a chain with no recorded
// bound (unrecorded ceiling) keeps every scope it was issued before and is
// only not extended the artifact scopes; a principal chain is unchanged.
func TestLegacyChainsWithholdOnlyArtifactScopes(t *testing.T) {
	assert.Equal(t, map[string]bool{"artifact.create": true, "artifact.read": true, "artifact.update": true}, legacyChainExcludedPermissions)

	unrecorded := store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
	principal := store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	for _, role := range []AgentRole{AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull} {
		candidates := append(append([]AgentTokenScope{}, ScopesForRole(role)...), ScopeProjectArtifactWrite, GCPTokenScopeForSA("sa-1"))
		var want []AgentTokenScope
		for _, s := range candidates {
			if !ceilingOptionalRoleScopes[s] {
				want = append(want, s)
			}
		}
		assert.Equal(t, want, filterScopes(candidates, unrecorded, ScopeCeilings{}), "%s: every pre-existing scope is kept", role)
		assert.Equal(t, candidates, filterScopes(candidates, principal, ScopeCeilings{}), "%s: principal chains are unchanged", role)
	}
	for _, p := range []string{"artifact.read", "artifact.create", "artifact.update"} {
		assert.False(t, EffectCeilingAllows(unrecorded, p, false), "unrecorded denies %s at use", p)
		assert.True(t, EffectCeilingAllows(principal, p, false), "principal allows %s", p)
		edge := &store.DelegationEdge{}
		edge.EffectCeiling = unrecorded
		edge.ProvenanceVersion = store.ProvenanceVersionV1
		cause, _ := hopEffectCeilingDeny(edge, p, Resource{Type: "artifact", ParentType: "project", ParentID: "p"}, "a1", false)
		assert.Equal(t, DenyCauseCeilingUnrecorded, cause, "unrecorded hop denies %s", p)
	}
}

// TestMigrationSentinelWithholdsOnlyArtifactRead: a backfilled edge keeps
// its non-sensitive reads and does not gain artifact.read.
func TestMigrationSentinelWithholdsOnlyArtifactRead(t *testing.T) {
	a := &AuthzService{}
	edge := &store.DelegationEdge{Role: string(AgentRoleFull)}
	allowed, reason, err := a.migrationSentinelCeiling(Resource{Type: "template", ParentType: "project", ParentID: "p"}, ActionRead, "a1", edge, "template.read", nil)
	assert.NoError(t, err)
	assert.True(t, allowed, "pre-existing read kept: %s", reason)
	allowed, reason, err = a.migrationSentinelCeiling(Resource{Type: "project", ID: "p"}, ActionRead, "a1", edge, "project.read", nil)
	assert.NoError(t, err)
	assert.True(t, allowed, "pre-existing read kept: %s", reason)
	allowed, _, err = a.migrationSentinelCeiling(Resource{Type: "artifact", ParentType: "project", ParentID: "p"}, ActionRead, "a1", edge, "artifact.read", nil)
	assert.NoError(t, err)
	assert.False(t, allowed, "artifact.read is withheld")
}

// TestArtifactHostAuthorizeImpliesPermits: Authorize applies the
// credential's own limits first, so an identity the hub builds in process
// (no token id) is refused even when it carries the read scope. The agent
// has a recorded delegation and the backfill is complete, so Permits is the
// only reason left for the deny; the control shows the same agent with a
// token id is authorized.
func TestArtifactHostAuthorizeImpliesPermits(t *testing.T) {
	srv, s := testServer(t)
	host := newArtifactHost(srv)
	agent := createTestAgent(t, s)
	delegator := createScopeSuperAdmin(t, s, "artifact-delegator-tokenless")
	addRecordedArtifactEdge(t, s, delegator.ID, agent.ID, agent.ProjectID)
	markEdgeBackfillComplete(t, s)
	tokenless := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: agent.ID}, ProjectID: agent.ProjectID,
		Scopes: []AgentTokenScope{ScopeProjectRead, ScopeProjectArtifactRead}, ScopeSchema: CurrentAgentScopeSchema,
	}}
	ctx := contextWithIdentity(context.Background(), tokenless)
	assert.False(t, host.Permits(ctx, agent.ProjectID, artifacts.PermissionRead))
	assert.False(t, host.Authorize(ctx, agent.ProjectID, artifacts.PermissionRead))

	withToken := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: agent.ID, ID: "jti-" + agent.ID}, ProjectID: agent.ProjectID,
		Scopes: []AgentTokenScope{ScopeProjectRead, ScopeProjectArtifactRead}, ScopeSchema: CurrentAgentScopeSchema,
	}}
	assert.True(t, host.Authorize(contextWithIdentity(context.Background(), withToken), agent.ProjectID, artifacts.PermissionRead),
		"control: the same agent with a token id is authorized, so the tokenless deny comes from Permits")
}

// TestArtifactHostServesGenuinelyMintedAgentToken uses the real create
// handler and the real mint and validation: a token minted for an agent
// whose source allows artifact.read carries a token id and the read scope,
// and is served; one whose source does not allow it is not.
func TestArtifactHostServesGenuinelyMintedAgentToken(t *testing.T) {
	ctx := context.Background()
	f := newUATCreateFixture(t, "artifact-real-token")
	f.srv.authzService.mintDevAuthOverride = false
	host := newArtifactHost(f.srv)

	mintedIdentity := func(name string, selectors ...string) (*store.Agent, context.Context) {
		t.Helper()
		rec := f.create(t, f.uat(t, selectors...), CreateAgentRequest{Name: name})
		agent, _ := f.createdAgent(t, rec, name)
		tok, err := f.srv.issueAgentTokenForTest(ctx, agent)
		require.NoError(t, err)
		claims, err := f.srv.agentTokenService.ValidateAgentToken(tok)
		require.NoError(t, err)
		require.NotEmpty(t, claims.ID, "a minted token carries a token id")
		return agent, contextWithIdentity(ctx, &agentIdentityWrapper{claims})
	}

	agent, withRead := mintedIdentity("artifact-real-token", append(minimalSelectors(t), "artifact:read")...)
	kind, ref, home, ok := host.Principal(withRead)
	assert.True(t, ok)
	assert.Equal(t, artifacts.PrincipalKindAgent, kind)
	assert.Equal(t, agent.ID, ref)
	assert.Equal(t, agent.ProjectID, home)
	assert.True(t, host.Permits(withRead, agent.ProjectID, artifacts.PermissionRead))
	assert.False(t, host.Permits(withRead, agent.ProjectID, artifacts.PermissionCreate))

	other, withoutRead := mintedIdentity("artifact-real-token-noread", minimalSelectors(t)...)
	_, _, _, ok = host.Principal(withoutRead)
	assert.False(t, ok, "a token whose source did not allow artifact.read is not served")
	assert.False(t, host.Permits(withoutRead, other.ProjectID, artifacts.PermissionRead))
}
