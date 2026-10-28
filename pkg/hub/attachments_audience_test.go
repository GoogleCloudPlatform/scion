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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Attachments reach agents by delivery into their home; a copy in the
// project's scratchpad is made only for a whole-project audience, on every
// backend, with attachment artifacts on (the default wherever the artifact
// service is ready, local blob storage included) or off.

// localStorageHub prepares f as a hub with local blob storage and no
// object storage (a workstation or single-node hub): an artifact store, a
// hub id, a local scratchpad for project A, and a recording dispatcher. It
// stores no experiment override, so hub.attachment_artifacts resolves to
// its deployment default. It returns the scratchpad's _webchat directory
// and the dispatcher.
func localStorageHub(t *testing.T, f *refFixture) (string, *recordingDispatcher) {
	t.Helper()
	f.srv.SetHubID("local-hub")
	installLocalArtifactService(t, f.srv)
	stagingDir := f.enableScratchpad(t)
	disp := &recordingDispatcher{}
	f.srv.SetDispatcher(disp)
	require.False(t, f.srv.objectStorageConfigured(), "no object storage")
	return stagingDir, disp
}

// callTo returns the last dispatch to agent a.
func callTo(t *testing.T, disp *recordingDispatcher, a *store.Agent) dispatchCall {
	t.Helper()
	calls := disp.getCalls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Agent != nil && calls[i].Agent.ID == a.ID {
			return calls[i]
		}
	}
	t.Fatalf("no dispatch to agent %s", a.Slug)
	return dispatchCall{}
}

// TestAttachmentAudience_LocalDefaults_DMAndTopic: with the defaults on a
// local-storage hub (no object storage) a direct-conversation attachment to
// an agent is delivered to its inbox and never copied into the scratchpad;
// a project topic thread's attachment is delivered to the inbox and also
// projected. Both download from history.
func TestAttachmentAudience_LocalDefaults_DMAndTopic(t *testing.T) {
	f := newRefFixture(t)
	stagingDir, disp := localStorageHub(t, f)
	require.True(t, f.srv.experimentEnabled(experiments.AttachmentArtifacts), "on by default with local blob storage")
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.aa.Slug}))

	// Direct conversation: inbox only.
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	inDM := f.uploadAs(t, f.ua, "", "dm.txt", "dm bytes")
	resp := f.sendWith(t, f.ua, dm, inDM)
	assert.NoDirExists(t, filepath.Join(stagingDir, inDM), "no scratchpad copy of a DM attachment")
	call := callTo(t, disp, f.aa)
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	del := call.StructuredMessage.AttachmentDeliveries[0]
	assert.Equal(t, resp.ID, del.MessageID)
	assert.Empty(t, del.ProjectedPath, "no projection for a DM")
	assert.Empty(t, call.StructuredMessage.Attachments, "no scratchpad path is listed")
	plan := f.srv.attachmentDelivery().Plan(context.Background(), f.aa, call.StructuredMessage.AttachmentDeliveries)
	require.Len(t, plan, 1)
	assert.Equal(t, attachmentInboxPath(resp.ID, "dm.txt"), plan[0].InboxPath)
	got := f.download(t, f.ua, inDM)
	require.Equal(t, http.StatusOK, got.status, got.body)
	assert.Equal(t, "dm bytes", got.body)

	// Project topic thread: inbox plus projection.
	inTopic := f.uploadAs(t, f.ua, f.projA.ID, "topic.txt", "topic bytes")
	resp = f.sendWith(t, f.ua, f.topicA, inTopic)
	assert.FileExists(t, filepath.Join(stagingDir, inTopic, "topic.txt"), "a project topic thread is projected")
	call = callTo(t, disp, f.aa)
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	assert.Equal(t, resp.ID, call.StructuredMessage.AttachmentDeliveries[0].MessageID)
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat/"+inTopic+"/topic.txt",
		call.StructuredMessage.AttachmentDeliveries[0].ProjectedPath)
	got = f.download(t, f.ua, inTopic)
	require.Equal(t, http.StatusOK, got.status, got.body)
	assert.Equal(t, "topic bytes", got.body)
}

// TestAttachmentAudience_ArtifactsExplicitlyOff_DMNotDelivered: with
// attachment artifacts explicitly off, a direct-conversation attachment is
// not copied into the scratchpad and no path is sent; the agent reads a
// not-delivered line, which the stored message does not carry. The file
// still downloads from history. A project topic thread is staged as
// before.
func TestAttachmentAudience_ArtifactsExplicitlyOff_DMNotDelivered(t *testing.T) {
	f := newRefFixture(t)
	stagingDir, disp := localStorageHub(t, f)
	storeAttachmentArtifactsOverride(t, f.srv, false)
	require.False(t, f.srv.experimentEnabled(experiments.AttachmentArtifacts))
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.aa.Slug}))

	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	legacy := f.uploadAs(t, f.ua, "", "legacy.txt", "legacy bytes")
	resp := f.sendWith(t, f.ua, dm, legacy)
	assert.NoDirExists(t, filepath.Join(stagingDir, legacy), "no scratchpad copy with artifacts off")
	call := callTo(t, disp, f.aa)
	assert.Empty(t, call.StructuredMessage.Attachments, "no path is listed")
	assert.Empty(t, call.StructuredMessage.AttachmentMounts)
	assert.Empty(t, call.StructuredMessage.AttachmentDeliveries)
	assert.Contains(t, call.StructuredMessage.Msg, "Attachment not delivered: legacy.txt")
	assert.Contains(t, call.Message, "Attachment not delivered: legacy.txt")
	stored, err := f.st.GetMessagesByIDs(context.Background(), []string{resp.ID})
	require.NoError(t, err)
	require.NotNil(t, stored[resp.ID])
	assert.NotContains(t, stored[resp.ID].Msg, "Attachment not delivered", "history keeps the sender's text")
	got := f.download(t, f.ua, legacy)
	require.Equal(t, http.StatusOK, got.status, got.body)
	assert.Equal(t, "legacy bytes", got.body)

	inTopic := f.uploadAs(t, f.ua, f.projA.ID, "topic.txt", "topic bytes")
	f.sendWith(t, f.ua, f.topicA, inTopic)
	assert.FileExists(t, filepath.Join(stagingDir, inTopic, "topic.txt"), "a project topic thread is still staged")
	call = callTo(t, disp, f.aa)
	assert.NotContains(t, call.StructuredMessage.Msg, "Attachment not delivered")
}

// TestAttachmentAudience_ParticipantGroupKeyNotStaged: a legacy attachment
// on a participant-list group conversation's key is not copied into a
// local scratchpad and is reported as not delivered.
func TestAttachmentAudience_ParticipantGroupKeyNotStaged(t *testing.T) {
	srv, s, project, sharedDir := agentAttachmentServer(t)
	group := seedGroupConversation(t, s, project.ID, "crew")
	ref := saveLegacyAttachment(t, srv, project.ID, "group.txt", "group bytes")
	for _, key := range []string{group, "group:" + project.ID + ":crew"} {
		paths, mounts, notDelivered := srv.stageChatAttachments(context.Background(), key, project.ID, api.NewUUID(), []AttachmentRef{ref})
		assert.Empty(t, paths, key)
		assert.Empty(t, mounts, key)
		assert.Len(t, notDelivered, 1, key)
	}
	assert.NoDirExists(t, filepath.Join(sharedDir, ".attachments", "_webchat", ref.ID))
}

// TestAttachmentAudience_TwoUsers: two users each have an agent in the same
// project on a local-storage hub. A direct-conversation attachment from
// user A to A's agent is not readable through user B's agent, with the
// defaults or with attachment artifacts explicitly off: no copy reaches the
// scratchpad both agents mount, the artifact service refuses B's agent, and
// B cannot download it.
func TestAttachmentAudience_TwoUsers(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	stagingDir, _ := localStorageHub(t, f)
	ab := &store.Agent{ID: tid("ref-agent-b-in-a"), ProjectID: f.projA.ID, Name: "ab", Slug: "ab",
		Phase: "running", OwnerID: f.ub.ID, CreatedBy: f.ub.ID}
	require.NoError(t, f.st.CreateAgent(ctx, ab))
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	missing := f.download(t, f.ub, uuid.NewString())

	// Defaults: stored as an artifact, delivered to A's agent only.
	id := f.uploadAs(t, f.ua, "", "private.txt", "for aa")
	resp := f.sendWith(t, f.ua, dm, id)
	assert.NoDirExists(t, filepath.Join(stagingDir, id))
	svc := f.srv.attachmentService()
	found, err := svc.MessageAttachments(ctx, []string{resp.ID})
	require.NoError(t, err)
	require.Len(t, found[resp.ID], 1)
	assert.False(t, svc.MayReadAttachments(withStandingMemo(storedAgentContext(ab)), resp.ID, found[resp.ID][0].MessageProjectID),
		"user B's agent does not read the attachment")
	requireSameAnswer(t, missing, f.download(t, f.ub, id))

	// Explicitly off: a legacy upload, never copied where B's agent reads.
	storeAttachmentArtifactsOverride(t, f.srv, false)
	legacy := f.uploadAs(t, f.ua, "", "private-legacy.txt", "for aa too")
	f.sendWith(t, f.ua, dm, legacy)
	assert.NoDirExists(t, filepath.Join(stagingDir, legacy))
	requireSameAnswer(t, missing, f.download(t, f.ub, legacy))
}

// TestAttachmentAudience_LocalDefaults_ParticipantGroupSend: end to end on
// a local-storage hub with the defaults, a message with an attachment sent
// into a participant-list group conversation is delivered by artifact and
// gets no copy in the project scratchpad.
func TestAttachmentAudience_LocalDefaults_ParticipantGroupSend(t *testing.T) {
	f := newRefFixture(t)
	stagingDir, _ := localStorageHub(t, f)
	require.True(t, f.srv.experimentEnabled(experiments.AttachmentArtifacts))
	group := seedGroupConversation(t, f.st, f.projA.ID, "crew")
	id := f.uploadAs(t, f.ua, f.projA.ID, "group.txt", "group bytes")
	alice := NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb))
	// The agent message route delivers through the agent's runtime broker.
	ctx := context.Background()
	brokerID := uuid.NewString()
	require.NoError(t, f.st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "group-broker", Slug: "group-broker", Status: store.BrokerStatusOnline,
	}))
	aa, err := f.st.GetAgent(ctx, f.aa.ID)
	require.NoError(t, err)
	aa.RuntimeBrokerID = brokerID
	require.NoError(t, f.st.UpdateAgent(ctx, aa))

	body, err := json.Marshal(MessageRequest{StructuredMessage: &messages.StructuredMessage{
		Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender: "user:" + f.ua.ID, SenderID: f.ua.ID,
		Recipient: "agent:" + f.aa.Slug, RecipientID: f.aa.ID,
		Msg: "into a group with a file", Type: messages.TypeInstruction, ConversationID: group,
	}, AttachmentIDs: []string{id}})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.aa.ID+"/message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(contextWithIdentity(r.Context(), alice))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, r, f.aa.ID)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NoDirExists(t, filepath.Join(stagingDir, id), "no scratchpad copy for a participant-list group conversation")
	entries, err := os.ReadDir(stagingDir)
	if err == nil {
		assert.Empty(t, entries, "nothing was copied into _webchat")
	}
}
