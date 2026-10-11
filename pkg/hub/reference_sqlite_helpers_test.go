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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func seedGroupConversation(t *testing.T, s store.Store, projectID, name string) string {
	t.Helper()
	pid := projectID
	conv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind: "group", Surface: "native", ExternalRef: "group:" + projectID + ":" + name,
		ProjectID: &pid, DriftState: "active",
	})
	require.NoError(t, err)
	return conv.ID
}

func participantCount(t *testing.T, s store.Store, convID string) int {
	t.Helper()
	parts, err := s.ListParticipants(context.Background(), convID)
	require.NoError(t, err)
	return len(parts)
}

func postMessageWithConv(t *testing.T, srv *Server, sender Identity, target *store.Agent, convID string) refAnswer {
	t.Helper()
	body, err := json.Marshal(MessageRequest{StructuredMessage: &messages.StructuredMessage{
		Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender: "x:" + sender.ID(), SenderID: sender.ID(),
		Recipient: "agent:" + target.Slug, RecipientID: target.ID,
		Msg: "into a group", Type: messages.TypeInstruction, ConversationID: convID,
	}})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(contextWithIdentity(r.Context(), sender))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, r, target.ID)
	return refAnswer{status: rr.Code, body: rr.Body.String()}
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

func (f *refFixture) send(t *testing.T, user *store.User, key string, body map[string]interface{}) refAnswer {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages", body)
	return refAnswer{status: rec.Code, body: rec.Body.String()}
}

func (f *refFixture) history(t *testing.T, user *store.User, key string) chatHistoryResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// revokeProjectAccess removes user from project's members group and deletes
// all of user's role bindings (in every project and at hub level, which is
// broader than this one project), then checks that the user can no longer
// read project.
func (f *refFixture) revokeProjectAccess(t *testing.T, user *store.User, project *store.Project) {
	t.Helper()
	ctx := context.Background()
	membersGroup, err := f.st.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	// A user whose access comes from a role binding alone is not in the
	// members group; that case is not an error here.
	if err := f.st.RemoveGroupMember(ctx, membersGroup.ID, store.GroupMemberTypeUser, user.ID); err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
	}
	_, err = f.st.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	require.NoError(t, err)
	ident := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeWeb))
	require.False(t, f.srv.canReadProject(ctx, ident, project.ID), "precondition: the user no longer reads the project")
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

// addParticipantAnswer posts an add-participant request as user.
func (f *refFixture) addParticipantAnswer(t *testing.T, user *store.User, convID, kind, principalID string) refAnswer {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/conversations/"+convID+"/participants",
		map[string]string{"principalKind": kind, "principalId": principalID})
	return refAnswer{status: rec.Code, body: rec.Body.String()}
}

// agentOfB creates an agent in project B.
func (f *refFixture) agentOfB(t *testing.T) *store.Agent {
	t.Helper()
	bb := &store.Agent{ID: tid("ref-agent-b"), ProjectID: f.projB.ID, Name: "bb", Slug: "bb",
		Phase: "running", OwnerID: f.ub.ID, CreatedBy: f.ub.ID}
	require.NoError(t, f.st.CreateAgent(context.Background(), bb))
	return bb
}

// postGroupMessage posts a group[...] message anchored on f.aa as alice.
func (f *refFixture) postGroupMessage(t *testing.T, recipient string) refAnswer {
	t.Helper()
	alice := NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb))
	return postAgentMessageAs(t, f.srv, func(c context.Context) context.Context { return contextWithIdentity(c, alice) },
		f.aa, MessageRequest{StructuredMessage: &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Recipient: recipient, Msg: "to a group", Type: messages.TypeInstruction,
		}})
}

// refFaultStore fails selected store calls once its switch is armed. Set
// the fields before arming.
type refFaultStore struct {
	store.Store
	fault               *storeFaultSwitch
	failGetMessage      bool
	failConvByExtRef    bool
	failGetProject      bool
	failGetConversation bool
	// failGetProjectID, when set, fails GetProject for that project only.
	failGetProjectID string
}

func dmKeyFor(t *testing.T, kindA, idA, kindB, idB string) string {
	t.Helper()
	key, err := messages.DMConversationKey(kindA, idA, kindB, idB)
	require.NoError(t, err)
	return key
}

// postAgentMessageAs posts req to /api/v1/agents/{target}/message with the
// given request context set up by withCtx.
func postAgentMessageAs(t *testing.T, srv *Server, withCtx func(context.Context) context.Context, target *store.Agent, req MessageRequest) refAnswer {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withCtx(r.Context()))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, r, target.ID)
	return refAnswer{status: rr.Code, body: rr.Body.String()}
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
	if (r.failGetProject || (r.failGetProjectID != "" && id == r.failGetProjectID)) && r.fault.Active() {
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
