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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fedTestIssuer = "https://idp.example"

func fedPrincipalID(sub string) string { return fedTestIssuer + ":" + sub }

func isFedTestPrincipal(id string) bool { return strings.HasPrefix(id, fedTestIssuer+":") }

// federatedBindingStore is a test store wrapper that records role bindings,
// group membership and users rows keyed to federated principal IDs
// (<issuer>:<sub>). The persistent store cannot hold these records today:
// CreateRoleBinding requires a principal ID that names an existing users
// row, and GetEffectiveGroups rejects an ID that is not a UUID (see
// ptone/scion#3427). The wrapper serves them so the authorization rule can
// be exercised end to end. Every other read passes through to the wrapped
// store. It can also inject a fault for each federated read and serve extra
// access-constraint rows.
type federatedBindingStore struct {
	store.Store
	mu               sync.Mutex
	bindings         []*store.RoleBinding
	groups           map[string][]string
	users            map[string]*store.User
	extraConstraints []*store.AccessConstraint
	failUser         bool
	failBindings     bool
	failGroups       bool
	failConstraints  bool
}

func (s *federatedBindingStore) addBinding(rb *store.RoleBinding) *store.RoleBinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	rb.ID = api.NewUUID()
	s.bindings = append(s.bindings, rb)
	return rb
}

func (s *federatedBindingStore) removeBinding(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.bindings[:0]
	for _, b := range s.bindings {
		if b.ID != id {
			kept = append(kept, b)
		}
	}
	s.bindings = kept
}

func (s *federatedBindingStore) set(fn func(*federatedBindingStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func (s *federatedBindingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if !isFedTestPrincipal(id) {
		return s.Store.GetUser(ctx, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failUser {
		return nil, errors.New("injected: user lookup failure")
	}
	if u, ok := s.users[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (s *federatedBindingStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if !isFedTestPrincipal(userID) {
		return s.Store.GetEffectiveGroups(ctx, userID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGroups {
		return nil, errors.New("injected: group resolution failure")
	}
	return append([]string(nil), s.groups[userID]...), nil
}

func (s *federatedBindingStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	s.mu.Lock()
	fail := s.failBindings && len(principals) > 0 && isFedTestPrincipal(principals[0].ID)
	s.mu.Unlock()
	if fail {
		return nil, errors.New("injected: binding lookup failure")
	}
	var real []store.PrincipalRef
	wanted := map[string]bool{}
	for _, p := range principals {
		wanted[p.Type+":"+p.ID] = true
		if !isFedTestPrincipal(p.ID) {
			real = append(real, p)
		}
	}
	var out []*store.RoleBinding
	if len(real) > 0 {
		got, err := s.Store.ListRoleBindingsForPrincipals(ctx, real, scopeTypes, scopeIDs)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bindings {
		if !wanted[b.PrincipalType+":"+b.PrincipalID] {
			continue
		}
		if len(scopeTypes) > 0 && !containsString(scopeTypes, b.ScopeType) {
			continue
		}
		if len(scopeIDs) > 0 && !containsString(scopeIDs, b.ScopeID) && b.ScopeType != store.RoleScopeSystem {
			continue
		}
		cp := *b
		out = append(out, &cp)
	}
	return out, nil
}

func (s *federatedBindingStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	s.mu.Lock()
	fail := s.failConstraints
	extra := append([]*store.AccessConstraint(nil), s.extraConstraints...)
	s.mu.Unlock()
	if fail {
		return nil, errors.New("injected: access constraint load failure")
	}
	page, err := s.Store.ListAccessConstraints(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	if offset == 0 {
		page = append(page, extra...)
	}
	return page, nil
}

// agentOwnerOverrideStore is a test store wrapper that reports a federated
// principal as the owner of a stored agent. The persistent store keeps an
// agent's owner as a users row ID, so an agent owned by <issuer>:<sub>
// cannot be stored today (see ptone/scion#3427). The agent is stored with a
// local owner and GetAgent reports the federated owner instead.
type agentOwnerOverrideStore struct {
	store.Store
	mu     *sync.Mutex
	owners map[string]string // agent ID -> owner principal ID
}

func (s *agentOwnerOverrideStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := s.Store.GetAgent(ctx, id)
	if err != nil || a == nil {
		return a, err
	}
	s.mu.Lock()
	owner, ok := s.owners[id]
	s.mu.Unlock()
	if ok {
		cp := *a
		cp.OwnerID = owner
		return &cp, nil
	}
	return a, nil
}

// installAgentOwnerOverride wraps the server store and the authorization
// store's base with one agentOwnerOverrideStore map, so handlers and
// authorization agree on the agent's owner. base is the store the
// authorization service should read through underneath the override.
func installAgentOwnerOverride(t *testing.T, srv *Server) *agentOwnerOverrideStore {
	t.Helper()
	mu := &sync.Mutex{}
	owners := map[string]string{}
	origSrv := srv.store
	srv.store = &agentOwnerOverrideStore{Store: origSrv, mu: mu, owners: owners}
	t.Cleanup(func() { srv.store = origSrv })
	origAuthz := srv.authzService.store
	override := &agentOwnerOverrideStore{Store: origAuthz, mu: mu, owners: owners}
	srv.authzService.store = override
	t.Cleanup(func() { srv.authzService.store = origAuthz })
	return override
}

// storeFedAgent stores an agent in projectID with a local owner and records
// ownerID (a federated principal) as its reported owner. It returns the
// agent as the handlers and authorization see it.
func storeFedAgent(t *testing.T, s store.Store, o *agentOwnerOverrideStore, projectID, localOwnerID, suffix, ownerID, mode string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID: tid("fed-agent-" + suffix), Slug: "fed-agent-" + suffix, Name: "Fed Agent " + suffix,
		ProjectID: projectID, OwnerID: localOwnerID, Phase: "stopped", MessageMode: mode,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	o.mu.Lock()
	o.owners[agent.ID] = ownerID
	o.mu.Unlock()
	cp := *agent
	cp.OwnerID = ownerID
	return &cp
}

// fedFixture is an rpaFixture whose authorization service reads through a
// federatedBindingStore, and whose stored agents can report a federated
// owner.
type fedFixture struct {
	*rpaFixture
	fed    *federatedBindingStore
	owners *agentOwnerOverrideStore
}

func newFedFixture(t *testing.T, name string) *fedFixture {
	t.Helper()
	f := newRPAFixture(t, "fed"+name)
	owners := installAgentOwnerOverride(t, f.srv)
	fs := &federatedBindingStore{Store: owners, groups: map[string][]string{}, users: map[string]*store.User{}}
	f.srv.authzService.store = fs
	return &fedFixture{rpaFixture: f, fed: fs, owners: owners}
}

// agent stores a project agent reported as owned by ownerID.
func (f *fedFixture) agent(t *testing.T, suffix, ownerID string) *store.Agent {
	t.Helper()
	return storeFedAgent(t, f.store, f.owners, f.projectID, f.ownerID, suffix, ownerID, "")
}

func fedIdentity(sub string) *FederatedUserIdentity {
	return NewFederatedUserIdentity(fedTestIssuer, sub, sub+"@idp.example", "Fed "+sub, "viewer", nil)
}

// accessOnlyRoleID returns the ID of a project role with no permissions, so
// a binding to it is project access without granting anything itself.
func (f *fedFixture) accessOnlyRoleID(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	name := "relchar-access-only"
	rd, err := f.store.GetRoleDefinitionByName(ctx, name, store.RoleScopeProject)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		rd, err = f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
			ID: api.NewUUID(), Name: name, Description: "project access, no permissions",
			ScopeType: store.RoleScopeProject, Permissions: []string{},
		})
		require.NoError(t, err)
	}
	return rd.ID
}

// grantFed records an access-only project binding keyed to
// user:<issuer>:<sub> on projectID.
func (f *fedFixture) grantFed(t *testing.T, principalID, projectID string, mutate func(*store.RoleBinding)) *store.RoleBinding {
	t.Helper()
	rb := &store.RoleBinding{
		RoleDefinitionID: f.accessOnlyRoleID(t), PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: principalID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	}
	if mutate != nil {
		mutate(rb)
	}
	return f.fed.addBinding(rb)
}

// grantFedSuperAdmin records a system-scope super-admin binding keyed to
// user:<issuer>:<sub>.
func (f *fedFixture) grantFedSuperAdmin(t *testing.T, principalID string) *store.RoleBinding {
	t.Helper()
	rd, err := f.store.GetRoleDefinitionByName(context.Background(), store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	return f.fed.addBinding(&store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: principalID,
		ScopeType: store.RoleScopeSystem, CreatedBy: store.SystemReconcileCreatedBy,
	})
}

// fedOutcome is what one federated evaluation observed.
type fedOutcome struct {
	decision  Decision
	stageKind string
	admission ProjectAdmissionResult
	admErr    error
}

// evaluate runs Decide (with Explain), the stage directly for rule, and
// ProjectTargetAdmission, each with a fresh memo.
func (f *fedFixture) evaluate(t *testing.T, ident Identity, res Resource, perm string, rule RelationshipRuleID) fedOutcome {
	t.Helper()
	ctx := context.Background()
	authz := f.srv.authzService
	principal := principalContextForIdentity(ident)
	require.Equal(t, PrincipalKindFederatedUser, principal.Kind)
	action, ok := registryActionFor(perm)
	require.True(t, ok)

	var out fedOutcome
	out.decision = decidePerm(authz, ident, res, action, perm, true)
	out.stageKind, _ = authz.relationshipProjectAccessStage(ctx, principal, res, perm, rule, &ProjectAdmissionCache{})
	out.admission, out.admErr = authz.ProjectTargetAdmission(ctx, principal, resourceProjectScope(res), perm, res, &ProjectAdmissionCache{})
	return out
}

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

		// A federated user with project access is still not mint-eligible.
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

		// The ancestry admission still reports not admitted for a
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
