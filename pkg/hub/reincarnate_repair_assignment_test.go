// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite && (!hubshard || hubshard_6)

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A user's same-role reincarnate that repairs T's delegation chain
// (ptone/scion#3948) also re-records T's service-account assignment from the
// user, when the user passes the assignment gate for T's account. A user
// who does not pass it repairs the edge all the same; the assignment is left as is
// and the plan says so. Reincarnates that keep the edge leave the assignment
// alone.

// withAssignedSA gives T an assign-mode identity on the fixture's account
// and an active assignment for it whose source is another user, and returns
// that row.
func (f *repairFixture) withAssignedSA(t *testing.T) *store.AgentServiceAccountAssignment {
	t.Helper()
	target := mustGetAgent(t, f.s, f.target.ID)
	target.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
		MetadataMode:        store.GCPMetadataModeAssign,
		ServiceAccountID:    f.sa.ID,
		ServiceAccountEmail: f.sa.Email,
		ProjectID:           f.sa.ProjectID,
	}
	require.NoError(t, f.s.UpdateAgent(context.Background(), target))
	f.target = mustGetAgent(t, f.s, f.target.ID)
	return seedAssignment(t, f.s, f.target, tid("repair-original-assigner"), f.sa.ID)
}

// reincarnateWith sends a reincarnation of T by identity with body and
// returns the response.
func (f *repairFixture) reincarnateWith(t *testing.T, identity Identity, body ReincarnateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.target.ID, identity, body), f.target.ID)
	return rec
}

// planWarnings decodes the plan warnings of a reincarnate response.
func planWarnings(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp.Plan.Warnings
}

// assertNoRepairWarning asserts the plan carries no repair-assignment
// warning.
func assertNoRepairWarning(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for _, w := range planWarnings(t, rec) {
		assert.NotContains(t, w, reincarnateRepairAssignmentNotRecordedWarning)
	}
}

// assertAssignmentKept asserts old remains T's only active assignment.
func (f *repairFixture) assertAssignmentKept(t *testing.T, old *store.AgentServiceAccountAssignment) {
	t.Helper()
	rows := activeAssignments(t, f.s, f.target.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.Equal(t, old.ID, rows[0].ID, "the assignment is left as is")
	assert.Equal(t, old.SourcePrincipalID, rows[0].SourcePrincipalID, "its source is unchanged")
}

// The user passes the gate: the active assignment is a new row for the same
// account whose source is the user, with the provenance and ceiling of the
// new edge, and it is the only active row.
func TestReincarnateRepairReRecordsAssignmentFromRequester(t *testing.T) {
	f := newRepairFixture(t)
	oldEdge := seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
	old := f.withAssignedSA(t)
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(f.sa.Email)
	enforceSAAssign(f.srv, checker)

	f.reincarnateAs(t, f.session())
	f.assertReRecordedByUser(t, oldEdge.ID)
	require.Equal(t, 1, checker.CallCount(), "the assignment gate's actAs check runs")
	assert.Equal(t, f.sa.ID, checker.Calls()[0].TargetSAID)

	rows := activeAssignments(t, f.s, f.target.ID)
	require.Len(t, rows, 1, "exactly one active row")
	row := rows[0]
	assert.NotEqual(t, old.ID, row.ID, "the assignment is re-recorded")
	assert.Equal(t, f.sa.ID, row.ServiceAccountID, "for the same account")
	assert.Equal(t, store.SAAssignmentOriginReincarnate, row.Origin)
	edge := f.targetEdge(t)
	assert.Equal(t, edge.AuthorityProvenance, row.AuthorityProvenance, "the edge's provenance")
	assert.Equal(t, edge.EffectCeiling, row.EffectCeiling, "the edge's ceiling")
	assert.Equal(t, f.userID, row.SourcePrincipalID, "the user is the source")
	assertCeilingFromSource(t, f.srv, f.session(), row.EffectCeiling)

	assert.Equal(t, true, auditSummary(t, f.s, mutationTypeAgentReincarnateClaim, f.target.ID)["service_account_recorded"])
}

// The user does not pass the gate (the actAs check denies): a dry run and a
// real run report it as a plan warning, the edge is repaired, the
// assignment is left as is, and the claim audit records that no assignment
// was recorded.
func TestReincarnateRepairGateDenialKeepsAssignment(t *testing.T) {
	f := newRepairFixture(t)
	oldEdge := seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
	old := f.withAssignedSA(t)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker())

	rec := f.reincarnateWith(t, f.session(), ReincarnateAgentRequest{Handoff: "h", DryRun: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, planWarnings(t, rec), reincarnateRepairAssignmentNotRecordedWarning+": "+
		(&saAssignDenial{kind: saAssignDenyForbidden, msg: saAssignActAsDeniedMessage(f.sa)}).Error())
	f.assertAssignmentKept(t, old)

	rec = f.reincarnateWith(t, f.session(), ReincarnateAgentRequest{Handoff: "h"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	warnings := planWarnings(t, rec)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], reincarnateRepairAssignmentNotRecordedWarning)
	waitForReincarnationSettled(t, f.s, f.target.ID)

	f.assertReRecordedByUser(t, oldEdge.ID)
	f.assertAssignmentKept(t, old)
	assert.Equal(t, false, auditSummary(t, f.s, mutationTypeAgentReincarnateClaim, f.target.ID)["service_account_recorded"])
}

// gcpSAReadErrStore fails every read of one service account while its
// fault switch is armed.
type gcpSAReadErrStore struct {
	store.Store
	fault  *storeFaultSwitch
	failID string
}

func (s *gcpSAReadErrStore) GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error) {
	if id == s.failID && s.fault.Active() {
		return nil, errInjectedLookup
	}
	return s.Store.GetGCPServiceAccount(ctx, id)
}

// A store fault reading T's account answers 503 on a dry run and a real
// run, with nothing claimed and the assignment left as is.
func TestReincarnateRepairAssignmentLookupFault503(t *testing.T) {
	f := newRepairFixture(t)
	_, fault := installStoreFault(t, f.srv, func(inner store.Store, fault *storeFaultSwitch) *gcpSAReadErrStore {
		return &gcpSAReadErrStore{Store: inner, fault: fault, failID: f.sa.ID}
	})
	oldEdge := seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
	old := f.withAssignedSA(t)
	target := mustGetAgent(t, f.s, f.target.ID)
	fault.Arm()
	for _, dryRun := range []bool{true, false} {
		rec := f.reincarnateWith(t, f.session(), ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun})
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "agent's service account", "dryRun=%v", dryRun)
	}
	assertNothingClaimed(t, f.s, target, oldEdge)
	f.assertAssignmentKept(t, old)
}

// Reincarnates that keep T's edge leave the assignment alone: a user's
// reincarnate of a fully recorded chain, and a self or agent-requester
// reincarnate of an unrecorded one.
func TestReincarnateWithoutRepairKeepsAssignment(t *testing.T) {
	cases := map[string]func(t *testing.T, f *repairFixture) Identity{
		"user, recorded chain": func(t *testing.T, f *repairFixture) Identity {
			seedFullAgentEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
			return f.session()
		},
		"self, unrecorded chain": func(t *testing.T, f *repairFixture) Identity {
			seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
			return agentIdentityFor(f.target.ID, f.project.ID, ScopeAgentLifecycle)
		},
		"agent, unrecorded chain": func(t *testing.T, f *repairFixture) Identity {
			seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
			other := f.otherAgent(t, "repair-sa-requester")
			seedFullAgentEdge(t, f.s, store.DelegationPrincipalUser, f.userID, other)
			return fullRequesterFor(other.ID, f.project.ID)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRepairFixture(t)
			identity := setup(t, f)
			old := f.withAssignedSA(t)
			checker := store.NewFakeCallerPermissionChecker().AllowTarget(f.sa.Email)
			enforceSAAssign(f.srv, checker)

			f.reincarnateAs(t, identity)
			f.assertAssignmentKept(t, old)
			assert.Zero(t, checker.CallCount(), "no assignment gate runs")
			assert.NotContains(t, auditSummary(t, f.s, mutationTypeAgentReincarnateClaim, f.target.ID), "service_account_recorded")
		})
	}
}

// A repair of an agent without an assign-mode identity records no
// assignment.
func TestReincarnateRepairWithoutAssignedSARecordsNoAssignment(t *testing.T) {
	f := newRepairFixture(t)
	oldEdge := seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
	f.reincarnateAs(t, f.session())
	f.assertReRecordedByUser(t, oldEdge.ID)
	assert.Empty(t, activeAssignments(t, f.s, f.target.ID))
	assert.NotContains(t, auditSummary(t, f.s, mutationTypeAgentReincarnateClaim, f.target.ID), "service_account_recorded")
}

// A user's reincarnate that changes the role re-records the edge but not
// the assignment: the gate does not run and the row is left as is.
func TestReincarnateRoleChangeKeepsAssignment(t *testing.T) {
	f := newRepairFixture(t)
	oldEdge := seedFullAgentEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
	old := f.withAssignedSA(t)
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(f.sa.Email)
	enforceSAAssign(f.srv, checker)

	rec := f.reincarnateWith(t, f.session(), ReincarnateAgentRequest{Handoff: "h", Role: string(AgentRoleReadOnly)})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, f.s, f.target.ID)

	edge := f.targetEdge(t)
	assert.NotEqual(t, oldEdge.ID, edge.ID, "the role change re-records the edge")
	assert.Equal(t, string(AgentRoleReadOnly), edge.Role)
	f.assertAssignmentKept(t, old)
	assert.Zero(t, checker.CallCount(), "no assignment gate runs")
	sum := auditSummary(t, f.s, mutationTypeAgentReincarnateClaim, f.target.ID)
	assert.Equal(t, true, sum["re_recorded"])
	assert.NotContains(t, sum, "service_account_recorded")
}

// A repair that also patches the service account records the patched
// account through the patch's own gate; the repair gate for the old account
// does not run, so a user who may not assign the old account gets no
// warning about it.
func TestReincarnateRepairWithServiceAccountPatchRecordsPatchedAccount(t *testing.T) {
	f := newRepairFixture(t)
	seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
	old := f.withAssignedSA(t)
	next := patchTestSA(t, f.s, f.project.ID, true, f.userID)
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(next.Email)
	enforceSAAssign(f.srv, checker)

	rec := f.reincarnateWith(t, f.session(), ReincarnateAgentRequest{Handoff: "h", ServiceAccount: next.ID})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assertNoRepairWarning(t, rec)
	waitForReincarnationSettled(t, f.s, f.target.ID)

	require.NotZero(t, checker.CallCount())
	for _, c := range checker.Calls() {
		assert.Equal(t, next.ID, c.TargetSAID, "only the patched account is checked")
	}
	rows := activeAssignments(t, f.s, f.target.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.NotEqual(t, old.ID, rows[0].ID)
	assert.Equal(t, next.ID, rows[0].ServiceAccountID, "the patched account")
	assert.Equal(t, f.userID, rows[0].SourcePrincipalID)
	assert.Equal(t, true, auditSummary(t, f.s, mutationTypeAgentReincarnateClaim, f.target.ID)["service_account_recorded"])
}

// The account-side denial forms: an account that is not registered,
// and one that is not verified, leave the assignment as is with a warning
// naming the reason. Nothing is written to the response.
func TestResolveRepairAssignmentAccountDenials(t *testing.T) {
	f := newRepairFixture(t)
	f.withAssignedSA(t)
	unverified := patchTestSA(t, f.s, f.project.ID, false, f.userID)
	cases := map[string]struct {
		saID string
		want string
	}{
		"not registered": {saID: tid("repair-missing-sa"), want: msgSANotAvailableInProject},
		"not verified":   {saID: unverified.ID, want: identityNotVerifiedMessage(unverified)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			agent := mustGetAgent(t, f.s, f.target.ID)
			agent.AppliedConfig.GCPIdentity.ServiceAccountID = tc.saID
			auth := &reincarnateAuthority{
				DelegatorType: store.DelegationPrincipalUser,
				DelegatorID:   f.userID,
				Role:          string(AgentRoleFull),
				Ceiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
				Provenance:    recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialSession),
			}
			w := httptest.NewRecorder()
			ok := f.srv.resolveRepairAssignment(w, reincarnateRequest(t, agent.ID, f.session(), nil), agent, auth, false, nil)
			require.True(t, ok, "the repair proceeds")
			assert.Equal(t, http.StatusOK, w.Code, "nothing is written to the response")
			assert.Empty(t, w.Body.String())
			assert.Nil(t, auth.Assignment, "no assignment is recorded")
			assert.Contains(t, auth.AssignmentNotRecorded, reincarnateRepairAssignmentNotRecordedWarning)
			assert.Contains(t, auth.AssignmentNotRecorded, tc.want)
		})
	}
}

// moveRepairFixture is a move fixture whose agent has an unrecorded own
// edge from a session user U holding lifecycle and delegation in the
// project, an assign-mode identity on an account U registered, and an
// active assignment for it from another source.
func moveRepairFixture(t *testing.T) (*moveFixture, *store.User, Identity, *store.GCPServiceAccount, *store.AgentServiceAccountAssignment) {
	t.Helper()
	f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) { dst.AutoProvide = true })
	prepareMoveWorkerFixture(t, f)
	user := newReincarnateAuthzUser(t, f.s, tidSlugSafe(t.Name()))
	grantAgentLifecycleAtProject(t, f.s, user.ID, f.project.ID)
	grantAgentDelegationAtProject(t, f.s, user.ID, f.project.ID)
	seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, user.ID, f.agent)
	sa := patchTestSA(t, f.s, f.project.ID, true, user.ID)
	agent := mustGetAgent(t, f.s, f.agent.ID)
	agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
		MetadataMode:        store.GCPMetadataModeAssign,
		ServiceAccountID:    sa.ID,
		ServiceAccountEmail: sa.Email,
		ProjectID:           sa.ProjectID,
	}
	require.NoError(t, f.s.UpdateAgent(context.Background(), agent))
	f.agent = mustGetAgent(t, f.s, f.agent.ID)
	old := seedAssignment(t, f.s, f.agent, tid("move-original-assigner"), sa.ID)
	id := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	return f, user, id, sa, old
}

// A move by a user that repairs the chain re-records the assignment from
// the user, as an in-place reincarnate does (planReincarnateMove hands the
// same authority to startReincarnation).
func TestReincarnateMoveRepairReRecordsAssignment(t *testing.T) {
	f, user, id, sa, old := moveRepairFixture(t)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email))

	rec := httptest.NewRecorder()
	f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.agent.ID, id, ReincarnateAgentRequest{Handoff: "h", TargetBroker: f.dst.ID}), f.agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assertNoRepairWarning(t, rec)
	// The claim records the assignment before the 202.
	waitForReincarnationSettled(t, f.s, f.agent.ID)

	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.NotEqual(t, old.ID, rows[0].ID, "the assignment is re-recorded")
	assert.Equal(t, sa.ID, rows[0].ServiceAccountID)
	assert.Equal(t, user.ID, rows[0].SourcePrincipalID, "the user is the source")
}

// A move dry run by a user who does not pass the gate carries the warning
// on the move plan, and writes nothing.
func TestReincarnateMoveRepairGateDenialWarnsOnPlan(t *testing.T) {
	f, _, id, _, old := moveRepairFixture(t)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker())

	rec := httptest.NewRecorder()
	f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.agent.ID, id, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID}), f.agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	found := false
	for _, w := range planWarnings(t, rec) {
		if strings.Contains(w, reincarnateRepairAssignmentNotRecordedWarning) {
			found = true
		}
	}
	assert.True(t, found, "the move plan carries the warning: %s", rec.Body.String())
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, old.ID, rows[0].ID, "the assignment is left as is")
}
