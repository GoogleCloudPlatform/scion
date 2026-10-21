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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The project recorded on an attachment at bind time (ptone/scion#4596,
// design section 1).

// seedDirectConversation stores a direct conversation for DM key key.
func seedDirectConversation(t *testing.T, s store.Store, key string) *store.Conversation {
	t.Helper()
	conv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind: "direct", Surface: "native", ExternalRef: key, DriftState: "active",
	})
	require.NoError(t, err)
	return conv
}

// projectForBind derives the bind project of a message stored on
// conversation convID (or, with convID "", on thread), sent by senderID.
func projectForBind(t *testing.T, f *refFixture, convID, thread, senderID string) string {
	t.Helper()
	got, err := f.srv.messageProjectForBind(context.Background(), &store.Message{
		ID: uuid.NewString(), ConversationID: convID, ThreadID: thread, SenderID: senderID,
	})
	require.NoError(t, err)
	return got
}

func TestMessageProjectForBind_Group(t *testing.T) {
	f := newRefFixture(t)
	conv := seedGroupConversation(t, f.st, f.projA.ID, "g1")
	require.Equal(t, f.projA.ID, projectForBind(t, f, conv, "", f.ua.ID))
}

func TestMessageProjectForBind_ProjectlessGroup(t *testing.T) {
	f := newRefFixture(t)
	conv, err := f.st.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind: "group", Surface: "native", ExternalRef: "group:none:" + uuid.NewString(), DriftState: "active",
	})
	require.NoError(t, err)
	require.Equal(t, "", projectForBind(t, f, conv.ID, "", f.ua.ID))
}

func TestMessageProjectForBind_UserDM(t *testing.T) {
	f := newRefFixture(t)
	conv := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "user", f.ub.ID))
	require.Equal(t, "", projectForBind(t, f, conv.ID, "", f.ua.ID))
}

// The same user-agent DM gives the same value whichever party sends.
func TestMessageProjectForBind_UserAgentDM(t *testing.T) {
	f := newRefFixture(t)
	conv := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ub.ID, "agent", f.aa.ID))
	require.Equal(t, f.projA.ID, projectForBind(t, f, conv.ID, "", f.ub.ID))
	require.Equal(t, f.projA.ID, projectForBind(t, f, conv.ID, "", f.aa.ID))
}

func TestMessageProjectForBind_AgentAgentDM(t *testing.T) {
	f := newRefFixture(t)
	other := &store.Agent{ID: uuid.NewString(), ProjectID: f.projB.ID, Name: "ab", Slug: "ab",
		Phase: "running", OwnerID: f.ub.ID, CreatedBy: f.ub.ID}
	require.NoError(t, f.st.CreateAgent(context.Background(), other))
	conv := seedDirectConversation(t, f.st, dmKeyFor(t, "agent", f.aa.ID, "agent", other.ID))
	require.Equal(t, "", projectForBind(t, f, conv.ID, "", f.aa.ID))
}

func TestMessageProjectForBind_DMAgentGone(t *testing.T) {
	f := newRefFixture(t)
	conv := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", uuid.NewString()))
	require.Equal(t, "", projectForBind(t, f, conv.ID, "", f.ua.ID))
}

func TestMessageProjectForBind_ThreadKeys(t *testing.T) {
	f := newRefFixture(t)
	require.Equal(t, f.projB.ID, projectForBind(t, f, "", f.topicB, f.ub.ID), "topic key")
	require.Equal(t, "", projectForBind(t, f, "", dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID), f.ua.ID), "DM key")
}

// TestMessageProjectForBind_IgnoresRequestProject (K8): the upload names
// another project the sender reads in its project_id form value, and the
// send names it in message metadata; the stored project is the
// conversation's.
func TestMessageProjectForBind_IgnoresRequestProject(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	projC := &store.Project{ID: uuid.NewString(), Name: "Project C", Slug: "project-c",
		OwnerID: f.ua.ID, CreatedBy: f.ua.ID, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.st.CreateProject(ctx, projC))
	f.srv.seedProjectCreatorMembership(ctx, projC)
	require.True(t, f.srv.canReadProject(ctx, NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb)), projC.ID), "alice reads project C")

	// The topic has a default agent, so the send takes the agent-routed
	// path, which carries client metadata.
	require.NoError(t, f.wcs.UpdateTopic(ctx, f.topicA, TopicUpdate{DefaultAgent: &f.aa.Slug}))
	id := f.uploadAs(t, f.ua, projC.ID, "notes.txt", "misleading project")
	rec := doRequestAsUser(t, f.srv, f.ua, http.MethodPost, "/api/v1/chat/conversations/"+f.topicA+"/messages",
		map[string]interface{}{"content": "see file", "attachments": []string{id},
			"metadata": map[string]string{"project_id": projC.ID, "projectId": projC.ID}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp chatMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	requireMessageScoped(t, st, id, resp.ID)
	a, err := st.GetAttachment(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, a.MessageProjectID)
	require.Equal(t, f.projA.ID, *a.MessageProjectID, "the stored project is the conversation's, not a request's")
}

// TestMessageProjectForBind_NilConversationNotFound: a (nil, nil) lookup
// is answered as not found, so nothing is bound.
func TestMessageProjectForBind_NilConversationNotFound(t *testing.T) {
	f := newRefFixture(t)
	conv := seedGroupConversation(t, f.st, f.projA.ID, "g1")
	f.faults.nilGetConversation = true
	f.fault.Arm()
	_, err := f.srv.messageProjectForBind(context.Background(), &store.Message{
		ID: uuid.NewString(), ConversationID: conv, SenderID: f.ua.ID,
	})
	require.ErrorIs(t, err, store.ErrNotFound)
}
