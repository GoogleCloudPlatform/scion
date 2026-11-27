//go:build !no_sqlite

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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecisionPrompt_ArtifactRefs_SendHistoryAndProjectView verifies that
// decision prompts accept artifact references (ptone/scion#9424), still refuse
// file attachments, include missing admitted refs in the markdown fallback,
// and enforce per-reader artifact authorization on both chat history and the
// Project Decisions pending endpoint.
func TestDecisionPrompt_ArtifactRefs_SendHistoryAndProjectView(t *testing.T) {
	f := newDecisionsFixture(t)
	st, _ := enableArtifactsForTest(t, f.srv)
	setExperimentOverrides(t, f.srv, map[string]bool{
		experiments.Decisions: true,
		experiments.Artifacts: true,
	})

	// Token-backed sender identity so artifact admission checks run under a
	// real request credential (as in production).
	senderIdent := tokenBackedSender(t, f.store, f.agent)
	topicConv, err := f.store.GetConversationByExternalRef(context.Background(), "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)
	topicConvRef := "conv:" + topicConv.ID

	// Project-scoped artifact (readable by project members alice & bob) and
	// a user-owned artifact granted only to the sending agent (readable by the
	// sending agent at send time, unreadable by bob at read time).
	sharedArt := seedMessageArtifact(t, st, f.project.ID, artifacts.PrincipalKindAgent, f.agent.ID, "Mockup v2")
	privateArt := seedAgentOnlyMessageArtifact(t, st, tid("decart-private-proj"), f.alice.ID, f.agent.ID, "Agent Private Draft")
	// Foreign artifact in another project: unreadable by the sending agent.
	foreignArt := seedMessageArtifact(t, st, tid("decart-other-proj"), artifacts.PrincipalKindUser, tid("decart-stranger"), "Secret")

	sendDecisionWithCred := func(req OutboundMessageRequest) *httptest.ResponseRecorder {
		body, err := json.Marshal(req)
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(requestAuthCtx(r.Context(), senderIdent))
		rr := httptest.NewRecorder()
		f.srv.handleAgentOutboundMessage(rr, r, f.agent.ID)
		return rr
	}

	// 1. File attachments stay refused on decision prompts.
	rrAttach := sendDecisionWithCred(OutboundMessageRequest{
		Recipient:     "user:" + f.bob.Email,
		Decisions:     decisionInputs(t, oneDecision),
		AttachmentIDs: []string{"att-1"},
	})
	assert.Equal(t, http.StatusBadRequest, rrAttach.Code)

	// 2. Send a decision prompt carrying sharedArt (in intro), privateArt (not in intro),
	//    and foreignArt (unreadable by sender -> dropped with warning).
	rr := sendDecisionWithCred(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Approve mockup? " + artifacts.FormatRef(sharedArt, 1),
		Decisions:       decisionInputs(t, oneDecision),
		Metadata: map[string]string{
			artifacts.MessageMetadataKey: refsValue(
				artifacts.MessageRef{ArtifactID: sharedArt, Seq: 1},
				artifacts.MessageRef{ArtifactID: privateArt},
				artifacts.MessageRef{ArtifactID: foreignArt},
			),
		},
	})
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	var sendResp struct {
		MessageID       string               `json:"message_id"`
		ArtifactWarning string               `json:"artifact_warning"`
		Decisions       []DecisionSendResult `json:"decisions"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &sendResp))
	require.NotEmpty(t, sendResp.MessageID)
	require.Len(t, sendResp.Decisions, 1)
	assert.Contains(t, sendResp.ArtifactWarning, "1 artifact reference(s) not attached", "foreign ref dropped")
	assert.NotContains(t, sendResp.ArtifactWarning, foreignArt, "warning must not leak dropped artifact id")

	// 3. Stored fallback body: sharedArt is in intro (not duplicated),
	//    privateArt (omitted from intro) is in **References:**, foreignArt is absent.
	storedMsg, err := f.store.GetMessage(context.Background(), sendResp.MessageID)
	require.NoError(t, err)
	assert.Contains(t, storedMsg.Msg, artifacts.FormatRef(sharedArt, 1))
	assert.Contains(t, storedMsg.Msg, "**References:**\n- "+artifacts.FormatRef(privateArt, 0))
	assert.NotContains(t, storedMsg.Msg, foreignArt)
	assert.NotContains(t, storedMsg.Msg, "Mockup v2", "fallback must never bake artifact titles")
	assert.NotContains(t, storedMsg.Msg, "Agent Private Draft", "fallback must never bake artifact titles")

	// 4. Chat history per-reader auth: bob (project member) sees sharedArt
	//    resolved (title + version) and privateArt as unavailable (no title).
	histRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/chat/conversations/"+f.topicID+"/messages", nil)
	require.Equal(t, http.StatusOK, histRR.Code, "body=%s", histRR.Body.String())

	var histResp chatHistoryResponse
	require.NoError(t, json.Unmarshal(histRR.Body.Bytes(), &histResp))
	histRefs := histResp.MessageArtifacts[sendResp.MessageID]
	require.Len(t, histRefs, 2, "only the 2 admitted refs are recorded")
	assertArtifactViewsForBob(t, histRefs, sharedArt, privateArt)

	// 5. Project Decisions pending endpoint also carries per-reader resolved artifacts.
	projRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/decisions?tab=pending", nil)
	require.Equal(t, http.StatusOK, projRR.Code, "body=%s", projRR.Body.String())

	var projResp struct {
		Prompts []ProjectDecisionPromptView `json:"prompts"`
	}
	require.NoError(t, json.Unmarshal(projRR.Body.Bytes(), &projResp))
	var found *ProjectDecisionPromptView
	for i := range projResp.Prompts {
		if projResp.Prompts[i].PromptMessageID == sendResp.MessageID {
			found = &projResp.Prompts[i]
			break
		}
	}
	require.NotNil(t, found, "expected decision prompt in project pending decisions")
	require.Len(t, found.Artifacts, 2)
	assert.False(t, found.ArtifactsUnavailable)
	assertArtifactViewsForBob(t, found.Artifacts, sharedArt, privateArt)

	// 5b. Profile Decisions pending endpoint shares the builder and carries per-reader artifacts.
	profRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/users/me/decisions?tab=pending", nil)
	require.Equal(t, http.StatusOK, profRR.Code, "body=%s", profRR.Body.String())
	var profResp struct {
		Prompts []ProjectDecisionPromptView `json:"prompts"`
	}
	require.NoError(t, json.Unmarshal(profRR.Body.Bytes(), &profResp))
	var profFound *ProjectDecisionPromptView
	for i := range profResp.Prompts {
		if profResp.Prompts[i].PromptMessageID == sendResp.MessageID {
			profFound = &profResp.Prompts[i]
			break
		}
	}
	require.NotNil(t, profFound, "expected decision prompt in profile pending decisions")
	require.Len(t, profFound.Artifacts, 2)
	assert.False(t, profFound.ArtifactsUnavailable)
	assertArtifactViewsForBob(t, profFound.Artifacts, sharedArt, privateArt)

	// 5c. When ListMessageRefs fails, both Project and Profile Decisions endpoints
	//     surface ArtifactsUnavailable=true rather than an empty list (ptone/scion#4295).
	f.srv.SetArtifactStore(failListMessageRefsStore{Store: st})
	for _, endpoint := range []string{
		"/api/v1/projects/" + f.project.ID + "/decisions?tab=pending",
		"/api/v1/users/me/decisions?tab=pending",
	} {
		errRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, endpoint, nil)
		require.Equal(t, http.StatusOK, errRR.Code, "endpoint=%s body=%s", endpoint, errRR.Body.String())
		var errResp struct {
			Prompts []ProjectDecisionPromptView `json:"prompts"`
		}
		require.NoError(t, json.Unmarshal(errRR.Body.Bytes(), &errResp))
		var uFound *ProjectDecisionPromptView
		for i := range errResp.Prompts {
			if errResp.Prompts[i].PromptMessageID == sendResp.MessageID {
				uFound = &errResp.Prompts[i]
				break
			}
		}
		require.NotNil(t, uFound, "endpoint=%s", endpoint)
		assert.Empty(t, uFound.Artifacts, "endpoint=%s", endpoint)
		assert.True(t, uFound.ArtifactsUnavailable, "endpoint=%s must surface artifactsUnavailable on store error", endpoint)
	}
	f.srv.SetArtifactStore(st)

	// 6. --update replaces artifact refs explicitly and reports a notice when
	//    refs from the replaced prompt were omitted; keeping the same artifact
	//    ID (even without @seq or with a new @seq) emits no drop notice for it.
	rrKeep := sendDecisionWithCred(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Revised mockup",
		Decisions:       decisionInputs(t, oneDecision),
		DecisionsUpdate: true,
		Metadata: map[string]string{
			artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: sharedArt}),
		},
	})
	require.Equal(t, http.StatusOK, rrKeep.Code, "body=%s", rrKeep.Body.String())
	var keepResp struct {
		MessageID              string `json:"message_id"`
		DecisionArtifactNotice string `json:"decision_artifact_notice"`
	}
	require.NoError(t, json.Unmarshal(rrKeep.Body.Bytes(), &keepResp))
	assert.Contains(t, keepResp.DecisionArtifactNotice, "1 artifact reference(s) from the replaced decision prompt are not attached to the new prompt")
	assert.Contains(t, keepResp.DecisionArtifactNotice, artifacts.FormatRef(privateArt, 0))
	assert.NotContains(t, keepResp.DecisionArtifactNotice, sharedArt, "kept artifact id must not be reported as dropped")

	// Updating again with no --artifact drops sharedArt and reports it.
	rrDropAll := sendDecisionWithCred(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Text only revision",
		Decisions:       decisionInputs(t, oneDecision),
		DecisionsUpdate: true,
	})
	require.Equal(t, http.StatusOK, rrDropAll.Code, "body=%s", rrDropAll.Body.String())
	var dropAllResp struct {
		MessageID              string `json:"message_id"`
		DecisionArtifactNotice string `json:"decision_artifact_notice"`
	}
	require.NoError(t, json.Unmarshal(rrDropAll.Body.Bytes(), &dropAllResp))
	assert.Contains(t, dropAllResp.DecisionArtifactNotice, artifacts.FormatRef(sharedArt, 0))
	recordedAfterDrop, err := st.ListMessageRefs(context.Background(), []string{dropAllResp.MessageID})
	require.NoError(t, err)
	assert.Empty(t, recordedAfterDrop[dropAllResp.MessageID], "omitting --artifact on --update attaches no refs")

	// 7. With hub.artifacts disabled, sending a decision prompt with artifact
	//    metadata still succeeds and returns the disabled warning.
	setExperimentOverrides(t, f.srv, map[string]bool{
		experiments.Decisions: true,
		experiments.Artifacts: false,
	})
	rrOff := sendDecisionWithCred(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Second check",
		Decisions:       decisionInputs(t, `[{"id":"D235","question":"Proceed?","choices":["yes","no"]}]`),
		Metadata: map[string]string{
			artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: sharedArt}),
		},
	})
	require.Equal(t, http.StatusOK, rrOff.Code, "body=%s", rrOff.Body.String())
	var offResp struct {
		ArtifactWarning string `json:"artifact_warning"`
	}
	require.NoError(t, json.Unmarshal(rrOff.Body.Bytes(), &offResp))
	assert.Contains(t, offResp.ArtifactWarning, "not enabled on this hub")
}

func assertArtifactViewsForBob(t *testing.T, refs []chatArtifactRef, sharedID, privateID string) {
	t.Helper()
	byID := make(map[string]chatArtifactRef, len(refs))
	for _, r := range refs {
		byID[r.ID] = r
	}
	shared, ok := byID[sharedID]
	require.True(t, ok, "missing shared artifact %s", sharedID)
	assert.True(t, shared.Available)
	assert.Equal(t, "Mockup v2", shared.Title)
	assert.Equal(t, 1, shared.Version)

	priv, ok := byID[privateID]
	require.True(t, ok, "missing private artifact %s", privateID)
	assert.False(t, priv.Available, "private artifact must be unavailable to non-owner reader")
	assert.Empty(t, priv.Title, "unavailable artifact must not leak title")
	assert.Zero(t, priv.Version, "unavailable artifact must not leak version")
	assert.Empty(t, priv.OwnerName, "unavailable artifact must not leak owner")
}

// seedAgentOnlyMessageArtifact publishes an artifact owned by ownerUserID in
// scopeRef with a read grant to granteeAgentID only (no project-scope grant),
// so granteeAgentID can attach it while another project member (bob) cannot read it.
func seedAgentOnlyMessageArtifact(t *testing.T, st artifacts.Store, scopeRef, ownerUserID, granteeAgentID, title string) string {
	t.Helper()
	now := time.Now()
	a := &artifacts.Artifact{
		ID: uuid.NewString(), ScopeKind: artifacts.ScopeKindProject, ScopeRef: scopeRef,
		OwnerKind: artifacts.PrincipalKindUser, OwnerRef: ownerUserID, Title: title, CreatedAt: now, UpdatedAt: now,
	}
	v := &artifacts.Version{
		ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "doc.md", TotalBytes: 3, FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady,
	}
	files := []artifacts.File{{
		VersionID: v.ID, Path: "doc.md", Size: 3, SHA256: strings.Repeat("ab", 32), MediaType: "text/markdown",
	}}
	grants := []artifacts.Grant{{
		ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: artifacts.SubjectPrincipal,
		SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindAgent, granteeAgentID), Permission: artifacts.GrantRead, CreatedAt: now,
	}}
	require.NoError(t, st.CreatePublished(context.Background(), a, v, files, grants))
	return a.ID
}

// failListMessageRefsStore wraps an artifacts.Store and fails ListMessageRefs.
type failListMessageRefsStore struct{ artifacts.Store }

func (failListMessageRefsStore) ListMessageRefs(context.Context, []string) (map[string][]artifacts.MessageRef, error) {
	return nil, errors.New("injected ListMessageRefs failure")
}

// TestDecisionPrompt_ArtifactRefs_UpdateEdgeCases covers --update dropped-ref
// notices for partial multi-key set updates, replaced decided rows, and
// deduplication across multiple replaced source prompts sharing an artifact.
func TestDecisionPrompt_ArtifactRefs_UpdateEdgeCases(t *testing.T) {
	f := newDecisionsFixture(t)
	st, _ := enableArtifactsForTest(t, f.srv)
	setExperimentOverrides(t, f.srv, map[string]bool{
		experiments.Decisions: true,
		experiments.Artifacts: true,
	})

	senderIdent := tokenBackedSender(t, f.store, f.agent)
	topicConv, err := f.store.GetConversationByExternalRef(context.Background(), "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)
	topicConvRef := "conv:" + topicConv.ID

	artA := seedMessageArtifact(t, st, f.project.ID, artifacts.PrincipalKindAgent, f.agent.ID, "Mockup A")
	artB := seedMessageArtifact(t, st, f.project.ID, artifacts.PrincipalKindAgent, f.agent.ID, "Mockup B")

	sendDecision := func(req OutboundMessageRequest) *httptest.ResponseRecorder {
		body, mErr := json.Marshal(req)
		require.NoError(t, mErr)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(requestAuthCtx(r.Context(), senderIdent))
		rr := httptest.NewRecorder()
		f.srv.handleAgentOutboundMessage(rr, r, f.agent.ID)
		return rr
	}

	// 1. Partial set update: send 2-key set (S1, S2) with artA, then --update
	//    only S1 without --artifact -> notice reports artA.
	rrSet := sendDecision(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Two decisions",
		Decisions:       decisionInputs(t, `[{"id":"S1","question":"Q1?","choices":["yes","no"]},{"id":"S2","question":"Q2?","choices":["yes","no"]}]`),
		Metadata: map[string]string{
			artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: artA}),
		},
	})
	require.Equal(t, http.StatusOK, rrSet.Code, "body=%s", rrSet.Body.String())

	rrPartial := sendDecision(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Revise S1 only",
		Decisions:       decisionInputs(t, `[{"id":"S1","question":"Q1 revised?","choices":["yes","no"]}]`),
		DecisionsUpdate: true,
	})
	require.Equal(t, http.StatusOK, rrPartial.Code, "body=%s", rrPartial.Body.String())
	var partialResp struct {
		DecisionArtifactNotice string `json:"decision_artifact_notice"`
	}
	require.NoError(t, json.Unmarshal(rrPartial.Body.Bytes(), &partialResp))
	assert.Contains(t, partialResp.DecisionArtifactNotice, "1 artifact reference(s) from the replaced decision prompt are not attached to the new prompt")
	assert.Contains(t, partialResp.DecisionArtifactNotice, artifacts.FormatRef(artA, 0))

	// 2. Replaced decided row: send DDec with artB, bob answers it, then
	//    --update DDec without --artifact -> notice still reports artB.
	rrDec := sendDecision(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Approve B?",
		Decisions:       decisionInputs(t, `[{"id":"DDec","question":"Ship B?","choices":["yes","no"]}]`),
		Metadata: map[string]string{
			artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: artB}),
		},
	})
	require.Equal(t, http.StatusOK, rrDec.Code, "body=%s", rrDec.Body.String())
	var decSendResp decisionSendResponse
	require.NoError(t, json.Unmarshal(rrDec.Body.Bytes(), &decSendResp))
	require.Len(t, decSendResp.Decisions, 1)

	ansRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost,
		"/api/v1/decisions/"+decSendResp.Decisions[0].RowID+"/answer",
		map[string]string{"choice_id": "1"})
	require.Equal(t, http.StatusOK, ansRR.Code, "body=%s", ansRR.Body.String())

	rrDecUpd := sendDecision(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Re-ask DDec",
		Decisions:       decisionInputs(t, `[{"id":"DDec","question":"Ship B v2?","choices":["yes","no"]}]`),
		DecisionsUpdate: true,
	})
	require.Equal(t, http.StatusOK, rrDecUpd.Code, "body=%s", rrDecUpd.Body.String())
	var decUpdResp struct {
		DecisionArtifactNotice string `json:"decision_artifact_notice"`
	}
	require.NoError(t, json.Unmarshal(rrDecUpd.Body.Bytes(), &decUpdResp))
	assert.Contains(t, decUpdResp.DecisionArtifactNotice, artifacts.FormatRef(artB, 0))

	// 3. Deduplication across two replaced source prompts sharing artA:
	//    P1 carries artA, P2 carries artA + artB; one --update replacing both
	//    without --artifact reports 2 unique refs (artA once, artB once).
	rrP1 := sendDecision(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Prompt P1",
		Decisions:       decisionInputs(t, `[{"id":"P1","question":"P1?","choices":["yes","no"]}]`),
		Metadata: map[string]string{
			artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: artA}),
		},
	})
	require.Equal(t, http.StatusOK, rrP1.Code)
	rrP2 := sendDecision(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Prompt P2",
		Decisions:       decisionInputs(t, `[{"id":"P2","question":"P2?","choices":["yes","no"]}]`),
		Metadata: map[string]string{
			artifacts.MessageMetadataKey: refsValue(
				artifacts.MessageRef{ArtifactID: artA},
				artifacts.MessageRef{ArtifactID: artB},
			),
		},
	})
	require.Equal(t, http.StatusOK, rrP2.Code)

	rrMerged := sendDecision(OutboundMessageRequest{
		ConversationRef: topicConvRef,
		Msg:             "Combine P1 and P2",
		Decisions:       decisionInputs(t, `[{"id":"P1","question":"P1 v2?","choices":["yes","no"]},{"id":"P2","question":"P2 v2?","choices":["yes","no"]}]`),
		DecisionsUpdate: true,
	})
	require.Equal(t, http.StatusOK, rrMerged.Code, "body=%s", rrMerged.Body.String())
	var mergedResp struct {
		DecisionArtifactNotice string `json:"decision_artifact_notice"`
	}
	require.NoError(t, json.Unmarshal(rrMerged.Body.Bytes(), &mergedResp))
	assert.Contains(t, mergedResp.DecisionArtifactNotice, "2 artifact reference(s) from the replaced decision prompt are not attached to the new prompt")
	assert.Equal(t, 1, strings.Count(mergedResp.DecisionArtifactNotice, artA), "shared artifact must be deduplicated in notice")
	assert.Equal(t, 1, strings.Count(mergedResp.DecisionArtifactNotice, artB))
}
