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
	"strings"
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
	token, _, _, err := f.srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	req := attachmentUploadRequest(t, projectID, name, content)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return uploadedAttachmentID(t, rec, name)
}

// attachmentUploadRequest returns a POST of one text file to the upload
// route, with project_id projectID ("" for a direct-message upload).
func attachmentUploadRequest(t *testing.T, projectID, name, content string) *http.Request {
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
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// uploadedAttachmentID requires a 201 upload answer naming one attachment
// called name and returns its id.
func uploadedAttachmentID(t *testing.T, rec *httptest.ResponseRecorder, name string) string {
	t.Helper()
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

// User access tokens read no attachment stored as an artifact: whatever the
// token carries and whatever the conversation, including an upload not sent
// yet, a token gets the answer of an unknown id, while the session of the
// same user reads the attachment. Token access comes with its own design
// (ptone/scion#4596).
func TestAttachmentArtifacts_TokensReadNoArtifactAttachments(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	threadFile := f.uploadAs(t, f.ua, f.projA.ID, "thread.txt", "in project A")
	f.sendWith(t, f.ua, f.topicA, threadFile)
	unsent := f.uploadAs(t, f.ua, f.projA.ID, "unsent.txt", "not sent yet")
	userDM := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	dmFile := f.uploadAs(t, f.ua, "", "dm.txt", "between users")
	f.sendWith(t, f.ua, userDM, dmFile)
	agentDM := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)
	agentMsg := f.seedMessage(t, f.projA.ID, agentDM, "", "for the agent")
	agentFile := f.uploadAs(t, f.ua, f.projA.ID, "agent.txt", "agent dm")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, agentFile, artifacts.PrincipalKindUser, f.ua.ID, agentMsg))
	conv, err := f.st.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "group", Surface: "native", ExternalRef: "group:none:" + uuid.NewString(), DriftState: "active",
	})
	require.NoError(t, err)
	require.NoError(t, f.st.AddParticipant(ctx, &store.ConversationParticipant{ID: uuid.NewString(),
		ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: f.ua.ID, Role: "member", JoinedAt: time.Now()}))
	groupMsg := f.seedMessage(t, uuid.Nil.String(), "group-thread-"+conv.ID, conv.ID, "in a group without a project")
	groupFile := f.uploadAs(t, f.ua, "", "group.txt", "projectless group")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, groupFile, artifacts.PrincipalKindUser, f.ua.ID, groupMsg))

	hubToken := selfToken(t, f.ua.ID, hubBoundary(), "project:read", "inbox:read", "agent:read")
	missing := f.downloadAs(t, hubToken, uuid.NewString())
	require.Equal(t, http.StatusNotFound, missing.status, missing.body)
	tokens := []struct {
		name  string
		token *ScopedUserIdentity
	}{
		{"hub boundary", hubToken},
		{"project A boundary", selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read", "agent:read")},
	}
	files := []struct{ name, id string }{
		{"project thread", threadFile},
		{"upload not sent", unsent},
		{"direct message between users", dmFile},
		{"direct message with an agent", agentFile},
		{"group without a project", groupFile},
	}
	for _, file := range files {
		require.Equal(t, http.StatusOK, f.download(t, f.ua, file.id).status, "session: %s", file.name)
		for _, tk := range tokens {
			require.Equal(t, missing, f.downloadAs(t, tk.token, file.id), "%s token: %s", tk.name, file.name)
		}
	}
}

// tokenRequest serves the chat conversation routes for a user access token:
// the chat route wrapper and the handler, with token as the request's caller.
func (f *refFixture) tokenRequest(t *testing.T, token *ScopedUserIdentity, method, path string, body interface{}) refAnswer {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.srv.chatRoute(f.srv.handleChatConversationRoutes)(rec, req.WithContext(contextWithIdentity(req.Context(), token)))
	return refAnswer{status: rec.Code, body: rec.Body.String()}
}

// A user access token cannot send an attachment stored as an artifact: an
// upload its holder made in a session gets the same answer as an unknown id
// when the token names it, and stays an unsent upload the session can send.
func TestAttachmentArtifacts_TokenCannotSendArtifactAttachment(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	id := f.uploadAs(t, f.ua, f.projA.ID, "draft.txt", "session upload")
	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read", "inbox:write")
	path := "/api/v1/chat/conversations/" + f.topicA + "/messages"

	unknown := uuid.NewString()
	missing := f.tokenRequest(t, token, http.MethodPost, path, map[string]interface{}{"content": "see file", "attachments": []string{unknown}})
	require.Equal(t, http.StatusBadRequest, missing.status, missing.body)
	require.Contains(t, missing.body, "not available in this conversation")
	got := f.tokenRequest(t, token, http.MethodPost, path, map[string]interface{}{"content": "see file", "attachments": []string{id}})
	require.Equal(t, missing.status, got.status, got.body)
	require.Equal(t, strings.ReplaceAll(missing.body, unknown, "ID"), strings.ReplaceAll(got.body, id, "ID"), "the same answer as an unknown id")

	// Still an unsent upload of alice: her session sends it.
	resp := f.sendWith(t, f.ua, f.topicA, id)
	require.Len(t, resp.Attachments, 1)
}

// A user access token's history of a conversation does not list attachments
// stored as artifacts; the session's history of the same message does.
func TestAttachmentArtifacts_TokenHistoryOmitsArtifactAttachments(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "listed.txt", "for history")
	resp := f.sendWith(t, f.ua, dm, id)
	require.Len(t, f.history(t, f.ua, dm).MessageAttachments[resp.ID], 1, "the session's history lists it")

	token := selfToken(t, f.ua.ID, hubBoundary(), "inbox:read")
	got := f.tokenRequest(t, token, http.MethodGet, "/api/v1/chat/conversations/"+dm+"/messages", nil)
	require.Equal(t, http.StatusOK, got.status, got.body)
	var hist chatHistoryResponse
	require.NoError(t, json.Unmarshal([]byte(got.body), &hist))
	require.NotEmpty(t, hist.Messages, "the token reads the conversation")
	require.Empty(t, hist.MessageAttachments[resp.ID], "the token's history lists no attachment stored as an artifact")
}

// streamEventAs opens the agent messages stream of agentID as identity,
// publishes evt on agent.<agentID>.message until its text arrives, and
// returns the stream body.
func streamEventAs(t *testing.T, srv *Server, events *ChannelEventPublisher, identity UserIdentity, agentID string, evt UserMessageEvent) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID+"/messages/stream", nil)
	rctx := context.WithValue(contextWithIdentity(ctx, identity), userContextKey{}, identity)
	rec := &syncRecorder{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.mux.ServeHTTP(rec, req.WithContext(rctx))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(rec.String(), evt.Msg) {
		events.PublishRaw("agent."+agentID+".message", evt)
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	body := rec.String()
	require.Contains(t, body, evt.Msg, "stream never delivered the event")
	return body
}

// The agent messages stream lists attachments stored as artifacts only to a
// session: a user access token's stream gets each event with only the
// attachments of the AttachmentStore, while the session's stream lists both.
func TestAttachmentArtifacts_AgentStreamOmitsArtifactAttachmentsForTokens(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	f.srv.events = events
	legacy := f.attach(t, f.projA.ID, f.ua.ID, "legacy.txt")
	artifact := f.uploadAs(t, f.ua, f.projA.ID, "artifact-file.txt", "stored as an artifact")
	evt := UserMessageEvent{
		ID: uuid.NewString(), SenderID: f.ua.ID, RecipientID: f.aa.ID, AgentID: f.aa.ID, Msg: "message-with-files",
		Attachments: []AttachmentRef{{ID: legacy, Name: "legacy.txt"}, {ID: artifact, Name: "artifact-file.txt"}},
	}

	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "agent:read")
	body := streamEventAs(t, f.srv, events, token, f.aa.ID, evt)
	assert.Contains(t, body, legacy, "the AttachmentStore attachment is listed to the token")
	require.NotContains(t, body, artifact, "the attachment stored as an artifact is not listed to the token")
	require.NotContains(t, body, "artifact-file.txt")

	session := NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb))
	body = streamEventAs(t, f.srv, events, session, f.aa.ID, evt)
	assert.Contains(t, body, legacy)
	assert.Contains(t, body, artifact, "the session's stream lists the attachment stored as an artifact")
}

// With the experiment on, a user access token's upload is stored in the
// AttachmentStore, not in the artifact service, and the token reads it back
// under the AttachmentStore's rule, as with the experiment off.
func TestAttachmentArtifacts_TokenUploadsUseAttachmentStore(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:read", "inbox:write")

	req := attachmentUploadRequest(t, f.projA.ID, "token.txt", "from a token")
	rec := httptest.NewRecorder()
	f.srv.chatRoute(f.srv.handleChatAttachments)(rec, req.WithContext(contextWithIdentity(req.Context(), token)))
	id := uploadedAttachmentID(t, rec, "token.txt")

	meta, err := f.wcs.GetAttachment(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, meta, "the upload is a row of the AttachmentStore")
	assert.Equal(t, f.ua.ID, meta.UploadedBy)
	assert.Equal(t, f.projA.ID, meta.ProjectID)
	_, err = st.GetAttachment(ctx, id)
	require.ErrorIs(t, err, artifacts.ErrNotFound, "nothing is stored in the artifact service")

	got := f.downloadAs(t, token, id)
	require.Equal(t, http.StatusOK, got.status, got.body)
	assert.Equal(t, "from a token", got.body)
}

// With the experiment on, uploads go to the artifact service only for a
// caller it lets read attachments: a session. A user access token and a
// federated user upload to the AttachmentStore as with the experiment off,
// instead of storing a file they could not read back (or failing).
func TestAttachmentArtifacts_NonSessionUploadsUseAttachmentStore(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	require.True(t, f.srv.attachmentUploadsToArtifacts(f.sessionContext(f.ua)), "session")
	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "project:read", "inbox:write")
	require.False(t, f.srv.attachmentUploadsToArtifacts(contextWithIdentity(ctx, token)), "user access token")

	fed := NewFederatedUserIdentity("https://issuer.attachments.test", "att-fed", "att-fed@test.com", "Fed", "member", nil)
	fedCtx := contextWithCredentialContext(contextWithIdentity(ctx, fed), CredentialContext{Kind: CredentialKindFederation})
	require.False(t, f.srv.attachmentUploadsToArtifacts(fedCtx), "federated user")
	require.False(t, f.srv.attachmentUploadsToArtifacts(ctx), "no identity")

	// The artifact service could not own a federated upload either.
	_, _, _, ok := newArtifactHost(f.srv).Principal(fedCtx)
	require.False(t, ok)
}

// End to end on the upload route (the chat route wrapper and the handler,
// with a federated identity and credential): with the experiment on, a
// federated user's project upload answers 201 and is stored in the
// AttachmentStore, not in the artifact service. The persistent store cannot
// record a federated binding today, so the binding is held by the
// federatedBindingStore the federated authz tests use.
func TestAttachmentArtifacts_FederatedUploadEndToEnd(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	fed := fedIdentity("att-upload")
	fs := &federatedBindingStore{Store: f.srv.authzService.store, groups: map[string][]string{}, users: map[string]*store.User{}}
	orig := f.srv.authzService.store
	f.srv.authzService.store = fs
	t.Cleanup(func() { f.srv.authzService.store = orig })
	rd, err := f.st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	fs.addBinding(&store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: fed.ID(),
		ScopeType: store.RoleScopeProject, ScopeID: f.projA.ID, CreatedBy: "test",
	})

	req := attachmentUploadRequest(t, f.projA.ID, "fed.txt", "from a federated user")
	rctx := contextWithCredentialContext(contextWithIdentity(req.Context(), fed), CredentialContext{Kind: CredentialKindFederation})
	rec := httptest.NewRecorder()
	f.srv.chatRoute(f.srv.handleChatAttachments)(rec, req.WithContext(rctx))
	id := uploadedAttachmentID(t, rec, "fed.txt")

	meta, err := f.wcs.GetAttachment(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, meta, "the upload is a row of the AttachmentStore")
	assert.Equal(t, fed.ID(), meta.UploadedBy)
	assert.Equal(t, f.projA.ID, meta.ProjectID)
	_, err = st.GetAttachment(ctx, id)
	require.ErrorIs(t, err, artifacts.ErrNotFound, "nothing is stored in the artifact service")
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
