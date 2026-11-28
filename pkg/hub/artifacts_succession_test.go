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
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    id,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	if parentAgent == nil {
		edge.DelegatorType = store.DelegationPrincipalUser
		edge.DelegatorID = ownerUserID
		edge.AuthorityProvenance = recordedProv(store.DelegationPrincipalUser, ownerUserID, store.SourceCredentialSession)
	} else {
		edge.DelegatorType = store.DelegationPrincipalAgent
		edge.DelegatorID = parentAgent.ID
		edge.AuthorityProvenance = recordedProv(store.DelegationPrincipalAgent, parentAgent.ID, store.SourceCredentialAgent)
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
		path += "?force=true"
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
	var audits []*store.MutationAuditRecord
	require.Eventually(t, func() bool {
		var listErr error
		audits, _, listErr = sf.s.ListMutationAudits(ctx, store.MutationAuditFilter{
			MutationType: mutationTypeArtifactOwnershipTransfer,
			TargetID:     artID,
		})
		return listErr == nil && len(audits) == 1
	}, 2*time.Second, 20*time.Millisecond, "expected ownership transfer MutationAuditRecord")
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

// TestArtifactSuccession_HardDelete (H2): hard delete transfers to parent
// immediately using the delete-engine snapshot even after the child row is gone.
func TestArtifactSuccession_HardDelete(t *testing.T) {
	sf := newSuccessionFixture(t)
	ctx := context.Background()

	parent := sf.createChainAgent(t, "parent-h2", sf.home, sf.delegator, nil, state.PhaseSuspended)
	child := sf.createChainAgent(t, "child-h2", sf.home, sf.delegator, parent, state.PhaseRunning)
	artID := sf.publishArtifactAsAgent(t, child, "h2-doc", "Hard Delete Doc")

	sf.deleteAgentViaEngine(t, sf.delegator, child.ID, true)

	_, err := sf.s.GetAgent(ctx, child.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "hard delete must remove agent row")

	art, err := sf.st.GetArtifact(ctx, artID)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, art.OwnerRef)
	assert.Equal(t, child.ID, art.FormerOwnerRef)
	assert.Nil(t, art.OrphanedAt)

	parentIdent := artifactTestAgent(parent.ID, sf.home, ScopesForRole(AgentRoleFull)...)
	reqBody, _ := json.Marshal(artifacts.CreateReviewRequestsBody{Reviewers: []string{"user:" + sf.member}})
	recRR := sf.doReq(t, parentIdent, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", reqBody)
	require.Equal(t, http.StatusOK, recRR.Code, recRR.Body.String())

	v2Content := "# Hard delete v2\n"
	recV2 := sf.doReq(t, parentIdent, http.MethodPost, "/api/v1/artifacts/"+artID+"/versions", artifactManifestBody("", "", "", "", v2Content))
	require.Equal(t, http.StatusCreated, recV2.Code, recV2.Body.String())
}

// TestArtifactSuccession_MultiLevel (H3): user -> G -> P -> C. P is soft-deleted
// first, then C is deleted -> X goes to G.
func TestArtifactSuccession_MultiLevel(t *testing.T) {
	sf := newSuccessionFixture(t)
	ctx := context.Background()

	grandparent := sf.createChainAgent(t, "gp-h3", sf.home, sf.delegator, nil, state.PhaseRunning)
	parent := sf.createChainAgent(t, "parent-h3", sf.home, sf.delegator, grandparent, state.PhaseRunning)
	child := sf.createChainAgent(t, "child-h3", sf.home, sf.delegator, parent, state.PhaseRunning)
	artID := sf.publishArtifactAsAgent(t, child, "", "Multi-level Doc")

	sf.deleteAgentViaEngine(t, sf.delegator, parent.ID, false)
	sf.deleteAgentViaEngine(t, sf.delegator, child.ID, false)

	art, err := sf.st.GetArtifact(ctx, artID)
	require.NoError(t, err)
	assert.Equal(t, grandparent.ID, art.OwnerRef)
	assert.Equal(t, child.ID, art.FormerOwnerRef)
}

// TestArtifactSuccession_IneligibleParentWalkUpAndOrphanFallback (H4 & H5):
//   - H4a: held, reincarnating, or deleting parent is skipped -> walks up to G.
//   - H4b: no eligible ancestor -> stays with C, reconcile marks orphaned_at,
//     human steward can share, manually transfer, and retire.
//   - H5: user-created child (Ancestry=[u]) -> no transfer, orphaned on reconcile.
func TestArtifactSuccession_IneligibleParentWalkUpAndOrphanFallback(t *testing.T) {
	t.Run("held_reincarnating_deleting_walk_up", func(t *testing.T) {
		for _, mode := range []string{"held", "reincarnating", "deleting"} {
			t.Run(mode, func(t *testing.T) {
				sf := newSuccessionFixture(t)
				ctx := context.Background()

				grandparent := sf.createChainAgent(t, "gp-h4-"+mode, sf.home, sf.delegator, nil, state.PhaseRunning)
				parent := sf.createChainAgent(t, "parent-h4-"+mode, sf.home, sf.delegator, grandparent, state.PhaseRunning)
				child := sf.createChainAgent(t, "child-h4-"+mode, sf.home, sf.delegator, parent, state.PhaseRunning)
				artID := sf.publishArtifactAsAgent(t, child, "", "Walk Up "+mode)

				switch mode {
				case "held":
					_, err := sf.s.CreateAgentHolds(ctx, []*store.AgentHold{{
						AgentID:           parent.ID,
						ProjectID:         sf.home,
						Cause:             store.AgentHoldCauseOwnerAccessEnded,
						RootPrincipalType: store.AgentHoldRootUser,
						RootPrincipalID:   sf.delegator,
						Trigger:           store.MembershipLossTriggerMemberRemove,
						ActorKind:         "system",
						ActorID:           "hub",
						CorrelationID:     "h4-hold",
					}})
					require.NoError(t, err)
				case "reincarnating":
					parent.ReincarnationState = store.ReincarnationStatePending
					require.NoError(t, sf.s.UpdateAgent(ctx, parent))
				case "deleting":
					deleting := store.DeletionStateDeleting
					n, err := sf.s.UpdateAgentDeletion(ctx, parent.ID,
						store.DeletionPredicate{States: []string{store.DeletionStateNone}},
						store.DeletionFields{State: &deleting})
					require.NoError(t, err)
					require.Equal(t, 1, n)
				}

				sf.deleteAgentViaEngine(t, sf.delegator, child.ID, false)

				art, err := sf.st.GetArtifact(ctx, artID)
				require.NoError(t, err)
				assert.Equal(t, grandparent.ID, art.OwnerRef, "ineligible parent (%s) must be skipped for grandparent", mode)
			})
		}
	})

	t.Run("no_eligible_ancestor_and_user_created_child", func(t *testing.T) {
		sf := newSuccessionFixture(t)
		ctx := context.Background()

		// H4b: only parent exists and is held -> no eligible ancestor.
		heldParent := sf.createChainAgent(t, "held-parent-h4", sf.home, sf.delegator, nil, state.PhaseRunning)
		child := sf.createChainAgent(t, "child-h4b", sf.home, sf.delegator, heldParent, state.PhaseRunning)
		artH4 := sf.publishArtifactAsAgent(t, child, "", "Orphaned H4b")
		_, err := sf.s.CreateAgentHolds(ctx, []*store.AgentHold{{
			AgentID:           heldParent.ID,
			ProjectID:         sf.home,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   sf.delegator,
			Trigger:           store.MembershipLossTriggerMemberRemove,
			ActorKind:         "system",
			ActorID:           "hub",
			CorrelationID:     "h4b-hold",
		}})
		require.NoError(t, err)

		// H5: user-created root agent (Ancestry=[delegator]).
		rootChild := sf.createChainAgent(t, "root-child-h5", sf.home, sf.delegator, nil, state.PhaseRunning)
		artH5 := sf.publishArtifactAsAgent(t, rootChild, "", "Orphaned H5")

		// Live target agent for manual steward transfer check.
		liveTarget := sf.createChainAgent(t, "live-target-h4", sf.home, sf.delegator, nil, state.PhaseRunning)

		sf.deleteAgentViaEngine(t, sf.delegator, child.ID, false)
		sf.deleteAgentViaEngine(t, sf.delegator, rootChild.ID, false)

		// Neither transferred; reconcile marks both orphaned.
		sf.srv.reapArtifactVersions(ctx)
		for _, tc := range []struct {
			name, id, owner string
		}{
			{"H4b", artH4, child.ID},
			{"H5", artH5, rootChild.ID},
		} {
			art, err := sf.st.GetArtifact(ctx, tc.id)
			require.NoError(t, err)
			assert.Equal(t, tc.owner, art.OwnerRef, tc.name)
			assert.NotNil(t, art.OrphanedAt, "%s should be marked orphaned", tc.name)
		}

		// Human steward (delegator) can still share, manually transfer, and retire.
		grantRec := sf.doReq(t, sf.user(sf.delegator), http.MethodPost, "/api/v1/artifacts/"+artH4+"/grants",
			[]byte(fmt.Sprintf(`{"subjectKind":"principal","subjectRef":"user:%s","permission":"read"}`, sf.member)))
		require.Equal(t, http.StatusCreated, grantRec.Code, grantRec.Body.String())

		transferRec := sf.doReq(t, sf.user(sf.delegator), http.MethodPost, "/api/v1/artifacts/"+artH4+"/transfer",
			[]byte(fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, liveTarget.ID, child.ID)))
		require.Equal(t, http.StatusOK, transferRec.Code, transferRec.Body.String())

		delRec := sf.doReq(t, sf.user(sf.delegator), http.MethodDelete, "/api/v1/artifacts/"+artH5, nil)
		require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())
	})
}

// TestArtifactSuccession_CrossProjectKeyCollisionRestoreAndAgentRefusals (H6, H7, H8, H9):
//   - H6: artifact moved to another project is not auto-transferred and is orphaned.
//   - H7: key collision clears child's key into former_key while parent's key stays intact.
//   - H8: restoring soft-deleted child does not revert transfer; next reconcile makes no change.
//   - H9: parent (new owner) is still refused (403) on user-only routes: grants, links, PATCH, DELETE, transfer.
func TestArtifactSuccession_CrossProjectKeyCollisionRestoreAndAgentRefusals(t *testing.T) {
	sf := newSuccessionFixture(t)
	ctx := context.Background()

	parent := sf.createChainAgent(t, "parent-h6789", sf.home, sf.delegator, nil, state.PhaseRunning)
	child := sf.createChainAgent(t, "child-h6789", sf.home, sf.delegator, parent, state.PhaseRunning)
	otherAgent := sf.createChainAgent(t, "other-h9", sf.home, sf.delegator, nil, state.PhaseRunning)

	// H7: parent and child both publish with key "shared-key".
	parentKeyArtID := sf.publishArtifactAsAgent(t, parent, "shared-key", "Parent Keyed")
	childKeyArtID := sf.publishArtifactAsAgent(t, child, "shared-key", "Child Keyed")

	// H6: child publishes an artifact, then admin moves it to sf.other.
	movedArtID := sf.publishArtifactAsAgent(t, child, "moved-key", "Moved Out")
	_, err := sf.st.UpdateArtifact(ctx, movedArtID, artifacts.ArtifactUpdate{
		HomeGrant: &artifacts.Grant{
			ID:           uuid.NewString(),
			ArtifactID:   movedArtID,
			SubjectKind:  artifacts.SubjectScope,
			SubjectRef:   sf.other,
			Permission:   artifacts.GrantRead,
			CreatedByRef: sf.owner,
			CreatedAt:    time.Now().UTC(),
		},
		MaxGrants: artifacts.MaxGrantsPerArtifact,
		Actor: &artifacts.Actor{
			Kind:      artifacts.PrincipalKindUser,
			Ref:       sf.owner,
			Authority: artifacts.AuthorityAdminGrant,
		},
	})
	require.NoError(t, err)

	// Soft-delete child.
	sf.deleteAgentViaEngine(t, sf.delegator, child.ID, false)

	// H6: moved artifact stays owned by child (not transferred to parent) and is orphaned on reconcile.
	sf.srv.reapArtifactVersions(ctx)
	movedArt, err := sf.st.GetArtifact(ctx, movedArtID)
	require.NoError(t, err)
	assert.Equal(t, sf.other, movedArt.ScopeRef)
	assert.Equal(t, child.ID, movedArt.OwnerRef)
	assert.NotNil(t, movedArt.OrphanedAt)

	// H7: child's colliding artifact transferred with key cleared; parent's key still resolves to parent's original artifact.
	childKeyArt, err := sf.st.GetArtifact(ctx, childKeyArtID)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, childKeyArt.OwnerRef)
	assert.Empty(t, childKeyArt.Key)
	assert.Equal(t, "shared-key", childKeyArt.FormerKey)

	parentIdent := artifactTestAgent(parent.ID, sf.home, ScopesForRole(AgentRoleFull)...)
	repubRec := sf.doReq(t, parentIdent, http.MethodPost, "/api/v1/artifacts", artifactManifestBody(sf.home, "shared-key", "Parent Keyed v2", "", "# v2\n"))
	require.Equal(t, http.StatusCreated, repubRec.Code, repubRec.Body.String())
	var repubPend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(repubRec.Body.Bytes(), &repubPend))
	assert.Equal(t, parentKeyArtID, repubPend.Artifact.ID, "publishing by key must still resolve to parent's original artifact")

	// H8: restore child -> transferred artifact stays with parent even after another reconcile pass.
	deletedChild, err := sf.s.GetAgent(ctx, child.ID)
	require.NoError(t, err)
	require.NoError(t, sf.srv.restoreAgentTx(ctx, deletedChild, AuditActor{}))
	sf.srv.reapArtifactVersions(ctx)

	afterRestore, err := sf.st.GetArtifact(ctx, childKeyArtID)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, afterRestore.OwnerRef)
	assert.Nil(t, afterRestore.OrphanedAt)

	// H9: parent (now owner) still gets 403 on user-only routes.
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"POST /grants", http.MethodPost, "/api/v1/artifacts/" + childKeyArtID + "/grants",
			fmt.Sprintf(`{"subjectKind":"principal","subjectRef":"user:%s","permission":"read"}`, sf.member)},
		{"POST /links", http.MethodPost, "/api/v1/artifacts/" + childKeyArtID + "/links", `{}`},
		{"PATCH /{id}", http.MethodPatch, "/api/v1/artifacts/" + childKeyArtID, `{"expiresIn":"48h"}`},
		{"DELETE /{id}", http.MethodDelete, "/api/v1/artifacts/" + childKeyArtID, ""},
		{"POST /transfer", http.MethodPost, "/api/v1/artifacts/" + childKeyArtID + "/transfer",
			fmt.Sprintf(`{"to":"agent:%s","expectOwner":"agent:%s"}`, otherAgent.ID, parent.ID)},
	} {
		rec := sf.doReq(t, parentIdent, tc.method, tc.path, []byte(tc.body))
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", tc.name, rec.Body.String())
	}
}

// TestArtifactSuccession_GateOff (H10): with hub.artifacts disabled, deleting C
// performs no transfer and ResolveOwnerSuccessor answers not-found.
func TestArtifactSuccession_GateOff(t *testing.T) {
	sf := newSuccessionFixture(t)
	ctx := context.Background()

	parent := sf.createChainAgent(t, "parent-h10", sf.home, sf.delegator, nil, state.PhaseRunning)
	child := sf.createChainAgent(t, "child-h10", sf.home, sf.delegator, parent, state.PhaseRunning)
	artID := sf.publishArtifactAsAgent(t, child, "", "Gate Off Doc")

	// Disable hub.artifacts experiment.
	var inactive []experiments.Experiment
	for _, e := range sf.srv.experimentRegistry().All() {
		if e.Name == experiments.Artifacts {
			e.Default = false
		}
		inactive = append(inactive, e)
	}
	reg, err := experiments.NewRegistry(inactive, nil)
	require.NoError(t, err)
	sf.srv.experiments = reg

	sf.deleteAgentViaEngine(t, sf.delegator, child.ID, false)

	art, err := sf.st.GetArtifact(ctx, artID)
	require.NoError(t, err)
	assert.Equal(t, child.ID, art.OwnerRef, "no transfer when hub.artifacts is disabled")

	_, found, err := newArtifactHost(sf.srv).ResolveOwnerSuccessor(ctx, child.ID)
	require.NoError(t, err)
	assert.False(t, found, "ResolveOwnerSuccessor must return not-found when gate is off")
}

// TestArtifactSuccession_ReconcilerBackstopAndChainedInherit (H11 & H12):
//   - H11a: soft-delete C with missed tail step -> reapArtifactVersions moves X to P with actor_ref=reconciler.
//   - H11b: hard-delete C with missed tail step -> X is orphaned (documents OQ-2).
//   - H12: C deleted -> X goes to P; then P soft-deleted with missed tail step -> reconcile moves X to G.
func TestArtifactSuccession_ReconcilerBackstopAndChainedInherit(t *testing.T) {
	sf := newSuccessionFixture(t)
	ctx := context.Background()

	grandparent := sf.createChainAgent(t, "gp-h1112", sf.home, sf.delegator, nil, state.PhaseRunning)
	parent := sf.createChainAgent(t, "parent-h1112", sf.home, sf.delegator, grandparent, state.PhaseRunning)
	childSoft := sf.createChainAgent(t, "child-soft-h11", sf.home, sf.delegator, parent, state.PhaseRunning)
	childHard := sf.createChainAgent(t, "child-hard-h11", sf.home, sf.delegator, parent, state.PhaseRunning)

	artSoftID := sf.publishArtifactAsAgent(t, childSoft, "", "Backstop Soft")
	artHardID := sf.publishArtifactAsAgent(t, childHard, "", "Backstop Hard")

	// Simulate missed tail step by temporarily unsetting the artifact store during delete.
	sf.srv.SetArtifactStore(nil)
	sf.deleteAgentViaEngine(t, sf.delegator, childSoft.ID, false)
	sf.deleteAgentViaEngine(t, sf.delegator, childHard.ID, true)
	sf.srv.SetArtifactStore(sf.st)

	// Before reconcile, both still belong to their deleted children.
	aSoft, _ := sf.st.GetArtifact(ctx, artSoftID)
	aHard, _ := sf.st.GetArtifact(ctx, artHardID)
	require.Equal(t, childSoft.ID, aSoft.OwnerRef)
	require.Equal(t, childHard.ID, aHard.OwnerRef)

	// Reconcile backstop repairs the soft-deleted child and orphans the hard-deleted child.
	sf.srv.reapArtifactVersions(ctx)

	aSoft, err := sf.st.GetArtifact(ctx, artSoftID)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, aSoft.OwnerRef)
	assert.Nil(t, aSoft.OrphanedAt)
	events, err := sf.st.ListEvents(ctx, artSoftID)
	require.NoError(t, err)
	var sawReconciler bool
	for _, ev := range events {
		if ev.Action == artifacts.EventOwnerTransferred && ev.ActorRef == artifacts.SuccessionTriggerReconciler {
			sawReconciler = true
		}
	}
	assert.True(t, sawReconciler, "expected reconciler owner_transferred event")

	// Confirm reconciler backstop emitted a valid hub MutationAuditRecord.
	require.Eventually(t, func() bool {
		audits, _, err := sf.s.ListMutationAudits(ctx, store.MutationAuditFilter{
			MutationType: mutationTypeArtifactOwnershipTransfer,
			TargetID:     artSoftID,
		})
		return err == nil && len(audits) >= 1
	}, 2*time.Second, 20*time.Millisecond, "reconciler transfer must emit a MutationAuditRecord")

	aHard, err = sf.st.GetArtifact(ctx, artHardID)
	require.NoError(t, err)
	assert.Equal(t, childHard.ID, aHard.OwnerRef)
	assert.NotNil(t, aHard.OrphanedAt, "hard-deleted child with missed tail step falls back to orphan marking")

	// H12: parent now owns artSoftID; soft-delete parent with missed tail step -> reconcile moves it to grandparent.
	sf.srv.SetArtifactStore(nil)
	sf.deleteAgentViaEngine(t, sf.delegator, parent.ID, false)
	sf.srv.SetArtifactStore(sf.st)

	sf.srv.reapArtifactVersions(ctx)
	aSoft, err = sf.st.GetArtifact(ctx, artSoftID)
	require.NoError(t, err)
	assert.Equal(t, grandparent.ID, aSoft.OwnerRef)
	assert.Equal(t, parent.ID, aSoft.FormerOwnerRef)
	assert.Nil(t, aSoft.OrphanedAt)
}
