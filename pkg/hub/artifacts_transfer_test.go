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
// authorization stack:
//   - session-only credential enforcement (UAT refused, CLI login Bearer JWT admitted);
//   - RBAC caller roles (delegator, project-owner, super-admin, project-admin,
//     hub-admin, member, agent);
//   - live vs deleted owner rules;
//   - real eligibleTransferTarget and TransferTargets filtering for all five
//     refused target classes (not_controlled, other_project, deleted, held,
//     reincarnating, and no_root) with byte-identical 403 bodies, while
//     admitting suspended caller-controlled targets in the same project;
//   - sponsor_ref re-recording, ownerDeleted clearing, and post-transfer publish.
func TestArtifactTransferRoutes(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, blobs := enableArtifactsForTest(t, f.srv)
	ctx := context.Background()

	setArtifactsTransferExperiment(t, f.srv, true)

	superID := tid("tr-super")
	hubAdminID := tid("tr-hub-admin")
	createRS3SuperAdmin(t, f.s, superID)
	createRS3HubAdmin(t, f.s, hubAdminID)

	// Give f.delegator membership in f.other so an agent in f.other has a valid
	// delegation chain there and is refused ONLY by the same-project guard.
	bindProjectRole(t, f.s, f.other, f.delegator, store.ProjectRoleMember)

	// Helper to create an agent in projectID with an active delegation edge from ownerUserID.
	newDelegatedAgent := func(name, projectID, ownerUserID string, phase state.Phase) *store.Agent {
		t.Helper()
		id := tid("tr-agent-" + name)
		createDCAgent(t, f.s, id, projectID, ownerUserID, AgentRoleFull)
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
			ScopeID:             projectID,
			Role:                string(AgentRoleFull),
			Active:              true,
			AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, ownerUserID, store.SourceCredentialSession),
			EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}))
		return a
	}

	// Eligible target owned by f.delegator (suspended is allowed).
	targetAgent := newDelegatedAgent("target-deleg", f.home, f.delegator, state.PhaseSuspended)
	// Live target owned by f.owner (not controlled by f.delegator).
	ownerAgent := newDelegatedAgent("target-owner", f.home, f.owner, state.PhaseRunning)
	// Caller-controlled agent in another project (f.other).
	otherProjectAgent := newDelegatedAgent("target-other-proj", f.other, f.delegator, state.PhaseRunning)
	// Caller-controlled agent in f.home that is soft-deleted.
	deletedTargetAgent := newDelegatedAgent("target-deleted", f.home, f.delegator, state.PhaseRunning)
	require.NoError(t, f.s.DeleteAgent(ctx, deletedTargetAgent.ID))
	// Caller-controlled agent in f.home with an active hold (agentStanding refusal).
	heldTargetAgent := newDelegatedAgent("target-held", f.home, f.delegator, state.PhaseRunning)
	_, err := f.s.CreateAgentHolds(ctx, []*store.AgentHold{{
		AgentID:           heldTargetAgent.ID,
		ProjectID:         f.home,
		Cause:             store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser,
		RootPrincipalID:   f.delegator,
		Trigger:           store.MembershipLossTriggerMemberRemove,
		ActorKind:         "system",
		ActorID:           "hub",
		CorrelationID:     "test-transfer-hold",
	}})
	require.NoError(t, err)
	// Caller-controlled agent in f.home with reincarnation in flight.
	reincarnatingAgent := newDelegatedAgent("target-reincarnating", f.home, f.delegator, state.PhaseRunning)
	reincarnatingAgent.ReincarnationState = store.ReincarnationStatePending
	require.NoError(t, f.s.UpdateAgent(ctx, reincarnatingAgent))
	// Caller-controlled agent in f.home with no delegation edge (no root): f.noEdge.

	// Create an artifact owned by f.agent with SponsorRef = f.delegator.
	id := reviewGrantArtifact(t, f, st, blobs)
	_, err = st.SetAgentSponsor(ctx, f.agent.ID, f.delegator)
	require.NoError(t, err)

	transferPath := "/api/v1/artifacts/" + id + "/transfer"
	targetsPath := "/api/v1/artifacts/" + id + "/transfer-targets"

	// Helper attaching both Identity and derived CredentialContext (as
	// UnifiedAuthMiddleware does on real HTTP requests).
	doReq := func(ident Identity, method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		reqCtx := contextWithIdentity(req.Context(), ident)
		reqCtx = contextWithCredentialContext(reqCtx, credentialContextForIdentity(ident))
		req = req.WithContext(reqCtx)
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

	// 2. Caller authorization checks: hub-admin (404), project-admin (403),
	// plain project member (403), agent caller (403).
	recHubAdmin := doReq(f.user(hubAdminID), http.MethodPost, transferPath,
		[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, targetAgent.ID, f.agent.ID)))
	assert.Equal(t, http.StatusNotFound, recHubAdmin.Code)

	for _, tc := range []struct {
		name  string
		ident Identity
	}{
		{"project-admin", f.user(f.admin)},
		{"project-member", f.user(f.member)},
		{"owning-agent", artifactTestAgent(f.agent.ID, f.home, ScopesForRole(AgentRoleFull)...)},
	} {
		rec := doReq(tc.ident, http.MethodPost, transferPath,
			[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, targetAgent.ID, f.agent.ID)))
		assert.Equal(t, http.StatusForbidden, rec.Code, tc.name)
		assert.Contains(t, rec.Body.String(), "transfer_not_allowed", tc.name)
	}

	// 3. Live-owner rule: project-owner and super-admin cannot transfer while
	// f.agent is live because they do not control f.agent.
	for _, tc := range []struct {
		name string
		uid  string
	}{
		{"project-owner", f.owner},
		{"super-admin", superID},
	} {
		rec := doReq(f.user(tc.uid), http.MethodPost, transferPath,
			[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, ownerAgent.ID, f.agent.ID)))
		assert.Equal(t, http.StatusForbidden, rec.Code, tc.name)
		assert.Contains(t, rec.Body.String(), "owner_live", tc.name)
	}

	// 4. GET transfer-targets for delegator: includes suspended targetAgent,
	// excludes current owner and every ineligible target class.
	recTargets := doReq(f.user(f.delegator), http.MethodGet, targetsPath, nil)
	require.Equal(t, http.StatusOK, recTargets.Code, recTargets.Body.String())
	var targetsResp artifacts.TransferTargetsResponse
	require.NoError(t, json.Unmarshal(recTargets.Body.Bytes(), &targetsResp))
	var targetIDs []string
	for _, c := range targetsResp.Agents {
		targetIDs = append(targetIDs, c.ID)
	}
	assert.Contains(t, targetIDs, targetAgent.ID)
	assert.NotContains(t, targetIDs, f.agent.ID, "current owner omitted")
	assert.NotContains(t, targetIDs, ownerAgent.ID, "uncontrolled agent omitted")
	assert.NotContains(t, targetIDs, otherProjectAgent.ID, "other-project agent omitted")
	assert.NotContains(t, targetIDs, deletedTargetAgent.ID, "deleted agent omitted")
	assert.NotContains(t, targetIDs, heldTargetAgent.ID, "held agent omitted")
	assert.NotContains(t, targetIDs, reincarnatingAgent.ID, "reincarnating agent omitted")
	assert.NotContains(t, targetIDs, f.noEdge.ID, "no-root agent omitted")

	// 5. Real hub eligibleTransferTarget (S4): every ineligible target class
	// returns 403 target_ineligible with a byte-identical response body.
	ineligibleCases := []struct {
		name    string
		agentID string
	}{
		{"not_controlled", ownerAgent.ID},
		{"other_project", otherProjectAgent.ID},
		{"deleted", deletedTargetAgent.ID},
		{"held", heldTargetAgent.ID},
		{"reincarnating", reincarnatingAgent.ID},
		{"no_root", f.noEdge.ID},
	}
	var wantBody string
	for _, tc := range ineligibleCases {
		rec := doReq(f.user(f.delegator), http.MethodPost, transferPath,
			[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, tc.agentID, f.agent.ID)))
		require.Equal(t, http.StatusForbidden, rec.Code, "target %s: %s", tc.name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "target_ineligible", "target %s", tc.name)
		if wantBody == "" {
			wantBody = rec.Body.String()
		} else {
			assert.Equal(t, wantBody, rec.Body.String(), "target %s must return byte-identical 403 body", tc.name)
		}
	}

	// 6. Delete f.agent and mark orphaned; now project-owner (and super-admin)
	// CAN transfer to an agent they control.
	require.NoError(t, f.s.DeleteAgent(ctx, f.agent.ID))
	_, err = st.SetOwnerOrphaned(ctx, artifacts.Owner{Kind: artifacts.PrincipalKindAgent, Ref: f.agent.ID}, true, time.Now(), 10)
	require.NoError(t, err)

	// Super-admin reaches steward clause 1 when f.agent is deleted (canTransfer=true).
	recSuperGet := doReq(f.user(superID), http.MethodGet, "/api/v1/artifacts/"+id, nil)
	require.Equal(t, http.StatusOK, recSuperGet.Code)
	var superGetResp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(recSuperGet.Body.Bytes(), &superGetResp))
	assert.True(t, superGetResp.CanTransfer, "super-admin should have canTransfer=true on deleted owner")

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

// TestArtifactTransferNoticeHygiene verifies that notifyArtifactTransfer cleans
// control, ANSI escape, and bidi override characters from DisplayName and falls
// back to "a steward" without leaking raw email or user ID.
func TestArtifactTransferNoticeHygiene(t *testing.T) {
	f := newReviewGrantFixture(t)
	ctx := context.Background()
	dispatcher := &recordingDispatcher{}
	f.srv.SetDispatcher(dispatcher)

	// 1. User with malicious DisplayName containing newline, ANSI CSI, and bidi override.
	dirtyUser, err := f.s.GetUser(ctx, f.owner)
	require.NoError(t, err)
	dirtyUser.DisplayName = "Alice\n\x1b[2J\u202eEvil"
	dirtyUser.Email = "secret-alice@example.com"
	require.NoError(t, f.s.UpdateUser(ctx, dirtyUser))

	res := f.srv.notifyArtifactTransfer(ctx, artifacts.TransferNotice{
		ArtifactID:      tid("tr-notice-art-1"),
		Title:           "Design Doc",
		FromAgentID:     f.agent.ID,
		ToAgentID:       f.sibling.ID,
		ActorUserID:     dirtyUser.ID,
		FormerOwnerLive: true,
	})
	assert.True(t, res.NewOwner)
	assert.True(t, res.FormerOwner)

	// 2. User with empty DisplayName and non-empty Email must fall back to "a steward"
	// and never leak the email or raw user ID.
	emptyNameUser, err := f.s.GetUser(ctx, f.delegator)
	require.NoError(t, err)
	emptyNameUser.DisplayName = ""
	emptyNameUser.Email = "private-delegator@example.com"
	require.NoError(t, f.s.UpdateUser(ctx, emptyNameUser))

	res2 := f.srv.notifyArtifactTransfer(ctx, artifacts.TransferNotice{
		ArtifactID:      tid("tr-notice-art-2"),
		Title:           "Plan",
		FromAgentID:     f.agent.ID,
		ToAgentID:       f.sibling.ID,
		ActorUserID:     emptyNameUser.ID,
		FormerOwnerLive: false,
	})
	assert.True(t, res2.NewOwner)
	assert.False(t, res2.FormerOwner)

	require.GreaterOrEqual(t, len(dispatcher.calls), 3)
	// First two notices are from dirtyUser: no newline, no ANSI escape, no bidi, no email.
	for i := 0; i < 2; i++ {
		msg := dispatcher.calls[i].Message
		assert.NotContains(t, msg, "\n")
		assert.NotContains(t, msg, "\x1b")
		assert.NotContains(t, msg, "\u202e")
		assert.NotContains(t, msg, "secret-alice@example.com")
		assert.Contains(t, msg, "Alice")
	}
	// Third notice is from emptyNameUser: uses "a steward", no email, no raw user ID.
	fallbackMsg := dispatcher.calls[2].Message
	assert.Contains(t, fallbackMsg, "a steward")
	assert.NotContains(t, fallbackMsg, "private-delegator@example.com")
	assert.NotContains(t, fallbackMsg, emptyNameUser.ID)
}
