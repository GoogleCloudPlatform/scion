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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAncestorCandidates (A1): unit test for candidate extraction, ordering,
// legacy CreatedBy fallback, deduplication, self-exclusion, UUID filtering,
// and maxSuccessionWalk cap.
func TestAncestorCandidates(t *testing.T) {
	u := uuid.NewString()
	g := uuid.NewString()
	p := uuid.NewString()
	self := uuid.NewString()

	assert.Empty(t, ancestorCandidates(nil))
	// Non-UUID root user -> empty.
	assert.Empty(t, ancestorCandidates(&store.Agent{ID: self, Ancestry: []string{"user-non-uuid"}}))
	// UUID root user -> included in candidate list (skipped on GetAgent ErrNotFound).
	assert.Equal(t, []string{u}, ancestorCandidates(&store.Agent{ID: self, Ancestry: []string{u}}))
	// [u, p] -> [p, u] (nearest first).
	assert.Equal(t, []string{p, u}, ancestorCandidates(&store.Agent{ID: self, Ancestry: []string{u, p}}))
	// [u, g, p] -> [p, g, u].
	assert.Equal(t, []string{p, g, u}, ancestorCandidates(&store.Agent{ID: self, Ancestry: []string{u, g, p}}))
	// Legacy [p] -> [p].
	assert.Equal(t, []string{p}, ancestorCandidates(&store.Agent{ID: self, Ancestry: []string{p}}))
	// Empty Ancestry with CreatedBy != OwnerID -> [CreatedBy].
	assert.Equal(t, []string{p}, ancestorCandidates(&store.Agent{ID: self, OwnerID: u, CreatedBy: p}))
	// Empty Ancestry with CreatedBy == OwnerID -> empty.
	assert.Empty(t, ancestorCandidates(&store.Agent{ID: self, OwnerID: u, CreatedBy: u}))
	// Dedup (nearest kept), self-id excluded (case-insensitive), non-UUID dropped.
	assert.Equal(t, []string{g, p}, ancestorCandidates(&store.Agent{
		ID:       self,
		Ancestry: []string{"not-a-uuid", g, strings.ToUpper(self), p, strings.ToUpper(p), g},
	}))

	// Cap at maxSuccessionWalk (32).
	longAncestry := make([]string, 40)
	for i := range longAncestry {
		longAncestry[i] = uuid.NewString()
	}
	got := ancestorCandidates(&store.Agent{ID: self, Ancestry: longAncestry})
	require.Len(t, got, maxSuccessionWalk)
	assert.Equal(t, longAncestry[39], got[0])
	assert.Equal(t, longAncestry[40-maxSuccessionWalk], got[maxSuccessionWalk-1])
}

// successionFixture sets up a hub server with artifacts + review requests
// enabled, a dispatcher spy, and helpers to create parent/child agent chains,
// publish artifacts, and delete agents through the delete engine.
type successionFixture struct {
	reviewGrantFixture
	st         artifacts.Store
	blobs      *storage.LocalStorage
	dispatcher *recordingDispatcher
}

func (sf *successionFixture) dispatchedCalls() []dispatchCall {
	sf.dispatcher.mu.Lock()
	defer sf.dispatcher.mu.Unlock()
	out := make([]dispatchCall, len(sf.dispatcher.calls))
	copy(out, sf.dispatcher.calls)
	return out
}

func newSuccessionFixture(t *testing.T) *successionFixture {
	t.Helper()
	rf := newReviewGrantFixture(t)
	st, blobs := enableArtifactsForTest(t, rf.srv)

	// Enable hub.artifacts, hub.artifacts_review_requests, and hub.artifacts_transfer.
	var active []experiments.Experiment
	for _, e := range rf.srv.experimentRegistry().All() {
		switch e.Name {
		case experiments.Artifacts, experiments.ArtifactsReviewRequests, experiments.ArtifactsTransfer:
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	rf.srv.experiments = reg

	disp := &recordingDispatcher{}
	rf.srv.SetDispatcher(disp)
	rf.srv.config.SoftDeleteRetention = 24 * time.Hour

	return &successionFixture{
		reviewGrantFixture: rf,
		st:                 st,
		blobs:              blobs,
		dispatcher:         disp,
	}
}

// createChainAgent creates an agent in projectID with an active delegation edge
// from its direct creator (either a user or a parent agent) and the given
// Ancestry slice.
func (sf *successionFixture) createChainAgent(t *testing.T, name, projectID, ownerUserID string, parentAgent *store.Agent, phase state.Phase) *store.Agent {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if phase == "" {
		phase = state.PhaseRunning
	}
	createdBy := ownerUserID
	ancestry := []string{ownerUserID}
	if parentAgent != nil {
		createdBy = parentAgent.ID
		ancestry = append(append([]string(nil), parentAgent.Ancestry...), parentAgent.ID)
	}
	require.NoError(t, sf.s.CreateAgent(ctx, &store.Agent{
		ID:        id,
		Slug:      name,
		Name:      name,
		ProjectID: projectID,
		Phase:     string(phase),
		CreatedBy: createdBy,
		OwnerID:   ownerUserID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: string(AgentRoleFull),
		},
		Ancestry: ancestry,
	}))
	a, err := sf.s.GetAgent(ctx, id)
	require.NoError(t, err)

	edge := &store.DelegationEdge{
		DelegateType:        store.DelegationPrincipalAgent,
		DelegateID:          id,
		ScopeType:           store.RoleScopeProject,
		ScopeID:             projectID,
		Role:                string(AgentRoleFull),
		Active:              true,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, ownerUserID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	if parentAgent == nil {
		edge.DelegatorType = store.DelegationPrincipalUser
		edge.DelegatorID = ownerUserID
	} else {
		edge.DelegatorType = store.DelegationPrincipalAgent
		edge.DelegatorID = parentAgent.ID
	}
	require.NoError(t, sf.s.CreateDelegationEdge(ctx, edge))
	return a
}

func (sf *successionFixture) doReq(t *testing.T, ident Identity, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	reqCtx := contextWithIdentity(req.Context(), ident)
	reqCtx = contextWithCredentialContext(reqCtx, credentialContextForIdentity(ident))
	req = req.WithContext(reqCtx)
	rec := httptest.NewRecorder()
	sf.srv.mux.ServeHTTP(rec, req)
	return rec
}

func artifactManifestBody(scope, key, title, kind, content string) []byte {
	sum := sha256.Sum256([]byte(content))
	payload := map[string]any{
		"entry": "doc.md",
		"files": []map[string]any{{
			"path":   "doc.md",
			"size":   len(content),
			"sha256": hex.EncodeToString(sum[:]),
		}},
	}
	if scope != "" {
		payload["scope"] = scope
	}
	if key != "" {
		payload["key"] = key
	}
	if title != "" {
		payload["title"] = title
	}
	if kind != "" {
		payload["kind"] = kind
	}
	b, _ := json.Marshal(payload)
	return b
}

func (sf *successionFixture) publishArtifactAsAgent(t *testing.T, agent *store.Agent, key, title string) string {
	t.Helper()
	ident := artifactTestAgent(agent.ID, agent.ProjectID, ScopesForRole(AgentRoleFull)...)
	content := "# Hello\n"
	rec := sf.doReq(t, ident, http.MethodPost, "/api/v1/artifacts", artifactManifestBody(agent.ProjectID, key, title, "", content))
	require.Equal(t, http.StatusCreated, rec.Code, "publish as %s: %s", agent.Slug, rec.Body.String())
	var pend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pend))
	id := pend.Artifact.ID
	putRec := sf.doReq(t, ident, http.MethodPut, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/files/doc.md", id, pend.Version.Seq), []byte(content))
	require.Equal(t, http.StatusNoContent, putRec.Code, "upload file: %s", putRec.Body.String())
	finRec := sf.doReq(t, ident, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", id, pend.Version.Seq), nil)
	require.Equal(t, http.StatusOK, finRec.Code, "finalize: %s", finRec.Body.String())
	return id
}

// deleteAgentViaEngine deletes agentID through the HTTP DELETE /api/v1/agents/{id}
// route (which runs the full delete engine and its post-commit tail steps).
func (sf *successionFixture) deleteAgentViaEngine(t *testing.T, actorUserID, agentID string, hard bool) {
	t.Helper()
	path := "/api/v1/agents/" + agentID
	if hard {
		path += "?hard=true"
	}
	rec := sf.doReq(t, sf.user(actorUserID), http.MethodDelete, path, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "delete agent %s (hard=%v): %s", agentID, hard, rec.Body.String())
}

// TestArtifactSuccession_SoftDeleteHappyPath (H1): user creates P, P creates C,
// C publishes artifact X, C is soft-deleted through the delete engine. Before
// any reconcile pass, X's owner is P; P can immediately request reviews and
// publish new versions; a human submits a review and the notice goes to P;
// MutationAuditRecord exists with authority ancestor_succession; no DM was sent
// to P for the transfer itself.
func TestArtifactSuccession_SoftDeleteHappyPath(t *testing.T) {
	sf := newSuccessionFixture(t)
	ctx := context.Background()

	parent := sf.createChainAgent(t, "parent-h1", sf.home, sf.delegator, nil, state.PhaseRunning)
	child := sf.createChainAgent(t, "child-h1", sf.home, sf.delegator, parent, state.PhaseRunning)

	artID := sf.publishArtifactAsAgent(t, child, "h1-doc", "Child Doc")

	// Soft-delete child through the delete engine.
	sf.deleteAgentViaEngine(t, sf.delegator, child.ID, false)

	// Confirm child was soft-deleted (row retained with DeletedAt set).
	deletedChild, err := sf.s.GetAgent(ctx, child.ID)
	require.NoError(t, err)
	require.False(t, deletedChild.DeletedAt.IsZero(), "expected soft delete")

	// Immediately (before any reconcile pass), owner is parent.
	art, err := sf.st.GetArtifact(ctx, artID)
	require.NoError(t, err)
	assert.Equal(t, artifacts.PrincipalKindAgent, art.OwnerKind)
	assert.Equal(t, parent.ID, art.OwnerRef)
	assert.Equal(t, child.ID, art.FormerOwnerRef)
	assert.Nil(t, art.OrphanedAt)

	// No transfer DM was sent to parent.
	assert.Empty(t, sf.dispatchedCalls(), "automatic succession must not send chat/DM notices")

	// MutationAuditRecord exists with type artifact.ownership_transfer and authority ancestor_succession.
	audits, _, err := sf.s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: mutationTypeArtifactOwnershipTransfer,
		TargetID:     artID,
	})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Contains(t, audits[0].BeforeSummary, "agent:"+child.ID)
	assert.Contains(t, audits[0].AfterSummary, "agent:"+parent.ID)
	assert.Contains(t, audits[0].AfterSummary, artifacts.AuthorityAncestorSuccession)

	// artifact_event records owner_transferred with system / agent_delete / ancestor_succession.
	events, err := sf.st.ListEvents(ctx, artID)
	require.NoError(t, err)
	var foundTransferEvent bool
	for _, ev := range events {
		if ev.Action == artifacts.EventOwnerTransferred {
			foundTransferEvent = true
			assert.Equal(t, artifacts.ActorKindSystem, ev.ActorKind)
			assert.Equal(t, artifacts.SuccessionTriggerAgentDelete, ev.ActorRef)
			assert.Equal(t, artifacts.AuthorityAncestorSuccession, ev.Authority)
		}
	}
	assert.True(t, foundTransferEvent, "expected owner_transferred event")

	parentIdent := artifactTestAgent(parent.ID, sf.home, ScopesForRole(AgentRoleFull)...)

	// Parent can immediately POST /review-requests (with bounded grant to f.member).
	reqBody, _ := json.Marshal(artifacts.CreateReviewRequestsBody{
		Reviewers: []string{"user:" + sf.member},
		Note:      "Please review child's work",
	})
	recRR := sf.doReq(t, parentIdent, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", reqBody)
	require.Equal(t, http.StatusOK, recRR.Code, "parent POST /review-requests: %s", recRR.Body.String())

	// Parent can immediately POST /versions (append and finalize version 2).
	v2Content := "# Hello v2\n"
	recV2 := sf.doReq(t, parentIdent, http.MethodPost, "/api/v1/artifacts/"+artID+"/versions", artifactManifestBody("", "", "", "", v2Content))
	require.Equal(t, http.StatusCreated, recV2.Code, "parent POST /versions: %s", recV2.Body.String())
	var v2Pend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(recV2.Body.Bytes(), &v2Pend))
	require.Equal(t, http.StatusNoContent, sf.doReq(t, parentIdent, http.MethodPut, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/files/doc.md", artID, v2Pend.Version.Seq), []byte(v2Content)).Code)
	require.Equal(t, http.StatusOK, sf.doReq(t, parentIdent, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", artID, v2Pend.Version.Seq), nil).Code)

	// Human reviewer (f.member) submits a review -> review notice is addressed to parent.
	revContent := "# Hello v2{>>Looks good!<<}\n"
	recRev := sf.doReq(t, sf.user(sf.member), http.MethodPost, "/api/v1/artifacts/"+artID+"/versions", artifactManifestBody("", "", "", artifacts.VersionKindReview, revContent))
	require.Equal(t, http.StatusCreated, recRev.Code, "human review create: %s", recRev.Body.String())
	var revPend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(recRev.Body.Bytes(), &revPend))
	require.Equal(t, http.StatusNoContent, sf.doReq(t, sf.user(sf.member), http.MethodPut, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/files/doc.md", artID, revPend.Version.Seq), []byte(revContent)).Code)
	finRev := sf.doReq(t, sf.user(sf.member), http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", artID, revPend.Version.Seq), []byte(`{"base":2}`))
	require.Equal(t, http.StatusOK, finRev.Code, "human review finalize: %s", finRev.Body.String())

	// Wait briefly for best-effort review notice dispatch.
	require.Eventually(t, func() bool {
		for _, c := range sf.dispatchedCalls() {
			if c.Agent != nil && c.Agent.ID == parent.ID {
				return true
			}
		}
		return false
	}, 2*time.Second, 20*time.Millisecond, "review notice should be delivered to parent agent")
}
