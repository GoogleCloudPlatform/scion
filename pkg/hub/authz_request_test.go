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
	authz, _ := authzTestSetup(t)
	admin := NewAuthenticatedUser("admin-1", "admin@example.com", "Admin", "admin", "api")

	decision := authz.Decide(context.Background(), AuthzRequest{
		Principal:  PrincipalContext{Identity: admin},
		Credential: CredentialContext{Kind: CredentialKindUAT, ID: "uat-1", ProjectID: "project-1", Scopes: []string{"agent:read"}},
		Resource:   Resource{Type: "agent", ID: "agent-1", ParentType: "project", ParentID: "project-1"},
		Action:     ActionRead,
	})

	assert.False(t, decision.Allowed)
	assert.Equal(t, "default deny", decision.Reason)
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
	assert.True(t, userDecision.Allowed)
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
	authz, _ := authzTestSetup(t)
	admin := NewAuthenticatedUser("admin-1", "admin@example.com", "Admin", "admin", "api")

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
	assert.Equal(t, "default deny", decision.Reason)
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
	// Include agentID in Ancestry so canAccessAsAncestor grants initial access,
	// letting us reach the delegation ceiling error path in Decide().
	resource := Resource{Type: "agent", ID: tid("dc-child-failopen"), ParentType: "project", ParentID: projectID, Ancestry: []string{agentID}}

	for _, tc := range []struct {
		action  Action
		allowed bool
		label   string
	}{
		{ActionDelete, false, "ActionDelete must fail closed on store error"},
		{ActionStop, false, "ActionStop must fail closed on store error"},
		{ActionUpdate, false, "ActionUpdate must fail closed on store error"},
		{ActionRead, true, "ActionRead should remain allowed on store error (read-only)"},
		{ActionList, true, "ActionList should remain allowed on store error (read-only)"},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			decision := faultyAuthz.CheckAccess(ctx, agent, resource, tc.action)
			require.Equal(t, tc.allowed, decision.Allowed, tc.label)
			if !tc.allowed {
				assert.Contains(t, decision.Reason, "fail-closed",
					"denied decision should indicate fail-closed")
			}
		})
	}
}
