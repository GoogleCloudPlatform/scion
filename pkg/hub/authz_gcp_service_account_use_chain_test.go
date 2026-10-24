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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Step 10 for gcp_service_account.use runs the chain check of recorded state
// (parentCeilingChainDeny) before the account's parent ceiling. These tests
// pin the states that check shares with the delegation chain check
// (walkDelegationChainHops): a missing edge, and local development
// provenance on an edge. Each test changes one fact against an admitted
// control.

// unattestedAgentIdentity wraps an AgentIdentity without its local
// ancestry provenance, so AncestryIsHubAttested reports false for it.
type unattestedAgentIdentity struct {
	AgentIdentity
}

// missingEdgeReason is the chain check's reason for a delegate with no
// active edge that it does not admit.
func missingEdgeReason(delegateID string) string {
	return fmt.Sprintf("no delegation edge for agent:%s (no delegated authority)", delegateID)
}

// Before the edge backfill, a hub-attested agent with no edge of its own is
// admitted on its recorded assignment, as the chain check admits it.
func TestMissingOwnEdgePreBackfillAdmitsGCPServiceAccountUse(t *testing.T) {
	authz, s := authzTestSetup(t)
	saX := tid("noedge-pre-sa")
	agentID, projectID := tid("noedge-pre-agent"), tid("noedge-pre-project")
	seedUseParentProofState(t, s, agentID, projectID, saX)
	require.False(t, authz.backfillCompleted(context.Background()), "precondition: the backfill has not completed")

	d := gcpUseDecision(authz, agentID, projectID, saX)
	require.True(t, d.Allowed, "cause=%q reason=%q", d.DenyCause, d.Reason)
}

// After the edge backfill every agent has an edge, so an agent with none is
// denied with the chain check's reason and cause, even with a recorded
// assignment from a source that holds assign.
func TestMissingOwnEdgePostBackfillDeniesGCPServiceAccountUse(t *testing.T) {
	authz, s := authzTestSetup(t)
	saX := tid("noedge-post-sa")
	agentID, projectID := tid("noedge-post-agent"), tid("noedge-post-project")
	seedUseParentProof(t, s, agentID, projectID, saX)

	d := gcpUseDecision(authz, agentID, projectID, saX)
	requireUseDeniedWithCause(t, d, "")
	require.Equal(t, missingEdgeReason(agentID), d.Reason)
}

// A missing edge above the agent denies at any backfill state: the agent's
// own edge is recorded from a parent agent, and the parent has no active
// edge (as after the parent is deleted). The control records the parent's
// edge from a user.
func TestMissingAncestorEdgeDeniesGCPServiceAccountUse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parentEdge bool
	}{
		{"parent edge recorded", true},
		{"parent edge missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authz, s := authzTestSetup(t)
			saX := tid("noedge-anc-sa")
			agentID, projectID := tid("noedge-anc-agent"), tid("noedge-anc-project")
			parentID := tid("noedge-anc-parent")
			seedUseParentProofState(t, s, agentID, projectID, saX)
			seedRecordedDelegationEdge(t, s, store.DelegationPrincipalAgent, parentID, store.DelegationPrincipalAgent, agentID,
				store.RoleScopeProject, projectID, string(AgentRoleFull))
			if tc.parentEdge {
				seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, useAssignerID(agentID), store.DelegationPrincipalAgent, parentID,
					store.RoleScopeProject, projectID, string(AgentRoleFull))
			}
			require.False(t, authz.backfillCompleted(context.Background()), "precondition: the backfill has not completed")

			d := gcpUseDecision(authz, agentID, projectID, saX)
			if tc.parentEdge {
				require.True(t, d.Allowed, "cause=%q reason=%q", d.DenyCause, d.Reason)
				return
			}
			requireUseDeniedWithCause(t, d, "")
			require.Equal(t, missingEdgeReason(parentID), d.Reason)
		})
	}
}

// Before the edge backfill, an agent with no edge whose identity is not
// hub-attested is denied: the pre-backfill allowance is for hub-attested
// identities only. The control is the same identity with its attestation.
func TestMissingOwnEdgeNotHubAttestedDeniesGCPServiceAccountUse(t *testing.T) {
	authz, s := authzTestSetup(t)
	saX := tid("noedge-unatt-sa")
	agentID, projectID := tid("noedge-unatt-agent"), tid("noedge-unatt-project")
	seedUseParentProofState(t, s, agentID, projectID, saX)
	require.False(t, authz.backfillCompleted(context.Background()), "precondition: the backfill has not completed")
	m, ok := ResourceParentCeilingFor(gcpServiceAccountUsePermission)
	require.True(t, ok)

	attested := newFullAgentIdentity(agentID, projectID, nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})
	unattested := unattestedAgentIdentity{attested}
	require.True(t, AncestryIsHubAttested(attested), "precondition: the control is hub-attested")
	require.False(t, AncestryIsHubAttested(unattested), "precondition: the wrapper is not hub-attested")

	allowed := Decision{Allowed: true, Reason: "granted"}
	control := authz.applyResourceParentCeiling(context.Background(), m, attested, gcpUseResource(saX), allowed)
	require.True(t, control.Allowed, "cause=%q reason=%q", control.DenyCause, control.Reason)

	d := authz.applyResourceParentCeiling(context.Background(), m, unattested, gcpUseResource(saX), allowed)
	requireUseDeniedWithCause(t, d, "")
	require.Equal(t, missingEdgeReason(agentID), d.Reason)
}

// seedDevLocalDelegationEdge records agentID's own edge from delegatorID
// with local development provenance (version 1, principal ceiling).
func seedDevLocalDelegationEdge(t *testing.T, s store.Store, delegatorID, agentID, projectID string) {
	t.Helper()
	require.NoError(t, s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    1,
			SourcePrincipalKind:  store.DelegationPrincipalUser,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: store.SourceCredentialDevLocal,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))
}

// An agent edge with local development provenance passes only while dev
// authority is enabled on this server and the delegator is the dev user, as
// in the chain check; otherwise it denies with ceiling_source_not_allowed,
// whatever the recorded assignment says.
func TestDevLocalEdgeProvenanceGCPServiceAccountUse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		devEnabled bool
		devUser    bool
		admitted   bool
	}{
		{"dev authority enabled, dev user delegator", true, true, true},
		{"dev authority disabled", false, true, false},
		{"another delegator", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authz, s := authzTestSetup(t)
			authz.setDevLocalAuthorityEnabled(tc.devEnabled)
			saX := tid("devlocal-use-sa")
			agentID, projectID := tid("devlocal-use-agent"), tid("devlocal-use-project")
			seedUseParentProof(t, s, agentID, projectID, saX)
			delegator := DevUserID
			if !tc.devUser {
				delegator = useAssignerID(agentID)
			}
			seedDevLocalDelegationEdge(t, s, delegator, agentID, projectID)

			d := gcpUseDecision(authz, agentID, projectID, saX)
			if tc.admitted {
				require.True(t, d.Allowed, "cause=%q reason=%q", d.DenyCause, d.Reason)
				return
			}
			requireUseDeniedWithCause(t, d, DenyCauseCeilingSourceNotAllowed)
		})
	}
}

// parentCeilingChainDeny leaves out the chain check's post-backfill
// allowance for registered non-sensitive reads, which holds only while no
// permission with a resource parent ceiling is such a read.
func TestResourceParentCeilingPermissionsAreNotRegisteredReads(t *testing.T) {
	require.NotEmpty(t, resourceParentCeilings)
	for _, m := range resourceParentCeilings {
		for _, action := range []Action{ActionRead, ActionList, ActionUse} {
			assert.False(t, ceilingReadAllowance(Resource{Type: m.ResourceType, ID: "x"}, action, m.DecisionPermission),
				"%s under %s", m.DecisionPermission, action)
		}
	}
}
