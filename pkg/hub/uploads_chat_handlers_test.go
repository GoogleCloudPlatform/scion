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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chatUploadEnv struct {
	srv      *Server
	st       store.Store
	artStore artifacts.Store
	blobs    *storage.LocalStorage
	user     *store.User
	token    string
}

func newChatUploadEnv(t *testing.T, largeUploads, attachmentArtifacts bool) *chatUploadEnv {
	t.Helper()
	srv, st := testServer(t)

	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "artifacts.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	artStore := artifacts.NewStore(db, "sqlite")
	require.NoError(t, artStore.Init(context.Background()))
	srv.SetArtifactStore(artStore)

	blobs, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "hub-test",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	srv.SetStorage(blobs)

	setChatUploadExperiments(t, srv, largeUploads, attachmentArtifacts)

	user := &store.User{
		ID:          uuid.NewString(),
		Email:       "chat-upl-1@example.com",
		DisplayName: "Chat Uploader 1",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(context.Background(), user))
	token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)

	return &chatUploadEnv{
		srv:      srv,
		st:       st,
		artStore: artStore,
		blobs:    blobs,
		user:     user,
		token:    token,
	}
}

func setChatUploadExperiments(t *testing.T, srv *Server, largeUploads, attachmentArtifacts bool) {
	t.Helper()
	cur := srv.experimentRegistry()
	var active []experiments.Experiment
	for _, e := range cur.All() {
		switch e.Name {
		case experiments.LargeUploads:
			e.Default = largeUploads
		case experiments.AttachmentArtifacts:
			e.Default = attachmentArtifacts
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, cur.Retired())
	require.NoError(t, err)
	srv.setExperimentRegistry(reg)
	if !attachmentArtifacts {
		ops := srv.GetOperationalSettings()
		if ops == nil {
			ops = NewOperationalSettings(srv.store, koanf.New("."), koanf.New("."))
			_, err := ops.Refresh(context.Background())
			require.NoError(t, err)
			srv.SetOperationalSettings(ops)
		}
		raw, err := json.Marshal(map[string]any{
			"overrides": map[string]bool{
				experiments.AttachmentArtifacts: false,
			},
		})
		require.NoError(t, err)
		_, err = srv.store.UpsertHubSetting(context.Background(), "experiments", raw, "test", -1, "managed")
		require.NoError(t, err)
		_, err = ops.Refresh(context.Background())
		require.NoError(t, err)
	}
	require.Equal(t, largeUploads, srv.experimentEnabled(experiments.LargeUploads))
	require.Equal(t, attachmentArtifacts, srv.experimentEnabled(experiments.AttachmentArtifacts))
}

func chatUploadJSONReq(t *testing.T, srv *Server, bearer, method, urlPath string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, urlPath, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func chatUploadChunkReq(t *testing.T, srv *Server, bearer, urlPath string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, urlPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func chatUploadDecodeErr(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp.Error
}

type nonComposerLocalStorage struct {
	storage.Storage
}

func TestChatUploads_ExperimentDisabled(t *testing.T) {
	env := newChatUploadEnv(t, false, true)
	require.False(t, env.srv.experimentEnabled(experiments.LargeUploads))

	rec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "notes.txt", Size: 16})
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeExperimentDisabled, chatUploadDecodeErr(t, rec).Code)
}

func TestChatUploads_HappyPathSHA256AndPreAllocatedID(t *testing.T) {
	env := newChatUploadEnv(t, true, true)
	ctx := context.Background()

	payload := bytes.Repeat([]byte("hello chat large attachment!\n"), 150) // 4350 bytes
	wantSum := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(wantSum[:])

	// 1. Start
	startRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{
			Name:      "report.txt",
			Size:      int64(len(payload)),
			MediaType: "text/plain",
		})
	require.Equal(t, http.StatusCreated, startRec.Code, "body: %s", startRec.Body.String())
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	assert.True(t, strings.HasPrefix(started.ID, "upl_"))
	assert.Equal(t, "hub_chunked", started.Transport)
	assert.Equal(t, int64(0), started.Received)
	assert.Equal(t, env.srv.attachmentMaxBytes(ctx, true), started.MaxBytes)

	sess, err := env.st.GetUploadSession(ctx, started.ID)
	require.NoError(t, err)
	require.NotEmpty(t, sess.ArtifactID, "pre-allocated artifact_id must be set at start")
	preAllocID := sess.ArtifactID
	stagingPrefix := sess.StagingRef

	// 2. Chunk 0 (2000 bytes)
	chunkURL := "/api/v1/chat/attachments/uploads/" + started.ID
	c0 := chatUploadChunkReq(t, env.srv, env.token, chunkURL+"?offset=0", payload[:2000])
	require.Equal(t, http.StatusOK, c0.Code, "body: %s", c0.Body.String())
	var c0Resp UploadChunkResponse
	require.NoError(t, json.Unmarshal(c0.Body.Bytes(), &c0Resp))
	assert.Equal(t, int64(2000), c0Resp.Received)

	// GET status does not extend expiry.
	afterC0, err := env.st.GetUploadSession(ctx, started.ID)
	require.NoError(t, err)
	stRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodGet, chunkURL, nil)
	require.Equal(t, http.StatusOK, stRec.Code, "body: %s", stRec.Body.String())
	var stResp UploadStatusResponse
	require.NoError(t, json.Unmarshal(stRec.Body.Bytes(), &stResp))
	assert.Equal(t, store.UploadStateUploading, stResp.State)
	assert.Equal(t, int64(2000), stResp.Received)
	afterGet, err := env.st.GetUploadSession(ctx, started.ID)
	require.NoError(t, err)
	assert.True(t, afterC0.IdleExpiresAt.Equal(afterGet.IdleExpiresAt), "GET status must not extend idle_expires_at")

	// 3. Chunk 1 (remaining bytes)
	c1 := chatUploadChunkReq(t, env.srv, env.token, chunkURL+"?offset=2000", payload[2000:])
	require.Equal(t, http.StatusOK, c1.Code, "body: %s", c1.Body.String())

	// 4. Complete
	compRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusOK, compRec.Code, "body: %s", compRec.Body.String())
	var compResp attachmentUploadResult
	require.NoError(t, json.Unmarshal(compRec.Body.Bytes(), &compResp))
	assert.Equal(t, preAllocID, compResp.ID, "artifact must use pre-allocated artifact_id")
	assert.Equal(t, "report.txt", compResp.Name)
	assert.Equal(t, "text/plain", compResp.MimeType)
	assert.Equal(t, int64(len(payload)), compResp.Size)
	assert.Equal(t, "/api/v1/chat/attachments/"+preAllocID, compResp.URL)

	// Composed blob SHA-256 equals source.
	blobPath := artifacts.BlobPath(env.srv.artifactBackend().HubID, wantHex)
	rc, _, err := env.blobs.Download(ctx, blobPath)
	require.NoError(t, err)
	gotBlob, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	gotSum := sha256.Sum256(gotBlob)
	assert.Equal(t, wantHex, hex.EncodeToString(gotSum[:]))

	// Staging prefix was cleaned up best-effort.
	listed, err := env.blobs.List(ctx, storage.ListOptions{Prefix: storage.DirPrefix(stagingPrefix)})
	require.NoError(t, err)
	assert.Empty(t, listed.Objects, "staging objects should be deleted after finalize")

	// Download via GET /api/v1/chat/attachments/{id} returns the exact bytes.
	dlRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodGet, compResp.URL, nil)
	require.Equal(t, http.StatusOK, dlRec.Code, "body: %s", dlRec.Body.String())
	assert.Equal(t, payload, dlRec.Body.Bytes())

	// 5. Replay complete returns identical JSON.
	replayRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusOK, replayRec.Code, "body: %s", replayRec.Body.String())
	assert.JSONEq(t, compRec.Body.String(), replayRec.Body.String())
}

func TestChatUploads_RetriedFinalizeNoDuplicateArtifact(t *testing.T) {
	env := newChatUploadEnv(t, true, true)
	ctx := context.Background()

	payload := []byte("idempotent finalize test payload")
	startRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "dup.txt", Size: int64(len(payload))})
	require.Equal(t, http.StatusCreated, startRec.Code)
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))

	chunkURL := "/api/v1/chat/attachments/uploads/" + started.ID
	require.Equal(t, http.StatusOK, chatUploadChunkReq(t, env.srv, env.token, chunkURL+"?offset=0", payload).Code)

	// Simulate Replica A running Finalize (publishing the artifact and deleting
	// staging) then crashing before transitioning upload_sessions -> completed.
	sess, err := env.st.GetUploadSession(ctx, started.ID)
	require.NoError(t, err)
	claimed, err := env.st.ClaimUploadLease(ctx, started.ID, store.UploadStateUploading, int64(len(payload)), "tok-crash", 10*time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, env.st.TransitionUploadSession(ctx, started.ID, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateFinalizing, "tok-crash", nil))

	digestHex, err := finalizeUploadDigestHex(claimed.HashState)
	require.NoError(t, err)
	sink := newObjectUploadSink(env.blobs, env.srv.attachmentService())
	userCtx := contextWithIdentity(ctx, NewAuthenticatedUser(env.user.ID, env.user.Email, env.user.DisplayName, env.user.Role, string(ClientTypeWeb)))
	firstResult, err := sink.Finalize(userCtx, claimed, digestHex)
	require.NoError(t, err)

	// Wait for crashed replica's finalize lease to expire, then Replica B
	// handles retried POST /complete.
	time.Sleep(25 * time.Millisecond)
	retryRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusOK, retryRec.Code, "body: %s", retryRec.Body.String())
	assert.JSONEq(t, string(firstResult), retryRec.Body.String())

	var got attachmentUploadResult
	require.NoError(t, json.Unmarshal(retryRec.Body.Bytes(), &got))
	assert.Equal(t, sess.ArtifactID, got.ID)
}

func TestChatUploads_DisallowedMediaTypeRefusedAtFirstChunk(t *testing.T) {
	env := newChatUploadEnv(t, true, true)
	ctx := context.Background()

	// 1. Disallowed extension (.html) refused at start.
	badExtRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "evil.html", Size: 64})
	require.Equal(t, http.StatusBadRequest, badExtRec.Code, "body: %s", badExtRec.Body.String())

	// 2. Neutral filename whose first bytes sniff as text/html is refused at
	// offset 0 BEFORE staging any part and without extending idle_expires_at.
	htmlPayload := []byte("<!DOCTYPE html><html><script>alert(1)</script></html>")
	startRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "innocent.bin", Size: int64(len(htmlPayload))})
	require.Equal(t, http.StatusCreated, startRec.Code, "body: %s", startRec.Body.String())
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))

	beforeSess, err := env.st.GetUploadSession(ctx, started.ID)
	require.NoError(t, err)

	chunkRec := chatUploadChunkReq(t, env.srv, env.token,
		"/api/v1/chat/attachments/uploads/"+started.ID+"?offset=0", htmlPayload)
	require.Equal(t, http.StatusBadRequest, chunkRec.Code, "body: %s", chunkRec.Body.String())
	assert.Equal(t, errCodeUploadDisallowedMediaType, chatUploadDecodeErr(t, chunkRec).Code)

	// Nothing staged in blob store.
	listed, err := env.blobs.List(ctx, storage.ListOptions{Prefix: storage.DirPrefix(beforeSess.StagingRef)})
	require.NoError(t, err)
	assert.Empty(t, listed.Objects, "disallowed media type must be refused before staging anything")

	// Expiry not extended and received stays 0.
	afterSess, err := env.st.GetUploadSession(ctx, started.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), afterSess.Received)
	assert.True(t, beforeSess.IdleExpiresAt.Equal(afterSess.IdleExpiresAt), "refused chunk must not extend idle_expires_at")
}

func TestChatUploads_ArtifactAttachmentsRequiredAndUnsupportedStorage(t *testing.T) {
	t.Run("artifact_attachments_required when experiment off", func(t *testing.T) {
		env := newChatUploadEnv(t, true, false)
		rec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
			UploadStartRequest{Name: "a.txt", Size: 10})
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		assert.Equal(t, errCodeArtifactAttachmentsRequired, chatUploadDecodeErr(t, rec).Code)
	})

	t.Run("large_uploads_unsupported_storage when blob store lacks Composer", func(t *testing.T) {
		env := newChatUploadEnv(t, true, true)
		env.srv.SetStorage(nonComposerLocalStorage{Storage: env.blobs})
		rec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
			UploadStartRequest{Name: "a.txt", Size: 10})
		require.Equal(t, http.StatusNotImplemented, rec.Code, "body: %s", rec.Body.String())
		assert.Equal(t, errCodeLargeUploadsUnsupportedStorage, chatUploadDecodeErr(t, rec).Code)
	})
}

func TestChatUploads_OwnerScopeBindingAndAuthz(t *testing.T) {
	env := newChatUploadEnv(t, true, true)
	ctx := context.Background()

	// Create a second user.
	other := &store.User{
		ID:          uuid.NewString(),
		Email:       "chat-upl-2@example.com",
		DisplayName: "Chat Uploader 2",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, env.st.CreateUser(ctx, other))
	otherToken, _, _, err := env.srv.userTokenService.GenerateTokenPair(other.ID, other.Email, other.DisplayName, other.Role, ClientTypeWeb)
	require.NoError(t, err)

	// User 1 starts a session.
	startRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "secret.txt", Size: 12})
	require.Equal(t, http.StatusCreated, startRec.Code)
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))

	chunkURL := "/api/v1/chat/attachments/uploads/" + started.ID

	// Other user gets 404 on every session endpoint.
	assert.Equal(t, http.StatusNotFound, chatUploadJSONReq(t, env.srv, otherToken, http.MethodGet, chunkURL, nil).Code)
	assert.Equal(t, http.StatusNotFound, chatUploadChunkReq(t, env.srv, otherToken, chunkURL+"?offset=0", []byte("123456789012")).Code)
	assert.Equal(t, http.StatusNotFound, chatUploadJSONReq(t, env.srv, otherToken, http.MethodPost, chunkURL+"/complete", nil).Code)
	assert.Equal(t, http.StatusNotFound, chatUploadJSONReq(t, env.srv, otherToken, http.MethodDelete, chunkURL, nil).Code)

	// Project-scoped upload by a non-member gets 403.
	project, _ := createTestHubManagedProject(t, env.srv, "Chat Upload Authz Project")
	noAccessRec := chatUploadJSONReq(t, env.srv, otherToken, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "p.txt", Size: 10, ProjectID: project.ID})
	assert.Equal(t, http.StatusForbidden, noAccessRec.Code, "body: %s", noAccessRec.Body.String())

	// Dev admin (project creator) can start a project-scoped chat upload.
	projStart := chatUploadJSONReq(t, env.srv, testDevToken, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "p.txt", Size: 5, ProjectID: project.ID})
	require.Equal(t, http.StatusCreated, projStart.Code, "body: %s", projStart.Body.String())

	// Agent principal is refused (403) on /api/v1/chat/attachments/uploads.
	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ag-chat-upload-refuse",
		Name:      "ag-chat-upload-refuse",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, env.st.CreateAgent(ctx, ag))
	agentTok, err := env.srv.GenerateAgentToken(ag.ID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	agentRec := chatUploadJSONReq(t, env.srv, agentTok, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "a.txt", Size: 5})
	assert.Equal(t, http.StatusForbidden, agentRec.Code, "body: %s", agentRec.Body.String())

	// Scoped UAT bearer disposition checks (review-1 R3, cases i-iv):
	// Give env.user project-member access on two distinct projects (projA, projB)
	// so account permissions allow both and token boundary/scope governs.
	projA, _ := createTestHubManagedProject(t, env.srv, "Chat Upload UAT ProjA")
	projB, _ := createTestHubManagedProject(t, env.srv, "Chat Upload UAT ProjB")
	memberRole, err := env.st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	for _, pid := range []string{projA.ID, projB.ID} {
		_, err := env.st.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRole.ID,
			PrincipalType:    "user",
			PrincipalID:      env.user.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          pid,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	tokProjA := mintHubConfigToken(t, env.srv, env.user.ID, projectBoundary(projA.ID), "inbox:write", "project:read")
	tokProjB := mintHubConfigToken(t, env.srv, env.user.ID, projectBoundary(projB.ID), "inbox:write", "project:read")
	tokNoInboxWrite := mintHubConfigToken(t, env.srv, env.user.ID, hubBoundary(), "project:read")
	tokHubInboxWrite := mintHubConfigToken(t, env.srv, env.user.ID, hubBoundary(), "inbox:write")

	// (i) Project-A boundary token with inbox:write + project:read starts a projA session (201)
	// and can PUT chunks on it (200).
	startProjA := chatUploadJSONReq(t, env.srv, tokProjA, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "a.txt", Size: 10, ProjectID: projA.ID})
	require.Equal(t, http.StatusCreated, startProjA.Code, "case (i) start projA: %s", startProjA.Body.String())
	var sessA UploadStartResponse
	require.NoError(t, json.Unmarshal(startProjA.Body.Bytes(), &sessA))
	putProjA := chatUploadChunkReq(t, env.srv, tokProjA, "/api/v1/chat/attachments/uploads/"+sessA.ID+"?offset=0", []byte("01234"))
	assert.Equal(t, http.StatusOK, putProjA.Code, "case (i) PUT projA: %s", putProjA.Body.String())

	// Start a projB session with tokProjB so case (ii) can exercise PUT on an existing projB session.
	startProjB := chatUploadJSONReq(t, env.srv, tokProjB, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "b.txt", Size: 10, ProjectID: projB.ID})
	require.Equal(t, http.StatusCreated, startProjB.Code, "setup projB: %s", startProjB.Body.String())
	var sessB UploadStartResponse
	require.NoError(t, json.Unmarshal(startProjB.Body.Bytes(), &sessB))

	// (ii) Project-A boundary token gets 403 on starting projB, on PUT/GET/DELETE/complete
	// of an existing projB session, and on starting project-less.
	assert.Equal(t, http.StatusForbidden, chatUploadJSONReq(t, env.srv, tokProjA, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "b2.txt", Size: 5, ProjectID: projB.ID}).Code, "case (ii) start projB with projA token")
	assert.Equal(t, http.StatusForbidden, chatUploadChunkReq(t, env.srv, tokProjA,
		"/api/v1/chat/attachments/uploads/"+sessB.ID+"?offset=0", []byte("01234")).Code, "case (ii) PUT existing projB session with projA token")
	assert.Equal(t, http.StatusForbidden, chatUploadJSONReq(t, env.srv, tokProjA, http.MethodGet,
		"/api/v1/chat/attachments/uploads/"+sessB.ID, nil).Code, "case (ii) GET existing projB session with projA token")
	assert.Equal(t, http.StatusForbidden, chatUploadJSONReq(t, env.srv, tokProjA, http.MethodPost,
		"/api/v1/chat/attachments/uploads/"+sessB.ID+"/complete", nil).Code, "case (ii) complete existing projB session with projA token")
	assert.Equal(t, http.StatusForbidden, chatUploadJSONReq(t, env.srv, tokProjA, http.MethodDelete,
		"/api/v1/chat/attachments/uploads/"+sessB.ID, nil).Code, "case (ii) DELETE existing projB session with projA token")

	// (iii) Token without inbox:write gets 403 on start and on PUT of an existing session.
	assert.Equal(t, http.StatusForbidden, chatUploadJSONReq(t, env.srv, tokNoInboxWrite, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "no-scope.txt", Size: 5, ProjectID: projA.ID}).Code, "case (iii) start without inbox:write")
	assert.Equal(t, http.StatusForbidden, chatUploadChunkReq(t, env.srv, tokNoInboxWrite,
		"/api/v1/chat/attachments/uploads/"+sessA.ID+"?offset=5", []byte("56789")).Code, "case (iii) PUT without inbox:write")

	// (iv) Project-boundary token gets 403 on project-less start and on PUT of an existing
	// project-less session; hub-boundary token with inbox:write gets 201 on start and 200 on PUT.
	assert.Equal(t, http.StatusForbidden, chatUploadJSONReq(t, env.srv, tokProjA, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "dm.txt", Size: 5}).Code, "case (iv) project-less start with project-boundary token")
	assert.Equal(t, http.StatusForbidden, chatUploadChunkReq(t, env.srv, tokProjA,
		chunkURL+"?offset=0", []byte("12345")).Code, "case (iv) project-less PUT with project-boundary token")

	startHubDM := chatUploadJSONReq(t, env.srv, tokHubInboxWrite, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "dm-hub.txt", Size: 5})
	require.Equal(t, http.StatusCreated, startHubDM.Code, "case (iv) project-less start with hub-boundary token: %s", startHubDM.Body.String())
	var sessHubDM UploadStartResponse
	require.NoError(t, json.Unmarshal(startHubDM.Body.Bytes(), &sessHubDM))
	assert.Equal(t, http.StatusOK, chatUploadChunkReq(t, env.srv, tokHubInboxWrite,
		"/api/v1/chat/attachments/uploads/"+sessHubDM.ID+"?offset=0", []byte("hello")).Code, "case (iv) project-less PUT with hub-boundary token")
}

func TestChatUploads_StaleWriterPartNeverReplacesCommittedPart(t *testing.T) {
	env := newChatUploadEnv(t, true, true)
	ctx := context.Background()

	part0 := []byte("chunk-zero-")
	goodPart1 := []byte("winning-part-1!")
	stalePart1 := []byte("STALE-OVERWRITE")
	require.Equal(t, len(goodPart1), len(stalePart1))
	wantFull := append(append([]byte(nil), part0...), goodPart1...)

	startRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "fenced.txt", Size: int64(len(wantFull))})
	require.Equal(t, http.StatusCreated, startRec.Code)
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))

	chunkURL := "/api/v1/chat/attachments/uploads/" + started.ID
	require.Equal(t, http.StatusOK, chatUploadChunkReq(t, env.srv, env.token, chunkURL+"?offset=0", part0).Code)

	// Stale writer claims a short lease at offset len(part0) and writes its
	// token-named part object, then stalls until its lease expires.
	staleClaimed, err := env.st.ClaimUploadLease(ctx, started.ID, store.UploadStateUploading, int64(len(part0)), "stale-tok", 10*time.Millisecond)
	require.NoError(t, err)
	sink := newObjectUploadSink(env.blobs, env.srv.attachmentService())
	stalePartPath, err := sink.WriteChunk(ctx, staleClaimed, int64(len(part0)), "stale-tok", bytes.NewReader(stalePart1), int64(len(stalePart1)))
	require.NoError(t, err)
	require.Contains(t, stalePartPath, "stale-tok")

	// Wait for stale lease to expire; winning writer takes over via HTTP PUT.
	time.Sleep(25 * time.Millisecond)
	winRec := chatUploadChunkReq(t, env.srv, env.token, fmt.Sprintf("%s?offset=%d", chunkURL, len(part0)), goodPart1)
	require.Equal(t, http.StatusOK, winRec.Code, "body: %s", winRec.Body.String())

	// Stale writer attempts to commit its part -> ErrLeaseLost.
	err = env.st.CommitUploadChunk(ctx, started.ID, "stale-tok", store.UploadChunkCommit{
		Received:      int64(len(wantFull)),
		HashState:     staleClaimed.HashState,
		Parts:         append(append([]string(nil), staleClaimed.Parts...), stalePartPath),
		IdleExpiresAt: time.Now().Add(time.Hour),
	})
	assert.ErrorIs(t, err, store.ErrLeaseLost)

	// Complete and verify the composed artifact contains ONLY the winning part's bytes.
	compRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusOK, compRec.Code, "body: %s", compRec.Body.String())
	var compResp attachmentUploadResult
	require.NoError(t, json.Unmarshal(compRec.Body.Bytes(), &compResp))

	dlRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodGet, compResp.URL, nil)
	require.Equal(t, http.StatusOK, dlRec.Code)
	assert.Equal(t, wantFull, dlRec.Body.Bytes(), "stale writer's part must never replace committed part")

	// Orphan stale part was also removed when staging prefix was deleted.
	exists, err := env.blobs.Exists(ctx, stalePartPath)
	require.NoError(t, err)
	assert.False(t, exists, "orphan stale part should be cleaned up with staging prefix")
}

func TestChatUploads_WritersAbortOffsetMismatchAndSweeper(t *testing.T) {
	env := newChatUploadEnv(t, true, true)
	ctx := context.Background()

	// 1. Offset mismatch + premature complete do not extend idle_expires_at,
	// and DELETE abort removes staging idempotently.
	startRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "abort.txt", Size: 20})
	require.Equal(t, http.StatusCreated, startRec.Code)
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	chunkURL := "/api/v1/chat/attachments/uploads/" + started.ID

	require.Equal(t, http.StatusOK, chatUploadChunkReq(t, env.srv, env.token, chunkURL+"?offset=0", []byte("0123456789")).Code)
	afterChunk, err := env.st.GetUploadSession(ctx, started.ID)
	require.NoError(t, err)

	// Wrong offset -> 409 offset_mismatch with details.received = 10.
	omRec := chatUploadChunkReq(t, env.srv, env.token, chunkURL+"?offset=5", []byte("abcde"))
	require.Equal(t, http.StatusConflict, omRec.Code)
	omErr := chatUploadDecodeErr(t, omRec)
	assert.Equal(t, errCodeUploadOffsetMismatch, omErr.Code)
	assert.Equal(t, float64(10), omErr.Details["received"])

	// Premature complete -> 409 upload_incomplete.
	incRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusConflict, incRec.Code)
	assert.Equal(t, errCodeUploadIncomplete, chatUploadDecodeErr(t, incRec).Code)

	afterRefused, err := env.st.GetUploadSession(ctx, started.ID)
	require.NoError(t, err)
	assert.True(t, afterChunk.IdleExpiresAt.Equal(afterRefused.IdleExpiresAt), "refused calls must not extend idle_expires_at")

	// Abort -> 204, staging removed, second DELETE -> 204 idempotent.
	delRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodDelete, chunkURL, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code)
	delRec2 := chatUploadJSONReq(t, env.srv, env.token, http.MethodDelete, chunkURL, nil)
	require.Equal(t, http.StatusNoContent, delRec2.Code)

	listed, err := env.blobs.List(ctx, storage.ListOptions{Prefix: storage.DirPrefix(afterChunk.StagingRef)})
	require.NoError(t, err)
	assert.Empty(t, listed.Objects, "abort should remove staged parts")

	// 2. Sweeper expires idle chat upload session and removes staging even when
	// hub.large_uploads is toggled off mid-flight.
	start2 := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "sweep.txt", Size: 20})
	require.Equal(t, http.StatusCreated, start2.Code)
	var s2 UploadStartResponse
	require.NoError(t, json.Unmarshal(start2.Body.Bytes(), &s2))
	require.Equal(t, http.StatusOK, chatUploadChunkReq(t, env.srv, env.token, "/api/v1/chat/attachments/uploads/"+s2.ID+"?offset=0", []byte("0123456789")).Code)

	sess2, err := env.st.GetUploadSession(ctx, s2.ID)
	require.NoError(t, err)
	listedBefore, err := env.blobs.List(ctx, storage.ListOptions{Prefix: storage.DirPrefix(sess2.StagingRef)})
	require.NoError(t, err)
	require.Len(t, listedBefore.Objects, 1, "staged part should exist before sweep")

	// Toggle experiment off -> next request returns 404 experiment_disabled.
	setChatUploadExperiments(t, env.srv, false, true)
	offRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodGet, "/api/v1/chat/attachments/uploads/"+s2.ID, nil)
	require.Equal(t, http.StatusNotFound, offRec.Code)
	assert.Equal(t, ErrCodeExperimentDisabled, chatUploadDecodeErr(t, offRec).Code)

	// Sweeper still runs with experiment off and removes staging.
	env.srv.sweepUploadSessions(ctx, sess2.IdleExpiresAt.Add(time.Minute))
	expiredSess, err := env.st.GetUploadSession(ctx, s2.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UploadStateExpired, expiredSess.State)

	listedAfter, err := env.blobs.List(ctx, storage.ListOptions{Prefix: storage.DirPrefix(sess2.StagingRef)})
	require.NoError(t, err)
	assert.Empty(t, listedAfter.Objects, "sweeper must remove ObjectSink staging on expiry")
}

func TestChatUploads_OneGiBReachableViaAdminAPI(t *testing.T) {
	env := newChatUploadEnv(t, true, true)
	ctx := context.Background()

	ops := NewOperationalSettings(env.st, koanf.New("."), koanf.New("."))
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)
	env.srv.SetOperationalSettings(ops)

	const oneGiB int64 = 1 << 30 // 1073741824

	putConfig := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	getConfig := func() ServerConfigDBResponse {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config", nil)
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "GET server-config: %s", rec.Body.String())
		var resp ServerConfigDBResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	// 1. Raise artifacts.max_file_bytes and artifacts.max_bundle_bytes to 1 GiB
	// through the real Admin HTTP PUT /api/v1/admin/server-config.
	rr := putConfig(fmt.Sprintf(`{"artifacts":{"max_file_bytes":%d,"max_bundle_bytes":%d}}`, oneGiB, oneGiB))
	require.Equal(t, http.StatusOK, rr.Code, "PUT 1 GiB artifacts limits: %s", rr.Body.String())

	gotCfg := ops.Artifacts()
	assert.Equal(t, oneGiB, gotCfg.MaxFileBytes)
	assert.Equal(t, oneGiB, gotCfg.MaxBundleBytes)

	getResp := getConfig()
	require.NotNil(t, getResp.Artifacts)
	require.NotNil(t, getResp.Artifacts.MaxFileBytes)
	require.NotNil(t, getResp.Artifacts.MaxBundleBytes)
	assert.Equal(t, oneGiB, *getResp.Artifacts.MaxFileBytes)
	assert.Equal(t, oneGiB, *getResp.Artifacts.MaxBundleBytes)

	// 2. Chat upload start with declared size 1 GiB succeeds with 201 and maxBytes == 1 GiB.
	startRec := chatUploadJSONReq(t, env.srv, env.token, http.MethodPost, "/api/v1/chat/attachments/uploads",
		UploadStartRequest{Name: "model-1gib.bin", Size: oneGiB})
	require.Equal(t, http.StatusCreated, startRec.Code, "body: %s", startRec.Body.String())
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	assert.Equal(t, oneGiB, started.MaxBytes)
	require.NoError(t, env.st.DeleteUploadSession(ctx, started.ID))

	// 3. Cross-field rule: max_file_bytes > max_bundle_bytes -> 4xx (422).
	rrBad := putConfig(fmt.Sprintf(`{"artifacts":{"max_file_bytes":%d}}`, oneGiB+1))
	require.GreaterOrEqual(t, rrBad.Code, 400, "expected 4xx for max_file_bytes > max_bundle_bytes, got %d: %s", rrBad.Code, rrBad.Body.String())
	require.Less(t, rrBad.Code, 500)

	// 4. Omitted keeps the stored value: updating max_files preserves 1 GiB limits.
	rrKeep := putConfig(`{"artifacts":{"max_files":50,"default_retention_days":14,"link_default_ttl_hours":12,"link_max_ttl_hours":48,"gc_grace_hours":48,"remote_images_enabled":false,"remote_image_max_count":5,"remote_image_max_bytes":1048576,"remote_image_fetch_timeout_s":5,"remote_image_total_budget_s":20}}`)
	require.Equal(t, http.StatusOK, rrKeep.Code, "PUT sibling artifacts keys: %s", rrKeep.Body.String())
	gotAfterKeep := ops.Artifacts()
	assert.Equal(t, oneGiB, gotAfterKeep.MaxFileBytes, "omitted max_file_bytes must keep stored value")
	assert.Equal(t, oneGiB, gotAfterKeep.MaxBundleBytes, "omitted max_bundle_bytes must keep stored value")
	assert.Equal(t, 50, gotAfterKeep.MaxFiles)
	assert.Equal(t, 14, gotAfterKeep.DefaultRetentionDays)
	assert.Equal(t, 12, gotAfterKeep.LinkDefaultTTLHours)
	assert.Equal(t, 48, gotAfterKeep.LinkMaxTTLHours)
	assert.Equal(t, 48, gotAfterKeep.GCGraceHours)
	assert.False(t, gotAfterKeep.RemoteImagesEnabled)
	assert.Equal(t, 5, gotAfterKeep.RemoteImageMaxCount)
	assert.Equal(t, int64(1048576), gotAfterKeep.RemoteImageMaxBytes)
	assert.Equal(t, 5, gotAfterKeep.RemoteImageFetchTimeoutS)
	assert.Equal(t, 20, gotAfterKeep.RemoteImageTotalBudgetS)

	// 5. Explicit null clears the key back to its compiled default while keeping siblings.
	rrNull := putConfig(`{"artifacts":{"max_file_bytes":null}}`)
	require.Equal(t, http.StatusOK, rrNull.Code, "PUT null max_file_bytes: %s", rrNull.Body.String())
	gotAfterNull := ops.Artifacts()
	assert.Equal(t, int64(32<<20), gotAfterNull.MaxFileBytes, "null max_file_bytes must clear back to 32 MiB default")
	assert.Equal(t, oneGiB, gotAfterNull.MaxBundleBytes, "sibling max_bundle_bytes must remain stored")
	assert.Equal(t, 50, gotAfterNull.MaxFiles, "sibling max_files must remain stored")
}
