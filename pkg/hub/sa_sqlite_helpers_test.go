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
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
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

// scaGenericDenyMsg is the literal generic 403 body, deliberately not a
// reference to saAssignGenericForbiddenMsg: tests that compare against this
// constant must fail if the production constant's value ever drifts, not
// just if it disappears.
const scaGenericDenyMsg = "You don't have permission to assign this GCP service account"

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

// mkHubScopedSA creates a hub-scoped SA with the given creator. Uses a
// stranger by default so tests that need to vary the creator are explicit.
func mkHubScopedSA(t *testing.T, s store.Store, createdBy string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:                 uuid.New().String(),
		Scope:              store.ScopeHub,
		ScopeID:            "test-hub-id",
		Email:              fmt.Sprintf("hub-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID:          "gcp-proj",
		DisplayName:        "Hub SA",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          createdBy,
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
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

// mintAssignErrStore fails the assignment lookup.
type mintAssignErrStore struct{ store.Store }

func activeAssignments(t *testing.T, s store.Store, agentID string) []store.AgentServiceAccountAssignment {
	t.Helper()
	rows, err := s.GetActiveAgentServiceAccountAssignments(context.Background(), agentID)
	require.NoError(t, err)
	return rows
}

// seedAssignment records an active assignment for agent from delegatorID.
func seedAssignment(t *testing.T, s store.Store, agent *store.Agent, delegatorID, saID string) *store.AgentServiceAccountAssignment {
	t.Helper()
	a := &store.AgentServiceAccountAssignment{
		AgentID: agent.ID, ProjectID: agent.ProjectID, ServiceAccountID: saID,
		Origin:              store.SAAssignmentOriginUpdate,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, delegatorID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(context.Background(), a))
	return a
}

// reincarnateClaimFor claims agent for a reincarnation directly, with sa as
// the assignment replacement (nil keeps it).
func reincarnateClaimFor(t *testing.T, srv *Server, agent *store.Agent, sa *store.AgentServiceAccountAssignment) error {
	t.Helper()
	now := time.Now()
	agent.ReincarnationUpdatedAt = &now
	rec := &store.AgentReincarnation{
		AgentID: agent.ID, FromGeneration: agent.Generation, ToGeneration: agent.Generation + 1,
		State: store.AgentReincarnationStatePending, PreviousAppliedConfig: agent.AppliedConfig,
	}
	return srv.reincarnateClaimTx(context.Background(), agent, rec, nil, sa, AuditActor{})
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

// spcFixture is a project with an assign-mode agent, the project-scoped
// service account it uses, and a project member who assigned it.
type spcFixture struct {
	srv       *Server
	s         store.Store
	projectID string
	assigner  string // a project member: holds gcp_service_account.assign
	sa        *store.GCPServiceAccount
	agent     *store.Agent
}

func newSPCFixture(t *testing.T) *spcFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	project := setupProjectWithBroker(t, s, "spc-proj", "spc-proj")
	assigner := tid("spc-assigner")
	createDCUser(t, s, assigner, "spc-assigner@test.com", project.ID, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, assigner)
	f := &spcFixture{srv: srv, s: s, projectID: project.ID, assigner: assigner}
	f.sa = spcServiceAccount(t, s, "spc-sa", store.ScopeProject, project.ID, true)
	f.agent = f.assignModeAgent(t, "spc-agent", f.sa.ID, assigner)
	setMode(srv, SAAssignCheckOff)
	return f
}

// spcServiceAccount registers a service account created by a stranger, so
// no owner shortcut can decide a check.
func spcServiceAccount(t *testing.T, s store.Store, name, scope, scopeID string, verified bool) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        tid(name),
		Scope:     scope,
		ScopeID:   scopeID,
		Email:     name + "@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		Verified:  verified,
		CreatedBy: tid("spc-stranger"),
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func assertParentCeilingDenied(t *testing.T, d ParentCeilingDecision, cause DenyCause) {
	t.Helper()
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
	assert.Equal(t, cause, d.DenyCause, "reason %q", d.Reason)
}

func assertParentCeilingAllowed(t *testing.T, d ParentCeilingDecision) {
	t.Helper()
	require.True(t, d.Allowed, "cause %q reason %q", d.DenyCause, d.Reason)
	assert.Equal(t, gcpServiceAccountAssignPermission, d.Proof.Permission)
	assert.Empty(t, d.DenyCause)
}

// spcTwoRowsStore reports two active assignments for every agent.
type spcTwoRowsStore struct {
	store.Store
}

// spcFaultStore fails one lookup.
type spcFaultStore struct {
	store.Store
	agentID  string // GetAgent of this ID fails
	assign   bool   // GetActiveAgentServiceAccountAssignments fails
	sa       bool   // GetGCPServiceAccount fails
	userID   string // GetUser of this ID fails
	bindings bool   // ListRoleBindingsForPrincipals fails
}

var errSPCInjected = errors.New("injected store fault")

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

func (s *mintAssignErrStore) GetActiveAgentServiceAccountAssignments(context.Context, string) ([]store.AgentServiceAccountAssignment, error) {
	return nil, errSPCInjected
}

// assignModeAgent stores an agent in the fixture project whose applied GCP
// identity assigns saID, with a recorded session edge from creator.
func (f *spcFixture) assignModeAgent(t *testing.T, name, saID, creator string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(name), Slug: name, Name: name, ProjectID: f.projectID,
		Phase: "running", CreatedBy: creator, OwnerID: creator, Ancestry: []string{creator},
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: string(AgentRoleFull),
			GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: saID,
			},
		},
	}
	require.NoError(t, f.s.CreateAgent(context.Background(), a))
	seedRecordedDelegationEdge(t, f.s, store.DelegationPrincipalUser, creator,
		store.DelegationPrincipalAgent, a.ID, store.RoleScopeProject, f.projectID, string(AgentRoleFull))
	return a
}

// record writes the agent's assignment: by default the assigner under a
// session credential with a principal ceiling, for the fixture account.
func (f *spcFixture) record(t *testing.T, mutate func(*store.AgentServiceAccountAssignment)) *store.AgentServiceAccountAssignment {
	t.Helper()
	a := &store.AgentServiceAccountAssignment{
		AgentID:          f.agent.ID,
		ProjectID:        f.projectID,
		ServiceAccountID: f.sa.ID,
		Origin:           store.SAAssignmentOriginCreateExplicit,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  store.DelegationPrincipalUser,
			SourcePrincipalID:    f.assigner,
			SourceCredentialKind: store.SourceCredentialSession,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	if mutate != nil {
		mutate(a)
	}
	require.NoError(t, f.s.ReplaceAgentServiceAccountAssignment(context.Background(), a))
	return a
}

func (f *spcFixture) eval(t *testing.T) ParentCeilingDecision {
	t.Helper()
	return f.srv.authzService.EvaluateServiceAccountParentCeiling(context.Background(), f.agent.ID, f.sa.ID)
}

// authzOver builds an AuthzService over s wired like the fixture's.
func (f *spcFixture) authzOver(s store.Store) *AuthzService {
	a := NewAuthzService(s, slog.Default())
	a.saIAMCheckMode = f.srv.saAssignCheckModeValue
	a.setDevLocalAuthorityEnabled(f.srv.authzService.devLocalAuthorityEnabled())
	return a
}

// setAppliedSA points the agent's applied identity at saID.
func (f *spcFixture) setAppliedSA(t *testing.T, saID string) {
	t.Helper()
	agent := mustGetAgent(t, f.s, f.agent.ID)
	agent.AppliedConfig.GCPIdentity.ServiceAccountID = saID
	require.NoError(t, f.s.UpdateAgent(context.Background(), agent))
}

func (s *spcTwoRowsStore) GetActiveAgentServiceAccountAssignments(ctx context.Context, agentID string) ([]store.AgentServiceAccountAssignment, error) {
	rows, err := s.Store.GetActiveAgentServiceAccountAssignments(ctx, agentID)
	if err != nil || len(rows) == 0 {
		return rows, err
	}
	return append(rows, rows[0]), nil
}

func (s *spcFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.agentID != "" && id == s.agentID {
		return nil, errSPCInjected
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *spcFaultStore) GetActiveAgentServiceAccountAssignments(ctx context.Context, agentID string) ([]store.AgentServiceAccountAssignment, error) {
	if s.assign {
		return nil, errSPCInjected
	}
	return s.Store.GetActiveAgentServiceAccountAssignments(ctx, agentID)
}

func (s *spcFaultStore) GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error) {
	if s.sa {
		return nil, errSPCInjected
	}
	return s.Store.GetGCPServiceAccount(ctx, id)
}

func (s *spcFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.userID != "" && id == s.userID {
		return nil, errSPCInjected
	}
	return s.Store.GetUser(ctx, id)
}

func (s *spcFaultStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.bindings {
		return nil, errSPCInjected
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}
