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
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Web-chat attachments stored as message-scoped artifacts
// (ptone/scion#4595), end to end through the hub's chat routes.

// setAttachmentExperiments installs a registry in which
// hub.attachment_artifacts is on or off and hub.artifacts is on only when
// artifactsOn is set.
func setAttachmentExperiments(t *testing.T, srv *Server, attachmentsOn, artifactsOn bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.AttachmentArtifacts:
			e.Default = attachmentsOn
		case experiments.Artifacts:
			e.Default = artifactsOn
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, attachmentsOn, srv.experimentEnabled(experiments.AttachmentArtifacts))
	require.Equal(t, artifactsOn, srv.experimentEnabled(experiments.Artifacts))
}

// enableAttachmentArtifacts turns hub.attachment_artifacts on for f's
// server (hub.artifacts stays off: it is not required) and installs an
// artifact store and local blob storage. It returns the store.
func enableAttachmentArtifacts(t *testing.T, f *refFixture) artifacts.Store {
	t.Helper()
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
	require.True(t, f.srv.attachmentUploadsToArtifacts(f.sessionContext(f.ua)), "uploads go to the artifact service")
	return st
}

// sessionContext returns a context carrying user's session identity, as the
// upload route sees it.
func (f *refFixture) sessionContext(user *store.User) context.Context {
	return contextWithIdentity(context.Background(),
		NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeWeb)))
}

// uploadAs posts one file to the upload route as user, with project_id
// projectID ("" for a direct-message upload), and returns the attachment
// id.
func (f *refFixture) uploadAs(t *testing.T, user *store.User, projectID, name, content string) string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	require.NoError(t, mw.WriteField("project_id", projectID))
	h := map[string][]string{
		"Content-Disposition": {`form-data; name="files"; filename="` + name + `"`},
		"Content-Type":        {"text/plain"},
	}
	part, err := mw.CreatePart(h)
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	token, _, _, err := f.srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp struct {
		Attachments []attachmentUploadResult `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Attachments, 1)
	a := resp.Attachments[0]
	require.Equal(t, name, a.Name)
	require.Equal(t, "/api/v1/chat/attachments/"+a.ID, a.URL)
	return a.ID
}

// sendWith sends content with attachment ids as user to key, requires 201
// and returns the response.
func (f *refFixture) sendWith(t *testing.T, user *store.User, key string, ids ...string) chatMessageResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages",
		map[string]interface{}{"content": "see file", "attachments": ids})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp chatMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// requireMessageScoped asserts that id is an attachment artifact bound to
// message messageID.
func requireMessageScoped(t *testing.T, st artifacts.Store, id, messageID string) {
	t.Helper()
	a, err := st.GetAttachment(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, artifacts.ScopeKindMessage, a.ScopeKind)
	require.Equal(t, messageID, a.ScopeRef)
}

func TestAttachmentArtifacts_DMReadableByBothParticipantsOnly(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)

	id := f.uploadAs(t, f.ua, "", "shot.txt", "dm bytes")
	_, err := f.wcs.GetAttachment(context.Background(), id)
	require.Error(t, err, "the upload is not in the legacy store")
	missing := f.download(t, f.uc, uuid.NewString())
	require.Equal(t, http.StatusNotFound, missing.status, missing.body)

	// Unsent: the uploader only.
	require.Equal(t, http.StatusOK, f.download(t, f.ua, id).status)
	requireSameAnswer(t, missing, f.download(t, f.uc, id))

	resp := f.sendWith(t, f.ua, dm, id)
	require.Len(t, resp.Attachments, 1)
	assert.Equal(t, AttachmentRef{ID: id, Name: "shot.txt", MimeType: resp.Attachments[0].MimeType, Size: int64(len("dm bytes"))}, resp.Attachments[0])
	requireMessageScoped(t, st, id, resp.ID)

	for _, u := range []*store.User{f.ua, f.uc} {
		got := f.download(t, u, id)
		require.Equal(t, http.StatusOK, got.status, "%s: %s", u.DisplayName, got.body)
		assert.Equal(t, "dm bytes", got.body)
	}
	// bob is outside the DM: the same answer as an unknown id.
	requireSameAnswer(t, missing, f.download(t, f.ub, id))

	// History lists it with the same shape.
	hist := f.history(t, f.uc, dm)
	refs := hist.MessageAttachments[resp.ID]
	require.Len(t, refs, 1)
	assert.Equal(t, id, refs[0].ID)
	assert.Equal(t, "shot.txt", refs[0].Name)
}

func TestAttachmentArtifacts_ProjectThread(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	dave := tid("att-user-dave")
	createTestUserWithProjectRole(t, f.st, dave, "dave@test.com", f.projA.ID, store.ProjectRoleMember)
	daveUser, err := f.st.GetUser(context.Background(), dave)
	require.NoError(t, err)

	id := f.uploadAs(t, f.ua, f.projA.ID, "plan.txt", "thread bytes")
	resp := f.sendWith(t, f.ua, f.topicA, id)
	require.Len(t, resp.Attachments, 1)

	for _, u := range []*store.User{f.ua, daveUser} {
		got := f.download(t, u, id)
		require.Equal(t, http.StatusOK, got.status, "project reader %s: %s", u.DisplayName, got.body)
	}
	missing := f.download(t, f.ub, uuid.NewString())
	requireSameAnswer(t, missing, f.download(t, f.ub, id))
	requireSameAnswer(t, missing, f.download(t, f.uc, id))
}

func TestAttachmentArtifacts_ProjectlessGroupJoinAndLeave(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	conv, err := f.st.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "group", Surface: "native", ExternalRef: "group:none:" + uuid.NewString(), DriftState: "active",
	})
	require.NoError(t, err)
	for _, u := range []*store.User{f.ua, f.uc} {
		require.NoError(t, f.st.AddParticipant(ctx, &store.ConversationParticipant{ID: uuid.NewString(),
			ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: u.ID, Role: "member", JoinedAt: time.Now()}))
	}
	// A message of a group without a project carries the nil project id.
	msgID := f.seedMessage(t, uuid.Nil.String(), "group-thread-"+conv.ID, conv.ID, "in a group without a project")
	id := f.uploadAs(t, f.ua, "", "notes.txt", "group bytes")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, id, artifacts.PrincipalKindUser, f.ua.ID, msgID))

	missing := f.download(t, f.ub, uuid.NewString())
	require.Equal(t, http.StatusOK, f.download(t, f.uc, id).status, "a participant reads it")
	requireSameAnswer(t, missing, f.download(t, f.ub, id))

	// carol leaves: she loses it on the next request.
	require.NoError(t, f.st.RemoveParticipant(ctx, conv.ID, "user", f.uc.ID))
	requireSameAnswer(t, missing, f.download(t, f.uc, id))

	// bob joins: he gains it, history included.
	require.NoError(t, f.st.AddParticipant(ctx, &store.ConversationParticipant{ID: uuid.NewString(),
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: f.ub.ID, Role: "member", JoinedAt: time.Now()}))
	require.Equal(t, http.StatusOK, f.download(t, f.ub, id).status, "a new participant reads it")
}

func TestAttachmentArtifacts_DeletedMessage(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "shot.txt", "to delete")
	resp := f.sendWith(t, f.ua, dm, id)
	require.Equal(t, http.StatusOK, f.download(t, f.uc, id).status)
	missing := f.download(t, f.uc, uuid.NewString())

	rec := doRequestAsUser(t, f.srv, f.ua, http.MethodDelete, "/api/v1/chat/conversations/"+dm+"/messages/"+resp.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, u := range []*store.User{f.ua, f.uc} {
		requireSameAnswer(t, missing, f.download(t, u, id))
	}
	_, err := st.GetAttachment(context.Background(), id)
	require.ErrorIs(t, err, artifacts.ErrNotFound, "the attachment is retired with its message")
}

// A message deleted without the delete hook (as by a store-level purge)
// makes its attachments unreadable at once, and the maintenance pass
// retires them.
func TestAttachmentArtifacts_MessageGoneWithoutHook(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "shot.txt", "purged")
	resp := f.sendWith(t, f.ua, dm, id)
	keep := f.uploadAs(t, f.ua, "", "keep.txt", "kept")
	kept := f.sendWith(t, f.ua, dm, keep)
	missing := f.download(t, f.uc, uuid.NewString())

	require.NoError(t, f.wcs.SetMessageDeleted(ctx, resp.ID, time.Now()))
	requireSameAnswer(t, missing, f.download(t, f.uc, id))

	f.srv.retireArtifactAttachments(ctx, st, time.Now())
	_, err := st.GetAttachment(ctx, id)
	require.ErrorIs(t, err, artifacts.ErrNotFound, "the maintenance pass retires it")
	requireMessageScoped(t, st, keep, kept.ID)
	require.Equal(t, http.StatusOK, f.download(t, f.uc, keep).status)
}

// An upload not sent within artifacts.UploadTTL is retired by the
// maintenance pass.
func TestAttachmentArtifacts_UnsentUploadRetired(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	id := f.uploadAs(t, f.ua, "", "draft.txt", "never sent")

	f.srv.retireArtifactAttachments(ctx, st, time.Now())
	_, err := st.GetAttachment(ctx, id)
	require.NoError(t, err, "a fresh upload stays")

	f.srv.retireArtifactAttachments(ctx, st, time.Now().Add(artifacts.UploadTTL+time.Minute))
	_, err = st.GetAttachment(ctx, id)
	require.ErrorIs(t, err, artifacts.ErrNotFound)
}

func TestAttachmentArtifacts_CrossProjectAddressee(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	convID, err := f.srv.conversationIDForKey(ctx, f.wcs, f.topicA)
	require.NoError(t, err)
	msgID := f.seedMessage(t, f.projA.ID, f.topicA, convID, "for bob too")
	id := f.uploadAs(t, f.ua, "", "brief.txt", "addressed")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, id, artifacts.PrincipalKindUser, f.ua.ID, msgID))

	missing := f.download(t, f.ub, uuid.NewString())
	requireSameAnswer(t, missing, f.download(t, f.ub, id))

	// bob, of project B, is an addressee of the message.
	require.NoError(t, f.st.AddAddressee(ctx, &store.MessageAddressee{ID: uuid.NewString(), MessageID: msgID,
		PrincipalKind: "user", PrincipalID: f.ub.ID, Via: "explicit", DeliveryState: "delivered"}))
	require.Equal(t, http.StatusOK, f.download(t, f.ub, id).status, "an addressee reads it")
	requireSameAnswer(t, missing, f.download(t, f.uc, id))
}

func TestAttachmentArtifacts_LegacyFallback(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	// A file of the legacy store, sent and read while the experiment is on.
	file := f.dmUploadSent(t)
	for _, u := range []*store.User{f.ua, f.uc} {
		got := f.download(t, u, file)
		require.Equal(t, http.StatusOK, got.status, "%s: %s", u.DisplayName, got.body)
		assert.Equal(t, "content of shared.txt", got.body)
	}
	missing := f.download(t, f.ub, uuid.NewString())
	requireSameAnswer(t, missing, f.download(t, f.ub, file))
}

func TestAttachmentArtifacts_ExperimentOff(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	ctx := context.Background()

	// Sent while on.
	onID := f.uploadAs(t, f.ua, "", "on.txt", "stored while on")
	f.sendWith(t, f.ua, dm, onID)

	setAttachmentExperiments(t, f.srv, false, false)
	require.False(t, f.srv.attachmentUploadsToArtifacts(f.sessionContext(f.ua)))

	// Uploads go to the legacy store, exactly as before.
	offID := f.uploadAs(t, f.ua, "", "off.txt", "stored while off")
	meta, err := f.wcs.GetAttachment(ctx, offID)
	require.NoError(t, err)
	require.Equal(t, f.ua.ID, meta.UploadedBy)
	_, err = st.GetAttachment(ctx, offID)
	require.ErrorIs(t, err, artifacts.ErrNotFound)
	f.sendWith(t, f.ua, dm, offID)
	require.Equal(t, http.StatusOK, f.download(t, f.uc, offID).status)

	// What was stored while it was on stays readable.
	got := f.download(t, f.uc, onID)
	require.Equal(t, http.StatusOK, got.status, got.body)
	assert.Equal(t, "stored while on", got.body)
	missing := f.download(t, f.ub, uuid.NewString())
	requireSameAnswer(t, missing, f.download(t, f.ub, onID))
}

// Send refuses an upload of someone else and one already sent exactly like
// an unknown id.
func TestAttachmentArtifacts_SendRefusals(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	missing := f.unknownAttachmentAnswer(t, f.ua, dm)

	carols := f.uploadAs(t, f.uc, "", "carol.txt", "hers")
	requireSameAnswer(t, missing, f.attachmentRefusal(t, f.ua, dm, carols))
	a, err := st.GetAttachment(context.Background(), carols)
	require.NoError(t, err)
	require.Equal(t, artifacts.ScopeKindUpload, a.ScopeKind, "a refused send binds nothing")

	own := f.uploadAs(t, f.ua, "", "alice.txt", "mine")
	f.sendWith(t, f.ua, dm, own)
	requireSameAnswer(t, missing, f.attachmentRefusal(t, f.ua, dm, own))
	requireSameAnswer(t, missing, f.attachmentRefusal(t, f.ua, f.topicA, own))
}

// The download route gives one answer for an unknown id, an attachment
// artifact the caller cannot read, and a legacy attachment the caller
// cannot read.
func TestAttachmentArtifacts_DownloadBodiesIdentical(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	sent := f.uploadAs(t, f.ua, "", "a.txt", "x")
	f.sendWith(t, f.ua, dm, sent)
	unsent := f.uploadAs(t, f.ua, "", "b.txt", "y")
	legacy := f.attach(t, "", f.ua.ID, "c.txt")

	missing := f.download(t, f.ub, uuid.NewString())
	require.Equal(t, http.StatusNotFound, missing.status)
	for _, id := range []string{sent, unsent, legacy, "not-a-uuid"} {
		requireSameAnswer(t, missing, f.download(t, f.ub, id))
	}
}

// On the artifact routes an attachment id answers like an unknown id, also
// to a caller who reads its message, on grants and links included.
func TestAttachmentArtifacts_ArtifactRoutesAnswerNotFound(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	setAttachmentExperiments(t, f.srv, true, true)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "a.txt", "x")
	f.sendWith(t, f.ua, dm, id)
	require.Equal(t, http.StatusOK, f.download(t, f.uc, id).status, "carol reads the message")
	unknown := uuid.NewString()

	for _, c := range []struct {
		method, suffix string
		body           []byte
	}{
		{http.MethodGet, "", nil},
		{http.MethodGet, "/files/a.txt", nil},
		{http.MethodGet, "/versions", nil},
		{http.MethodPatch, "", []byte(`{"expiresAt":"2099-01-01T00:00:00Z"}`)},
		{http.MethodDelete, "", nil},
		{http.MethodPost, "/grants", []byte(`{"subjectKind":"principal","subjectRef":"user:` + f.ub.ID + `","permission":"read"}`)},
		{http.MethodPost, "/links", []byte(`{}`)},
	} {
		for _, u := range []*store.User{f.ua, f.uc} {
			got := userArtifactRequest(t, f.srv, u, c.method, "/api/v1/artifacts/"+id+c.suffix, c.body)
			want := userArtifactRequest(t, f.srv, u, c.method, "/api/v1/artifacts/"+unknown+c.suffix, c.body)
			assert.Equal(t, http.StatusNotFound, got.Code, "%s %s as %s: %s", c.method, c.suffix, u.DisplayName, got.Body.String())
			assert.Equal(t, want.Code, got.Code, "%s %s as %s", c.method, c.suffix, u.DisplayName)
			assert.Equal(t, want.Body.String(), got.Body.String(), "%s %s as %s", c.method, c.suffix, u.DisplayName)
		}
	}
	list := userArtifactRequest(t, f.srv, f.ua, http.MethodGet, "/api/v1/artifacts?mine=1", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.NotContains(t, list.Body.String(), id)
	require.Equal(t, http.StatusOK, f.download(t, f.uc, id).status, "still readable through the chat route")
}

// An attachment sent to an agent reaches the project's staging directory
// from its stored bytes.
func TestAttachmentArtifacts_StagedForAgent(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	stagingDir := f.enableScratchpad(t)
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.aa.Slug}))

	id := f.uploadAs(t, f.ua, f.projA.ID, "log.txt", "agent bytes")
	f.sendWith(t, f.ua, f.topicA, id)
	got, err := os.ReadFile(filepath.Join(stagingDir, id, "log.txt"))
	require.NoError(t, err, "the attachment is staged for the agent")
	assert.Equal(t, "agent bytes", string(got))
}

// A retry of a send whose upload the first send already bound, with the
// same idempotency key, gets the replay of the first send, not a refusal.
func TestAttachmentArtifacts_IdempotentRetryReplays(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "shot.txt", "once")
	body := map[string]interface{}{"content": "see file", "attachments": []string{id}, "idempotency_key": "retry-" + id}

	first := doRequestAsUser(t, f.srv, f.ua, http.MethodPost, "/api/v1/chat/conversations/"+dm+"/messages", body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	var sent chatMessageResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &sent))
	requireMessageScoped(t, st, id, sent.ID)
	before := f.threadMessageCount(t, dm)

	retry := doRequestAsUser(t, f.srv, f.ua, http.MethodPost, "/api/v1/chat/conversations/"+dm+"/messages", body)
	require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
	var replay chatMessageResponse
	require.NoError(t, json.Unmarshal(retry.Body.Bytes(), &replay))
	assert.Equal(t, sent.ID, replay.ID, "the replay names the first message")
	assert.Equal(t, before, f.threadMessageCount(t, dm), "nothing is sent twice")

	// Without the key, the same id is refused like an unknown one.
	missing := f.unknownAttachmentAnswer(t, f.ua, dm)
	requireSameAnswer(t, missing, f.attachmentRefusal(t, f.ua, dm, id))
}

// downloadAs serves the download route for identity directly (the route's
// handler, with identity as the request's caller and no credential record,
// as an internal caller would set it).
func (f *refFixture) downloadAs(t *testing.T, identity Identity, id string) refAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/attachments/"+id, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	f.srv.handleAttachmentDownload(rec, req, id)
	return refAnswer{status: rec.Code, body: rec.Body.String()}
}

// A user access token reaches attachments only within its boundary, as on
// the other inbox routes: a project-boundary token with inbox.read reads an
// attachment of its boundary project while its holder is a member, never
// one of a direct message between users, and never without inbox.read.
func TestAttachmentArtifacts_ScopedTokenBoundary(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	dmFile := f.uploadAs(t, f.ua, "", "dm.txt", "direct")
	f.sendWith(t, f.ua, dm, dmFile)
	threadFile := f.uploadAs(t, f.ua, f.projA.ID, "thread.txt", "in project A")
	f.sendWith(t, f.ua, f.topicA, threadFile)
	threadFileB := f.uploadAs(t, f.ub, f.projB.ID, "b.txt", "in project B")
	f.sendWith(t, f.ub, f.topicB, threadFileB)

	inA := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read")
	missing := f.downloadAs(t, inA, uuid.NewString())
	require.Equal(t, http.StatusNotFound, missing.status, missing.body)

	got := f.downloadAs(t, inA, threadFile)
	require.Equal(t, http.StatusOK, got.status, "boundary project, live member: %s", got.body)
	assert.Equal(t, "in project A", got.body)
	requireSameAnswer(t, missing, f.downloadAs(t, inA, dmFile))

	// Without inbox.read the token reads no attachment at all.
	noInbox := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read")
	requireSameAnswer(t, missing, f.downloadAs(t, noInbox, threadFile))

	// A token of bob bounded to project A, where bob is not a member, reads
	// nothing there, and nothing of his own project B either.
	bobInA := selfToken(t, f.ub.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read")
	requireSameAnswer(t, missing, f.downloadAs(t, bobInA, threadFile))
	requireSameAnswer(t, missing, f.downloadAs(t, bobInA, threadFileB))
	bobInB := selfToken(t, f.ub.ID, projectBoundary(f.projB.ID), "project:read", "inbox:read")
	require.Equal(t, http.StatusOK, f.downloadAs(t, bobInB, threadFileB).status)
}

// A user access token reads an attachment of a project's message only with
// project:read on that project through the token, as on the AttachmentStore
// download route, even when its holder is an addressee of the message: the
// addressee rule that admits bob's session does not widen his tokens.
func TestAttachmentArtifacts_TokenProjectMessageNeedsProjectRead(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	convID, err := f.srv.conversationIDForKey(ctx, f.wcs, f.topicA)
	require.NoError(t, err)
	msgID := f.seedMessage(t, f.projA.ID, f.topicA, convID, "for bob too")
	id := f.uploadAs(t, f.ua, "", "brief.txt", "addressed")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, id, artifacts.PrincipalKindUser, f.ua.ID, msgID))
	// bob, of project B, is an addressee of the message.
	require.NoError(t, f.st.AddAddressee(ctx, &store.MessageAddressee{ID: uuid.NewString(), MessageID: msgID,
		PrincipalKind: "user", PrincipalID: f.ub.ID, Via: "explicit", DeliveryState: "delivered"}))
	require.Equal(t, http.StatusOK, f.download(t, f.ub, id).status, "bob's session reads it as an addressee")

	missing := f.downloadAs(t, selfToken(t, f.ub.ID, hubBoundary(), "inbox:read"), uuid.NewString())
	require.Equal(t, http.StatusNotFound, missing.status, missing.body)

	// bob's hub-boundary tokens read nothing of project A, where he is not
	// a member: neither with inbox:read alone nor with project:read too.
	require.Equal(t, missing, f.downloadAs(t, selfToken(t, f.ub.ID, hubBoundary(), "inbox:read"), id), "inbox:read alone")
	require.Equal(t, missing, f.downloadAs(t, selfToken(t, f.ub.ID, hubBoundary(), "project:read", "inbox:read"), id), "with project:read")

	// alice, a member of project A, reads it with a token carrying both.
	got := f.downloadAs(t, selfToken(t, f.ua.ID, hubBoundary(), "project:read", "inbox:read"), id)
	require.Equal(t, http.StatusOK, got.status, got.body)
	assert.Equal(t, "addressed", got.body)
	require.Equal(t, missing, f.downloadAs(t, selfToken(t, f.ua.ID, hubBoundary(), "inbox:read"), id), "member without project:read")
}

// With the experiment on, uploads go to the artifact service only for a
// caller it can own as a user principal: a session or a user access token.
// A federated user, which the artifact service does not serve, uploads to
// the AttachmentStore as with the experiment off, instead of failing.
func TestAttachmentArtifacts_FederatedUploadsUseAttachmentStore(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	require.True(t, f.srv.attachmentUploadsToArtifacts(f.sessionContext(f.ua)), "session")
	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:write")
	require.True(t, f.srv.attachmentUploadsToArtifacts(contextWithIdentity(ctx, token)), "user access token")

	fed := NewFederatedUserIdentity("https://issuer.attachments.test", "att-fed", "att-fed@test.com", "Fed", "member", nil)
	fedCtx := contextWithCredentialContext(contextWithIdentity(ctx, fed), CredentialContext{Kind: CredentialKindFederation})
	require.False(t, f.srv.attachmentUploadsToArtifacts(fedCtx), "federated user")
	require.False(t, f.srv.attachmentUploadsToArtifacts(ctx), "no identity")

	// The artifact service would refuse the federated upload, which is why
	// it is routed to the AttachmentStore.
	_, _, _, ok := newArtifactHost(f.srv).Principal(fedCtx)
	require.False(t, ok)
}

// While the artifacts settings section is disabled, stored attachments
// answer as unknown ids on download and are refused on send like unknown
// ones; enabled again, they are back.
func TestAttachmentArtifacts_SettingsSectionDisabled(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	sent := f.uploadAs(t, f.ua, "", "sent.txt", "x")
	f.sendWith(t, f.ua, dm, sent)
	unsent := f.uploadAs(t, f.ua, "", "unsent.txt", "y")

	ops := artifactsOps(t, `{"enabled":false}`)
	f.srv.SetOperationalSettings(ops)
	require.False(t, f.srv.attachmentArtifactsReady())
	missing := f.download(t, f.uc, uuid.NewString())
	requireSameAnswer(t, missing, f.download(t, f.uc, sent))
	refused := f.unknownAttachmentAnswer(t, f.ua, dm)
	requireSameAnswer(t, refused, f.attachmentRefusal(t, f.ua, dm, unsent))

	// Enabled again (a fresh settings source with the section at its
	// default, enabled), the stored attachment reads as before.
	f.srv.SetOperationalSettings(artifactsOps(t, ""))
	require.True(t, f.srv.attachmentArtifactsReady())
	require.Equal(t, http.StatusOK, f.download(t, f.uc, sent).status)
}

// failBindStore is an artifact store whose BindAttachment always loses, as
// to a concurrent send of the same upload.
type failBindStore struct{ artifacts.Store }

func (failBindStore) BindAttachment(context.Context, string, string, string, string, time.Time) error {
	return artifacts.ErrAttachmentBound
}

// An attachment whose bind is lost after the message is stored is left out
// of the message: not in the response, not in history.
func TestAttachmentArtifacts_LostBindLeftOut(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "race.txt", "lost")
	f.srv.SetArtifactStore(failBindStore{st})

	resp := f.sendWith(t, f.ua, dm, id)
	assert.Empty(t, resp.Attachments, "the response names no attachment it could not bind")
	hist := f.history(t, f.ua, dm)
	assert.Empty(t, hist.MessageAttachments[resp.ID], "history names none either")
	a, err := st.GetAttachment(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, artifacts.ScopeKindUpload, a.ScopeKind, "the upload is still unsent")
}

// With the artifact service serving the route and no legacy store, an
// unknown id gets the same answer as an attachment the caller may not read.
func TestAttachmentArtifacts_NoLegacyStoreAnswersNotFound(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "a.txt", "x")
	f.sendWith(t, f.ua, dm, id)
	f.srv.SetAttachmentStore(nil)

	missing := f.download(t, f.ub, uuid.NewString())
	require.Equal(t, http.StatusNotFound, missing.status, missing.body)
	requireSameAnswer(t, missing, f.download(t, f.ub, id))
	require.Equal(t, http.StatusOK, f.download(t, f.uc, id).status)
}
