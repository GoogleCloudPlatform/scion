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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setArtifactsTransferExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		switch e.Name {
		case experiments.Artifacts:
			e.Default = true
		case experiments.ArtifactsTransfer:
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.ArtifactsTransfer))
	require.True(t, srv.experimentEnabled(experiments.Artifacts))
}

// TestArtifactTransferRoutes verifies POST /api/v1/artifacts/{id}/transfer and
// GET /api/v1/artifacts/{id}/transfer-targets end-to-end through the real Hub
// authorization stack, including session-only credential enforcement (Q1),
// RBAC roles (delegator, project-owner, super-admin, project-admin, hub-admin,
// member, UAT, agent), live vs deleted owner rules, target eligibility
// (same project, controlled by caller, suspended allowed, held refused),
// sponsor_ref re-recording, ownerDeleted clearing, and post-transfer publish.
func TestArtifactTransferRoutes(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, blobs := enableArtifactsForTest(t, f.srv)
	ctx := context.Background()

	setArtifactsTransferExperiment(t, f.srv, true)

	superID := tid("tr-super")
	hubAdminID := tid("tr-hub-admin")
	createRS3SuperAdmin(t, f.s, superID)
	createRS3HubAdmin(t, f.s, hubAdminID)

	// Helper to create an agent with an active delegation edge from ownerUserID.
	newDelegatedAgent := func(name, ownerUserID string, phase state.Phase) *store.Agent {
		t.Helper()
		id := tid("tr-agent-" + name)
		createDCAgent(t, f.s, id, f.home, ownerUserID, AgentRoleFull)
		a, err := f.s.GetAgent(ctx, id)
		require.NoError(t, err)
		if phase != "" && a.Phase != string(phase) {
			a.Phase = string(phase)
			require.NoError(t, f.s.UpdateAgent(ctx, a))
		}
		require.NoError(t, f.s.CreateDelegationEdge(ctx, &store.DelegationEdge{
			DelegatorType:       store.DelegationPrincipalUser,
			DelegatorID:         ownerUserID,
			DelegateType:        store.DelegationPrincipalAgent,
			DelegateID:          id,
			ScopeType:           store.RoleScopeProject,
			ScopeID:             f.home,
			Role:                string(AgentRoleFull),
			Active:              true,
			AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, ownerUserID, store.SourceCredentialSession),
			EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}))
		return a
	}

	// Target agent owned by f.delegator (suspended is allowed).
	targetAgent := newDelegatedAgent("target-deleg", f.delegator, state.PhaseSuspended)
	// Target agent owned by f.owner.
	ownerAgent := newDelegatedAgent("target-owner", f.owner, state.PhaseRunning)

	// Create an artifact owned by f.agent with SponsorRef = f.delegator.
	id := reviewGrantArtifact(t, f, st, blobs)
	_, err := st.SetAgentSponsor(ctx, f.agent.ID, f.delegator)
	require.NoError(t, err)

	transferPath := "/api/v1/artifacts/" + id + "/transfer"
	targetsPath := "/api/v1/artifacts/" + id + "/transfer-targets"

	// Helper attaching both Identity and derived CredentialContext (as
	// UnifiedAuthMiddleware does on real HTTP requests).
	doReq := func(ident Identity, method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		ctx := contextWithIdentity(req.Context(), ident)
		ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(ident))
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rec, req)
		return rec
	}

	// 1. Q1 verification: UAT (ScopedUserIdentity) is refused with session-only 403,
	// while a real Bearer user session JWT (as issued by `scion hub auth login`)
	// authenticates as AuthenticatedUser + CredentialKindInteractive and passes.
	delegUser := NewAuthenticatedUser(f.delegator, "rg-delegator@test.com", "delegator", "member", "api")
	uat := artifactTestUAT(t, delegUser, f.home, "artifact:read", "artifact:manage", "artifact:steward")
	recUAT := doReq(uat, http.MethodPost, transferPath,
		[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, targetAgent.ID, f.agent.ID)))
	assert.Equal(t, http.StatusForbidden, recUAT.Code)
	assert.Contains(t, recUAT.Body.String(), "session_required")

	// Verify real CLI login JWT through full middleware (f.srv.Handler()) passes session gate.
	delegStoreUser, err := f.s.GetUser(ctx, f.delegator)
	require.NoError(t, err)
	cliJWT, _, _, err := f.srv.userTokenService.GenerateTokenPair(
		delegStoreUser.ID, delegStoreUser.Email, delegStoreUser.DisplayName, delegStoreUser.Role, ClientTypeCLI)
	require.NoError(t, err)
	cliReq := httptest.NewRequest(http.MethodGet, targetsPath, nil)
	cliReq.Header.Set("Authorization", "Bearer "+cliJWT)
	cliRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(cliRec, cliReq)
	require.Equal(t, http.StatusOK, cliRec.Code, "CLI session JWT must pass session-only gate: %s", cliRec.Body.String())

	// 2. hub-admin without project membership gets 404 (read-first, D1).
	recHubAdmin := doReq(f.user(hubAdminID), http.MethodPost, transferPath,
		[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, targetAgent.ID, f.agent.ID)))
	assert.Equal(t, http.StatusNotFound, recHubAdmin.Code)

	// 3. project-admin (not project-owner, not delegator) gets 403 transfer_not_allowed.
	recAdmin := doReq(f.user(f.admin), http.MethodPost, transferPath,
		[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, targetAgent.ID, f.agent.ID)))
	assert.Equal(t, http.StatusForbidden, recAdmin.Code)
	assert.Contains(t, recAdmin.Body.String(), "transfer_not_allowed")

	// 4. project-owner cannot transfer while f.agent is live (f.owner doesn't control f.agent).
	recOwnerLive := doReq(f.user(f.owner), http.MethodPost, transferPath,
		[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, ownerAgent.ID, f.agent.ID)))
	assert.Equal(t, http.StatusForbidden, recOwnerLive.Code)
	assert.Contains(t, recOwnerLive.Body.String(), "owner_live")

	// 5. Delegator sees targetAgent (suspended is allowed) in transfer-targets,
	// and does NOT see ownerAgent (not controlled by delegator) or f.agent (current owner).
	recTargets := doReq(f.user(f.delegator), http.MethodGet, targetsPath, nil)
	require.Equal(t, http.StatusOK, recTargets.Code, recTargets.Body.String())
	var targetsResp artifacts.TransferTargetsResponse
	require.NoError(t, json.Unmarshal(recTargets.Body.Bytes(), &targetsResp))
	var targetIDs []string
	for _, c := range targetsResp.Agents {
		targetIDs = append(targetIDs, c.ID)
	}
	assert.Contains(t, targetIDs, targetAgent.ID)
	assert.NotContains(t, targetIDs, ownerAgent.ID)
	assert.NotContains(t, targetIDs, f.agent.ID)

	// 6. Delegator cannot transfer to ownerAgent (not controlled -> target_ineligible).
	recBadTarget := doReq(f.user(f.delegator), http.MethodPost, transferPath,
		[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, ownerAgent.ID, f.agent.ID)))
	assert.Equal(t, http.StatusForbidden, recBadTarget.Code)
	assert.Contains(t, recBadTarget.Body.String(), "target_ineligible")

	// 7. Delete f.agent and mark orphaned; now project-owner CAN transfer to ownerAgent.
	require.NoError(t, f.s.DeleteAgent(ctx, f.agent.ID))
	_, err = st.SetOwnerOrphaned(ctx, artifacts.Owner{Kind: artifacts.PrincipalKindAgent, Ref: f.agent.ID}, true, time.Now(), 10)
	require.NoError(t, err)

	// Verify GET shows ownerDeleted=true and canTransfer=true for project-owner.
	recGet := doReq(f.user(f.owner), http.MethodGet, "/api/v1/artifacts/"+id, nil)
	require.Equal(t, http.StatusOK, recGet.Code)
	var getResp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(recGet.Body.Bytes(), &getResp))
	assert.True(t, getResp.Artifact.OwnerDeleted)
	assert.True(t, getResp.CanTransfer)

	// Project-owner transfers deleted agent's artifact to ownerAgent.
	recTransfer := doReq(f.user(f.owner), http.MethodPost, transferPath,
		[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, ownerAgent.ID, f.agent.ID)))
	require.Equal(t, http.StatusOK, recTransfer.Code, recTransfer.Body.String())
	var trResp artifacts.TransferResponse
	require.NoError(t, json.Unmarshal(recTransfer.Body.Bytes(), &trResp))
	assert.Equal(t, ownerAgent.ID, trResp.Artifact.Artifact.OwnerRef)
	assert.Equal(t, f.agent.ID, trResp.Artifact.Artifact.FormerOwnerRef)
	assert.False(t, trResp.Artifact.Artifact.OwnerDeleted)
	assert.NotNil(t, trResp.Artifact.Artifact.TransferredAt)

	// Verify sponsor_ref was re-recorded to f.owner.
	stored, err := st.GetArtifact(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, f.owner, stored.SponsorRef)

	// Verify new owner (ownerAgent) can create a new version (seq=2).
	ownerAgentToken := artifactTestAgent(ownerAgent.ID, f.home, ScopesForRole(AgentRoleFull)...)
	v2Manifest := `{"entry":"doc.md","files":[{"path":"doc.md","size":2,"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}]}`
	recV2 := doReq(ownerAgentToken, http.MethodPost, "/api/v1/artifacts/"+id+"/versions", []byte(v2Manifest))
	assert.Equal(t, http.StatusCreated, recV2.Code, recV2.Body.String())
}
