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
// hubDeliveryIdentity. The expected outcomes are literals recorded before
// the hub_delivery stage-4 arm existed, and the test passes unchanged after
// it: stage 4 for these principals is the owner-activity check followed by
// the owner's delegation-chain check, exactly as before.
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

// boundedCeiling returns a mutate func giving the edge a bounded V1
// ceiling holding exactly ids.
func boundedCeiling(ids ...string) func(e *store.DelegationEdge) {
	return func(e *store.DelegationEdge) {
		e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: ids}
	}
}

// hdIdentity builds the hub_delivery credential for agentID through the
// production constructor.
func hdIdentity(t *testing.T, a *AuthzService, agentID string) *hubDeliveryIdentity {
	t.Helper()
	h, err := a.newHubDeliveryIdentity(context.Background(), agentID)
	require.NoError(t, err)
	return h
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
		hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, c, f.projectAlpha.ID, boundedCeiling(secretPerm))
		r := hdDirect(t, f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}, secretPerm)
		assert.True(t, r.allowed, r.reason)
	})

	t.Run("two_hop/allowed", func(t *testing.T) {
		f := newGoldenFixture(t)
		a, c := tid("hd-adm-2hop-a"), tid("hd-adm-2hop-c")
		hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.projectOwnerID})
		hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID, a})
		hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, a, f.projectAlpha.ID, nil)
		hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, boundedCeiling(secretPerm, "agent.read"))
		r := hdDirect(t, f.authz, hdIdentity(t, f.authz, c), Resource{Type: "secret", ID: f.secretID}, secretPerm)
		assert.True(t, r.allowed, r.reason)
		assert.Equal(t, 2, countDecisionSteps(r.explain, "delegation_ceiling_delivery_hop"), "explain: %+v", r.explain)
		assert.True(t, hasDecisionStep(r.explain, "delegation_ceiling_delivery_root"))
	})

	type denyRow struct {
		name       string
		setup      func(t *testing.T, f *goldenFixture) (a *AuthzService, h *hubDeliveryIdentity, res Resource)
		wantReason string // exact, or a prefix when wantPrefix
		wantPrefix bool
		wantCause  DenyCause
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
			hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, boundedCeiling(secretPerm))
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
			name: "missing_edge",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				c := tid("hd-adm-missing-edge")
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
			name: "dev_local_dev_auth_off",
			setup: single("dev-off", DevUserID, func(e *store.DelegationEdge) {
				e.SourceCredentialKind = store.SourceCredentialDevLocal
			}),
			wantReason: "delegation ceiling: " + reasonDevLocalDisabled, wantCause: DenyCauseCeilingSourceNotAllowed,
		},
		{
			// Malformed state (deny-only): local-development provenance
			// with a delegator other than the local development user.
			name: "dev_local_wrong_delegator",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				f.authz.setDevLocalAuthorityEnabled(true)
				t.Cleanup(func() { f.authz.setDevLocalAuthorityEnabled(false) })
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
			setup:      single("bounded-no-perm", "", boundedCeiling("agent.read", "secret.use")),
			wantReason: "delegation ceiling: secret.deliver is outside the frozen effect ceiling", wantCause: DenyCauseCeilingEffectExceeded,
		},
		{
			// The resource names the target agent itself: there is no
			// self-operation exception for delivery.
			name: "no_self_operation_exception",
			setup: func(t *testing.T, f *goldenFixture) (*AuthzService, *hubDeliveryIdentity, Resource) {
				a, h, _ := single("no-self-op", "", boundedCeiling("agent.read"))(t, f)
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
			wantReason: "is suspended", wantPrefix: false, wantCause: DenyCauseCeilingDelegatorLacksPermission,
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
			wantReason: "is suspended", wantCause: DenyCauseCeilingDelegatorLacksPermission,
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
				hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, boundedCeiling("agent.read"))
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
			wantReason: "is not live", wantCause: DenyCauseCeilingOrphaned,
		},
		{
			name: "two_hop/outer_delegator_suspended",
			setup: twoHop("outer-suspended", func(t *testing.T, f *goldenFixture, a, c string) *AuthzService {
				setUserStatus(t, f.store, f.projectOwnerID, store.UserStatusSuspended)
				return f.authz
			}),
			wantReason: "is suspended", wantCause: DenyCauseCeilingDelegatorLacksPermission,
		},
		{
			name: "two_hop/delegator_agent_not_found",
			setup: twoHop("agent-not-found", func(t *testing.T, f *goldenFixture, a, c string) *AuthzService {
				return failingWith(f, func(m *materialFailingStore) { m.getAgentErr = store.ErrNotFound })
			}),
			wantReason: "is not live", wantCause: DenyCauseCeilingOrphaned,
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
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newGoldenFixture(t)
			a, h, res := row.setup(t, f)
			r := hdDirect(t, a, h, res, secretPerm)
			assert.False(t, r.allowed, "reason %q", r.reason)
			switch {
			case row.wantPrefix:
				assert.True(t, strings.HasPrefix(r.reason, row.wantReason), "reason %q", r.reason)
			case strings.HasPrefix(row.wantReason, "is "):
				assert.Contains(t, r.reason, row.wantReason)
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGoldenFixture(t)
			c := tc.setup(t, f)
			res := Resource{Type: "secret", ID: f.secretID}

			r := hdDirect(t, f.authz, hdIdentity(t, f.authz, c), res, "secret.deliver")
			assert.False(t, r.allowed, r.reason)
			assert.Equal(t, tc.want, r.cause, "hub_delivery: %q", r.reason)

			stored, err := f.store.GetAgent(context.Background(), c)
			require.NoError(t, err)
			req := AuthzRequest{
				Principal: PrincipalContext{Kind: PrincipalKindAgent, ID: c, Identity: &storedAgentIdentity{agent: stored}},
				Resource:  res, Action: ActionDeliver, Permission: "secret.deliver",
			}
			var ordinary DenyCause
			allowed, reason, err := f.authz.checkDelegationCeiling(context.Background(), req, "secret.deliver", c, nil, &ordinary)
			require.NoError(t, err)
			assert.False(t, allowed, reason)
			assert.Equal(t, ordinary, r.cause, "the two walks must agree: ordinary %q, hub_delivery %q", reason, r.reason)
		})
	}
}

// TestHubDelivery_AuthorityCacheNotShared pins that the admission walk
// never reads a delegator-authority result cached by the ordinary walk in
// the same request: after an ordinary-proof admit through an active
// super-admin delegator fills the request cache, the delegator is
// suspended, and a delivery walk on the same edge in the same request
// context denies from the live store record.
func TestHubDelivery_AuthorityCacheNotShared(t *testing.T) {
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
	assert.Contains(t, reason, "is suspended")
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
			// The RQ-1b agent-owned source arm is not enabled (P-2,
			// ptone/scion#2228): an owner at ancestry position 1 or later
			// is refused before any store read, so even a store that
			// fails every read gives this detail.
			name: "agent_source_refused", authz: allFailing,
			h:          literal(tid("hd-s4-agent-src-child"), []string{f.projectOwnerID, tid("hd-s4-agent-src-parent")}),
			src:        src(tid("hd-s4-agent-src-parent")),
			wantDetail: "delivery source is an agent; agent-owned sources are not admitted",
		},
		{
			name: "user_source/not_live", h: literal(tid("hd-s4-ghost-child"), []string{tid("hd-s4-ghost-user")}),
			src: src(tid("hd-s4-ghost-user")), wantDetail: "delivery source user " + tid("hd-s4-ghost-user") + " is not live",
		},
		{name: "lookup_error/agent_probe", authz: allFailing, h: h, src: src(f.projectOwnerID),
			wantDetail: "delivery source check failed (fail-closed): injected agent failure"},
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

// TestHubDelivery_DecideRows runs hub_delivery requests through Decide on
// the golden fixture's opted-in user secret. Rows where an earlier
// relationship stage catches the fault assert that stage (Step 9 replaces
// the reason, and step 10 never runs); the rest assert the step-10 deny.
func TestHubDelivery_DecideRows(t *testing.T) {
	type row struct {
		name  string
		setup func(t *testing.T, f *goldenFixture) string // returns the target agent
		// Exactly one of wantStage (a relationship reject kind) or
		// wantCause (a step-10 deny) is set.
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
	rows := []row{
		{
			name:       "bounded_without_permission",
			setup:      withEdge("bounded-no-perm", boundedCeiling("agent.read")),
			wantCause:  DenyCauseCeilingEffectExceeded,
			wantReason: "delegation ceiling: secret.deliver is outside the frozen effect ceiling",
		},
		{
			name: "missing_edge",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := tid("hd-decide-missing")
				hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID})
				return c
			},
			wantStage:  RelationshipRejectExecutionProject,
			wantDetail: "execution agent has no authoritative source user",
		},
		{
			name: "delegator_user_suspended",
			setup: func(t *testing.T, f *goldenFixture) string {
				c := withEdge("suspended", nil)(t, f)
				setUserStatus(t, f.store, f.projectOwnerID, store.UserStatusSuspended)
				return c
			},
			wantStage:  RelationshipRejectExecutionProject,
			wantDetail: "execution source user is not active",
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
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newGoldenFixture(t)
			withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
			c := r.setup(t, f)
			h := hdIdentity(t, f.authz, c)
			d := decidePerm(f.authz, h, Resource{Type: "secret", ID: f.secretID}, ActionDeliver, "secret.deliver", true)
			assert.False(t, d.Allowed, "reason %q", d.Reason)
			if r.wantStage != "" {
				assert.Equal(t, "relationship grant restricted by "+r.wantStage, d.Reason)
				assert.Equal(t, DenyCause(""), d.DenyCause)
				res := relationshipResult(t, d, RelationshipRuleProgeny)
				assert.Equal(t, r.wantStage, res.RejectedBy)
				assert.Equal(t, r.wantDetail, res.Detail)
				assertNoDeliveryCeilingStep(t, d)
				return
			}
			assert.Equal(t, r.wantReason, d.Reason)
			assert.Equal(t, r.wantCause, d.DenyCause)
			assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
			assert.True(t, relationshipResult(t, d, RelationshipRuleProgeny).Accepted, "the item grant passed; step 10 denied")
			assert.True(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_denied"), "trace: %+v", d.ExplainTrace)
		})
	}
}

// TestHubDelivery_Stage4AgentSource runs the stage-4 source rules through
// Decide on a delegation chain written by production paths: the creator's
// session creates A, and A's own token creates C (C.Ancestry = [creator,
// A]; C's edge is bounded and carries the deliver permissions).
//
// The item rows are stored at the store layer. The user-owned row has the
// shape the user secret handler writes. The agent-authored row (CreatedBy
// = A's bare ID, user scope, opted in) is a shape no production handler
// writes; it is the RQ-1b contract path, which stays refused until the P-2
// ruling (ptone/scion#2228).
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
