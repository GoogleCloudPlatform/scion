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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adoptionAdmin creates a hub system admin (admin role and the system
// super-admin binding).
func adoptionAdmin(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	id := tid(name)
	createTestUserWithRole(t, s, id, name+"@adopt.test", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), v), rec.Body.String())
}

func (f *legacyFixture) adoptionPreview(t *testing.T, admin *store.User, body map[string]interface{}) delegationAdoptionPreviewResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, admin, http.MethodPost, delegationAdoptionPath+"/previews", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp delegationAdoptionPreviewResponse
	decodeJSONBody(t, rec, &resp)
	return resp
}

func withFingerprint(body map[string]interface{}, p delegationAdoptionPreviewResponse) map[string]interface{} {
	out := map[string]interface{}{"planFingerprint": p.PlanFingerprint, "planId": p.PlanID}
	for k, v := range body {
		out[k] = v
	}
	return out
}

func adoptBody(agentIDs ...string) map[string]interface{} {
	return map[string]interface{}{"operation": "adopt", "scope": map[string]interface{}{"agentIds": agentIDs}}
}

func (f *legacyFixture) adoptionCommit(t *testing.T, admin *store.User, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, admin, http.MethodPost, delegationAdoptionPath+"/commits", body)
}

func (f *legacyFixture) adoptionRecords(t *testing.T) []*store.DelegationAdoption {
	t.Helper()
	recs, _, err := f.store.ListDelegationAdoptions(context.Background(), store.DelegationAdoptionFilter{})
	require.NoError(t, err)
	return recs
}

func (f *legacyFixture) adoptionAudits(t *testing.T, mutationType string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.store.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType})
	require.NoError(t, err)
	return recs
}

// The ceiling_unrecorded denial keeps its message byte-for-byte and adds
// the adoption details; no edge or ancestor ID is returned.
func TestUnrecordedDenialCarriesAdoptionDetails(t *testing.T) {
	f := newLegacyFixture(t, "adopt-details")
	token := f.agentToken(t, f.legacy.ID)
	rec := f.createAsParent(t, token, f.assignBody("adopt-details-c"))
	assertSAGateDenied(t, rec)
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, saAssignGenericForbiddenMsg, apiErr.Message)
	assert.Equal(t, "ceiling_unrecorded", apiErr.Details["deny_cause"])
	assert.Equal(t, "delegation_provenance_adoption", apiErr.Details["remediation"])
	assert.Equal(t, "/api/v1/admin/delegation-adoption", apiErr.Details["remediation_path"])
	assert.Equal(t, "gcp_service_account", apiErr.Details["resource_type"], "existing details are kept")
	body := rec.Body.String()
	assert.NotContains(t, body, f.legacy.ID)
	assert.NotContains(t, body, f.owner.ID)
	for _, e := range activeEdgesFor(t, f.store, f.legacy.ID) {
		assert.NotContains(t, body, e.ID)
	}

	// Other causes carry no adoption details.
	w := httptest.NewRecorder()
	writeForbiddenStructuredDenialCause(w, "x", "agent", ActionCreate, DeniedByDelegationCeiling, DenyCauseCeilingEffectExceeded)
	assert.NotContains(t, w.Body.String(), "remediation")
	w = httptest.NewRecorder()
	writeForbiddenDenialCause(w, "", "", DenyCauseCeilingUnrecorded)
	assert.Contains(t, w.Body.String(), `"remediation_path":"/api/v1/admin/delegation-adoption"`)
	assert.Contains(t, w.Body.String(), "Insufficient permissions")
	w = httptest.NewRecorder()
	writeForbiddenDenialCause(w, "", "", "")
	assert.NotContains(t, w.Body.String(), "details")
}

func TestDelegationAdoptionPreviewRequiresSystemAdmin(t *testing.T) {
	f := newLegacyFixture(t, "adopt-authz")
	admin := adoptionAdmin(t, f.store, "adopt-authz-admin")
	body := adoptBody(f.legacy.ID)

	// Agent token.
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, delegationAdoptionPath+"/previews", body, f.agentToken(t, f.legacy.ID))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// A non-admin user (project owner).
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPost, delegationAdoptionPath+"/previews", body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, delegationAdoptionPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// A user access token of the admin, at the handler.
	uat := NewScopedUserIdentityWithCeiling(authUser(admin), "", nil, "uat-"+admin.ID,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"hub.health.read"}})
	for _, h := range []http.HandlerFunc{f.srv.handleDelegationAdoptionPreviews, f.srv.handleDelegationAdoptionCommits} {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, delegationAdoptionPath+"/previews", bytes.NewReader(b)).
			WithContext(contextWithIdentity(context.Background(), uat))
		w := httptest.NewRecorder()
		h(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	}
	// No identity.
	w := httptest.NewRecorder()
	f.srv.handleDelegationAdoption(w, httptest.NewRequest(http.MethodGet, delegationAdoptionPath, nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	p := f.adoptionPreview(t, admin, body)
	assert.Equal(t, 1, p.Writes)
	assert.NotEmpty(t, p.PlanFingerprint)
	assert.Empty(t, f.adoptionRecords(t), "a preview writes nothing")
}

// Rows written after the boot snapshot (here: every row of the legacy
// fixture, seeded after Migrate) are adopted only by an explicit preview
// and commit.
func TestDelegationAdoptionCommitAdoptsPostSnapshotRowOnlyWhenPreviewed(t *testing.T) {
	f := newLegacyFixture(t, "adopt-post")
	f.defaultAssignSA(t)
	f.withAssignedSA(t, f.legacy)
	admin := adoptionAdmin(t, f.store, "adopt-post-admin")
	token := f.agentToken(t, f.legacy.ID)

	var status delegationAdoptionStatusResponse
	rec := doRequestAsUser(t, f.srv, admin, http.MethodGet, delegationAdoptionPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	decodeJSONBody(t, rec, &status)
	require.NotNil(t, status.Marker, "the boot migration completed on the empty database")
	assert.Equal(t, 1, status.NotInCohortCount)
	require.Len(t, status.NotInCohort, 1)
	assert.Equal(t, f.legacy.ID, status.NotInCohort[0].DelegateID)
	assertSAGateDenied(t, f.createAsParent(t, token, CreateAgentRequest{Name: "adopt-post-pre"}))

	body := adoptBody(f.legacy.ID)
	p := f.adoptionPreview(t, admin, body)
	require.Len(t, p.Hops, 1)
	assert.Equal(t, "adopt", p.Hops[0].Outcome)
	assert.Equal(t, compatIDs(t, AgentRoleFull, true), p.Hops[0].After.PermissionIDs)
	rec = f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var commit delegationAdoptionCommitResponse
	decodeJSONBody(t, rec, &commit)
	assert.Equal(t, 1, commit.Committed)
	require.Len(t, commit.Records, 1)
	assert.Equal(t, store.DelegationAdoptionOriginAdmin, commit.Records[0].Origin)
	assert.Equal(t, p.PlanID, commit.Records[0].CohortID)

	e := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	assert.Equal(t, store.SourceCredentialSystemMigration, e.SourceCredentialKind)
	assert.Equal(t, admin.ID, e.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindSession, e.InitiatorCredentialKind)
	assert.Empty(t, e.InitiatorCredentialID)
	_, childEdge := f.createdAgent(t, f.createAsParent(t, token, CreateAgentRequest{Name: "adopt-post-c"}), "adopt-post-c")
	assert.Equal(t, store.EffectCeilingBounded, childEdge.Kind)
}

func TestDelegationAdoptionCommitRejectsStalePlan(t *testing.T) {
	f := newLegacyFixture(t, "adopt-stale")
	admin := adoptionAdmin(t, f.store, "adopt-stale-admin")
	body := adoptBody(f.legacy.ID)
	p := f.adoptionPreview(t, admin, body)

	// The agent's applied role changes after the preview.
	ctx := context.Background()
	a, err := f.store.GetAgent(ctx, f.legacy.ID)
	require.NoError(t, err)
	a.AppliedConfig.AgentRole = string(AgentRoleBaseline)
	require.NoError(t, f.store.UpdateAgent(ctx, a))

	rec := f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeStaleAuthorizationPreview, decodeTargetAPIError(t, rec).Code)
	assert.Equal(t, store.EffectCeilingUnrecorded, activeEdgesFor(t, f.store, f.legacy.ID)[0].Kind)
	assert.Empty(t, f.adoptionRecords(t))

	// A forged fingerprint is stale too; a missing one is rejected.
	rec = f.adoptionCommit(t, admin, map[string]interface{}{"operation": "adopt", "scope": body["scope"], "planFingerprint": strings.Repeat("0", 64)})
	assert.Equal(t, http.StatusConflict, rec.Code)
	rec = f.adoptionCommit(t, admin, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelegationAdoptionCommitRechecksAdmin(t *testing.T) {
	f := newLegacyFixture(t, "adopt-recheck")
	admin := adoptionAdmin(t, f.store, "adopt-recheck-admin")
	body := adoptBody(f.legacy.ID)
	p := f.adoptionPreview(t, admin, body)

	delegationAdoptionCommitHook = func() {
		u, err := f.store.GetUser(context.Background(), admin.ID)
		require.NoError(t, err)
		u.Status = "suspended"
		require.NoError(t, f.store.UpdateUser(context.Background(), u))
	}
	t.Cleanup(func() { delegationAdoptionCommitHook = nil })
	rec := f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeMutationPermissionLost, decodeTargetAPIError(t, rec).Code)
	assert.Equal(t, store.EffectCeilingUnrecorded, activeEdgesFor(t, f.store, f.legacy.ID)[0].Kind)
	assert.Empty(t, f.adoptionRecords(t))
}

func TestDelegationAdoptionCommitIsAllOrNothing(t *testing.T) {
	f := newLegacyFixture(t, "adopt-atomic")
	c := f.seedLegacyAgent(t, "adopt-atomic-c", f.legacy, AgentRoleFull)
	admin := adoptionAdmin(t, f.store, "adopt-atomic-admin")
	body := adoptBody(c.ID)
	p := f.adoptionPreview(t, admin, body)
	require.Equal(t, 2, p.Writes)

	delegationAdoptionHopHook = func(i int) error {
		if i == 1 {
			return errors.New("injected write failure")
		}
		return nil
	}
	t.Cleanup(func() { delegationAdoptionHopHook = nil })
	rec := f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	for _, id := range []string{f.legacy.ID, c.ID} {
		edges := activeEdgesFor(t, f.store, id)
		require.Len(t, edges, 1)
		assert.Equal(t, store.EffectCeilingUnrecorded, edges[0].Kind, "no hop of a failed commit is written")
	}
	assert.Empty(t, f.adoptionRecords(t))
	assert.Empty(t, f.adoptionAudits(t, mutationTypeDelegationAdoption))

	delegationAdoptionHopHook = nil
	rec = f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, f.adoptionRecords(t), 2)
}

func TestDelegationAdoptionCommitWritesBeforeAfterAudit(t *testing.T) {
	f := newLegacyFixture(t, "adopt-audit")
	admin := adoptionAdmin(t, f.store, "adopt-audit-admin")
	original := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	body := adoptBody(f.legacy.ID)
	p := f.adoptionPreview(t, admin, body)
	rec := f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	adopted := activeEdgesFor(t, f.store, f.legacy.ID)[0]

	hops := f.adoptionAudits(t, mutationTypeDelegationAdoption)
	require.Len(t, hops, 1)
	a := hops[0]
	assert.Equal(t, admin.ID, a.ActorPrincipalID)
	assert.Equal(t, "delegation_edge", a.TargetType)
	assert.Equal(t, adopted.ID, a.TargetID)
	assert.Contains(t, a.BeforeSummary, original.ID)
	assert.Contains(t, a.BeforeSummary, `"provenance_version":0`)
	assert.Contains(t, a.AfterSummary, `"provenance_version":1`)
	assert.Contains(t, a.AfterSummary, `"ceiling_kind":"bounded"`)
	assert.Contains(t, a.AfterSummary, `"policy_version":1`)
	assert.NotContains(t, a.AfterSummary, "agent.create", "IDs are summarized by count and hash")

	summary := f.adoptionAudits(t, mutationTypeDelegationAdoptionCommit)
	require.Len(t, summary, 1)
	assert.Equal(t, p.PlanID, summary[0].TargetID)
	assert.Contains(t, summary[0].AfterSummary, `"hops":1`)
}

func (f *legacyFixture) recordFor(t *testing.T, agentID string) *store.DelegationAdoption {
	t.Helper()
	var found *store.DelegationAdoption
	for _, r := range f.adoptionRecords(t) {
		if r.DelegateID == agentID && r.Status != store.DelegationAdoptionReverted {
			found = r
		}
	}
	require.NotNil(t, found)
	return found
}

func TestDelegationAdoptionRevertPreviewAndCommit(t *testing.T) {
	f := newLegacyFixture(t, "adopt-revert")
	admin := adoptionAdmin(t, f.store, "adopt-revert-admin")
	original := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	runBootAdoption(t, f.store)
	rec := f.recordFor(t, f.legacy.ID)
	require.Equal(t, store.DelegationAdoptionAdopted, rec.Status)

	body := map[string]interface{}{"operation": "revert", "recordIds": []string{rec.ID}}
	p := f.adoptionPreview(t, admin, body)
	require.Len(t, p.Reverts, 1)
	assert.Equal(t, delegationadoption.RevertOutcomeRevert, p.Reverts[0].Outcome)
	assert.Equal(t, original.ID, p.Reverts[0].OriginalEdgeID)
	resp := f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())

	e := activeEdgesFor(t, f.store, f.legacy.ID)
	require.Len(t, e, 1)
	assert.Equal(t, original.ID, e[0].ID, "the original row is reactivated")
	assert.Equal(t, store.EffectCeilingUnrecorded, e[0].Kind)
	reverted, err := f.store.GetDelegationAdoption(context.Background(), rec.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DelegationAdoptionReverted, reverted.Status)
	old, err := f.store.GetDelegationEdge(context.Background(), rec.AdoptedEdgeID)
	require.NoError(t, err)
	assert.False(t, old.Active)
	assert.Equal(t, store.EdgeDeactivationAdoptionReverted, old.Cause)
	assert.Len(t, f.adoptionAudits(t, mutationTypeDelegationAdoptionRevert), 1)

	// Replaying the same commit is stale.
	assert.Equal(t, http.StatusConflict, f.adoptionCommit(t, admin, withFingerprint(body, p)).Code)
}

func TestRevertedAdoptionRestoresUnrecordedDenial(t *testing.T) {
	f := newLegacyFixture(t, "adopt-revert-deny")
	f.withAssignedSA(t, f.legacy)
	admin := adoptionAdmin(t, f.store, "adopt-revert-deny-admin")
	token := f.agentToken(t, f.legacy.ID)
	runBootAdoption(t, f.store)
	created := f.createAsParent(t, token, f.assignBody("adopt-revert-deny-1"))
	requireCreated(t, created.Code, created.Body.String())

	body := map[string]interface{}{"operation": "revert", "recordIds": []string{f.recordFor(t, f.legacy.ID).ID}}
	p := f.adoptionPreview(t, admin, body)
	require.Equal(t, http.StatusOK, f.adoptionCommit(t, admin, withFingerprint(body, p)).Code)

	assertSAGateDenied(t, f.createAsParent(t, token, f.assignBody("adopt-revert-deny-2")))
	f.assertGateUnrecorded(t, token, SurfaceAgentCreate)
}

func TestDelegationAdoptionRevertRefusesAmbiguousOriginal(t *testing.T) {
	f := newLegacyFixture(t, "adopt-ambig")
	admin := adoptionAdmin(t, f.store, "adopt-ambig-admin")
	ctx := context.Background()
	// An operational repair replaced the edge, and an older duplicate
	// inactive row matches as well: the original is ambiguous.
	orig := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	require.NoError(t, f.store.DeactivateDelegationEdge(ctx, orig.ID))
	dup := addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.legacy.ID, f.proj.ID)
	require.NoError(t, f.store.DeactivateDelegationEdge(ctx, dup))
	repaired := delegationadoption.AdoptedEdge(orig, compatIDs(t, AgentRoleFull, false), delegationadoption.Actor{})
	require.NoError(t, f.store.CreateDelegationEdge(ctx, repaired))
	runBootAdoption(t, f.store)
	rec := f.recordFor(t, f.legacy.ID)
	require.Equal(t, store.DelegationAdoptionRecognized, rec.Status)
	require.Empty(t, rec.OriginalEdgeID)

	body := map[string]interface{}{"operation": "revert", "recordIds": []string{rec.ID}}
	p := f.adoptionPreview(t, admin, body)
	require.Len(t, p.Reverts, 1)
	assert.Equal(t, delegationadoption.RevertOutcomeRefused, p.Reverts[0].Outcome)
	assert.Equal(t, delegationadoption.ReasonAmbiguousOriginal, p.Reverts[0].Reason)
	resp := f.adoptionCommit(t, admin, withFingerprint(body, p))
	assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	assert.Equal(t, repaired.ID, activeEdgesFor(t, f.store, f.legacy.ID)[0].ID)

	// The admin confirms the original row explicitly.
	body["confirmOriginalEdgeIds"] = map[string]string{rec.ID: orig.ID}
	p = f.adoptionPreview(t, admin, body)
	require.Equal(t, delegationadoption.RevertOutcomeRevert, p.Reverts[0].Outcome)
	resp = f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	assert.Equal(t, orig.ID, activeEdgesFor(t, f.store, f.legacy.ID)[0].ID)
}

func TestDelegationAdoptionStatusReportsPendingByReason(t *testing.T) {
	f := newLegacyFixture(t, "adopt-status")
	admin := adoptionAdmin(t, f.store, "adopt-status-admin")
	f.seedLegacyAgent(t, "adopt-status-none", nil, AgentRoleNone)
	f.seedLegacyAgent(t, "adopt-status-child", f.legacy, AgentRoleFull)
	ghostUser := tid("adopt-status-ghost-user")
	ghost := f.storeAgent(t, "adopt-status-ghost", []string{ghostUser}, AgentRoleFull)
	require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: ghostUser,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: ghost.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.proj.ID, Role: string(AgentRoleFull), Active: true,
	}))
	runBootAdoption(t, f.store)

	rec := doRequestAsUser(t, f.srv, admin, http.MethodGet, delegationAdoptionPath+"?status=excluded", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var status delegationAdoptionStatusResponse
	decodeJSONBody(t, rec, &status)
	require.NotNil(t, status.Marker)
	assert.True(t, status.Marker.Completed)
	assert.Equal(t, 2, status.Counts["adopted"])
	// The fixture's other agents have no edge (missing_edge).
	assert.Equal(t, 1, status.Reasons["role_none"])
	assert.Equal(t, 1, status.Reasons["root_missing"])
	assert.Equal(t, status.Counts["excluded"], status.Reasons["role_none"]+status.Reasons["root_missing"]+status.Reasons["missing_edge"])
	assert.Equal(t, status.Counts["excluded"], status.Total)
	for _, r := range status.Records {
		assert.Equal(t, store.DelegationAdoptionExcluded, r.Status)
	}
	assert.Zero(t, status.NotInCohortCount)

	rec = doRequestAsUser(t, f.srv, admin, http.MethodGet, delegationAdoptionPath+"?reason=root_missing", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	decodeJSONBody(t, rec, &status)
	require.Len(t, status.Records, 1)
	assert.Equal(t, ghost.ID, status.Records[0].DelegateID)
}
