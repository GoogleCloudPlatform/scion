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
	assertArtifactViewsForBob(t, found.Artifacts, sharedArt, privateArt)

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
	assert.Contains(t, keepResp.DecisionArtifactNotice, "1 artifact reference(s) from the replaced decision prompt were not included in this --update")
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
