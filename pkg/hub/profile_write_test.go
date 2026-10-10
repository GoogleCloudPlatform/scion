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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// profileWriteFixture is a server wired for every profile write route: a
// member user (alice), a hub-boundary user access token for alice, and a
// federated user identity.
type profileWriteFixture struct {
	srv      *Server
	store    store.Store
	alice    *store.User
	session  string
	token    string
	fed      *FederatedUserIdentity
	secrets  *secret.LocalBackend
	webChat  WebChatStore
	chatDB   *sql.DB
	presence *PresenceManager
}

func newProfileWriteFixture(t *testing.T) *profileWriteFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	alice := &store.User{
		ID:          tid("pw-alice"),
		Email:       "alice@profile-write.test",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)

	session, _, _, err := srv.userTokenService.GenerateTokenPair(alice.ID, alice.Email, alice.DisplayName, alice.Role, ClientTypeWeb)
	require.NoError(t, err)

	srv.SetStorage(newMockStorage("test-bucket"))
	backend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(backend)
	chatDB := openTestMemorySQLite(t, "sqlite3")
	wcs := NewWebChatStore(chatDB, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	as, err := NewLocalDiskAttachmentStore(t.TempDir())
	require.NoError(t, err)
	srv.SetAttachmentStore(as)
	srv.InitPresenceManager()
	t.Cleanup(srv.presenceManager.Stop)

	return &profileWriteFixture{
		srv:      srv,
		store:    s,
		alice:    alice,
		session:  session,
		token:    mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "inbox:read"),
		fed:      NewFederatedUserIdentity("https://issuer.pw.test", "pw-fed", "pw-fed@profile-write.test", "Fed", "member", nil),
		secrets:  backend,
		webChat:  wcs,
		chatDB:   chatDB,
		presence: srv.presenceManager,
	}
}

// profileRequest is one request to a profile write route.
type profileRequest struct {
	method      string
	path        string
	contentType string
	body        []byte
}

func jsonProfileRequest(t *testing.T, method, path string, body interface{}) profileRequest {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	return profileRequest{method: method, path: path, contentType: "application/json", body: raw}
}

// attachmentProfileRequest is a project-less chat attachment upload of one
// file.
func attachmentProfileRequest(t *testing.T) profileRequest {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="files"; filename="notes.txt"`},
		"Content-Type":        {"text/plain"},
	})
	require.NoError(t, err)
	_, err = part.Write([]byte("profile write"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return profileRequest{method: http.MethodPost, path: "/api/v1/chat/attachments", contentType: mw.FormDataContentType(), body: body.Bytes()}
}

func (pr profileRequest) build() *http.Request {
	req := httptest.NewRequest(pr.method, pr.path, bytes.NewReader(pr.body))
	if pr.contentType != "" {
		req.Header.Set("Content-Type", pr.contentType)
	}
	return req
}

// asBearer runs pr through the full middleware chain with a bearer
// credential (a session access token or a user access token).
func (f *profileWriteFixture) asBearer(pr profileRequest, key string) *httptest.ResponseRecorder {
	req := pr.build()
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// asIdentity runs pr through the route table with identity and its
// credential set on the request context, as the authentication middleware
// sets them.
func (f *profileWriteFixture) asIdentity(pr profileRequest, identity Identity) *httptest.ResponseRecorder {
	req := pr.build()
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// requireProfileWriteRefused asserts the uniform profile write refusal.
func requireProfileWriteRefused(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", what, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeForbidden, resp.Error.Code, what)
	assert.Equal(t, "user", resp.Error.Details["resource_type"], what)
	assert.Equal(t, "update", resp.Error.Details["denied_action"], what)
}

func TestIsFederatedCaller(t *testing.T) {
	member := NewAuthenticatedUser("u-1", "u@test.com", "U", "member", "web")
	fed := NewFederatedUserIdentity("https://issuer.pw.test", "sub", "f@test.com", "F", "member", nil)
	cases := []struct {
		name       string
		identity   Identity
		credential CredentialKind
		want       bool
	}{
		{name: "no identity", want: false},
		{name: "session user", identity: member, want: false},
		{name: "dev user", identity: NewDevUser(DevUserConfig{Username: "dev", Email: "dev@localhost"}), want: false},
		{name: "user access token", identity: NewScopedUserIdentity(member, "", []string{"hub_config:read"}), want: false},
		{name: "federated user", identity: fed, want: true},
		{name: "user access token wrapping a federated user", identity: NewScopedUserIdentity(fed, "", nil), want: true},
		{name: "federation credential", identity: member, credential: CredentialKindFederation, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.identity != nil {
				ctx = contextWithIdentity(ctx, tc.identity)
			}
			if tc.credential != "" {
				ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: tc.credential})
			}
			assert.Equal(t, tc.want, isFederatedCaller(ctx))
		})
	}
}

// userTemplateWrites returns each user template write route for template id.
func userTemplateWrites(t *testing.T, id string) []profileRequest {
	base := "/api/v1/users/me/templates"
	return []profileRequest{
		jsonProfileRequest(t, http.MethodPost, base, CreateTemplateRequest{Name: "created-template"}),
		jsonProfileRequest(t, http.MethodPut, base+"/"+id, map[string]string{"name": "renamed-template", "description": "renamed"}),
		jsonProfileRequest(t, http.MethodPost, base+"/"+id+"/upload", UploadRequest{Files: []FileUploadRequest{{Path: "scion-agent.yaml", Size: 10}}}),
		jsonProfileRequest(t, http.MethodPost, base+"/"+id+"/finalize", FinalizeRequest{}),
		jsonProfileRequest(t, http.MethodDelete, base+"/"+id, nil),
	}
}

// TestUserTemplateWrites_FederatedUserRefused pins that a federated user
// gets 403 on every user template write, including on a template it owns,
// that its templates are unchanged, and that it still lists them.
func TestUserTemplateWrites_FederatedUserRefused(t *testing.T) {
	f := newProfileWriteFixture(t)
	owned := createUserTemplate(t, f.store, f.fed.ID(), "fed-template")

	for _, pr := range userTemplateWrites(t, owned.ID) {
		requireProfileWriteRefused(t, f.asIdentity(pr, f.fed), pr.method+" "+pr.path)
	}

	result, err := f.store.ListTemplates(context.Background(), store.TemplateFilter{Scope: store.TemplateScopeUser, ScopeID: f.fed.ID()}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, result.Items, 1, "a refused write creates and deletes nothing")
	assert.Equal(t, owned.Name, result.Items[0].Name)
	assert.Equal(t, owned.Description, result.Items[0].Description)
	assert.Equal(t, owned.Status, result.Items[0].Status)

	rec := f.asIdentity(jsonProfileRequest(t, http.MethodGet, "/api/v1/users/me/templates", nil), f.fed)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestUserTemplateWrites_SessionAndTokenUnchanged pins that a session and a
// user access token keep their behaviour on the user template writes.
func TestUserTemplateWrites_SessionAndTokenUnchanged(t *testing.T) {
	f := newProfileWriteFixture(t)

	sessionTmpl := createUserTemplate(t, f.store, f.alice.ID, "session-template")
	sessionTmpl.StoragePath = "templates/user/session-template"
	require.NoError(t, f.store.UpdateTemplate(context.Background(), sessionTmpl))
	want := []int{http.StatusCreated, http.StatusOK, http.StatusOK, http.StatusBadRequest, http.StatusNoContent}
	for i, pr := range userTemplateWrites(t, sessionTmpl.ID) {
		rec := f.asBearer(pr, f.session)
		assert.Equal(t, want[i], rec.Code, "session %s %s: %s", pr.method, pr.path, rec.Body.String())
	}

	tokenTmpl := createUserTemplate(t, f.store, f.alice.ID, "token-template")
	writes := userTemplateWrites(t, tokenTmpl.ID)
	writes[0] = jsonProfileRequest(t, http.MethodPost, "/api/v1/users/me/templates", CreateTemplateRequest{Name: "token-created"})
	want = []int{http.StatusCreated, http.StatusOK, http.StatusForbidden, http.StatusForbidden, http.StatusNoContent}
	for i, pr := range writes {
		rec := f.asBearer(pr, f.token)
		assert.Equal(t, want[i], rec.Code, "token %s %s: %s", pr.method, pr.path, rec.Body.String())
	}
}

// userEnvSecretWrites returns each user-scope env var and secret write
// route, for keys that are already set.
func userEnvSecretWrites(t *testing.T, envKey, secretKey string) []profileRequest {
	description := "patched"
	return []profileRequest{
		jsonProfileRequest(t, http.MethodPut, "/api/v1/env/NEW_PW_ENV?scope=user", SetEnvVarRequest{Value: "new"}),
		jsonProfileRequest(t, http.MethodPut, "/api/v1/env/"+envKey+"?scope=user", SetEnvVarRequest{Value: "replaced"}),
		jsonProfileRequest(t, http.MethodPut, "/api/v1/secrets/"+secretKey, SetSecretRequest{Value: "cmVwbGFjZWQ="}),
		jsonProfileRequest(t, http.MethodPatch, "/api/v1/secrets/"+secretKey, PatchSecretRequest{Description: &description}),
		jsonProfileRequest(t, http.MethodDelete, "/api/v1/secrets/"+secretKey, nil),
		jsonProfileRequest(t, http.MethodDelete, "/api/v1/env/"+envKey+"?scope=user", nil),
	}
}

func seedUserEnvAndSecret(t *testing.T, f *profileWriteFixture, userID, envKey, secretKey string) {
	t.Helper()
	ctx := context.Background()
	_, err := f.store.UpsertEnvVar(ctx, &store.EnvVar{ID: tid("pw-env"), Key: envKey, Value: "seeded", Scope: store.ScopeUser, ScopeID: userID})
	require.NoError(t, err)
	_, _, err = f.secrets.Set(ctx, &secret.SetSecretInput{Name: secretKey, Value: "seeded", SecretType: store.SecretTypeEnvironment, Target: secretKey, Scope: store.ScopeUser, ScopeID: userID, Description: "seeded"})
	require.NoError(t, err)
}

// TestUserEnvSecretWrites_FederatedUserRefused pins that a federated user
// gets 403 on every user-scope env var and secret write and that its
// values are unchanged.
func TestUserEnvSecretWrites_FederatedUserRefused(t *testing.T) {
	f := newProfileWriteFixture(t)
	ctx := context.Background()
	seedUserEnvAndSecret(t, f, f.fed.ID(), "PW_ENV", "PW_SECRET")
	metaBefore, err := f.secrets.GetMeta(ctx, "PW_SECRET", store.ScopeUser, f.fed.ID())
	require.NoError(t, err)

	for _, pr := range userEnvSecretWrites(t, "PW_ENV", "PW_SECRET") {
		requireProfileWriteRefused(t, f.asIdentity(pr, f.fed), pr.method+" "+pr.path)
	}

	env, err := f.store.GetEnvVar(ctx, "PW_ENV", store.ScopeUser, f.fed.ID())
	require.NoError(t, err)
	assert.Equal(t, "seeded", env.Value)
	_, err = f.store.GetEnvVar(ctx, "NEW_PW_ENV", store.ScopeUser, f.fed.ID())
	assert.ErrorIs(t, err, store.ErrNotFound)
	metaAfter, err := f.secrets.GetMeta(ctx, "PW_SECRET", store.ScopeUser, f.fed.ID())
	require.NoError(t, err)
	assert.Equal(t, metaBefore, metaAfter)
	value, err := f.secrets.Get(ctx, "PW_SECRET", store.ScopeUser, f.fed.ID())
	require.NoError(t, err)
	assert.Equal(t, "seeded", value.Value)
}

// TestUserEnvSecretWrites_SessionAndTokenUnchanged pins that a session and
// a user access token keep their behaviour on the user-scope env var and
// secret writes.
func TestUserEnvSecretWrites_SessionAndTokenUnchanged(t *testing.T) {
	want := []int{http.StatusOK, http.StatusOK, http.StatusOK, http.StatusOK, http.StatusNoContent, http.StatusNoContent}
	for _, caller := range []string{"session", "token"} {
		t.Run(caller, func(t *testing.T) {
			f := newProfileWriteFixture(t)
			key := f.session
			if caller == "token" {
				key = f.token
			}
			seedUserEnvAndSecret(t, f, f.alice.ID, "PW_ENV", "PW_SECRET")
			for i, pr := range userEnvSecretWrites(t, "PW_ENV", "PW_SECRET") {
				rec := f.asBearer(pr, key)
				assert.Equal(t, want[i], rec.Code, "%s %s: %s", pr.method, pr.path, rec.Body.String())
			}
		})
	}
}

// chatProfileWrites returns the chat preference, presence and project-less
// attachment writes.
func chatProfileWrites(t *testing.T) []profileRequest {
	return []profileRequest{
		jsonProfileRequest(t, http.MethodPut, "/api/v1/chat/user-prefs", map[string]string{"spaceSortMode": "alpha"}),
		jsonProfileRequest(t, http.MethodPost, "/api/v1/chat/presence", map[string][]string{"projectIds": {}}),
		attachmentProfileRequest(t),
	}
}

// TestChatProfileWrites_FederatedUserRefused pins that a federated user
// gets 403 on the chat preference, presence and project-less attachment
// writes, records nothing, and still reads its preferences.
func TestChatProfileWrites_FederatedUserRefused(t *testing.T) {
	f := newProfileWriteFixture(t)
	ctx := context.Background()

	for _, pr := range chatProfileWrites(t) {
		requireProfileWriteRefused(t, f.asIdentity(pr, f.fed), pr.method+" "+pr.path)
	}

	prefs, err := f.webChat.GetUserPrefs(ctx, f.fed.ID())
	require.NoError(t, err)
	if prefs != nil {
		assert.NotEqual(t, "alpha", prefs.SpaceSortMode, "no preferences are saved")
	}
	_, tracked := f.presence.GetAllStates()[f.fed.ID()]
	assert.False(t, tracked, "no presence is recorded")
	var count int
	require.NoError(t, f.chatDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM webchat_attachment WHERE uploaded_by = ?`, f.fed.ID()).Scan(&count))
	assert.Zero(t, count, "no attachment is stored")

	rec := f.asIdentity(jsonProfileRequest(t, http.MethodGet, "/api/v1/chat/user-prefs", nil), f.fed)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestChatProfileWrites_SessionAndTokenUnchanged pins that a session and a
// user access token keep their behaviour on the chat preference, presence
// and project-less attachment writes.
func TestChatProfileWrites_SessionAndTokenUnchanged(t *testing.T) {
	f := newProfileWriteFixture(t)
	want := []int{http.StatusOK, http.StatusOK, http.StatusCreated}
	for _, caller := range []struct{ name, key string }{{"session", f.session}, {"token", f.token}} {
		for i, pr := range chatProfileWrites(t) {
			rec := f.asBearer(pr, caller.key)
			assert.Equal(t, want[i], rec.Code, "%s %s %s: %s", caller.name, pr.method, pr.path, rec.Body.String())
		}
	}
}

// TestChatLinkVerification_FederatedUserRefused pins that a federated user
// gets 403 on each chat link verify route and the code stays pending, while
// a session and a user access token still link the account.
func TestChatLinkVerification_FederatedUserRefused(t *testing.T) {
	srv := &Server{
		telegramLinkService: NewTelegramLinkService(),
		discordLinkService:  NewDiscordLinkService(),
		teamsLinkService:    NewTeamsLinkService(),
	}
	t.Cleanup(srv.telegramLinkService.Close)
	t.Cleanup(srv.discordLinkService.Close)
	t.Cleanup(srv.teamsLinkService.Close)

	member := NewAuthenticatedUser("user-1", "user@example.com", "User", "member", "web")
	fed := NewFederatedUserIdentity("https://issuer.pw.test", "link-fed", "fed@example.com", "Fed", "member", nil)
	providers := []struct {
		name    string
		service *chatLinkService
		handler http.HandlerFunc
	}{
		{"telegram", srv.telegramLinkService.chatLinkService, srv.handleTelegramLinkVerify},
		{"discord", srv.discordLinkService.chatLinkService, srv.handleDiscordLinkVerify},
		{"teams", srv.teamsLinkService.chatLinkService, srv.handleTeamsLinkVerify},
	}
	verify := func(handler http.HandlerFunc, identity Identity, code string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"code":"`+code+`"}`))
		ctx := contextWithIdentity(req.Context(), identity)
		ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
		rec := httptest.NewRecorder()
		handler(rec, req.WithContext(ctx))
		return rec
	}

	for _, p := range providers {
		t.Run(p.name, func(t *testing.T) {
			p.service.RegisterCode("FED123", p.name+"-fed")
			requireProfileWriteRefused(t, verify(p.handler, fed, "FED123"), p.name)
			status, userID, _ := p.service.GetStatusByUser(p.name + "-fed")
			assert.Equal(t, "pending", status)
			assert.Empty(t, userID)

			p.service.RegisterCode("SES123", p.name+"-session")
			assert.Equal(t, http.StatusOK, verify(p.handler, member, "SES123").Code)

			p.service.RegisterCode("TOK123", p.name+"-token")
			token := NewScopedUserIdentity(member, "", []string{"hub_config:read"})
			assert.Equal(t, http.StatusOK, verify(p.handler, token, "TOK123").Code)
		})
	}
}

// genericUserTemplateWrites returns each /api/v1/templates write on a
// user-scope template id, and the user-scope create.
func genericUserTemplateWrites(t *testing.T, id string) []profileRequest {
	base := "/api/v1/templates"
	return []profileRequest{
		jsonProfileRequest(t, http.MethodPost, base, CreateTemplateRequest{Name: "generic-created", Scope: store.TemplateScopeUser}),
		jsonProfileRequest(t, http.MethodPut, base+"/"+id, map[string]string{"name": "generic-renamed"}),
		jsonProfileRequest(t, http.MethodPatch, base+"/"+id, map[string]string{"description": "patched"}),
		jsonProfileRequest(t, http.MethodPost, base+"/"+id+"/upload", UploadRequest{Files: []FileUploadRequest{{Path: "scion-agent.yaml", Size: 10}}}),
		jsonProfileRequest(t, http.MethodPost, base+"/"+id+"/finalize", FinalizeRequest{}),
		jsonProfileRequest(t, http.MethodPut, base+"/"+id+"/files/notes.txt", map[string]string{"content": "x"}),
		jsonProfileRequest(t, http.MethodDelete, base+"/"+id+"/files/notes.txt", nil),
		jsonProfileRequest(t, http.MethodDelete, base+"/"+id, nil),
	}
}

// TestGenericUserTemplateWrites_FederatedUserRefused pins that the
// /api/v1/templates routes refuse a federated user on a user-scope
// template, as /api/v1/users/me/templates does, and change nothing.
func TestGenericUserTemplateWrites_FederatedUserRefused(t *testing.T) {
	f := newProfileWriteFixture(t)
	owned := createUserTemplate(t, f.store, f.fed.ID(), "fed-generic")

	for _, pr := range genericUserTemplateWrites(t, owned.ID) {
		requireProfileWriteRefused(t, f.asIdentity(pr, f.fed), pr.method+" "+pr.path)
	}

	result, err := f.store.ListTemplates(context.Background(), store.TemplateFilter{Scope: store.TemplateScopeUser, ScopeID: f.fed.ID()}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, result.Items, 1, "a refused write creates and deletes nothing")
	assert.Equal(t, owned.Name, result.Items[0].Name)
	assert.Equal(t, owned.Description, result.Items[0].Description)
	assert.Equal(t, owned.Status, result.Items[0].Status)
}

// TestGenericUserTemplateWrites_SessionAndTokenUnchanged pins that a
// session and a user access token keep their behaviour on the
// /api/v1/templates user-scope create, patch and delete.
func TestGenericUserTemplateWrites_SessionAndTokenUnchanged(t *testing.T) {
	f := newProfileWriteFixture(t)
	writes := func(id, name string) []profileRequest {
		return []profileRequest{
			jsonProfileRequest(t, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{Name: name, Scope: store.TemplateScopeUser}),
			jsonProfileRequest(t, http.MethodPatch, "/api/v1/templates/"+id, map[string]string{"description": "patched"}),
			jsonProfileRequest(t, http.MethodDelete, "/api/v1/templates/"+id, nil),
		}
	}
	cases := []struct {
		name string
		key  string
		want []int
	}{
		{"session", f.session, []int{http.StatusCreated, http.StatusOK, http.StatusNoContent}},
		{"token", f.token, []int{http.StatusForbidden, http.StatusForbidden, http.StatusNoContent}},
	}
	for _, tc := range cases {
		tmpl := createUserTemplate(t, f.store, f.alice.ID, tc.name+"-generic")
		for i, pr := range writes(tmpl.ID, tc.name+"-generic-created") {
			rec := f.asBearer(pr, tc.key)
			assert.Equal(t, tc.want[i], rec.Code, "%s %s %s: %s", tc.name, pr.method, pr.path, rec.Body.String())
		}
	}
}

// TestUserSkillWrites_FederatedUserRefused pins that a federated user gets
// 403 on every write to a user-scope skill, including the user-scope
// create, and that its skill is unchanged.
func TestUserSkillWrites_FederatedUserRefused(t *testing.T) {
	f := newProfileWriteFixture(t)
	ctx := context.Background()
	owned := &store.Skill{ID: tid("pw-skill"), Name: "fed-skill", Slug: "fed-skill", Scope: store.SkillScopeUser, ScopeID: f.fed.ID(), OwnerID: f.fed.ID(), CreatedBy: f.fed.ID(), Status: "active"}
	require.NoError(t, f.store.CreateSkill(ctx, owned))

	base := "/api/v1/skills"
	writes := []profileRequest{
		jsonProfileRequest(t, http.MethodPost, base, CreateSkillRequest{Name: "fed-created", Scope: store.SkillScopeUser}),
		jsonProfileRequest(t, http.MethodPatch, base+"/"+owned.ID, map[string]string{"description": "patched"}),
		jsonProfileRequest(t, http.MethodPost, base+"/"+owned.ID+"/versions", map[string]string{"version": "1.0.0"}),
		jsonProfileRequest(t, http.MethodPost, base+"/"+owned.ID+"/versions/"+tid("pw-version")+"/deprecate", nil),
		jsonProfileRequest(t, http.MethodPost, base+"/"+owned.ID+"/upload", map[string]string{"version": "1.0.0"}),
		jsonProfileRequest(t, http.MethodPost, base+"/"+owned.ID+"/finalize", map[string]string{"version": "1.0.0"}),
		{method: http.MethodPut, path: base + "/" + owned.ID + "/files/SKILL.md?version=1.0.0", contentType: "text/markdown", body: []byte("# skill")},
		jsonProfileRequest(t, http.MethodDelete, base+"/"+owned.ID, nil),
	}
	for _, pr := range writes {
		requireProfileWriteRefused(t, f.asIdentity(pr, f.fed), pr.method+" "+pr.path)
	}

	got, err := f.store.GetSkill(ctx, owned.ID)
	require.NoError(t, err, "a refused delete leaves the skill")
	assert.Equal(t, owned.Description, got.Description)
	result, err := f.store.ListSkills(ctx, store.SkillFilter{Scope: store.SkillScopeUser, ScopeID: f.fed.ID()}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, result.Items, 1, "a refused create adds nothing")
}

// TestUserSkillWrites_SessionUnchanged pins that a session still creates,
// patches and deletes a user-scope skill.
func TestUserSkillWrites_SessionUnchanged(t *testing.T) {
	f := newProfileWriteFixture(t)
	rec := f.asBearer(jsonProfileRequest(t, http.MethodPost, "/api/v1/skills", CreateSkillRequest{Name: "session-skill", Scope: store.SkillScopeUser}), f.session)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created CreateSkillResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	rec = f.asBearer(jsonProfileRequest(t, http.MethodPatch, "/api/v1/skills/"+created.Skill.ID, map[string]string{"description": "patched"}), f.session)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.asBearer(jsonProfileRequest(t, http.MethodDelete, "/api/v1/skills/"+created.Skill.ID, nil), f.session)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestUserHarnessConfigWrites_FederatedUserRefused pins that a federated
// user gets 403 on every write to a user-scope harness config, including
// the user-scope create, and that its config is unchanged.
func TestUserHarnessConfigWrites_FederatedUserRefused(t *testing.T) {
	f := newProfileWriteFixture(t)
	ctx := context.Background()
	owned := &store.HarnessConfig{ID: tid("pw-hc"), Name: "fed-hc", Slug: "fed-hc", Harness: "claude", Scope: store.HarnessConfigScopeUser, ScopeID: f.fed.ID(), OwnerID: f.fed.ID(), CreatedBy: f.fed.ID(), Status: store.HarnessConfigStatusActive}
	require.NoError(t, f.store.CreateHarnessConfig(ctx, owned))

	base := "/api/v1/harness-configs"
	writes := []profileRequest{
		jsonProfileRequest(t, http.MethodPost, base, CreateHarnessConfigRequest{Name: "fed-created", Harness: "claude", Scope: store.HarnessConfigScopeUser}),
		jsonProfileRequest(t, http.MethodPut, base+"/"+owned.ID, map[string]string{"name": "fed-renamed", "harness": "claude"}),
		jsonProfileRequest(t, http.MethodPatch, base+"/"+owned.ID, map[string]string{"description": "patched"}),
		jsonProfileRequest(t, http.MethodPost, base+"/"+owned.ID+"/upload", UploadRequest{Files: []FileUploadRequest{{Path: "config.yaml", Size: 10}}}),
		jsonProfileRequest(t, http.MethodPost, base+"/"+owned.ID+"/finalize", FinalizeRequest{}),
		jsonProfileRequest(t, http.MethodPut, base+"/"+owned.ID+"/files/config.yaml", map[string]string{"content": "x"}),
		jsonProfileRequest(t, http.MethodDelete, base+"/"+owned.ID, nil),
	}
	for _, pr := range writes {
		requireProfileWriteRefused(t, f.asIdentity(pr, f.fed), pr.method+" "+pr.path)
	}

	got, err := f.store.GetHarnessConfig(ctx, owned.ID)
	require.NoError(t, err, "a refused delete leaves the config")
	assert.Equal(t, owned.Name, got.Name)
	assert.Equal(t, owned.Description, got.Description)
	result, err := f.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Scope: store.HarnessConfigScopeUser, ScopeID: f.fed.ID()}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, result.Items, 1, "a refused create adds nothing")
}

// TestUserHarnessConfigWrites_SessionUnchanged pins that a session still
// patches and deletes its own user-scope harness config.
func TestUserHarnessConfigWrites_SessionUnchanged(t *testing.T) {
	f := newProfileWriteFixture(t)
	owned := &store.HarnessConfig{ID: tid("pw-hc-session"), Name: "session-hc", Slug: "session-hc", Harness: "claude", Scope: store.HarnessConfigScopeUser, ScopeID: f.alice.ID, OwnerID: f.alice.ID, CreatedBy: f.alice.ID, Status: store.HarnessConfigStatusActive}
	require.NoError(t, f.store.CreateHarnessConfig(context.Background(), owned))
	rec := f.asBearer(jsonProfileRequest(t, http.MethodPatch, "/api/v1/harness-configs/"+owned.ID, map[string]string{"description": "patched"}), f.session)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.asBearer(jsonProfileRequest(t, http.MethodDelete, "/api/v1/harness-configs/"+owned.ID, nil), f.session)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestUserResourceImport_FederatedUserRefused pins that a federated user
// gets 403 on a user-scope resource import, before any fetch.
func TestUserResourceImport_FederatedUserRefused(t *testing.T) {
	f := newProfileWriteFixture(t)
	for _, kind := range []string{"template", "harness-config"} {
		pr := jsonProfileRequest(t, http.MethodPost, "/api/v1/resources/import", map[string]string{"kind": kind, "sourceUrl": "https://example.invalid/repo", "scope": "user"})
		requireProfileWriteRefused(t, f.asIdentity(pr, f.fed), kind)
	}
	result, err := f.store.ListTemplates(context.Background(), store.TemplateFilter{Scope: store.TemplateScopeUser, ScopeID: f.fed.ID()}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, result.Items)
}
