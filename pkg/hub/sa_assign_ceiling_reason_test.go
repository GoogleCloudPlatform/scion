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
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#2144: when the project-default service-account assignment is
// denied at the delegation ceiling, the 403 body must say why — orphaned
// creator vs. a creator that lost the permission — instead of the generic
// "You don't have permission..." message that also covers ordinary policy
// denials. These tests exercise evaluateSAAssignment directly (the
// transport-independent body authorizeSAAssignment calls), the same way
// TestEvaluateSAAssignment_NilRequestPolicyDenial does.

// scaCreateSA registers a project-scoped, verified GCP service account for
// the ceiling-reason tests. Verification is irrelevant to Layer 1 (the Hub
// policy / delegation ceiling check evaluateSAAssignment performs before
// ever reaching Layer 2's actAs check), but true matches a realistic
// project-default assignment.
func scaCreateSA(t *testing.T, s store.Store, projectID string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        tid("sca-sa-" + projectID),
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Email:     "sca-default@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		Verified:  true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// scaGenericDenyMsg is the literal generic 403 body, deliberately not a
// reference to saAssignGenericForbiddenMsg: tests that compare against this
// constant must fail if the production constant's value ever drifts, not
// just if it disappears.
const scaGenericDenyMsg = "You don't have permission to assign this GCP service account"

// scaUnrecordedDenyMsg is the Layer 1 body for DenyCauseCeilingUnrecorded,
// spelled out literally for the same reason as scaGenericDenyMsg.
const scaUnrecordedDenyMsg = "This agent cannot assign service accounts: its delegation chain includes an agent " +
	"created without recorded provenance (this agent or one of the agents that created it). " +
	"Have an authorized user recreate this agent directly (not from another agent)."

// scaCreateDelegatorWithoutAssign creates an active, existing user bound to a
// minimal custom project-scoped role that omits gcp_service_account.assign.
//
// GoogleCloudPlatform/scion#2062 added gcp_service_account.assign to all
// three built-in project roles (project-owner, project-admin, project-
// member), so none of them can stand in any longer for "an active project
// member who lacks the permission" — every built-in role now has it. A
// custom role is the only way left to construct a delegator that genuinely
// lacks the permission while still being a real, resolvable project member.
func scaCreateDelegatorWithoutAssign(t *testing.T, s store.Store, userID, email, projectID string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: email, DisplayName: email, Role: "member", Status: "active",
	}))

	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "sca-no-assign-" + userID,
		Description: "Project role without gcp_service_account.assign, for ceiling tests",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// TestEvaluateSAAssignment_CeilingOrphanedDelegator covers the case that
// motivated the issue: the agent's delegator (its creator, here a user) no
// longer exists. The Hub policy kernel allows via the agent-jwt-scope
// synthetic binding (project:agent:create covers gcp_service_account.assign),
// then Step 10 (the delegation ceiling) flips it to a deny because the
// delegator cannot be resolved.
func TestEvaluateSAAssignment_CeilingOrphanedDelegator(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("sca-orphan-proj")
	agentID := tid("sca-orphan-agent")
	goneUserID := tid("sca-orphan-gone-user")

	createDCProject(t, s, projectID, "sca-orphan-project")
	createDCAgent(t, s, agentID, projectID, goneUserID, AgentRoleFull)
	sa := scaCreateSA(t, s, projectID)

	// The delegator (goneUserID) is never created — GetUser returns
	// ErrNotFound, which is the orphaned-delegation branch.
	createDCEdge(t, s, store.DelegationPrincipalUser, goneUserID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	agentCtx := contextWithIdentity(ctx, agent)

	denial := srv.evaluateSAAssignment(agentCtx, nil, sa, SurfaceProjectDefault)
	require.NotNil(t, denial, "an orphaned delegator must deny the assignment")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
	assert.Equal(t,
		"This agent cannot assign service accounts: a principal in its delegation chain "+
			"(the user or agent that created it, or one of their creators) does not exist. "+
			"Ask an admin to recreate the agent under a current user.",
		denial.msg)
	assert.NotContains(t, denial.msg, goneUserID, "the 403 body must not name the missing principal's ID")
	assert.NotContains(t, denial.msg, agentID, "the 403 body must not name the agent's ID")
}

// TestEvaluateSAAssignment_CeilingDelegatorLacksPermission covers the second
// ceiling cause: the delegator still exists and is active, but does not hold
// gcp_service_account.assign. Every built-in project role has carried that
// permission since GoogleCloudPlatform/scion#2062, and the ceiling treats a
// super-admin as holding every permission, so only a delegator bound to a
// custom role that omits the permission can lack it — hence the delegator
// here is bound to a minimal custom role instead of a seeded one.
func TestEvaluateSAAssignment_CeilingDelegatorLacksPermission(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("sca-lacks-proj")
	agentID := tid("sca-lacks-agent")
	delegatorID := tid("sca-lacks-delegator")

	createDCProject(t, s, projectID, "sca-lacks-project")
	scaCreateDelegatorWithoutAssign(t, s, delegatorID, "sca-lacks-delegator@example.com", projectID)
	assertNotSystemAdmin(t, srv.authzService, ctx, delegatorID)
	createDCAgent(t, s, agentID, projectID, delegatorID, AgentRoleFull)
	sa := scaCreateSA(t, s, projectID)

	createDCEdge(t, s, store.DelegationPrincipalUser, delegatorID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	agentCtx := contextWithIdentity(ctx, agent)

	denial := srv.evaluateSAAssignment(agentCtx, nil, sa, SurfaceProjectDefault)
	require.NotNil(t, denial, "a delegator that lost the permission must deny the assignment")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
	assert.Equal(t,
		"This agent cannot assign service accounts: a principal in its delegation chain "+
			"(the user or agent that created it, or one of their creators) does not hold permission "+
			"to assign this service account.",
		denial.msg)
	assert.NotContains(t, denial.msg, delegatorID, "the 403 body must not name the delegator's ID")
	assert.NotContains(t, denial.msg, agentID, "the 403 body must not name the agent's ID")
}

// TestEvaluateSAAssignment_CeilingAgentDelegatorLacksPermission covers the
// agent-delegator twin of TestEvaluateSAAssignment_CeilingDelegatorLacksPermission:
// the immediate delegator is itself an agent (not a user) whose recorded role
// no longer carries the scope that maps to gcp_service_account.assign.
//
// Chain: user U (exists, holds project:agent:create) -> agent A (role
// readonly, so it lacks the agent-create scope) -> agent B (role full). B
// requests the SA assignment. The Hub policy kernel (within Layer 1) allows
// via B's own agent-jwt-scope synthetic binding; Step 10 checks B's
// delegator A, finds A's recorded role (readonly) does not carry the scope
// that maps to gcp_service_account.assign, and denies with
// DenyCauseCeilingDelegatorLacksPermission without recursing to U. This
// exercises the agent branch of that check, the twin of the user-delegator
// branch the sibling test above covers.
func TestEvaluateSAAssignment_CeilingAgentDelegatorLacksPermission(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("sca-agentlacks-proj")
	userID := tid("sca-agentlacks-user")
	agentAID := tid("sca-agentlacks-agent-a")
	agentBID := tid("sca-agentlacks-agent-b")

	createDCProject(t, s, projectID, "sca-agentlacks-project")
	createDCUser(t, s, userID, "sca-agentlacks-user@example.com", projectID, store.ProjectRoleOwner)
	assertNotSystemAdmin(t, srv.authzService, ctx, userID)
	createDCAgent(t, s, agentAID, projectID, userID, AgentRoleReadOnly)
	createDCAgent(t, s, agentBID, projectID, agentAID, AgentRoleFull)
	sa := scaCreateSA(t, s, projectID)

	createDCEdge(t, s, store.DelegationPrincipalUser, userID,
		store.DelegationPrincipalAgent, agentAID,
		store.RoleScopeProject, projectID, string(AgentRoleReadOnly))
	createDCEdge(t, s, store.DelegationPrincipalAgent, agentAID,
		store.DelegationPrincipalAgent, agentBID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agentB := dcAgentIdentity(agentBID, projectID, AgentRoleFull)
	agentBCtx := contextWithIdentity(ctx, agentB)

	denial := srv.evaluateSAAssignment(agentBCtx, nil, sa, SurfaceProjectDefault)
	require.NotNil(t, denial, "an agent delegator that lacks the permission must deny the assignment")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
	assert.Equal(t,
		"This agent cannot assign service accounts: a principal in its delegation chain "+
			"(the user or agent that created it, or one of their creators) does not hold permission "+
			"to assign this service account.",
		denial.msg)
	assert.NotContains(t, denial.msg, userID, "the 403 body must not name the grandparent user's ID")
	assert.NotContains(t, denial.msg, agentAID, "the 403 body must not name the delegator agent's ID")
	assert.NotContains(t, denial.msg, agentBID, "the 403 body must not name the requesting agent's ID")
}

// TestEvaluateSAAssignment_OrdinaryDenialKeepsGenericMessage pins that a
// non-ceiling denial (here: a user with no role anywhere) is completely
// unaffected by this change — byte-for-byte the same message as before.
func TestEvaluateSAAssignment_OrdinaryDenialKeepsGenericMessage(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("sca-ordinary-proj")
	createDCProject(t, s, projectID, "sca-ordinary-project")
	sa := scaCreateSA(t, s, projectID)

	stranger := NewAuthenticatedUser(tid("sca-ordinary-stranger"), "stranger@example.com", "Stranger",
		store.UserRoleMember, "test")
	strangerCtx := contextWithIdentity(ctx, stranger)

	denial := srv.evaluateSAAssignment(strangerCtx, nil, sa, SurfaceProjectDefault)
	require.NotNil(t, denial, "a stranger with no role must be denied")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
	assert.Equal(t, scaGenericDenyMsg, denial.msg,
		"an ordinary policy denial must keep the exact generic message, byte for byte")
}

// scaGetUserErrorStore wraps a real store and forces GetUser to fail with a
// non-ErrNotFound error, simulating a genuine store fault (as opposed to a
// definitively-missing delegator) inside evaluateUserDelegatorAuthority.
type scaGetUserErrorStore struct {
	store.Store
	failUserID string
	err        error
}

func (s *scaGetUserErrorStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.failUserID {
		return nil, s.err
	}
	return s.Store.GetUser(ctx, id)
}

// TestDelegationCeiling_StoreErrorSetsCeilingErrorCause covers the third
// DenyCause directly at the AuthzService level (the same level
// TestDelegationCeiling_MintAndAssignFailClosed uses): a genuine store fault
// while checking the delegator's permission must classify as
// DenyCauseCeilingError, not one of the two specific causes, so that
// evaluateSAAssignment's switch (tested above) falls through to the generic
// message for it — it is transient/internal, not a fact about the caller.
//
// This is built directly on AuthzService rather than a full *Server: New()
// runs a store-introspection migration step that the error-injecting wrapper
// does not support, and the ceiling classification this test targets lives
// entirely in AuthzService.Decide, one layer below evaluateSAAssignment.
func TestDelegationCeiling_StoreErrorSetsCeilingErrorCause(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Skipf("skipping: test store unavailable (%v)", err)
	}
	ctx := context.Background()
	require.NoError(t, s.Migrate(ctx))
	_ = s.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")
	reconcileBuiltInRoles(ctx, s)

	projectID := tid("sca-storeerr-proj")
	agentID := tid("sca-storeerr-agent")
	delegatorID := tid("sca-storeerr-delegator")

	createDCProject(t, s, projectID, "sca-storeerr-project")
	createDCUser(t, s, delegatorID, "sca-storeerr-delegator@example.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, delegatorID, AgentRoleFull)
	sa := scaCreateSA(t, s, projectID)
	createDCEdge(t, s, store.DelegationPrincipalUser, delegatorID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	injectedErr := errors.New("simulated store failure")
	errStore := &scaGetUserErrorStore{Store: s, failUserID: delegatorID, err: injectedErr}
	authz := NewAuthzService(errStore, slog.Default())

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := gcpServiceAccountResource(sa)

	decision := authz.CheckAccess(ctx, agent, resource, ActionAssign)
	require.False(t, decision.Allowed, "a ceiling store fault must fail closed")
	assert.Equal(t, DenyCauseCeilingError, decision.DenyCause,
		"a genuine store fault must classify as ceiling_error, not orphaned or lacks-permission")
}

// TestSAAssignForbiddenMessage_AllCauses drives saAssignForbiddenMessage (the
// helper evaluateSAAssignment's Layer 1 calls) directly with every DenyCause
// value, including one no constant names, rather than relying on the
// switch's default case being exercised only implicitly through the
// ceiling_error store-fault test above.
func TestSAAssignForbiddenMessage_AllCauses(t *testing.T) {
	tests := []struct {
		name  string
		cause DenyCause
		want  string
	}{
		{
			name:  "no cause falls through to the generic message",
			cause: "",
			want:  scaGenericDenyMsg,
		},
		{
			name:  "ceiling_error falls through to the generic message",
			cause: DenyCauseCeilingError,
			want:  scaGenericDenyMsg,
		},
		{
			name:  "ceiling_orphaned gets the chain-aware diagnosis",
			cause: DenyCauseCeilingOrphaned,
			want: "This agent cannot assign service accounts: a principal in its delegation chain " +
				"(the user or agent that created it, or one of their creators) does not exist. " +
				"Ask an admin to recreate the agent under a current user.",
		},
		{
			name:  "ceiling_delegator_lacks_permission gets the chain-aware diagnosis",
			cause: DenyCauseCeilingDelegatorLacksPermission,
			want: "This agent cannot assign service accounts: a principal in its delegation chain " +
				"(the user or agent that created it, or one of their creators) does not hold permission " +
				"to assign this service account.",
		},
		{
			name:  "ceiling_unrecorded names the missing provenance and the remedy",
			cause: DenyCauseCeilingUnrecorded,
			want:  scaUnrecordedDenyMsg,
		},
		{
			name:  "an unrecognised cause falls through to the generic message",
			cause: DenyCause("some_future_cause_nobody_wired_a_message_for"),
			want:  scaGenericDenyMsg,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, saAssignForbiddenMessage(tt.cause))
		})
	}
}

// TestEvaluateSAAssignment_CeilingOrphanedGrandparent covers an agent created
// by an agent, where the FAILING link is the grandparent (a user), not the
// agent's immediate creator.
//
// Chain: user U (never created — orphaned) -> agent A (holds
// project:agent:create) -> agent B. B requests the SA assignment. The Hub
// policy kernel (within Layer 1) allows via B's own agent-jwt-scope
// synthetic binding; Step 10 walks the chain: B's delegator A still holds
// the permission, so the walk recurses into A's own chain, finds A's
// delegator U does not resolve, and denies the mint with
// DenyCauseCeilingOrphaned — propagated back through B's result via
// the cause pointer threaded through walkDelegationChain's recursive call.
// The message must not name B's immediate creator (A, which exists) as the
// missing principal — it names the delegation chain generically instead.
func TestEvaluateSAAssignment_CeilingOrphanedGrandparent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("sca-grandparent-proj")
	goneUserID := tid("sca-grandparent-gone-user")
	agentAID := tid("sca-grandparent-agent-a")
	agentBID := tid("sca-grandparent-agent-b")

	createDCProject(t, s, projectID, "sca-grandparent-project")
	// goneUserID is deliberately never created: GetUser returns ErrNotFound,
	// the orphaned-delegation branch, when A's chain is walked.
	createDCAgent(t, s, agentAID, projectID, goneUserID, AgentRoleFull)
	createDCAgent(t, s, agentBID, projectID, agentAID, AgentRoleFull)
	sa := scaCreateSA(t, s, projectID)

	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, goneUserID,
		store.DelegationPrincipalAgent, agentAID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalAgent, agentAID,
		store.DelegationPrincipalAgent, agentBID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agentB := dcAgentIdentity(agentBID, projectID, AgentRoleFull)
	agentBCtx := contextWithIdentity(ctx, agentB)

	denial := srv.evaluateSAAssignment(agentBCtx, nil, sa, SurfaceProjectDefault)
	require.NotNil(t, denial, "an orphaned grandparent must deny the assignment")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
	assert.Equal(t,
		"This agent cannot assign service accounts: a principal in its delegation chain "+
			"(the user or agent that created it, or one of their creators) does not exist. "+
			"Ask an admin to recreate the agent under a current user.",
		denial.msg,
		"the message must not claim B's existing immediate creator (A) is the missing principal")
	assert.NotContains(t, denial.msg, goneUserID, "the 403 body must not name the missing principal's ID")
	assert.NotContains(t, denial.msg, agentAID, "the 403 body must not name the intermediate agent's ID")
	assert.NotContains(t, denial.msg, agentBID, "the 403 body must not name the requesting agent's ID")
}

// scaOrphanedDenyMsg and scaLacksPermissionDenyMsg are the literal
// ceiling-cause 403 bodies (see scaGenericDenyMsg for why they are literals).
const (
	scaOrphanedDenyMsg = "This agent cannot assign service accounts: a principal in its delegation chain " +
		"(the user or agent that created it, or one of their creators) does not exist. " +
		"Ask an admin to recreate the agent under a current user."
	scaLacksPermissionDenyMsg = "This agent cannot assign service accounts: a principal in its delegation chain " +
		"(the user or agent that created it, or one of their creators) does not hold permission " +
		"to assign this service account."
)

// TestEvaluateSAAssignment_CeilingCauseByDelegatorState pins the DenyCause
// the delegation ceiling records for each non-live delegator shape, and the
// SA-assign 403 body that cause selects:
//   - the migration sentinel, a soft-deleted agent delegator (immediate or
//     deeper) and a missing agent delegator do not resolve to a live
//     principal: ceiling_orphaned, "does not exist";
//   - a suspended user delegator exists but holds no permission, whether or
//     not it is a super-admin: ceiling_delegator_lacks_permission, "does not
//     hold permission".
//
// Where the chain can be live, the case first shows that it passes the Hub
// policy layer, so the deny comes from the delegator's state alone.
func TestEvaluateSAAssignment_CeilingCauseByDelegatorState(t *testing.T) {
	type chainFixture struct {
		s         store.Store
		projectID string
		userID    string
	}
	// chain stores agents ids[0..n-1]: the user delegates to ids[0] and each
	// agent delegates to the next.
	chain := func(t *testing.T, f chainFixture, ids ...string) {
		t.Helper()
		prev, prevType := f.userID, store.DelegationPrincipalUser
		for _, id := range ids {
			createDCAgent(t, f.s, id, f.projectID, prev, AgentRoleFull)
			seedRecordedDelegationEdge(t, f.s, prevType, prev, store.DelegationPrincipalAgent, id,
				store.RoleScopeProject, f.projectID, string(AgentRoleFull))
			prev, prevType = id, store.DelegationPrincipalAgent
		}
	}

	for _, tc := range []struct {
		name string
		// setup builds the chain and returns the requesting agent. When
		// live is true the chain is live on return; breakChain then makes
		// it non-live.
		setup      func(t *testing.T, f chainFixture) string
		live       bool
		breakChain func(t *testing.T, f chainFixture)
		cause      DenyCause
		msg        string
	}{
		{
			name: "migration sentinel",
			setup: func(t *testing.T, f chainFixture) string {
				id := tid("sca-state-sentinel")
				createDCAgent(t, f.s, id, f.projectID, f.userID, AgentRoleFull)
				require.NoError(t, f.s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
					DelegatorType: store.DelegationPrincipalUser, DelegatorID: "system/migration",
					DelegateType: store.DelegationPrincipalAgent, DelegateID: id,
					ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
					Role: string(AgentRoleFull), Active: true, Grandfathered: true,
				}))
				return id
			},
			cause: DenyCauseCeilingOrphaned,
			msg:   scaOrphanedDenyMsg,
		},
		{
			name: "soft-deleted immediate agent delegator",
			setup: func(t *testing.T, f chainFixture) string {
				chain(t, f, tid("sca-state-sdi-parent"), tid("sca-state-sdi-child"))
				return tid("sca-state-sdi-child")
			},
			live:       true,
			breakChain: func(t *testing.T, f chainFixture) { softDeleteStoredAgent(t, f.s, tid("sca-state-sdi-parent")) },
			cause:      DenyCauseCeilingOrphaned,
			msg:        scaOrphanedDenyMsg,
		},
		{
			name: "soft-deleted deeper agent delegator",
			setup: func(t *testing.T, f chainFixture) string {
				chain(t, f, tid("sca-state-sdd-a"), tid("sca-state-sdd-b"), tid("sca-state-sdd-c"))
				return tid("sca-state-sdd-c")
			},
			live:       true,
			breakChain: func(t *testing.T, f chainFixture) { softDeleteStoredAgent(t, f.s, tid("sca-state-sdd-a")) },
			cause:      DenyCauseCeilingOrphaned,
			msg:        scaOrphanedDenyMsg,
		},
		{
			name: "missing agent delegator",
			setup: func(t *testing.T, f chainFixture) string {
				id := tid("sca-state-missing-child")
				createDCAgent(t, f.s, id, f.projectID, f.userID, AgentRoleFull)
				createDCEdge(t, f.s, store.DelegationPrincipalAgent, tid("sca-state-missing-parent"),
					store.DelegationPrincipalAgent, id, store.RoleScopeProject, f.projectID, string(AgentRoleFull))
				return id
			},
			cause: DenyCauseCeilingOrphaned,
			msg:   scaOrphanedDenyMsg,
		},
		{
			name: "suspended user delegator",
			setup: func(t *testing.T, f chainFixture) string {
				chain(t, f, tid("sca-state-susp-agent"))
				return tid("sca-state-susp-agent")
			},
			live:       true,
			breakChain: func(t *testing.T, f chainFixture) { setUserStatus(t, f.s, f.userID, store.UserStatusSuspended) },
			cause:      DenyCauseCeilingDelegatorLacksPermission,
			msg:        scaLacksPermissionDenyMsg,
		},
		{
			name: "suspended super-admin user delegator",
			setup: func(t *testing.T, f chainFixture) string {
				adminID := tid("sca-state-admin")
				createTestUserWithRole(t, f.s, adminID, "sca-state-admin@example.com", "admin", store.SystemRoleSuperAdmin)
				chain(t, chainFixture{s: f.s, projectID: f.projectID, userID: adminID}, tid("sca-state-admin-agent"))
				return tid("sca-state-admin-agent")
			},
			live: true,
			breakChain: func(t *testing.T, f chainFixture) {
				setUserStatus(t, f.s, tid("sca-state-admin"), store.UserStatusSuspended)
			},
			cause: DenyCauseCeilingDelegatorLacksPermission,
			msg:   scaLacksPermissionDenyMsg,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			f := chainFixture{s: s, projectID: tid("sca-state-proj"), userID: tid("sca-state-user")}
			createDCProject(t, s, f.projectID, "sca-state-project")
			createDCUser(t, s, f.userID, "sca-state-user@example.com", f.projectID, store.ProjectRoleOwner)
			sa := scaCreateSA(t, s, f.projectID)

			agentID := tc.setup(t, f)
			agent := dcAgentIdentity(agentID, f.projectID, AgentRoleFull)
			agentCtx := contextWithIdentity(ctx, agent)
			// Layer 1 of the gate is CheckAccess on the service account.
			layer1 := func() Decision {
				return srv.authzService.CheckAccess(agentCtx, agent, gcpServiceAccountResource(sa), ActionAssign)
			}
			if tc.live {
				live := layer1()
				require.True(t, live.Allowed, "the live chain passes the Hub policy layer: reason %q", live.Reason)
				tc.breakChain(t, f)
			}

			d := layer1()
			require.False(t, d.Allowed, "reason %q", d.Reason)
			assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "reason %q", d.Reason)
			assert.Equal(t, tc.cause, d.DenyCause, "reason %q", d.Reason)

			denial := srv.evaluateSAAssignment(agentCtx, nil, sa, SurfaceProjectDefault)
			require.NotNil(t, denial, "the non-live chain denies the assignment")
			assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
			assert.Equal(t, tc.msg, denial.msg)
		})
	}
}

// TestSAAssignUnrecordedChainHTTPBody covers the HTTP response when an agent
// whose only delegation edge has kind unrecorded creates a child with an
// assign-mode service account: a 403 with the unrecorded-provenance message
// and the structured SA-assign details.
func TestSAAssignUnrecordedChainHTTPBody(t *testing.T) {
	f := newLegacyFixture(t, "sca-unrec")
	edges, err := f.store.GetDelegationEdgesForDelegate(t.Context(), store.DelegationPrincipalAgent, f.legacy.ID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	require.Equal(t, store.EffectCeilingUnrecorded, edges[0].Kind)

	rec := f.createAsParent(t, f.agentToken(t, f.legacy.ID), f.assignBody("sca-unrec-c"))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeForbidden, apiErr.Code)
	assert.Equal(t, scaUnrecordedDenyMsg, apiErr.Message)
	assert.NotEqual(t, scaGenericDenyMsg, apiErr.Message)
	assert.Equal(t, gcpServiceAccountResource(f.sa).Type, apiErr.Details["resource_type"])
	assert.Equal(t, string(ActionAssign), apiErr.Details["denied_action"])
}
