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

// Parity coverage for enforceUATConstraints' step-1 scope gate (authz.go):
// the gate now evaluates the credential's frozen permission ceiling
// (scoped.Ceiling().Allows(permissionID)) instead of a raw
// scoped.HasScope(resource:action) string comparison, so that this gate and
// the later restriction Decide applies (7a) share one source of truth. These
// tests pin that the switch is behavior-preserving and narrowing-only.

import (
	"context"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUATCeilingStep1_LegacyAttachOnlyRealEndpointParity pins (a): a legacy,
// pre-ceiling UAT row (CeilingVersion/CeilingPermissionIDs unset,
// normalized on load from raw Scopes, never from live ResolveSelector)
// authorizes attach and denies port_access at the real HTTP endpoints,
// exactly as it did before step 1 switched from HasScope to Ceiling().Allows.
func TestUATCeilingStep1_LegacyAttachOnlyRealEndpointParity(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("step1-legacy-endpoint-project")
	ownerID := tid("step1-legacy-endpoint-owner")
	memberID := tid("step1-legacy-endpoint-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)
	uatpExposePort(t, s, agent, 8099)

	uatKey, _ := seedLegacyTokenInStore(t, s, memberID, projectID, []string{"agent:attach"})

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	requireAuthorizedPTY(t, rec, "a legacy attach-only UAT must still authorize attach at the real endpoint: %s", rec.Body.String())

	rec = doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/8099/proxy/", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "a legacy attach-only UAT must still be denied port_access at the real endpoint: %s", rec.Body.String())
}

// TestUATCeilingStep1_AliasSelectorMintTimeParity pins (b): a token minted
// (through the real production mint path, not a simulated legacy row) with
// the agent:manage alias selector keeps exactly its expanded permission set
// — lifecycle allowed, attach/port_access excluded (ExcludeFromManageAlias)
// — under the new Ceiling().Allows gate, matching the pre-switch behavior
// where HasScope checked the same mint-time-expanded concrete scopes.
func TestUATCeilingStep1_AliasSelectorMintTimeParity(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid("step1-alias-owner")
	project := &store.Project{ID: tid("step1-alias-project"), Name: "p", Slug: "step1-alias-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "step1-alias@example.com", project.ID, store.ProjectRoleOwner)

	agent := &store.Agent{ID: tid("step1-alias-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	uatKey := mintScopedUAT(t, srv, ownerID, project.ID, []string{"agent:manage"})
	scoped, err := srv.uatService.ValidateToken(ctx, uatKey)
	require.NoError(t, err)

	resource := Resource{Type: "agent", ID: agent.ID, OwnerID: ownerID, ParentType: "project", ParentID: project.ID}

	lifecycleDecision := srv.authzService.CheckAccess(ctx, scoped, resource, ActionLifecycle)
	assert.True(t, lifecycleDecision.Allowed, "a mint-time agent:manage token must keep lifecycle: %+v", lifecycleDecision)

	attachDecision := srv.authzService.CheckAccess(ctx, scoped, resource, ActionAttach)
	assert.False(t, attachDecision.Allowed, "a mint-time agent:manage token must still exclude attach (ExcludeFromManageAlias)")
}

// TestUATCeilingStep1_ExactScopeDenialIsCeilingGated pins (c): a ceiling
// that does not include the resolved permission is denied at step 1, with a
// control assertion that the permission the ceiling DOES hold still passes.
// Removing the Ceiling().Allows check in enforceUATConstraints fails the
// deny assertion. Inverting it fails both assertions.
func TestUATCeilingStep1_ExactScopeDenialIsCeilingGated(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	ownerID := tid("step1-exact-owner")
	project := &store.Project{ID: tid("step1-exact-project"), Name: "p", Slug: "step1-exact-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "step1-exact@example.com", project.ID, store.ProjectRoleOwner)

	agent := &store.Agent{ID: tid("step1-exact-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	base := NewAuthenticatedUser(ownerID, "step1-exact@example.com", "Owner", "member", "api")
	// A ceiling holding only agent.read: agent.delete is absent.
	scoped := NewScopedUserIdentityWithCeiling(base, project.ID, []string{"agent:read"}, "", permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersionV1,
		PermissionIDs: []string{"agent.read"},
	})
	principal := principalContextForIdentity(scoped)
	resource := Resource{Type: "agent", ID: agent.ID, OwnerID: ownerID, ParentType: "project", ParentID: project.ID}

	deny := authz.enforceUATConstraints(ctx, principal, scoped, resource, ActionDelete, "agent.delete")
	require.NotNil(t, deny, "a ceiling that does not include agent.delete must be denied at step 1")
	assert.False(t, deny.Allowed)
	assert.Contains(t, deny.Reason, "token does not have scope: agent:delete")

	// Control: the permission the ceiling DOES hold passes step 1 (returns nil).
	allow := authz.enforceUATConstraints(ctx, principal, scoped, resource, ActionRead, "agent.read")
	assert.Nil(t, allow, "the ceiling's own permission must still pass step 1")
}

// TestUATCeilingStep1_NoWideningDespiteLiveProjectAuthority pins (d): a
// narrow (read-only) UAT is denied agent.delete even though the same user's
// live project-owner role grants it outside any UAT (projectOwnerPermissionIDs
// includes agent.delete). The control assertion proves the live authority is
// real, isolating the denial to the token's ceiling, not an absence of
// underlying authority.
func TestUATCeilingStep1_NoWideningDespiteLiveProjectAuthority(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	ownerID := tid("step1-nowiden-owner")
	project := &store.Project{ID: tid("step1-nowiden-project"), Name: "p", Slug: "step1-nowiden-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	createTestUserWithProjectRole(t, s, ownerID, "step1-nowiden@example.com", project.ID, store.ProjectRoleOwner)

	agent := &store.Agent{ID: tid("step1-nowiden-agent"), Slug: "a", Name: "a", ProjectID: project.ID, OwnerID: ownerID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	base := NewAuthenticatedUser(ownerID, "step1-nowiden@example.com", "Owner", "member", "api")
	// A narrow, read-only UAT for a user whose live role authority is
	// broader than the token.
	scoped := legacyScopedIdentity(base, project.ID, []string{"agent:read"})

	resource := Resource{Type: "agent", ID: agent.ID, OwnerID: ownerID, ParentType: "project", ParentID: project.ID}
	decision := authz.CheckAccess(ctx, scoped, resource, ActionDelete)
	assert.False(t, decision.Allowed, "a read-only UAT must not inherit the user's live agent.delete authority")

	// Control: the same live authority, without the narrow UAT, is allowed.
	controlDecision := authz.CheckAccess(ctx, base, resource, ActionDelete)
	assert.True(t, controlDecision.Allowed, "sanity: the user's live role does grant agent.delete outside any UAT")
}

// TestCreateTokenWithParams_DeniedSelectorLeavesNoTokenRow pins the binding
// condition on CreateTokenWithParams's mint path: CanMintSelector (the
// eligibility gate) and BuildCeilingFromSelectors (the persisted-ceiling
// resolver) are called with the exact same expanded-selector slice — no
// separate re-derivation — and both run, and can fail closed, strictly
// before the WithTx token-insert transaction. A denied selector must
// therefore never reach the insert: no row is created for the attempt, not
// even a revoked/expired one.
func TestCreateTokenWithParams_DeniedSelectorLeavesNoTokenRow(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("no-row-on-denial-member")
	project := &store.Project{ID: tid("no-row-on-denial-project"), Name: "p", Slug: "no-row-on-denial-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	// A plain project member: projectMemberCuratedPermissionIDs does not
	// include agent.delete (owner/admin only), and the member has no
	// relationship candidacy for it either (agent.delete is not a
	// relationship-mintable selector).
	createTestUserWithProjectRole(t, s, memberID, "no-row-on-denial@example.com", project.ID, store.ProjectRoleMember)

	before, err := s.ListUserAccessTokens(ctx, memberID)
	require.NoError(t, err)
	require.Empty(t, before, "precondition: no tokens exist yet for this user")

	_, _, mintErr := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "denied-mint",
		project.ID, []string{"agent:delete"}, nil)
	var sv *UATScopeViolationError
	require.ErrorAs(t, mintErr, &sv, "a plain member must not be able to mint agent:delete")
	assert.Equal(t, "agent:delete", sv.Selector)
	assert.NotErrorIs(t, mintErr, ErrUATProjectForbidden)

	after, err := s.ListUserAccessTokens(ctx, memberID)
	require.NoError(t, err)
	assert.Empty(t, after, "a denied selector must leave no token row")
}
