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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setLargeUploadsExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	cur := srv.experimentRegistry()
	var active []experiments.Experiment
	for _, e := range cur.All() {
		if e.Name == experiments.LargeUploads {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, cur.Retired())
	require.NoError(t, err)
	srv.setExperimentRegistry(reg)
	require.Equal(t, on, srv.experimentEnabled(experiments.LargeUploads))
}

func doChunkPut(t *testing.T, srv *Server, bearer, urlPath string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, urlPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else {
		req.Header.Set("Authorization", "Bearer "+testDevToken)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeUploadAPIError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp.Error
}

func TestWorkspaceUploads_ExperimentDisabled(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "Uploads Exp Off")
	// Default is off.
	require.False(t, srv.experimentEnabled(experiments.LargeUploads))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "a.bin", Path: "a.bin", Size: 16})
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	apiErr := decodeUploadAPIError(t, rec)
	assert.Equal(t, ErrCodeExperimentDisabled, apiErr.Code)

	// Existing single-request write route still works unchanged.
	writeRec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files/small.txt", project.ID),
		map[string]string{"content": "hello"})
	require.Equal(t, http.StatusOK, writeRec.Code, "body: %s", writeRec.Body.String())
}

func TestWorkspaceUploads_HappyPathSHA256AndPartialHidden(t *testing.T) {
	srv, _ := testServer(t)
	setLargeUploadsExperiment(t, srv, true)
	project, wsPath := createTestHubManagedProject(t, srv, "Uploads Happy")

	payload := bytes.Repeat([]byte("scion-large-upload-test-"), 200) // 4800 bytes
	wantSum := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(wantSum[:])

	// 1. Start
	startRec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{
			Name:      "model.bin",
			Path:      "nested/dir/model.bin",
			Size:      int64(len(payload)),
			MediaType: "application/octet-stream",
			Overwrite: true,
		})
	require.Equal(t, http.StatusCreated, startRec.Code, "body: %s", startRec.Body.String())
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	assert.Contains(t, started.ID, "upl_")
	assert.Equal(t, "hub_chunked", started.Transport)
	assert.Equal(t, int64(0), started.Received)
	assert.Equal(t, srv.WorkspaceUploadMaxFileBytes(context.Background()), started.MaxBytes)
	assert.Positive(t, started.ChunkBytes)

	// 2. Chunk 1 (first 2000 bytes)
	chunkURL := fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", project.ID, started.ID)
	c1 := doChunkPut(t, srv, "", chunkURL+"?offset=0", payload[:2000])
	require.Equal(t, http.StatusOK, c1.Code, "body: %s", c1.Body.String())
	var c1Resp UploadChunkResponse
	require.NoError(t, json.Unmarshal(c1.Body.Bytes(), &c1Resp))
	assert.Equal(t, int64(2000), c1Resp.Received)

	// Partial file exists on disk but is hidden from workspace file listing.
	partialDisk := filepath.Join(wsPath, "nested", "dir", uploadPartialName(started.ID))
	_, err := os.Stat(partialDisk)
	require.NoError(t, err, "partial file should exist on disk during upload")

	listRec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, listRec.Code)
	var listed ProjectWorkspaceListResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listed))
	for _, f := range listed.Files {
		assert.False(t, isUploadPartialName(filepath.Base(f.Path)), "partial leaked into listing: %s", f.Path)
	}

	// 3. Chunk 2 (remaining bytes)
	c2 := doChunkPut(t, srv, "", chunkURL+"?offset=2000", payload[2000:])
	require.Equal(t, http.StatusOK, c2.Code, "body: %s", c2.Body.String())
	var c2Resp UploadChunkResponse
	require.NoError(t, json.Unmarshal(c2.Body.Bytes(), &c2Resp))
	assert.Equal(t, int64(len(payload)), c2Resp.Received)

	// 4. Complete
	compRec := doRequest(t, srv, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusOK, compRec.Code, "body: %s", compRec.Body.String())
	var compResp ProjectWorkspaceFile
	require.NoError(t, json.Unmarshal(compRec.Body.Bytes(), &compResp))
	assert.Equal(t, "nested/dir/model.bin", compResp.Path)
	assert.Equal(t, int64(len(payload)), compResp.Size)

	// Target file matches SHA-256 and partial is gone.
	gotBytes, err := os.ReadFile(filepath.Join(wsPath, "nested", "dir", "model.bin"))
	require.NoError(t, err)
	gotSum := sha256.Sum256(gotBytes)
	assert.Equal(t, wantHex, hex.EncodeToString(gotSum[:]))
	_, err = os.Stat(partialDisk)
	assert.True(t, os.IsNotExist(err), "partial should be renamed away")

	// 5. Complete replay returns identical JSON result.
	replayRec := doRequest(t, srv, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusOK, replayRec.Code, "body: %s", replayRec.Body.String())
	assert.JSONEq(t, compRec.Body.String(), replayRec.Body.String())
}

func TestWorkspaceUploads_OffsetMismatchResumeAndNoExpiryExtension(t *testing.T) {
	srv, st := testServer(t)
	setLargeUploadsExperiment(t, srv, true)
	project, wsPath := createTestHubManagedProject(t, srv, "Uploads Resume")

	payload := []byte("0123456789abcdef")
	startRec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "res.bin", Path: "res.bin", Size: int64(len(payload)), Overwrite: true})
	require.Equal(t, http.StatusCreated, startRec.Code)
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	chunkURL := fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", project.ID, started.ID)

	// Upload first 6 bytes.
	require.Equal(t, http.StatusOK, doChunkPut(t, srv, "", chunkURL+"?offset=0", payload[:6]).Code)

	origSess, err := st.GetUploadSession(context.Background(), started.ID)
	require.NoError(t, err)
	origIdle := origSess.IdleExpiresAt

	// Advance clock by 5 minutes so any accidental expiry extension is observable.
	origNow := uploadNowFn
	t.Cleanup(func() { uploadNowFn = origNow })
	uploadNowFn = func() time.Time { return time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond) }

	// Wrong offset -> 409 offset_mismatch with received=6; must NOT extend idle_expires_at.
	badOff := doChunkPut(t, srv, "", chunkURL+"?offset=0", payload[:6])
	require.Equal(t, http.StatusConflict, badOff.Code, "body: %s", badOff.Body.String())
	errObj := decodeUploadAPIError(t, badOff)
	assert.Equal(t, errCodeUploadOffsetMismatch, errObj.Code)
	assert.Equal(t, float64(6), errObj.Details["received"])

	// Declared size exceeded -> 413 declared_size_exceeded; must NOT extend idle_expires_at.
	tooFar := doChunkPut(t, srv, "", chunkURL+"?offset=6", bytes.Repeat([]byte("x"), 20))
	require.Equal(t, http.StatusRequestEntityTooLarge, tooFar.Code)
	assert.Equal(t, errCodeUploadDeclaredExceeded, decodeUploadAPIError(t, tooFar).Code)

	// Premature complete -> 409 upload_incomplete; must NOT extend idle_expires_at.
	premature := doRequest(t, srv, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusConflict, premature.Code)
	assert.Equal(t, errCodeUploadIncomplete, decodeUploadAPIError(t, premature).Code)

	// Status GET -> 200; must NOT extend idle_expires_at.
	statusRec := doRequest(t, srv, http.MethodGet, chunkURL, nil)
	require.Equal(t, http.StatusOK, statusRec.Code)
	var statusResp UploadStatusResponse
	require.NoError(t, json.Unmarshal(statusRec.Body.Bytes(), &statusResp))
	assert.Equal(t, store.UploadStateUploading, statusResp.State)
	assert.Equal(t, int64(6), statusResp.Received)

	afterRefused, err := st.GetUploadSession(context.Background(), started.ID)
	require.NoError(t, err)
	assert.True(t, afterRefused.IdleExpiresAt.Equal(origIdle),
		"refused calls/status extended idle_expires_at: got %v, want %v", afterRefused.IdleExpiresAt, origIdle)

	// Resume from offset 6 -> 200 and extends idle_expires_at.
	c2 := doChunkPut(t, srv, "", chunkURL+"?offset=6", payload[6:])
	require.Equal(t, http.StatusOK, c2.Code)
	afterCommit, err := st.GetUploadSession(context.Background(), started.ID)
	require.NoError(t, err)
	assert.True(t, afterCommit.IdleExpiresAt.After(origIdle), "successful chunk commit should extend idle_expires_at")

	// Complete succeeds.
	require.Equal(t, http.StatusOK, doRequest(t, srv, http.MethodPost, chunkURL+"/complete", nil).Code)
	got, err := os.ReadFile(filepath.Join(wsPath, "res.bin"))
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestWorkspaceUploads_BusyLeaseAndFinalizing202AndReclaim(t *testing.T) {
	srv, st := testServer(t)
	setLargeUploadsExperiment(t, srv, true)
	project, wsPath := createTestHubManagedProject(t, srv, "Uploads Lease")

	payload := []byte("lease-test-payload")
	startRec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "lease.bin", Path: "lease.bin", Size: int64(len(payload)), Overwrite: true})
	require.Equal(t, http.StatusCreated, startRec.Code)
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	chunkURL := fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", project.ID, started.ID)

	// Hold an active lease directly in the store -> chunk returns 409 upload_busy.
	_, err := st.ClaimUploadLease(context.Background(), started.ID, store.UploadStateUploading, 0, "other-replica-tok", time.Minute)
	require.NoError(t, err)
	busyRec := doChunkPut(t, srv, "", chunkURL+"?offset=0", payload)
	require.Equal(t, http.StatusConflict, busyRec.Code)
	assert.Equal(t, errCodeUploadBusy, decodeUploadAPIError(t, busyRec).Code)

	// Release lease and upload all bytes.
	require.NoError(t, st.ReleaseUploadLease(context.Background(), started.ID, "other-replica-tok"))
	require.Equal(t, http.StatusOK, doChunkPut(t, srv, "", chunkURL+"?offset=0", payload).Code)

	// Simulate another replica holding an active finalize lease -> complete returns 202 {"state":"finalizing"}.
	_, err = st.ClaimUploadLease(context.Background(), started.ID, store.UploadStateUploading, int64(len(payload)), "fin-replica-tok", time.Minute)
	require.NoError(t, err)
	require.NoError(t, st.TransitionUploadSession(context.Background(), started.ID, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateFinalizing, "fin-replica-tok", nil))

	fin202 := doRequest(t, srv, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusAccepted, fin202.Code, "body: %s", fin202.Body.String())
	var finBody map[string]string
	require.NoError(t, json.Unmarshal(fin202.Body.Bytes(), &finBody))
	assert.Equal(t, "finalizing", finBody["state"])

	// Release the finalizing lease (simulating expired finalize lease) -> retry re-claims and completes!
	require.NoError(t, st.ReleaseUploadLease(context.Background(), started.ID, "fin-replica-tok"))
	finOK := doRequest(t, srv, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusOK, finOK.Code, "body: %s", finOK.Body.String())

	got, err := os.ReadFile(filepath.Join(wsPath, "lease.bin"))
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestWorkspaceUploads_TakeoverRehashIntegrityMismatch(t *testing.T) {
	srv, st := testServer(t)
	setLargeUploadsExperiment(t, srv, true)
	project, wsPath := createTestHubManagedProject(t, srv, "Uploads Takeover")

	// Seed an existing target file to verify it remains byte-identical on integrity failure.
	existing := []byte("original-intact-content")
	require.NoError(t, os.WriteFile(filepath.Join(wsPath, "target.bin"), existing, 0o644))

	payload := []byte("0123456789")
	startRec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "target.bin", Path: "target.bin", Size: int64(len(payload)), Overwrite: true})
	require.Equal(t, http.StatusCreated, startRec.Code)
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	chunkURL := fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", project.ID, started.ID)

	// Upload first half, then force an expired lease so next claim increments takeovers.
	require.Equal(t, http.StatusOK, doChunkPut(t, srv, "", chunkURL+"?offset=0", payload[:5]).Code)
	_, err := st.ClaimUploadLease(context.Background(), started.ID, store.UploadStateUploading, 5, "stale-tok", time.Millisecond)
	require.NoError(t, err)
	time.Sleep(15 * time.Millisecond)

	// Second chunk takes over the expired lease (takeovers -> 1).
	require.Equal(t, http.StatusOK, doChunkPut(t, srv, "", chunkURL+"?offset=5", payload[5:]).Code)

	// Corrupt the partial file on disk (simulating a stale writer landing bytes after takeover).
	partialPath := filepath.Join(wsPath, uploadPartialName(started.ID))
	require.NoError(t, os.WriteFile(partialPath, []byte("CORRUPTED!"), 0o644))

	// Complete detects mismatch -> 409 integrity_mismatch, session becomes failed, existing target untouched.
	compRec := doRequest(t, srv, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusConflict, compRec.Code, "body: %s", compRec.Body.String())
	assert.Equal(t, errCodeUploadIntegrityMismatch, decodeUploadAPIError(t, compRec).Code)

	sess, err := st.GetUploadSession(context.Background(), started.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UploadStateFailed, sess.State)

	gotExisting, err := os.ReadFile(filepath.Join(wsPath, "target.bin"))
	require.NoError(t, err)
	assert.Equal(t, existing, gotExisting, "existing target must remain byte-identical after integrity failure")
}

func TestWorkspaceUploads_OwnerScopeBindingAndAgentRefused(t *testing.T) {
	srv, st := testServer(t)
	setLargeUploadsExperiment(t, srv, true)

	// Create two hub-admin users and two hub-managed projects so project-level
	// authz passes for both users and we specifically test (owner, scope) binding.
	ctx := context.Background()
	alice := &store.User{
		ID:          tid("user-upload-alice"),
		Email:       "upload-alice@test.com",
		DisplayName: "Upload Alice",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	bob := &store.User{
		ID:          tid("user-upload-bob"),
		Email:       "upload-bob@test.com",
		DisplayName: "Upload Bob",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, alice))
	require.NoError(t, st.CreateUser(ctx, bob))

	p1, _ := createTestHubManagedProject(t, srv, "Uploads Scope P1")
	p2, _ := createTestHubManagedProject(t, srv, "Uploads Scope P2")
	for _, u := range []*store.User{alice, bob} {
		for _, p := range []*store.Project{p1, p2} {
			require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, p.ID, u.ID))
		}
	}

	// Alice starts an upload on P1.
	startRec := doRequestAsUser(t, srv, alice, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", p1.ID),
		UploadStartRequest{Name: "secret.bin", Path: "secret.bin", Size: 10})
	require.Equal(t, http.StatusCreated, startRec.Code, "body: %s", startRec.Body.String())
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))

	// Bob gets 404 on Alice's session (status, chunk, complete, abort).
	bobStatus := doRequestAsUser(t, srv, bob, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", p1.ID, started.ID), nil)
	assert.Equal(t, http.StatusNotFound, bobStatus.Code)

	bobToken, _, _, err := srv.userTokenService.GenerateTokenPair(bob.ID, bob.Email, bob.DisplayName, bob.Role, ClientTypeWeb)
	require.NoError(t, err)
	bobChunk := doChunkPut(t, srv, bobToken,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s?offset=0", p1.ID, started.ID), []byte("0123456789"))
	assert.Equal(t, http.StatusNotFound, bobChunk.Code)

	// Alice gets 404 when using P1's session ID on P2 (scope mismatch).
	wrongProj := doRequestAsUser(t, srv, alice, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", p2.ID, started.ID), nil)
	assert.Equal(t, http.StatusNotFound, wrongProj.Code)

	// Agent principal is refused with 403 (OQ-5).
	ag := &store.Agent{
		ID:        tid("ag-upload-refuse"),
		Slug:      "ag-upload-refuse",
		Name:      "ag-upload-refuse",
		ProjectID: p1.ID,
		Phase:     "running",
	}
	require.NoError(t, st.CreateAgent(ctx, ag))
	agTok, err := srv.GenerateAgentToken(ag.ID, p1.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	agReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", p1.ID),
		bytes.NewReader([]byte(`{"name":"a.bin","path":"a.bin","size":4}`)))
	agReq.Header.Set("Content-Type", "application/json")
	agReq.Header.Set("Authorization", "Bearer "+agTok)
	agRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(agRec, agReq)
	assert.Equal(t, http.StatusForbidden, agRec.Code, "body: %s", agRec.Body.String())
}

func TestWorkspaceUploads_Quota429ConcurrentAndMaxFileSize413(t *testing.T) {
	srv, _ := testServer(t)
	setLargeUploadsExperiment(t, srv, true)
	project, _ := createTestHubManagedProject(t, srv, "Uploads Quota")

	// 1. Exceeding WorkspaceUploadMaxFileBytes -> 413 with real limit.
	maxBytes := srv.WorkspaceUploadMaxFileBytes(context.Background())
	tooBig := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "huge.bin", Path: "huge.bin", Size: maxBytes + 1})
	require.Equal(t, http.StatusRequestEntityTooLarge, tooBig.Code, "body: %s", tooBig.Body.String())
	tooBigErr := decodeUploadAPIError(t, tooBig)
	assert.Equal(t, errCodeUploadDeclaredExceeded, tooBigErr.Code)
	assert.Equal(t, float64(maxBytes), tooBigErr.Details["maxBytes"])

	// 2. Concurrent starts race against default max_sessions_per_user (4):
	//    exactly 4 succeed (201) and the rest get 429 upload_quota_exceeded with Retry-After.
	const total = 10
	var created atomic.Int32
	var quota429 atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range total {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			rec := doRequest(t, srv, http.MethodPost,
				fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
				UploadStartRequest{
					Name: fmt.Sprintf("f%d.bin", idx),
					Path: fmt.Sprintf("f%d.bin", idx),
					Size: 1024,
				})
			switch rec.Code {
			case http.StatusCreated:
				created.Add(1)
			case http.StatusTooManyRequests:
				quota429.Add(1)
				assert.NotEmpty(t, rec.Header().Get("Retry-After"))
				assert.Equal(t, errCodeUploadQuotaExceeded, decodeUploadAPIError(t, rec).Code)
			default:
				t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(4), created.Load())
	assert.Equal(t, int32(total-4), quota429.Load())
}

func TestWorkspaceUploads_SecurityPathTraversalSymlinkAndAtomicity(t *testing.T) {
	srv, _ := testServer(t)
	setLargeUploadsExperiment(t, srv, true)
	project, wsPath := createTestHubManagedProject(t, srv, "Uploads Security")

	// Path traversal, .git, .scion refused at start.
	for _, badPath := range []string{"../escape.txt", ".git/config", ".scion/evil.txt", ".scion-upload-fake.partial"} {
		rec := doRequest(t, srv, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
			UploadStartRequest{Name: "x", Path: badPath, Size: 8})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "path %q should be refused, got %d: %s", badPath, rec.Code, rec.Body.String())
	}

	// Symlinked dest_path refused at start.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(wsPath, "link.txt")))
	linkRec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "link.txt", Path: "link.txt", Size: 8, Overwrite: true})
	assert.Equal(t, http.StatusBadRequest, linkRec.Code, "symlink dest_path must be refused: %s", linkRec.Body.String())

	// Existing file stays byte-identical after abort and overwrite=false conflict.
	orig := []byte("must-not-change")
	require.NoError(t, os.WriteFile(filepath.Join(wsPath, "keep.txt"), orig, 0o644))

	// Case A: abort after uploading a chunk.
	s1Rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "keep.txt", Path: "keep.txt", Size: 10, Overwrite: true})
	require.Equal(t, http.StatusCreated, s1Rec.Code)
	var s1 UploadStartResponse
	require.NoError(t, json.Unmarshal(s1Rec.Body.Bytes(), &s1))
	u1 := fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", project.ID, s1.ID)
	require.Equal(t, http.StatusOK, doChunkPut(t, srv, "", u1+"?offset=0", []byte("12345")).Code)
	abortRec := doRequest(t, srv, http.MethodDelete, u1, nil)
	require.Equal(t, http.StatusNoContent, abortRec.Code)
	_, err := os.Stat(filepath.Join(wsPath, uploadPartialName(s1.ID)))
	assert.True(t, os.IsNotExist(err), "abort should remove partial")
	got, err := os.ReadFile(filepath.Join(wsPath, "keep.txt"))
	require.NoError(t, err)
	assert.Equal(t, orig, got)

	// Case B: overwrite=false fails at complete with 409, leaves existing file byte-identical.
	s2Rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "keep.txt", Path: "keep.txt", Size: 5, Overwrite: false})
	require.Equal(t, http.StatusCreated, s2Rec.Code)
	var s2 UploadStartResponse
	require.NoError(t, json.Unmarshal(s2Rec.Body.Bytes(), &s2))
	u2 := fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", project.ID, s2.ID)
	require.Equal(t, http.StatusOK, doChunkPut(t, srv, "", u2+"?offset=0", []byte("abcde")).Code)
	c2Rec := doRequest(t, srv, http.MethodPost, u2+"/complete", nil)
	assert.Equal(t, http.StatusConflict, c2Rec.Code)
	got, err = os.ReadFile(filepath.Join(wsPath, "keep.txt"))
	require.NoError(t, err)
	assert.Equal(t, orig, got)

	// Case C: symlinked partial refused at chunk write.
	s3Rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "sympartial.txt", Path: "sympartial.txt", Size: 5, Overwrite: true})
	require.Equal(t, http.StatusCreated, s3Rec.Code)
	var s3 UploadStartResponse
	require.NoError(t, json.Unmarshal(s3Rec.Body.Bytes(), &s3))
	require.NoError(t, os.Symlink(outside, filepath.Join(wsPath, uploadPartialName(s3.ID))))
	symChunk := doChunkPut(t, srv, "",
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s?offset=0", project.ID, s3.ID), []byte("abcde"))
	assert.Equal(t, http.StatusBadRequest, symChunk.Code, "symlinked partial must be refused: %s", symChunk.Body.String())
	outsideBytes, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, []byte("secret"), outsideBytes, "symlink target outside workspace must not be modified")
}

func TestWorkspaceUploads_SweeperRemovesPartialsAndRowsRegardlessOfExperiment(t *testing.T) {
	srv, st := testServer(t)
	setLargeUploadsExperiment(t, srv, true)
	project, wsPath := createTestHubManagedProject(t, srv, "Uploads Sweeper")

	orig := []byte("existing-file-bytes")
	require.NoError(t, os.WriteFile(filepath.Join(wsPath, "sweep.bin"), orig, 0o644))

	startRec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", project.ID),
		UploadStartRequest{Name: "sweep.bin", Path: "sweep.bin", Size: 20, Overwrite: true})
	require.Equal(t, http.StatusCreated, startRec.Code)
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	u := fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", project.ID, started.ID)
	require.Equal(t, http.StatusOK, doChunkPut(t, srv, "", u+"?offset=0", []byte("0123456789")).Code)

	partialPath := filepath.Join(wsPath, uploadPartialName(started.ID))
	_, err := os.Stat(partialPath)
	require.NoError(t, err)

	// Turn experiment OFF: sweeper must still clean up expired sessions and partials.
	setLargeUploadsExperiment(t, srv, false)

	// Sweep at now + 2h -> transitions uploading -> expired and removes partial.
	future := time.Now().UTC().Add(2 * time.Hour)
	srv.sweepUploadSessions(context.Background(), future)

	sess, err := st.GetUploadSession(context.Background(), started.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UploadStateExpired, sess.State)
	_, err = os.Stat(partialPath)
	assert.True(t, os.IsNotExist(err), "sweeper should remove partial on expiry")

	// Existing target file is still byte-identical.
	got, err := os.ReadFile(filepath.Join(wsPath, "sweep.bin"))
	require.NoError(t, err)
	assert.Equal(t, orig, got)

	// Sweep again past 15m terminal retention -> deletes the session row.
	srv.sweepUploadSessions(context.Background(), time.Now().UTC().Add(20*time.Minute))
	_, err = st.GetUploadSession(context.Background(), started.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "sweeper should delete expired row past retention window")
}

func TestWorkspaceUploads_LinkedRemoteProjectReturns409(t *testing.T) {
	srv, st := testServer(t)
	setLargeUploadsExperiment(t, srv, true)

	// Create a linked project (non-empty GitRemote, no SharedWorkspace) with no local workspace.
	proj := &store.Project{
		ID:        tid("proj-linked-remote-upload"),
		Slug:      "proj-linked-remote-upload",
		Name:      "Linked Remote Upload",
		GitRemote: "https://github.com/example/repo.git",
	}
	require.NoError(t, st.CreateProject(context.Background(), proj))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", proj.ID),
		UploadStartRequest{Name: "a.bin", Path: "a.bin", Size: 8})
	assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
}
