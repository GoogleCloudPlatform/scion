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

// Tests for current project access of a federated user (ptone/scion#3427):
// a FederatedUserIdentity (<issuer>:<sub>) has access to a project only
// through active hub-recorded role bindings keyed to user:<issuer>:<sub>,
// directly or through a group. Token claims, the issuer's configured role
// and scopes, ancestry and ownership never count.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertFedAdmit(t *testing.T, out fedOutcome, wantSource ProjectAccessSource) {
	t.Helper()
	require.True(t, out.decision.Allowed, "decision: %s", out.decision.Reason)
	assert.Empty(t, out.stageKind)
	require.NoError(t, out.admErr)
	assert.True(t, out.admission.Admitted)
	assert.Equal(t, wantSource, out.admission.Source)
}

func assertFedDeny(t *testing.T, out fedOutcome) {
	t.Helper()
	require.False(t, out.decision.Allowed, "decision must deny: %s", out.decision.Reason)
	assert.Equal(t, RelationshipRejectProjectAccess, out.stageKind)
	assert.False(t, out.decision.IsIndeterminate(), "a policy deny, not a fault: %s", out.decision.Reason)
	assert.Equal(t, "relationship grant restricted by "+RelationshipRejectProjectAccess, out.decision.Reason)
	r := relationshipResult(t, out.decision, RelationshipRuleOwner)
	assert.False(t, r.Accepted)
	assert.Equal(t, RelationshipRejectProjectAccess, r.RejectedBy)
	assert.False(t, out.admission.Admitted)
	if out.admErr != nil {
		assert.False(t, isProjectAccessLookupFault(out.admErr), "unexpected fault: %v", out.admErr)
	}
}

// TestRelationshipProjectAccess_Federated covers the rows of the
// ptone/scion#3427 rule for a federated owner of a project agent.
func TestRelationshipProjectAccess_Federated(t *testing.T) {
	ctx := context.Background()

	t.Run("R1_DirectBindingAdmits", func(t *testing.T) {
		f := newFedFixture(t, "r1")
		fed := fedIdentity("r1")
		f.grantFed(t, fed.ID(), f.projectID, nil)
		agent := f.agent(t, "fedr1", fed.ID())

		out := f.evaluate(t, fed, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		assertFedAdmit(t, out, ProjectAccessSourceMembership)
		assert.Equal(t, string(RelationshipRuleOwner), out.decision.MatchedGrant)

		out = f.evaluate(t, fed, agentResource(agent), "agent.update", RelationshipRuleOwner)
		assertFedAdmit(t, out, ProjectAccessSourceMembership)
	})

	t.Run("R2_GroupBindingAdmits", func(t *testing.T) {
		f := newFedFixture(t, "r2")
		fed := fedIdentity("r2")
		groupID := api.NewUUID()
		require.NoError(t, f.store.CreateGroup(ctx, &store.Group{ID: groupID, Slug: "fed-r2-group", Name: "G"}))
		_, err := f.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: f.accessOnlyRoleID(t), PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: groupID,
			ScopeType: store.RoleScopeProject, ScopeID: f.projectID, CreatedBy: "test",
		})
		require.NoError(t, err)
		f.fed.set(func(s *federatedBindingStore) { s.groups[fed.ID()] = []string{groupID} })
		agent := f.agent(t, "fedr2", fed.ID())

		out := f.evaluate(t, fed, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		assertFedAdmit(t, out, ProjectAccessSourceGroup)
	})

	t.Run("R3_SystemAuthorityAdmits", func(t *testing.T) {
		f := newFedFixture(t, "r3")
		fed := fedIdentity("r3")
		f.grantFedSuperAdmin(t, fed.ID())
		agent := f.agent(t, "fedr3", fed.ID())

		out := f.evaluate(t, fed, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		assertFedAdmit(t, out, ProjectAccessSourceSystemRole)
	})

	// The system-authority arm must survive the project's access
	// constraints, evaluated fail-closed.
	t.Run("R3_SystemAuthorityConstraints", func(t *testing.T) {
		pc := func(fed Identity) PrincipalContext { return principalContextForIdentity(fed) }
		strp := func(s string) *string { return &s }

		t.Run("ProjectConstraintReducesDenied", func(t *testing.T) {
			f := newFedFixture(t, "r3c1")
			fed := fedIdentity("r3c1")
			f.grantFedSuperAdmin(t, fed.ID())
			agent := f.agent(t, "fedr3c1", fed.ID())
			f.fed.set(func(s *federatedBindingStore) {
				s.extraConstraints = append(s.extraConstraints, &store.AccessConstraint{
					ID: api.NewUUID(), Name: "read-only", SubjectKind: string(SubjectKindPrincipal),
					SubjectPrincipalType: strp("user"), SubjectPrincipalID: strp(fed.ID()),
					ScopeType: store.RoleScopeProject, ScopeID: f.projectID, MaximumPermissions: []string{"agent.read"},
				})
			})
			adm, err := f.srv.authzService.ProjectTargetAdmission(ctx, pc(fed), f.projectID, "agent.attach", agentResource(agent), nil)
			require.NoError(t, err)
			assert.False(t, adm.Admitted, "a project constraint the permission does not survive denies")
			adm, err = f.srv.authzService.ProjectTargetAdmission(ctx, pc(fed), f.projectID, "agent.read", agentResource(agent), nil)
			require.NoError(t, err)
			assert.True(t, adm.Admitted, "a permission inside the constraint still admits")
		})

		unreadable := func(scopeType, scopeID string) *store.AccessConstraint {
			return &store.AccessConstraint{
				ID: api.NewUUID(), Name: "unreadable", SubjectKind: "unrecognized_kind",
				ScopeType: scopeType, ScopeID: scopeID, MaximumPermissions: []string{"agent.read"},
			}
		}

		t.Run("UnreadableConstraintDenied", func(t *testing.T) {
			f := newFedFixture(t, "r3c2")
			fed := fedIdentity("r3c2")
			f.grantFedSuperAdmin(t, fed.ID())
			agent := f.agent(t, "fedr3c2", fed.ID())
			adm, err := f.srv.authzService.ProjectTargetAdmission(ctx, pc(fed), f.projectID, "agent.attach", agentResource(agent), nil)
			require.NoError(t, err)
			require.True(t, adm.Admitted, "precondition: admitted before the constraint row exists")

			for _, c := range []*store.AccessConstraint{unreadable(store.RoleScopeProject, f.projectID), unreadable(store.RoleScopeSystem, ""), unreadable("unrecognized_scope", "")} {
				f.fed.set(func(s *federatedBindingStore) { s.extraConstraints = []*store.AccessConstraint{c} })
				memo := &ProjectAdmissionCache{}
				adm, err = f.srv.authzService.ProjectTargetAdmission(ctx, pc(fed), f.projectID, "agent.attach", agentResource(agent), memo)
				require.Error(t, err, "scope %q", c.ScopeType)
				assert.True(t, isProjectAccessLookupFault(err))
				assert.False(t, adm.Admitted)
				_, cached := memo.get(projectAdmissionCacheKey{principalKind: PrincipalKindFederatedUser, principalID: fed.ID(), projectID: f.projectID, permissionID: "agent.attach", class: ProjectTargetClass{ResourceType: "agent"}})
				assert.False(t, cached, "a fault is never memoized")
			}
		})

		t.Run("UnreadableConstraintOnOtherProjectIgnored", func(t *testing.T) {
			f := newFedFixture(t, "r3c3")
			fed := fedIdentity("r3c3")
			f.grantFedSuperAdmin(t, fed.ID())
			agent := f.agent(t, "fedr3c3", fed.ID())
			f.fed.set(func(s *federatedBindingStore) {
				s.extraConstraints = []*store.AccessConstraint{unreadable(store.RoleScopeProject, tid("fed-r3c3-other"))}
			})
			adm, err := f.srv.authzService.ProjectTargetAdmission(ctx, pc(fed), f.projectID, "agent.attach", agentResource(agent), nil)
			require.NoError(t, err)
			assert.True(t, adm.Admitted)
		})

		t.Run("ConstraintLoadFaultDenied", func(t *testing.T) {
			f := newFedFixture(t, "r3c4")
			fed := fedIdentity("r3c4")
			f.grantFedSuperAdmin(t, fed.ID())
			agent := f.agent(t, "fedr3c4", fed.ID())
			f.fed.set(func(s *federatedBindingStore) { s.failConstraints = true })
			adm, err := f.srv.authzService.ProjectTargetAdmission(ctx, pc(fed), f.projectID, "agent.attach", agentResource(agent), nil)
			require.Error(t, err)
			assert.True(t, isProjectAccessLookupFault(err))
			assert.False(t, adm.Admitted)
		})

		// The strict check applies to federated users only; a local
		// user's system authority is evaluated as before.
		t.Run("LocalUserUnchanged", func(t *testing.T) {
			f := newFedFixture(t, "r3c5")
			adminID := tid("fed-r3c5-admin")
			createTestUserWithRole(t, f.store, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
			agent := uatpAgent(t, f.store, f.projectID, adminID, "fedr3c5", adminID)
			f.fed.set(func(s *federatedBindingStore) {
				s.extraConstraints = []*store.AccessConstraint{unreadable(store.RoleScopeProject, f.projectID)}
			})
			adm, err := f.srv.authzService.ProjectTargetAdmission(ctx, activeUserPrincipal(adminID), f.projectID, "agent.attach", agentResource(agent), nil)
			require.NoError(t, err)
			assert.True(t, adm.Admitted)
		})

		// stageKind drives the owner relationship's project-access stage
		// for the federated owner of a project agent, with a fresh memo.
		stageKind := func(f *fedFixture, fed Identity, agent *store.Agent) string {
			kind, _ := f.srv.authzService.relationshipProjectAccessStage(ctx, pc(fed), agentResource(agent), "agent.attach", RelationshipRuleOwner, &ProjectAdmissionCache{})
			return kind
		}

		t.Run("ProjectScopeWithoutIDDenied", func(t *testing.T) {
			f := newFedFixture(t, "r3c6")
			fed := fedIdentity("r3c6")
			f.grantFedSuperAdmin(t, fed.ID())
			otherProject := tid("fed-r3c6-other-project")
			createRS1Project(t, f.store, otherProject, tid("fed-r3c6-other-owner"))
			here := f.agent(t, "fedr3c6", fed.ID())
			there := storeFedAgent(t, f.store, f.owners, otherProject, f.ownerID, "fedr3c6other", fed.ID(), "")
			require.Empty(t, stageKind(f, fed, here), "precondition: admitted before the constraint row exists")
			require.Empty(t, stageKind(f, fed, there), "precondition: admitted before the constraint row exists")

			f.fed.set(func(s *federatedBindingStore) {
				s.extraConstraints = []*store.AccessConstraint{{
					ID: api.NewUUID(), Name: "project-scope-no-id", SubjectKind: string(SubjectKindAllPrincipals),
					ScopeType: store.RoleScopeProject, ScopeID: "", MaximumPermissions: []string{"agent.read"},
				}}
			})
			for _, agent := range []*store.Agent{here, there} {
				assert.Equal(t, RelationshipRejectProjectAccessError, stageKind(f, fed, agent), "project %s", agent.ProjectID)
				_, err := f.srv.authzService.ProjectTargetAdmission(ctx, pc(fed), agent.ProjectID, "agent.attach", agentResource(agent), nil)
				require.Error(t, err)
				assert.True(t, isProjectAccessLookupFault(err))
			}
		})

		t.Run("SubjectUnreadableOnOtherProjectAdmits", func(t *testing.T) {
			f := newFedFixture(t, "r3c7")
			fed := fedIdentity("r3c7")
			f.grantFedSuperAdmin(t, fed.ID())
			agent := f.agent(t, "fedr3c7", fed.ID())
			f.fed.set(func(s *federatedBindingStore) {
				s.extraConstraints = []*store.AccessConstraint{unreadable(store.RoleScopeProject, tid("fed-r3c7-project-q"))}
			})
			assert.Empty(t, stageKind(f, fed, agent))
			adm, err := f.srv.authzService.ProjectTargetAdmission(ctx, pc(fed), f.projectID, "agent.attach", agentResource(agent), nil)
			require.NoError(t, err)
			assert.True(t, adm.Admitted)
			assert.Equal(t, ProjectAccessSourceSystemRole, adm.Source)
		})

		t.Run("SubjectUnreadableOnThisProjectDenied", func(t *testing.T) {
			f := newFedFixture(t, "r3c8")
			fed := fedIdentity("r3c8")
			f.grantFedSuperAdmin(t, fed.ID())
			agent := f.agent(t, "fedr3c8", fed.ID())
			require.Empty(t, stageKind(f, fed, agent), "precondition: admitted before the constraint row exists")
			f.fed.set(func(s *federatedBindingStore) {
				s.extraConstraints = []*store.AccessConstraint{unreadable(store.RoleScopeProject, f.projectID)}
			})
			assert.Equal(t, RelationshipRejectProjectAccessError, stageKind(f, fed, agent))
			_, err := f.srv.authzService.ProjectTargetAdmission(ctx, pc(fed), f.projectID, "agent.attach", agentResource(agent), nil)
			require.Error(t, err)
			assert.True(t, isProjectAccessLookupFault(err))
		})
	})

	t.Run("R4_NoBindingDenied", func(t *testing.T) {
		f := newFedFixture(t, "r4")
		fed := fedIdentity("r4")
		agent := f.agent(t, "fedr4", fed.ID())

		out := f.evaluate(t, fed, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		assertFedDeny(t, out)
	})

	t.Run("R5_BindingRemovedDenied", func(t *testing.T) {
		f := newFedFixture(t, "r5")
		fed := fedIdentity("r5")
		rb := f.grantFed(t, fed.ID(), f.projectID, nil)
		agent := f.agent(t, "fedr5", fed.ID())
		res := agentResource(agent)

		before := f.evaluate(t, fed, res, "agent.attach", RelationshipRuleOwner)
		assertFedAdmit(t, before, ProjectAccessSourceMembership)

		f.fed.removeBinding(rb.ID)

		after := f.evaluate(t, fed, res, "agent.attach", RelationshipRuleOwner)
		assertFedDeny(t, after)
	})

	t.Run("R6_BindingExpiredDenied", func(t *testing.T) {
		f := newFedFixture(t, "r6")
		fed := fedIdentity("r6")
		expiresAt := time.Now().Add(1500 * time.Millisecond)
		f.grantFed(t, fed.ID(), f.projectID, func(rb *store.RoleBinding) { rb.ExpiresAt = &expiresAt })
		agent := f.agent(t, "fedr6", fed.ID())
		res := agentResource(agent)

		before := f.evaluate(t, fed, res, "agent.attach", RelationshipRuleOwner)
		require.True(t, time.Now().Before(expiresAt), "precondition ran after expiry; raise the window")
		assertFedAdmit(t, before, ProjectAccessSourceMembership)

		time.Sleep(time.Until(expiresAt) + 100*time.Millisecond)

		after := f.evaluate(t, fed, res, "agent.attach", RelationshipRuleOwner)
		assertFedDeny(t, after)
	})

	t.Run("R6_BindingNotYetActiveDenied", func(t *testing.T) {
		f := newFedFixture(t, "r6b")
		fed := fedIdentity("r6b")
		notBefore := time.Now().Add(time.Hour)
		f.grantFed(t, fed.ID(), f.projectID, func(rb *store.RoleBinding) { rb.NotBefore = &notBefore })
		agent := f.agent(t, "fedr6b", fed.ID())

		out := f.evaluate(t, fed, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		assertFedDeny(t, out)
	})

	// Claims never count: an identity carrying the issuer's highest role,
	// every scope and the email of a project member is denied without a
	// binding, and admitted the same way as a plain identity with one.
	t.Run("R7_ClaimsOnlyDenied", func(t *testing.T) {
		f := newFedFixture(t, "r7")
		memberID := tid("fed-r7-member")
		uatpMember(t, f.store, f.projectID, memberID)
		rich := NewFederatedUserIdentity(fedTestIssuer, "r7", memberID+"@test.com", "Fed", "admin", allRegisteredAgentScopes())
		agent := f.agent(t, "fedr7", rich.ID())

		out := f.evaluate(t, rich, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		assertFedDeny(t, out)
	})

	t.Run("R7_ClaimsWithBindingUnchanged", func(t *testing.T) {
		f := newFedFixture(t, "r7b")
		memberID := tid("fed-r7b-member")
		uatpMember(t, f.store, f.projectID, memberID)
		plain := fedIdentity("r7b")
		rich := NewFederatedUserIdentity(fedTestIssuer, "r7b", memberID+"@test.com", "Fed", "admin", allRegisteredAgentScopes())
		f.grantFed(t, plain.ID(), f.projectID, nil)
		agent := f.agent(t, "fedr7b", plain.ID())

		plainOut := f.evaluate(t, plain, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		richOut := f.evaluate(t, rich, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		assertFedAdmit(t, plainOut, ProjectAccessSourceMembership)
		assertFedAdmit(t, richOut, ProjectAccessSourceMembership)
		assert.Equal(t, plainOut.admission, richOut.admission)
		assert.Equal(t, plainOut.decision.Allowed, richOut.decision.Allowed)
		assert.Equal(t, plainOut.decision.MatchedGrant, richOut.decision.MatchedGrant)
	})

	t.Run("R8_LookupFaultDenied", func(t *testing.T) {
		faults := []struct {
			name   string
			inject func(*federatedBindingStore)
		}{
			{"Bindings", func(s *federatedBindingStore) { s.failBindings = true }},
			{"Groups", func(s *federatedBindingStore) { s.failGroups = true }},
			{"User", func(s *federatedBindingStore) { s.failUser = true }},
		}
		for _, fc := range faults {
			fc := fc
			t.Run(fc.name, func(t *testing.T) {
				f := newFedFixture(t, "r8"+strings.ToLower(fc.name))
				fed := fedIdentity("r8" + strings.ToLower(fc.name))
				f.grantFed(t, fed.ID(), f.projectID, nil)
				agent := f.agent(t, "fedr8"+fc.name, fed.ID())
				res := agentResource(agent)
				principal := principalContextForIdentity(fed)

				before := f.evaluate(t, fed, res, "agent.attach", RelationshipRuleOwner)
				assertFedAdmit(t, before, ProjectAccessSourceMembership)

				f.fed.set(fc.inject)
				memo := &ProjectAdmissionCache{}

				after := f.evaluate(t, fed, res, "agent.attach", RelationshipRuleOwner)
				assert.False(t, after.decision.Allowed, "decision: %s", after.decision.Reason)
				assert.True(t, after.decision.IsIndeterminate(), "a fault is indeterminate: %s", after.decision.Reason)
				assert.Equal(t, RelationshipRejectProjectAccessError, after.stageKind)
				require.Error(t, after.admErr)
				assert.True(t, isProjectAccessLookupFault(after.admErr))

				adm, err := f.srv.authzService.ProjectTargetAdmission(ctx, principal, f.projectID, "agent.attach", res, memo)
				require.Error(t, err)
				assert.False(t, adm.Admitted)

				// Never memoized: clearing the fault and reusing the
				// same memo reads the store again and admits.
				f.fed.set(func(s *federatedBindingStore) { s.failBindings, s.failGroups, s.failUser = false, false, false })
				adm, err = f.srv.authzService.ProjectTargetAdmission(ctx, principal, f.projectID, "agent.attach", res, memo)
				require.NoError(t, err)
				assert.True(t, adm.Admitted)
			})
		}
	})

	// Account status is a deny-only gate: an existing users row that is
	// not active denies even with an active binding. A missing row is
	// not a grant (R4).
	t.Run("R9_SuspendedUserDenied", func(t *testing.T) {
		f := newFedFixture(t, "r9")
		fed := fedIdentity("r9")
		f.grantFed(t, fed.ID(), f.projectID, nil)
		agent := f.agent(t, "fedr9", fed.ID())
		res := agentResource(agent)

		f.fed.set(func(s *federatedBindingStore) {
			s.users[fed.ID()] = &store.User{ID: fed.ID(), Email: fed.Email(), Status: store.UserStatusActive}
		})
		before := f.evaluate(t, fed, res, "agent.attach", RelationshipRuleOwner)
		assertFedAdmit(t, before, ProjectAccessSourceMembership)

		f.fed.set(func(s *federatedBindingStore) { s.users[fed.ID()].Status = store.UserStatusSuspended })
		after := f.evaluate(t, fed, res, "agent.attach", RelationshipRuleOwner)
		assertFedDeny(t, after)
		require.Error(t, after.admErr)
		assert.ErrorIs(t, after.admErr, ErrProjectAccessDenied)

		// The system-authority arm is gated the same way.
		f.grantFedSuperAdmin(t, fed.ID())
		ok, err := f.srv.authzService.SystemAuthorityProof(ctx, principalContextForIdentity(fed), f.projectID, "agent.attach", ProjectTargetClass{ResourceType: "agent"})
		require.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("R10_BindingOnOtherProjectDenied", func(t *testing.T) {
		f := newFedFixture(t, "r10")
		fed := fedIdentity("r10")
		otherProject := tid("fed-r10-other-project")
		createRS1Project(t, f.store, otherProject, tid("fed-r10-other-owner"))
		f.grantFed(t, fed.ID(), otherProject, nil)
		agent := f.agent(t, "fedr10", fed.ID())

		out := f.evaluate(t, fed, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		assertFedDeny(t, out)
	})

	t.Run("R11_OtherFederatedKindsAndMintUnchanged", func(t *testing.T) {
		f := newFedFixture(t, "r11")
		agent := uatpAgent(t, f.store, f.projectID, f.ownerID, "fedr11", f.ownerID)
		res := agentResource(agent)
		authz := f.srv.authzService

		for _, ident := range []Identity{
			NewFederatedAgentIdentity(fedTestIssuer, "r11-agent", f.projectID, "worker", "user:x", nil, nil),
			NewFederatedServiceIdentity(fedTestIssuer, "r11-svc", "svc@idp.example", nil),
		} {
			p := principalContextForIdentity(ident)
			_, err := authz.ProjectTargetAdmission(ctx, p, f.projectID, "agent.read", res, nil)
			assert.ErrorIs(t, err, ErrUnsupportedPrincipalKind, "%s", p.Kind)
			_, _, err = authz.ProjectMembershipEvidence(ctx, p, f.projectID)
			assert.ErrorIs(t, err, ErrUnsupportedPrincipalKind, "%s", p.Kind)
			_, err = authz.SystemAuthorityProof(ctx, p, f.projectID, "agent.read", ProjectTargetClass{ResourceType: "agent"})
			assert.ErrorIs(t, err, ErrUnsupportedPrincipalKind, "%s", p.Kind)
			kind, _ := authz.relationshipProjectAccessStage(ctx, p, res, "agent.read", RelationshipRuleOwner, nil)
			assert.Empty(t, kind, "the stage does not apply to %s", p.Kind)
		}

		// A federated user with project access is not mint-eligible.
		fed := fedIdentity("r11")
		f.grantFed(t, fed.ID(), f.projectID, nil)
		f.grantFedSuperAdmin(t, fed.ID())
		p := principalContextForIdentity(fed)
		_, err := authz.MintTimeSystemGrant(ctx, p, "agent.read")
		assert.ErrorIs(t, err, ErrUnsupportedPrincipalKind)
		_, err = authz.CanMintSelector(ctx, p, TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID}, []string{"agent:read"})
		assert.ErrorIs(t, err, ErrUnsupportedPrincipalKind)
		_, err = authz.CanMintSelector(ctx, p, TokenBoundary{Kind: BoundaryKindHub}, []string{"agent:read"})
		assert.ErrorIs(t, err, ErrUnsupportedPrincipalKind)
	})

	t.Run("R12_MessageOwnerProjectMode", func(t *testing.T) {
		f := newFedFixture(t, "r12")
		fed := fedIdentity("r12")
		agent := storeFedAgent(t, f.store, f.owners, f.projectID, f.ownerID, "fedr12", fed.ID(), store.MessageModeProject)

		allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, fed, agent, false)
		assert.False(t, allowed, "owner without a binding: %s", reason)

		rb := f.grantFed(t, fed.ID(), f.projectID, nil)
		allowed, reason, _ = f.srv.authorizeAgentMessage(ctx, fed, agent, false)
		assert.True(t, allowed, "owner with a binding: %s", reason)
		assert.Equal(t, "agent.message permission granted", reason)

		f.fed.removeBinding(rb.ID)
		allowed, reason, _ = f.srv.authorizeAgentMessage(ctx, fed, agent, false)
		assert.False(t, allowed, "owner after removal: %s", reason)

		// The ancestry admission reports not admitted for a
		// federated sender.
		admitted, fault := f.srv.authzService.messageAncestorProjectAccess(ctx, fed, f.projectID, agentResource(agent))
		assert.False(t, admitted)
		assert.False(t, fault)
	})

	t.Run("R13_NonProjectTargetsUnchanged", func(t *testing.T) {
		f := newFedFixture(t, "r13")
		fed := fedIdentity("r13")
		p := principalContextForIdentity(fed)
		group := Resource{Type: "group", ID: tid("fed-r13-group"), OwnerID: fed.ID()}
		userTemplate := templateResource(&store.Template{ID: tid("fed-r13-tpl"), OwnerID: fed.ID(), Scope: store.TemplateScopeUser, ScopeID: fed.ID()})
		globalTemplate := templateResource(&store.Template{ID: tid("fed-r13-gtpl"), OwnerID: fed.ID(), Scope: store.TemplateScopeGlobal})

		for _, tc := range []struct {
			res  Resource
			perm string
		}{{group, "group.update"}, {userTemplate, "template.update"}, {globalTemplate, "template.update"}} {
			require.Empty(t, resourceProjectScope(tc.res), "%s must not be project-scoped", tc.res.ID)
			kind, _ := f.srv.authzService.relationshipProjectAccessStage(ctx, p, tc.res, tc.perm, RelationshipRuleOwner, nil)
			assert.Empty(t, kind, "the stage does not apply to %s", tc.res.ID)
		}
		d := decidePerm(f.srv.authzService, fed, group, ActionUpdate, "group.update", true)
		assert.True(t, d.Allowed, "hub-level owner grant unchanged: %s", d.Reason)
		assert.Equal(t, "owner", d.MatchedGrant)
	})

	// A federated user whose email claim names a local user with an
	// active binding on the project is not admitted: the binding belongs
	// to the local user's ID, not to user:<issuer>:<sub>.
	t.Run("R14_EmailOfBoundLocalUserDenied", func(t *testing.T) {
		f := newFedFixture(t, "r14")
		localID := tid("fed-r14-local")
		uatpMember(t, f.store, f.projectID, localID)
		local, err := f.store.GetUser(ctx, localID)
		require.NoError(t, err)
		fed := NewFederatedUserIdentity(fedTestIssuer, "r14", local.Email, local.DisplayName, "viewer", nil)
		agent := f.agent(t, "fedr14", fed.ID())

		out := f.evaluate(t, fed, agentResource(agent), "agent.attach", RelationshipRuleOwner)
		assertFedDeny(t, out)

		// The local user's own access is real.
		adm, err := f.srv.authzService.ProjectTargetAdmission(ctx, activeUserPrincipal(localID), f.projectID, "agent.attach", agentResource(agent), nil)
		require.NoError(t, err)
		assert.True(t, adm.Admitted)
	})

	// An absent project, a project with no qualifying binding and a
	// project the principal is bound to elsewhere all give the same deny.
	t.Run("R15_AbsentProjectIndistinguishable", func(t *testing.T) {
		f := newFedFixture(t, "r15a")
		fed := fedIdentity("r15a")
		existing := agentResource(&store.Agent{ID: tid("fed-r15a-agent"), ProjectID: f.projectID, OwnerID: fed.ID()})
		absent := agentResource(&store.Agent{ID: tid("fed-r15a-agent2"), ProjectID: tid("fed-r15a-absent-project"), OwnerID: fed.ID()})

		e := f.evaluate(t, fed, existing, "agent.attach", RelationshipRuleOwner)
		a := f.evaluate(t, fed, absent, "agent.attach", RelationshipRuleOwner)
		assertFedDeny(t, e)
		assertFedDeny(t, a)
		assert.Equal(t, e.decision.Reason, a.decision.Reason)
		assert.Equal(t, e.stageKind, a.stageKind)
		assert.Equal(t, e.admission, a.admission)
		assert.Equal(t, e.admErr, a.admErr)
	})
}

// TestRelationshipProjectAccess_FederatedStoreToday pins the persistent
// store's behaviour for a federated principal (ptone/scion#3427): a
// binding keyed to user:<issuer>:<sub> cannot be recorded, group
// resolution reports invalid input, and a federated owner's Decide denies
// as a resolution error. The change does not alter this.
func TestRelationshipProjectAccess_FederatedStoreToday(t *testing.T) {
	f := newRPAFixture(t, "fedstore")
	ctx := context.Background()
	fed := fedIdentity("store")
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: fed.ID(),
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, CreatedBy: "test",
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = f.store.GetEffectiveGroups(ctx, fed.ID())
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	err = f.store.CreateAgent(ctx, &store.Agent{
		ID: tid("fed-store-direct"), Slug: "fed-store-direct", Name: "Fed Store", ProjectID: f.projectID,
		OwnerID: fed.ID(), Phase: "stopped",
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput, "an agent owner must be a users row ID")

	owners := installAgentOwnerOverride(t, f.srv)
	agent := storeFedAgent(t, f.store, owners, f.projectID, f.ownerID, "fedstore", fed.ID(), "")
	d := decidePerm(f.srv.authzService, fed, agentResource(agent), ActionAttach, "agent.attach", true)
	assert.False(t, d.Allowed)
	assert.True(t, d.IsIndeterminate(), "reason: %s", d.Reason)
	assert.Equal(t, DenyCauseResolutionError, d.DenyCause)

	_, err = f.srv.authzService.ProjectTargetAdmission(ctx, principalContextForIdentity(fed), f.projectID, "agent.attach", agentResource(agent), nil)
	require.Error(t, err)
	assert.True(t, isProjectAccessLookupFault(err))

	// At a Decide endpoint (agent read), the federated owner's public
	// refusal is identical to an unrelated local non-member's.
	issuer := installFederatedUserIssuer(t, f.srv)
	outsiderID := tid("fed-store-outsider")
	f.hubUser(t, outsiderID)
	outsider, err := f.store.GetUser(ctx, outsiderID)
	require.NoError(t, err)
	path := "/api/v1/agents/" + agent.ID
	localRec := doRequestAsUser(t, f.srv, outsider, http.MethodGet, path, nil)
	ownerRec := doFederatedRequest(t, f.srv, issuer.token("store", fed.Email(), nil), http.MethodGet, path, nil)
	require.Equal(t, http.StatusForbidden, localRec.Code, "local outsider: %s", localRec.Body.String())
	assert.Equal(t, localRec.Code, ownerRec.Code)
	assert.Equal(t, localRec.Body.String(), ownerRec.Body.String())
}

// fedTokenIssuer signs user-type federation tokens for a test server.
type fedTokenIssuer struct {
	sign func(claims map[string]interface{}) string
	aud  string
}

func installFederatedUserIssuer(t *testing.T, srv *Server) *fedTokenIssuer {
	t.Helper()
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	audience := "fed-test-audience"
	auth := newTestAuthenticatorWithConfig(t, config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{{
			IssuerURL: fedTestIssuer, JWKSURL: jwksSrv.URL, ExpectedAudience: audience, IssuerType: "user",
		}},
	}, audience)
	prev := srv.federationAuth.Load()
	srv.federationAuth.Store(auth)
	t.Cleanup(func() { srv.federationAuth.Store(prev) })
	return &fedTokenIssuer{aud: audience, sign: func(claims map[string]interface{}) string {
		return signGenericToken(t, privKey, kid, claims)
	}}
}

func (i *fedTokenIssuer) token(sub, email string, extra map[string]interface{}) string {
	now := time.Now()
	claims := map[string]interface{}{
		"iss": fedTestIssuer, "sub": sub, "aud": i.aud,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(),
		"email": email, "name": "Fed " + sub,
	}
	for k, v := range extra {
		claims[k] = v
	}
	return i.sign(claims)
}

func doFederatedRequest(t *testing.T, srv *Server, token, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = strings.NewReader(string(b))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(FederationTokenHeader, token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestRelationshipProjectAccess_FederatedRefusalIdentical pins that a
// federated owner with no binding gets exactly the refusal an unrelated
// non-member gets, at a Decide endpoint (agent read) and at the message
// handler, and that token claims (a project_id, a groups claim and
// the email of a project member) do not change it.
func TestRelationshipProjectAccess_FederatedRefusalIdentical(t *testing.T) {
	f := newFedFixture(t, "r15")
	ctx := context.Background()
	issuer := installFederatedUserIssuer(t, f.srv)
	memberID := tid("fed-r15-member")
	outsiderID := tid("fed-r15-outsider")
	uatpMember(t, f.store, f.projectID, memberID)
	f.hubUser(t, outsiderID)
	outsider, err := f.store.GetUser(ctx, outsiderID)
	require.NoError(t, err)

	ownerID := fedPrincipalID("r15-owner")
	readAgent := f.agent(t, "fedr15read", ownerID)
	msgAgent := storeFedAgent(t, f.store, f.owners, f.projectID, f.ownerID, "fedr15msg", ownerID, store.MessageModeProject)

	ownerTok := issuer.token("r15-owner", "owner@idp.example", nil)
	claimsTok := issuer.token("r15-owner", memberID+"@test.com", map[string]interface{}{
		"project_id": f.projectID, "groups": []string{"admins"}, "root_user": "user:" + memberID,
	})
	strangerTok := issuer.token("r15-stranger", "stranger@idp.example", nil)

	readPath := "/api/v1/agents/" + readAgent.ID
	msgPath := "/api/v1/agents/" + msgAgent.ID + "/message"
	msgBody := map[string]interface{}{"message": "hello", "interrupt": false}

	// Precondition: with a binding the federated owner is authorized at
	// both endpoints, so the refusals below come from project access.
	rb := f.grantFed(t, ownerID, f.projectID, nil)
	readRec := doFederatedRequest(t, f.srv, ownerTok, http.MethodGet, readPath, nil)
	require.Equal(t, http.StatusOK, readRec.Code, "precondition: owner with a binding may read: %s", readRec.Body.String())
	msgRec := doFederatedRequest(t, f.srv, ownerTok, http.MethodPost, msgPath, msgBody)
	require.NotEqual(t, http.StatusForbidden, msgRec.Code, "precondition: owner with a binding may message: %s", msgRec.Body.String())
	require.NotEqual(t, http.StatusUnauthorized, msgRec.Code, "precondition: %s", msgRec.Body.String())
	f.fed.removeBinding(rb.ID)

	for _, ep := range []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"agent_read", http.MethodGet, readPath, nil},
		{"message", http.MethodPost, msgPath, msgBody},
	} {
		ep := ep
		t.Run(ep.name, func(t *testing.T) {
			localRec := doRequestAsUser(t, f.srv, outsider, ep.method, ep.path, ep.body)
			require.Equal(t, http.StatusForbidden, localRec.Code, "local outsider: %s", localRec.Body.String())
			for name, tok := range map[string]string{"owner": ownerTok, "owner_with_claims": claimsTok, "federated_stranger": strangerTok} {
				rec := doFederatedRequest(t, f.srv, tok, ep.method, ep.path, ep.body)
				assert.Equal(t, localRec.Code, rec.Code, "%s: %s", name, rec.Body.String())
				assert.Equal(t, localRec.Body.String(), rec.Body.String(), "%s refusal must be identical to an unrelated user's", name)
				assert.NotContains(t, rec.Body.String(), RelationshipRejectProjectAccess)
			}
		})
	}
}
