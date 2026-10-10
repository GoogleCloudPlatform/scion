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

package hub

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adtTestIdentity returns a DelegatedAgentIdentity for unit tests,
// with a hub boundary and an agent.read ceiling.
func adtTestIdentity(agentID, projectID string) *DelegatedAgentIdentity {
	return &DelegatedAgentIdentity{
		agentID:             agentID,
		agentProjectID:      projectID,
		authorizingUserID:   "delegation-test-issuer",
		grantID:             "delegation-test-grant",
		credentialID:        "delegation-test-credential",
		exchangeAgentCredID: "delegation-test-agent-credential",
		boundary:            TokenBoundary{Kind: BoundaryKindHub},
		ceiling:             permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.read"}},
	}
}

// adtFakeTypeIdentity reports the delegated Type() string without
// being *DelegatedAgentIdentity.
type adtFakeTypeIdentity struct{}

func (adtFakeTypeIdentity) ID() string   { return "fake-delegated" }
func (adtFakeTypeIdentity) Type() string { return "agent_delegated" }

// TestAgentDelegation_ClassificationIsByConcreteType: only the concrete type
// classifies as the delegated pair; a look-alike Type() string classifies
// to nothing and Decide denies it.
func TestAgentDelegation_ClassificationIsByConcreteType(t *testing.T) {
	id := adtTestIdentity("agent-x", "project-x")
	pc := principalContextForIdentity(id)
	cc := credentialContextForIdentity(id)
	assert.Equal(t, PrincipalKindAgentDelegated, pc.Kind)
	assert.Equal(t, "agent-x", pc.ID)
	assert.Equal(t, CredentialKindDelegatedAgent, cc.Kind)
	assert.Equal(t, "delegation-test-credential", cc.ID)
	require.NotNil(t, cc.Boundary)
	assert.Equal(t, BoundaryKindHub, cc.Boundary.Kind)
	assert.Equal(t, []string{"agent.read"}, cc.Ceiling.PermissionIDs)
	assert.False(t, AncestryIsHubAttested(id))

	ctx := contextWithIdentity(context.Background(), id)
	assert.Nil(t, GetUserIdentityFromContext(ctx))
	assert.Nil(t, GetAgentIdentityFromContext(ctx))
	assert.Same(t, id, GetIdentityFromContext(ctx))

	fake := adtFakeTypeIdentity{}
	assert.Empty(t, principalContextForIdentity(fake).Kind)
	assert.Empty(t, credentialContextForIdentity(fake).Kind)
	a := &AuthzService{}
	d := a.decide(context.Background(), AuthzRequest{Principal: PrincipalContext{Identity: fake}, Resource: Resource{Type: "agent", ID: "a"}, Action: ActionRead})
	assert.False(t, d.Allowed)
}

// TestAgentDelegation_DecideWithoutRequestStateDenies: a delegated identity
// that did not come from the middleware (no request state, or a different
// identity object) and a request with no admitted route both deny, marked
// for audit and attributed to agent delegation.
func TestAgentDelegation_DecideWithoutRequestStateDenies(t *testing.T) {
	a := &AuthzService{}
	id := adtTestIdentity("agent-x", "project-x")
	req := AuthzRequest{Principal: PrincipalContext{Identity: id}, Resource: Resource{Type: "agent", ID: "agent-y", ParentType: "project", ParentID: "project-x"}, Action: ActionRead}

	d := a.Decide(context.Background(), req)
	assert.False(t, d.Allowed)
	assert.True(t, d.AlwaysAudit)
	assert.Equal(t, DeniedByAgentDelegation, d.DeniedBy)
	require.NotNil(t, d.AgentDelegation)
	assert.Equal(t, agentDelegationCodeStateMissing, d.AgentDelegation.AgentDelegationCode)
	assert.Equal(t, "agent-x", d.AgentDelegation.ActorAgentID)
	assert.Equal(t, "delegation-test-issuer", d.AgentDelegation.AuthorizingUserID)
	assert.Equal(t, "delegation-test-grant", d.AgentDelegation.SourceGrantID)
	assert.Equal(t, actorKindAgentDelegated, d.AgentDelegation.ActorKind)

	// State present, but no route admission in the context.
	ctx := contextWithDelegatedState(context.Background(), &delegatedRequestState{identity: id})
	d = a.Decide(ctx, req)
	assert.False(t, d.Allowed)
	assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, d.AgentDelegation.AgentDelegationCode)

	// State for another identity object with the same values.
	ctx = contextWithDelegatedState(context.Background(), &delegatedRequestState{identity: adtTestIdentity("agent-x", "project-x")})
	d = a.Decide(ctx, req)
	assert.Equal(t, agentDelegationCodeStateMissing, d.AgentDelegation.AgentDelegationCode)

	// Admitted route, but no server hooks (the experiment cannot be read).
	ctx = contextWithDelegatedState(context.Background(), &delegatedRequestState{identity: id})
	ctx = contextWithDelegatedAdmission(ctx, agentDelegationAdmittedRoutes[0])
	d = a.Decide(ctx, req)
	assert.Equal(t, agentDelegationCodeExperimentDisabled, d.AgentDelegation.AgentDelegationCode)
}

// TestAgentDelegation_SupplyingTheDelegatedKindToAnotherIdentityDenies: a
// caller cannot make an ordinary identity look delegated, or the reverse.
func TestAgentDelegation_SupplyingTheDelegatedKindToAnotherIdentityDenies(t *testing.T) {
	a := &AuthzService{}
	user := NewAuthenticatedUser("u1", "u1@example.com", "U1", "member", "web")
	d := a.decide(context.Background(), AuthzRequest{
		Principal: PrincipalContext{Identity: user}, Credential: CredentialContext{Kind: CredentialKindDelegatedAgent},
		Resource: Resource{Type: "agent", ID: "a"}, Action: ActionRead,
	})
	assert.False(t, d.Allowed)
	d = a.decide(context.Background(), AuthzRequest{
		Principal: PrincipalContext{Identity: adtTestIdentity("a", "p")}, Credential: CredentialContext{Kind: CredentialKindInteractive},
		Resource: Resource{Type: "agent", ID: "a"}, Action: ActionRead,
	})
	assert.False(t, d.Allowed)
}

// TestAgentDelegation_PermissiveHelpersRefuse pins the explicit arms of
// the shared helpers that would otherwise treat an unknown identity
// permissively (.design/agent-delegation.md §12.3).
func TestAgentDelegation_PermissiveHelpersRefuse(t *testing.T) {
	id := adtTestIdentity("agent-x", "project-x")
	a := &AuthzService{}

	d := a.CanDelegate(context.Background(), id, GrantDescriptor{Type: GrantTypeAgentDelegation, AgentRole: string(AgentRoleNone), ProjectID: "project-x"})
	assert.False(t, d.Allowed)
	assert.Equal(t, "delegated credential cannot delegate", d.Reason)
	assert.Empty(t, a.intersectCredentialCaveats(id, []string{"agent.read", "project.read"}))

	s := &Server{authzService: a}
	allowed, err := s.catalogListReadBatch(id)(context.Background(), id, []Resource{{Type: "template", ID: "t1", ScopeKind: store.TemplateScopeGlobal}, {Type: "template", ID: "t2"}})
	require.NoError(t, err)
	assert.Equal(t, []bool{false, false}, allowed)

	_, err = a.ResolveListScopes(context.Background(), id, "agent.list")
	assert.Error(t, err)

	binding := scopedCursorBinding("agents", map[string]string{}, id)
	other := adtTestIdentity("agent-x", "project-x")
	other.credentialID = "another-credential"
	assert.NotEqual(t, binding, scopedCursorBinding("agents", map[string]string{}, other), "a cursor is bound to the credential")

	target := &store.Agent{ID: "agent-y", ProjectID: "project-x"}
	denial := s.authorizeAgentTargetAction(context.Background(), id, target, ActionLifecycle)
	require.NotNil(t, denial)
	assert.Equal(t, http.StatusForbidden, denial.status)

	ok, _, _ := s.authorizeAgentMessage(context.Background(), id, target, false)
	assert.False(t, ok)
	ok, _, _ = s.authorizeAgentMessage(context.Background(), id, target, true)
	assert.False(t, ok, "not even the system plane admits a delegated sender")
	assert.False(t, s.ComputeMessageability(context.Background(), id, target).CanMessage)

	assert.False(t, s.envViewAllowed(context.Background(), id, target, &Capabilities{Actions: []string{string(ActionAttach)}}))
	ctx := contextWithIdentity(context.Background(), id)
	assert.False(t, canViewAgentEnv(ctx, s, target))

	caps := &Capabilities{Actions: []string{}}
	s.addAgentCreateIfAnyProjectAllows(ctx, id, caps)
	assert.Empty(t, caps.Actions)

	assert.Empty(t, a.ComputeScopeCapabilities(ctx, id, "project", "project-x", "agent").Actions)
}

// TestAgentDelegation_CapabilitiesArePrimaryActionOnly: on an admitted
// route a delegated caller's capabilities are the route's primary action on
// the route's own target, and nothing elsewhere.
func TestAgentDelegation_CapabilitiesArePrimaryActionOnly(t *testing.T) {
	a := &AuthzService{}
	id := adtTestIdentity("agent-x", "project-x")
	target := &store.Agent{ID: "agent-y", ProjectID: "project-x"}

	// No admission in the context: nothing.
	assert.Empty(t, a.ComputeCapabilities(context.Background(), id, agentResource(target)).Actions)

	ctx := contextWithDelegatedAdmission(context.Background(), agentDelegationAdmittedRoutes[0])
	ctx = withAgentSubRoute(ctx, AgentSubRoute{RouteID: AgentRouteRoot, OperationID: opAgentRead, Method: http.MethodGet, AgentID: target.ID})
	assert.Equal(t, []string{string(ActionRead)}, a.ComputeCapabilities(ctx, id, agentResource(target)).Actions)

	other := &store.Agent{ID: "agent-z", ProjectID: "project-x"}
	assert.Empty(t, a.ComputeCapabilities(ctx, id, agentResource(other)).Actions, "another resource gets nothing")
	batch := a.ComputeCapabilitiesBatch(ctx, id, []Resource{agentResource(target), agentResource(other)}, "agent")
	assert.Equal(t, []string{string(ActionRead)}, batch[0].Actions)
	assert.Empty(t, batch[1].Actions)
	forActions := a.ComputeCapabilitiesForActions(ctx, id, []Resource{agentResource(target)}, []Action{ActionRead, ActionAttach})
	assert.Equal(t, []string{string(ActionRead)}, forActions[0].Actions)
}

// TestAgentDelegationAdmission_MatchesCatalog pins the admission table
// against the catalog in both directions: every entry names an exact agent
// sub-route resolution whose operation is the entry's and admits the
// delegated pair, and every catalog operation that admits the delegated
// pair has an entry. Every policy key is the primary permission of an
// admitted operation, and every admitted operation's primary permission is
// a policy key.
func TestAgentDelegationAdmission_MatchesCatalog(t *testing.T) {
	tableOps := map[authzop.OperationID]bool{}
	for _, entry := range agentDelegationAdmittedRoutes {
		require.NotEmpty(t, entry.Operation)
		spec, ok := catalogOperationByID(entry.Operation)
		require.True(t, ok, "operation %s is not in the catalog", entry.Operation)
		assert.True(t, catalogAdmitsDelegated(spec), "catalog operation %s does not admit the delegated pair", entry.Operation)
		found := false
		for _, row := range agentSubRouteTable {
			if row.id == entry.RouteID && row.operation(entry.Method) == entry.Operation {
				found = true
			}
		}
		assert.True(t, found, "entry %s %s matches no sub-route row with that operation", entry.Method, entry.RouteID)
		assert.True(t, permissions.AgentDelegable(spec.BasePermission, permissions.BoundaryKindHub) ||
			permissions.AgentDelegable(spec.BasePermission, permissions.BoundaryKindProject),
			"the primary permission of %s is not delegable", entry.Operation)
		assert.NotEqual(t, authzop.OperationID("agent.attach"), entry.Operation)
		tableOps[entry.Operation] = true
	}
	for _, spec := range authzop.Catalog {
		if catalogAdmitsDelegated(spec) {
			assert.True(t, tableOps[spec.ID], "catalog operation %s admits the delegated pair but has no admission entry", spec.ID)
		}
		hasP := slices.Contains(spec.Principals, authzop.PrincipalAgentDelegated)
		hasC := slices.Contains(spec.Credentials, authzop.CredentialDelegatedAgent)
		assert.Equal(t, hasP, hasC, "operation %s admits only half of the delegated pair", spec.ID)
	}
	for perm := range permissions.AgentDelegableRegistry {
		admitted := false
		for _, entry := range agentDelegationAdmittedRoutes {
			spec, _ := catalogOperationByID(entry.Operation)
			if spec.BasePermission == perm {
				admitted = true
			}
		}
		assert.True(t, admitted, "delegable permission %s has no admitted operation", perm)
	}
}

// TestAgentDelegablePolicy_NoDurableOrCredentialEffect: no policy key is the
// base permission of an operation that grants or changes authority,
// changes ownership, mints, issues or assigns a credential, or creates a
// resource.
func TestAgentDelegablePolicy_NoDurableOrCredentialEffect(t *testing.T) {
	forbidden := map[authzop.SecurityEffect]bool{
		authzop.EffectGrantAuthority: true, authzop.EffectChangeAuthority: true, authzop.EffectChangeOwnership: true,
		authzop.EffectMintCredential: true, authzop.EffectIssueCredential: true, authzop.EffectAssignCredential: true,
		authzop.EffectCreateResource: true,
	}
	for _, spec := range authzop.Catalog {
		if _, delegable := permissions.AgentDelegableRegistry[spec.BasePermission]; !delegable {
			continue
		}
		for _, effect := range spec.Effects {
			assert.False(t, forbidden[effect], "delegable permission %s is the base permission of %s, which has effect %s", spec.BasePermission, spec.ID, effect)
		}
	}
	for _, perm := range []string{"agent.create", "project.create", "agent.lifecycle", "agent.delete", "agent.delegation.create", "agent.delegation.exchange"} {
		_, delegable := permissions.AgentDelegableRegistry[perm]
		assert.False(t, delegable, "%s must not be delegable", perm)
	}
}

// TestAgentDelegationPermissions_NoTokenScopeAndNoRoleButSuperAdmin: the
// agent delegation permissions carry no token selector, and no seeded role
// other than super-admin (which receives every permission) holds them.
func TestAgentDelegationPermissions_NoTokenScopeAndNoRoleButSuperAdmin(t *testing.T) {
	for _, id := range []string{agentDelegationCreatePermission, agentDelegationExchangePermission} {
		p, ok := registryPermission(id)
		require.True(t, ok, id)
		assert.Empty(t, p.UATScope, id)
		assert.Empty(t, p.AgentScopes, id)
		_, hasBoundaries := permissions.PermissionAllowedBoundaries[id]
		assert.False(t, hasBoundaries, id)
	}
}

// TestAgentEnvVisibility_DecidedOnlyByTheHelpers: every response site that
// redacts an agent environment decides visibility through envViewAllowed
// or canViewAgentEnv, so the delegated arm in those two helpers covers
// every site.
func TestAgentEnvVisibility_DecidedOnlyByTheHelpers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	sites := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "agent_env_redaction.go" {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		if !strings.Contains(string(src), "redactAppliedConfigEnvForResponse") && !strings.Contains(string(src), "ResponseView(") {
			continue
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var fn string
			switch f := call.Fun.(type) {
			case *ast.Ident:
				fn = f.Name
			case *ast.SelectorExpr:
				fn = f.Sel.Name
			}
			switch fn {
			case "ResponseView":
				t.Errorf("%s: ResponseView called outside agent_env_redaction.go", fset.Position(call.Pos()))
			case "redactAppliedConfigEnvForResponse":
				sites++
				require.Len(t, call.Args, 2)
				inner, ok := call.Args[1].(*ast.CallExpr)
				helper := ""
				if ok {
					switch f := inner.Fun.(type) {
					case *ast.Ident:
						helper = f.Name
					case *ast.SelectorExpr:
						helper = f.Sel.Name
					}
				}
				assert.Contains(t, []string{"envViewAllowed", "canViewAgentEnv"}, helper,
					"%s: env visibility decided outside the helpers", fset.Position(call.Pos()))
			}
			return true
		})
	}
	assert.Positive(t, sites)
}

// TestAgentDelegationRoutes_ResolveToTheirOperations pins the two new agent
// sub-routes and their catalog patterns.
func TestAgentDelegationRoutes_ResolveToTheirOperations(t *testing.T) {
	route, ok := ResolveAgentSubRoute(http.MethodPost, "/api/v1/agents/a-1/delegations/g-1/exchange")
	require.True(t, ok)
	assert.Equal(t, AgentRouteDelegationExchange, route.RouteID)
	assert.Equal(t, opAgentDelegationExchange, route.OperationID)
	assert.Equal(t, "g-1", route.Suffix.Param)
	route, ok = ResolveAgentSubRoute(http.MethodPost, "/api/v1/agents/a-1/delegations")
	require.True(t, ok)
	assert.Equal(t, opAgentDelegationCreate, route.OperationID)
	_, ok = ResolveAgentSubRoute(http.MethodPost, "/api/v1/agents/a-1/delegations/g-1/exchange/extra")
	assert.False(t, ok)
	for _, row := range agentSubRouteTable {
		switch row.id {
		case AgentRouteDelegationExchange:
			assert.Equal(t, "/api/v1/agents/{id}/delegations/{grantId}/exchange", row.catalogPattern(false))
		case AgentRouteDelegations:
			assert.Equal(t, "/api/v1/agents/{id}/delegations", row.catalogPattern(false))
		case AgentRoutePortItem:
			assert.Equal(t, "/api/v1/agents/{id}/ports/{port}", row.catalogPattern(false))
		}
	}
}

// TestDelegatedCredentialFormat: 256 random bits, base64url, fixed prefix,
// SHA-256 at rest, and the token type detection order.
func TestDelegatedCredentialFormat(t *testing.T) {
	tok, hash, err := newDelegatedCredentialToken()
	require.NoError(t, err)
	assert.True(t, wellFormedDelegatedCredential(tok))
	assert.Equal(t, hashDelegatedCredential(tok), hash)
	assert.Len(t, hash, 64)
	tok2, _, err := newDelegatedCredentialToken()
	require.NoError(t, err)
	assert.NotEqual(t, tok, tok2)
	assert.Equal(t, tokenTypeDelegatedAgent, detectTokenType(tok))
	assert.False(t, wellFormedDelegatedCredential("scion_adt_"))
	assert.False(t, wellFormedDelegatedCredential("scion_pat_"+strings.Repeat("A", 43)))
	assert.Equal(t, tokenTypeUAT, detectTokenType("scion_pat_x"))
}
