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
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// refFixture is two projects with one owner each, a topic in each, and a
// third user who belongs to neither project. All three are hub members.
//
//	ua (alice): owner of A, no role in B
//	ub (bob):   owner of B, no role in A
//	uc (carol): no role in A or B
type refFixture struct {
	srv          *Server
	st           store.Store
	wcs          WebChatStore
	ua, ub, uc   *store.User
	projA, projB *store.Project
	topicA       string
	topicB       string
	// faults is the server's store; armed with fault.Arm(), it fails the
	// calls named in its fields.
	faults *refFaultStore
	fault  *storeFaultSwitch
	as     *LocalDiskAttachmentStore
	aa     *store.Agent // agent of project A, owned by ua
}

var errRefStoreFault = errors.New("store fault for test")

// refFaultStore fails selected store calls once its switch is armed. Set
// the fields before arming.
type refFaultStore struct {
	store.Store
	fault               *storeFaultSwitch
	failGetMessage      bool
	failConvByExtRef    bool
	failGetProject      bool
	failGetConversation bool
}

func (r *refFaultStore) GetMessage(ctx context.Context, id string) (*store.Message, error) {
	if r.failGetMessage && r.fault.Active() {
		return nil, errRefStoreFault
	}
	return r.Store.GetMessage(ctx, id)
}

func (r *refFaultStore) GetConversationByExternalRef(ctx context.Context, surface, ref string) (*store.Conversation, error) {
	if r.failConvByExtRef && r.fault.Active() {
		return nil, errRefStoreFault
	}
	return r.Store.GetConversationByExternalRef(ctx, surface, ref)
}

func (r *refFaultStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if r.failGetProject && r.fault.Active() {
		return nil, errRefStoreFault
	}
	return r.Store.GetProject(ctx, id)
}

func (r *refFaultStore) GetConversation(ctx context.Context, id string) (*store.Conversation, error) {
	if r.failGetConversation && r.fault.Active() {
		return nil, errRefStoreFault
	}
	return r.Store.GetConversation(ctx, id)
}

func newRefFixture(t *testing.T) *refFixture {
	t.Helper()
	srv, st, _, _, projA, faults, fault := setupDemoPolicyTestWithFault(t,
		func(inner store.Store, fault *storeFaultSwitch) *refFaultStore {
			return &refFaultStore{Store: inner, fault: fault}
		})
	db := openTestMemorySQLite(t, "sqlite3")
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	as, err := NewLocalDiskAttachmentStore(t.TempDir())
	require.NoError(t, err)
	srv.SetAttachmentStore(as)
	ctx := context.Background()

	ua, err := st.GetUser(ctx, tid("user-alice"))
	require.NoError(t, err)
	ub, err := st.GetUser(ctx, tid("user-bob"))
	require.NoError(t, err)

	uc := &store.User{
		ID:          tid("ref-user-carol"),
		Email:       "carol@test.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, uc))
	ensureHubMembership(ctx, st, uc.ID)

	projB := &store.Project{
		ID:        tid("ref-project-b"),
		Name:      "Project B",
		Slug:      "project-b",
		OwnerID:   ub.ID,
		CreatedBy: ub.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, projB))
	srv.seedProjectCreatorMembership(ctx, projB)

	f := &refFixture{
		srv: srv, st: st, wcs: wcs,
		ua: ua, ub: ub, uc: uc,
		projA: projA, projB: projB,
		topicA: tid("ref-topic-a"),
		topicB: tid("ref-topic-b"),
		faults: faults, fault: fault,
		as: as,
	}
	f.aa = &store.Agent{ID: tid("ref-agent-a"), ProjectID: projA.ID, Name: "aa", Slug: "aa",
		Phase: "running", OwnerID: ua.ID, CreatedBy: ua.ID}
	require.NoError(t, st.CreateAgent(ctx, f.aa))
	for _, tp := range []struct {
		id, project, by string
	}{{f.topicA, projA.ID, ua.ID}, {f.topicB, projB.ID, ub.ID}} {
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID: tp.id, ProjectID: tp.project, Name: "topic-" + tp.id[:8],
			CreatedBy: tp.by, CreatedAt: time.Now().UTC(),
		}))
		setTopicConversationID(t, db, st, tp.id, tp.project)
	}
	return f
}

// attach stores a file as uploaded by uploader into projectID ("" for a
// direct-message upload) and returns its ID.
func (f *refFixture) attach(t *testing.T, projectID, uploader, name string) string {
	t.Helper()
	ctx := context.Background()
	content := "content of " + name
	meta, err := f.as.Save(ctx, projectID, name, strings.NewReader(content), int64(len(content)), "text/plain")
	require.NoError(t, err)
	meta.ProjectID = projectID
	meta.UploadedBy = uploader
	require.NoError(t, f.wcs.CreateAttachment(ctx, meta))
	return meta.ID
}

// seedMessage stores a web chat message on thread directly.
func (f *refFixture) seedMessage(t *testing.T, projectID, thread, convID, text string) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, f.st.CreateMessage(context.Background(), &store.Message{
		ID: id, ProjectID: projectID,
		Sender: "user:seed", SenderID: f.ua.ID, Recipient: "thread:" + thread,
		Msg: text, Type: "instruction", Channel: "web",
		ThreadID: thread, ConversationID: convID,
		CreatedAt: time.Now().UTC(),
	}))
	return id
}

// threadMessageCount counts the web messages stored on thread.
func (f *refFixture) threadMessageCount(t *testing.T, thread string) int {
	t.Helper()
	res, err := f.st.ListMessages(context.Background(),
		store.MessageFilter{Channel: "web", ThreadID: thread}, store.ListOptions{Limit: 200})
	require.NoError(t, err)
	return len(res.Items)
}

// refAnswer is one HTTP answer: status and body.
type refAnswer struct {
	status int
	body   string
}

// requireSameAnswer asserts that two requests got exactly the same answer.
func requireSameAnswer(t *testing.T, want, got refAnswer) {
	t.Helper()
	assert.Equal(t, want.status, got.status, "status must match:\n  want %d %s\n  got  %d %s", want.status, want.body, got.status, got.body)
	assert.Equal(t, want.body, got.body, "body must match")
}

func (f *refFixture) send(t *testing.T, user *store.User, key string, body map[string]interface{}) refAnswer {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages", body)
	return refAnswer{status: rec.Code, body: rec.Body.String()}
}

func dmKeyFor(t *testing.T, kindA, idA, kindB, idB string) string {
	t.Helper()
	key, err := messages.DMConversationKey(kindA, idA, kindB, idB)
	require.NoError(t, err)
	return key
}

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

func (f *refFixture) history(t *testing.T, user *store.User, key string) chatHistoryResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
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
	ctx := context.Background()

	// An agent of project B, which carol cannot read (any more).
	agent := &store.Agent{ID: tid("ref-agent-b"), ProjectID: f.projB.ID, Name: "ab", Slug: "ab",
		Phase: "running", OwnerID: f.ub.ID, CreatedBy: f.ub.ID}
	require.NoError(t, f.st.CreateAgent(ctx, agent))
	require.False(t, f.srv.canReadProject(ctx, NewAuthenticatedUser(f.uc.ID, f.uc.Email, f.uc.DisplayName, f.uc.Role, string(ClientTypeWeb)), f.projB.ID))

	dm := dmKeyFor(t, "agent", agent.ID, "user", f.uc.ID)
	f.seedMessage(t, f.projB.ID, dm, "", "earlier DM message")

	hist := f.history(t, f.uc, dm)
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

// attachmentRefusal sends one attachment and returns the answer with the
// attachment ID replaced by a placeholder, so answers for different IDs
// can be compared.
func (f *refFixture) attachmentRefusal(t *testing.T, user *store.User, key, attachmentID string) refAnswer {
	t.Helper()
	got := f.send(t, user, key, map[string]interface{}{"content": "see file", "attachments": []string{attachmentID}})
	return refAnswer{status: got.status, body: strings.ReplaceAll(got.body, attachmentID, "<id>")}
}

func (f *refFixture) unknownAttachmentAnswer(t *testing.T, user *store.User, key string) refAnswer {
	t.Helper()
	missing := f.attachmentRefusal(t, user, key, uuid.NewString())
	require.Equal(t, http.StatusBadRequest, missing.status, missing.body)
	assert.Contains(t, missing.body, `attachment \"<id>\" is not available in this conversation`)
	return missing
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

// enableScratchpad gives project A a scratchpad shared dir backed by a
// directory on this host and returns the directory chat attachments are
// staged into.
func (f *refFixture) enableScratchpad(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj, err := f.st.GetProject(ctx, f.projA.ID)
	require.NoError(t, err)
	proj.SharedDirs = []api.SharedDir{{Name: attachmentSharedDirName}}
	require.NoError(t, f.st.UpdateProject(ctx, proj))
	sharedDir := config.SharedDirHostPath(home, proj.Slug, proj.ID, attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(sharedDir, 0o755))
	staging := f.srv.resolveAttachmentStaging(ctx, proj.ID)
	require.NotNil(t, staging, "staging must resolve for the test to mean anything")
	return staging.hostDir()
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

func (f *refFixture) download(t *testing.T, user *store.User, id string) refAnswer {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/chat/attachments/"+id, nil)
	return refAnswer{status: rec.Code, body: rec.Body.String()}
}

// dmUploadSent has alice upload a file in her DM with carol and send it
// there, and returns the attachment ID.
func (f *refFixture) dmUploadSent(t *testing.T) string {
	t.Helper()
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	file := f.attach(t, "", f.ua.ID, "shared.txt")
	got := f.send(t, f.ua, dm, map[string]interface{}{"content": "file", "attachments": []string{file}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	return file
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

func TestCanReadNativeMessage_StoreErrorDenies(t *testing.T) {
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

	f.faults.failGetConversation = true
	f.faults.failGetProject = true
	f.fault.Arm()
	assert.False(t, f.srv.canReadNativeMessage(ctx, alice, inConversation), "a conversation lookup error denies")
	assert.False(t, f.srv.canReadNativeMessage(ctx, alice, byTopic), "a project lookup error denies")
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
	membersGroup, err := f.st.GetGroupBySlug(ctx, "project:"+f.projA.Slug+":members")
	require.NoError(t, err)
	_ = f.st.RemoveGroupMember(ctx, membersGroup.ID, store.GroupMemberTypeUser, f.ua.ID)
	_, err = f.st.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.ua.ID)
	require.NoError(t, err)
	alice := NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb))
	require.False(t, f.srv.canReadProject(ctx, alice, f.projA.ID), "precondition: alice no longer reads project A")

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
	assert.NotEqual(t, http.StatusOK, got.status, "a project lookup error refuses the post: %s", got.body)
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
