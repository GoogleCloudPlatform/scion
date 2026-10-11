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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditingServer returns a create-ready server whose audit sink is captured.
func auditingServer(t *testing.T) (*Server, store.Store, *store.Project, *mockAuditLogger) {
	t.Helper()
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	audit := &mockAuditLogger{}
	srv.SetAuditLogger(audit)
	return srv, s, project, audit
}

// onlySAEvent fails unless exactly one SA record was produced. Exactly one
// matters in both directions: none is the audit gap, and duplicates would mean
// a surface emitting on top of what EvaluateActAs already emits, which
// double-counts every assignment in any report built on these records.
func onlySAEvent(t *testing.T, audit *mockAuditLogger) *store.SAAssignmentEvent {
	t.Helper()
	require.Len(t, audit.saEvents, 1, "expected exactly one SA assignment record")
	return audit.saEvents[0]
}

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

// scaUnrecordedDenyMsg is the Layer 1 body for DenyCauseCeilingUnrecorded,
// spelled out literally for the same reason as scaGenericDenyMsg.
const scaUnrecordedDenyMsg = "This agent cannot assign service accounts: its delegation chain includes an agent " +
	"created without recorded provenance (this agent or one of the agents that created it). " +
	"Have an authorized user reincarnate this agent, or recreate it directly (not from another agent)."

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

// enforceSAAssign switches the agent-assignment surface into enforce mode with
// the given checker, and wires a token generator.
//
// The generator is load-bearing rather than incidental: in enforce mode
// saAssignCheckerFor SUBSTITUTES the unavailable checker when
// gcpTokenGenerator is nil (sa_assign_gate.go:173). That substitute also
// denies — so a test that only asserted "the request was refused" would pass
// while the scripted checker was never consulted at all, which is the same
// class of false pass these tests exist to close. With the generator present
// the configured checker is used, and CallCount can prove it.
func enforceSAAssign(srv *Server, checker store.CallerPermissionChecker) {
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.saAssignCheckMode = SAAssignCheckEnforce
	srv.saAssignChecker = checker
}

// enforceHookIdentity is the same for the lifecycle-hook execution-identity
// surface, which has its own mode and its own checker by design.
func enforceHookIdentity(srv *Server, checker store.CallerPermissionChecker) {
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.hookIdentityCheckMode = SAAssignCheckEnforce
	srv.hookIdentityChecker = checker
}

// wiringSA seeds a service account for these tests.
//
// ⚠️ CreatedBy is a stranger on purpose. gcpServiceAccountResource maps
// CreatedBy to Resource.OwnerID and authz short-circuits on resource owner, so
// seeding the account under the caller would let the request pass the Hub
// policy layer for the wrong reason — and, worse for a DENY test, would leave
// the reader unable to tell which layer produced the refusal.
func wiringSA(t *testing.T, s store.Store, scope, scopeID, email string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:                 tid("wiring-sa-" + email),
		Scope:              scope,
		ScopeID:            scopeID,
		Email:              email,
		ProjectID:          tid("gcp-project"),
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          tid("some-other-user"),
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func setupHubScopedAssignTest(t *testing.T) *hubScopedAssignFixture {
	t.Helper()
	srv, s := bypassAgentsServer(t)
	ctx := context.Background()

	f := &hubScopedAssignFixture{srv: srv, store: s}

	f.owner = &store.User{
		ID:          tid("hsa-owner"),
		Email:       "hsa-owner@example.com",
		DisplayName: "HSA Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	f.member = &store.User{
		ID:          tid("hsa-member"),
		Email:       "hsa-member@example.com",
		DisplayName: "HSA Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	f.admin = &store.User{
		ID:          tid("hsa-admin"),
		Email:       "hsa-admin@example.com",
		DisplayName: "HSA Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	for _, u := range []*store.User{f.owner, f.member, f.admin} {
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
	}

	// Grant super-admin role binding for admin user (CO1 cutover: role bindings required)
	saRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: saRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	f.project = &store.Project{
		ID:        tid("hsa-project"),
		Name:      "HSA Project",
		Slug:      "hsa-project",
		OwnerID:   f.owner.ID,
		CreatedBy: f.owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	srv.seedProjectCreatorMembership(ctx, f.project)

	// Add member to the project members group
	membersGroup, err := s.GetGroupBySlug(ctx, "project:hsa-project:members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   f.member.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	return f
}

// setMode directly sets the server's saAssignCheckMode. Tests use this to
// toggle between enforce and off without rebuilding the server.
func setMode(srv *Server, mode string) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.saAssignCheckMode = mode
}

// builtInRoleRevision returns the declared revision of a built-in project role.
func builtInRoleRevision(t *testing.T, name string) int {
	t.Helper()
	for _, role := range BuiltInRoles() {
		if role.Name == name && role.ScopeType == store.RoleScopeProject {
			return role.Revision
		}
	}
	t.Fatalf("no built-in project role %q", name)
	return 0
}

// oracleProbe is one request's observable answer: everything a caller can see.
type oracleProbe struct {
	status int
	body   string
}

func probe(rec *httptest.ResponseRecorder) oracleProbe {
	return oracleProbe{status: rec.Code, body: rec.Body.String()}
}

// requireIndistinguishable compares two answers byte for byte, not by asserting
// that each matches some expected message.
//
// ⚠️ AND THE COLLAPSE TOOK SOMETHING AWAY FROM EVERY OTHER TEST HERE. The
// response no longer says WHICH branch produced the 400. So an assertion of the
// form "seed an unreachable account, expect 400 and this message" is now
// satisfied just as well by a fixture that never persisted the account at all —
// a mistyped ID, a seeding helper whose error was swallowed, a store reset
// between calls. That green used to be impossible to get by accident, because
// nonexistence said something different; it is now the default failure mode of
// a broken fixture.
//
// Two instruments, and they answer different questions:
//
//   - THE COLLAPSE HOLDS: same request, account present-but-unreachable versus
//     absent, answers identical. That is this function.
//   - THE PREDICATE STILL RUNS: the account must be genuinely PRESENT in both
//     arms and only its reachability varied — reachable is admitted, unreachable
//     is refused. Nothing in this file does that, and it is not this file's job.
//     TestBypassAgents_UpdateAgentServiceAccountChecks holds the tightest pair —
//     "another project is rejected" against "verified in-project is still
//     accepted", one fixture, reachability the only variable. Create's is
//     TestAgentCreate_HubScopedSA_AssignableByCreatorAndAdmin against
//     TestAgentCreate_OtherProjectSA_StillRejected. Those admitted arms are what
//     stop the refusals here being vacuous.
//
// Asserting the collapse without the second instrument somewhere is how a
// deleted scope check would pass review: every refusal test still refuses, for
// the wrong reason, and nothing says so.
//
// The property under test is that the two cases are THE SAME ANSWER, and an
// expected-message assertion does not test that: two branches can both contain
// msgSANotAvailableInProject and still differ in status, in error code, or in a
// field added later. Comparing the whole response tests the property directly
// and keeps testing it when the response shape changes — a future field that
// leaks the distinction fails here without anyone having to think of it.
func requireIndistinguishable(t *testing.T, missing, unreachable oracleProbe) {
	t.Helper()
	assert.Equal(t, missing.status, unreachable.status,
		"a nonexistent SA and an unreachable one must not differ by status code:\n"+
			"  nonexistent -> %d\n  unreachable -> %d\n"+
			"status alone is enough to enumerate IDs; the body need never be read",
		missing.status, unreachable.status)
	assert.Equal(t, missing.body, unreachable.body,
		"a nonexistent SA and an unreachable one must not differ by response body:\n"+
			"  nonexistent -> %s\n  unreachable -> %s",
		missing.body, unreachable.body)
}

// hubScopedAssignFixture sets up an environment suitable for testing
// hub-scoped SA assignment semantics.
type hubScopedAssignFixture struct {
	srv     *Server
	store   store.Store
	owner   *store.User // project owner, hub member
	member  *store.User // plain hub member, not project owner
	admin   *store.User // hub admin
	project *store.Project
}
