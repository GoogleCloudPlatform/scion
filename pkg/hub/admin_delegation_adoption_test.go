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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
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

	f.srv.delegationAdoptionCommitHook = func() {
		u, err := f.store.GetUser(context.Background(), admin.ID)
		require.NoError(t, err)
		u.Status = "suspended"
		require.NoError(t, f.store.UpdateUser(context.Background(), u))
	}
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

	f.srv.delegationAdoptionHopHook = func(i int) error {
		if i == 1 {
			return errors.New("injected write failure")
		}
		return nil
	}
	rec := f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	for _, id := range []string{f.legacy.ID, c.ID} {
		edges := activeEdgesFor(t, f.store, id)
		require.Len(t, edges, 1)
		assert.Equal(t, store.EffectCeilingUnrecorded, edges[0].Kind, "no hop of a failed commit is written")
	}
	assert.Empty(t, f.adoptionRecords(t))
	assert.Empty(t, f.adoptionAudits(t, mutationTypeDelegationAdoption))

	f.srv.delegationAdoptionHopHook = nil
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
	assert.Contains(t, summary[0].AfterSummary, `"hops":1,"covered_records":0`)
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

// adoptionHandlerRequest calls an adoption handler directly with the given
// identity and credential context, without the authentication middleware.
// A zero credential leaves the context without a credential context.
func adoptionHandlerRequest(h http.HandlerFunc, path string, identity Identity, credential *CredentialContext, body map[string]interface{}) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	ctx := contextWithIdentity(context.Background(), identity)
	if credential != nil {
		ctx = contextWithCredentialContext(ctx, *credential)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// ensureDevUserRecord stores the trusted local development user as an
// active admin, as a dev-auth hub does.
func ensureDevUserRecord(t *testing.T, s store.Store) {
	t.Helper()
	if _, err := s.GetUser(context.Background(), DevUserID); err == nil {
		return
	}
	createTestUserWithRole(t, s, DevUserID, "dev@adopt.test", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
}

// The adoption admin endpoints require an interactive or dev credential.
// The credential kind comes from the request's credential context; an
// admin identity under any other credential kind, or without a credential
// context, is refused on both preview and commit.
func TestDelegationAdoptionRequiresInteractiveOrDevCredential(t *testing.T) {
	f := newLegacyFixture(t, "adopt-cred")
	admin := adoptionAdmin(t, f.store, "adopt-cred-admin")
	ensureDevUserRecord(t, f.store)
	devAgent := f.seedLegacyAgent(t, "adopt-cred-dev", nil, AgentRoleFull)
	require.True(t, f.srv.authzService.devLocalAuthorityEnabled())

	adminUser := authUser(admin)
	devUser := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@adopt.test"})
	uat := NewScopedUserIdentityWithCeiling(adminUser, "", nil, "uat-"+admin.ID,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"hub.health.read"}})
	federated := NewFederatedUserIdentity("https://issuer.adopt.test", "sub", admin.Email, "Fed", "admin", nil)
	agent := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: f.legacy.ID}, ProjectID: f.proj.ID}}
	// A federated identity whose own ID holds the system super-admin
	// binding, so only the federated-identity check refuses it.
	federatedAdmin := &bindableFederatedAdmin{UserIdentity: adminUser, issuerURL: "https://issuer.adopt.test"}
	require.True(t, f.srv.authzService.IsSystemAdmin(context.Background(), federatedAdmin.ID()))

	cred := func(c CredentialContext) *CredentialContext { return &c }
	denied := []struct {
		name       string
		identity   Identity
		credential *CredentialContext
	}{
		{"broker credential carrying an admin user", adminUser, cred(CredentialContext{Kind: CredentialKindBroker, ID: "broker-1", Type: "broker"})},
		{"user access token", uat, cred(credentialContextForIdentity(uat))},
		{"user access token kind on an admin user", adminUser, cred(CredentialContext{Kind: CredentialKindUAT})},
		{"agent", agent, cred(CredentialContext{Kind: CredentialKindAgentJWT})},
		{"agent credential kind on an admin user", adminUser, cred(CredentialContext{Kind: CredentialKindAgentJWT})},
		{"federated identity", federated, cred(credentialContextForIdentity(federated))},
		{"federated identity with an interactive kind", federated, cred(CredentialContext{Kind: CredentialKindInteractive})},
		{"hub delivery credential on an admin user", adminUser, cred(CredentialContext{Kind: CredentialKindHubDelivery})},
		{"unknown credential kind", adminUser, cred(CredentialContext{Kind: "something_else"})},
		{"missing credential context", adminUser, nil},
		{"dev credential on an admin session identity", adminUser, cred(CredentialContext{Kind: CredentialKindDev})},
		{"user access token of an admin with an interactive kind", uat, cred(CredentialContext{Kind: CredentialKindInteractive})},
		{"federated system admin with an interactive kind", federatedAdmin, cred(CredentialContext{Kind: CredentialKindInteractive})},
	}

	body := adoptBody(f.legacy.ID)
	p := f.adoptionPreview(t, admin, body)
	commitBody := withFingerprint(body, p)
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			rec := adoptionHandlerRequest(f.srv.handleDelegationAdoptionPreviews, delegationAdoptionPath+"/previews", tc.identity, tc.credential, body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "preview: %s", rec.Body.String())
			rec = adoptionHandlerRequest(f.srv.handleDelegationAdoptionCommits, delegationAdoptionPath+"/commits", tc.identity, tc.credential, commitBody)
			assert.Equal(t, http.StatusForbidden, rec.Code, "commit: %s", rec.Body.String())
		})
	}
	assert.Empty(t, f.adoptionRecords(t), "no denied request writes")
	assert.Equal(t, store.EffectCeilingUnrecorded, activeEdgesFor(t, f.store, f.legacy.ID)[0].Kind)

	// Allowed: an interactive admin session, on preview and commit. The
	// recorded initiator credential kind is session.
	interactive := cred(CredentialContext{Kind: CredentialKindInteractive, Type: adminUser.Type()})
	rec := adoptionHandlerRequest(f.srv.handleDelegationAdoptionPreviews, delegationAdoptionPath+"/previews", adminUser, interactive, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = adoptionHandlerRequest(f.srv.handleDelegationAdoptionCommits, delegationAdoptionPath+"/commits", adminUser, interactive, commitBody)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	e := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	assert.Equal(t, store.EffectCeilingBounded, e.Kind)
	assert.Equal(t, admin.ID, e.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindSession, e.InitiatorCredentialKind)

	// Allowed: the trusted local development user on a dev credential. The
	// recorded initiator credential kind is dev_local.
	devCred := cred(CredentialContext{Kind: CredentialKindDev})
	devBody := adoptBody(devAgent.ID)
	rec = adoptionHandlerRequest(f.srv.handleDelegationAdoptionPreviews, delegationAdoptionPath+"/previews", devUser, devCred, devBody)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dp delegationAdoptionPreviewResponse
	decodeJSONBody(t, rec, &dp)
	require.Equal(t, 1, dp.Writes)
	rec = adoptionHandlerRequest(f.srv.handleDelegationAdoptionCommits, delegationAdoptionPath+"/commits", devUser, devCred, withFingerprint(devBody, dp))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	e = activeEdgesFor(t, f.store, devAgent.ID)[0]
	assert.Equal(t, store.EffectCeilingBounded, e.Kind)
	assert.Equal(t, DevUserID, e.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindDevLocal, e.InitiatorCredentialKind)
}

// The trusted local development user is refused when this server does not
// accept local development authority.
func TestDelegationAdoptionRefusesDevUserWithoutDevLocalAuthority(t *testing.T) {
	f := newLegacyFixture(t, "adopt-devoff")
	ensureDevUserRecord(t, f.store)
	devUser := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@adopt.test"})
	devCred := &CredentialContext{Kind: CredentialKindDev}
	body := adoptBody(f.legacy.ID)
	rec := adoptionHandlerRequest(f.srv.handleDelegationAdoptionPreviews, delegationAdoptionPath+"/previews", devUser, devCred, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var p delegationAdoptionPreviewResponse
	decodeJSONBody(t, rec, &p)

	f.srv.authzService.setDevLocalAuthorityEnabled(false)
	rec = adoptionHandlerRequest(f.srv.handleDelegationAdoptionPreviews, delegationAdoptionPath+"/previews", devUser, devCred, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = adoptionHandlerRequest(f.srv.handleDelegationAdoptionCommits, delegationAdoptionPath+"/commits", devUser, devCred, withFingerprint(body, p))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, f.adoptionRecords(t))
}

// A hop denied only because its provenance version is not understood keeps
// the ceiling_unrecorded denial but carries no adoption details: adoption
// does not address that hop.
func TestUnknownProvenanceVersionDenialCarriesNoAdoptionDetails(t *testing.T) {
	f := newLegacyFixture(t, "adopt-v2")
	ctx := context.Background()
	orig := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	require.NoError(t, f.store.DeactivateDelegationEdge(ctx, orig.ID))
	v2 := *orig
	v2.ID = ""
	v2.Active = true
	v2.AuthorityProvenance = store.AuthorityProvenance{
		ProvenanceVersion:    2,
		SourcePrincipalKind:  orig.DelegatorType,
		SourcePrincipalID:    orig.DelegatorID,
		SourceCredentialKind: store.SourceCredentialSession,
	}
	v2.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	require.NoError(t, f.store.CreateDelegationEdge(ctx, &v2))

	token := f.agentToken(t, f.legacy.ID)
	rec := f.createAsParent(t, token, f.assignBody("adopt-v2-c"))
	assertSAGateDenied(t, rec)
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, saAssignGenericForbiddenMsg, apiErr.Message)
	assert.NotContains(t, apiErr.Details, "deny_cause")
	assert.NotContains(t, apiErr.Details, "remediation")
	assert.NotContains(t, apiErr.Details, "remediation_path")
	assert.Equal(t, "gcp_service_account", apiErr.Details["resource_type"])

}

func TestDecisionAdoptionDetailsCause(t *testing.T) {
	assert.Equal(t, DenyCauseCeilingUnrecorded, Decision{DenyCause: DenyCauseCeilingUnrecorded, adoptionRemediable: true}.adoptionDetailsCause())
	assert.Empty(t, Decision{DenyCause: DenyCauseCeilingUnrecorded}.adoptionDetailsCause(), "unknown provenance version")
	assert.Empty(t, Decision{DenyCause: DenyCauseCeilingEffectExceeded, adoptionRemediable: true}.adoptionDetailsCause())
	assert.Empty(t, Decision{}.adoptionDetailsCause())
}

// legacyRecords returns the adoption records of the fixture's legacy agent.
func (f *legacyFixture) legacyRecords(t *testing.T) []*store.DelegationAdoption {
	t.Helper()
	var out []*store.DelegationAdoption
	for _, r := range f.adoptionRecords(t) {
		if r.DelegateID == f.legacy.ID {
			out = append(out, r)
		}
	}
	return out
}

func (f *legacyFixture) adoptionStatus(t *testing.T, admin *store.User) delegationAdoptionStatusResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, admin, http.MethodGet, delegationAdoptionPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var status delegationAdoptionStatusResponse
	decodeJSONBody(t, rec, &status)
	return status
}

// Before the boot snapshot exists, the status view says so explicitly.
func TestDelegationAdoptionStatusReportsMissingSnapshot(t *testing.T) {
	f := newLegacyFixture(t, "adopt-nosnap")
	admin := adoptionAdmin(t, f.store, "adopt-nosnap-admin")
	ctx := context.Background()
	for _, section := range []string{delegationadoption.MarkerSection, delegationadoption.CohortSection} {
		require.NoError(t, f.store.DeleteHubSetting(ctx, section))
	}
	status := f.adoptionStatus(t, admin)
	assert.False(t, status.SnapshotTaken)
	assert.Nil(t, status.Cohort)
	assert.Nil(t, status.Marker)
	rec := doRequestAsUser(t, f.srv, admin, http.MethodGet, delegationAdoptionPath, nil)
	assert.Contains(t, rec.Body.String(), `"snapshotTaken":false`)

	runBootAdoption(t, f.store)
	status = f.adoptionStatus(t, admin)
	assert.True(t, status.SnapshotTaken)
	require.NotNil(t, status.Cohort)
	assert.NotEmpty(t, status.Cohort.CohortID)
}

// A revert's fingerprint binds the confirmed original edge: a commit that
// confirms a different original than its preview did is stale.
func TestDelegationAdoptionRevertFingerprintBindsConfirmedOriginal(t *testing.T) {
	f := newLegacyFixture(t, "adopt-bind")
	admin := adoptionAdmin(t, f.store, "adopt-bind-admin")
	ctx := context.Background()
	orig := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	require.NoError(t, f.store.DeactivateDelegationEdge(ctx, orig.ID))
	dup := addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.legacy.ID, f.proj.ID)
	require.NoError(t, f.store.DeactivateDelegationEdge(ctx, dup))
	repaired := delegationadoption.AdoptedEdge(orig, compatIDs(t, AgentRoleFull, false), delegationadoption.Actor{})
	require.NoError(t, f.store.CreateDelegationEdge(ctx, repaired))
	runBootAdoption(t, f.store)
	rec := f.recordFor(t, f.legacy.ID)
	require.Empty(t, rec.OriginalEdgeID)

	body := map[string]interface{}{"operation": "revert", "recordIds": []string{rec.ID},
		"confirmOriginalEdgeIds": map[string]string{rec.ID: orig.ID}}
	p := f.adoptionPreview(t, admin, body)
	require.Equal(t, delegationadoption.RevertOutcomeRevert, p.Reverts[0].Outcome)
	require.Equal(t, orig.ID, p.Reverts[0].OriginalEdgeID)

	// The commit confirms the other matching row under the preview's
	// fingerprint.
	swapped := withFingerprint(body, p)
	swapped["confirmOriginalEdgeIds"] = map[string]string{rec.ID: dup}
	resp := f.adoptionCommit(t, admin, swapped)
	require.Equal(t, http.StatusConflict, resp.Code, resp.Body.String())
	assert.Equal(t, ErrCodeStaleAuthorizationPreview, decodeTargetAPIError(t, resp).Code)
	assert.Equal(t, repaired.ID, activeEdgesFor(t, f.store, f.legacy.ID)[0].ID, "nothing is reverted")

	// The matching commit succeeds.
	resp = f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	assert.Equal(t, orig.ID, activeEdgesFor(t, f.store, f.legacy.ID)[0].ID)
}

// A commit whose plan writes more hops than the cap is refused with 422 and
// writes nothing.
func TestDelegationAdoptionCommitRefusesPlanOverCap(t *testing.T) {
	f := newLegacyFixture(t, "adopt-cap")
	admin := adoptionAdmin(t, f.store, "adopt-cap-admin")
	// The fixture's legacy agent plus delegationAdoptionMaxHops more.
	for i := 0; i < delegationAdoptionMaxHops; i++ {
		f.seedLegacyAgent(t, fmt.Sprintf("adopt-cap-%03d", i), nil, AgentRoleFull)
	}
	body := map[string]interface{}{"operation": "adopt", "scope": map[string]interface{}{"projectId": f.proj.ID}}
	p := f.adoptionPreview(t, admin, body)
	require.Equal(t, delegationAdoptionMaxHops+1, p.Writes)
	assert.True(t, p.ExceedsCap)

	rec := f.adoptionCommit(t, admin, withFingerprint(body, p))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "narrow the scope")
	assert.Empty(t, f.adoptionRecords(t))
	assert.Equal(t, store.EffectCeilingUnrecorded, activeEdgesFor(t, f.store, f.legacy.ID)[0].Kind)
}

// A revert records who reverted and keeps the adopter: actor_* and
// after_summary are not overwritten.
func TestDelegationAdoptionRevertKeepsAdopter(t *testing.T) {
	f := newLegacyFixture(t, "adopt-keep")
	adopter := adoptionAdmin(t, f.store, "adopt-keep-adopter")
	reverter := adoptionAdmin(t, f.store, "adopt-keep-reverter")
	original := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	body := adoptBody(f.legacy.ID)
	p := f.adoptionPreview(t, adopter, body)
	require.Equal(t, http.StatusOK, f.adoptionCommit(t, adopter, withFingerprint(body, p)).Code)
	adopted := f.recordFor(t, f.legacy.ID)
	require.Equal(t, adopter.ID, adopted.ActorID)
	adoptedSummary := adopted.AfterSummary
	require.NotEmpty(t, adoptedSummary)

	rbody := map[string]interface{}{"operation": "revert", "recordIds": []string{adopted.ID}}
	rp := f.adoptionPreview(t, reverter, rbody)
	require.Equal(t, http.StatusOK, f.adoptionCommit(t, reverter, withFingerprint(rbody, rp)).Code)

	got, err := f.store.GetDelegationAdoption(context.Background(), adopted.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DelegationAdoptionReverted, got.Status)
	assert.Equal(t, store.DelegationPrincipalUser, got.ActorKind)
	assert.Equal(t, adopter.ID, got.ActorID, "the adopter is kept")
	assert.Equal(t, adoptedSummary, got.AfterSummary, "the adopted edge summary is kept")
	assert.Equal(t, store.DelegationPrincipalUser, got.RevertedByKind)
	assert.Equal(t, reverter.ID, got.RevertedByID)
	assert.Contains(t, got.RevertSummary, original.ID)
	require.NotNil(t, got.RevertedAt)
}

// When an admin adoption and a later boot record both point at one edge,
// reverting through either record reverts the edge once and marks both.
func TestDelegationAdoptionRevertCoversRecordsOnTheSameEdge(t *testing.T) {
	f := newLegacyFixture(t, "adopt-pair")
	admin := adoptionAdmin(t, f.store, "adopt-pair-admin")
	original := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	body := adoptBody(f.legacy.ID)
	p := f.adoptionPreview(t, admin, body)
	require.Equal(t, http.StatusOK, f.adoptionCommit(t, admin, withFingerprint(body, p)).Code)
	runBootAdoption(t, f.store)

	recs := f.legacyRecords(t)
	require.Len(t, recs, 2)
	edge := recs[0].AdoptedEdgeID
	require.NotEmpty(t, edge)
	require.Equal(t, edge, recs[1].AdoptedEdgeID, "both records point at the adopted edge")
	var adopted, recognized *store.DelegationAdoption
	for _, r := range recs {
		switch r.Status {
		case store.DelegationAdoptionAdopted:
			adopted = r
		case store.DelegationAdoptionRecognized:
			recognized = r
		}
	}
	require.NotNil(t, adopted)
	require.NotNil(t, recognized)

	rbody := map[string]interface{}{"operation": "revert", "recordIds": []string{recognized.ID, adopted.ID}}
	rp := f.adoptionPreview(t, admin, rbody)
	require.Len(t, rp.Reverts, 1)
	assert.Zero(t, rp.Refused)
	assert.Equal(t, 1, rp.Writes)
	resp := f.adoptionCommit(t, admin, withFingerprint(rbody, rp))
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	for _, r := range f.legacyRecords(t) {
		assert.Equal(t, store.DelegationAdoptionReverted, r.Status, r.ID)
		assert.Equal(t, original.ID, r.OriginalEdgeID)
	}
	e := activeEdgesFor(t, f.store, f.legacy.ID)
	require.Len(t, e, 1)
	assert.Equal(t, original.ID, e[0].ID)
	assert.Len(t, f.adoptionAudits(t, mutationTypeDelegationAdoptionRevert), 1, "the edge is reverted once")
	var revertSummary *store.MutationAuditRecord
	for _, a := range f.adoptionAudits(t, mutationTypeDelegationAdoptionCommit) {
		if strings.Contains(a.AfterSummary, `"operation":"revert"`) {
			revertSummary = a
		}
	}
	require.NotNil(t, revertSummary)
	assert.Contains(t, revertSummary.AfterSummary, `"hops":1,"covered_records":1`, "one edge, plus one covered record")
}

// adoptionStatusRequest calls the adoption status handler directly with the
// given identity and credential context. A nil credential leaves the context
// without a credential context.
func adoptionStatusRequest(h http.HandlerFunc, identity Identity, credential *CredentialContext) *httptest.ResponseRecorder {
	ctx := contextWithIdentity(context.Background(), identity)
	if credential != nil {
		ctx = contextWithCredentialContext(ctx, *credential)
	}
	req := httptest.NewRequest(http.MethodGet, delegationAdoptionPath, nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// The request credential kind is checked on its own: with the recorded-kind
// mapping replaced by one that admits every request, an admin under a
// credential kind other than interactive or dev is refused on status,
// preview and commit.
func TestDelegationAdoptionRequestCredentialKindIsCheckedOnItsOwn(t *testing.T) {
	f := newLegacyFixture(t, "adopt-reqkind")
	admin := adoptionAdmin(t, f.store, "adopt-reqkind-admin")
	adminUser := authUser(admin)
	body := adoptBody(f.legacy.ID)
	p := f.adoptionPreview(t, admin, body)
	commitBody := withFingerprint(body, p)

	mapped := 0
	f.srv.delegationAdoptionInitiatorKindHook = func(Identity, CredentialKind) string {
		mapped++
		return store.InitiatorCredentialKindSession
	}

	cred := func(c CredentialContext) *CredentialContext { return &c }
	denied := []struct {
		name       string
		credential *CredentialContext
	}{
		{"broker credential", cred(CredentialContext{Kind: CredentialKindBroker, ID: "broker-1", Type: "broker"})},
		{"user access token kind", cred(CredentialContext{Kind: CredentialKindUAT})},
		{"agent credential kind", cred(CredentialContext{Kind: CredentialKindAgentJWT})},
		{"federation credential kind", cred(CredentialContext{Kind: CredentialKindFederation})},
		{"hub delivery credential", cred(CredentialContext{Kind: CredentialKindHubDelivery})},
		{"unknown credential kind", cred(CredentialContext{Kind: "something_else"})},
		{"missing credential context", nil},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			rec := adoptionStatusRequest(f.srv.handleDelegationAdoption, adminUser, tc.credential)
			assert.Equal(t, http.StatusForbidden, rec.Code, "status: %s", rec.Body.String())
			rec = adoptionHandlerRequest(f.srv.handleDelegationAdoptionPreviews, delegationAdoptionPath+"/previews", adminUser, tc.credential, body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "preview: %s", rec.Body.String())
			rec = adoptionHandlerRequest(f.srv.handleDelegationAdoptionCommits, delegationAdoptionPath+"/commits", adminUser, tc.credential, commitBody)
			assert.Equal(t, http.StatusForbidden, rec.Code, "commit: %s", rec.Body.String())
		})
	}
	assert.Zero(t, mapped, "a refused request kind is not mapped")
	assert.Empty(t, f.adoptionRecords(t), "no denied request writes")

	// The replaced mapping is in effect: an interactive admin is admitted
	// through it.
	rec := adoptionHandlerRequest(f.srv.handleDelegationAdoptionPreviews, delegationAdoptionPath+"/previews", adminUser,
		cred(CredentialContext{Kind: CredentialKindInteractive}), body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, mapped)
}

// The status view shares the adoption credential rule: a broker request
// carrying an admin user is refused, and an interactive admin is admitted.
func TestDelegationAdoptionStatusRequiresInteractiveOrDevCredential(t *testing.T) {
	f := newLegacyFixture(t, "adopt-statuscred")
	admin := adoptionAdmin(t, f.store, "adopt-statuscred-admin")
	adminUser := authUser(admin)

	rec := adoptionStatusRequest(f.srv.handleDelegationAdoption, adminUser,
		&CredentialContext{Kind: CredentialKindBroker, ID: "broker-1", Type: "broker"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "snapshotTaken")

	rec = adoptionStatusRequest(f.srv.handleDelegationAdoption, adminUser,
		&CredentialContext{Kind: CredentialKindInteractive, Type: adminUser.Type()})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var status delegationAdoptionStatusResponse
	decodeJSONBody(t, rec, &status)
	assert.NotNil(t, status.Counts)
}

// A revert hop whose covered record names a different original edge is
// refused: the preview reports the refusal, the commit returns 422, and no
// record or edge changes.
func TestDelegationAdoptionRevertRefusesCoveredRecordWithOtherOriginal(t *testing.T) {
	f := newLegacyFixture(t, "adopt-covorig")
	admin := adoptionAdmin(t, f.store, "adopt-covorig-admin")
	body := adoptBody(f.legacy.ID)
	p := f.adoptionPreview(t, admin, body)
	require.Equal(t, http.StatusOK, f.adoptionCommit(t, admin, withFingerprint(body, p)).Code)
	runBootAdoption(t, f.store)

	var adopted, recognized *store.DelegationAdoption
	for _, r := range f.legacyRecords(t) {
		switch r.Status {
		case store.DelegationAdoptionAdopted:
			adopted = r
		case store.DelegationAdoptionRecognized:
			recognized = r
		}
	}
	require.NotNil(t, adopted)
	require.NotNil(t, recognized)
	require.NotEmpty(t, adopted.OriginalEdgeID)
	require.Equal(t, adopted.AdoptedEdgeID, recognized.AdoptedEdgeID)

	// The recognized record names another original edge.
	otherID := "edge-not-the-original"
	require.NotEqual(t, adopted.OriginalEdgeID, otherID)
	recognized.OriginalEdgeID = otherID
	require.NoError(t, f.store.UpdateDelegationAdoption(context.Background(), recognized))
	adoptedEdge := activeEdgesFor(t, f.store, f.legacy.ID)[0]

	rbody := map[string]interface{}{"operation": "revert", "recordIds": []string{adopted.ID}}
	rp := f.adoptionPreview(t, admin, rbody)
	require.Len(t, rp.Reverts, 1)
	assert.Equal(t, 1, rp.Refused)
	assert.Equal(t, delegationadoption.RevertOutcomeRefused, rp.Reverts[0].Outcome)
	assert.Equal(t, delegationadoption.ReasonCoveredOriginalDiffers, rp.Reverts[0].Reason)
	assert.Equal(t, []string{recognized.ID}, rp.Reverts[0].CoveredRecordIDs)

	resp := f.adoptionCommit(t, admin, withFingerprint(rbody, rp))
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	for _, r := range f.legacyRecords(t) {
		assert.NotEqual(t, store.DelegationAdoptionReverted, r.Status, r.ID)
		if r.ID == recognized.ID {
			assert.Equal(t, otherID, r.OriginalEdgeID, "the covered record keeps its original edge")
		}
	}
	e := activeEdgesFor(t, f.store, f.legacy.ID)
	require.Len(t, e, 1)
	assert.Equal(t, adoptedEdge.ID, e[0].ID)
	assert.Empty(t, f.adoptionAudits(t, mutationTypeDelegationAdoptionRevert))
}
