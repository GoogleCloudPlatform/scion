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
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestChatReply_TargetMustBeInSameConversation(t *testing.T) {
	f := newRefFixture(t)

	otherProjectMsg := f.seedMessage(t, f.projB.ID, f.topicB, "", "in project B")
	otherDM := dmKeyFor(t, "user", f.ub.ID, "user", f.uc.ID)
	otherDMMsg := f.seedMessage(t, f.projB.ID, otherDM, "", "in another DM")
	sameTopicMsg := f.seedMessage(t, f.projA.ID, f.topicA, "", "in topic A")

	before := f.threadMessageCount(t, f.topicA)

	missing := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "re", "reply_to_id": uuid.NewString()})
	require.Equal(t, http.StatusBadRequest, missing.status, missing.body)
	assert.Contains(t, missing.body, "reply_to_id does not refer to a message in this conversation")

	for name, id := range map[string]string{"other project topic": otherProjectMsg, "other DM": otherDMMsg} {
		t.Run(name, func(t *testing.T) {
			got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "re", "reply_to_id": id})
			requireSameAnswer(t, missing, got)
		})
	}
	assert.Equal(t, before, f.threadMessageCount(t, f.topicA), "a refused reply must not store a message")

	ok := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "re", "reply_to_id": sameTopicMsg})
	require.Equal(t, http.StatusCreated, ok.status, ok.body)

	hist := f.history(t, f.ua, f.topicA)
	assert.Contains(t, hist.ReplyPreviews, sameTopicMsg, "a same-conversation reply keeps its preview")
}

func TestChatReply_TargetMatchedByConversationWhenSwitchOn(t *testing.T) {
	f := newRefFixture(t)
	enableReadSwitch(t, f.srv)

	first := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "first"})
	require.Equal(t, http.StatusCreated, first.status, first.body)

	convID, err := f.srv.conversationIDForKey(context.Background(), f.wcs, f.topicA)
	require.NoError(t, err)
	require.NotEmpty(t, convID, "the topic must have a conversation once a message is sent")

	// Same conversation, stored under a different thread key.
	target := f.seedMessage(t, f.projA.ID, "legacy-thread-key", convID, "same conversation")

	got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "re", "reply_to_id": target})
	require.Equal(t, http.StatusCreated, got.status, got.body)
}

func TestChatHistory_ReplyPreviewsOnlyFromSameConversation(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	foreign := f.seedMessage(t, f.projB.ID, f.topicB, "", "text of project B")
	local := f.seedMessage(t, f.projA.ID, f.topicA, "", "text of topic A")
	replyForeign := f.seedMessage(t, f.projA.ID, f.topicA, "", "reply 1")
	replyLocal := f.seedMessage(t, f.projA.ID, f.topicA, "", "reply 2")
	// Written directly, as a row stored before the send-time check.
	require.NoError(t, f.wcs.SetMessageReplyTo(ctx, replyForeign, foreign))
	require.NoError(t, f.wcs.SetMessageReplyTo(ctx, replyLocal, local))

	hist := f.history(t, f.ua, f.topicA)
	assert.NotContains(t, hist.ReplyPreviews, foreign, "a message of another conversation is never previewed")
	assert.Contains(t, hist.ReplyPreviews, local, "a same-conversation preview is still shown")
}

func TestChatSendRefusal_ReasonLoggedNotReturned(t *testing.T) {
	logs := captureSlog(t)
	f := newRefFixture(t)

	unknown := f.send(t, f.ub, uuid.NewString(), map[string]interface{}{"content": "hi"})
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)

	// bob is a hub member with no role in project A.
	logs.Reset()
	got := f.send(t, f.ub, f.topicA, map[string]interface{}{"content": "hi"})
	requireSameAnswer(t, unknown, got)
	logged := logs.String()
	assert.Contains(t, logged, "authorization denied", "the denial must be logged")
	assert.Contains(t, logged, "chat send refused as not found")
	assert.Contains(t, logged, f.ub.ID, "the log names the caller")
	for _, word := range []string{"denied", "permission", "resource_type", "reason"} {
		assert.NotContains(t, got.body, word, "the response carries no reason")
	}

	// carol is not a participant of the alice/bob DM.
	logs.Reset()
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.ub.ID)
	got = f.send(t, f.uc, dm, map[string]interface{}{"content": "hi"})
	requireSameAnswer(t, unknown, got)
	assert.Contains(t, logs.String(), "not a participant of this DM")

	// History answers the same way.
	getAnswer := func(key string) refAnswer {
		rec := doRequestAsUser(t, f.srv, f.ub, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
		return refAnswer{status: rec.Code, body: rec.Body.String()}
	}
	missingHist := getAnswer(uuid.NewString())
	require.Equal(t, http.StatusNotFound, missingHist.status, missingHist.body)
	requireSameAnswer(t, missingHist, getAnswer(f.topicA))
	requireSameAnswer(t, missingHist, getAnswer(dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)))
}

func TestChatDMHistory_StaysReadableAfterProjectAccessEnds(t *testing.T) {
	f := newRefFixture(t)

	// alice's DM with agent aa of project A.
	dm := dmKeyFor(t, "agent", f.aa.ID, "user", f.ua.ID)
	f.seedMessage(t, f.projA.ID, dm, "", "earlier DM message")
	require.NotEmpty(t, f.history(t, f.ua, dm).Messages)

	f.revokeProjectAccess(t, f.ua, f.projA)

	hist := f.history(t, f.ua, dm)
	assert.NotEmpty(t, hist.Messages, "the user's own DM history with the agent stays readable")
}

func TestChatRead_WatermarkMustBeInConversation(t *testing.T) {
	f := newRefFixture(t)
	other := f.seedMessage(t, f.projB.ID, f.topicB, "", "in project B")
	local := f.seedMessage(t, f.projA.ID, f.topicA, "", "in topic A")

	mark := func(id string) refAnswer {
		rec := doRequestAsUser(t, f.srv, f.ua, http.MethodPost,
			"/api/v1/chat/conversations/"+f.topicA+"/read", map[string]string{"messageId": id})
		return refAnswer{status: rec.Code, body: rec.Body.String()}
	}

	missing := mark(uuid.NewString())
	require.Equal(t, http.StatusBadRequest, missing.status, missing.body)
	assert.Contains(t, missing.body, "messageId does not refer to a message in this conversation")
	requireSameAnswer(t, missing, mark(other))

	rs, err := f.wcs.GetReadState(context.Background(), f.ua.ID, f.topicA)
	require.NoError(t, err)
	if rs != nil {
		assert.Empty(t, rs.LastReadMessageID, "a refused read marker is not stored")
	}

	ok := mark(local)
	require.Equal(t, http.StatusOK, ok.status, ok.body)
}

// A store error while checking a reply target refuses the send with the
// route's retryable answer; nothing is stored.
func TestChatReply_TargetLookupErrorRefusesSend(t *testing.T) {
	t.Run("message lookup", func(t *testing.T) {
		f := newRefFixture(t)
		target := f.seedMessage(t, f.projA.ID, f.topicA, "", "in topic A")
		before := f.threadMessageCount(t, f.topicA)

		f.faults.failGetMessage = true
		f.fault.Arm()
		got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "re", "reply_to_id": target})
		require.Equal(t, http.StatusServiceUnavailable, got.status, got.body)
		assert.Contains(t, got.body, "SERVICE_UNAVAILABLE")
		assert.Equal(t, before, f.threadMessageCount(t, f.topicA), "nothing is stored")
	})

	t.Run("conversation lookup", func(t *testing.T) {
		f := newRefFixture(t)
		dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
		// Stored under another thread key with a conversation ID, so the
		// check has to resolve the DM's conversation.
		target := f.seedMessage(t, f.projA.ID, "legacy-thread-key", uuid.NewString(), "elsewhere")
		before := f.threadMessageCount(t, dm)

		f.faults.failConvByExtRef = true
		f.fault.Arm()
		got := f.send(t, f.ua, dm, map[string]interface{}{"content": "re", "reply_to_id": target})
		require.Equal(t, http.StatusServiceUnavailable, got.status, got.body)
		assert.Equal(t, before, f.threadMessageCount(t, dm), "nothing is stored")
	})
}

func TestChatSend_DMAttachmentUploadedByOtherUserNotAccepted(t *testing.T) {
	f := newRefFixture(t)
	agentDM := dmKeyFor(t, "agent", f.aa.ID, "user", f.ua.ID)
	missing := f.unknownAttachmentAnswer(t, f.ua, agentDM)

	carolsUpload := f.attach(t, "", f.uc.ID, "carol.txt")
	requireSameAnswer(t, missing, f.attachmentRefusal(t, f.ua, agentDM, carolsUpload))

	// The sender's own direct-message upload is accepted.
	userDM := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	own := f.attach(t, "", f.ua.ID, "alice.txt")
	ok := f.send(t, f.ua, userDM, map[string]interface{}{"content": "mine", "attachments": []string{own}})
	require.Equal(t, http.StatusCreated, ok.status, ok.body)
}

func TestChatSend_ProjectAttachmentInDMNotAccepted(t *testing.T) {
	f := newRefFixture(t)
	agentDM := dmKeyFor(t, "agent", f.aa.ID, "user", f.ua.ID)
	userDM := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	ownProjectFile := f.attach(t, f.projA.ID, f.ua.ID, "a.txt")
	otherProjectFile := f.attach(t, f.projB.ID, f.ub.ID, "b.txt")

	for _, key := range []string{agentDM, userDM} {
		missing := f.unknownAttachmentAnswer(t, f.ua, key)
		requireSameAnswer(t, missing, f.attachmentRefusal(t, f.ua, key, ownProjectFile))
		requireSameAnswer(t, missing, f.attachmentRefusal(t, f.ua, key, otherProjectFile))
	}
}

func TestChatSend_TopicAttachmentFromOtherProjectNotAccepted(t *testing.T) {
	f := newRefFixture(t)
	missing := f.unknownAttachmentAnswer(t, f.ua, f.topicA)

	otherProjectFile := f.attach(t, f.projB.ID, f.ub.ID, "b.txt")
	requireSameAnswer(t, missing, f.attachmentRefusal(t, f.ua, f.topicA, otherProjectFile))
	dmUpload := f.attach(t, "", f.ua.ID, "dm.txt")
	requireSameAnswer(t, missing, f.attachmentRefusal(t, f.ua, f.topicA, dmUpload))

	before := f.threadMessageCount(t, f.topicA)
	ownFile := f.attach(t, f.projA.ID, f.ua.ID, "a.txt")
	ok := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "mine", "attachments": []string{ownFile}})
	require.Equal(t, http.StatusCreated, ok.status, ok.body)
	assert.Equal(t, before+1, f.threadMessageCount(t, f.topicA))
}

func TestChatSend_RefusedSendStagesNoFile(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	stagingDir := f.enableScratchpad(t)

	// The topic's default agent accepts no messages.
	sealed := &store.Agent{ID: tid("ref-agent-sealed"), ProjectID: f.projA.ID, Name: "sealed", Slug: "sealed",
		Phase: "running", OwnerID: f.ua.ID, CreatedBy: f.ua.ID, MessageMode: store.MessageModeNone}
	require.NoError(t, f.st.CreateAgent(ctx, sealed))
	require.NoError(t, f.wcs.UpdateTopic(ctx, f.topicA, TopicUpdate{DefaultAgent: &sealed.Slug}))

	file := f.attach(t, f.projA.ID, f.ua.ID, "a.txt")
	got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "see file", "attachments": []string{file}})
	require.Equal(t, http.StatusForbidden, got.status, got.body)
	assert.Contains(t, got.body, ErrCodeMessageDenied)
	_, err := os.Stat(filepath.Join(stagingDir, file))
	assert.True(t, os.IsNotExist(err), "a refused send must not stage its attachment (stat err: %v)", err)

	// The same send to an agent that accepts it stages the file.
	require.NoError(t, f.wcs.UpdateTopic(ctx, f.topicA, TopicUpdate{DefaultAgent: &f.aa.Slug}))
	ok := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "see file", "attachments": []string{file}})
	require.Equal(t, http.StatusCreated, ok.status, ok.body)
	_, err = os.Stat(filepath.Join(stagingDir, file))
	assert.NoError(t, err, "an accepted send stages its attachment")
}

func TestAttachmentDownload_DMUploadReadableByUploaderAndPeer(t *testing.T) {
	f := newRefFixture(t)
	file := f.dmUploadSent(t)
	for _, u := range []*store.User{f.ua, f.uc} {
		got := f.download(t, u, file)
		require.Equal(t, http.StatusOK, got.status, "%s: %s", u.DisplayName, got.body)
		assert.Equal(t, "content of shared.txt", got.body)
	}
	// An upload not sent anywhere yet downloads for its uploader.
	unsent := f.attach(t, "", f.ub.ID, "draft.txt")
	assert.Equal(t, http.StatusOK, f.download(t, f.ub, unsent).status)
}

func TestAttachmentDownload_DMUploadMatchesUnknownForOtherUser(t *testing.T) {
	f := newRefFixture(t)
	file := f.dmUploadSent(t)
	missing := f.download(t, f.ub, uuid.NewString())
	require.Equal(t, http.StatusNotFound, missing.status, missing.body)
	requireSameAnswer(t, missing, f.download(t, f.ub, file))
	unsent := f.attach(t, "", f.ua.ID, "draft.txt")
	requireSameAnswer(t, missing, f.download(t, f.ub, unsent))
}

func TestAttachmentDownload_ProjectFileMatchesUnknownForNonMember(t *testing.T) {
	f := newRefFixture(t)
	file := f.attach(t, f.projA.ID, f.ua.ID, "a.txt")
	require.Equal(t, http.StatusOK, f.download(t, f.ua, file).status, "a project reader downloads it")
	missing := f.download(t, f.ub, uuid.NewString())
	requireSameAnswer(t, missing, f.download(t, f.ub, file))
}

// listLinksFaultWCS fails ListMessageIDsForAttachment.
type listLinksFaultWCS struct {
	WebChatStore
}

func (listLinksFaultWCS) ListMessageIDsForAttachment(context.Context, string, int) ([]string, error) {
	return nil, errRefStoreFault
}

func TestAttachmentDownload_StoreErrorAnswersAsUnknown(t *testing.T) {
	f := newRefFixture(t)
	file := f.dmUploadSent(t)
	require.Equal(t, http.StatusOK, f.download(t, f.uc, file).status, "the peer can download it while the store works")

	missing := f.download(t, f.uc, uuid.NewString())
	f.srv.SetWebChatStore(listLinksFaultWCS{WebChatStore: f.wcs})
	requireSameAnswer(t, missing, f.download(t, f.uc, file))
}

// getTopicFaultWCS fails GetTopic.
type getTopicFaultWCS struct {
	WebChatStore
}

func (getTopicFaultWCS) GetTopic(context.Context, string) (*WebChatTopic, error) {
	return nil, errRefStoreFault
}

// Each lookup canReadNativeMessage makes denies on its own when it fails.
func TestCanReadNativeMessage_StoreErrorDenies(t *testing.T) {
	// setup returns alice, a message of topic A linked to the topic's
	// conversation, and one with no conversation (resolved by topic); both
	// are readable while the store works.
	setup := func(t *testing.T) (*refFixture, UserIdentity, *store.Message, *store.Message) {
		t.Helper()
		f := newRefFixture(t)
		ctx := context.Background()
		alice := NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb))
		convID, err := f.srv.conversationIDForKey(ctx, f.wcs, f.topicA)
		require.NoError(t, err)
		require.NotEmpty(t, convID)
		inConversation, err := f.st.GetMessage(ctx, f.seedMessage(t, f.projA.ID, f.topicA, convID, "with conversation"))
		require.NoError(t, err)
		byTopic, err := f.st.GetMessage(ctx, f.seedMessage(t, f.projA.ID, f.topicA, "", "by topic"))
		require.NoError(t, err)
		require.True(t, f.srv.canReadNativeMessage(ctx, alice, inConversation), "readable while the store works")
		require.True(t, f.srv.canReadNativeMessage(ctx, alice, byTopic), "readable while the store works")
		return f, alice, inConversation, byTopic
	}

	t.Run("conversation lookup", func(t *testing.T) {
		f, alice, inConversation, _ := setup(t)
		f.faults.failGetConversation = true
		f.fault.Arm()
		assert.False(t, f.srv.canReadNativeMessage(context.Background(), alice, inConversation))
	})

	t.Run("topic lookup", func(t *testing.T) {
		f, alice, _, byTopic := setup(t)
		f.srv.SetWebChatStore(getTopicFaultWCS{WebChatStore: f.wcs})
		assert.False(t, f.srv.canReadNativeMessage(context.Background(), alice, byTopic))
	})

	t.Run("project lookup", func(t *testing.T) {
		f, alice, _, byTopic := setup(t)
		f.faults.failGetProject = true
		f.fault.Arm()
		assert.False(t, f.srv.canReadNativeMessage(context.Background(), alice, byTopic))
	})
}

func TestAttachmentDownload_AgentDMFileReadableAfterProjectAccessEnds(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	// A file agent aa sent in its DM with alice: a file of project A,
	// uploaded by the agent, linked to the DM message.
	dm := dmKeyFor(t, "agent", f.aa.ID, "user", f.ua.ID)
	file := f.attach(t, f.projA.ID, f.aa.ID, "report.txt")
	msgID := f.seedMessage(t, f.projA.ID, dm, "", "here is the report")
	require.NoError(t, f.wcs.LinkAttachmentToMessage(ctx, msgID, file))

	require.Equal(t, http.StatusOK, f.download(t, f.ua, file).status)

	// alice loses access to project A.
	f.revokeProjectAccess(t, f.ua, f.projA)

	got := f.download(t, f.ua, file)
	require.Equal(t, http.StatusOK, got.status, "the DM's file stays readable with the DM: %s", got.body)

	// bob is a hub member outside the DM with no role in project A.
	missing := f.download(t, f.ub, uuid.NewString())
	requireSameAnswer(t, missing, f.download(t, f.ub, file))
	// A file of project A linked to no message bob can read stays unknown.
	unlinked := f.attach(t, f.projA.ID, f.aa.ID, "other.txt")
	requireSameAnswer(t, missing, f.download(t, f.ub, unlinked))
}

func TestCheckAccessError_DeniesGroupPost(t *testing.T) {
	f := newRefFixture(t)
	f.srv.SetDispatcher(&recordingDispatcher{})
	alice := NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb))

	// While the store works the owner passes the check: the participant
	// rows are written (delivery itself may fail later in this fixture).
	okConv := seedGroupConversation(t, f.st, f.projA.ID, "works")
	ok := postMessageWithConv(t, f.srv, alice, f.aa, okConv)
	require.NotContains(t, ok.body, "caller-supplied conversation_id does not exist", "the owner passes the check: %s", ok.body)
	require.Greater(t, participantCount(t, f.st, okConv), 0, "participant rows are written for an allowed post")

	groupA := seedGroupConversation(t, f.st, f.projA.ID, "general")
	before := participantCount(t, f.st, groupA)
	f.faults.failGetProject = true
	f.fault.Arm()
	got := postMessageWithConv(t, f.srv, alice, f.aa, groupA)
	missing := postMessageWithConv(t, f.srv, alice, f.aa, tid("check-access-error-unknown-conv"))
	require.Equal(t, http.StatusBadRequest, missing.status, missing.body)
	requireSameAnswer(t, missing, got)
	assert.Equal(t, before, participantCount(t, f.st, groupA), "no participant row is written")
}

// A refusal of the sender's access is answered as not found but stays
// distinguishable inside the hub, so a scheduled send records no_access.
func TestChatSendRefusal_MarkedAsAccessRefusal(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	bob := NewAuthenticatedUser(f.ub.ID, f.ub.Email, f.ub.DisplayName, f.ub.Role, string(ClientTypeWeb))

	_, refused := f.srv.authorizeChatSend(ctx, bob, f.topicA)
	require.NotNil(t, refused)
	_, missing := f.srv.authorizeChatSend(ctx, bob, uuid.NewString())
	require.NotNil(t, missing)

	assert.Equal(t, missing.Status, refused.Status)
	assert.Equal(t, missing.Message, refused.Message)
	assert.Equal(t, ScheduledFailureNoAccess, scheduledFailureFromSendError(refused))
	assert.Equal(t, ScheduledFailureDeliveryError, scheduledFailureFromSendError(missing))
}

// With no user, the DM participant check gives the same refusal as
// authorizeChatSend gives a request with no user.
func TestAuthorizeDMKeyParticipant_NoUserRefused(t *testing.T) {
	key := dmKeyFor(t, "user", tid("dm-no-user-a"), "user", tid("dm-no-user-b"))
	want := chatSendForbidden()

	got := authorizeDMKeyParticipant(context.Background(), nil, key, chatSendPath(key))
	require.NotNil(t, got)
	assert.Equal(t, *want, *got)

	var typedNil *AuthenticatedUser
	got = authorizeDMKeyParticipant(context.Background(), typedNil, key, chatSendPath(key))
	require.NotNil(t, got)
	assert.Equal(t, *want, *got)

	_, fromSend := (&Server{}).authorizeChatSend(context.Background(), nil, key)
	require.NotNil(t, fromSend)
	assert.Equal(t, *fromSend, *got, "same answer as authorizeChatSend with no user")
}
