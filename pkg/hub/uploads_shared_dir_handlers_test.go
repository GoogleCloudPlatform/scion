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
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sdSetLargeUploadsExperiment(t *testing.T, srv *Server, on bool) {
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

func sdDoChunkPut(t *testing.T, srv *Server, bearer, urlPath string, body []byte) *httptest.ResponseRecorder {
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

func sdDecodeUploadAPIError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp.Error
}

func createTestProjectWithSharedDirs(t *testing.T, srv *Server, st store.Store, name string, dirs ...string) (*store.Project, map[string]string) {
	t.Helper()
	project, _ := createTestHubManagedProject(t, srv, name)
	existing := make(map[string]bool, len(project.SharedDirs))
	for _, d := range project.SharedDirs {
		existing[d.Name] = true
	}
	for _, d := range dirs {
		if !existing[d] {
			project.SharedDirs = append(project.SharedDirs, api.SharedDir{Name: d})
			existing[d] = true
		}
	}
	require.NoError(t, st.UpdateProject(context.Background(), project))
	paths := make(map[string]string, len(dirs))
	for _, d := range dirs {
		p := resolveTestSharedDirPath(t, project, d)
		require.NoError(t, os.MkdirAll(p, 0o755))
		paths[d] = p
	}
	return project, paths
}

func TestSharedDirUploads_ExperimentDisabled(t *testing.T) {
	srv, st := testServer(t)
	project, _ := createTestProjectWithSharedDirs(t, srv, st, "SD Uploads Exp Off", "scratchpad")
	require.False(t, srv.experimentEnabled(experiments.LargeUploads))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads", project.ID),
		UploadStartRequest{Name: "a.bin", Path: "a.bin", Size: 16})
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeExperimentDisabled, sdDecodeUploadAPIError(t, rec).Code)

	// Existing single-request shared-dir write route still works unchanged.
	writeRec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/files/small.txt", project.ID),
		map[string]string{"content": "hello"})
	require.Equal(t, http.StatusOK, writeRec.Code, "body: %s", writeRec.Body.String())
}

func TestSharedDirUploads_HappyPathSHA256AndPartialHidden(t *testing.T) {
	srv, st := testServer(t)
	sdSetLargeUploadsExperiment(t, srv, true)
	project, sdPaths := createTestProjectWithSharedDirs(t, srv, st, "SD Uploads Happy", "scratchpad")
	sdPath := sdPaths["scratchpad"]

	// Seed a visible file so archive and listing have at least one real entry.
	require.NoError(t, os.WriteFile(filepath.Join(sdPath, "readme.txt"), []byte("visible"), 0o644))

	payload := bytes.Repeat([]byte("scion-shared-dir-upload-"), 200) // 4800 bytes
	wantSum := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(wantSum[:])

	uploadsBase := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads", project.ID)

	// 1. Start
	startRec := doRequest(t, srv, http.MethodPost, uploadsBase, UploadStartRequest{
		Name:      "dataset.bin",
		Path:      "nested/dir/dataset.bin",
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
	chunkURL := fmt.Sprintf("%s/%s", uploadsBase, started.ID)
	c1 := sdDoChunkPut(t, srv, "", chunkURL+"?offset=0", payload[:2000])
	require.Equal(t, http.StatusOK, c1.Code, "body: %s", c1.Body.String())
	var c1Resp UploadChunkResponse
	require.NoError(t, json.Unmarshal(c1.Body.Bytes(), &c1Resp))
	assert.Equal(t, int64(2000), c1Resp.Received)

	// Status GET reflects committed offset.
	statusRec := doRequest(t, srv, http.MethodGet, chunkURL, nil)
	require.Equal(t, http.StatusOK, statusRec.Code, "body: %s", statusRec.Body.String())
	var statusResp UploadStatusResponse
	require.NoError(t, json.Unmarshal(statusRec.Body.Bytes(), &statusResp))
	assert.Equal(t, started.ID, statusResp.ID)
	assert.Equal(t, store.UploadStateUploading, statusResp.State)
	assert.Equal(t, int64(2000), statusResp.Received)
	assert.Equal(t, int64(len(payload)), statusResp.DeclaredSize)

	// Partial file exists on disk, but is hidden from shared-dir listing, search, and archive.
	partialDisk := filepath.Join(sdPath, "nested", "dir", uploadPartialName(started.ID))
	_, err := os.Stat(partialDisk)
	require.NoError(t, err, "partial file should exist on disk during upload")

	listRec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/files", project.ID), nil)
	require.Equal(t, http.StatusOK, listRec.Code)
	var listed SharedDirListResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listed))
	for _, f := range listed.Files {
		assert.False(t, isUploadPartialName(filepath.Base(f.Path)), "partial leaked into shared-dir listing: %s", f.Path)
	}

	searchRec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/files?q=partial", project.ID), nil)
	require.Equal(t, http.StatusOK, searchRec.Code)
	var searched SharedDirListResponse
	require.NoError(t, json.Unmarshal(searchRec.Body.Bytes(), &searched))
	for _, f := range searched.Files {
		assert.False(t, isUploadPartialName(filepath.Base(f.Path)), "partial leaked into shared-dir search: %s", f.Path)
	}

	archRec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/archive", project.ID), nil)
	require.Equal(t, http.StatusOK, archRec.Code)
	zr, err := zip.NewReader(bytes.NewReader(archRec.Body.Bytes()), int64(archRec.Body.Len()))
	require.NoError(t, err)
	for _, zf := range zr.File {
		assert.False(t, isUploadPartialName(filepath.Base(zf.Name)), "partial leaked into shared-dir archive: %s", zf.Name)
	}

	// 3. Chunk 2 (remaining bytes)
	c2 := sdDoChunkPut(t, srv, "", chunkURL+"?offset=2000", payload[2000:])
	require.Equal(t, http.StatusOK, c2.Code, "body: %s", c2.Body.String())
	var c2Resp UploadChunkResponse
	require.NoError(t, json.Unmarshal(c2.Body.Bytes(), &c2Resp))
	assert.Equal(t, int64(len(payload)), c2Resp.Received)

	// 4. Complete
	compRec := doRequest(t, srv, http.MethodPost, chunkURL+"/complete", nil)
	require.Equal(t, http.StatusOK, compRec.Code, "body: %s", compRec.Body.String())
	var compResp ProjectWorkspaceFile
	require.NoError(t, json.Unmarshal(compRec.Body.Bytes(), &compResp))
	assert.Equal(t, "nested/dir/dataset.bin", compResp.Path)
	assert.Equal(t, int64(len(payload)), compResp.Size)

	// Target file matches SHA-256 and partial is gone.
	gotBytes, err := os.ReadFile(filepath.Join(sdPath, "nested", "dir", "dataset.bin"))
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

func TestSharedDirUploads_OwnerScopeBindingAndAgentRefused(t *testing.T) {
	srv, st := testServer(t)
	sdSetLargeUploadsExperiment(t, srv, true)
	ctx := context.Background()

	alice := &store.User{
		ID:          tid("user-sdupload-alice"),
		Email:       "sdupload-alice@test.com",
		DisplayName: "SD Upload Alice",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	bob := &store.User{
		ID:          tid("user-sdupload-bob"),
		Email:       "sdupload-bob@test.com",
		DisplayName: "SD Upload Bob",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, alice))
	require.NoError(t, st.CreateUser(ctx, bob))

	p1, _ := createTestProjectWithSharedDirs(t, srv, st, "SD Scope P1", "scratchpad", "other-dir")
	p2, _ := createTestProjectWithSharedDirs(t, srv, st, "SD Scope P2", "scratchpad")
	for _, u := range []*store.User{alice, bob} {
		for _, p := range []*store.Project{p1, p2} {
			require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, p.ID, u.ID))
		}
	}

	// Undeclared shared dir -> 404.
	undeclared := doRequestAsUser(t, srv, alice, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/nonexistent/uploads", p1.ID),
		UploadStartRequest{Name: "a.bin", Path: "a.bin", Size: 8})
	assert.Equal(t, http.StatusNotFound, undeclared.Code)

	// Alice starts an upload on P1/scratchpad.
	startRec := doRequestAsUser(t, srv, alice, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads", p1.ID),
		UploadStartRequest{Name: "secret.bin", Path: "secret.bin", Size: 10})
	require.Equal(t, http.StatusCreated, startRec.Code, "body: %s", startRec.Body.String())
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))

	// Bob gets 404 on Alice's session (status and chunk).
	bobStatus := doRequestAsUser(t, srv, bob, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads/%s", p1.ID, started.ID), nil)
	assert.Equal(t, http.StatusNotFound, bobStatus.Code)

	bobToken, _, _, err := srv.userTokenService.GenerateTokenPair(bob.ID, bob.Email, bob.DisplayName, bob.Role, ClientTypeWeb)
	require.NoError(t, err)
	bobChunk := sdDoChunkPut(t, srv, bobToken,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads/%s?offset=0", p1.ID, started.ID), []byte("0123456789"))
	assert.Equal(t, http.StatusNotFound, bobChunk.Code)

	// Alice gets 404 when using P1/scratchpad's session ID on another project (P2).
	wrongProj := doRequestAsUser(t, srv, alice, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads/%s", p2.ID, started.ID), nil)
	assert.Equal(t, http.StatusNotFound, wrongProj.Code)

	// Alice gets 404 when using P1/scratchpad's session ID on another shared dir (other-dir).
	wrongDir := doRequestAsUser(t, srv, alice, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/other-dir/uploads/%s", p1.ID, started.ID), nil)
	assert.Equal(t, http.StatusNotFound, wrongDir.Code)

	// Alice gets 404 when using a shared-dir session ID on the workspace uploads route.
	wrongTarget := doRequestAsUser(t, srv, alice, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/%s", p1.ID, started.ID), nil)
	assert.Equal(t, http.StatusNotFound, wrongTarget.Code)

	// Agent principal is refused with 403 (OQ-5).
	ag := &store.Agent{
		ID:        tid("ag-sdupload-refuse"),
		Slug:      "ag-sdupload-refuse",
		Name:      "ag-sdupload-refuse",
		ProjectID: p1.ID,
		Phase:     "running",
	}
	require.NoError(t, st.CreateAgent(ctx, ag))
	agTok, err := srv.GenerateAgentToken(ag.ID, p1.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	agReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads", p1.ID),
		bytes.NewReader([]byte(`{"name":"a.bin","path":"a.bin","size":4}`)))
	agReq.Header.Set("Content-Type", "application/json")
	agReq.Header.Set("Authorization", "Bearer "+agTok)
	agRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(agRec, agReq)
	assert.Equal(t, http.StatusForbidden, agRec.Code, "body: %s", agRec.Body.String())
}

func TestSharedDirUploads_Quota429ConcurrentAndMaxFileSize413(t *testing.T) {
	srv, st := testServer(t)
	sdSetLargeUploadsExperiment(t, srv, true)
	project, _ := createTestProjectWithSharedDirs(t, srv, st, "SD Uploads Quota", "scratchpad")
	uploadsURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads", project.ID)

	// 1. Exceeding WorkspaceUploadMaxFileBytes -> 413 with real limit.
	maxBytes := srv.WorkspaceUploadMaxFileBytes(context.Background())
	tooBig := doRequest(t, srv, http.MethodPost, uploadsURL,
		UploadStartRequest{Name: "huge.bin", Path: "huge.bin", Size: maxBytes + 1})
	require.Equal(t, http.StatusRequestEntityTooLarge, tooBig.Code, "body: %s", tooBig.Body.String())
	tooBigErr := sdDecodeUploadAPIError(t, tooBig)
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
			rec := doRequest(t, srv, http.MethodPost, uploadsURL,
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
				assert.Equal(t, errCodeUploadQuotaExceeded, sdDecodeUploadAPIError(t, rec).Code)
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

func TestSharedDirUploads_SecurityPathTraversalSymlinkAndAtomicity(t *testing.T) {
	srv, st := testServer(t)
	sdSetLargeUploadsExperiment(t, srv, true)
	project, sdPaths := createTestProjectWithSharedDirs(t, srv, st, "SD Uploads Security", "scratchpad")
	sdPath := sdPaths["scratchpad"]
	uploadsURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads", project.ID)

	// Path traversal, .git, .scion, and reserved partial name refused at start.
	for _, badPath := range []string{"../escape.txt", ".git/config", ".scion/evil.txt", ".scion-upload-fake.partial"} {
		rec := doRequest(t, srv, http.MethodPost, uploadsURL,
			UploadStartRequest{Name: "x", Path: badPath, Size: 8})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "path %q should be refused, got %d: %s", badPath, rec.Code, rec.Body.String())
	}

	// Symlinked dest_path refused at start.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(sdPath, "link.txt")))
	linkRec := doRequest(t, srv, http.MethodPost, uploadsURL,
		UploadStartRequest{Name: "link.txt", Path: "link.txt", Size: 8, Overwrite: true})
	assert.Equal(t, http.StatusBadRequest, linkRec.Code, "symlink dest_path must be refused: %s", linkRec.Body.String())

	// Existing file stays byte-identical after abort and overwrite=false conflict.
	orig := []byte("must-not-change")
	require.NoError(t, os.WriteFile(filepath.Join(sdPath, "keep.txt"), orig, 0o644))

	// Case A: abort after uploading a chunk.
	s1Rec := doRequest(t, srv, http.MethodPost, uploadsURL,
		UploadStartRequest{Name: "keep.txt", Path: "keep.txt", Size: 10, Overwrite: true})
	require.Equal(t, http.StatusCreated, s1Rec.Code)
	var s1 UploadStartResponse
	require.NoError(t, json.Unmarshal(s1Rec.Body.Bytes(), &s1))
	u1 := fmt.Sprintf("%s/%s", uploadsURL, s1.ID)
	require.Equal(t, http.StatusOK, sdDoChunkPut(t, srv, "", u1+"?offset=0", []byte("12345")).Code)
	abortRec := doRequest(t, srv, http.MethodDelete, u1, nil)
	require.Equal(t, http.StatusNoContent, abortRec.Code)
	_, err := os.Stat(filepath.Join(sdPath, uploadPartialName(s1.ID)))
	assert.True(t, os.IsNotExist(err), "abort should remove partial")
	got, err := os.ReadFile(filepath.Join(sdPath, "keep.txt"))
	require.NoError(t, err)
	assert.Equal(t, orig, got)

	// Case B: overwrite=false fails at complete with 409, leaves existing file byte-identical.
	s2Rec := doRequest(t, srv, http.MethodPost, uploadsURL,
		UploadStartRequest{Name: "keep.txt", Path: "keep.txt", Size: 5, Overwrite: false})
	require.Equal(t, http.StatusCreated, s2Rec.Code)
	var s2 UploadStartResponse
	require.NoError(t, json.Unmarshal(s2Rec.Body.Bytes(), &s2))
	u2 := fmt.Sprintf("%s/%s", uploadsURL, s2.ID)
	require.Equal(t, http.StatusOK, sdDoChunkPut(t, srv, "", u2+"?offset=0", []byte("abcde")).Code)
	c2Rec := doRequest(t, srv, http.MethodPost, u2+"/complete", nil)
	assert.Equal(t, http.StatusConflict, c2Rec.Code)
	got, err = os.ReadFile(filepath.Join(sdPath, "keep.txt"))
	require.NoError(t, err)
	assert.Equal(t, orig, got)

	// Case C: symlinked partial refused at chunk write.
	s3Rec := doRequest(t, srv, http.MethodPost, uploadsURL,
		UploadStartRequest{Name: "sympartial.txt", Path: "sympartial.txt", Size: 5, Overwrite: true})
	require.Equal(t, http.StatusCreated, s3Rec.Code)
	var s3 UploadStartResponse
	require.NoError(t, json.Unmarshal(s3Rec.Body.Bytes(), &s3))
	require.NoError(t, os.Symlink(outside, filepath.Join(sdPath, uploadPartialName(s3.ID))))
	symChunk := sdDoChunkPut(t, srv, "", fmt.Sprintf("%s/%s?offset=0", uploadsURL, s3.ID), []byte("abcde"))
	assert.Equal(t, http.StatusBadRequest, symChunk.Code, "symlinked partial must be refused: %s", symChunk.Body.String())
	outsideBytes, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, []byte("secret"), outsideBytes, "symlink target outside shared dir must not be modified")
}

func TestSharedDirUploads_SweeperRemovesPartialsThroughSharedDirRoot(t *testing.T) {
	srv, st := testServer(t)
	sdSetLargeUploadsExperiment(t, srv, true)
	project, sdPaths := createTestProjectWithSharedDirs(t, srv, st, "SD Uploads Sweeper", "scratchpad")
	sdPath := sdPaths["scratchpad"]

	orig := []byte("existing-shared-dir-bytes")
	require.NoError(t, os.WriteFile(filepath.Join(sdPath, "sweep.bin"), orig, 0o644))

	uploadsURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads", project.ID)
	startRec := doRequest(t, srv, http.MethodPost, uploadsURL,
		UploadStartRequest{Name: "sweep.bin", Path: "sweep.bin", Size: 20, Overwrite: true})
	require.Equal(t, http.StatusCreated, startRec.Code)
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	u := fmt.Sprintf("%s/%s", uploadsURL, started.ID)
	require.Equal(t, http.StatusOK, sdDoChunkPut(t, srv, "", u+"?offset=0", []byte("0123456789")).Code)

	partialPath := filepath.Join(sdPath, uploadPartialName(started.ID))
	_, err := os.Stat(partialPath)
	require.NoError(t, err)

	// Turn experiment OFF: sweeper must still clean up expired sessions and partials.
	sdSetLargeUploadsExperiment(t, srv, false)

	// Sweep at now + 2h -> transitions uploading -> expired and removes partial through *os.Root.
	future := time.Now().UTC().Add(2 * time.Hour)
	srv.sweepUploadSessions(context.Background(), future)

	sess, err := st.GetUploadSession(context.Background(), started.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UploadStateExpired, sess.State)
	_, err = os.Stat(partialPath)
	assert.True(t, os.IsNotExist(err), "sweeper should remove shared-dir partial on expiry")

	// Existing target file is still byte-identical.
	got, err := os.ReadFile(filepath.Join(sdPath, "sweep.bin"))
	require.NoError(t, err)
	assert.Equal(t, orig, got)

	// Sweep again past 15m terminal retention -> deletes the session row.
	srv.sweepUploadSessions(context.Background(), time.Now().UTC().Add(20*time.Minute))
	_, err = st.GetUploadSession(context.Background(), started.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "sweeper should delete expired row past retention window")
}

func TestSharedDirUploads_WriteGateRevocationStopsNextChunk(t *testing.T) {
	srv, st := testServer(t)
	sdSetLargeUploadsExperiment(t, srv, true)
	project, _ := createTestProjectWithSharedDirs(t, srv, st, "SD Uploads WriteGate", "scratchpad")
	uploadsURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads", project.ID)

	// Start and upload chunk 1 while writes are allowed.
	startRec := doRequest(t, srv, http.MethodPost, uploadsURL,
		UploadStartRequest{Name: "gated.bin", Path: "gated.bin", Size: 16, Overwrite: true})
	require.Equal(t, http.StatusCreated, startRec.Code, "body: %s", startRec.Body.String())
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	u := fmt.Sprintf("%s/%s", uploadsURL, started.ID)
	require.Equal(t, http.StatusOK, sdDoChunkPut(t, srv, "", u+"?offset=0", []byte("01234567")).Code)

	// Activate Cloud Run non-durable write gate: shared-dir writes are now refused.
	t.Setenv("K_SERVICE", "scion-hub")
	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{Backend: "local"}

	// Next chunk is refused with 503 shared_dir_writes_unavailable.
	c2 := sdDoChunkPut(t, srv, "", u+"?offset=8", []byte("89abcdef"))
	require.Equal(t, http.StatusServiceUnavailable, c2.Code, "body: %s", c2.Body.String())
	assert.Equal(t, "shared_dir_writes_unavailable", sdDecodeUploadAPIError(t, c2).Code)

	// Complete and abort are also refused as writes.
	comp := doRequest(t, srv, http.MethodPost, u+"/complete", nil)
	assert.Equal(t, http.StatusServiceUnavailable, comp.Code, "body: %s", comp.Body.String())
	abort := doRequest(t, srv, http.MethodDelete, u, nil)
	assert.Equal(t, http.StatusServiceUnavailable, abort.Code, "body: %s", abort.Body.String())

	// Status GET is a read and remains allowed.
	statusRec := doRequest(t, srv, http.MethodGet, u, nil)
	assert.Equal(t, http.StatusOK, statusRec.Code, "body: %s", statusRec.Body.String())
}

type sdProviderFaultStore struct {
	store.Store
	fault *storeFaultSwitch
	fail  atomic.Bool
}

func (w *sdProviderFaultStore) GetProjectProviders(ctx context.Context, projectID string) ([]store.ProjectProvider, error) {
	if w.fault.Active() && w.fail.Load() {
		return nil, errors.New("injected transient GetProjectProviders error")
	}
	return w.Store.GetProjectProviders(ctx, projectID)
}

func TestSharedDirUploads_SweeperRetriesOnGetProjectProvidersFault(t *testing.T) {
	srv, st, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *sdProviderFaultStore {
		return &sdProviderFaultStore{Store: inner, fault: f}
	})
	ctx := context.Background()

	proj := &store.Project{
		ID:         tid("proj-sd-sweep-fault"),
		Slug:       "proj-sd-sweep-fault",
		Name:       "SD Sweep Fault",
		GitRemote:  "https://github.com/example/repo.git",
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	}
	require.NoError(t, st.CreateProject(ctx, proj))

	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := &store.UploadSession{
		ID:             "upl_sd_provider_fault",
		HubID:          "local",
		OwnerKind:      "user",
		OwnerID:        "u1",
		TargetKind:     store.UploadTargetSharedDir,
		ScopeProjectID: proj.ID,
		ScopeSharedDir: "scratchpad",
		DestPath:       "orphan.bin",
		Overwrite:      true,
		FileName:       "orphan.bin",
		MediaType:      "application/octet-stream",
		DeclaredSize:   1024,
		StagingRef:     uploadPartialName("upl_sd_provider_fault"),
		IdleExpiresAt:  now.Add(time.Hour),
		HardExpiresAt:  now.Add(24 * time.Hour),
	}
	require.NoError(t, st.CreateUploadSession(ctx, sess, store.UploadQuota{}))
	require.NoError(t, st.TransitionUploadSession(ctx, sess.ID, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateAborted, "", nil))

	// With a transient GetProjectProviders fault, sweeping past the 15m terminal
	// retention window must keep the row (backing it off) rather than deleting it.
	wrapped.fail.Store(true)
	fault.Arm()
	srv.sweepUploadSessions(ctx, now.Add(20*time.Minute))

	kept, err := st.GetUploadSession(ctx, sess.ID)
	require.NoError(t, err, "terminal row must be retained when GetProjectProviders fails")
	assert.Equal(t, store.UploadStateAborted, kept.State)

	// Once the fault clears, resolveSharedDirPath returns
	// errSharedDirRequiresColocatedBroker (nothing to clean on this hub), so the
	// next sweep past retention deletes the terminal row.
	wrapped.fail.Store(false)
	srv.sweepUploadSessions(ctx, now.Add(40*time.Minute))

	_, err = st.GetUploadSession(ctx, sess.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "terminal row should be deleted once resolve returns co-located broker sentinel")
}

func TestSharedDirUploads_RoleRevocationStopsNextChunk(t *testing.T) {
	srv, st := testServer(t)
	sdSetLargeUploadsExperiment(t, srv, true)
	ctx := context.Background()

	member := &store.User{
		ID:          tid("user-sdupload-revoked"),
		Email:       "sdupload-revoked@test.com",
		DisplayName: "SD Upload Revoked",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, member))

	project, _ := createTestProjectWithSharedDirs(t, srv, st, "SD Uploads Role Revoke", "scratchpad")
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, member.ID))

	uploadsURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratchpad/uploads", project.ID)
	startRec := doRequestAsUser(t, srv, member, http.MethodPost, uploadsURL,
		UploadStartRequest{Name: "revoke.bin", Path: "revoke.bin", Size: 16, Overwrite: true})
	require.Equal(t, http.StatusCreated, startRec.Code, "body: %s", startRec.Body.String())
	var started UploadStartResponse
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))

	memberToken, _, _, err := srv.userTokenService.GenerateTokenPair(
		member.ID, member.Email, member.DisplayName, member.Role, ClientTypeWeb)
	require.NoError(t, err)

	u := fmt.Sprintf("%s/%s", uploadsURL, started.ID)
	c1 := sdDoChunkPut(t, srv, memberToken, u+"?offset=0", []byte("01234567"))
	require.Equal(t, http.StatusOK, c1.Code, "body: %s", c1.Body.String())

	// Revoke the user's project role binding between chunks.
	_, err = st.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, member.ID)
	require.NoError(t, err)

	// Next chunk PUT must be rejected by route authorization (403 or 404).
	c2 := sdDoChunkPut(t, srv, memberToken, u+"?offset=8", []byte("89abcdef"))
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, c2.Code,
		"revoked project role must stop the next chunk, got %d: %s", c2.Code, c2.Body.String())
}
