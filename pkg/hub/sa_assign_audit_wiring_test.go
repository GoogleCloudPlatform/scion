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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// The §7 audit record, driven over HTTP, on all four surfaces
// ---------------------------------------------------------------------------
//
// audit_emission_test.go proves the sink writes the right bytes. These tests
// prove the sink is REACHED — that a real request on each surface produces a
// record, without the handler doing anything to make it happen.
//
// That inheritance is the design claim being tested. The record is emitted
// inside store.EvaluateActAs rather than at the four call sites, and the pure
// decision sequence is unexported, so a surface cannot reach a verdict without
// filing one. A per-handler emit would be correct on the day it was written and
// silently incomplete at the next surface; this arrangement is the difference
// between "all four surfaces were remembered" and "all four surfaces are
// covered by construction".
//
// Every case here runs with the gate INERT, which is the shipped state. The
// resulting record says allowed / check-disabled, and that IS the correct audit
// record for a hub that is not checking. It is not a placeholder to be
// suppressed until enforcement arrives: it is the evidence that no check was
// applied, which is the fact most worth having on record afterwards.

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

// ---------------------------------------------------------------------------
// Surface 1: agent create
// ---------------------------------------------------------------------------

func TestSAAudit_AgentCreate_RecordsTheAllow(t *testing.T) {
	srv, s, project, audit := auditingServer(t)
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "audit-create@p.iam.gserviceaccount.com")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "audit-create",
		ProjectID: project.ID,
		Task:      "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	ev := onlySAEvent(t, audit)
	assert.Equal(t, store.SAAssignmentDecision, ev.Type)
	assert.Equal(t, SurfaceAgentCreate, ev.Surface)
	assert.Equal(t, sa.ID, ev.TargetSAID)
	assert.Equal(t, sa.Email, ev.TargetSAEmail)
	assert.Equal(t, store.PermissionActAs, ev.Permission)
	require.NotNil(t, ev.Decision)
	assert.Equal(t, store.ActAsAllowed, *ev.Decision)

	// The mechanism is what makes an inert-gate allow readable as such. Without
	// it this record is indistinguishable from an IAM-approved assignment.
	assert.Equal(t, store.MechanismCheckDisabled, ev.Mechanism,
		"the record must say the allow came from the toggle, not from a check")
	assert.Nil(t, ev.CacheHit, "no cache exists; false would assert a miss that never happened")
}

func TestSAAudit_AgentCreate_RecordsTheDeny(t *testing.T) {
	srv, s, project, audit := auditingServer(t)
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "audit-create-deny@p.iam.gserviceaccount.com")
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().DenyTarget(sa.Email, "no actAs grant"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "audit-create-deny",
		ProjectID: project.ID,
		Task:      "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusForbidden, rec.Code)

	// The refusal is the record most worth having, and the easiest to lose: an
	// early return in a handler drops it without failing anything else.
	ev := onlySAEvent(t, audit)
	require.NotNil(t, ev.Decision)
	assert.Equal(t, store.ActAsDenied, *ev.Decision)
	assert.Equal(t, SurfaceAgentCreate, ev.Surface)
	assert.Equal(t, sa.ID, ev.TargetSAID)
	assert.NotEmpty(t, ev.Reason, "a denial with no reason cannot be acted on")
}

// ---------------------------------------------------------------------------
// Surface 2: agent PATCH
// ---------------------------------------------------------------------------

func TestSAAudit_AgentPatch_RecordsItsOwnSurface(t *testing.T) {
	srv, s, project, audit := auditingServer(t)
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "audit-patch@p.iam.gserviceaccount.com")

	agent := &store.Agent{
		ID:        tid("audit-patch-agent"),
		Slug:      "audit-patch-agent",
		Name:      "audit-patch-agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseCreated),
		CreatedBy: tid("some-other-user"),
		OwnerID:   tid("some-other-user"),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode":      store.GCPMetadataModeAssign,
				"service_account_id": sa.ID,
			},
		})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// PATCH must NOT be labelled agent-create. The two have different blast
	// radii — swapping the identity of a live agent is not the same event as
	// choosing one at creation — and a report that cannot tell them apart
	// cannot answer the question anyone actually asks of it.
	ev := onlySAEvent(t, audit)
	assert.Equal(t, SurfaceAgentPatch, ev.Surface)
	assert.Equal(t, sa.ID, ev.TargetSAID)
}

// ---------------------------------------------------------------------------
// Surface 3: project default
// ---------------------------------------------------------------------------

// P10 CHANGED: TestSAAudit_ProjectDefault_RecordsADecision. Project-default
// assignment now runs the full authorization gate (authorizeSAAssignment),
// which produces a DECISION record via EvaluateActAs, not a binding record.
// The ruling changed in P10: the project operator selected an available
// default but did not grant every future creator permission to act as it.
func TestSAAudit_ProjectDefault_RecordsADecision(t *testing.T) {
	srv, s, project, audit := auditingServer(t)
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "audit-default@p.iam.gserviceaccount.com")

	ctx := context.Background()
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	project.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	// No gcp_identity in the request: the account comes from project settings.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "audit-default",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	// P10: the project-default path now produces a decision record, not a
	// binding record, because authorizeSAAssignment runs EvaluateActAs.
	ev := onlySAEvent(t, audit)
	assert.Equal(t, store.SAAssignmentDecision, ev.Type,
		"P10: project-default now produces a decision record via authorizeSAAssignment")
	assert.Equal(t, SurfaceProjectDefault, ev.Surface)
	assert.Equal(t, sa.ID, ev.TargetSAID)
	assert.Equal(t, sa.Email, ev.TargetSAEmail)
	assert.Equal(t, store.PermissionActAs, ev.Permission)
	require.NotNil(t, ev.Decision, "P10: decision record must carry a verdict")
	assert.Equal(t, store.ActAsAllowed, *ev.Decision)

	// With the gate inert (default), the mechanism records check-disabled.
	assert.Equal(t, store.MechanismCheckDisabled, ev.Mechanism,
		"the record must say the allow came from the toggle, not from a check")
}

// TestSAAudit_ProjectDefault_StillBindsWhenTheSinkIsMissing. The emit sits
// inside the agent-creation path on a site the project rules protect. A nil
// sink must warn and proceed, never refuse — failing closed there would turn a
// logging misconfiguration into an outage.
func TestSAAudit_ProjectDefault_StillBindsWhenTheSinkIsMissing(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetAuditLogger(nil)
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "audit-nosink@p.iam.gserviceaccount.com")

	ctx := context.Background()
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	project.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "audit-nosink",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	got, err := s.GetAgentBySlug(ctx, project.ID, "audit-nosink")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID,
		"the binding must still happen when there is nowhere to record it")
}

// ---------------------------------------------------------------------------
// Surface 4: lifecycle-hook execution identity
// ---------------------------------------------------------------------------

// TestSAAudit_LifecycleHook_RecordsAcrossThePackageBoundary is the case that
// justifies the sink interface living in pkg/store. This surface is validated
// in pkg/lifecyclehooks, which cannot import pkg/hub, so the only way it can
// emit is through a store-side interface that the hub's logger satisfies. If
// that ever drifts, this surface loses its audit trail quietly — it would fall
// back to the nil-sink warning path rather than fail to build.
func TestSAAudit_LifecycleHook_RecordsAcrossThePackageBoundary(t *testing.T) {
	srv, s := testServer(t)
	audit := &mockAuditLogger{}
	srv.SetAuditLogger(audit)
	sa := wiringSA(t, s, store.ScopeHub, "test-hub-id", "audit-hook@p.iam.gserviceaccount.com")

	req := validCreateRequest()
	req.ExecutionIdentity = sa.ID
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/lifecycle-hooks", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	ev := onlySAEvent(t, audit)
	assert.Equal(t, store.SAAssignmentDecision, ev.Type)
	assert.Equal(t, "hook-execution-identity", ev.Surface)
	assert.Equal(t, sa.ID, ev.TargetSAID)
	require.NotNil(t, ev.Decision)
	assert.Equal(t, store.ActAsAllowed, *ev.Decision)
	assert.Equal(t, store.MechanismCheckDisabled, ev.Mechanism)
}

func TestSAAudit_LifecycleHook_RecordsTheDeny(t *testing.T) {
	srv, s := testServer(t)
	audit := &mockAuditLogger{}
	srv.SetAuditLogger(audit)
	sa := wiringSA(t, s, store.ScopeHub, "test-hub-id", "audit-hook-deny@p.iam.gserviceaccount.com")
	enforceHookIdentity(srv, store.NewFakeCallerPermissionChecker().DenyTarget(sa.Email, "no actAs grant"))

	req := validCreateRequest()
	req.ExecutionIdentity = sa.ID
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/lifecycle-hooks", req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	ev := onlySAEvent(t, audit)
	require.NotNil(t, ev.Decision)
	assert.Equal(t, store.ActAsDenied, *ev.Decision)
	assert.Equal(t, "hook-execution-identity", ev.Surface)
}

// ---------------------------------------------------------------------------
// Inheritance
// ---------------------------------------------------------------------------

// TestSAAudit_EveryDecisionSurfaceNamesItself. Four surfaces, four distinct
// labels, none of them empty — checked together because the failure mode is
// collective: a fifth surface added later reaches EvaluateActAs and inherits
// the record automatically, but nothing forces it to inherit a NAME. The
// store-side guard substitutes "surface-unnamed" rather than an empty string
// precisely so that omission is alertable, and this test is what would catch a
// surface arriving with the placeholder.
func TestSAAudit_EveryDecisionSurfaceNamesItself(t *testing.T) {
	seen := map[string]bool{}

	srv, s, project, audit := auditingServer(t)
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "audit-names@p.iam.gserviceaccount.com")
	require.Equal(t, http.StatusCreated,
		doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name: "audit-names", ProjectID: project.ID, Task: "t",
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID,
			},
		}).Code)
	seen[onlySAEvent(t, audit).Surface] = true

	hookSrv, hookStore := testServer(t)
	hookAudit := &mockAuditLogger{}
	hookSrv.SetAuditLogger(hookAudit)
	hookSA := wiringSA(t, hookStore, store.ScopeHub, "test-hub-id", "audit-names-hook@p.iam.gserviceaccount.com")
	req := validCreateRequest()
	req.ExecutionIdentity = hookSA.ID
	require.Equal(t, http.StatusCreated,
		doRequest(t, hookSrv, http.MethodPost, "/api/v1/admin/lifecycle-hooks", req).Code)
	seen[onlySAEvent(t, hookAudit).Surface] = true

	for surface := range seen {
		assert.NotEmpty(t, surface)
		assert.NotEqual(t, store.SurfaceUnnamed, surface,
			"a surface reached the gate without naming itself")
	}
	assert.Len(t, seen, 2, "two surfaces exercised here must not share a label")
}
