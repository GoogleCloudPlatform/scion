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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedMessageArtifact writes a ready one-file artifact homed in scopeRef
// with the home-scope read grant, and returns its id.
func seedMessageArtifact(t *testing.T, st artifacts.Store, scopeRef, ownerKind, ownerRef, title string) string {
	t.Helper()
	now := time.Now()
	a := &artifacts.Artifact{ID: uuid.NewString(), ScopeKind: artifacts.ScopeKindProject, ScopeRef: scopeRef,
		OwnerKind: ownerKind, OwnerRef: ownerRef, Title: title, CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "doc.md", TotalBytes: 3, FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	files := []artifacts.File{{VersionID: v.ID, Path: "doc.md", Size: 3, SHA256: strings.Repeat("ab", 32), MediaType: "text/markdown"}}
	grants := []artifacts.Grant{{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: artifacts.SubjectScope,
		SubjectRef: scopeRef, Permission: artifacts.GrantRead, CreatedAt: now}}
	require.NoError(t, st.CreatePublished(context.Background(), a, v, files, grants))
	return a.ID
}

// tokenBackedSender returns the request identity of agent a as the hub
// builds it from a validated agent token with the baseline role's scopes,
// and records the delegation edge the artifact host's chain check needs.
func tokenBackedSender(t *testing.T, s store.Store, a *store.Agent) *agentIdentityWrapper {
	t.Helper()
	delegator := tid("msgart-delegator-" + a.ID)
	createTestUserWithProjectRole(t, s, delegator, "delegator-"+a.Slug+"@test.com", a.ProjectID, store.ProjectRoleOwner)
	addRecordedArtifactEdge(t, s, delegator, a.ID, a.ProjectID)
	ensureEdgeBackfillComplete(t, s)
	id := artifactTestAgent(a.ID, a.ProjectID, ScopesForRole(AgentRoleBaseline)...)
	id.AgentTokenClaims.Ancestry = a.Ancestry
	return id
}

func refsValue(refs ...artifacts.MessageRef) string { return artifacts.EncodeMessageRefs(refs) }

// TestAdmitMessageArtifacts pins the admission step: only well-formed
// references the sender can read under its own credential survive; the
// other hub-reserved keys are stripped; the input is never mutated; with
// the feature off nothing is admitted.
func TestAdmitMessageArtifacts(t *testing.T) {
	srv, s, project, sender, _, _, _, _ := paritySetup(t)
	st, _ := enableArtifactsForTest(t, srv)
	ident := tokenBackedSender(t, s, sender)
	ctx := contextWithIdentity(context.Background(), ident)

	own := seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindAgent, sender.ID, "Mine")
	elsewhere := seedMessageArtifact(t, st, tid("msgart-other-project"), artifacts.PrincipalKindUser, tid("msgart-stranger"), "Not yours")
	missing := uuid.NewString()

	md := map[string]string{
		"keep":                       "me",
		messaging.MetaBodyOffloaded:  "spoofed",
		artifacts.MessageMetadataKey: `["scion://artifact/` + own + `@1","scion://artifact/` + elsewhere + `","scion://artifact/` + missing + `","junk"]`,
	}
	before := map[string]string{}
	for k, v := range md {
		before[k] = v
	}

	out, admitted, dropped := srv.admitMessageArtifacts(ctx, md)
	assert.Equal(t, before, md, "input metadata must not be mutated")
	assert.Equal(t, []artifacts.MessageRef{{ArtifactID: own, Seq: 1}}, admitted)
	assert.Equal(t, 3, dropped, "unreadable, missing and malformed refs are all dropped")
	assert.Equal(t, refsValue(artifacts.MessageRef{ArtifactID: own, Seq: 1}), out[artifacts.MessageMetadataKey])
	assert.Equal(t, "me", out["keep"])
	assert.NotContains(t, out, messaging.MetaBodyOffloaded)

	// The warning is the same text whatever the reason.
	assert.Equal(t, artifactRefsWarning(1), artifactRefsWarning(1))
	assert.NotContains(t, artifactRefsWarning(3), elsewhere)
	assert.Empty(t, artifactRefsWarning(0))

	// Nothing admitted: the key is absent, not empty.
	out, admitted, dropped = srv.admitMessageArtifacts(ctx, map[string]string{artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: elsewhere})})
	assert.Empty(t, admitted)
	assert.Equal(t, 1, dropped)
	assert.NotContains(t, out, artifacts.MessageMetadataKey)

	// A sender identity the artifact service does not serve (no token id,
	// as for in-process identities) admits nothing, even for its own artifact.
	inProc := contextWithIdentity(context.Background(), agentCtxIdentity(sender))
	_, admitted, dropped = srv.admitMessageArtifacts(inProc, map[string]string{artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: own})})
	assert.Empty(t, admitted)
	assert.Equal(t, 1, dropped)

	// Feature off: nothing admitted.
	reg, err := experiments.NewRegistry(experiments.Default().All(), nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.False(t, srv.experimentEnabled(experiments.Artifacts))
	out, admitted, dropped = srv.admitMessageArtifacts(ctx, map[string]string{artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: own})})
	assert.Empty(t, admitted)
	assert.Equal(t, 1, dropped)
	assert.NotContains(t, out, artifacts.MessageMetadataKey)
}

// agentCtxIdentity is the in-process agent identity shape other tests use:
// no token id, no scopes.
func agentCtxIdentity(a *store.Agent) Identity {
	return GetIdentityFromContext(agentCtx(context.Background(), a))
}

// TestMessageArtifacts_AgentDMDeliveryAndRecord: an agent DM with artifact
// references delivers only the admitted refs, in metadata and as fetch-hint
// lines in the rendered envelope (version or "current", no title); the
// persisted body is unchanged; the refs are recorded for the message; the
// sender gets a warning for what was dropped.
func TestMessageArtifacts_AgentDMDeliveryAndRecord(t *testing.T) {
	srv, s, project, sender, target, _, dispatcher, _ := paritySetup(t)
	st, _ := enableArtifactsForTest(t, srv)
	enableOffload(t, srv, 0, true) // envelope switch on, offload off
	ident := tokenBackedSender(t, s, sender)

	own := seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindAgent, sender.ID, "Design doc")
	pinned := seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindAgent, sender.ID, "Pinned")
	elsewhere := seedMessageArtifact(t, st, tid("msgart-other-project"), artifacts.PrincipalKindUser, tid("msgart-stranger"), "Secret title")

	body := "please review"
	sm := &messages.StructuredMessage{
		Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type: messages.TypeInstruction, Sender: "agent:" + sender.Slug, SenderID: sender.ID,
		Recipient: "agent:" + target.Slug, RecipientID: target.ID, Msg: body,
		Metadata: map[string]string{artifacts.MessageMetadataKey: refsValue(
			artifacts.MessageRef{ArtifactID: own},
			artifacts.MessageRef{ArtifactID: pinned, Seq: 1},
			artifacts.MessageRef{ArtifactID: elsewhere},
		)},
	}
	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: sm})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), ident))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, artifactRefsWarning(1), resp.ArtifactWarning)
	assert.NotContains(t, rr.Body.String(), "Secret title")

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	got := calls[0].StructuredMessage
	require.NotNil(t, got)
	wantRefs := []artifacts.MessageRef{{ArtifactID: own}, {ArtifactID: pinned, Seq: 1}}
	assert.Equal(t, refsValue(wantRefs...), got.Metadata[artifacts.MessageMetadataKey])
	dt := got.DeliveryText
	assert.Contains(t, dt, "Artifact: current - scion artifact get scion://artifact/"+own)
	assert.Contains(t, dt, "Artifact: v1 - scion artifact get scion://artifact/"+pinned+"@1")
	assert.NotContains(t, dt, elsewhere, "a dropped ref must not reach the recipient")
	assert.NotContains(t, dt, "Design doc", "envelopes carry no titles")
	assert.NotContains(t, dt, "Secret title")
	assert.Equal(t, body, got.Msg, "the hint lines live in the rendered envelope only")

	require.NotEmpty(t, resp.MessageID)
	row, err := s.GetMessage(context.Background(), resp.MessageID)
	require.NoError(t, err)
	assert.Equal(t, body, row.Msg, "the persisted body is unchanged")

	recorded, err := st.ListMessageRefs(context.Background(), []string{resp.MessageID})
	require.NoError(t, err)
	assert.ElementsMatch(t, wantRefs, recorded[resp.MessageID])
}

// TestMessageArtifacts_ViewsAreReadCheckedPerViewer: the web view of a
// message's references resolves under each viewer's own credential. A
// viewer who cannot read an artifact sees only the reference, exactly as
// for one that does not exist.
func TestMessageArtifacts_ViewsAreReadCheckedPerViewer(t *testing.T) {
	srv, s := testServer(t)
	st, _ := enableArtifactsForTest(t, srv)
	ctx := context.Background()

	owner := &store.User{ID: tid("msgart-owner"), Email: "owner@test.example", DisplayName: "Owner Person",
		Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, owner))

	id := seedMessageArtifact(t, st, tid("msgart-project"), artifacts.PrincipalKindUser, owner.ID, "Quarterly report")
	missing := uuid.NewString()
	require.NoError(t, st.AddMessageRefs(ctx, "msg-1", []artifacts.MessageRef{{ArtifactID: id, Seq: 1}, {ArtifactID: missing}}))
	require.NoError(t, st.AddMessageRefs(ctx, "msg-2", []artifacts.MessageRef{{ArtifactID: id}}))

	ownerCtx := contextWithIdentity(ctx, NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, "member", "web"))
	strangerCtx := contextWithIdentity(ctx, NewAuthenticatedUser(tid("msgart-stranger"), "s@test.example", "S", "member", "web"))

	views := srv.messageArtifactViews(ownerCtx, []string{"msg-1", "msg-2", "msg-none"})
	require.Len(t, views, 2)
	var readable, gone chatArtifactRef
	for _, v := range views["msg-1"] {
		if v.ID == id {
			readable = v
		} else {
			gone = v
		}
	}
	assert.True(t, readable.Available)
	assert.Equal(t, "Quarterly report", readable.Title)
	assert.Equal(t, 1, readable.Version)
	assert.Equal(t, "Owner Person", readable.OwnerName)
	assert.False(t, gone.Available)
	assert.Equal(t, chatArtifactRef{RefView: artifacts.RefView{Ref: artifacts.FormatRef(missing, 0), ID: missing}}, gone)

	for msgID, list := range srv.messageArtifactViews(strangerCtx, []string{"msg-1", "msg-2"}) {
		for _, v := range list {
			assert.False(t, v.Available, "%s: %+v", msgID, v)
			assert.Empty(t, v.Title)
			assert.Empty(t, v.OwnerName)
			assert.Empty(t, v.OwnerRef)
			assert.Zero(t, v.Version)
		}
	}

	// Feature off: no views at all.
	reg, err := experiments.NewRegistry(experiments.Default().All(), nil)
	require.NoError(t, err)
	srv.experiments = reg
	assert.Nil(t, srv.messageArtifactViews(ownerCtx, []string{"msg-1"}))
}
