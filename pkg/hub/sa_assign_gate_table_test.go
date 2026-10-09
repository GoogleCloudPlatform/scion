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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// gateTableOutcome is what one evaluateSAAssignment call produced, reduced to
// the parts a caller can observe: allow, or the denial's rendering kind, its
// message and its adoption-details cause, plus how many times the actAs
// checker was consulted.
type gateTableOutcome struct {
	Allowed    bool
	Kind       saAssignDenialKind
	Msg        string
	Cause      DenyCause
	CheckCalls int
}

// gateTableEvaluate is the single call site of the gate in this table, so a
// signature change touches one line here and no expectation.
func gateTableEvaluate(srv *Server, ctx context.Context, sa *store.GCPServiceAccount, projectID string) *saAssignDenial {
	return srv.evaluateSAAssignment(ctx, nil, sa, projectID, SurfaceAgentCreate)
}

// TestSAAssignGate_BeforeAfterTable pins every observable outcome of the
// service-account assignment gate across SA scope, IAM check mode, caller,
// hub policy and actAs result. The literal table is the contract: any change
// to the gate's outcomes, its order, or its response text fails here.
func TestSAAssignGate_BeforeAfterTable(t *testing.T) {
	f := setupHubScopedAssignTest(t)
	ctx := context.Background()
	s := f.store

	projectSA := wiringSA(t, s, store.ScopeProject, f.project.ID, "gate-table-project@proj.iam.gserviceaccount.com")
	hubSA := mkHubScopedSA(t, s, tid("gate-table-stranger"))

	// A hub member who is not a member of the project.
	outsider := &store.User{
		ID: tid("gate-table-outsider"), Email: "gate-table-outsider@example.com",
		DisplayName: "Outsider", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, outsider))
	ensureHubMembership(ctx, s, outsider.ID)

	// An agent whose creator no longer exists (orphaned chain).
	orphanAgentID := tid("gate-table-orphan-agent")
	createDCAgent(t, s, orphanAgentID, f.project.ID, tid("gate-table-gone-user"), AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, tid("gate-table-gone-user"),
		store.DelegationPrincipalAgent, orphanAgentID, store.RoleScopeProject, f.project.ID, string(AgentRoleFull))

	// An agent created by the project owner under recorded provenance, in no
	// GCP identity mode (so it has no GCP identity of its own).
	liveAgentID := tid("gate-table-live-agent")
	createDCAgent(t, s, liveAgentID, f.project.ID, f.owner.ID, AgentRoleFull)
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, f.owner.ID,
		store.DelegationPrincipalAgent, liveAgentID, store.RoleScopeProject, f.project.ID, string(AgentRoleFull))

	member := NewAuthenticatedUser(f.member.ID, f.member.Email, f.member.DisplayName, "member", "api")
	memberNoEmail := NewAuthenticatedUser(f.member.ID, "", f.member.DisplayName, "member", "api")
	admin := NewAuthenticatedUser(f.admin.ID, f.admin.Email, f.admin.DisplayName, "admin", "api")
	outsiderID := NewAuthenticatedUser(outsider.ID, outsider.Email, outsider.DisplayName, "member", "api")

	const (
		modeOff         = "off"
		modeEnforce     = "enforce"
		modeNoGenerator = "enforce-no-generator"
	)
	const (
		actAllow = "allow"
		actDeny  = "deny"
		actFail  = "fail"
	)

	actAsDeniedMsg := func(email string) string {
		return "You don't have permission to use this GCP service account (" +
			store.PermissionActAs + " is required on " + email + ")"
	}

	rows := []struct {
		name     string
		sa       *store.GCPServiceAccount
		mode     string
		identity Identity
		actAs    string
		nilAuthz bool
		want     gateTableOutcome
	}{
		{name: "01 project SA, mode off, member", sa: projectSA, mode: modeOff, identity: member,
			want: gateTableOutcome{Allowed: true}},
		{name: "02 project SA, enforce, member, actAs allowed", sa: projectSA, mode: modeEnforce, identity: member, actAs: actAllow,
			want: gateTableOutcome{Allowed: true, CheckCalls: 1}},
		{name: "03 project SA, enforce, member, actAs denied", sa: projectSA, mode: modeEnforce, identity: member, actAs: actDeny,
			want: gateTableOutcome{Kind: saAssignDenyForbidden, Msg: actAsDeniedMsg(projectSA.Email), CheckCalls: 1}},
		{name: "04 project SA, enforce, member, actAs check failed", sa: projectSA, mode: modeEnforce, identity: member, actAs: actFail,
			want: gateTableOutcome{Kind: saAssignDenyForbidden, CheckCalls: 1,
				Msg: "Could not verify your permission to use this GCP service account because the check did not complete; try again"}},
		{name: "05 project SA, enforce without generator, member", sa: projectSA, mode: modeNoGenerator, identity: member,
			want: gateTableOutcome{Kind: saAssignDenyForbidden,
				Msg: "GCP permission checking is not available on this Hub; service-account assignment is refused until it is configured"}},
		{name: "06 project SA, mode off, non-member", sa: projectSA, mode: modeOff, identity: outsiderID,
			want: gateTableOutcome{Kind: saAssignDenyForbiddenStructured, Msg: scaGenericDenyMsg}},
		{name: "07 hub SA, mode off, admin", sa: hubSA, mode: modeOff, identity: admin,
			want: gateTableOutcome{Kind: saAssignDenyForbidden, Msg: "Hub-scoped service account assignment requires gcpIamCheckMode=enforce"}},
		{name: "08 hub SA, enforce, admin, actAs allowed", sa: hubSA, mode: modeEnforce, identity: admin, actAs: actAllow,
			want: gateTableOutcome{Allowed: true, CheckCalls: 1}},
		{name: "09 hub SA, enforce, hub member, actAs allowed", sa: hubSA, mode: modeEnforce, identity: member, actAs: actAllow,
			want: gateTableOutcome{Allowed: true, CheckCalls: 1}},
		{name: "10 project SA, enforce, agent with orphaned chain", sa: projectSA, mode: modeEnforce, actAs: actAllow,
			identity: dcAgentIdentity(orphanAgentID, f.project.ID, AgentRoleFull),
			want: gateTableOutcome{Kind: saAssignDenyForbiddenStructured,
				Msg: "This agent cannot assign service accounts: a principal in its delegation chain " +
					"(the user or agent that created it, or one of their creators) does not exist. " +
					"Ask an admin to recreate the agent under a current user."}},
		{name: "11 hub SA, mode off, member", sa: hubSA, mode: modeOff, identity: member,
			want: gateTableOutcome{Kind: saAssignDenyForbidden, Msg: "Hub-scoped service account assignment requires gcpIamCheckMode=enforce"}},
		{name: "12 project SA, mode off, no identity", sa: projectSA, mode: modeOff, identity: nil,
			want: gateTableOutcome{Kind: saAssignDenyUnauthorized}},
		{name: "13 project SA, mode off, no authz service", sa: projectSA, mode: modeOff, identity: member, nilAuthz: true,
			want: gateTableOutcome{Kind: saAssignDenyForbiddenStructured, Msg: scaGenericDenyMsg}},
		{name: "14 nil SA", sa: nil, mode: modeOff, identity: member,
			want: gateTableOutcome{Kind: saAssignDenyForbidden}},
		{name: "15 project SA, enforce, agent without a GCP identity", sa: projectSA, mode: modeEnforce, actAs: actAllow,
			identity: dcAgentIdentity(liveAgentID, f.project.ID, AgentRoleFull),
			want: gateTableOutcome{Kind: saAssignDenyForbidden,
				Msg: "Your identity cannot be granted permission to use this GCP service account"}},
		{name: "16 project SA, enforce, user without email", sa: projectSA, mode: modeEnforce, identity: memberNoEmail, actAs: actAllow,
			want: gateTableOutcome{Kind: saAssignDenyForbidden,
				Msg: "Your identity cannot be granted permission to use this GCP service account"}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			checker := store.NewFakeCallerPermissionChecker()
			if row.sa != nil {
				switch row.actAs {
				case actAllow:
					checker.AllowTarget(row.sa.Email)
				case actDeny:
					checker.DenyTarget(row.sa.Email, "scripted deny")
				case actFail:
					checker.FailTarget(row.sa.Email, errors.New("scripted transport failure"))
				}
			}
			switch row.mode {
			case modeOff:
				setMode(f.srv, SAAssignCheckOff)
			case modeEnforce:
				enforceSAAssign(f.srv, checker)
			case modeNoGenerator:
				f.srv.mu.Lock()
				f.srv.saAssignCheckMode = SAAssignCheckEnforce
				f.srv.saAssignChecker = checker
				f.srv.gcpTokenGenerator = nil
				f.srv.mu.Unlock()
			}
			if row.nilAuthz {
				saved := f.srv.authzService
				f.srv.authzService = nil
				t.Cleanup(func() { f.srv.authzService = saved })
			}

			rctx := ctx
			if row.identity != nil {
				rctx = contextWithIdentity(ctx, row.identity)
			}
			denial := gateTableEvaluate(f.srv, rctx, row.sa, f.project.ID)

			got := gateTableOutcome{Allowed: denial == nil, CheckCalls: checker.CallCount()}
			if denial != nil {
				got.Kind, got.Msg, got.Cause = denial.kind, denial.msg, denial.cause
			}
			assert.Equal(t, row.want, got)
		})
	}
}

// TestSAAssignGate_DefaultLadderBeforeAfterTable pins the default ladder's
// refusals that come before the gate (resolveDefaultSAAssignmentCore): an
// unreachable or unverified default fails with the ladder's own text, and a
// hub-scoped default with the IAM check off fails with the gate's hub-mode
// denial.
func TestSAAssignGate_DefaultLadderBeforeAfterTable(t *testing.T) {
	f := setupHubScopedAssignTest(t)
	ctx := contextWithIdentity(context.Background(),
		NewAuthenticatedUser(f.member.ID, f.member.Email, f.member.DisplayName, "member", "api"))
	s := f.store
	setMode(f.srv, SAAssignCheckOff)

	otherProjectSA := wiringSA(t, s, store.ScopeProject, tid("gate-ladder-other-project"), "gate-ladder-other@proj.iam.gserviceaccount.com")
	unverified := wiringSA(t, s, store.ScopeProject, f.project.ID, "gate-ladder-unverified@proj.iam.gserviceaccount.com")
	unverified.Verified = false
	unverified.VerificationStatus = ""
	require.NoError(t, s.UpdateGCPServiceAccount(context.Background(), unverified))
	hubSA := mkHubScopedSA(t, s, tid("gate-ladder-stranger"))

	_, err := f.srv.resolveDefaultSAAssignmentCore(ctx, nil, f.project.ID, otherProjectSA.ID, SurfaceProjectDefault, defaultTierProject)
	require.Error(t, err)
	assert.Equal(t, "project default GCP service account is not available in this project; update the project's default GCP identity setting", err.Error())

	_, err = f.srv.resolveDefaultSAAssignmentCore(ctx, nil, f.project.ID, unverified.ID, SurfaceProjectDefault, defaultTierProject)
	require.Error(t, err)
	assert.Equal(t, "project default GCP service account is not verified; verify it before it can be assigned to agents", err.Error())

	_, err = f.srv.resolveDefaultSAAssignmentCore(ctx, nil, f.project.ID, hubSA.ID, SurfaceHubDefault, defaultTierHub)
	require.Error(t, err)
	var denial *saAssignDenial
	require.ErrorAs(t, err, &denial)
	assert.Equal(t, saAssignDenyForbidden, denial.kind)
	assert.Equal(t, "Hub-scoped service account assignment requires gcpIamCheckMode=enforce", denial.msg)

	cfg, err := f.srv.resolveDefaultSAAssignmentCore(ctx, nil, f.project.ID,
		wiringSA(t, s, store.ScopeProject, f.project.ID, "gate-ladder-ok@proj.iam.gserviceaccount.com").ID,
		SurfaceProjectDefault, defaultTierProject)
	require.NoError(t, err)
	assert.Equal(t, store.GCPMetadataModeAssign, cfg.MetadataMode)
}

// TestSAAssignPolicyPreconditions_Table covers the shared service-account
// rules on their own: reachability (another project's account, a user-scoped
// account), verification, and the hub-scope mode rule, in that order.
func TestSAAssignPolicyPreconditions_Table(t *testing.T) {
	verified := func(scope, scopeID string) *store.GCPServiceAccount {
		return &store.GCPServiceAccount{Scope: scope, ScopeID: scopeID, Verified: true,
			VerificationStatus: store.GCPVerificationVerified}
	}
	unverified := verified(store.ScopeProject, "p1")
	unverified.Verified = false
	unverifiedOther := verified(store.ScopeProject, "p2")
	unverifiedOther.VerificationStatus = store.GCPVerificationUnverified
	unverifiedHub := verified(store.ScopeHub, "hub")
	unverifiedHub.Verified = false

	cases := []struct {
		name string
		sa   *store.GCPServiceAccount
		mode string
		want saAssignPreconditionKind // "" = no refusal
	}{
		{"nil account", nil, SAAssignCheckEnforce, saAssignPreconditionUnreachable},
		{"own project, off", verified(store.ScopeProject, "p1"), SAAssignCheckOff, ""},
		{"own project, enforce", verified(store.ScopeProject, "p1"), SAAssignCheckEnforce, ""},
		{"other project", verified(store.ScopeProject, "p2"), SAAssignCheckEnforce, saAssignPreconditionUnreachable},
		{"other project and unverified", unverifiedOther, SAAssignCheckEnforce, saAssignPreconditionUnreachable},
		{"user scoped", verified(store.ScopeUser, "u1"), SAAssignCheckEnforce, saAssignPreconditionUnreachable},
		{"unverified", unverified, SAAssignCheckEnforce, saAssignPreconditionUnverified},
		{"hub, enforce", verified(store.ScopeHub, "hub"), SAAssignCheckEnforce, ""},
		{"hub, off", verified(store.ScopeHub, "hub"), SAAssignCheckOff, saAssignPreconditionHubMode},
		{"hub, empty mode", verified(store.ScopeHub, "hub"), "", saAssignPreconditionHubMode},
		{"hub, unverified, off", unverifiedHub, SAAssignCheckOff, saAssignPreconditionUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := saAssignPolicyPreconditions(tc.sa, "p1", tc.mode)
			if tc.want == "" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Kind)
		})
	}
}
