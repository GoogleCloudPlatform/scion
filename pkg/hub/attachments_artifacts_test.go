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
	"log/slog"
	"maps"
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
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Web-chat attachments stored as message-scoped artifacts
// (ptone/scion#4595), end to end through the hub's chat routes.

// setAttachmentExperiments installs a registry in which
// hub.attachment_artifacts is on or off and hub.artifacts is on only when
// artifactsOn is set. hub.attachment_artifacts is also stored as an
// override (storeAttachmentArtifactsOverride): a hub whose artifact service
// is ready defaults it on, so only a stored override turns it off there.
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
	storeAttachmentArtifactsOverride(t, srv, attachmentsOn)
	require.Equal(t, attachmentsOn, srv.experimentEnabled(experiments.AttachmentArtifacts))
	require.Equal(t, artifactsOn, srv.experimentEnabled(experiments.Artifacts))
}

// storeAttachmentArtifactsOverride stores an explicit override of
// hub.attachment_artifacts in srv's operational settings, as an admin's
// setting is stored. Turning it off on a server without operational
// settings installs settings over an empty store; turning it on there
// stores nothing, because it already resolves on.
func storeAttachmentArtifactsOverride(t *testing.T, srv *Server, on bool) {
	t.Helper()
	ops := srv.GetOperationalSettings()
	if ops == nil {
		if on {
			// Without stored settings the experiment already resolves on
			// (the deployment default, or the test registry's compiled
			// default). Installing settings here would also change other
			// values the server reads from them, such as the conversation
			// envelope switch the agent messages stream applies.
			return
		}
		ops = NewOperationalSettings(newFakeHubSettingStore(), emptyKoanf(), emptyKoanf())
		_, err := ops.Refresh(context.Background())
		require.NoError(t, err)
		srv.SetOperationalSettings(ops)
	}
	snap := ops.ExperimentsSnapshot()
	overrides := make(map[string]bool, len(snap.Overrides)+1)
	for k, v := range snap.Overrides {
		overrides[k] = v
	}
	overrides[experiments.AttachmentArtifacts] = on
	doc, err := json.Marshal(map[string]interface{}{"overrides": overrides})
	require.NoError(t, err)
	_, err = ops.Update(context.Background(), "experiments", doc, "test", snap.Revision, "managed")
	require.NoError(t, err)
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
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, id, artifacts.PrincipalKindUser, f.ua.ID, msgID, ""))

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
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, id, artifacts.PrincipalKindUser, f.ua.ID, msgID, f.projA.ID))

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

// While the experiment is on, an attachment stored as an artifact sent to
// an agent reaches it by delivery into its home (ptone/scion#4596), and is
// additionally projected into the project's scratchpad through the layout
// authority so sibling agents find it (ptone/scion#5827): the
// projection holds the stored bytes under .attachments/_webchat/<id>/.
func TestAttachmentArtifacts_ProjectedForSiblings(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	stagingDir := f.enableScratchpad(t)
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.aa.Slug}))

	id := f.uploadAs(t, f.ua, f.projA.ID, "log.txt", "agent bytes")
	f.sendWith(t, f.ua, f.topicA, id)
	got, err := os.ReadFile(filepath.Join(stagingDir, id, "log.txt"))
	require.NoError(t, err, "the attachment is projected into the scratchpad")
	assert.Equal(t, "agent bytes", string(got))
}

// stageChatAttachments records an artifact attachment's projection on its
// reference (experiment on, project topic thread) and adds no legacy entry
// for it; the delivery built from the reference carries the projection after
// the inbox path. On a direct conversation's key the same attachment is not
// projected.
func TestStageChatAttachments_ArtifactProjectionOnRef(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	stagingDir := f.enableScratchpad(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, f.projA.ID, "p.txt", "projected")
	resp := f.sendWith(t, f.ua, dm, id)

	info, err := f.srv.attachmentService().MessageAttachments(context.Background(), []string{resp.ID})
	require.NoError(t, err)
	require.Len(t, info[resp.ID], 1)

	// Direct conversation key: inbox only.
	refs := []AttachmentRef{newArtifactAttachmentRef(info[resp.ID][0])}
	paths, mounts := f.srv.stageChatAttachments(context.Background(), dm, f.projA.ID, resp.ID, refs)
	assert.Empty(t, paths)
	assert.Empty(t, mounts)
	assert.Empty(t, refs[0].projectedPath, "a DM attachment is never projected")
	assert.NoDirExists(t, filepath.Join(stagingDir, id))

	// Project topic thread key: projected.
	refs = []AttachmentRef{newArtifactAttachmentRef(info[resp.ID][0])}
	paths, mounts = f.srv.stageChatAttachments(context.Background(), f.topicA, f.projA.ID, resp.ID, refs)
	assert.Empty(t, paths, "a delivered artifact attachment adds no legacy entry")
	assert.Empty(t, mounts)
	assert.Equal(t, "/scion-volumes/scratchpad/.attachments/_webchat/"+id+"/p.txt", refs[0].projectedPath)
	assert.Equal(t, attachmentSharedDirName, refs[0].projectedMount)
	assert.FileExists(t, filepath.Join(stagingDir, id, "p.txt"))

	dels := attachmentDeliveries(resp.ID, refs)
	require.Len(t, dels, 1)
	assert.Equal(t, refs[0].projectedPath, dels[0].ProjectedPath)
	assert.Equal(t, attachmentSharedDirName, dels[0].ProjectedMount)
}

// TestAttachmentArtifacts_DMToAgentNotProjected: with artifacts on, an
// attachment sent to an agent in a direct conversation is delivered to the
// inbox only and never projected into the scratchpad.
// The experiment is read at send time: once it is off, the same kind of
// send stages the legacy attachment for the agent (T1 behaviour, the
// documented T1-only Low).
func TestAttachmentArtifacts_DMToAgentNotProjected(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	stagingDir := f.enableScratchpad(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)

	id := f.uploadAs(t, f.ua, "", "secret.txt", "dm bytes")
	f.sendWith(t, f.ua, dm, id)
	assert.NoDirExists(t, filepath.Join(stagingDir, id), "a DM attachment is not projected while artifacts are on")

	setAttachmentExperiments(t, f.srv, false, false)
	legacy := f.uploadAs(t, f.ua, "", "legacy.txt", "legacy bytes")
	f.sendWith(t, f.ua, dm, legacy)
	got, err := os.ReadFile(filepath.Join(stagingDir, legacy, "legacy.txt"))
	require.NoError(t, err, "with artifacts off a DM attachment is staged (no inbox delivery exists)")
	assert.Equal(t, "legacy bytes", string(got))
}

// TestAttachmentArtifacts_UploadedOnSentOffNotProjectedInDM: an attachment
// uploaded as an artifact while the experiment was on, and sent after it
// was turned off, is still delivered into the recipient's inbox, so in a
// direct conversation it gets no scratchpad copy either; in a project topic
// thread it is staged as before.
func TestAttachmentArtifacts_UploadedOnSentOffNotProjectedInDM(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	stagingDir := f.enableScratchpad(t)
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.aa.Slug}))
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID)

	inDM := f.uploadAs(t, f.ua, "", "dm.txt", "dm bytes")
	inTopic := f.uploadAs(t, f.ua, f.projA.ID, "topic.txt", "topic bytes")
	setAttachmentExperiments(t, f.srv, false, false)

	resp := f.sendWith(t, f.ua, dm, inDM)
	assert.NoDirExists(t, filepath.Join(stagingDir, inDM), "an artifact attachment in a DM gets no scratchpad copy, experiment on or off")

	// It is still delivered: bound to the message, and planned for the
	// agent recipient with an inbox path and no scratchpad copy.
	info, err := f.srv.attachmentService().MessageAttachments(context.Background(), []string{resp.ID})
	require.NoError(t, err)
	require.Len(t, info[resp.ID], 1, "the attachment is bound to the DM message")
	assert.Equal(t, inDM, info[resp.ID][0].ID)
	dels := attachmentDeliveries(resp.ID, []AttachmentRef{newArtifactAttachmentRef(info[resp.ID][0])})
	require.Len(t, dels, 1)
	plan := f.srv.attachmentDelivery().Plan(context.Background(), f.aa, dels)
	require.Len(t, plan, 1)
	assert.NotEqual(t, messages.AttachmentWithheld, plan[0].State, "the recipient reads the attachment")
	assert.Equal(t, attachmentInboxPath(resp.ID, "dm.txt"), plan[0].InboxPath)
	assert.Empty(t, plan[0].ProjectedPath)

	f.sendWith(t, f.ua, f.topicA, inTopic)
	got, err := os.ReadFile(filepath.Join(stagingDir, inTopic, "topic.txt"))
	require.NoError(t, err, "a project topic thread still gets the copy")
	assert.Equal(t, "topic bytes", string(got))
}

// TestChatKeyReadAudience: the classification the send authorization, the
// conversation access check and the projection decision share. A native
// project group conversation is a topic, read by project membership; a DM
// is read by its participants; a key that names no topic (a
// participant-list group conversation is not a chat v2 key) is none.
func TestChatKeyReadAudience(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	group := seedGroupConversation(t, f.st, f.projA.ID, "classify")

	aud, topic := chatKeyReadAudience(ctx, f.wcs, f.topicA)
	assert.Equal(t, chatAudienceProject, aud)
	require.NotNil(t, topic)
	assert.Equal(t, f.projA.ID, topic.ProjectID)

	aud, topic = chatKeyReadAudience(ctx, f.wcs, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))
	assert.Equal(t, chatAudienceParticipants, aud)
	assert.Nil(t, topic)

	for _, key := range []string{group, "group:" + f.projA.ID + ":classify", uuid.NewString(), ""} {
		aud, topic = chatKeyReadAudience(ctx, f.wcs, key)
		assert.Equal(t, chatAudienceNone, aud, key)
		assert.Nil(t, topic, key)
	}
	aud, _ = chatKeyReadAudience(ctx, nil, f.topicA)
	assert.Equal(t, chatAudienceNone, aud, "no chat store")
}

// TestProjectionAudienceIsProject: only a project topic thread of the
// message's own project counts as a whole-project audience.
func TestProjectionAudienceIsProject(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	group := seedGroupConversation(t, f.st, f.projA.ID, "audience")
	for _, c := range []struct {
		name, key, project string
		want               bool
	}{
		{"own topic", f.topicA, f.projA.ID, true},
		{"topic of another project", f.topicB, f.projA.ID, false},
		{"user-agent dm", dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID), f.projA.ID, false},
		{"user-user dm", dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID), f.projA.ID, false},
		{"group conversation id", group, f.projA.ID, false},
		{"group conversation ref", "group:" + f.projA.ID + ":audience", f.projA.ID, false},
		{"unknown key", uuid.NewString(), f.projA.ID, false},
		{"empty key", "", f.projA.ID, false},
		{"no project", f.topicA, "", false},
	} {
		assert.Equal(t, c.want, f.srv.projectionAudienceIsProject(ctx, c.key, c.project), c.name)
	}
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

// A user access token's history of a conversation lists the attachments
// stored as artifacts it may read under the token rule (ptone/scion#4596,
// section 9), as the session's history does.
func TestAttachmentArtifacts_TokenHistoryListsReadableArtifactAttachments(t *testing.T) {
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
	require.Len(t, hist.MessageAttachments[resp.ID], 1, "the token's history lists the attachment it may read")
}

// streamPublishedAs opens the agent messages stream of agentID as identity,
// calls publish until text arrives on the stream, and returns the stream
// body.
func streamPublishedAs(t *testing.T, srv *Server, events *ChannelEventPublisher, identity UserIdentity, agentID string, publish func(), text string) string {
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
	for time.Now().Before(deadline) && !strings.Contains(rec.String(), text) {
		publish()
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	body := rec.String()
	require.Contains(t, body, text, "stream never delivered the event")
	return body
}

// Attachments stored as artifacts are published only on the chat subjects
// (which reach sessions only): agent.<id>.message, user.<id>.message and
// project.<id>.user.message carry the AttachmentStore's attachments only, so
// the hub's agent messages stream, which a user access token may open, never
// lists an attachment stored as an artifact.
func TestAttachmentArtifacts_AgentStreamOmitsArtifactAttachmentsForTokens(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	f.srv.events = events
	ctx := context.Background()
	legacy := f.attach(t, f.projA.ID, f.ua.ID, "legacy.txt")
	artifact := f.uploadAs(t, f.ua, f.projA.ID, "artifact-file.txt", "stored as an artifact")
	refs := []AttachmentRef{{ID: legacy, Name: "legacy.txt"}, {ID: artifact, Name: "artifact-file.txt", artifact: true}}
	msg := &store.Message{
		ID: uuid.NewString(), ProjectID: f.projA.ID, Sender: "user:alice", SenderID: f.ua.ID,
		Recipient: "user:carol", RecipientID: f.uc.ID, AgentID: f.aa.ID, Msg: "message-with-files",
		Channel: "web", ThreadID: f.topicA, CreatedAt: time.Now().UTC(),
	}

	// Every subject the publish reaches, once.
	ch, unsubscribe := events.Subscribe("agent."+f.aa.ID+".message", "user."+f.uc.ID+".message",
		"project."+f.projA.ID+".user.message", "project."+f.projA.ID+".chat.message")
	events.PublishUserMessage(ctx, msg, refs, nil)
	seen := map[string]string{}
	for len(seen) < 4 {
		select {
		case evt := <-ch:
			seen[evt.Subject] = string(evt.Data)
		case <-time.After(5 * time.Second):
			t.Fatalf("published subjects: %d of 4", len(seen))
		}
	}
	unsubscribe()
	for _, subject := range []string{"agent." + f.aa.ID + ".message", "user." + f.uc.ID + ".message", "project." + f.projA.ID + ".user.message"} {
		require.Contains(t, seen[subject], legacy, subject)
		require.NotContains(t, seen[subject], artifact, "%s carries no attachment stored as an artifact", subject)
	}
	chat := seen["project."+f.projA.ID+".chat.message"]
	require.Contains(t, chat, legacy)
	require.Contains(t, chat, artifact, "the chat subject carries the attachment stored as an artifact")

	// End to end: the token's agent messages stream lists the legacy file only.
	token := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "agent:read")
	body := streamPublishedAs(t, f.srv, events, token, f.aa.ID, func() { events.PublishUserMessage(ctx, msg, refs, nil) }, msg.Msg)
	assert.Contains(t, body, legacy, "the AttachmentStore attachment is listed to the token")
	require.NotContains(t, body, artifact, "the attachment stored as an artifact is not listed to the token")
	require.NotContains(t, body, "artifact-file.txt")
}

// With the experiment on, uploads go to the artifact service for a session
// and a user access token (ptone/scion#4596, section 9). A federated user
// uploads to the AttachmentStore as with the experiment off, instead of
// storing a file it could not read back (or failing).
func TestAttachmentArtifacts_NonSessionUploadsUseAttachmentStore(t *testing.T) {
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

func (failBindStore) BindAttachment(context.Context, string, string, string, string, string, time.Time) error {
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

// TestArtifactAttachmentRef_HistoryNeverFlags: the history merge builds
// plain references (design section 3); history applies the read decision
// itself, and its refs never reach the chat subjects or the bind step.
func TestArtifactAttachmentRef_HistoryNeverFlags(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	id := f.uploadAs(t, f.ua, f.projA.ID, "notes.txt", "history")
	resp := f.sendWith(t, f.ua, f.topicA, id)
	merged := f.srv.mergeArtifactAttachments(f.sessionContext(f.ua), []string{resp.ID}, nil)
	require.Len(t, merged[resp.ID], 1)
	require.Equal(t, id, merged[resp.ID][0].ID)
	require.False(t, merged[resp.ID][0].artifact, "history references are never flagged")
}

// TestDeliverToUser_RepublishCarriesNoArtifactAttachments pins the attachment
// handling of deliverToUser's re-publish path (design section 4,
// ptone/scion#5504): a bus message whose transport metadata names a legacy
// attachment of the sender and an artifact attachment id links the legacy
// one, leaves the artifact row in its upload scope, carries exactly the
// linked file on the SSE event and the member fan-out (never the artifact id,
// never a flagged reference), and leaves the shared message unchanged.
func TestDeliverToUser_RepublishCarriesNoArtifactAttachments(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	legacy := f.attach(t, f.projA.ID, f.aa.ID, "legacy.txt")
	artifactID := f.uploadAs(t, f.ua, f.projA.ID, "stored.txt", "stored as an artifact")
	encoded, ok := attachmentRefsMetadata([]AttachmentRef{
		{ID: legacy, Name: "legacy.txt"},
		{ID: artifactID, Name: "stored.txt"},
	})
	require.True(t, ok)

	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	ch, unsubscribe := events.Subscribe("user." + f.ub.ID + ".message")
	t.Cleanup(unsubscribe)
	proxy := NewMessageBrokerProxy(nil, f.st, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	proxy.webChatStore = f.wcs
	// A message log, so the record branch runs on this path too.
	logger, logBuf := newCapturingMessageLogger()
	proxy.messageLog = logger
	var fanned [][]AttachmentRef
	proxy.memberFanout = func(_ context.Context, _ *store.Message, refs []AttachmentRef, _ []artifacts.MessageRef) {
		fanned = append(fanned, refs)
	}

	msg := messages.NewInstruction("agent:aa", "user:bob", "here are the files")
	msg.SenderID = f.aa.ID
	msg.RecipientID = f.ub.ID
	msg.Metadata = map[string]string{attachmentsMetadataKey: encoded}
	before := *msg
	before.Metadata = maps.Clone(msg.Metadata)
	proxy.deliverToUser(ctx, f.projA.ID, "project."+f.projA.ID+".user.message", msg)

	result, err := f.st.ListMessages(ctx, store.MessageFilter{RecipientID: f.ub.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	// The message has a conversation, and the record names it, so a record
	// that set it on the shared message instead of a copy shows in the
	// unchanged-message check below.
	require.NotEmpty(t, result.Items[0].ConversationID, "the re-published message has a conversation")
	assertLogHasConversationID(t, logBuf, "user message delivered via broker", result.Items[0].ConversationID)
	linked, err := f.wcs.GetAttachmentsByMessage(ctx, result.Items[0].ID)
	require.NoError(t, err)
	require.Len(t, linked, 1)
	require.Equal(t, legacy, linked[0].ID, "the sender's legacy attachment is linked")

	a, err := st.GetAttachment(ctx, artifactID)
	require.NoError(t, err)
	require.Equal(t, artifacts.ScopeKindUpload, a.ScopeKind, "this path never binds an artifact attachment")

	select {
	case evt := <-ch:
		require.NotContains(t, string(evt.Data), artifactID, "the SSE event names no artifact attachment")
		var payload UserMessageEvent
		require.NoError(t, json.Unmarshal(evt.Data, &payload))
		require.Len(t, payload.Attachments, 1, "the SSE event carries the linked file (ptone/scion#5504)")
		require.Equal(t, legacy, payload.Attachments[0].ID)
	case <-time.After(5 * time.Second):
		t.Fatal("no SSE event")
	}
	require.Len(t, fanned, 1, "one member fan-out")
	require.Len(t, fanned[0], 1, "the member fan-out carries the linked file only")
	require.Equal(t, legacy, fanned[0][0].ID)
	require.False(t, fanned[0][0].artifact)
	require.Equal(t, before, *msg, "the shared message, its metadata included, is unchanged")
}
