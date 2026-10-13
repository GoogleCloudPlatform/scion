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
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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
	t.Run("never existed", func(t *testing.T) {
		f := newSPCFixture(t)
		f.record(t, func(a *store.AgentServiceAccountAssignment) {
			a.SourcePrincipalKind = store.DelegationPrincipalAgent
			a.SourcePrincipalID = tid("spc-gone-agent")
			a.SourceCredentialKind = store.SourceCredentialAgent
			a.EffectCeiling = boundedCeiling("gcp_service_account.assign")
		})
		assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingOrphaned)
	})

	// The source agent's row is marked deleted while its edge stays
	// active, so only the source deletion check can deny.
	t.Run("soft deleted", func(t *testing.T) {
		f := newSPCFixture(t)
		parent := f.assignModeAgent(t, "spc-parent", f.sa.ID, f.assigner)
		f.record(t, func(a *store.AgentServiceAccountAssignment) {
			a.SourcePrincipalKind = store.DelegationPrincipalAgent
			a.SourcePrincipalID = parent.ID
			a.SourceCredentialKind = store.SourceCredentialAgent
			a.SourceCredentialID = "jti-1"
			a.EffectCeiling = boundedCeiling("agent.create", "gcp_service_account.assign")
		})
		assertParentCeilingAllowed(t, f.eval(t))
		markAgentDeleted(t, f.s, parent.ID)
		require.NotEmpty(t, activeEdgesFor(t, f.s, parent.ID), "precondition: the source's edge stays active")
		assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingOrphaned)
	})
}

// markAgentDeleted sets the agent row's DeletedAt and nothing else: its
// edges and assignment stay active.
func markAgentDeleted(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	agent := mustGetAgent(t, s, agentID)
	agent.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	require.False(t, mustGetAgent(t, s, agentID).DeletedAt.IsZero(), "precondition: the row reads as deleted")
}

// TestSAParentCeiling_DeletedAgentDenies: the evaluated agent's row is
// marked deleted while its assignment stays active, so only the agent
// deletion check can deny.
func TestSAParentCeiling_DeletedAgentDenies(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	assertParentCeilingAllowed(t, f.eval(t))
	markAgentDeleted(t, f.s, f.agent.ID)
	require.Len(t, activeAssignments(t, f.s, f.agent.ID), 1, "precondition: the assignment stays active")
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingOrphaned)
}

// ---- stale and missing provenance ------------------------------------------

// spcMismatchedIDs returns account IDs that differ from the fixture
// account: the unrelated account newMismatchFixture registers, and two that
// share a strict prefix with it (one it extends, one that extends it), so
// only an exact comparison refuses them. Account IDs are UUIDs, so the
// prefix-shaped IDs cannot be registered.
func spcMismatchedIDs() map[string]string {
	id := tid("spc-sa")
	return map[string]string{
		"other account":         tid("spc-sa2"),
		"prefix of the account": id[:len(id)-1],
		"account with a suffix": id + "0",
	}
}

// newMismatchFixture is newSPCFixture plus a second registered, verified
// account in the project (spcMismatchedIDs' "other account").
func newMismatchFixture(t *testing.T) *spcFixture {
	t.Helper()
	f := newSPCFixture(t)
	require.Equal(t, tid("spc-sa"), f.sa.ID)
	spcServiceAccount(t, f.s, "spc-sa2", store.ScopeProject, f.projectID, true)
	return f
}

func TestSAParentCeiling_AssignmentSAMismatchDenies(t *testing.T) {
	for name, id := range spcMismatchedIDs() {
		t.Run(name, func(t *testing.T) {
			f := newMismatchFixture(t)
			f.record(t, func(a *store.AgentServiceAccountAssignment) { a.ServiceAccountID = id })
			assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingProvenanceStale)
		})
	}
}

func TestSAParentCeiling_AppliedConfigSAMismatchDenies(t *testing.T) {
	for name, id := range spcMismatchedIDs() {
		t.Run(name, func(t *testing.T) {
			f := newMismatchFixture(t)
			f.record(t, nil)
			assertParentCeilingAllowed(t, f.eval(t))
			f.setAppliedSA(t, id)
			assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingProvenanceStale)
		})
	}
}

func TestSAParentCeiling_RequestedSAMismatchDenies(t *testing.T) {
	for name, id := range spcMismatchedIDs() {
		t.Run(name, func(t *testing.T) {
			f := newMismatchFixture(t)
			f.record(t, nil)
			assertParentCeilingAllowed(t, f.eval(t))
			d := f.srv.authzService.EvaluateServiceAccountParentCeiling(context.Background(), f.agent.ID, id)
			assertParentCeilingDenied(t, d, DenyCauseCeilingProvenanceStale)
		})
	}
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
	assert.Empty(t, hubScopeRuleViolations(t, hubScopeRuleSources(t)))
}

// hubScopeRuleFiles are the two surfaces that apply the service-account
// rules: the assignment gate and the parent-ceiling evaluator.
var hubScopeRuleFiles = []string{"sa_assign_gate.go", "authz_resource_parent_ceiling.go"}

// hubScopeRuleSources reads hubScopeRuleFiles from the package directory.
func hubScopeRuleSources(t *testing.T) map[string]string {
	t.Helper()
	out := make(map[string]string, len(hubScopeRuleFiles))
	for _, file := range hubScopeRuleFiles {
		src, err := os.ReadFile(filepath.Join(".", file))
		require.NoError(t, err)
		out[file] = string(src)
	}
	return out
}

// hubScopeRuleViolations parses srcs (file name to source) and reports
// every way the hub-scope rule is not held by saAssignPolicyPreconditions
// alone. A rule site is a == or != comparison against SAAssignCheckEnforce,
// or a store.ScopeHub that is an operand of a binary expression or sits in
// an if condition. Every rule site must be inside the FuncDecl
// saAssignPolicyPreconditions; across all files there must be exactly one
// of each kind, in the same if condition; and every file must call
// saAssignPolicyPreconditions. A switch that only picks text per scope
// (saAssignPolicyDeniedMessage) holds no rule site.
func hubScopeRuleViolations(t *testing.T, srcs map[string]string) []string {
	t.Helper()
	const rulesFunc = "saAssignPolicyPreconditions"
	isIdent := func(e ast.Expr, name string) bool {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name == name
		case *ast.SelectorExpr:
			return x.Sel.Name == name
		}
		return false
	}
	isScopeHub := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ScopeHub" {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		return ok && x.Name == "store"
	}

	var violations []string
	var enforceIfs, scopeIfs []*ast.IfStmt
	files := make([]string, 0, len(srcs))
	for file := range srcs {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, srcs[file], 0)
		require.NoError(t, err, file)
		callsRules := false
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			inRules := fn.Name.Name == rulesFunc
			var stack []ast.Node
			site := func(kind string, n ast.Node) *ast.IfStmt {
				var cond *ast.IfStmt
				for i := len(stack) - 1; i >= 0 && cond == nil; i-- {
					if is, ok := stack[i].(*ast.IfStmt); ok && is.Cond != nil &&
						is.Cond.Pos() <= n.Pos() && n.End() <= is.Cond.End() {
						cond = is
					}
				}
				if !inRules {
					violations = append(violations, fmt.Sprintf("%s: %s in %s, outside %s",
						fset.Position(n.Pos()), kind, fn.Name.Name, rulesFunc))
				}
				return cond
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				switch x := n.(type) {
				case *ast.CallExpr:
					if isIdent(x.Fun, rulesFunc) && !inRules {
						callsRules = true
					}
				case *ast.BinaryExpr:
					if (x.Op == token.EQL || x.Op == token.NEQ) &&
						(isIdent(x.X, "SAAssignCheckEnforce") || isIdent(x.Y, "SAAssignCheckEnforce")) {
						if is := site("comparison against SAAssignCheckEnforce", x); inRules {
							enforceIfs = append(enforceIfs, is)
						}
					}
					if isScopeHub(x.X) || isScopeHub(x.Y) {
						if is := site("store.ScopeHub comparison", x); inRules {
							scopeIfs = append(scopeIfs, is)
						}
					}
				case *ast.SelectorExpr:
					// A store.ScopeHub inside an if condition that the
					// comparison case above has not counted already.
					if !isScopeHub(x) {
						break
					}
					if bin, ok := stack[len(stack)-1].(*ast.BinaryExpr); ok && (isScopeHub(bin.X) || isScopeHub(bin.Y)) {
						break
					}
					for _, anc := range stack {
						if is, ok := anc.(*ast.IfStmt); ok && is.Cond != nil &&
							is.Cond.Pos() <= x.Pos() && x.End() <= is.Cond.End() {
							if is := site("store.ScopeHub in an if condition", x); inRules {
								scopeIfs = append(scopeIfs, is)
							}
							break
						}
					}
				}
				stack = append(stack, n)
				return true
			})
		}
		if !callsRules {
			violations = append(violations, file+": does not call "+rulesFunc)
		}
	}
	if len(enforceIfs) != 1 || len(scopeIfs) != 1 {
		violations = append(violations, fmt.Sprintf("%s holds %d comparisons against SAAssignCheckEnforce and %d store.ScopeHub conditions, want exactly 1 of each",
			rulesFunc, len(enforceIfs), len(scopeIfs)))
	} else if enforceIfs[0] == nil || enforceIfs[0] != scopeIfs[0] {
		violations = append(violations, rulesFunc+": the hub-scope and check-mode tests are not one if condition")
	}
	return violations
}

// TestSAParentCeiling_HubScopeRuleGuardCatchesCopies runs the source guard
// of TestSAParentCeiling_PreconditionsSharedWithAssignGate over edited
// copies of the two files: each copy, move or removal of the hub-scope
// rule is reported, and the unedited sources are not.
func TestSAParentCeiling_HubScopeRuleGuardCatchesCopies(t *testing.T) {
	const (
		gate    = "sa_assign_gate.go"
		ceiling = "authz_resource_parent_ceiling.go"
		rule    = "if sa.Scope == store.ScopeHub && iamCheckMode != SAAssignCheckEnforce {"
		anchor  = "\t// Preconditions: the shared service-account rules.\n"
	)
	base := hubScopeRuleSources(t)
	require.Equal(t, 1, strings.Count(base[gate], rule), "the rule's text in %s", gate)
	require.Equal(t, 1, strings.Count(base[gate], anchor), "the preconditions call in evaluateSAAssignment")
	require.Empty(t, hubScopeRuleViolations(t, base), "unedited sources")

	appendFunc := func(body string) func(map[string]string) {
		return func(m map[string]string) {
			m[ceiling] += "\nfunc spcGuardCopy(sa *store.GCPServiceAccount, mode string) bool {\n" + body + "\n}\n"
		}
	}
	inEvaluate := func(stmt string) func(map[string]string) {
		return func(m map[string]string) {
			m[gate] = strings.Replace(m[gate], anchor, stmt+"\n"+anchor, 1)
		}
	}
	removeRule := func(m map[string]string) {
		m[gate] = strings.Replace(m[gate], rule, "if false {", 1)
	}
	cases := []struct {
		name string
		edit []func(map[string]string)
	}{
		{"copy in the ceiling file", []func(map[string]string){
			appendFunc("\treturn sa.Scope == store.ScopeHub && mode != SAAssignCheckEnforce")}},
		{"copy in evaluateSAAssignment", []func(map[string]string){
			inEvaluate("\tif sa.Scope == store.ScopeHub && s.saAssignCheckModeValue() != SAAssignCheckEnforce {\n\t\treturn &saAssignDenial{kind: saAssignDenyForbidden}\n\t}")}},
		{"reversed form", []func(map[string]string){
			appendFunc("\treturn store.ScopeHub == sa.Scope && SAAssignCheckEnforce != mode")}},
		{"switch form", []func(map[string]string){
			appendFunc("\tswitch sa.Scope {\n\tcase store.ScopeHub:\n\t\tif mode != SAAssignCheckEnforce {\n\t\t\treturn false\n\t\t}\n\t}\n\treturn true")}},
		{"scope as a call argument in an if", []func(map[string]string){
			appendFunc("\tif sa != nil && slices.Contains([]string{store.ScopeHub}, sa.Scope) {\n\t\treturn false\n\t}\n\treturn true")}},
		{"removal", []func(map[string]string){removeRule}},
		{"move into evaluateSAAssignment", []func(map[string]string){removeRule,
			inEvaluate("\tif sa.Scope == store.ScopeHub && mode != SAAssignCheckEnforce {\n\t\treturn &saAssignDenial{kind: saAssignDenyForbidden}\n\t}")}},
		{"ceiling stops calling the rules", []func(map[string]string){func(m map[string]string) {
			m[ceiling] = strings.ReplaceAll(m[ceiling], "saAssignPolicyPreconditions(", "saAssignPolicyPreconditionsCopy(")
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srcs := maps.Clone(base)
			for _, edit := range tc.edit {
				edit(srcs)
			}
			require.NotEqual(t, base, srcs, "the edit applied")
			assert.NotEmpty(t, hubScopeRuleViolations(t, srcs))
		})
	}
}

// ---- lookup errors ---------------------------------------------------------

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
		if d.Name == store.SystemRoleSuperAdmin {
			// The super-admin role is seeded with every registry
			// permission by construction.
			continue
		}
		assert.NotContains(t, d.Permissions, "gcp_service_account.use", "seeded role %s", d.Name)
	}
}

// TestSAParentCeiling_NotCalledFromTokenHandlers pins that the evaluator is
// not wired into a handler: the GCP token endpoints compose it with the
// actAs check in a later change, which removes this test.
func TestSAParentCeiling_NotCalledFromTokenHandlers(t *testing.T) {
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; guards EvaluateServiceAccountParentCeiling callers.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; guards EvaluateServiceAccountParentCeiling callers.
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; guards EvaluateServiceAccountParentCeiling callers.
	entries, err := readGuardScanDirs(".")
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
