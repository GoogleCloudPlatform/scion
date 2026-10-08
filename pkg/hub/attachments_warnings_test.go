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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3667: an attachment staged on a different broker than the hub
// is not on the hub host. The send still goes ahead, but the drop is reported
// back as a warning rather than only logged.

// missingStagedPath is a well-formed scratchpad path whose file was never
// written on this host — what the hub sees when the sending CLI staged the
// file on another machine.
const missingStagedPath = "/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/elsewhere.png"

func TestIngestAgentAttachments_MissingStagedFileWarns(t *testing.T) {
	srv, _, project, sharedDir := agentAttachmentServer(t)

	refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1",
		[]string{missingStagedPath})

	assert.Empty(t, refs, "a missing file cannot be recorded")
	require.Len(t, warnings, 1, "the missing file must be reported, not silently dropped")
	assert.Equal(t, missingStagedPath, warnings[0].Path, "the warning names the path the sender supplied")
	assert.Equal(t, attachmentWarnNotFound, warnings[0].Reason)
	assert.NotContains(t, warnings[0].Reason, sharedDir, "the reason must not include the hub's host path")
}

func TestIngestAgentAttachments_ReadableFileNoWarnings(t *testing.T) {
	srv, _, project, sharedDir := agentAttachmentServer(t)
	staged := stageAgentFile(t, sharedDir, "notes.md", "# hello\n")

	refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", []string{staged})

	require.Len(t, refs, 1)
	assert.Equal(t, "notes.md", refs[0].Name)
	assert.Empty(t, warnings, "a recorded attachment produces no warning")
}

func TestIngestAgentAttachments_MixedBatchWarnsPerPath(t *testing.T) {
	srv, _, project, sharedDir := agentAttachmentServer(t)
	staged := stageAgentFile(t, sharedDir, "notes.md", "# hello\n")

	refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1",
		[]string{missingStagedPath, staged, "/etc/passwd"})

	require.Len(t, refs, 1, "the readable file is still recorded")
	require.Len(t, warnings, 2)
	assert.Equal(t, missingStagedPath, warnings[0].Path)
	assert.Equal(t, attachmentWarnNotFound, warnings[0].Reason)
	assert.Equal(t, "/etc/passwd", warnings[1].Path)
	assert.Equal(t, string(errAttachmentOutsideSharedDir), warnings[1].Reason)
}

func TestIngestAgentAttachments_NoSharedDirWarnsAll(t *testing.T) {
	srv, s, _, _ := agentAttachmentServer(t)

	// A project that does not declare the scratchpad shared dir at all.
	bare := &store.Project{ID: api.NewUUID(), Name: "bare-project", Slug: "bare-project"}
	require.NoError(t, s.CreateProject(context.Background(), bare))

	paths := []string{missingStagedPath, "/scion-volumes/" + attachmentSharedDirName + "/b.txt"}
	refs, warnings := srv.ingestAgentAttachments(context.Background(), bare.ID, "agent-1", paths)

	assert.Empty(t, refs)
	require.Len(t, warnings, 2)
	for i, w := range warnings {
		assert.Equal(t, paths[i], w.Path)
		assert.Equal(t, attachmentWarnNoSharedDir, w.Reason)
	}
}

func TestIngestAgentAttachments_TooManyWarnsForExtras(t *testing.T) {
	srv, _, project, sharedDir := agentAttachmentServer(t)
	staged := stageAgentFile(t, sharedDir, "notes.md", "# hello\n")

	paths := make([]string, MaxAttachmentsPerMessage+1)
	for i := range paths {
		paths[i] = staged
	}
	refs, warnings := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1", paths)

	assert.Len(t, refs, MaxAttachmentsPerMessage)
	require.Len(t, warnings, 1, "only the entry past the limit is reported")
	assert.Equal(t, attachmentWarnTooMany, warnings[0].Reason)
}

// A hub without chat storage records nothing and so has nothing to warn
// about: the paths still travel on the message itself.
func TestIngestAgentAttachments_NoStoresNoWarnings(t *testing.T) {
	srv, _ := testServer(t)
	refs, warnings := srv.ingestAgentAttachments(context.Background(), "p", "agent-1", []string{missingStagedPath})
	assert.Nil(t, refs)
	assert.Nil(t, warnings)
}

// attachmentDMSetup adds two running agents and a dispatcher to an
// agentAttachmentServer, so ExecuteAgentDM runs against a project whose
// scratchpad shared dir exists on this host.
func attachmentDMSetup(t *testing.T) (*Server, store.Store, *store.Project, string, *store.Agent, *store.Agent) {
	t.Helper()
	srv, s, project, sharedDir := agentAttachmentServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      api.NewUUID(),
		Email:   "attach-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)
	// The owner is a project member, so the fixture agents are in good
	// standing (ptone/scion#3433).
	ensureStandingRoot(t, s, project.ID, owner.ID)

	brokerID := api.NewUUID()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "attach-broker", Slug: "attach-broker", Status: store.BrokerStatusOnline,
	}))

	newAgent := func(slug string) *store.Agent {
		a := &store.Agent{
			ID:              api.NewUUID(),
			Name:            slug,
			Slug:            slug,
			ProjectID:       project.ID,
			Phase:           "running",
			RuntimeBrokerID: brokerID,
			MessageMode:     store.MessageModeProject,
			Ancestry:        []string{owner.ID},
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	sender := newAgent("attach-sender")
	target := newAgent("attach-target")
	srv.SetDispatcher(&recordingDispatcher{})
	return srv, s, project, sharedDir, sender, target
}

func TestExecuteAgentDM_MissingAttachmentReturnsWarning(t *testing.T) {
	srv, _, _, sharedDir, sender, target := attachmentDMSetup(t)
	staged := stageAgentFile(t, sharedDir, "notes.md", "# hello\n")

	input := deliveryDMInput(sender, target, "with attachments")
	input.Attachments = []string{staged, missingStagedPath}
	result, dmErr := srv.ExecuteAgentDM(context.Background(), input)

	require.Nil(t, dmErr, "an unreadable attachment must not fail the send")
	assert.Equal(t, AgentDMAccepted, result.Outcome, "the message is still delivered")
	require.Len(t, result.AttachmentWarnings, 1)
	assert.Equal(t, missingStagedPath, result.AttachmentWarnings[0].Path)
	assert.Equal(t, attachmentWarnNotFound, result.AttachmentWarnings[0].Reason)

	// The readable attachment was still stored and carried on the
	// delivered message.
	d, ok := srv.dispatcher.(*recordingDispatcher)
	require.True(t, ok)
	calls := d.getCalls()
	require.Len(t, calls, 1)
	require.NotNil(t, calls[0].StructuredMessage)
	refs := parseAttachmentRefs(calls[0].StructuredMessage.Metadata)
	require.Len(t, refs, 1, "only the readable attachment is carried")
	assert.Equal(t, "notes.md", refs[0].Name)
	meta, err := srv.webChatStore.GetAttachment(context.Background(), refs[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "notes.md", meta.Filename)
}

// The agent-sender branch of the message handler (POST
// /projects/P/agents/ID/message sent by an agent) carries the warnings
// through to its JSON response.
func TestHandleAgentMessage_AgentSenderMissingAttachmentReturnsWarning(t *testing.T) {
	srv, _, _, sharedDir, sender, target := attachmentDMSetup(t)
	staged := stageAgentFile(t, sharedDir, "notes.md", "# hello\n")

	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "agent:" + sender.Slug,
		SenderID:    sender.ID,
		Recipient:   "agent:" + target.Slug,
		RecipientID: target.ID,
		Msg:         "see attached",
		Attachments: []string{staged, missingStagedPath},
	}})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: sender.ProjectID,
		Ancestry:  sender.Ancestry,
	}}))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "an unreadable attachment must not fail the send: %s", rr.Body.String())

	var resp struct {
		MessageID          string              `json:"message_id"`
		Status             string              `json:"status"`
		AttachmentWarnings []AttachmentWarning `json:"attachment_warnings"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "dispatched", resp.Status)
	require.Len(t, resp.AttachmentWarnings, 1, "body: %s", rr.Body.String())
	assert.Equal(t, missingStagedPath, resp.AttachmentWarnings[0].Path)
	assert.Equal(t, attachmentWarnNotFound, resp.AttachmentWarnings[0].Reason)
	assert.NotContains(t, rr.Body.String(), sharedDir, "the response must not include the hub's host path")
}

func TestExecuteAgentDM_ReadableAttachmentNoWarning(t *testing.T) {
	srv, _, _, sharedDir, sender, target := attachmentDMSetup(t)
	staged := stageAgentFile(t, sharedDir, "notes.md", "# hello\n")

	input := deliveryDMInput(sender, target, "with attachment")
	input.Attachments = []string{staged}
	result, dmErr := srv.ExecuteAgentDM(context.Background(), input)

	require.Nil(t, dmErr)
	assert.Equal(t, AgentDMAccepted, result.Outcome)
	assert.Empty(t, result.AttachmentWarnings)
}

func TestWriteAgentDMResult_IncludesAttachmentWarnings(t *testing.T) {
	w := httptest.NewRecorder()
	WriteAgentDMResult(w, &AgentDMResult{
		Outcome:            AgentDMAccepted,
		MessageID:          "m1",
		AttachmentWarnings: []AttachmentWarning{{Path: missingStagedPath, Reason: attachmentWarnNotFound}},
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code, "a warning does not change the status code")
	var resp struct {
		Status             string              `json:"status"`
		AttachmentWarnings []AttachmentWarning `json:"attachment_warnings"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "dispatched", resp.Status)
	require.Len(t, resp.AttachmentWarnings, 1)
	assert.Equal(t, missingStagedPath, resp.AttachmentWarnings[0].Path)

	// Omitted entirely when there is nothing to report.
	w = httptest.NewRecorder()
	WriteAgentDMResult(w, &AgentDMResult{Outcome: AgentDMAccepted, MessageID: "m2"}, nil)
	assert.NotContains(t, w.Body.String(), "attachment_warnings")
}

func TestMessageDeliveryResponse_AttachmentWarningsOmitEmpty(t *testing.T) {
	b, err := json.Marshal(MessageDeliveryResponse{MessageID: "m", Status: "dispatched"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "attachment_warnings")

	b, err = json.Marshal(MessageDeliveryResponse{
		MessageID:          "m",
		Status:             "dispatched",
		AttachmentWarnings: []AttachmentWarning{{Path: "p", Reason: "r"}},
	})
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(b), `"attachment_warnings":[{"path":"p","reason":"r"}]`), string(b))
}

// The agent→user path: the outbound handler reports the warning alongside
// the usual "sent" status.
func TestOutboundMessage_MissingAttachmentReturnsWarning(t *testing.T) {
	srv, s, project, sharedDir := agentAttachmentServer(t)
	ctx := context.Background()

	user := &store.User{ID: api.NewUUID(), Email: "attach-human@example.com", DisplayName: "Attach Human"}
	require.NoError(t, s.CreateUser(ctx, user))
	agent := &store.Agent{ID: api.NewUUID(), Name: "attach-agent", Slug: "attach-agent", ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, agent))

	staged := stageAgentFile(t, sharedDir, "notes.md", "# hello\n")
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:   "user:" + user.Email,
		Msg:         "see attached",
		Attachments: []string{staged, missingStagedPath},
	})
	require.Equal(t, http.StatusOK, rr.Code, "an unreadable attachment must not fail the send: %s", rr.Body.String())

	var resp struct {
		MessageID          string              `json:"message_id"`
		Status             string              `json:"status"`
		AttachmentWarnings []AttachmentWarning `json:"attachment_warnings"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "sent", resp.Status)
	assert.NotEmpty(t, resp.MessageID)
	require.Len(t, resp.AttachmentWarnings, 1)
	assert.Equal(t, missingStagedPath, resp.AttachmentWarnings[0].Path)
	assert.Equal(t, attachmentWarnNotFound, resp.AttachmentWarnings[0].Reason)
	assert.NotContains(t, rr.Body.String(), sharedDir, "the response must not include the hub's host path")

	// The readable file was still linked to the message.
	attachments, err := srv.webChatStore.GetAttachmentsByMessage(ctx, resp.MessageID)
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	assert.Equal(t, "notes.md", attachments[0].Filename)
}

func TestOutboundMessage_ReadableAttachmentNoWarning(t *testing.T) {
	srv, s, project, sharedDir := agentAttachmentServer(t)
	ctx := context.Background()

	user := &store.User{ID: api.NewUUID(), Email: "attach-human2@example.com", DisplayName: "Attach Human"}
	require.NoError(t, s.CreateUser(ctx, user))
	agent := &store.Agent{ID: api.NewUUID(), Name: "attach-agent2", Slug: "attach-agent2", ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, agent))

	staged := stageAgentFile(t, sharedDir, "notes.md", "# hello\n")
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:   "user:" + user.Email,
		Msg:         "see attached",
		Attachments: []string{staged},
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "attachment_warnings")
}
