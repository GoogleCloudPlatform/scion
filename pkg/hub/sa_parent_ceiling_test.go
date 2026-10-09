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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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

// ---- allow -----------------------------------------------------------------

func TestSAParentCeiling_UserSourceExactSAAllows(t *testing.T) {
	f := newSPCFixture(t)
	row := f.record(t, nil)
	d := f.eval(t)
	assertParentCeilingAllowed(t, d)
	assert.Equal(t, ParentProof{
		Permission:           "gcp_service_account.assign",
		SourcePrincipal:      ProvenancePrincipal{Kind: store.DelegationPrincipalUser, ID: f.assigner},
		SourceCredentialKind: store.SourceCredentialSession,
		AssignmentID:         row.ID,
		ServiceAccountID:     f.sa.ID,
	}, d.Proof)

	// The decision permission stays use; assign is only the parent proof.
	m, ok := ResourceParentCeilingFor("gcp_service_account.use")
	require.True(t, ok)
	assert.Equal(t, "gcp_service_account.use", m.DecisionPermission)
	assert.Equal(t, d.Proof.Permission, m.ParentPermission)
}

func TestSAParentCeiling_AgentSourceExactSAAllows(t *testing.T) {
	f := newSPCFixture(t)
	parent := f.assignModeAgent(t, "spc-parent", f.sa.ID, f.assigner)
	f.record(t, func(a *store.AgentServiceAccountAssignment) {
		a.SourcePrincipalKind = store.DelegationPrincipalAgent
		a.SourcePrincipalID = parent.ID
		a.SourceCredentialKind = store.SourceCredentialAgent
		a.SourceCredentialID = "jti-1"
		a.EffectCeiling = boundedCeiling("agent.create", "gcp_service_account.assign")
	})
	d := f.eval(t)
	assertParentCeilingAllowed(t, d)
	assert.Equal(t, ProvenancePrincipal{Kind: store.DelegationPrincipalAgent, ID: parent.ID}, d.Proof.SourcePrincipal)
}

// spcEnsureDevUser makes the local development user exist, active, with the
// project member role, and turns local development authority on.
func spcEnsureDevUser(t *testing.T, f *spcFixture) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.GetUser(ctx, DevUserID); err != nil {
		require.NoError(t, f.s.CreateUser(ctx, &store.User{
			ID: DevUserID, Email: "dev@localhost", DisplayName: "Dev", Role: "admin", Status: "active",
		}))
	}
	grantProjectRole(t, f.s, DevUserID, f.projectID, store.ProjectRoleMember)
	f.srv.authzService.setDevLocalAuthorityEnabled(true)
}

func devLocalRow(a *store.AgentServiceAccountAssignment) {
	a.SourcePrincipalKind = store.DelegationPrincipalUser
	a.SourcePrincipalID = DevUserID
	a.SourceCredentialKind = store.SourceCredentialDevLocal
}

func TestSAParentCeiling_DevLocalSourceExactSAAllows(t *testing.T) {
	f := newSPCFixture(t)
	spcEnsureDevUser(t, f)
	f.record(t, devLocalRow)
	assertParentCeilingAllowed(t, f.eval(t))
}

func TestSAParentCeiling_DevLocalSourceDeniedWhenDevAuthDisabled(t *testing.T) {
	f := newSPCFixture(t)
	spcEnsureDevUser(t, f)
	f.record(t, devLocalRow)
	f.srv.authzService.setDevLocalAuthorityEnabled(false)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingSourceNotAllowed)
}

// ---- assign authority ------------------------------------------------------

func TestSAParentCeiling_AssignOnOtherSADenies(t *testing.T) {
	f := newSPCFixture(t)
	ctx := context.Background()
	// The source holds assign only in another project, on that project's
	// account; the agent's account is in the fixture project.
	other := setupProjectWithBroker(t, f.s, "spc-other", "spc-other")
	outsider := tid("spc-outsider")
	createDCUser(t, f.s, outsider, "spc-outsider@test.com", other.ID, store.ProjectRoleMember)
	ensureHubMembership(ctx, f.s, outsider)
	otherSA := spcServiceAccount(t, f.s, "spc-other-sa", store.ScopeProject, other.ID, true)
	f.record(t, func(a *store.AgentServiceAccountAssignment) { a.SourcePrincipalID = outsider })

	allowed := f.srv.authzService.CheckAccess(ctx, NewAuthenticatedUser(outsider, "spc-outsider@test.com", "", "member", "api"),
		gcpServiceAccountResource(otherSA), ActionAssign)
	require.True(t, allowed.Allowed, "precondition: the source holds assign on its own project's account")
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingDelegatorLacksPermission)
}

func TestSAParentCeiling_SourceLacksAssignDenies(t *testing.T) {
	f := newSPCFixture(t)
	noAssign := tid("spc-no-assign")
	scaCreateDelegatorWithoutAssign(t, f.s, noAssign, "spc-no-assign@test.com", f.projectID)
	f.record(t, func(a *store.AgentServiceAccountAssignment) { a.SourcePrincipalID = noAssign })
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingDelegatorLacksPermission)
}

func TestSAParentCeiling_SourceLosesAssignAfterAssignmentDenies(t *testing.T) {
	f := newSPCFixture(t)
	ctx := context.Background()
	f.record(t, nil)
	assertParentCeilingAllowed(t, f.eval(t))

	_, err := f.s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.assigner)
	require.NoError(t, err)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingDelegatorLacksPermission)
}

// ---- source liveness -------------------------------------------------------

func TestSAParentCeiling_UserSourceSuspendedDenies(t *testing.T) {
	f := newSPCFixture(t)
	ctx := context.Background()
	f.record(t, nil)
	u, err := f.s.GetUser(ctx, f.assigner)
	require.NoError(t, err)
	u.Status = "suspended"
	require.NoError(t, f.s.UpdateUser(ctx, u))
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingOrphaned)
}

func TestSAParentCeiling_UserSourceDeletedDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, func(a *store.AgentServiceAccountAssignment) { a.SourcePrincipalID = tid("spc-gone-user") })
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingOrphaned)
}

func TestSAParentCeiling_AgentSourceDeletedDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, func(a *store.AgentServiceAccountAssignment) {
		a.SourcePrincipalKind = store.DelegationPrincipalAgent
		a.SourcePrincipalID = tid("spc-gone-agent")
		a.SourceCredentialKind = store.SourceCredentialAgent
		a.EffectCeiling = boundedCeiling("gcp_service_account.assign")
	})
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingOrphaned)
}

// ---- stale and missing provenance ------------------------------------------

func TestSAParentCeiling_AssignmentSAMismatchDenies(t *testing.T) {
	f := newSPCFixture(t)
	sa2 := spcServiceAccount(t, f.s, "spc-sa2", store.ScopeProject, f.projectID, true)
	f.record(t, func(a *store.AgentServiceAccountAssignment) { a.ServiceAccountID = sa2.ID })
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingProvenanceStale)
}

func TestSAParentCeiling_AppliedConfigSAMismatchDenies(t *testing.T) {
	f := newSPCFixture(t)
	ctx := context.Background()
	f.record(t, nil)
	sa2 := spcServiceAccount(t, f.s, "spc-sa2", store.ScopeProject, f.projectID, true)
	f.agent.AppliedConfig.GCPIdentity.ServiceAccountID = sa2.ID
	require.NoError(t, f.s.UpdateAgent(ctx, f.agent))
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingProvenanceStale)
}

func TestSAParentCeiling_RequestedSAMismatchDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	sa2 := spcServiceAccount(t, f.s, "spc-sa2", store.ScopeProject, f.projectID, true)
	d := f.srv.authzService.EvaluateServiceAccountParentCeiling(context.Background(), f.agent.ID, sa2.ID)
	assertParentCeilingDenied(t, d, DenyCauseCeilingProvenanceStale)
}

func TestSAParentCeiling_AssignmentInactiveDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	_, err := f.s.DeactivateAgentServiceAccountAssignments(context.Background(), f.agent.ID,
		store.Deactivation{Cause: store.EdgeDeactivationSACleared, OpID: "op-clear"})
	require.NoError(t, err)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingProvenanceMissing)
}

func TestSAParentCeiling_NoAssignmentDenies(t *testing.T) {
	f := newSPCFixture(t)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingProvenanceMissing)
}

func TestSAParentCeiling_UnrecordedAssignmentDenies(t *testing.T) {
	cases := map[string]func(*store.AgentServiceAccountAssignment){
		"provenance version 0": func(a *store.AgentServiceAccountAssignment) { a.ProvenanceVersion = 0 },
		"provenance version 2": func(a *store.AgentServiceAccountAssignment) { a.ProvenanceVersion = 2 },
		"unrecorded ceiling":   func(a *store.AgentServiceAccountAssignment) { a.EffectCeiling = store.EffectCeiling{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSPCFixture(t)
			f.record(t, mutate)
			assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingProvenanceMissing)
		})
	}
}

// spcTwoRowsStore reports two active assignments for every agent.
type spcTwoRowsStore struct {
	store.Store
}

func (s *spcTwoRowsStore) GetActiveAgentServiceAccountAssignments(ctx context.Context, agentID string) ([]store.AgentServiceAccountAssignment, error) {
	rows, err := s.Store.GetActiveAgentServiceAccountAssignments(ctx, agentID)
	if err != nil || len(rows) == 0 {
		return rows, err
	}
	return append(rows, rows[0]), nil
}

func TestSAParentCeiling_TwoActiveAssignmentsDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	d := f.authzOver(&spcTwoRowsStore{Store: f.s}).EvaluateServiceAccountParentCeiling(context.Background(), f.agent.ID, f.sa.ID)
	assertParentCeilingDenied(t, d, DenyCauseCeilingProvenanceAmbiguous)
}

func TestSAParentCeiling_InconsistentSourceDenies(t *testing.T) {
	cases := map[string]struct {
		mutate func(*store.AgentServiceAccountAssignment)
		cause  DenyCause
	}{
		"session source of kind agent": {func(a *store.AgentServiceAccountAssignment) {
			a.SourcePrincipalKind = store.DelegationPrincipalAgent
		}, DenyCauseCeilingOrphaned},
		"dev_local source not the dev user": {func(a *store.AgentServiceAccountAssignment) {
			a.SourceCredentialKind = store.SourceCredentialDevLocal
		}, DenyCauseCeilingOrphaned},
		"scheduler source without a revision": {func(a *store.AgentServiceAccountAssignment) {
			a.SourceCredentialKind = store.SourceCredentialScheduler
		}, DenyCauseCeilingOrphaned},
		"migration source": {func(a *store.AgentServiceAccountAssignment) {
			a.SourceCredentialKind = store.SourceCredentialSystemMigration
		}, DenyCauseCeilingSourceNotAllowed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSPCFixture(t)
			f.record(t, tc.mutate)
			assertParentCeilingDenied(t, f.eval(t), tc.cause)
		})
	}
}

// ---- effect ceiling --------------------------------------------------------

func uatRow(ids ...string) func(*store.AgentServiceAccountAssignment) {
	return func(a *store.AgentServiceAccountAssignment) {
		a.SourceCredentialKind = store.SourceCredentialUAT
		a.SourceCredentialID = tid("spc-token")
		a.EffectCeiling = boundedCeiling(ids...)
	}
}

func TestSAParentCeiling_AccessTokenCeilingWithoutAssignDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, uatRow("agent.create", "agent.read"))
	// The user's live role includes assign: the recorded ceiling decides.
	require.True(t, f.srv.authzService.CheckAccess(context.Background(),
		NewAuthenticatedUser(f.assigner, "spc-assigner@test.com", "", "member", "api"),
		gcpServiceAccountResource(f.sa), ActionAssign).Allowed)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingEffectExceeded)
}

func TestSAParentCeiling_AccessTokenSourceRevokedLaterEvaluatesUser(t *testing.T) {
	f := newSPCFixture(t)
	// The recorded access token does not exist any more: the assignment is
	// evaluated against the user's live authority within the recorded
	// ceiling.
	f.record(t, uatRow("agent.create", "gcp_service_account.assign"))
	assertParentCeilingAllowed(t, f.eval(t))
}

// ---- service account rows --------------------------------------------------

func TestSAParentCeiling_ProjectSA_OtherProjectDenies(t *testing.T) {
	f := newSPCFixture(t)
	other := setupProjectWithBroker(t, f.s, "spc-other", "spc-other")
	otherSA := spcServiceAccount(t, f.s, "spc-other-sa", store.ScopeProject, other.ID, true)
	agent := f.assignModeAgent(t, "spc-agent-other", otherSA.ID, f.assigner)
	f.agent, f.sa = agent, otherSA
	f.record(t, nil)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingResourceMissing)
}

func TestSAParentCeiling_HubSA_RequiresEnforceMode(t *testing.T) {
	f := newSPCFixture(t)
	hubSA := spcServiceAccount(t, f.s, "spc-hub-sa", store.ScopeHub, "hub", true)
	f.agent = f.assignModeAgent(t, "spc-agent-hub", hubSA.ID, f.assigner)
	f.sa = hubSA
	f.record(t, nil)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingResourceMissing)

	// A bare service with no mode wiring fails closed the same way.
	bare := NewAuthzService(f.s, slog.Default())
	setMode(f.srv, SAAssignCheckEnforce)
	assertParentCeilingDenied(t, bare.EvaluateServiceAccountParentCeiling(context.Background(), f.agent.ID, hubSA.ID),
		DenyCauseCeilingResourceMissing)
}

func TestSAParentCeiling_HubSA_HubMemberAssignRule(t *testing.T) {
	f := newSPCFixture(t)
	hubSA := spcServiceAccount(t, f.s, "spc-hub-sa", store.ScopeHub, "hub", true)
	f.agent = f.assignModeAgent(t, "spc-agent-hub", hubSA.ID, f.assigner)
	f.sa = hubSA
	f.record(t, nil)
	setMode(f.srv, SAAssignCheckEnforce)
	user := NewAuthenticatedUser(f.assigner, "spc-assigner@test.com", "", "member", "api")
	want := f.srv.authzService.CheckAccess(context.Background(), user, gcpServiceAccountResource(hubSA), ActionAssign)
	d := f.eval(t)
	assert.Equal(t, want.Allowed, d.Allowed, "the evaluator follows the hub-scoped assign rule (reason %q)", want.Reason)
	if !want.Allowed {
		assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, d.DenyCause)
	}
}

func TestSAParentCeiling_UserSA_Unreachable(t *testing.T) {
	f := newSPCFixture(t)
	userSA := spcServiceAccount(t, f.s, "spc-user-sa", store.ScopeUser, f.assigner, true)
	f.agent = f.assignModeAgent(t, "spc-agent-user-sa", userSA.ID, f.assigner)
	f.sa = userSA
	f.record(t, nil)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingResourceMissing)
}

func TestSAParentCeiling_UnverifiedSADenies(t *testing.T) {
	f := newSPCFixture(t)
	unverified := spcServiceAccount(t, f.s, "spc-unverified", store.ScopeProject, f.projectID, false)
	f.agent = f.assignModeAgent(t, "spc-agent-unverified", unverified.ID, f.assigner)
	f.sa = unverified
	f.record(t, nil)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingResourceMissing)
}

func TestSAParentCeiling_MissingSADenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	require.NoError(t, f.s.DeleteGCPServiceAccount(context.Background(), f.sa.ID))
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingResourceMissing)
}

// TestSAParentCeiling_PreconditionsSharedWithAssignGate drives the same
// service-account rows through the assignment gate and the evaluator: both
// refuse exactly the rows saAssignPolicyPreconditions refuses.
func TestSAParentCeiling_PreconditionsSharedWithAssignGate(t *testing.T) {
	type row struct {
		name     string
		scope    string
		other    bool // project-scoped in another project
		verified bool
		mode     string
	}
	rows := []row{
		{"own project", store.ScopeProject, false, true, SAAssignCheckOff},
		{"other project", store.ScopeProject, true, true, SAAssignCheckOff},
		{"unverified", store.ScopeProject, false, false, SAAssignCheckOff},
		{"user scoped", store.ScopeUser, false, true, SAAssignCheckOff},
		{"hub, off", store.ScopeHub, false, true, SAAssignCheckOff},
	}
	for i, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newSPCFixture(t)
			scopeID := f.projectID
			switch {
			case r.scope == store.ScopeHub:
				scopeID = "hub"
			case r.scope == store.ScopeUser:
				scopeID = f.assigner
			case r.other:
				scopeID = tid("spc-elsewhere")
			}
			sa := spcServiceAccount(t, f.s, "spc-shared-"+string(rune('a'+i)), r.scope, scopeID, r.verified)
			f.agent = f.assignModeAgent(t, "spc-shared-agent", sa.ID, f.assigner)
			f.sa = sa
			f.record(t, nil)
			setMode(f.srv, r.mode)
			stored, err := f.s.GetGCPServiceAccount(context.Background(), sa.ID)
			require.NoError(t, err)

			pre := saAssignPolicyPreconditions(stored, f.projectID, r.mode)
			d := f.eval(t)
			ctx := contextWithIdentity(context.Background(),
				NewAuthenticatedUser(f.assigner, "spc-assigner@test.com", "", "member", "api"))
			gate := f.srv.evaluateSAAssignment(ctx, nil, stored, f.projectID, SurfaceAgentCreate)
			if pre != nil {
				assertParentCeilingDenied(t, d, DenyCauseCeilingResourceMissing)
				assert.Contains(t, d.Reason, string(pre.Kind))
				require.NotNil(t, gate, "the gate refuses the same row")
			} else {
				assertParentCeilingAllowed(t, d)
				assert.Nil(t, gate)
			}
		})
	}

	// Neither surface keeps its own copy of the hub-scope rule.
	for _, file := range []string{"sa_assign_gate.go", "authz_resource_parent_ceiling.go"} {
		src, err := os.ReadFile(filepath.Join(".", file))
		require.NoError(t, err)
		body := string(src)
		assert.LessOrEqual(t, strings.Count(body, "store.ScopeHub"), 1, "%s: the hub-scope rule lives only in saAssignPolicyPreconditions", file)
		assert.Contains(t, body, "saAssignPolicyPreconditions(", "%s calls the shared rules", file)
	}
}

// ---- lookup errors ---------------------------------------------------------

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

func spcEvalOver(t *testing.T, f *spcFixture, fault *spcFaultStore) ParentCeilingDecision {
	t.Helper()
	fault.Store = f.s
	return f.authzOver(fault).EvaluateServiceAccountParentCeiling(context.Background(), f.agent.ID, f.sa.ID)
}

func TestSAParentCeiling_AgentLookupErrorDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	assertParentCeilingDenied(t, spcEvalOver(t, f, &spcFaultStore{agentID: f.agent.ID}), DenyCauseCeilingError)
}

func TestSAParentCeiling_AssignmentLookupErrorDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	// A lookup error is never read as "no assignment".
	assertParentCeilingDenied(t, spcEvalOver(t, f, &spcFaultStore{assign: true}), DenyCauseCeilingError)
}

func TestSAParentCeiling_SALookupErrorDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	assertParentCeilingDenied(t, spcEvalOver(t, f, &spcFaultStore{sa: true}), DenyCauseCeilingError)
}

func TestSAParentCeiling_SourceLookupErrorDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	assertParentCeilingDenied(t, spcEvalOver(t, f, &spcFaultStore{userID: f.assigner}), DenyCauseCeilingError)

	parent := f.assignModeAgent(t, "spc-parent", f.sa.ID, f.assigner)
	f.record(t, func(a *store.AgentServiceAccountAssignment) {
		a.SourcePrincipalKind = store.DelegationPrincipalAgent
		a.SourcePrincipalID = parent.ID
		a.SourceCredentialKind = store.SourceCredentialAgent
		a.EffectCeiling = boundedCeiling("gcp_service_account.assign")
	})
	assertParentCeilingDenied(t, spcEvalOver(t, f, &spcFaultStore{agentID: parent.ID}), DenyCauseCeilingError)
}

func TestSAParentCeiling_CheckAccessErrorDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	assertParentCeilingDenied(t, spcEvalOver(t, f, &spcFaultStore{bindings: true}), DenyCauseCeilingError)
}

// ---- contract shape --------------------------------------------------------

func TestResourceParentCeilingFor_Table(t *testing.T) {
	require.Len(t, resourceParentCeilings, 1, "the reviewed table has exactly one entry")
	m, ok := ResourceParentCeilingFor("gcp_service_account.use")
	require.True(t, ok)
	assert.Equal(t, ResourceParentCeiling{
		DecisionPermission: "gcp_service_account.use",
		ParentPermission:   "gcp_service_account.assign",
		ResourceType:       "gcp_service_account",
	}, m)
	for _, id := range []string{m.DecisionPermission, m.ParentPermission} {
		assert.Equal(t, m.ResourceType, registryResourceType(id), "%s is registered on the resource", id)
	}
	for _, id := range []string{"", "gcp_service_account.assign", "secret.use", "agent.create"} {
		_, ok := ResourceParentCeilingFor(id)
		assert.False(t, ok, "%q has no parent ceiling", id)
	}
}

func TestSAParentCeiling_UseHasNoAgentScopeAndNoSeededRole(t *testing.T) {
	_, s := testServer(t)
	for _, p := range permissions.Registry {
		if p.ID == "gcp_service_account.use" {
			assert.Empty(t, p.AgentScopes, "use keeps no static agent scope")
		}
	}
	defs, err := s.ListRoleDefinitions(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, defs)
	for _, d := range defs {
		assert.NotContains(t, d.Permissions, "gcp_service_account.use", "seeded role %s", d.Name)
	}
}

// TestSAParentCeiling_NotCalledFromTokenHandlers pins that the evaluator is
// not wired into a handler: the GCP token endpoints compose it with the
// actAs check in a later change, which removes this test.
func TestSAParentCeiling_NotCalledFromTokenHandlers(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			name == "authz_resource_parent_ceiling.go" {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.NotContains(t, string(src), "EvaluateServiceAccountParentCeiling", "%s must not call the evaluator", name)
	}
}
