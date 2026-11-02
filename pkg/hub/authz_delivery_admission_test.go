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

// Tests for hub_delivery admission (ptone/scion#2228 part 2b): the
// relationship stage-4 source check and the step-10 admission walk.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubDelivery_Stage4ParityForOtherPrincipals pins relationship stage 4
// (relationshipSourceStage) for every principal type other than
// hubDeliveryIdentity. The expected outcomes are the outcomes of the
// owner-activity check followed by the owner's delegation-chain check
// (relationshipSourceActive, relationshipSourceDelegationHolds), written as
// literals derived by reading that code; they were not recorded by running
// the test before the hub_delivery stage-4 arm existed. The test runs the
// branch of relationshipSourceStage that every principal other than
// hubDeliveryIdentity takes, which the hub_delivery arm leaves unchanged.
func TestHubDelivery_Stage4ParityForOtherPrincipals(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	suspendedID := tid("hd-parity-suspended-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: suspendedID, Email: "suspended@golden.test", DisplayName: "Suspended",
		Role: "member", Status: store.UserStatusSuspended,
	}))

	newAgent := func(id string, ancestry []string, deleted bool) {
		a := &store.Agent{
			ID: id, Slug: "slug-" + id[:8], Name: "name-" + id[:8],
			ProjectID: f.projectAlpha.ID, Phase: string(state.PhaseRunning),
			OwnerID: ancestry[0], CreatedBy: ancestry[0], Ancestry: ancestry,
		}
		if deleted {
			a.DeletedAt = time.Now()
		}
		require.NoError(t, f.store.CreateAgent(ctx, a))
	}

	// An agent owner whose single recorded principal edge comes from an
	// active super-admin: the owner's delegation chain holds every
	// permission.
	liveAgentOwner := tid("hd-parity-live-agent-owner")
	newAgent(liveAgentOwner, []string{f.superAdminID}, false)
	seedRecordedDelegationEdge(t, f.store, store.DelegationPrincipalUser, f.superAdminID, store.DelegationPrincipalAgent,
		liveAgentOwner, store.RoleScopeProject, f.projectAlpha.ID, string(AgentRoleFull))

	deletedAgentOwner := tid("hd-parity-deleted-agent-owner")
	newAgent(deletedAgentOwner, []string{f.superAdminID}, true)

	suspendedRootAgentOwner := tid("hd-parity-suspended-root-agent-owner")
	newAgent(suspendedRootAgentOwner, []string{suspendedID}, false)

	sources := []struct {
		name       string
		ownerID    string
		wantHolds  bool
		wantDetail string
	}{
		{"user_owner_active", f.projectOwnerID, true, ""},
		{"user_owner_suspended", suspendedID, false, "sharing source owner is not active"},
		{"owner_empty", "", false, "sharing source has no owner"},
		{"owner_unknown", tid("hd-parity-unknown-owner"), false, "sharing source owner not found"},
		{"agent_owner_live", liveAgentOwner, true, ""},
		{"agent_owner_deleted", deletedAgentOwner, false, "sharing source owner agent is deleted"},
		{"agent_owner_root_suspended", suspendedRootAgentOwner, false, "sharing source owner agent's root user is not active"},
	}

	owner := NewAuthenticatedUser(f.projectOwnerID, "owner@golden.test", "Owner", "member", "web")
	storedAgent, err := f.store.GetAgent(ctx, f.agentAlpha.ID)
	require.NoError(t, err)
	principals := map[string]PrincipalContext{
		"user":         principalContextForIdentity(owner),
		"agent_jwt":    principalContextForIdentity(progenyPairAgent(tid("hd-parity-jwt"), f.projectAlpha.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())),
		"stored_agent": principalContextForIdentity(&storedAgentIdentity{agent: storedAgent}),
	}

	resource := Resource{Type: "secret", ID: f.secretID}
	for pName, principal := range principals {
		for _, src := range sources {
			for _, action := range []Action{ActionDeliver, ActionRead} {
				t.Run(pName+"/"+src.name+"/"+string(action), func(t *testing.T) {
					s := &SharingSource{Kind: "secret", ID: f.secretID, OwnerID: src.ownerID, Policy: SharingPolicyOptInRequired, OptedIn: true}
					holds, detail := f.authz.relationshipSourceStage(ctx, principal, s, resource, action, "secret.deliver")
					assert.Equal(t, src.wantHolds, holds, "detail %q", detail)
					assert.Equal(t, src.wantDetail, detail)
				})
			}
		}
	}
}

// =============================================================================
// Step-10 admission walk (hubDeliveryAdmission)
// =============================================================================

// hdAgent stores an agent in projectID with the given ancestry; the owner
// and creator are the last ancestry entry.
func hdAgent(t *testing.T, s store.Store, id, projectID string, ancestry []string) {
	t.Helper()
	last := ancestry[len(ancestry)-1]
	require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
		ID: id, Slug: "slug-" + id[:8], Name: "name-" + id[:8],
		ProjectID: projectID, Phase: string(state.PhaseRunning),
		OwnerID: last, CreatedBy: last, Ancestry: ancestry,
	}))
}

// hdEdge stores an active edge delegatorType:delegatorID -> agent:delegateID
// in projectID. By default it is recorded (provenance version 1, source
// kind equal to the delegator type, session or agent credential) with a
// principal ceiling; mutate adjusts it before it is written.
func hdEdge(t *testing.T, s store.Store, delegatorType, delegatorID, delegateID, projectID string, mutate func(e *store.DelegationEdge)) {
	t.Helper()
	cred := store.SourceCredentialSession
	if delegatorType == store.DelegationPrincipalAgent {
		cred = store.SourceCredentialAgent
	}
	e := &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: delegateID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID,
		Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  delegatorType,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: cred,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	if mutate != nil {
		mutate(e)
	}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), e))
}

// withBoundedCeiling returns a mutate func giving the edge a bounded V1
// ceiling holding exactly ids.
func withBoundedCeiling(ids ...string) func(e *store.DelegationEdge) {
	return func(e *store.DelegationEdge) {
		e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: ids}
	}
}

// hdDevLocalAgent seeds the local development user (an active super-admin,
// as the workstation server seeds it), stores agent id in alpha with
// ancestry [DevUserID], and gives it the edge a dev-auth create writes: a
// recorded principal edge from user:DevUserID with dev_local provenance.
// mutate adjusts the edge before it is written. Dev auth stays as the
// caller sets it.
func hdDevLocalAgent(t *testing.T, f *goldenFixture, id string, mutate func(e *store.DelegationEdge)) {
	t.Helper()
	seedDevUser(context.Background(), f.store, DevUserConfig{})
	u, err := f.store.GetUser(context.Background(), DevUserID)
	require.NoError(t, err)
	require.Equal(t, store.UserStatusActive, u.Status)
	hdAgent(t, f.store, id, f.projectAlpha.ID, []string{DevUserID})
	hdEdge(t, f.store, store.DelegationPrincipalUser, DevUserID, id, f.projectAlpha.ID, func(e *store.DelegationEdge) {
		e.SourceCredentialKind = store.SourceCredentialDevLocal
		if mutate != nil {
			mutate(e)
		}
	})
}

// withDevLocalSchedulerProvenance makes the edge a scheduler edge written
// under a revision the local development user authorized.
func withDevLocalSchedulerProvenance(e *store.DelegationEdge) {
	e.AuthorityProvenance = schedulerProv(store.DelegationPrincipalUser, DevUserID, store.InitiatorCredentialKindDevLocal)
}

// hdIdentity builds the hub_delivery credential for agentID through the
// production constructor.
func hdIdentity(t *testing.T, a *AuthzService, agentID string) *hubDeliveryIdentity {
	t.Helper()
	h, err := a.newHubDeliveryIdentity(context.Background(), agentID)
	require.NoError(t, err)
	return h
}

// hdSchedulerInconsistent builds C in alpha with ancestry [project owner]
// whose only edge, from the project owner, is a scheduler edge consistent in
// every field except SourceAuthorizationRevision = 0 (malformed state,
// deny-only: every scheduler edge a writer records has a revision of 1 or
// more).
func hdSchedulerInconsistent(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
	t.Helper()
	c := tid("hd-adm-sched-inconsistent")
	hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
	hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID, func(e *store.DelegationEdge) {
		e.AuthorityProvenance = schedulerProv(store.DelegationPrincipalUser, f.projectOwnerID, store.InitiatorCredentialKindSession)
		e.SourceAuthorizationRevision = 0
	})
	return f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}
}

type hdCeilingResult struct {
	allowed bool
	reason  string
	cause   DenyCause
	explain []DecisionStep
}

// hdDirect calls checkDelegationCeiling directly for h, so neither Step 0b
// nor the relationship stages run, and asserts a nil error.
func hdDirect(t *testing.T, a *AuthzService, h *hubDeliveryIdentity, res Resource, perm string) hdCeilingResult {
	t.Helper()
	req := AuthzRequest{
		Principal:  PrincipalContext{Kind: PrincipalKindAgent, ID: h.ID(), Identity: h},
		Resource:   res,
		Action:     ActionDeliver,
		Permission: perm,
	}
	var r hdCeilingResult
	allowed, reason, err := a.checkDelegationCeiling(contextWithDelegationCeilingCache(context.Background()), req, perm, h.ID(), &r.explain, &r.cause)
	require.NoError(t, err, "the hub_delivery arm never returns an error")
	r.allowed, r.reason = allowed, reason
	assert.False(t, hasDecisionStep(r.explain, "delegation_ceiling_allowed"), "the ordinary proof must never run for this type")
	return r
}

func countDecisionSteps(explain []DecisionStep, step string) int {
	n := 0
	for _, s := range explain {
		if s.Step == step {
			n++
		}
	}
	return n
}

// TestHubDelivery_Step10Admission covers every rule of the step-10
// admission walk through direct checkDelegationCeiling calls with a
// constructor-built hub_delivery credential. Fixture rows are written at
// the store layer (agents, edges, users, memberships); rows marked
// "malformed state" seed a shape no production writer produces and are
// deny-only.
func TestHubDelivery_Step10Admission(t *testing.T) {
	secretPerm := "secret.deliver"

	t.Run("single_hop_principal/allowed", func(t *testing.T) {
		f := newGoldenFixture(t)
		c := tid("hd-adm-allowed")
		hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
		hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID, nil)
		h := hdIdentity(t, f.authz, c)
		for _, perm := range []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver"} {
			r := hdDirect(t, f.authz, h, Resource{Type: permissionResource(t, perm), ID: tid("hd-adm-res-" + perm)}, perm)
			assert.True(t, r.allowed, "%s: %q", perm, r.reason)
			assert.Equal(t, "delivery ceiling passed", r.reason)
			assert.Equal(t, DenyCause(""), r.cause)
			assert.Equal(t, 1, countDecisionSteps(r.explain, "delegation_ceiling_delivery_hop"), "explain: %+v", r.explain)
			assert.True(t, hasDecisionStep(r.explain, "delegation_ceiling_delivery_root"))
		}
	})

	t.Run("identity_only_principal/allowed", func(t *testing.T) {
		f := newGoldenFixture(t)
		c := tid("hd-adm-identity-only")
		hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
		hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID, nil)
		h := hdIdentity(t, f.authz, c)
		req := AuthzRequest{Principal: PrincipalContext{Identity: h}, Resource: Resource{Type: "secret", ID: f.secretID}, Action: ActionDeliver, Permission: secretPerm}
		var explain []DecisionStep
		allowed, reason, err := f.authz.checkDelegationCeiling(context.Background(), req, secretPerm, c, &explain, nil)
		require.NoError(t, err)
		assert.True(t, allowed, reason)
		assert.True(t, hasDecisionStep(explain, "delegation_ceiling_delivery_root"))
	})

	t.Run("bounded_with_permission/allowed", func(t *testing.T) {
		f := newGoldenFixture(t)
		c := tid("hd-adm-bounded-ok")
		hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
		hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID, withBoundedCeiling(secretPerm))
		r := hdDirect(t, f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}, secretPerm)
		assert.True(t, r.allowed, r.reason)
	})

	t.Run("two_hop/allowed", func(t *testing.T) {
		f := newGoldenFixture(t)
		a, c := tid("hd-adm-2hop-a"), tid("hd-adm-2hop-c")
		hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.projectOwnerID})
		hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID, a})
		hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, a, f.projectAlpha.ID, nil)
		hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, withBoundedCeiling(secretPerm, "agent.read"))
		r := hdDirect(t, f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}, secretPerm)
		assert.True(t, r.allowed, r.reason)
		assert.Equal(t, 2, countDecisionSteps(r.explain, "delegation_ceiling_delivery_hop"), "explain: %+v", r.explain)
		assert.True(t, hasDecisionStep(r.explain, "delegation_ceiling_delivery_root"))
	})

	// The workstation path: on a single machine every agent is created
	// through dev auth, so its edge is a dev_local edge from
	// user:DevUserID. With dev auth on and the seeded local development
	// user, the walk and the provenance root admit it. Positive controls
	// for the dev_local deny rows below.
	devLocalAllowed := map[string]func(e *store.DelegationEdge){
		"dev_local_principal/allowed":           nil,
		"dev_local_scheduler_initiator/allowed": withDevLocalSchedulerProvenance,
	}
	for name, mutate := range devLocalAllowed {
		t.Run(name, func(t *testing.T) {
			f := newGoldenFixture(t)
			f.authz.setDevLocalAuthorityEnabled(true)
			c := tid("hd-adm-" + name)
			hdDevLocalAgent(t, f, c, mutate)
			for _, perm := range []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver"} {
				r := hdDirect(t, f.authz, hdIdentity(t, f.authz, c), Resource{Type: permissionResource(t, perm), ID: tid("hd-adm-dev-res-" + perm)}, perm)
				assert.True(t, r.allowed, "%s: %q", perm, r.reason)
				assert.Equal(t, "delivery ceiling passed", r.reason)
				assert.Equal(t, DenyCause(""), r.cause)
				assert.Equal(t, 1, countDecisionSteps(r.explain, "delegation_ceiling_delivery_hop"), "explain: %+v", r.explain)
				assert.True(t, hasDecisionStep(r.explain, "delegation_ceiling_delivery_root"))
			}
		})
	}

	// The premise of no_self_operation_exception: no deliver permission is
	// a self operation, so the walk's selfTarget=false and a regression to
	// true agree for every deliver permission. Pinning the premise makes a
	// change to selfOperationSet that adds a deliver permission a test
	// failure here.
	t.Run("no_self_operation_exception/premise", func(t *testing.T) {
		require.NotEmpty(t, hubDeliveryPermissionIDs)
		for id := range hubDeliveryPermissionIDs {
			assert.False(t, selfOperationSet[id], "%s must not be a self operation", id)
		}
	})

	type denyRow struct {
		name       string
		setup      func(t *testing.T, f *goldenFixture) (a *AuthzService, h *hubDeliveryIdentity, res Resource)
		wantReason string // exact, or a prefix when wantPrefix
		// wantReasonFor, when set, gives the exact reason for rows whose
		// reason names a fixture ID.
		wantReasonFor func(f *goldenFixture) string
		wantPrefix    bool
		wantCause     DenyCause
	}
	// single builds C in alpha with ancestry [delegator] and one edge from
	// delegator, adjusted by mutate. An empty delegatorID is the project
	// owner; memberNone names the hub member with no alpha membership.
	const memberNone = "@member-none"
	single := func(name, delegatorID string, mutate func(e *store.DelegationEdge)) func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
		return func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
			switch delegatorID {
			case "":
				delegatorID = f.projectOwnerID
			case memberNone:
				delegatorID = f.memberNoneID
			}
			c := tid("hd-adm-" + name)
			hdAgent(t, f.store, c, f.projectAlpha.ID, []string{delegatorID})
			hdEdge(t, f.store, store.DelegationPrincipalUser, delegatorID, c, f.projectAlpha.ID, mutate)
			return f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}
		}
	}
	// twoHop builds U -> A (principal) -> C (bounded with secret.deliver),
	// U = f.projectOwnerID, then runs after(f, a, c).
	twoHop := func(name string, after func(t *testing.T, f *goldenFixture, a, c string) *AuthzService) func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
		return func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
			a, c := tid("hd-adm-"+name+"-a"), tid("hd-adm-"+name+"-c")
			hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.projectOwnerID})
			hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID, a})
			hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, a, f.projectAlpha.ID, nil)
			hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, withBoundedCeiling(secretPerm))
			h := hdIdentity(t, f.authz, c)
			authz := f.authz
			if after != nil {
				authz = after(t, f, a, c)
			}
			return authz, h, Resource{Type: "secret", ID: f.secretID}
		}
	}
	failingWith := func(f *goldenFixture, mutate func(m *materialFailingStore)) *AuthzService {
		m := &materialFailingStore{Store: f.store}
		mutate(m)
		return NewAuthzService(m, slog.Default())
	}

	rows := []denyRow{
		{
			name: "cross_project",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				a, h, _ := single("cross-project", "", nil)(t, f)
				return a, h, Resource{Type: "secret", ID: f.secretID, ParentType: "project", ParentID: f.projectBeta.ID}
			},
			wantReason: "delivery resource is in a different project", wantCause: DenyCauseCeilingProvenanceMissing,
		},
		{
			// The golden fixture deletes the backfill marker: the
			// pre-backfill state, where the ordinary walk has an allowance.
			name: "missing_edge/backfill_absent",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				c := tid("hd-adm-missing-edge")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				return f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}
			},
			wantReason: "no active delegation edge", wantCause: DenyCauseCeilingProvenanceMissing,
		},
		{
			name: "missing_edge/backfill_present",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				markEdgeBackfillComplete(t, f.store)
				c := tid("hd-adm-missing-edge-bf")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				return f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}
			},
			wantReason: "no active delegation edge", wantCause: DenyCauseCeilingProvenanceMissing,
		},
		{
			// Malformed state (deny-only): a live agent whose only edge is
			// inactive.
			name:       "inactive_edge_only",
			setup:      single("inactive-edge", "", func(e *store.DelegationEdge) { e.Active = false }),
			wantReason: "no active delegation edge", wantCause: DenyCauseCeilingProvenanceMissing,
		},
		{
			name: "edge_in_other_project_only",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				c := tid("hd-adm-other-project-edge")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectBeta.ID, nil)
				return f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}
			},
			wantReason: "no active delegation edge", wantCause: DenyCauseCeilingProvenanceMissing,
		},
		{
			// Malformed state (deny-only): two active edges for one
			// delegate and scope, which the partial unique index forbids.
			name: "two_active_edges",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				_, h, res := single("two-edges", "", nil)(t, f)
				edge := func(id string) *store.DelegationEdge {
					return &store.DelegationEdge{
						ID: id, DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.projectOwnerID,
						DelegateType: store.DelegationPrincipalAgent, DelegateID: h.ID(),
						ScopeType: store.RoleScopeProject, ScopeID: f.projectAlpha.ID, Active: true,
						AuthorityProvenance: store.AuthorityProvenance{ProvenanceVersion: 1, SourcePrincipalKind: store.DelegationPrincipalUser, SourcePrincipalID: f.projectOwnerID, SourceCredentialKind: store.SourceCredentialSession},
						EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
					}
				}
				a := failingWith(f, func(m *materialFailingStore) {
					m.delegationEdgesOverride = []*store.DelegationEdge{edge(tid("hd-adm-dup-1")), edge(tid("hd-adm-dup-2"))}
				})
				return a, h, res
			},
			wantReason: "multiple active delegation edges", wantCause: DenyCauseCeilingProvenanceAmbiguous,
		},
		{
			name: "migration_sentinel",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				c := tid("hd-adm-sentinel")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				createDCEdge(t, f.store, store.DelegationPrincipalUser, migrationDelegatorID, store.DelegationPrincipalAgent, c,
					store.RoleScopeProject, f.projectAlpha.ID, string(AgentRoleFull))
				return f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}
			},
			wantReason: "migration-provenance delegation supplies no delivery authority", wantCause: DenyCauseCeilingOrphaned,
		},
		{
			// The test server enables dev authority by default, so the row
			// turns it off itself.
			name: "dev_local_dev_auth_off",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				f.authz.setDevLocalAuthorityEnabled(false)
				return single("dev-off", DevUserID, func(e *store.DelegationEdge) {
					e.SourceCredentialKind = store.SourceCredentialDevLocal
				})(t, f)
			},
			wantReason: "delegation ceiling: " + reasonDevLocalDisabled, wantCause: DenyCauseCeilingSourceNotAllowed,
		},
		{
			// Malformed state (deny-only): local-development provenance
			// with a delegator other than the local development user.
			name: "dev_local_wrong_delegator",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				f.authz.setDevLocalAuthorityEnabled(true)
				return single("dev-wrong", "", func(e *store.DelegationEdge) {
					e.SourceCredentialKind = store.SourceCredentialDevLocal
				})(t, f)
			},
			wantReason: "delegation ceiling: local development provenance with another delegator", wantCause: DenyCauseCeilingSourceNotAllowed,
		},
		{
			// A pre-provenance edge (version 0, unrecorded ceiling).
			name: "unknown_provenance_version",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				c := tid("hd-adm-unrecorded-v0")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				createDCEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, store.DelegationPrincipalAgent, c,
					store.RoleScopeProject, f.projectAlpha.ID, string(AgentRoleFull))
				return f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}
			},
			wantReason: "delegation ceiling: secret.deliver requires recorded provenance", wantCause: DenyCauseCeilingUnrecorded,
		},
		{
			// Malformed state (deny-only): an unrecorded ceiling kind with
			// provenance version 1.
			name: "unrecorded_ceiling_version_1",
			setup: single("unrecorded-v1", "", func(e *store.DelegationEdge) {
				e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
			}),
			wantReason: "delegation ceiling: secret.deliver requires recorded provenance", wantCause: DenyCauseCeilingUnrecorded,
		},
		{
			name:       "bounded_without_permission",
			setup:      single("bounded-no-perm", "", withBoundedCeiling("agent.read", "secret.use")),
			wantReason: "delegation ceiling: secret.deliver is outside the frozen effect ceiling", wantCause: DenyCauseCeilingEffectExceeded,
		},
		{
			// The resource names the target agent itself: there is no
			// self-operation exception for delivery.
			name: "no_self_operation_exception",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				a, h, _ := single("no-self-op", "", withBoundedCeiling("agent.read"))(t, f)
				return a, h, Resource{Type: "agent", ID: h.ID(), OwnerID: h.ID()}
			},
			wantReason: "delegation ceiling: secret.deliver is outside the frozen effect ceiling", wantCause: DenyCauseCeilingEffectExceeded,
		},
		{
			name:       "delegator_user_missing",
			setup:      single("user-missing", tid("hd-adm-ghost-user"), nil),
			wantReason: "delegator " + tid("hd-adm-ghost-user") + " is not live", wantCause: DenyCauseCeilingOrphaned,
		},
		{
			name: "delegator_user_suspended",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				a, h, res := single("user-suspended", f.memberAlphaID, nil)(t, f)
				setUserStatus(t, f.store, f.memberAlphaID, store.UserStatusSuspended)
				return a, h, res
			},
			wantReasonFor: func(f *goldenFixture) string { return "delegator " + f.memberAlphaID + " is suspended" },
			wantCause:     DenyCauseCeilingDelegatorLacksPermission,
		},
		{
			// No super-admin shortcut: a suspended super-admin delegator is
			// inactive like any other user.
			name: "delegator_super_admin_suspended",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				a, h, res := single("superadmin-suspended", f.superAdminID, nil)(t, f)
				setUserStatus(t, f.store, f.superAdminID, store.UserStatusSuspended)
				return a, h, res
			},
			wantReasonFor: func(f *goldenFixture) string { return "delegator " + f.superAdminID + " is suspended" },
			wantCause:     DenyCauseCeilingDelegatorLacksPermission,
		},
		{
			// Malformed state (deny-only): a delegator type outside
			// {user, agent}, which the schema enum forbids.
			name: "unsupported_delegator_type",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				_, h, res := single("unsupported-type", "", nil)(t, f)
				a := failingWith(f, func(m *materialFailingStore) {
					m.delegationEdgesOverride = []*store.DelegationEdge{{
						ID: tid("hd-adm-group-edge"), DelegatorType: "group", DelegatorID: tid("hd-adm-group"),
						DelegateType: store.DelegationPrincipalAgent, DelegateID: h.ID(),
						ScopeType: store.RoleScopeProject, ScopeID: f.projectAlpha.ID, Active: true,
						AuthorityProvenance: store.AuthorityProvenance{ProvenanceVersion: 1, SourceCredentialKind: store.SourceCredentialSession},
						EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
					}}
				})
				return a, h, res
			},
			wantReason: "unsupported delegator type", wantCause: "",
		},
		{
			name: "cycle",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				c := tid("hd-adm-cycle")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				hdEdge(t, f.store, store.DelegationPrincipalAgent, c, c, f.projectAlpha.ID, nil)
				return f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}
			},
			wantReason: "delegation chain cycle", wantCause: "",
		},
		{
			name: "two_hop/inner_bounded_without_permission",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				a, c := tid("hd-adm-2hop-noperm-a"), tid("hd-adm-2hop-noperm-c")
				hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.projectOwnerID})
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID, a})
				hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, a, f.projectAlpha.ID, nil)
				hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, withBoundedCeiling("agent.read"))
				return f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}
			},
			wantReason: "delegation ceiling: secret.deliver is outside the frozen effect ceiling", wantCause: DenyCauseCeilingEffectExceeded,
		},
		{
			// Malformed state (deny-only): the inner delegator is
			// soft-deleted while its own edge stays active.
			name: "two_hop/inner_delegator_deleted",
			setup: twoHop("inner-deleted", func(t *testing.T, f *goldenFixture, a, c string) *AuthzService {
				ag, err := f.store.GetAgent(context.Background(), a)
				require.NoError(t, err)
				ag.DeletedAt = time.Now()
				require.NoError(t, f.store.UpdateAgent(context.Background(), ag))
				return f.authz
			}),
			wantReasonFor: func(*goldenFixture) string {
				return "delegator agent " + tid("hd-adm-inner-deleted-a") + " is not live"
			},
			wantCause: DenyCauseCeilingOrphaned,
		},
		{
			name: "two_hop/outer_delegator_suspended",
			setup: twoHop("outer-suspended", func(t *testing.T, f *goldenFixture, a, c string) *AuthzService {
				setUserStatus(t, f.store, f.projectOwnerID, store.UserStatusSuspended)
				return f.authz
			}),
			wantReasonFor: func(f *goldenFixture) string { return "delegator " + f.projectOwnerID + " is suspended" },
			wantCause:     DenyCauseCeilingDelegatorLacksPermission,
		},
		{
			name: "two_hop/delegator_agent_not_found",
			setup: twoHop("agent-not-found", func(t *testing.T, f *goldenFixture, a, c string) *AuthzService {
				return failingWith(f, func(m *materialFailingStore) { m.getAgentErr = store.ErrNotFound })
			}),
			wantReasonFor: func(*goldenFixture) string {
				return "delegator agent " + tid("hd-adm-agent-not-found-a") + " is not live"
			},
			wantCause: DenyCauseCeilingOrphaned,
		},
		{
			name: "lookup_error/edges",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				_, h, res := single("edge-err", "", nil)(t, f)
				return failingWith(f, func(m *materialFailingStore) {
					m.getDelegationEdgesForDelegateErr = errors.New("injected edge failure")
				}), h, res
			},
			wantReason: "delivery ceiling check failed (fail-closed): injected edge failure", wantCause: DenyCauseCeilingError,
		},
		{
			name: "lookup_error/delegator_user",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				_, h, res := single("user-err", "", nil)(t, f)
				return failingWith(f, func(m *materialFailingStore) { m.getUserErr = errors.New("injected user failure") }), h, res
			},
			wantReason: "delivery ceiling check failed (fail-closed): injected user failure", wantCause: DenyCauseCeilingError,
		},
		{
			name: "lookup_error/delegator_agent",
			setup: twoHop("agent-err", func(t *testing.T, f *goldenFixture, a, c string) *AuthzService {
				return failingWith(f, func(m *materialFailingStore) { m.getAgentErr = errors.New("injected agent failure") })
			}),
			wantReason: "delivery ceiling check failed (fail-closed): injected agent failure", wantCause: DenyCauseCeilingError,
		},
		{
			// A single user hop makes no GetAgent call in the walk, so
			// ResolveProvenanceRoot's agent load is the first to fail.
			name: "lookup_error/provenance_root",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				_, h, res := single("root-err", "", nil)(t, f)
				return failingWith(f, func(m *materialFailingStore) { m.getAgentErr = errors.New("injected agent failure") }), h, res
			},
			wantReason: "delivery ceiling check failed (fail-closed): ", wantPrefix: true, wantCause: DenyCauseCeilingError,
		},
		{
			// The delegator is active but has no membership in the
			// agent's project, so the provenance root is not admitted.
			name:       "root_not_admitted",
			setup:      single("root-not-admitted", memberNone, nil),
			wantReason: "delivery provenance root denied: ", wantPrefix: true, wantCause: DenyCauseCeilingDelegatorLacksPermission,
		},
		{
			// Malformed state (deny-only): the target is soft-deleted after
			// the credential was built, while its edge stays active.
			name: "target_deleted_after_mint",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				a, h, res := single("target-deleted", "", nil)(t, f)
				ag, err := f.store.GetAgent(context.Background(), h.ID())
				require.NoError(t, err)
				ag.DeletedAt = time.Now()
				require.NoError(t, f.store.UpdateAgent(context.Background(), ag))
				return a, h, res
			},
			wantReason: "delivery provenance root denied: ", wantPrefix: true, wantCause: DenyCauseForProvenanceError(ErrProvenanceInactive),
		},
		{
			// Malformed state (deny-only): a recorded edge whose source
			// principal kind differs from its delegator type. The walk's
			// per-hop checks pass; ResolveProvenanceRoot's principal typing
			// denies.
			name: "delegator_type_mismatch",
			setup: single("type-mismatch", "", func(e *store.DelegationEdge) {
				e.SourcePrincipalKind = store.DelegationPrincipalAgent
			}),
			wantReason: "delivery provenance root denied: ", wantPrefix: true, wantCause: DenyCauseForProvenanceError(ErrProvenanceMismatch),
		},
		{
			// Malformed state (deny-only): C's only edge is a scheduler
			// edge consistent in every field except
			// SourceAuthorizationRevision = 0. The walk's per-hop checks
			// pass (principal ceiling, version 1, live delegator, no
			// local-development initiator); ResolveProvenanceRoot's
			// scheduler-edge check denies. The cause is B.3's mapping of
			// ErrProvenanceMismatch, which is ceiling_orphaned.
			name:       "scheduler_edge_inconsistent",
			setup:      hdSchedulerInconsistent,
			wantReason: "delivery provenance root denied: ", wantPrefix: true, wantCause: DenyCauseForProvenanceError(ErrProvenanceMismatch),
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newGoldenFixture(t)
			a, h, res := row.setup(t, f)
			r := hdDirect(t, a, h, res, secretPerm)
			assert.False(t, r.allowed, "reason %q", r.reason)
			switch {
			case row.wantReasonFor != nil:
				assert.Equal(t, row.wantReasonFor(f), r.reason)
			case row.wantPrefix:
				assert.True(t, strings.HasPrefix(r.reason, row.wantReason), "reason %q", r.reason)
			default:
				assert.Equal(t, row.wantReason, r.reason)
			}
			assert.Equal(t, row.wantCause, r.cause)
			assert.True(t, hasDecisionStep(r.explain, "delegation_ceiling_delivery_denied"), "explain: %+v", r.explain)
			assert.False(t, hasDecisionStep(r.explain, "delegation_ceiling_delivery_root"), "a deny never records an admitted root")
		})
	}
}

// TestHubDelivery_LivenessCausesMatchOrdinaryWalk pins that, for each
// delegator liveness fact, the admission walk records the same DenyCause
// the ordinary walk records for an agent-JWT-class principal on the same
// edge: the two liveness definitions agree on causes.
func TestHubDelivery_LivenessCausesMatchOrdinaryWalk(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *goldenFixture) string // returns the target agent
		// authz, when set, is the service both walks run on.
		authz func(f *goldenFixture) *AuthzService
		want  DenyCause
	}{
		{
			name: "user_missing",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := tid("hd-live-user-missing")
				ghost := tid("hd-live-ghost")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{ghost})
				hdEdge(t, f.store, store.DelegationPrincipalUser, ghost, c, f.projectAlpha.ID, nil)
				return c
			},
			want: DenyCauseCeilingOrphaned,
		},
		{
			name: "user_suspended",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := tid("hd-live-user-suspended")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.memberAlphaID})
				hdEdge(t, f.store, store.DelegationPrincipalUser, f.memberAlphaID, c, f.projectAlpha.ID, nil)
				setUserStatus(t, f.store, f.memberAlphaID, store.UserStatusSuspended)
				return c
			},
			want: DenyCauseCeilingDelegatorLacksPermission,
		},
		{
			name: "agent_deleted",
			setup: func(t *testing.T, f *goldenFixture) string {
				a, c := tid("hd-live-agent-deleted-a"), tid("hd-live-agent-deleted-c")
				hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.superAdminID})
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.superAdminID, a})
				hdEdge(t, f.store, store.DelegationPrincipalUser, f.superAdminID, a, f.projectAlpha.ID, nil)
				hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, nil)
				ag, err := f.store.GetAgent(context.Background(), a)
				require.NoError(t, err)
				ag.DeletedAt = time.Now()
				require.NoError(t, f.store.UpdateAgent(context.Background(), ag))
				return c
			},
			want: DenyCauseCeilingOrphaned,
		},
		{
			// The delegator agent's row is absent (GetAgent answers
			// not-found for it only), as after a hard delete.
			name: "agent_not_found",
			setup: func(t *testing.T, f *goldenFixture) string {
				a, c := tid("hd-live-agent-not-found-a"), tid("hd-live-agent-not-found-c")
				hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.superAdminID})
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.superAdminID, a})
				hdEdge(t, f.store, store.DelegationPrincipalUser, f.superAdminID, a, f.projectAlpha.ID, nil)
				hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, nil)
				return c
			},
			authz: func(f *goldenFixture) *AuthzService {
				return NewAuthzService(&materialFailingStore{Store: f.store, getAgentNotFoundID: tid("hd-live-agent-not-found-a")}, slog.Default())
			},
			want: DenyCauseCeilingOrphaned,
		},
		{
			// The delegator agent is held (ptone/scion#3433): not live in
			// either walk (checkAgentHoldsPermission, hubDeliveryDelegatorLiveness).
			name: "agent_held",
			setup: func(t *testing.T, f *goldenFixture) string {
				a, c := tid("hd-live-agent-held-a"), tid("hd-live-agent-held-c")
				hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.superAdminID})
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.superAdminID, a})
				hdEdge(t, f.store, store.DelegationPrincipalUser, f.superAdminID, a, f.projectAlpha.ID, nil)
				hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, nil)
				holdAgentFor(t, f.store, a, f.projectAlpha.ID, f.superAdminID)
				return c
			},
			want: DenyCauseCeilingOrphaned,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGoldenFixture(t)
			c := tc.setup(t, f)
			res := Resource{Type: "secret", ID: f.secretID}
			h := hdIdentity(t, f.authz, c)
			authz := f.authz
			if tc.authz != nil {
				authz = tc.authz(f)
			}

			r := hdDirect(t, authz, h, res, "secret.deliver")
			assert.False(t, r.allowed, r.reason)
			assert.Equal(t, tc.want, r.cause, "hub_delivery: %q", r.reason)

			stored, err := f.store.GetAgent(context.Background(), c)
			require.NoError(t, err)
			req := AuthzRequest{
				Principal: PrincipalContext{Kind: PrincipalKindAgent, ID: c, Identity: &storedAgentIdentity{agent: stored}},
				Resource:  res, Action: ActionDeliver, Permission: "secret.deliver",
			}
			var ordinary DenyCause
			allowed, reason, err := authz.checkDelegationCeiling(context.Background(), req, "secret.deliver", c, nil, &ordinary)
			require.NoError(t, err)
			assert.False(t, allowed, reason)
			assert.Equal(t, ordinary, r.cause, "the two walks must agree: ordinary %q, hub_delivery %q", reason, r.reason)
		})
	}
}

// TestHubDelivery_AuthorityCacheNotShared pins that the admission walk
// never reads or writes the delegator-authority cache of the ordinary walk
// in the same request.
//
// Ordinary first: after an ordinary-proof admit through an active
// super-admin delegator fills the request cache, the delegator is
// suspended, and a delivery walk on the same edge in the same request
// context denies from the live store record.
//
// Delivery first: an admitted delivery walk leaves the authority cache
// empty.
func TestHubDelivery_AuthorityCacheNotShared(t *testing.T) {
	t.Run("delivery_first", func(t *testing.T) {
		f := newGoldenFixture(t)
		ctx := contextWithDelegationCeilingCache(context.Background())
		c := tid("hd-cache-agent-delivery-first")
		hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
		hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID, nil)
		h := hdIdentity(t, f.authz, c)
		req := AuthzRequest{
			Principal: PrincipalContext{Kind: PrincipalKindAgent, ID: c, Identity: h},
			Resource:  Resource{Type: "secret", ID: f.secretID}, Action: ActionDeliver, Permission: "secret.deliver",
		}
		require.Empty(t, getDelegationCeilingCache(ctx).authority)
		allowed, reason, err := f.authz.checkDelegationCeiling(ctx, req, "secret.deliver", c, nil, nil)
		require.NoError(t, err)
		require.True(t, allowed, "the delivery walk admits through the active project owner: %q", reason)
		assert.Empty(t, getDelegationCeilingCache(ctx).authority, "the delivery walk wrote no delegator authority")
	})

	f := newGoldenFixture(t)
	ctx := contextWithDelegationCeilingCache(context.Background())
	c := tid("hd-cache-agent")
	hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.superAdminID})
	hdEdge(t, f.store, store.DelegationPrincipalUser, f.superAdminID, c, f.projectAlpha.ID, nil)
	h := hdIdentity(t, f.authz, c)
	res := Resource{Type: "secret", ID: f.secretID}

	stored, err := f.store.GetAgent(ctx, c)
	require.NoError(t, err)
	ordinaryReq := AuthzRequest{
		Principal: PrincipalContext{Kind: PrincipalKindAgent, ID: c, Identity: &storedAgentIdentity{agent: stored}},
		Resource:  res, Action: ActionDeliver, Permission: "secret.deliver",
	}
	allowed, reason, err := f.authz.checkDelegationCeiling(ctx, ordinaryReq, "secret.deliver", c, nil, nil)
	require.NoError(t, err)
	require.True(t, allowed, "the ordinary proof admits through the active super-admin: %q", reason)
	require.NotEmpty(t, getDelegationCeilingCache(ctx).authority, "the ordinary proof cached its delegator authority")

	setUserStatus(t, f.store, f.superAdminID, store.UserStatusSuspended)

	hdReq := AuthzRequest{
		Principal: PrincipalContext{Kind: PrincipalKindAgent, ID: c, Identity: h},
		Resource:  res, Action: ActionDeliver, Permission: "secret.deliver",
	}
	var cause DenyCause
	allowed, reason, err = f.authz.checkDelegationCeiling(ctx, hdReq, "secret.deliver", c, nil, &cause)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Equal(t, "delegator "+f.superAdminID+" is suspended", reason)
	assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, cause)
}

// =============================================================================
// Stage 4 (hubDeliverySourceHolds)
// =============================================================================

// TestHubDelivery_Stage4SourceHoldsDirect covers every rule of the
// hub_delivery stage-4 arm through direct hubDeliverySourceHolds calls.
func TestHubDelivery_Stage4SourceHoldsDirect(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	c := tid("hd-s4-agent")
	hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
	hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID, nil)
	h := hdIdentity(t, f.authz, c)

	// literal returns a hub_delivery literal for agent id with the given
	// stored ancestry.
	literal := func(id string, ancestry []string) *hubDeliveryIdentity {
		return &hubDeliveryIdentity{
			agentID: id, projectID: f.projectAlpha.ID, ancestry: ancestry, originUserID: ancestry[0], boundAgentID: id,
			evidence: &storedAgentIdentity{agent: &store.Agent{ID: id, ProjectID: f.projectAlpha.ID, Ancestry: ancestry}},
		}
	}
	src := func(owner string) *SharingSource {
		return &SharingSource{Kind: "secret", ID: f.secretID, OwnerID: owner, Policy: SharingPolicyOptInRequired, OptedIn: true}
	}
	secret := Resource{Type: "secret", ID: f.secretID}
	allFailing := NewAuthzService(&materialFailingStore{
		Store:                            f.store,
		getUserErr:                       errors.New("injected user failure"),
		getAgentErr:                      errors.New("injected agent failure"),
		getDelegationEdgesForDelegateErr: errors.New("injected edge failure"),
	}, slog.Default())

	t.Run("user_source/holds", func(t *testing.T) {
		holds, detail := f.authz.hubDeliverySourceHolds(ctx, h, src(f.projectOwnerID), secret, ActionDeliver, "secret.deliver")
		assert.True(t, holds, detail)
		assert.Empty(t, detail)
	})

	// The owner differs from the agent's OwnerID: the source is typed by
	// its ancestry position, never by the agent's owner field.
	t.Run("user_source/owner_differs_from_agent_owner", func(t *testing.T) {
		a := tid("hd-s4-parent")
		hd := literal(tid("hd-s4-child"), []string{f.projectOwnerID, a})
		hd.evidence.agent.OwnerID = a
		holds, detail := f.authz.hubDeliverySourceHolds(ctx, hd, src(f.projectOwnerID), secret, ActionDeliver, "secret.deliver")
		assert.True(t, holds, detail)
	})

	rows := []struct {
		name       string
		authz      *AuthzService
		h          *hubDeliveryIdentity
		src        *SharingSource
		action     Action
		perm       string
		wantDetail string
		wantPrefix bool
	}{
		{name: "tuple/missing_credential", h: nil, src: src(f.projectOwnerID), wantDetail: "delivery credential is missing"},
		{name: "tuple/no_evidence", h: func() *hubDeliveryIdentity { x := *h; x.evidence = nil; return &x }(), src: src(f.projectOwnerID),
			wantDetail: "delivery credential has no stored-agent evidence"},
		{name: "tuple/wrong_bound", h: func() *hubDeliveryIdentity { x := *h; x.boundAgentID = tid("hd-s4-other"); return &x }(), src: src(f.projectOwnerID),
			wantDetail: "delivery credential is bound to a different agent"},
		{name: "tuple/action_read", h: h, src: src(f.projectOwnerID), action: ActionRead, wantDetail: "delivery credential is limited to deliver permissions"},
		{name: "tuple/non_deliver_permission", h: h, src: src(f.projectOwnerID), perm: "secret.use", wantDetail: "delivery credential is limited to deliver permissions"},
		{name: "owner_empty", h: h, src: src(""), wantDetail: "delivery source has no owner"},
		{name: "owner_not_in_ancestry", h: h, src: src(f.memberAlphaID), wantDetail: "delivery source is not in the target's ancestry"},
		{
			// The form handleAgentSecrets writes for an agent-created row:
			// no layer strips the prefix.
			name: "created_by_prefixed_agent_no_match", h: literal(tid("hd-s4-prefixed"), []string{f.projectOwnerID, tid("hd-s4-prefixed-parent")}),
			src: src("agent:" + tid("hd-s4-prefixed-parent")), wantDetail: "delivery source is not in the target's ancestry",
		},
		{
			name: "owner_twice_in_ancestry", h: literal(tid("hd-s4-twice"), []string{f.projectOwnerID, f.projectOwnerID}),
			src: src(f.projectOwnerID), wantDetail: "delivery source appears more than once in the target's ancestry",
		},
		{
			// The agent-owned source arm is not enabled; it waits on the
			// open decision on agent-shared user-scope rows
			// (ptone/scion#2228). An owner at ancestry position 1 or
			// later is refused before any store read, so even a store
			// that fails every read gives this detail.
			name: "agent_source_refused", authz: allFailing,
			h:          literal(tid("hd-s4-agent-src-child"), []string{f.projectOwnerID, tid("hd-s4-agent-src-parent")}),
			src:        src(tid("hd-s4-agent-src-parent")),
			wantDetail: "delivery source is an agent; agent-owned sources are not admitted",
		},
		{
			name: "user_source/not_live", h: literal(tid("hd-s4-ghost-child"), []string{tid("hd-s4-ghost-user")}),
			src: src(tid("hd-s4-ghost-user")), wantDetail: "delivery source user " + tid("hd-s4-ghost-user") + " is not live",
		},
		{name: "lookup_error/agent_probe", authz: NewAuthzService(&materialFailingStore{Store: f.store, getAgentErr: errors.New("injected agent failure")}, slog.Default()),
			h: h, src: src(f.projectOwnerID), wantDetail: "delivery source check failed (fail-closed): injected agent failure"},
		{name: "lookup_error/user", authz: NewAuthzService(&materialFailingStore{Store: f.store, getUserErr: errors.New("injected user failure")}, slog.Default()),
			h: h, src: src(f.projectOwnerID), wantDetail: "delivery source check failed (fail-closed): injected user failure"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			a := row.authz
			if a == nil {
				a = f.authz
			}
			action := row.action
			if action == "" {
				action = ActionDeliver
			}
			perm := row.perm
			if perm == "" {
				perm = "secret.deliver"
			}
			holds, detail := a.hubDeliverySourceHolds(ctx, row.h, row.src, secret, action, perm)
			assert.False(t, holds)
			assert.Equal(t, row.wantDetail, detail)
		})
	}

	t.Run("user_source/suspended", func(t *testing.T) {
		g := newGoldenFixture(t)
		gc := tid("hd-s4-suspended-agent")
		hdAgent(t, g.store, gc, g.projectAlpha.ID, []string{g.memberAlphaID})
		gh := hdIdentity(t, g.authz, gc)
		setUserStatus(t, g.store, g.memberAlphaID, store.UserStatusSuspended)
		holds, detail := g.authz.hubDeliverySourceHolds(ctx, gh, src(g.memberAlphaID), secret, ActionDeliver, "secret.deliver")
		assert.False(t, holds)
		assert.Equal(t, "delivery source user "+g.memberAlphaID+" is suspended", detail)
	})

	// Malformed state (deny-only): a user ID that also names an agent row.
	t.Run("user_source/ambiguous_type", func(t *testing.T) {
		g := newGoldenFixture(t)
		gc := tid("hd-s4-ambiguous-agent")
		hdAgent(t, g.store, gc, g.projectAlpha.ID, []string{g.projectOwnerID})
		gh := hdIdentity(t, g.authz, gc)
		hdAgent(t, g.store, g.projectOwnerID, g.projectAlpha.ID, []string{g.projectOwnerID})
		holds, detail := g.authz.hubDeliverySourceHolds(ctx, gh, src(g.projectOwnerID), secret, ActionDeliver, "secret.deliver")
		assert.False(t, holds)
		assert.Equal(t, "delivery source owner resolves to both a user and an agent", detail)
	})

	// The routing: relationshipSourceStage sends every hub_delivery
	// principal, a typed nil included, to the arm.
	t.Run("routing", func(t *testing.T) {
		holds, detail := f.authz.relationshipSourceStage(ctx, PrincipalContext{Kind: PrincipalKindAgent, Identity: (*hubDeliveryIdentity)(nil)},
			src(f.projectOwnerID), secret, ActionDeliver, "secret.deliver")
		assert.False(t, holds)
		assert.Equal(t, "delivery credential is missing", detail)
	})
}

// =============================================================================
// Decide-level rows
// =============================================================================

// assertNoDeliveryCeilingStep asserts step 10 never ran for d.
func assertNoDeliveryCeilingStep(t *testing.T, d Decision) {
	t.Helper()
	for _, s := range d.ExplainTrace {
		assert.False(t, strings.HasPrefix(s.Step, "delegation_ceiling_delivery_"), "step 10 must not run: %+v", d.ExplainTrace)
	}
}

// hdWithStore points a at store m for the rest of the test. Decide-level
// fault rows use it instead of a fresh AuthzService, so the server's own
// configuration (the progeny adapters among it) stays in place.
func hdWithStore(t *testing.T, a *AuthzService, m store.Store) *AuthzService {
	t.Helper()
	orig := a.store
	a.store = m
	t.Cleanup(func() { a.store = orig })
	return a
}

// hdDevSecret stores a user-scope secret owned by the local development
// user, opted in to progeny delivery, and returns it as a resource.
func hdDevSecret(t *testing.T, f *goldenFixture) Resource {
	t.Helper()
	id := tid("hd-dev-user-secret")
	require.NoError(t, f.store.CreateSecret(context.Background(), &store.Secret{
		ID: id, Key: "hd_dev_user_secret", Scope: "user", ScopeID: DevUserID, AllowProgeny: true, CreatedBy: DevUserID,
	}))
	return Resource{Type: "secret", ID: id}
}

// TestHubDelivery_DecideRows runs hub_delivery requests through Decide on
// the golden fixture's opted-in user secret (or, for the local development
// rows, a secret of the local development user).
//
//   - Allowed rows assert the admit and the step-10 trace.
//   - Rows the step-10 walk denies assert its reason and cause.
//   - The other rows are the Decide companions of the Direct rows in
//     TestHubDelivery_Step10Admission whose fault an earlier relationship
//     stage catches. They assert that stage and its detail (Step 9 replaces
//     the reason, and step 10 never runs).
func TestHubDelivery_DecideRows(t *testing.T) {
	const (
		noSource     = "execution agent has no authoritative source user"
		notActive    = "execution source user is not active"
		lookupFailed = "execution agent lookup failed"
	)
	type row struct {
		name  string
		setup func(t *testing.T, f *goldenFixture) string // returns the target agent
		// afterMint, when set, runs after the credential is built.
		afterMint func(t *testing.T, f *goldenFixture, c string)
		// faults, when set, gives the fault store Decide runs on; the
		// credential is built before it applies.
		faults func(f *goldenFixture, c string) *materialFailingStore
		// resource, when set, replaces the golden opted-in secret.
		resource func(t *testing.T, f *goldenFixture) Resource
		// Exactly one of wantHops (an admit), wantStage (a relationship
		// reject kind) or wantCause (a step-10 deny) is set.
		wantHops   int
		wantStage  string
		wantDetail string
		wantCause  DenyCause
		wantReason string
	}
	withEdge := func(name string, mutate func(e *store.DelegationEdge)) func(t *testing.T, f *goldenFixture) string {
		return func(t *testing.T, f *goldenFixture) string {
			c := tid("hd-decide-" + name)
			hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
			hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID, mutate)
			return c
		}
	}
	// twoHopA names the inner delegator agent of twoHop(name).
	twoHopA := func(name string) string { return tid("hd-decide-2hop-" + name + "-a") }
	// twoHop builds U -> A (principal) -> C, U = f.projectOwnerID; C's edge
	// is bounded with secret.deliver unless inner is set. after runs last.
	twoHop := func(name string, inner func(e *store.DelegationEdge), after func(t *testing.T, f *goldenFixture, a string)) func(t *testing.T, f *goldenFixture) string {
		return func(t *testing.T, f *goldenFixture) string {
			a, c := twoHopA(name), tid("hd-decide-2hop-"+name+"-c")
			hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.projectOwnerID})
			hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID, a})
			hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, a, f.projectAlpha.ID, nil)
			if inner == nil {
				inner = withBoundedCeiling("secret.deliver")
			}
			hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, inner)
			if after != nil {
				after(t, f, a)
			}
			return c
		}
	}
	devLocal := func(name string, enabled bool, mutate func(e *store.DelegationEdge)) func(t *testing.T, f *goldenFixture) string {
		return func(t *testing.T, f *goldenFixture) string {
			f.authz.setDevLocalAuthorityEnabled(enabled)
			c := tid("hd-decide-" + name)
			hdDevLocalAgent(t, f, c, mutate)
			return c
		}
	}
	softDelete := func(t *testing.T, f *goldenFixture, id string) {
		t.Helper()
		ag, err := f.store.GetAgent(context.Background(), id)
		require.NoError(t, err)
		ag.DeletedAt = time.Now()
		require.NoError(t, f.store.UpdateAgent(context.Background(), ag))
	}
	overrideEdges := func(edges func(f *goldenFixture, c string) []*store.DelegationEdge) func(f *goldenFixture, c string) *materialFailingStore {
		return func(f *goldenFixture, c string) *materialFailingStore {
			return &materialFailingStore{Store: f.store, delegationEdgesOverride: edges(f, c)}
		}
	}
	recordedEdge := func(id, delegatorType, delegatorID, c, projectID string) *store.DelegationEdge {
		return &store.DelegationEdge{
			ID: id, DelegatorType: delegatorType, DelegatorID: delegatorID,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: c,
			ScopeType: store.RoleScopeProject, ScopeID: projectID, Active: true,
			AuthorityProvenance: store.AuthorityProvenance{ProvenanceVersion: 1, SourcePrincipalKind: delegatorType, SourcePrincipalID: delegatorID, SourceCredentialKind: store.SourceCredentialSession},
			EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}
	}

	rows := []row{
		// Allowed.
		{name: "allowed/single_hop_principal", setup: withEdge("allowed", nil), wantHops: 1},
		{name: "allowed/two_hop", setup: twoHop("allowed", nil, nil), wantHops: 2},
		{
			// The workstation path, with dev auth on (positive control for
			// dev_local_dev_auth_off).
			name: "allowed/dev_local_principal", setup: devLocal("dev-allowed", true, nil),
			resource: hdDevSecret, wantHops: 1,
		},
		{
			name: "allowed/dev_local_scheduler_initiator", setup: devLocal("dev-sched-allowed", true, withDevLocalSchedulerProvenance),
			resource: hdDevSecret, wantHops: 1,
		},

		// Step 10 denies.
		{
			name:       "bounded_without_permission",
			setup:      withEdge("bounded-no-perm", withBoundedCeiling("agent.read")),
			wantCause:  DenyCauseCeilingEffectExceeded,
			wantReason: "delegation ceiling: secret.deliver is outside the frozen effect ceiling",
		},
		{
			// Stage 2b's resolver does not apply the dev auth check, so the
			// walk is the first to deny.
			name: "dev_local_dev_auth_off", setup: devLocal("dev-off", false, nil), resource: hdDevSecret,
			wantCause: DenyCauseCeilingSourceNotAllowed, wantReason: "delegation ceiling: " + reasonDevLocalDisabled,
		},
		{
			// A pre-provenance edge: stage 2b's resolver accepts the legacy
			// hop; the walk requires recorded provenance.
			name: "unknown_provenance_version",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := tid("hd-decide-unrecorded-v0")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				createDCEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, store.DelegationPrincipalAgent, c,
					store.RoleScopeProject, f.projectAlpha.ID, string(AgentRoleFull))
				return c
			},
			wantCause: DenyCauseCeilingUnrecorded, wantReason: "delegation ceiling: secret.deliver requires recorded provenance",
		},
		{
			// Malformed state (deny-only).
			name: "unrecorded_ceiling_version_1",
			setup: withEdge("unrecorded-v1", func(e *store.DelegationEdge) {
				e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
			}),
			wantCause: DenyCauseCeilingUnrecorded, wantReason: "delegation ceiling: secret.deliver requires recorded provenance",
		},
		{
			name:      "two_hop/inner_bounded_without_permission",
			setup:     twoHop("inner-no-perm", withBoundedCeiling("agent.read"), nil),
			wantCause: DenyCauseCeilingEffectExceeded, wantReason: "delegation ceiling: secret.deliver is outside the frozen effect ceiling",
		},

		// Stage 2b (execution project) denies first.
		{
			name: "missing_edge/backfill_absent",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := tid("hd-decide-missing")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				return c
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			name: "missing_edge/backfill_present",
			setup: func(t *testing.T, f *goldenFixture) string {
				markEdgeBackfillComplete(t, f.store)
				c := tid("hd-decide-missing-bf")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				return c
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			// Malformed state (deny-only).
			name:      "inactive_edge_only",
			setup:     withEdge("inactive-edge", func(e *store.DelegationEdge) { e.Active = false }),
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			// Malformed state (deny-only): two active edges for one
			// delegate and scope.
			name:  "two_active_edges",
			setup: withEdge("two-edges", nil),
			faults: overrideEdges(func(f *goldenFixture, c string) []*store.DelegationEdge {
				return []*store.DelegationEdge{
					recordedEdge(tid("hd-decide-dup-1"), store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID),
					recordedEdge(tid("hd-decide-dup-2"), store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID),
				}
			}),
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			name: "migration_sentinel",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := tid("hd-decide-sentinel")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				createDCEdge(t, f.store, store.DelegationPrincipalUser, migrationDelegatorID, store.DelegationPrincipalAgent, c,
					store.RoleScopeProject, f.projectAlpha.ID, string(AgentRoleFull))
				return c
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			// Malformed state (deny-only).
			name: "dev_local_wrong_delegator",
			setup: func(t *testing.T, f *goldenFixture) string {
				f.authz.setDevLocalAuthorityEnabled(true)
				return withEdge("dev-wrong", func(e *store.DelegationEdge) { e.SourceCredentialKind = store.SourceCredentialDevLocal })(t, f)
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			name: "delegator_user_missing",
			setup: func(t *testing.T, f *goldenFixture) string {
				c, ghost := tid("hd-decide-user-missing"), tid("hd-decide-ghost-user")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{ghost})
				hdEdge(t, f.store, store.DelegationPrincipalUser, ghost, c, f.projectAlpha.ID, nil)
				return c
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			name: "delegator_user_suspended",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := withEdge("suspended", nil)(t, f)
				setUserStatus(t, f.store, f.projectOwnerID, store.UserStatusSuspended)
				return c
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: notActive,
		},
		{
			name: "delegator_super_admin_suspended",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := tid("hd-decide-superadmin-suspended")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.superAdminID})
				hdEdge(t, f.store, store.DelegationPrincipalUser, f.superAdminID, c, f.projectAlpha.ID, nil)
				setUserStatus(t, f.store, f.superAdminID, store.UserStatusSuspended)
				return c
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: notActive,
		},
		{
			name: "cycle",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := tid("hd-decide-cycle")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				hdEdge(t, f.store, store.DelegationPrincipalAgent, c, c, f.projectAlpha.ID, nil)
				return c
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			// Malformed state (deny-only).
			name:      "two_hop/inner_delegator_deleted",
			setup:     twoHop("inner-deleted", nil, func(t *testing.T, f *goldenFixture, a string) { softDelete(t, f, a) }),
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			name: "two_hop/outer_delegator_suspended",
			setup: twoHop("outer-suspended", nil, func(t *testing.T, f *goldenFixture, _ string) {
				setUserStatus(t, f.store, f.projectOwnerID, store.UserStatusSuspended)
			}),
			wantStage: RelationshipRejectExecutionProject, wantDetail: notActive,
		},
		{
			name:  "two_hop/delegator_agent_not_found",
			setup: twoHop("agent-not-found", nil, nil),
			faults: func(f *goldenFixture, _ string) *materialFailingStore {
				return &materialFailingStore{Store: f.store, getAgentNotFoundID: twoHopA("agent-not-found")}
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			name:  "lookup_error/edges",
			setup: withEdge("edge-err", nil),
			faults: func(f *goldenFixture, _ string) *materialFailingStore {
				return &materialFailingStore{Store: f.store, getDelegationEdgesForDelegateErr: errors.New("injected edge failure")}
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			name:  "lookup_error/delegator_agent",
			setup: twoHop("agent-err", nil, nil),
			faults: func(f *goldenFixture, _ string) *materialFailingStore {
				return &materialFailingStore{Store: f.store, getAgentErr: errors.New("injected agent failure"), getAgentErrID: twoHopA("agent-err")}
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			// The Direct row's fault is the target's own load in
			// ResolveProvenanceRoot; through Decide, stage 2b loads the
			// target first.
			name:  "lookup_error/provenance_root",
			setup: withEdge("root-err", nil),
			faults: func(f *goldenFixture, c string) *materialFailingStore {
				return &materialFailingStore{Store: f.store, getAgentErr: errors.New("injected agent failure"), getAgentErrID: c}
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: lookupFailed,
		},
		{
			name: "root_not_admitted",
			// memberNone is an active hub member with no alpha membership.
			// Stage 2b runs before the item fact, so the item row does not
			// matter here.
			setup: func(t *testing.T, f *goldenFixture) string {
				c := tid("hd-decide-not-admitted")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.memberNoneID})
				hdEdge(t, f.store, store.DelegationPrincipalUser, f.memberNoneID, c, f.projectAlpha.ID, nil)
				return c
			},
			wantStage:  RelationshipRejectExecutionProject,
			wantDetail: "execution source user lacks admission to the agent's project",
		},
		{
			// Malformed state (deny-only): the target is soft-deleted after
			// the credential was built.
			name:      "target_deleted_after_mint",
			setup:     withEdge("target-deleted", nil),
			afterMint: softDelete,
			wantStage: RelationshipRejectExecutionProject, wantDetail: "execution agent is deleted",
		},
		{
			// Malformed state (deny-only).
			name:      "delegator_type_mismatch",
			setup:     withEdge("type-mismatch", func(e *store.DelegationEdge) { e.SourcePrincipalKind = store.DelegationPrincipalAgent }),
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			// Malformed state (deny-only).
			name: "scheduler_edge_inconsistent",
			setup: func(t *testing.T, f *goldenFixture) string {
				_, h, _ := hdSchedulerInconsistent(t, f)
				return h.ID()
			},
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
		{
			// Malformed state (deny-only): a delegator type outside
			// {user, agent}.
			name:  "unsupported_delegator_type",
			setup: withEdge("unsupported-type", nil),
			faults: overrideEdges(func(f *goldenFixture, c string) []*store.DelegationEdge {
				e := recordedEdge(tid("hd-decide-group-edge"), "group", tid("hd-decide-group"), c, f.projectAlpha.ID)
				return []*store.DelegationEdge{e}
			}),
			wantStage: RelationshipRejectExecutionProject, wantDetail: noSource,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newGoldenFixture(t)
			withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
			c := r.setup(t, f)
			h := hdIdentity(t, f.authz, c)
			if r.afterMint != nil {
				r.afterMint(t, f, c)
			}
			authz := f.authz
			if r.faults != nil {
				authz = hdWithStore(t, f.authz, r.faults(f, c))
			}
			res := Resource{Type: "secret", ID: f.secretID}
			if r.resource != nil {
				res = r.resource(t, f)
			}
			d := decidePerm(authz, h, res, ActionDeliver, "secret.deliver", true)
			switch {
			case r.wantHops > 0:
				require.True(t, d.Allowed, "reason %q", d.Reason)
				assert.Equal(t, ScopeTypeRelationship, d.Scope)
				assert.True(t, relationshipResult(t, d, RelationshipRuleProgeny).Accepted)
				assert.Equal(t, r.wantHops, countDecisionSteps(d.ExplainTrace, "delegation_ceiling_delivery_hop"), "trace: %+v", d.ExplainTrace)
				assert.True(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_root"), "trace: %+v", d.ExplainTrace)
			case r.wantStage != "":
				assert.False(t, d.Allowed, "reason %q", d.Reason)
				assert.Equal(t, "relationship grant restricted by "+r.wantStage, d.Reason)
				assert.Equal(t, DenyCause(""), d.DenyCause)
				rr := relationshipResult(t, d, RelationshipRuleProgeny)
				assert.Equal(t, r.wantStage, rr.RejectedBy)
				assert.Equal(t, r.wantDetail, rr.Detail)
				assertNoDeliveryCeilingStep(t, d)
			default:
				assert.False(t, d.Allowed, "reason %q", d.Reason)
				assert.Equal(t, r.wantReason, d.Reason)
				assert.Equal(t, r.wantCause, d.DenyCause)
				assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
				assert.True(t, relationshipResult(t, d, RelationshipRuleProgeny).Accepted, "the item grant passed; step 10 denied")
				assert.True(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_denied"), "trace: %+v", d.ExplainTrace)
			}
		})
	}

	// A lookup error at the walk's delegator read. GetUser fails only when
	// called from the walk's liveness check, so every earlier user read
	// (stage 2b's resolver and admission, stage 4's source check) passes:
	// a single fault. The guard asserts the injection fired exactly once
	// and that earlier stages did read users.
	t.Run("lookup_error/delegator_user", func(t *testing.T) {
		f := newGoldenFixture(t)
		withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
		c := withEdge("user-err", nil)(t, f)
		h := hdIdentity(t, f.authz, c)
		m := &materialFailingStore{Store: f.store, getUserErr: errors.New("injected user failure"), getUserErrCaller: "hubDeliveryDelegatorLiveness"}
		authz := hdWithStore(t, f.authz, m)
		d := decidePerm(authz, h, Resource{Type: "secret", ID: f.secretID}, ActionDeliver, "secret.deliver", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, "delivery ceiling check failed (fail-closed): injected user failure", d.Reason)
		assert.Equal(t, DenyCauseCeilingError, d.DenyCause)
		assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
		assert.True(t, relationshipResult(t, d, RelationshipRuleProgeny).Accepted, "every relationship stage passed")
		assert.Equal(t, 1, m.getUserErrCallerFired, "exactly one injected failure")
		assert.Greater(t, m.getUserCalls, m.getUserErrCallerFired, "earlier stages read users from the store")
	})
}

// TestHubDelivery_Stage4AgentSource runs the stage-4 source rules through
// Decide on a delegation chain written by production paths: the creator's
// session creates A, and A's own token creates C (C.Ancestry = [creator,
// A]; C's edge is bounded and carries the deliver permissions).
//
// The item rows are stored at the store layer. The user-owned row has the
// shape the user secret handler writes. The agent-authored row (CreatedBy
// = A's bare ID, user scope, opted in) is a shape no production handler
// writes; it is the agent-owned source path, which stays refused until the
// open decision on agent-shared user-scope rows is made (ptone/scion#2228).
func TestHubDelivery_Stage4AgentSource(t *testing.T) {
	f := newChainFixture(t, "hd-s4-chain")
	ctx := context.Background()
	authz := f.srv.authzService
	withDeliveryCredentialKinds(t, CredentialKindHubDelivery)

	a, _ := f.sessionParent(t, "hd-s4-a")
	c, cEdge := f.childOf(t, a, "hd-s4-c")
	require.Equal(t, []string{f.creator.ID, a.ID}, c.Ancestry)
	require.Contains(t, deliverOf(cEdge.PermissionIDs), "secret.deliver", "C's edge carries the deliver permissions")
	other := hubMemberUser(t, f.store, "hd-s4-other-user")

	secret := func(id, key, scopeUser, createdBy string) string {
		require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
			ID: id, Key: key, Scope: "user", ScopeID: scopeUser, AllowProgeny: true, CreatedBy: createdBy,
		}))
		return id
	}
	userRow := secret(tid("hd-s4-user-row"), "hd_s4_user_row", f.creator.ID, f.creator.ID)
	agentRow := secret(tid("hd-s4-agent-row"), "hd_s4_agent_row", f.creator.ID, a.ID)
	prefixedRow := secret(tid("hd-s4-prefixed-row"), "hd_s4_prefixed_row", f.creator.ID, "agent:"+a.ID)
	otherRow := secret(tid("hd-s4-other-row"), "hd_s4_other_row", other.ID, other.ID)

	h := hdIdentity(t, authz, c.ID)
	decide := func(rowID string) Decision {
		return decidePerm(authz, h, Resource{Type: "secret", ID: rowID}, ActionDeliver, "secret.deliver", true)
	}

	t.Run("positive/owner_differs_from_root", func(t *testing.T) {
		require.Equal(t, a.ID, c.OwnerID, "C's owner is its creator agent A, not the root user")
		d := decide(userRow)
		require.True(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, ScopeTypeRelationship, d.Scope)
		assert.Empty(t, d.RoleName)
		r := relationshipResult(t, d, RelationshipRuleProgeny)
		assert.True(t, r.Accepted, "%+v", r)
		require.NotNil(t, r.Source)
		assert.Equal(t, f.creator.ID, r.Source.OwnerID)
		assert.Equal(t, 2, countDecisionSteps(d.ExplainTrace, "delegation_ceiling_delivery_hop"), "trace: %+v", d.ExplainTrace)
		assert.True(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_root"))
	})

	t.Run("agent_authored_row_refused", func(t *testing.T) {
		d := decide(agentRow)
		assert.False(t, d.Allowed)
		assert.Equal(t, "relationship grant restricted by "+RelationshipRejectSourceInactive, d.Reason)
		assert.Equal(t, DenyCause(""), d.DenyCause)
		r := relationshipResult(t, d, RelationshipRuleProgeny)
		assert.Equal(t, RelationshipRejectSourceInactive, r.RejectedBy)
		assert.Equal(t, "delivery source is an agent; agent-owned sources are not admitted", r.Detail)
		assertNoDeliveryCeilingStep(t, d)
	})

	for name, rowID := range map[string]string{"created_by_prefixed_agent_no_match": prefixedRow, "owner_not_in_ancestry": otherRow} {
		t.Run(name, func(t *testing.T) {
			d := decide(rowID)
			assert.False(t, d.Allowed)
			r := relationshipResult(t, d, RelationshipRuleProgeny)
			assert.Equal(t, RelationshipRejectFact, r.RejectedBy, "denied at stage 3: %+v", r)
			assertNoDeliveryCeilingStep(t, d)
		})
	}
}

// TestHubDelivery_AgentCreatedChain runs hub_delivery requests through
// Decide on delegation chains in which agents create agents. Agents and
// edges are written by the production create paths, except where a row
// says legacy-state seeding: an agent row and edge in the shape written
// before provenance was recorded. The item is the creator's user-scope
// secret, opted in to progeny delivery, so stage 4 takes the user-source
// branch (position 0).
func TestHubDelivery_AgentCreatedChain(t *testing.T) {
	// userSecret stores the creator's opted-in user secret.
	userSecret := func(t *testing.T, f *chainFixture, name string) Resource {
		t.Helper()
		id := tid(name + "-secret")
		require.NoError(t, f.store.CreateSecret(context.Background(), &store.Secret{
			ID: id, Key: strings.ReplaceAll(name, "-", "_") + "_secret", Scope: "user", ScopeID: f.creator.ID,
			AllowProgeny: true, CreatedBy: f.creator.ID,
		}))
		return Resource{Type: "secret", ID: id}
	}
	const effectExceeded = "delegation ceiling: secret.deliver is outside the frozen effect ceiling"
	// assertDeniedAtFirstHop asserts a step-10 effect-ceiling deny at the
	// target's own hop, after every relationship stage passed.
	assertDeniedAtFirstHop := func(t *testing.T, d Decision) {
		t.Helper()
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, effectExceeded, d.Reason)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause)
		assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
		assert.True(t, relationshipResult(t, d, RelationshipRuleProgeny).Accepted, "stage 2b and stage 4 passed; step 10 denied")
		assert.Equal(t, 1, countDecisionSteps(d.ExplainTrace, "delegation_ceiling_delivery_hop"), "denied at the target's hop: %+v", d.ExplainTrace)
		assert.False(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_root"))
	}

	// U's session creates A; A's token creates B; B's token creates C.
	// Every agent-created edge carries the deliver permissions.
	t.Run("multi_hop", func(t *testing.T) {
		f := newChainFixture(t, "hd-acc-multi")
		withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
		authz := f.srv.authzService
		a, _ := f.sessionParent(t, "hd-acc-multi-a")
		b, bEdge := f.childOf(t, a, "hd-acc-multi-b")
		c, cEdge := f.childOf(t, b, "hd-acc-multi-c")
		require.Equal(t, []string{f.creator.ID, a.ID, b.ID}, c.Ancestry)
		require.Contains(t, deliverOf(bEdge.PermissionIDs), "secret.deliver")
		require.Contains(t, deliverOf(cEdge.PermissionIDs), "secret.deliver")

		d := decidePerm(authz, hdIdentity(t, authz, c.ID), userSecret(t, f, "hd-acc-multi"), ActionDeliver, "secret.deliver", true)
		require.True(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, ScopeTypeRelationship, d.Scope)
		assert.Equal(t, 3, countDecisionSteps(d.ExplainTrace, "delegation_ceiling_delivery_hop"), "one hop step per edge: %+v", d.ExplainTrace)
		assert.True(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_root"))
	})

	// U's UAT creates A: A's edge is bounded by the UAT's selectors, none
	// of which covers a deliver permission, but the deliver permissions are
	// intrinsic to a hop recorded from a user access token
	// (uatHopEffectCeiling). A's token then creates C, whose edge carries
	// them (parentDeliverEligibility over A's chain), so the strict walk
	// admits C through both hops, and the root admission runs.
	t.Run("uat_parent_deliver_intrinsic", func(t *testing.T) {
		f := newChainFixture(t, "hd-acc-uat")
		withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
		authz := f.srv.authzService
		// A's token carries agent:create through the dev-auth mint
		// override (a role below full mints as full, filtered by the
		// chain ceiling), which the workstation server applies; set it
		// here so the row does not depend on the test server's default.
		authz.mintDevAuthOverride = true
		f.setProjectAnnotation(t, projectSettingDefaultAgentRole, string(AgentRoleFull))
		rec := f.create(t, f.uat(t, minimalSelectors(t)...), CreateAgentRequest{Name: "hd-acc-uat-a"})
		a, aEdge := f.createdAgent(t, rec, "hd-acc-uat-a")
		require.Equal(t, store.EffectCeilingBounded, aEdge.Kind)
		require.Contains(t, aEdge.PermissionIDs, "agent.create", "the UAT holds agent:create")
		require.Empty(t, deliverOf(aEdge.PermissionIDs), "no UAT selector covers a deliver permission: the stored edge is the token's list")

		c, cEdge := f.childOf(t, a, "hd-acc-uat-c")
		require.Equal(t, []string{f.creator.ID, a.ID}, c.Ancestry)
		require.Equal(t, store.EffectCeilingBounded, cEdge.Kind)
		assert.ElementsMatch(t, hubDeliveryPermissionList, deliverOf(cEdge.PermissionIDs), "A's chain allows the deliver permissions, so C's edge carries them")

		d := decidePerm(authz, hdIdentity(t, authz, c.ID), userSecret(t, f, "hd-acc-uat"), ActionDeliver, "secret.deliver", true)
		require.True(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, 2, countDecisionSteps(d.ExplainTrace, "delegation_ceiling_delivery_hop"), "one hop step per edge: %+v", d.ExplainTrace)
		assert.True(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_uat_intrinsic"), "A's hop passes through the intrinsic rule")
		assert.True(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_root"))
	})

	// Legacy-state seeding: A has the pre-provenance shape (ancestry [U]
	// and a zero-value edge user:U -> A). A's token creates C. Stage 2b's
	// resolver accepts the legacy hop and stage 4 holds for the user
	// source; step 10 still denies, at C's hop, whose edge carries no
	// deliver permission. This is where the two walks differ: the
	// structural resolver admits an unrecorded hop, the admission walk
	// never does.
	t.Run("unrecorded_parent", func(t *testing.T) {
		f := newChainFixture(t, "hd-acc-unrec")
		withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
		authz := f.srv.authzService
		a := createFixtureAgent(t, f.bypassAgentsFixture, "hd-acc-unrec-a", []string{f.creator.ID}, AgentRoleFull)
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.creator.ID, a.ID, f.proj.ID)

		c, cEdge := f.childOf(t, a, "hd-acc-unrec-c")
		require.Equal(t, []string{f.creator.ID, a.ID}, c.Ancestry)
		require.Equal(t, store.EffectCeilingBounded, cEdge.Kind)
		require.Empty(t, deliverOf(cEdge.PermissionIDs), "an unrecorded parent hop gives C no deliver permission")

		d := decidePerm(authz, hdIdentity(t, authz, c.ID), userSecret(t, f, "hd-acc-unrec"), ActionDeliver, "secret.deliver", true)
		assertDeniedAtFirstHop(t, d)
	})

	// Legacy-state seeding: A has the pre-provenance shape with no edge.
	// Before the edge backfill completes, A's create of C is refused at
	// C's dispatch mint (ceiling_orphaned) and leaves no agent row, as
	// TestAgentCreateDeliverIDs_MissingParentEdge pins. A's own delivery
	// credential is denied: directly by the walk (no edge), and through
	// Decide by stage 2b (no authoritative source user).
	t.Run("missing_parent_edge", func(t *testing.T) {
		f := newChainFixture(t, "hd-acc-missing")
		withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
		authz := f.srv.authzService
		a := createFixtureAgent(t, f.bypassAgentsFixture, "hd-acc-missing-a", []string{f.creator.ID}, AgentRoleFull)
		require.Empty(t, activeEdgesFor(t, f.store, a.ID))

		rec := f.createAsParent(t, f.agentToken(t, a.ID), CreateAgentRequest{Name: "hd-acc-missing-c"})
		assert.Equal(t, agentTokenDenialMessage(DenyCauseCeilingOrphaned, false), assertCeilingDenial(t, rec))
		_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "hd-acc-missing-c")
		assert.ErrorIs(t, err, store.ErrNotFound, "no agent row for C")

		res := userSecret(t, f, "hd-acc-missing")
		h := hdIdentity(t, authz, a.ID)
		r := hdDirect(t, authz, h, res, "secret.deliver")
		assert.False(t, r.allowed)
		assert.Equal(t, "no active delegation edge", r.reason)
		assert.Equal(t, DenyCauseCeilingProvenanceMissing, r.cause)

		d := decidePerm(authz, h, res, ActionDeliver, "secret.deliver", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, "relationship grant restricted by "+RelationshipRejectExecutionProject, d.Reason)
		rr := relationshipResult(t, d, RelationshipRuleProgeny)
		assert.Equal(t, RelationshipRejectExecutionProject, rr.RejectedBy)
		assert.Equal(t, "execution agent has no authoritative source user", rr.Detail)
		assertNoDeliveryCeilingStep(t, d)
	})

	// A authors a dispatch_agent schedule; the fire creates C. A scheduled
	// child's ancestry stays empty, so no progeny item can match it: the
	// request is denied before step 10. The row pins that existing
	// property; the deliver permissions on the fire's edge are pinned by
	// the scheduled-create tests.
	t.Run("scheduled_agent", func(t *testing.T) {
		f := newChainFixture(t, "hd-acc-sched")
		withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
		authz := f.srv.authzService
		ctx := context.Background()
		a, _ := f.sessionParent(t, "hd-acc-sched-a")
		require.NoError(t, f.srv.dispatchAgentEventHandler()(ctx, withAgentRevision(t, f.srv, store.ScheduledEvent{
			ID: tid("hd-acc-sched-evt"), ProjectID: f.proj.ID, EventType: "dispatch_agent",
			Payload: `{"agentName":"hd-acc-sched-c","task":"t"}`, CreatedBy: a.ID,
		}, a.ID)))
		c, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "hd-acc-sched-c")
		require.NoError(t, err)
		require.Empty(t, c.Ancestry, "a scheduled child records no ancestry")

		d := decidePerm(authz, hdIdentity(t, authz, c.ID), userSecret(t, f, "hd-acc-sched"), ActionDeliver, "secret.deliver", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		require.NotNil(t, d.Provenance)
		for _, rr := range d.Provenance.Relationships {
			assert.False(t, rr.Accepted, "no relationship candidate holds: %+v", rr)
		}
		assertNoDeliveryCeilingStep(t, d)
	})

	// On the two-hop fixture of the allowed control: a non-deliver
	// permission under hub_delivery is denied at Step 0b, and C's own
	// agent token asking for secret.deliver is denied at the Step 0
	// delivery gate.
	t.Run("non_deliver_control", func(t *testing.T) {
		f := newChainFixture(t, "hd-acc-control")
		withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
		authz := f.srv.authzService
		a, _ := f.sessionParent(t, "hd-acc-control-a")
		c, _ := f.childOf(t, a, "hd-acc-control-c")
		res := userSecret(t, f, "hd-acc-control")
		h := hdIdentity(t, authz, c.ID)

		d := decidePerm(authz, h, res, ActionDeliver, "secret.deliver", true)
		require.True(t, d.Allowed, "positive control: reason %q", d.Reason)

		assertStep0bDenied(t, decidePerm(authz, h, res, ActionRead, "secret.read", true),
			"delivery credential is limited to deliver permissions", "secret.read")

		jwt := f.agentIdentityFor(t, f.agentToken(t, c.ID))
		assertDeliveryGateDenied(t, decidePerm(authz, jwt, res, ActionDeliver, "secret.deliver", true), "agent_jwt")
	})
}
