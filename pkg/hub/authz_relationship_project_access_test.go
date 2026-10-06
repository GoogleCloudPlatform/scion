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

// Tests for the relationship project-access stage (stage 2c,
// relationshipProjectAccessStage; ptone/scion#2141): a local user's owner or
// ancestor relationship on a project-scoped target requires active project
// access at use time.
//
// Every row runs for an interactive session user (*AuthenticatedUser) and a
// UAT holder (*ScopedUserIdentity). For the UAT holder, each row also runs
// the Decide step-1 bearer gate admission and the stage directly, each with
// its own fresh memo, counts the store reads each check made, and asserts
// that the two checks agree.

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rpaStore wraps the server store, counts GetUser calls per user ID, and can
// inject a GetUser fault for one user ID. GetUser is the first store read of
// ProjectMembershipEvidence (requireActiveUser), so a fault there is a real
// store fault inside the admission lookup.
type rpaStore struct {
	store.Store
	mu             sync.Mutex
	getUserCalls   map[string]int
	failGetUserFor string
}

func (s *rpaStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	s.mu.Lock()
	s.getUserCalls[id]++
	fail := s.failGetUserFor != "" && s.failGetUserFor == id
	s.mu.Unlock()
	if fail {
		return nil, errors.New("injected: user lookup failure")
	}
	return s.Store.GetUser(ctx, id)
}

func (s *rpaStore) calls(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getUserCalls[id]
}

func (s *rpaStore) setFault(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failGetUserFor = id
}

type rpaFixture struct {
	srv       *Server
	store     store.Store // the real store, for fixture writes
	counting  *rpaStore   // the store the authz service reads through
	projectID string
	ownerID   string // the project owner (not a principal under test)
}

func newRPAFixture(t *testing.T, name string) *rpaFixture {
	t.Helper()
	srv, s := testServer(t)
	f := &rpaFixture{
		srv:       srv,
		store:     s,
		projectID: tid("rpa-" + name + "-project"),
		ownerID:   tid("rpa-" + name + "-powner"),
	}
	createRS1Project(t, s, f.projectID, f.ownerID)
	f.counting = &rpaStore{Store: s, getUserCalls: map[string]int{}}
	orig := srv.authzService.store
	srv.authzService.store = f.counting
	t.Cleanup(func() { srv.authzService.store = orig })
	return f
}

// hubUser creates an active user with hub membership and no project binding.
func (f *rpaFixture) hubUser(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@test.com", DisplayName: "User", Role: "member", Status: store.UserStatusActive,
	}))
	ensureHubMembership(ctx, f.store, id)
}

// expiringMember creates an active user whose only project binding is a
// project-member binding that expires at expiresAt.
func (f *rpaFixture) expiringMember(t *testing.T, id string, expiresAt time.Time) {
	t.Helper()
	ctx := context.Background()
	f.hubUser(t, id)
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: id,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, ExpiresAt: &expiresAt, CreatedBy: "test",
	})
	require.NoError(t, err)
}

// rpaPrincipal names the two principal classes every row runs for.
type rpaPrincipal string

const (
	rpaInteractive rpaPrincipal = "interactive"
	rpaUAT         rpaPrincipal = "uat"
)

var rpaPrincipals = []rpaPrincipal{rpaInteractive, rpaUAT}

// rpaUATScopes is the ceiling of the UAT used by every row: it covers the
// permissions the rows evaluate, so only live authority decides.
var rpaUATScopes = []string{"agent:attach", "agent:read"}

func rpaIdentity(kind rpaPrincipal, userID, projectID string) Identity {
	switch kind {
	case rpaUAT:
		return NewScopedUserIdentity(NewAuthenticatedUser(userID, userID+"@test.com", "User", "member", "api"), projectID, rpaUATScopes)
	default:
		return NewAuthenticatedUser(userID, userID+"@test.com", "User", "member", "web")
	}
}

// rpaOutcome is what one row evaluation observed.
type rpaOutcome struct {
	decision Decision
	// stageKind is the direct stage result for rule ("" = passes).
	stageKind string
	// gateAdmitted is the Decide step-1 bearer gate result (UAT only).
	gateAdmitted bool
	gateDecision *Decision
}

// evaluate runs Decide (with Explain) for the principal and, for rule, the
// stage directly. For a UAT it also runs the Decide step-1 bearer gate
// admission and asserts that the gate and the stage each read the store
// (fresh memos) and agree.
func (f *rpaFixture) evaluate(t *testing.T, kind rpaPrincipal, userID string, res Resource, perm string, rule RelationshipRuleID) rpaOutcome {
	t.Helper()
	ctx := context.Background()
	authz := f.srv.authzService
	ident := rpaIdentity(kind, userID, f.projectID)
	principal := principalContextForIdentity(ident)
	action, ok := registryActionFor(perm)
	require.True(t, ok)

	var out rpaOutcome
	out.decision = decidePerm(authz, ident, res, action, perm, true)

	before := f.counting.calls(userID)
	out.stageKind, _ = authz.relationshipProjectAccessStage(ctx, principal, res, perm, rule, &ProjectAdmissionCache{})
	stageReads := f.counting.calls(userID) - before

	if kind == rpaUAT {
		in, gated := bearerGateInputsFor(principal, credentialContextForIdentity(ident))
		require.True(t, gated, "UAT request must reach the bearer gate")
		before = f.counting.calls(userID)
		var trace bearerGateTrace
		out.gateDecision = authz.evaluateBearerGate(ctx, principal, in, res, TargetScopeEvidence{}, action, perm, &ProjectAdmissionCache{}, &trace)
		gateReads := f.counting.calls(userID) - before
		out.gateAdmitted = out.gateDecision == nil

		// Both checks ran (each made its own store read) and agree.
		assert.Positive(t, gateReads, "the step-1 admission must run")
		assert.Positive(t, stageReads, "the relationship stage must run")
		assert.Equal(t, out.gateAdmitted, out.stageKind == "",
			"step-1 admission (%v) and relationship stage (%q) must agree", out.gateDecision, out.stageKind)
	}
	return out
}

// assertRelationshipAdmit asserts Decide admitted through rule and the stage
// passed.
func assertRelationshipAdmit(t *testing.T, out rpaOutcome, rule RelationshipRuleID) {
	t.Helper()
	require.True(t, out.decision.Allowed, "decision: %s", out.decision.Reason)
	assert.Equal(t, string(rule), out.decision.MatchedGrant)
	assert.Empty(t, out.stageKind)
	r := relationshipResult(t, out.decision, rule)
	assert.True(t, r.Accepted, "%s candidate: %+v", rule, r)
}

// assertProjectAccessDeny asserts the row denies for the principal: for an
// interactive user, at the relationship stage with wantKind; for a UAT, at
// the step-1 bearer gate, with the stage agreeing on wantKind.
func assertProjectAccessDeny(t *testing.T, kind rpaPrincipal, out rpaOutcome, rule RelationshipRuleID, wantKind string) {
	t.Helper()
	require.False(t, out.decision.Allowed, "decision must deny: %s", out.decision.Reason)
	assert.Equal(t, wantKind, out.stageKind)
	wantFault := wantKind == RelationshipRejectProjectAccessError
	assert.Equal(t, wantFault, out.decision.IsIndeterminate(), "indeterminate: %s", out.decision.Reason)
	if wantFault {
		assert.Equal(t, DenyCauseResolutionError, out.decision.DenyCause)
	} else {
		assert.Empty(t, out.decision.DenyCause)
	}
	switch kind {
	case rpaInteractive:
		r := relationshipResult(t, out.decision, rule)
		assert.False(t, r.Accepted)
		assert.Equal(t, wantKind, r.RejectedBy)
		assert.Equal(t, "relationship grant restricted by "+wantKind, out.decision.Reason)
	case rpaUAT:
		require.NotNil(t, out.gateDecision)
		assert.Equal(t, bearerReasonProjectAccessDenied, out.decision.Reason)
		assert.Equal(t, bearerReasonProjectAccessDenied, out.gateDecision.Reason)
	}
}

// TestRelationshipProjectAccess covers owner and ancestor relationships on a
// project agent with and without active project access. For a UAT, the
// agreement of the step-1 admission and the stage is asserted inside every
// _uat subtest by evaluate.
func TestRelationshipProjectAccess(t *testing.T) {
	for _, kind := range rpaPrincipals {
		kind := kind

		// an active direct project binding admits. The member reads
		// another member's agent through the project role (no relationship
		// involved); the admission source is the direct membership.
		t.Run("DirectBindingAdmits_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "a1"+string(kind))
			memberID := tid("rpa-a1-member-" + string(kind))
			uatpMember(t, f.store, f.projectID, memberID)
			agent := uatpAgent(t, f.store, f.projectID, f.ownerID, "a1"+string(kind), f.ownerID)
			res := agentResource(agent)

			out := f.evaluate(t, kind, memberID, res, "agent.read", RelationshipRuleOwner)
			require.True(t, out.decision.Allowed, "DirectBindingAdmits: %s", out.decision.Reason)
			adm, err := f.srv.authzService.ProjectTargetAdmission(context.Background(),
				principalContextForIdentity(rpaIdentity(kind, memberID, f.projectID)), f.projectID, "agent.read", res, nil)
			require.NoError(t, err)
			assert.True(t, adm.Admitted)
			assert.Equal(t, ProjectAccessSourceMembership, adm.Source)
			if kind == rpaUAT {
				assert.True(t, out.gateAdmitted)
			}
		})

		// valid system authority with no membership row admits. The
		// super-admin created the agent; with Explain, the owner candidate
		// is evaluated and passes the stage on system authority alone.
		t.Run("SystemAuthorityWithoutMembershipAdmits_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "a2"+string(kind))
			adminID := tid("rpa-a2-admin-" + string(kind))
			createTestUserWithRole(t, f.store, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
			agent := uatpAgent(t, f.store, f.projectID, adminID, "a2"+string(kind), adminID)
			res := agentResource(agent)

			out := f.evaluate(t, kind, adminID, res, "agent.attach", RelationshipRuleOwner)
			require.True(t, out.decision.Allowed, "SystemAuthorityWithoutMembershipAdmits: %s", out.decision.Reason)
			assert.Empty(t, out.stageKind)
			r := relationshipResult(t, out.decision, RelationshipRuleOwner)
			assert.True(t, r.Accepted, "owner candidate: %+v", r)
			adm, err := f.srv.authzService.ProjectTargetAdmission(context.Background(),
				principalContextForIdentity(rpaIdentity(kind, adminID, f.projectID)), f.projectID, "agent.attach", res, nil)
			require.NoError(t, err)
			assert.True(t, adm.Admitted)
			assert.Equal(t, ProjectAccessSourceSystemRole, adm.Source, "admitted without a membership row")
			if kind == rpaUAT {
				assert.True(t, out.gateAdmitted)
			}
		})

		// the creator/owner with current access admits through the
		// owner relationship (the member role does not grant attach).
		t.Run("OwnerWithAccessAdmits_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "a3"+string(kind))
			memberID := tid("rpa-a3-member-" + string(kind))
			uatpMember(t, f.store, f.projectID, memberID)
			agent := uatpAgent(t, f.store, f.projectID, memberID, "a3"+string(kind), f.ownerID)

			out := f.evaluate(t, kind, memberID, agentResource(agent), "agent.attach", RelationshipRuleOwner)
			assertRelationshipAdmit(t, out, RelationshipRuleOwner)
		})

		// an ancestor with current access admits through the ancestor
		// relationship.
		t.Run("AncestorWithAccessAdmits_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "a4"+string(kind))
			memberID := tid("rpa-a4-member-" + string(kind))
			uatpMember(t, f.store, f.projectID, memberID)
			agent := uatpAgent(t, f.store, f.projectID, f.ownerID, "a4"+string(kind), memberID, f.ownerID)

			out := f.evaluate(t, kind, memberID, agentResource(agent), "agent.attach", RelationshipRuleAncestor)
			assertRelationshipAdmit(t, out, RelationshipRuleAncestor)
		})

		// the owner/creator with only historical ancestry (no current
		// project binding) is denied.
		t.Run("OwnerWithHistoricalAncestryOnlyDenied_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "b1"+string(kind))
			userID := tid("rpa-b1-user-" + string(kind))
			f.hubUser(t, userID)
			agent := uatpAgent(t, f.store, f.projectID, userID, "b1"+string(kind), userID)

			out := f.evaluate(t, kind, userID, agentResource(agent), "agent.attach", RelationshipRuleOwner)
			assertProjectAccessDeny(t, kind, out, RelationshipRuleOwner, RelationshipRejectProjectAccess)
		})

		// an ancestor with no current access is denied.
		t.Run("AncestorWithoutAccessDenied_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "b2"+string(kind))
			userID := tid("rpa-b2-user-" + string(kind))
			f.hubUser(t, userID)
			agent := uatpAgent(t, f.store, f.projectID, f.ownerID, "b2"+string(kind), userID, f.ownerID)

			out := f.evaluate(t, kind, userID, agentResource(agent), "agent.attach", RelationshipRuleAncestor)
			assertProjectAccessDeny(t, kind, out, RelationshipRuleAncestor, RelationshipRejectProjectAccess)
		})

		// removing the final qualifying binding denies on the next
		// check. Admitted first, then the binding row is deleted.
		t.Run("FinalBindingRemovedDenied_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "b3"+string(kind))
			memberID := tid("rpa-b3-member-" + string(kind))
			uatpMember(t, f.store, f.projectID, memberID)
			agent := uatpAgent(t, f.store, f.projectID, memberID, "b3"+string(kind), memberID)
			res := agentResource(agent)

			before := f.evaluate(t, kind, memberID, res, "agent.attach", RelationshipRuleOwner)
			require.True(t, before.decision.Allowed, "precondition: admitted with the binding: %s", before.decision.Reason)

			uatpDeleteProjectBinding(t, f.store, memberID, f.projectID)

			after := f.evaluate(t, kind, memberID, res, "agent.attach", RelationshipRuleOwner)
			assertProjectAccessDeny(t, kind, after, RelationshipRuleOwner, RelationshipRejectProjectAccess)
			after = f.evaluate(t, kind, memberID, res, "agent.attach", RelationshipRuleAncestor)
			assertProjectAccessDeny(t, kind, after, RelationshipRuleAncestor, RelationshipRejectProjectAccess)
		})

		// expiry of the final qualifying binding denies on the next
		// check. Admitted while the binding is live, then re-checked after
		// its ExpiresAt has passed.
		t.Run("FinalBindingExpiredDenied_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "b4"+string(kind))
			memberID := tid("rpa-b4-member-" + string(kind))
			expiresAt := time.Now().Add(1500 * time.Millisecond)
			f.expiringMember(t, memberID, expiresAt)
			agent := uatpAgent(t, f.store, f.projectID, memberID, "b4"+string(kind), memberID)
			res := agentResource(agent)

			before := f.evaluate(t, kind, memberID, res, "agent.attach", RelationshipRuleOwner)
			require.True(t, time.Now().Before(expiresAt), "precondition ran after expiry; raise the window")
			require.True(t, before.decision.Allowed, "precondition: admitted before expiry: %s", before.decision.Reason)

			time.Sleep(time.Until(expiresAt) + 100*time.Millisecond)

			after := f.evaluate(t, kind, memberID, res, "agent.attach", RelationshipRuleOwner)
			assertProjectAccessDeny(t, kind, after, RelationshipRuleOwner, RelationshipRejectProjectAccess)
			after = f.evaluate(t, kind, memberID, res, "agent.attach", RelationshipRuleAncestor)
			assertProjectAccessDeny(t, kind, after, RelationshipRuleAncestor, RelationshipRejectProjectAccess)
		})

		// a store fault in the admission lookup fails closed with its
		// own reject kind and the resolution-error tag. Admitted first, then
		// a GetUser fault is injected for the principal.
		t.Run("LookupFaultDenied_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "b5"+string(kind))
			memberID := tid("rpa-b5-member-" + string(kind))
			uatpMember(t, f.store, f.projectID, memberID)
			agent := uatpAgent(t, f.store, f.projectID, memberID, "b5"+string(kind), f.ownerID)
			res := agentResource(agent)

			before := f.evaluate(t, kind, memberID, res, "agent.attach", RelationshipRuleOwner)
			require.True(t, before.decision.Allowed, "precondition: admitted without a fault: %s", before.decision.Reason)

			f.counting.setFault(memberID)

			after := f.evaluate(t, kind, memberID, res, "agent.attach", RelationshipRuleOwner)
			assertProjectAccessDeny(t, kind, after, RelationshipRuleOwner, RelationshipRejectProjectAccessError)
		})
	}
}

// TestRelationshipProjectAccess_Invariants pins behaviour the stage must
// not change.
func TestRelationshipProjectAccess_Invariants(t *testing.T) {
	ctx := context.Background()

	// Principal status: an inactive user with an active membership is
	// denied.
	for _, kind := range rpaPrincipals {
		kind := kind
		t.Run("InactiveUser_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "inactive"+string(kind))
			memberID := tid("rpa-inactive-member-" + string(kind))
			uatpMember(t, f.store, f.projectID, memberID)
			agent := uatpAgent(t, f.store, f.projectID, memberID, "inactive"+string(kind), memberID)
			setUserStatus(t, f.store, memberID, store.UserStatusSuspended)

			out := f.evaluate(t, kind, memberID, agentResource(agent), "agent.attach", RelationshipRuleOwner)
			assert.False(t, out.decision.Allowed, "inactive user: %s", out.decision.Reason)
			assert.Equal(t, RelationshipRejectProjectAccess, out.stageKind)
			assert.False(t, out.decision.IsIndeterminate())
		})
	}

	// Credential restriction: a UAT whose scope lacks the permission is
	// denied with the scope error, before any project-access check.
	t.Run("UATScopeLacksPermission_uat", func(t *testing.T) {
		f := newRPAFixture(t, "scope")
		memberID := tid("rpa-scope-member")
		uatpMember(t, f.store, f.projectID, memberID)
		agent := uatpAgent(t, f.store, f.projectID, memberID, "scope", memberID)
		scoped := NewScopedUserIdentity(NewAuthenticatedUser(memberID, memberID+"@test.com", "User", "member", "api"), f.projectID, []string{"agent:read"})

		d := decidePerm(f.srv.authzService, scoped, agentResource(agent), ActionAttach, "agent.attach", true)
		assert.False(t, d.Allowed)
		assert.Equal(t, "token does not have scope: agent:attach", d.Reason)
		assert.False(t, d.IsIndeterminate())
	})

	// Non-project targets: an owner relationship on a hub-level group is
	// unaffected for a user with no project access at all.
	for _, kind := range rpaPrincipals {
		kind := kind
		t.Run("NonProjectTarget_"+string(kind), func(t *testing.T) {
			f := newRPAFixture(t, "nonproject"+string(kind))
			userID := tid("rpa-nonproject-user-" + string(kind))
			f.hubUser(t, userID)
			group := Resource{Type: "group", ID: tid("rpa-nonproject-group-" + string(kind)), OwnerID: userID}

			kindStage, _ := f.srv.authzService.relationshipProjectAccessStage(ctx,
				principalContextForIdentity(rpaIdentity(kind, userID, f.projectID)), group, "group.update", RelationshipRuleOwner, nil)
			assert.Empty(t, kindStage, "the stage does not apply to a non-project target")
			if kind == rpaInteractive {
				d := decidePerm(f.srv.authzService, rpaIdentity(kind, userID, f.projectID), group, ActionUpdate, "group.update", true)
				assert.True(t, d.Allowed, "hub-level owner grant unchanged: %s", d.Reason)
				assert.Equal(t, "owner", d.MatchedGrant)
			}
		})
	}

	// Agent principals: an agent ancestor's relationship is unchanged; the
	// stage does not apply to agents.
	t.Run("AgentPrincipalUnchanged", func(t *testing.T) {
		f := newRPAFixture(t, "agent")
		ancestorID := tid("rpa-agent-ancestor")
		ancestor := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: ancestorID},
			ProjectID: tid("rpa-agent-other-project"),
			Scopes:    allRegisteredAgentScopes(),
		}}
		descendant := agentResource(&store.Agent{
			ID: tid("rpa-agent-desc"), ProjectID: f.projectID,
			Ancestry: []string{tid("rpa-agent-root"), ancestorID},
		})

		kindStage, _ := f.srv.authzService.relationshipProjectAccessStage(ctx,
			principalContextForIdentity(ancestor), descendant, "agent.notify", RelationshipRuleAncestor, nil)
		assert.Empty(t, kindStage)
		d := decidePerm(f.srv.authzService, ancestor, descendant, Action("notify"), "agent.notify", true)
		assert.True(t, d.Allowed, "agent ancestor unchanged: %s", d.Reason)
		assert.Equal(t, "relationship grant: ancestor access", d.Reason)
		r := relationshipResult(t, d, RelationshipRuleAncestor)
		assert.True(t, r.Accepted)
	})

	// The delegation-ceiling walk evaluates a user delegator's
	// relationships with the stage disabled (userRelationshipAuthority is
	// not covered by this decision).
	t.Run("CeilingWalkUnchanged", func(t *testing.T) {
		f := newRPAFixture(t, "ceiling")
		userID := tid("rpa-ceiling-user")
		f.hubUser(t, userID)
		agent := uatpAgent(t, f.store, f.projectID, userID, "ceiling", userID)
		user, err := f.store.GetUser(ctx, userID)
		require.NoError(t, err)

		ok, reason, err := f.srv.authzService.userRelationshipAuthority(ctx, user, agentResource(agent), ActionAttach, "agent.attach")
		require.NoError(t, err)
		assert.True(t, ok, "ceiling-walk relationship authority unchanged: %s", reason)
	})
}

// TestRelationshipProjectAccess_StagePlacement pins that the stage runs
// after the hub-attested ancestry stage and before the relationship fact.
func TestRelationshipProjectAccess_StagePlacement(t *testing.T) {
	f := newRPAFixture(t, "placement")
	ctx := context.Background()
	userID := tid("rpa-placement-user")
	f.hubUser(t, userID)
	principal := principalContextForIdentity(rpaIdentity(rpaInteractive, userID, f.projectID))
	res := agentResource(&store.Agent{ID: tid("rpa-placement-agent"), ProjectID: f.projectID, OwnerID: userID, Ancestry: []string{userID}})
	access := &relationshipProjectAccess{memo: &ProjectAdmissionCache{}}

	factCalled := false
	c := relationshipCandidate{
		rule: RelationshipRuleOwner,
		fact: func(context.Context) (*SharingSource, bool, string) {
			factCalled = true
			return nil, true, ""
		},
		decision: Decision{Allowed: true},
	}
	var rejectedBy string
	_, ok := f.srv.authzService.runRelationshipStages(ctx, principal, "user", res, "agent.attach", nil, c, access,
		func(kind, _ string) { rejectedBy = kind })
	assert.False(t, ok)
	assert.Equal(t, RelationshipRejectProjectAccess, rejectedBy)
	assert.False(t, factCalled, "the relationship fact must not run after a project-access reject")

	// A permission outside the relationship policy is rejected at stage 1,
	// before the project-access stage reads anything.
	before := f.counting.calls(userID)
	_, ok = f.srv.authzService.runRelationshipStages(ctx, principal, "user", res, "project.delete", nil, c, access,
		func(kind, _ string) { rejectedBy = kind })
	assert.False(t, ok)
	assert.Equal(t, RelationshipRejectPolicy, rejectedBy)
	assert.Equal(t, before, f.counting.calls(userID))
}

// TestRelationshipProjectAccess_ProjectParentWithoutID pins that a
// project-parented target with no project ID is rejected by the stage, not
// treated as a non-project target.
func TestRelationshipProjectAccess_ProjectParentWithoutID(t *testing.T) {
	f := newRPAFixture(t, "noparentid")
	ctx := context.Background()
	memberID := tid("rpa-noparentid-member")
	uatpMember(t, f.store, f.projectID, memberID)
	res := Resource{Type: "agent", ID: tid("rpa-noparentid-agent"), OwnerID: memberID, ParentType: "project"}
	ident := rpaIdentity(rpaInteractive, memberID, f.projectID)

	kind, _ := f.srv.authzService.relationshipProjectAccessStage(ctx, principalContextForIdentity(ident), res, "agent.attach", RelationshipRuleOwner, nil)
	assert.Equal(t, RelationshipRejectProjectAccess, kind)

	d := decidePerm(f.srv.authzService, ident, res, ActionAttach, "agent.attach", true)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, RelationshipRejectProjectAccess, relationshipResult(t, d, RelationshipRuleOwner).RejectedBy)
}

// TestRelationshipProjectAccess_RuleFilter pins that only the owner and
// ancestor rules are gated: progeny and hub-member service-account assign
// candidates pass the stage on a project target even without access.
func TestRelationshipProjectAccess_RuleFilter(t *testing.T) {
	f := newRPAFixture(t, "rulefilter")
	ctx := context.Background()
	userID := tid("rpa-rulefilter-user")
	f.hubUser(t, userID)
	principal := principalContextForIdentity(rpaIdentity(rpaInteractive, userID, f.projectID))
	res := agentResource(&store.Agent{ID: tid("rpa-rulefilter-agent"), ProjectID: f.projectID, OwnerID: userID, Ancestry: []string{userID}})

	for _, rule := range []RelationshipRuleID{RelationshipRuleProgeny, RelationshipRuleHubMemberSAAssign} {
		kind, _ := f.srv.authzService.relationshipProjectAccessStage(ctx, principal, res, "agent.attach", rule, nil)
		assert.Empty(t, kind, "rule %s is not gated", rule)
	}
	for _, rule := range []RelationshipRuleID{RelationshipRuleOwner, RelationshipRuleAncestor} {
		kind, _ := f.srv.authzService.relationshipProjectAccessStage(ctx, principal, res, "agent.attach", rule, nil)
		assert.Equal(t, RelationshipRejectProjectAccess, kind, "rule %s is gated", rule)
	}
}

// TestRelationshipProjectAccess_DevPrincipal pins that the dev/local-user
// principal is gated like a session user: denied without project access,
// admitted once a binding exists.
func TestRelationshipProjectAccess_DevPrincipal(t *testing.T) {
	f := newRPAFixture(t, "dev")
	ctx := context.Background()
	dev := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@test.com"})
	require.Equal(t, PrincipalKindDev, principalContextForIdentity(dev).Kind)
	if _, err := f.store.GetUser(ctx, dev.ID()); err != nil {
		f.hubUser(t, dev.ID())
	}
	res := agentResource(&store.Agent{ID: tid("rpa-dev-agent"), ProjectID: f.projectID, OwnerID: dev.ID()})

	d := decidePerm(f.srv.authzService, dev, res, ActionAttach, "agent.attach", true)
	assert.False(t, d.Allowed, "dev owner without access: %s", d.Reason)
	assert.Equal(t, RelationshipRejectProjectAccess, relationshipResult(t, d, RelationshipRuleOwner).RejectedBy)

	grantProjectAccessOnly(t, f.store, dev.ID(), f.projectID)
	kind, _ := f.srv.authzService.relationshipProjectAccessStage(ctx, principalContextForIdentity(dev), res, "agent.attach", RelationshipRuleOwner, nil)
	assert.Empty(t, kind)
	d = decidePerm(f.srv.authzService, dev, res, ActionAttach, "agent.attach", true)
	assert.True(t, d.Allowed, "dev owner with access: %s", d.Reason)
	assert.Equal(t, "owner", d.MatchedGrant)
}

// TestRelationshipProjectAccess_ProjectScopedResources covers the owner
// relationship on project-scoped template, skill and harness config
// targets, whose target class carries the project scope kind.
func TestRelationshipProjectAccess_ProjectScopedResources(t *testing.T) {
	cases := []struct {
		name   string
		perm   string
		action Action
		build  func(id, ownerID, projectID string) Resource
	}{
		{"template", "template.update", ActionUpdate, func(id, ownerID, projectID string) Resource {
			return templateResource(&store.Template{ID: id, OwnerID: ownerID, Scope: store.TemplateScopeProject, ScopeID: projectID})
		}},
		{"skill", "skill.update", ActionUpdate, func(id, ownerID, projectID string) Resource {
			return skillResource(&store.Skill{ID: id, OwnerID: ownerID, Scope: store.SkillScopeProject, ScopeID: projectID})
		}},
		{"harness_config", "harness_config.update", ActionUpdate, func(id, ownerID, projectID string) Resource {
			return harnessConfigResource(&store.HarnessConfig{ID: id, OwnerID: ownerID, Scope: store.HarnessConfigScopeProject, ScopeID: projectID})
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newRPAFixture(t, "scoped"+tc.name)
			ctx := context.Background()
			userID := tid("rpa-scoped-user-" + tc.name)
			f.hubUser(t, userID)
			res := tc.build(tid("rpa-scoped-res-"+tc.name), userID, f.projectID)
			require.Equal(t, f.projectID, resourceProjectScope(res), "constructor must set the project parent")

			for _, kind := range rpaPrincipals {
				kindStage, _ := f.srv.authzService.relationshipProjectAccessStage(ctx,
					principalContextForIdentity(rpaIdentity(kind, userID, f.projectID)), res, tc.perm, RelationshipRuleOwner, nil)
				assert.Equal(t, RelationshipRejectProjectAccess, kindStage, "%s without access", kind)
			}
			d := decidePerm(f.srv.authzService, rpaIdentity(rpaInteractive, userID, f.projectID), res, tc.action, tc.perm, true)
			assert.False(t, d.Allowed, "owner without access: %s", d.Reason)
			assert.Equal(t, RelationshipRejectProjectAccess, relationshipResult(t, d, RelationshipRuleOwner).RejectedBy)

			grantProjectAccessOnly(t, f.store, userID, f.projectID)

			for _, kind := range rpaPrincipals {
				kindStage, _ := f.srv.authzService.relationshipProjectAccessStage(ctx,
					principalContextForIdentity(rpaIdentity(kind, userID, f.projectID)), res, tc.perm, RelationshipRuleOwner, nil)
				assert.Empty(t, kindStage, "%s with access", kind)
			}
			d = decidePerm(f.srv.authzService, rpaIdentity(rpaInteractive, userID, f.projectID), res, tc.action, tc.perm, true)
			assert.True(t, d.Allowed, "owner with access: %s", d.Reason)
			assert.Equal(t, "owner", d.MatchedGrant)
		})
	}
}

// messageAgent creates an agent in the fixture project with the given
// message mode, owned by the project owner, with ancestry.
func (f *rpaFixture) messageAgent(t *testing.T, suffix, mode string, ancestry ...string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:          tid("rpa-msg-agent-" + suffix),
		Slug:        "rpa-msg-agent-" + suffix,
		Name:        "RPA Message Agent " + suffix,
		ProjectID:   f.projectID,
		OwnerID:     f.ownerID,
		Phase:       "stopped",
		Ancestry:    ancestry,
		MessageMode: mode,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), agent))
	return agent
}

// TestRelationshipProjectAccess_Message covers the messaging ancestry allow
// (authorizeUserToAgent) for a full-session user: the ancestry allow
// requires active access to the agent's project.
func TestRelationshipProjectAccess_Message(t *testing.T) {
	ctx := context.Background()
	send := func(f *rpaFixture, userID string, agent *store.Agent) (bool, string) {
		allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, rpaIdentity(rpaInteractive, userID, f.projectID), agent, false)
		return allowed, reason
	}

	for _, mode := range []string{store.MessageModeLineage, store.MessageModeProject} {
		mode := mode

		t.Run("BindingRemovedDenied_"+mode, func(t *testing.T) {
			f := newRPAFixture(t, "msgremoved"+mode)
			memberID := tid("rpa-msgremoved-member-" + mode)
			uatpMember(t, f.store, f.projectID, memberID)
			agent := f.messageAgent(t, "removed-"+mode, mode, memberID, f.ownerID)

			allowed, reason := send(f, memberID, agent)
			require.True(t, allowed, "precondition: ancestor with access may message: %s", reason)
			assert.Equal(t, "user in target ancestry", reason)

			uatpDeleteProjectBinding(t, f.store, memberID, f.projectID)

			allowed, reason = send(f, memberID, agent)
			assert.False(t, allowed, "ancestor without access: %s", reason)
		})

		t.Run("BindingExpiredDenied_"+mode, func(t *testing.T) {
			f := newRPAFixture(t, "msgexpired"+mode)
			memberID := tid("rpa-msgexpired-member-" + mode)
			expiresAt := time.Now().Add(1500 * time.Millisecond)
			f.expiringMember(t, memberID, expiresAt)
			agent := f.messageAgent(t, "expired-"+mode, mode, memberID, f.ownerID)

			allowed, reason := send(f, memberID, agent)
			require.True(t, time.Now().Before(expiresAt), "precondition ran after expiry; raise the window")
			require.True(t, allowed, "precondition: ancestor with a live binding may message: %s", reason)

			time.Sleep(time.Until(expiresAt) + 100*time.Millisecond)

			allowed, reason = send(f, memberID, agent)
			assert.False(t, allowed, "ancestor after expiry: %s", reason)
		})
	}

	t.Run("MemberAncestorAllowed", func(t *testing.T) {
		f := newRPAFixture(t, "msgmember")
		memberID := tid("rpa-msgmember-member")
		uatpMember(t, f.store, f.projectID, memberID)
		agent := f.messageAgent(t, "member", store.MessageModeLineage, memberID, f.ownerID)

		allowed, reason := send(f, memberID, agent)
		assert.True(t, allowed, "member ancestor: %s", reason)
		assert.Equal(t, "user in target ancestry", reason)
	})

	t.Run("SystemAuthorityWithoutMembershipAllowed", func(t *testing.T) {
		f := newRPAFixture(t, "msgsysauth")
		userID := tid("rpa-msgsysauth-user")
		createTestUserWithRole(t, f.store, userID, userID+"@test.com", "member", store.SystemRoleSuperAdmin)
		agent := f.messageAgent(t, "sysauth", store.MessageModeLineage, userID, f.ownerID)

		// The session identity's role is "member", so the super-admin
		// identity shortcut does not apply; the store system binding is the
		// only source of project access.
		allowed, reason := send(f, userID, agent)
		assert.True(t, allowed, "system authority ancestor: %s", reason)
		assert.Equal(t, "user in target ancestry", reason)
	})

	t.Run("LookupFaultDenied", func(t *testing.T) {
		f := newRPAFixture(t, "msgfault")
		memberID := tid("rpa-msgfault-member")
		uatpMember(t, f.store, f.projectID, memberID)
		agent := f.messageAgent(t, "fault", store.MessageModeLineage, memberID, f.ownerID)

		allowed, reason := send(f, memberID, agent)
		require.True(t, allowed, "precondition: allowed without a fault: %s", reason)

		f.counting.setFault(memberID)

		allowed, reason = send(f, memberID, agent)
		assert.False(t, allowed)
		assert.Contains(t, reason, "project access check failed (fail-closed)")
		// The public code is the lineage-mode refusal any unauthorized
		// sender gets.
		assert.Equal(t, ReasonModeLineageNoAncestry, mapReasonToCode(reason))
		assert.Equal(t, ReasonMissingPermission, mapReasonToCode(messageAncestorFaultReason(store.MessageModeProject)))
	})
}

// TestRelationshipProjectAccess_MessageRefusalIdentical pins that an
// ancestor whose project binding was removed gets exactly the refusal an
// unrelated non-member gets from the message handler: same status and same
// body, for a lineage-mode and a project-mode agent.
func TestRelationshipProjectAccess_MessageRefusalIdentical(t *testing.T) {
	for _, mode := range []string{store.MessageModeLineage, store.MessageModeProject} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			f := newRPAFixture(t, "msgsame"+mode)
			ctx := context.Background()
			formerID := tid("rpa-msgsame-former-" + mode)
			outsiderID := tid("rpa-msgsame-outsider-" + mode)
			uatpMember(t, f.store, f.projectID, formerID)
			f.hubUser(t, outsiderID)
			agent := f.messageAgent(t, "same-"+mode, mode, formerID, f.ownerID)

			former, err := f.store.GetUser(ctx, formerID)
			require.NoError(t, err)
			outsider, err := f.store.GetUser(ctx, outsiderID)
			require.NoError(t, err)
			body := map[string]interface{}{"message": "hello", "interrupt": false}
			path := "/api/v1/agents/" + agent.ID + "/message"

			uatpDeleteProjectBinding(t, f.store, formerID, f.projectID)

			formerRec := doRequestAsUser(t, f.srv, former, http.MethodPost, path, body)
			outsiderRec := doRequestAsUser(t, f.srv, outsider, http.MethodPost, path, body)

			require.Equal(t, http.StatusForbidden, outsiderRec.Code, "outsider: %s", outsiderRec.Body.String())
			assert.Equal(t, outsiderRec.Code, formerRec.Code)
			assert.Equal(t, outsiderRec.Body.String(), formerRec.Body.String(),
				"the former-member ancestor's refusal must be identical to an unrelated sender's")
		})
	}
}

// TestRelationshipProjectAccess_DecideRefusalIdentical pins that, at an
// endpoint authorized through Decide (agent PTY attach), a former-member
// owner/ancestor gets exactly the refusal an unrelated non-member gets, and
// the relationship reject kind never reaches the response body.
func TestRelationshipProjectAccess_DecideRefusalIdentical(t *testing.T) {
	f := newRPAFixture(t, "decidesame")
	ctx := context.Background()
	formerID := tid("rpa-decidesame-former")
	outsiderID := tid("rpa-decidesame-outsider")
	uatpMember(t, f.store, f.projectID, formerID)
	f.hubUser(t, outsiderID)
	agent := uatpAgent(t, f.store, f.projectID, formerID, "decidesame", formerID)

	former, err := f.store.GetUser(ctx, formerID)
	require.NoError(t, err)
	outsider, err := f.store.GetUser(ctx, outsiderID)
	require.NoError(t, err)
	path := "/api/v1/agents/" + agent.ID + "/pty"

	rec := doRequestAsUser(t, f.srv, former, http.MethodGet, path, nil)
	requireAuthorizedPTY(t, rec, "precondition: owner with access may attach")

	uatpDeleteProjectBinding(t, f.store, formerID, f.projectID)

	// The former member's own decision is denied at the relationship stage.
	d := decidePerm(f.srv.authzService, rpaIdentity(rpaInteractive, formerID, f.projectID), agentResource(agent), ActionAttach, "agent.attach", true)
	require.False(t, d.Allowed)
	require.Equal(t, RelationshipRejectProjectAccess, relationshipResult(t, d, RelationshipRuleOwner).RejectedBy)

	formerRec := doRequestAsUser(t, f.srv, former, http.MethodGet, path, nil)
	outsiderRec := doRequestAsUser(t, f.srv, outsider, http.MethodGet, path, nil)

	require.Equal(t, http.StatusForbidden, outsiderRec.Code, "outsider: %s", outsiderRec.Body.String())
	assert.Equal(t, outsiderRec.Code, formerRec.Code)
	assert.Equal(t, outsiderRec.Body.String(), formerRec.Body.String(),
		"the former member's refusal must be identical to an unrelated user's")
	assert.NotContains(t, formerRec.Body.String(), RelationshipRejectProjectAccess)
	assert.NotContains(t, formerRec.Body.String(), "relationship grant")
}
