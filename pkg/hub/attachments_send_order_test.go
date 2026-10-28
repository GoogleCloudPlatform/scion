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
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The agent message handler checks the sender (the DM key ownership check
// and decideKeyedPostSender) before it validates attachment_ids
// (ptone/scion#4596): a refused sender gets the sender check's answer and
// learns nothing about the ids.

// TestAgentSend_DMKeyMismatchRefusedBeforeAttachmentIDs: a send whose DM
// thread_id names another agent, carrying another user's upload id, gets
// the ownership check's 400, never an attachment answer, and binds nothing.
func TestAgentSend_DMKeyMismatchRefusedBeforeAttachmentIDs(t *testing.T) {
	f := newSendFixture(t)
	foreign := f.uploadAs(t, f.ub, f.projB.ID, "bob.txt", "bob's bytes")
	body := f.messageBody(f.target, "with another agent's dm key", []string{foreign})
	body.StructuredMessage.ThreadID = dmKeyFor(t, "agent", f.sender.ID, "user", f.ua.ID)
	body.StructuredMessage.Channel = "web"

	rec := doRequestAsUser(t, f.srv, f.ua, http.MethodPost, "/api/v1/agents/"+f.target.ID+"/message", body)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "DM thread_id does not match the sender and recipient")
	require.NotContains(t, rec.Body.String(), "attachment")
	f.requireUnbound(t, foreign)
}

// TestAgentSend_ThreadPostByNonReaderRefusedBeforeAttachmentIDs: a thread
// post by a user who may message the agent but cannot read its project,
// carrying another user's upload id (which the attachment check would refuse
// with 400), gets exactly the answer for an unknown agent
// (decideKeyedPostSender's 404), never an attachment answer, and binds
// nothing.
func TestAgentSend_ThreadPostByNonReaderRefusedBeforeAttachmentIDs(t *testing.T) {
	f := newRefFixture(t)
	art := enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	target := &store.Agent{ID: tid("order-thread-agent"), ProjectID: f.projA.ID, Name: "order-thread", Slug: "order-thread",
		MessageMode: store.MessageModeProject, Phase: "running", OwnerID: f.ua.ID, CreatedBy: f.ua.ID}
	require.NoError(t, f.st.CreateAgent(ctx, target))
	msgAuthzGrantAgentMessage(t, f.st, f.uc.ID, f.projA.ID)
	carol := userIdentityOf(f.uc)
	require.False(t, f.srv.canReadProject(ctx, carol, f.projA.ID), "precondition: carol cannot read project A")
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, carol, target, false)
	require.True(t, allowed, "precondition: carol may message the agent: %s", reason)
	foreign := f.uploadAs(t, f.ua, f.projA.ID, "alice.txt", "alice's bytes")

	post := func(id string) refAnswer {
		req := threadRequest(target, "order-thread")
		req.AttachmentIDs = []string{foreign}
		rec := doRequestAsUser(t, f.srv, f.uc, http.MethodPost, "/api/v1/agents/"+id+"/message", req)
		return refAnswer{status: rec.Code, body: rec.Body.String()}
	}
	unknown := post(tid("order-thread-unknown-agent"))
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
	got := post(target.ID)
	requireSameAnswer(t, unknown, got)
	require.NotContains(t, got.body, "attachment")
	a, err := art.GetAttachment(ctx, foreign)
	require.NoError(t, err)
	require.Equal(t, artifacts.ScopeKindUpload, a.ScopeKind, "the upload is not bound")
}

// TestAgentSend_AgentSenderDMKeyRefusedBeforeAttachmentIDs: an agent sender
// naming a dm: thread_id (a DM key's second slot is always a user, so the
// ownership check refuses any agent sender), with another principal's upload
// id, gets the ownership check's 400, never an attachment answer, and binds
// nothing.
func TestAgentSend_AgentSenderDMKeyRefusedBeforeAttachmentIDs(t *testing.T) {
	f := newSendFixture(t)
	foreign := f.uploadAs(t, f.ub, f.projB.ID, "bob.txt", "bob's bytes")
	body := f.messageBody(f.target, "an agent naming a dm key", []string{foreign})
	body.StructuredMessage.ThreadID = dmKeyFor(t, "agent", f.target.ID, "user", f.ua.ID)
	body.StructuredMessage.Channel = "web"

	rec := f.agentRequest(t, f.sender, jsonRequest(t, "/api/v1/agents/"+f.target.ID+"/message", body))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "DM thread_id does not match the sender and recipient")
	require.NotContains(t, rec.Body.String(), "attachment")
	f.requireUnbound(t, foreign)
}
