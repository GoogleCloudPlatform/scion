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
	"fmt"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthzDecideUATCannotUseAdminBypass(t *testing.T) {
	authz, s := authzTestSetup(t)
	// CO1: Create user with super-admin role binding so bindings grant the
	// requested permission. The UAT scope restriction (not admin bypass)
	// must block access when the scope does not cover the action.
	userID := tid("admin-uat")
	createTestUserWithRole(t, s, userID, "admin@example.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(userID, "admin@example.com", "Admin", "admin", "api")

	decision := authz.Decide(context.Background(), AuthzRequest{
		Principal:  PrincipalContext{Identity: admin},
		Credential: CredentialContext{Kind: CredentialKindUAT, ID: "uat-1", ProjectID: "project-1", Scopes: []string{"project:read"}},
		Resource:   Resource{Type: "agent", ID: "agent-1", ParentType: "project", ParentID: "project-1"},
		Action:     ActionRead,
	})

	assert.False(t, decision.Allowed)
	assert.Contains(t, decision.Reason, "credential_scope")
	assert.Equal(t, "uat-1", decision.CredentialID)
	assert.Equal(t, string(CredentialKindUAT), decision.CredentialKind)
}

func TestAuthzDecideFederatedIdentitiesHaveExplicitOutcomes(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()

	federatedUser := NewFederatedUserIdentity("https://issuer.example", "user-1", "user@example.com", "User", "member", nil)

	userDecision := authz.Decide(ctx, AuthzRequest{
		Principal:  PrincipalContext{Identity: federatedUser},
		Credential: CredentialContext{Kind: CredentialKindFederation},
		Resource:   Resource{Type: "agent", ID: "agent-1", OwnerID: federatedUser.ID()},
		Action:     ActionRead,
	})
	// AK1: federated user IDs (issuer:subject) are not valid UUIDs, so
	// principal resolution fails closed before relationship grants are
	// evaluated.
	assert.False(t, userDecision.Allowed)
	assert.Equal(t, "principal resolution error (fail-closed)", userDecision.Reason)
	assert.Equal(t, PrincipalKindFederatedUser, userDecision.PrincipalKind)
	assert.Equal(t, string(CredentialKindFederation), userDecision.CredentialKind)

	// Phase 1G: federated agents without store-recorded delegation edges are
	// denied (absent edge = no authority, the load-bearing security fix).
	// Previously this was allowed via ancestry matching, but that is no longer
	// safe for federated identities whose ancestry is an unattested remote claim.
	federatedAgent := NewFederatedAgentIdentity("https://issuer.example", "agent-1", "remote-project", "Agent", "root-user", nil, nil)
	agentDecision := authz.Decide(ctx, AuthzRequest{
		Principal:  PrincipalContext{Identity: federatedAgent},
		Credential: CredentialContext{Kind: CredentialKindFederation},
		Resource:   Resource{Type: "agent", ID: "agent-2", Ancestry: []string{federatedAgent.ID()}},
		Action:     ActionRead,
	})
	assert.False(t, agentDecision.Allowed, "federated agent without delegation edge should be denied (Phase 1G)")
	assert.Equal(t, PrincipalKindFederatedAgent, agentDecision.PrincipalKind)

	federatedService := NewFederatedServiceIdentity("https://issuer.example", "service-1", "service@example.com", nil)
	serviceDecision := authz.CheckAccess(ctx, federatedService, Resource{Type: "agent", ID: "agent-1"}, ActionRead)
	assert.False(t, serviceDecision.Allowed)
	assert.Equal(t, "federated service identities are not supported", serviceDecision.Reason)

	spoofedServiceDecision := authz.Decide(ctx, AuthzRequest{
		Principal:  PrincipalContext{Kind: PrincipalKindUser, Identity: federatedService},
		Credential: CredentialContext{Kind: CredentialKindFederation},
		Resource:   Resource{Type: "agent", ID: "agent-1", OwnerID: federatedService.ID()},
		Action:     ActionRead,
	})
	assert.False(t, spoofedServiceDecision.Allowed)
	assert.Equal(t, "principal kind does not match identity", spoofedServiceDecision.Reason)
}

func TestCheckAccessUsesInteractiveCredentialCompatibilityAdapter(t *testing.T) {
	authz, s := authzTestSetup(t)
	// CO1: Create user with super-admin role binding. CheckAccess infers
	// interactive credential kind; the AK1 kernel evaluates role bindings.
	userID := tid("admin-interactive")
	createTestUserWithRole(t, s, userID, "admin@example.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(userID, "admin@example.com", "Admin", "admin", "api")

	decision := authz.CheckAccess(context.Background(), admin, Resource{Type: "agent", ID: "agent-1"}, ActionRead)

	assert.True(t, decision.Allowed)
	assert.Equal(t, string(CredentialKindInteractive), decision.CredentialKind)
}

func TestAuthzDecideFederatedAdminCannotUseLocalAdminBypass(t *testing.T) {
	authz, _ := authzTestSetup(t)
	federatedAdmin := NewFederatedUserIdentity("https://issuer.example", "user-1", "user@example.com", "User", "admin", nil)

	decision := authz.Decide(context.Background(), AuthzRequest{
		Principal:  PrincipalContext{Identity: federatedAdmin},
		Credential: CredentialContext{Kind: CredentialKindFederation},
		Resource:   Resource{Type: "agent", ID: "agent-1"},
		Action:     ActionRead,
	})

	assert.False(t, decision.Allowed)
	// AK1: federated user IDs are not valid UUIDs, so principal resolution
	// fails closed before role bindings can be evaluated.
	assert.Equal(t, "principal resolution error (fail-closed)", decision.Reason)
}

// faultyGetUserStore wraps a real store and injects a non-ErrNotFound error
// from GetUser for a specific user ID. This lets us trigger the "genuine store
// fault" path in checkUserHoldsPermission → walkDelegationChain → Decide().
type faultyGetUserStore struct {
	store.Store
	faultyUserID string
}

func (f *faultyGetUserStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == f.faultyUserID {
		return nil, fmt.Errorf("injected store fault for user %s", id)
	}
	return f.Store.GetUser(ctx, id)
}

// TestAuthzDecideFailClosedOnStoreErrorForMutatingActions verifies that when
// checkDelegationCeiling returns a non-nil error, Decide() denies non-read-only
// actions (ActionDelete, ActionStop, ActionUpdate). This pins the fix for the
// fail-open gap where only isMintingOperation actions were denied on error,
// leaving mutating-but-non-minting actions (delete, stop, update) allowed.
//
// The test injects a genuine store fault (non-ErrNotFound) via a thin store
// wrapper. The fault hits checkUserHoldsPermission → walkDelegationChain,
// which returns the error to Decide(). The fix in Decide() uses
// !isReadOnlyOperation (matching walkDelegationChain) instead of isMintingOperation.
func TestAuthzDecideFailClosedOnStoreErrorForMutatingActions(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("dc-proj-failopen")
	userID := tid("dc-user-failopen")
	agentID := tid("dc-agent-failopen")

	createDCProject(t, s, projectID, "dc-failopen-project")
	createDCUser(t, s, userID, "failopen@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, userID, AgentRoleFull)

	// Create delegation edge pointing to a user whose GetUser will fail
	// with a non-ErrNotFound error (genuine store fault).
	faultyDelegatorID := tid("dc-faulty-delegator")
	createDCEdge(t, s, store.DelegationPrincipalUser, faultyDelegatorID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	// Create a new AuthzService backed by a wrapper store that injects a
	// genuine fault when GetUser is called for the faulty delegator.
	faultyStore := &faultyGetUserStore{Store: s, faultyUserID: faultyDelegatorID}
	faultyAuthz := NewAuthzService(faultyStore, slog.Default())

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)

	// AK1: the kernel evaluates agent permissions from JWT-scope synthetic
	// bindings. Only actions whose permissions map to the agent's scopes
	// are granted. Use agent.delete (from project:agent:lifecycle) and
	// agent.create (from project:agent:create) for mutating, and
	// project.read (from project:read) for read-only.
	for _, tc := range []struct {
		action   Action
		resource Resource
		allowed  bool
		label    string
	}{
		{ActionDelete, Resource{Type: "agent", ID: tid("dc-child-failopen"), ParentType: "project", ParentID: projectID}, false, "ActionDelete must fail closed on store error"},
		{ActionCreate, Resource{Type: "agent", ID: tid("dc-child-failopen2"), ParentType: "project", ParentID: projectID}, false, "ActionCreate must fail closed on store error"},
		{ActionRead, Resource{Type: "project", ID: projectID}, true, "project read should remain allowed on store error (read-only)"},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			decision := faultyAuthz.CheckAccess(ctx, agent, tc.resource, tc.action)
			require.Equal(t, tc.allowed, decision.Allowed, tc.label)
			if !tc.allowed {
				assert.Contains(t, decision.Reason, "fail-closed",
					"denied decision should indicate fail-closed")
			}
		})
	}
}
