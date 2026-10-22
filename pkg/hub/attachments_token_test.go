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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// User access tokens and attachments stored as artifacts (ptone/scion#4596,
// section 9): the token rule of MayReadMessage, clause 0 for an unsent
// upload, token uploads, and the token checks of the two send paths.

// tokenFixture is a refFixture with the experiment on and its artifact
// database at hand.
type tokenFixture struct {
	*refFixture
	art     artifacts.Store
	db      *sql.DB
	unknown refAnswer
}

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	f := newRefFixture(t)
	setAttachmentExperiments(t, f.srv, true, false)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "artifacts.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := artifacts.NewStore(db, "sqlite")
	require.NoError(t, st.Init(context.Background()))
	f.srv.SetArtifactStore(st)
	blobs, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "hub", LocalPath: t.TempDir()})
	require.NoError(t, err)
	f.srv.SetStorage(blobs)
	f.srv.SetDispatcher(&recordingDispatcher{})
	tf := &tokenFixture{refFixture: f, art: st, db: db}
	tf.unknown = f.download(t, f.ua, uuid.NewString())
	require.Equal(t, http.StatusNotFound, tf.unknown.status)
	return tf
}

// storeMsg stores a web message on conversation conv (nil for none) or
// thread, and returns its id.
func (f *tokenFixture) storeMsg(t *testing.T, projectID, thread string, conv *store.Conversation, sender *store.User) string {
	t.Helper()
	msg := &store.Message{ID: uuid.NewString(), ProjectID: projectID, Sender: "user:" + sender.Email, SenderID: sender.ID,
		Recipient: "thread:" + thread, Msg: "with a file", Type: "instruction", Channel: "web", ThreadID: thread}
	if conv != nil {
		msg.ConversationID = conv.ID
		msg.ThreadID = conv.ExternalRef
	}
	require.NoError(t, f.st.CreateMessage(context.Background(), msg))
	return msg.ID
}

// boundFile uploads a file as user (a session) and binds it to messageID
// with the recorded project.
func (f *tokenFixture) boundFile(t *testing.T, user *store.User, messageID, project string) string {
	t.Helper()
	id := f.uploadAs(t, user, "", "file.txt", "file bytes")
	require.NoError(t, f.srv.attachmentService().BindAttachment(context.Background(), id, artifacts.PrincipalKindUser, user.ID, messageID, project))
	return id
}

// requireRead asserts that identity reads attachment id (want) or gets the
// unknown-id answer, byte-equal.
func (f *tokenFixture) requireRead(t *testing.T, identity Identity, id string, want bool, msgAndArgs ...interface{}) {
	t.Helper()
	got := f.downloadAs(t, identity, id)
	if want {
		require.Equal(t, http.StatusOK, got.status, append([]interface{}{got.body}, msgAndArgs...)...)
		return
	}
	require.Equal(t, f.unknown, got, msgAndArgs...)
}

// TestAttachmentTokenRead_Allowed: a token with inbox:read reads the
// attachments of a project thread, a direct message between users and a
// direct message with an agent, as its holder does.
func TestAttachmentTokenRead_Allowed(t *testing.T) {
	f := newTokenFixture(t)
	t.Run("ProjectThread", func(t *testing.T) {
		id := f.uploadAs(t, f.ua, f.projA.ID, "thread.txt", "in project A")
		f.sendWith(t, f.ua, f.topicA, id)
		f.requireRead(t, selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read"), id, true)
	})
	t.Run("UserDM", func(t *testing.T) {
		dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
		id := f.uploadAs(t, f.ua, "", "dm.txt", "between users")
		f.sendWith(t, f.ua, dm, id)
		f.requireRead(t, selfToken(t, f.ua.ID, hubBoundary(), "inbox:read"), id, true)
	})
	t.Run("AgentDM", func(t *testing.T) {
		conv := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))
		id := f.boundFile(t, f.ua, f.storeMsg(t, f.projA.ID, "", conv, f.ua), f.projA.ID)
		f.requireRead(t, selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read", "agent:read"), id, true)
	})
}

// TestAttachmentTokenRead_NoInboxRead (KB1): a token without inbox:read
// reads nothing, where its holder's session does.
func TestAttachmentTokenRead_NoInboxRead(t *testing.T) {
	f := newTokenFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "dm.txt", "between users")
	f.sendWith(t, f.ua, dm, id)
	require.Equal(t, http.StatusOK, f.download(t, f.ua, id).status)
	token := selfToken(t, f.ua.ID, hubBoundary(), "project:read", "agent:read")
	require.False(t, newArtifactHost(f.srv).PermitsMessageRead(contextWithIdentity(context.Background(), token)))
	f.requireRead(t, token, id, false, "a token without inbox:read")
}

// TestAttachmentTokenRead_ProjectBoundaryOtherProject (KB2): the recorded
// project must pass the token's inbox:read self-scope check: a token bounded
// to project B does not read an attachment recorded for project A, even in a
// conversation its holder reads.
func TestAttachmentTokenRead_ProjectBoundaryOtherProject(t *testing.T) {
	f := newTokenFixture(t)
	conv := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ub.ID, "user", f.uc.ID))
	id := f.boundFile(t, f.ub, f.storeMsg(t, f.projB.ID, "", conv, f.ub), f.projA.ID)
	require.Equal(t, http.StatusOK, f.download(t, f.ub, id).status, "the holder's session reads it")
	f.requireRead(t, selfToken(t, f.ub.ID, projectBoundary(f.projB.ID), "inbox:read"), id, false, "recorded for another project")
	f.requireRead(t, selfToken(t, f.ub.ID, hubBoundary(), "inbox:read"), id, true, "a hub boundary reaches it")
}

// TestAttachmentTokenRead_ProjectBoundaryProjectless: an attachment recorded
// with no project needs a hub boundary.
func TestAttachmentTokenRead_ProjectBoundaryProjectless(t *testing.T) {
	f := newTokenFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "dm.txt", "between users")
	f.sendWith(t, f.ua, dm, id)
	f.requireRead(t, selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "inbox:read"), id, false, "a project boundary")
	f.requireRead(t, selfToken(t, f.ua.ID, hubBoundary(), "inbox:read"), id, true, "a hub boundary")
}

// TestAttachmentTokenRead_NullProjectRefused (KB3): an attachment whose
// recorded project is unknown (NULL, bound before the column existed) is
// refused to every token, a hub boundary too; NULL is not "no project".
func TestAttachmentTokenRead_NullProjectRefused(t *testing.T) {
	f := newTokenFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "dm.txt", "between users")
	f.sendWith(t, f.ua, dm, id)
	_, err := f.db.Exec(`UPDATE artifact SET message_project_id = NULL WHERE id = ?`, id)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, f.download(t, f.ua, id).status, "sessions do not read the recorded project")
	f.requireRead(t, selfToken(t, f.ua.ID, hubBoundary(), "inbox:read"), id, false, "an unknown project")
}

// TestAttachmentTokenRead_DMAgentClause_IndependentOfCaller (KB4, KB5): for
// a direct conversation, a token needs agent:read on every agent party of
// the key, whichever party (if any) its holder is.
func TestAttachmentTokenRead_DMAgentClause_IndependentOfCaller(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()

	// The holder is the user party of a DM with an agent.
	conv := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))
	id := f.boundFile(t, f.ua, f.storeMsg(t, f.projA.ID, "", conv, f.ua), f.projA.ID)
	f.requireRead(t, selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read"), id, false, "without agent:read")
	f.requireRead(t, selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read", "agent:read"), id, true, "with agent:read")

	// The holder is an addressee of a DM between two agents, one of which
	// it cannot read (an agent of project B). Whether that agent sits in
	// the first or the second slot of the key, the read is refused.
	token := selfToken(t, f.ua.ID, hubBoundary(), "project:read", "inbox:read", "agent:read")
	for _, slot := range []struct {
		name, id string
	}{
		{"first slot", "00000000-0000-4000-8000-000000000001"},
		{"second slot", "ffffffff-ffff-4fff-bfff-ffffffffffff"},
	} {
		t.Run(slot.name, func(t *testing.T) {
			other := &store.Agent{ID: slot.id, ProjectID: f.projB.ID, Name: "b-" + slot.id[:4], Slug: "b-" + slot.id[:4],
				Phase: "running", OwnerID: f.ub.ID, CreatedBy: f.ub.ID}
			require.NoError(t, f.st.CreateAgent(ctx, other))
			key := dmKeyFor(t, "agent", f.aa.ID, "agent", other.ID)
			kindA, idA, _, _, err := messages.ParseDMKey(key)
			require.NoError(t, err)
			require.Equal(t, slot.name == "first slot", kindA == "agent" && idA == other.ID, "slot of the unreadable agent")
			conv := seedDirectConversation(t, f.st, key)
			msgID := f.storeMsg(t, f.projA.ID, "", conv, f.ua)
			require.NoError(t, f.st.AddAddressee(ctx, &store.MessageAddressee{ID: uuid.NewString(), MessageID: msgID,
				PrincipalKind: "user", PrincipalID: f.ua.ID, Via: "explicit", DeliveryState: "delivered"}))
			file := f.boundFile(t, f.ua, msgID, f.projA.ID)
			f.requireRead(t, token, file, false, "an agent party the token cannot read")
		})
	}
}

// TestAttachmentTokenRead_DMAgentClause_MissingAgent: an agent party with no
// agent row refuses the read.
func TestAttachmentTokenRead_DMAgentClause_MissingAgent(t *testing.T) {
	f := newTokenFixture(t)
	conv := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", uuid.NewString()))
	id := f.boundFile(t, f.ua, f.storeMsg(t, f.projA.ID, "", conv, f.ua), f.projA.ID)
	f.requireRead(t, selfToken(t, f.ua.ID, hubBoundary(), "project:read", "inbox:read", "agent:read"), id, false, "a missing agent")
}

// TestAttachmentTokenRead_UnboundUploadNeedsHubBoundary (KB8): the holder's
// own unsent upload is read by a hub-boundary token with inbox:read and by
// the holder's session, and refused to a project-bounded token.
func TestAttachmentTokenRead_UnboundUploadNeedsHubBoundary(t *testing.T) {
	f := newTokenFixture(t)
	id := f.uploadAs(t, f.ua, f.projA.ID, "unsent.txt", "not sent yet")
	require.Equal(t, http.StatusOK, f.download(t, f.ua, id).status, "the session")
	f.requireRead(t, selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read"), id, false, "a project-bounded token")
	f.requireRead(t, selfToken(t, f.ua.ID, hubBoundary(), "inbox:read"), id, true, "a hub-boundary token")
	f.requireRead(t, selfToken(t, f.uc.ID, hubBoundary(), "inbox:read"), id, false, "another user's token")
}

// tokenUpload uploads one file through the upload route as token.
func (f *tokenFixture) tokenUpload(t *testing.T, token *ScopedUserIdentity, projectID, name string) string {
	t.Helper()
	req := attachmentUploadRequest(t, projectID, name, "from a token")
	rec := httptest.NewRecorder()
	f.srv.chatAttachmentRoute(f.srv.handleChatAttachments)(rec, req.WithContext(contextWithIdentity(req.Context(), token)))
	return uploadedAttachmentID(t, rec, name)
}

// TestTokenUpload_StoresArtifact: with the experiment on, a token's upload
// is stored in the artifact service as an unsent upload of its holder.
func TestTokenUpload_StoresArtifact(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()
	token := selfToken(t, f.ua.ID, hubBoundary(), "inbox:read", "inbox:write")
	id := f.tokenUpload(t, token, "", "token.txt")
	info, err := f.art.GetAttachment(ctx, id)
	require.NoError(t, err, "stored in the artifact service")
	require.Equal(t, artifacts.PrincipalKindUser, info.OwnerKind)
	require.Equal(t, f.ua.ID, info.OwnerRef)
	meta, err := f.wcs.GetAttachment(ctx, id)
	require.True(t, err != nil || meta == nil, "nothing is stored in the AttachmentStore")
	f.requireRead(t, token, id, true, "the hub-boundary token reads its holder's upload")
}

// TestTokenSend_BindsOwnUpload_RefusedOutsideBoundary (KB7): a token sends
// its holder's upload where it may post, and the upload is bound; where it
// may not post (outside its boundary), its ids get the answer of an
// unknown id from the send validation itself.
func TestTokenSend_BindsOwnUpload_RefusedOutsideBoundary(t *testing.T) {
	f := newTokenFixture(t)
	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read", "inbox:write")
	id := f.tokenUpload(t, token, f.projA.ID, "token.txt")
	got := f.tokenRequest(t, token, http.MethodPost, "/api/v1/chat/conversations/"+f.topicA+"/messages",
		map[string]interface{}{"content": "see file", "attachments": []string{id}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	rows, err := f.art.ListMessageAttachments(context.Background(), []string{messageIDFromBody(t, got.body)})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the upload is bound to the message")

	// Outside the boundary: a topic of project B, asked of the send
	// validation directly (the route refuses the conversation first).
	other := f.tokenUpload(t, token, f.projA.ID, "other.txt")
	ctx := contextWithIdentity(context.Background(), token)
	target := &chatSendTarget{Key: f.topicB, ProjectID: f.projB.ID, wcs: f.wcs}
	_, _, serr := f.srv.validateChatSendInput(ctx, token, target, chatSendInput{Content: "x", Attachments: []string{other}})
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "is not available in this conversation")
}

// messageIDFromBody returns the id of a chat send response body.
func messageIDFromBody(t *testing.T, body string) string {
	t.Helper()
	var resp chatMessageResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.NotEmpty(t, resp.ID, body)
	return resp.ID
}

// TestHumanCLISend_TokenBindsOwnUpload: a token sends its holder's upload to
// an agent of a project its boundary reaches, and the upload is bound.
func TestHumanCLISend_TokenBindsOwnUpload(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()
	brokerID := uuid.NewString()
	require.NoError(t, f.st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: brokerID, Name: "tok-broker", Slug: "tok-broker", Status: store.BrokerStatusOnline}))
	target := &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: "tk", Slug: "tk", Phase: "running",
		OwnerID: f.ua.ID, CreatedBy: f.ua.ID, RuntimeBrokerID: brokerID, MessageMode: store.MessageModeProject, Ancestry: []string{f.ua.ID}}
	require.NoError(t, f.st.CreateAgent(ctx, target))
	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read", "inbox:write", "agent:read", "agent:message")
	id := f.tokenUpload(t, token, f.projA.ID, "cli.txt")
	got := postAgentMessageAs(t, f.srv, func(ctx context.Context) context.Context { return contextWithIdentity(ctx, token) }, target,
		tokenCLIMessage(target, id))
	require.Equal(t, http.StatusOK, got.status, got.body)
	info, err := f.art.GetAttachment(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, artifacts.ScopeKindMessage, info.ScopeKind, "bound to the message")
}

// TestHumanCLISend_TokenWithoutInboxWriteRefused (KB6): at the human CLI
// bind site a token needs inbox:write for the target agent's project; a
// token that may message the agent but lacks it is refused, and nothing is
// bound.
func TestHumanCLISend_TokenWithoutInboxWriteRefused(t *testing.T) {
	f := newTokenFixture(t)
	uploader := selfToken(t, f.ua.ID, hubBoundary(), "inbox:read", "inbox:write")
	id := f.tokenUpload(t, uploader, "", "cli.txt")
	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "agent:read", "agent:message")
	got := postAgentMessageAs(t, f.srv, func(ctx context.Context) context.Context { return contextWithIdentity(ctx, token) }, f.aa,
		tokenCLIMessage(f.aa, id))
	require.Equal(t, http.StatusForbidden, got.status, got.body)
	info, err := f.art.GetAttachment(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, artifacts.ScopeKindUpload, info.ScopeKind, "still an unsent upload")
}

func tokenCLIMessage(target *store.Agent, ids ...string) MessageRequest {
	return MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version: messages.Version, Recipient: "agent:" + target.Slug, RecipientID: target.ID,
			Msg: "see file", Type: messages.TypeInstruction,
		},
		AttachmentIDs: ids,
	}
}

// TestHumanCLISend_TokenTargetOutsideBoundaryRefused: at the human CLI bind
// site, a project-A token's ids for an agent of project B are refused by the
// site's own check (the agent-message permission refuses such a token
// first on the route; this pins the second line).
func TestHumanCLISend_TokenTargetOutsideBoundaryRefused(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()
	other := &store.Agent{ID: uuid.NewString(), ProjectID: f.projB.ID, Name: "bb", Slug: "bb", Phase: "running",
		OwnerID: f.ub.ID, CreatedBy: f.ub.ID}
	require.NoError(t, f.st.CreateAgent(ctx, other))
	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read", "inbox:write", "agent:read", "agent:message")
	id := f.tokenUpload(t, token, f.projA.ID, "cli.txt")
	_, msg, ok := f.srv.validateMessageAttachments(withStandingMemo(contextWithIdentity(ctx, token)), other, []string{id}, nil)
	require.False(t, ok, "refused: %s", msg)
	_, _, ok = f.srv.validateMessageAttachments(withStandingMemo(contextWithIdentity(ctx, token)), f.aa, []string{id}, nil)
	require.True(t, ok, "an agent of the token's own project passes the site's check")
}

// TestAttachmentArtifacts_TokenHistoryFollowsTheRule: a project-B token's
// history of a project-B thread does not list an attachment recorded for
// project A, while the holder's session history does.
func TestAttachmentArtifacts_TokenHistoryFollowsTheRule(t *testing.T) {
	f := newTokenFixture(t)
	id := f.uploadAs(t, f.ub, f.projB.ID, "thread.txt", "in project B")
	resp := f.sendWith(t, f.ub, f.topicB, id)
	_, err := f.db.Exec(`UPDATE artifact SET message_project_id = ? WHERE id = ?`, f.projA.ID, id)
	require.NoError(t, err)
	require.Len(t, f.history(t, f.ub, f.topicB).MessageAttachments[resp.ID], 1, "the session's history lists it")
	token := selfToken(t, f.ub.ID, projectBoundary(f.projB.ID), "project:read", "inbox:read")
	got := f.tokenRequest(t, token, http.MethodGet, "/api/v1/chat/conversations/"+f.topicB+"/messages", nil)
	require.Equal(t, http.StatusOK, got.status, got.body)
	var hist chatHistoryResponse
	require.NoError(t, json.Unmarshal([]byte(got.body), &hist))
	require.NotEmpty(t, hist.Messages, "the token reads the thread")
	require.Empty(t, hist.MessageAttachments[resp.ID], "an attachment recorded for another project is not listed")
}
