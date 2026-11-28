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
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// slowHubBlobStorage delays Upload so tests can verify that hub artifact and
// attachment upload handlers extend the connection's write deadline through
// the full hub middleware stack (ptone/scion#9648).
type slowHubBlobStorage struct {
	storage.Storage
	delay time.Duration
}

func (s *slowHubBlobStorage) Upload(ctx context.Context, objectPath string, reader io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.Storage.Upload(ctx, objectPath, reader, opts)
}

// slowUploadBody drips bytes across a pause longer than the listener's
// ReadTimeout so tests prove SetReadDeadline reaches the underlying net.Conn
// through the hub middleware chain.
type slowUploadBody struct {
	data  []byte
	pos   int
	delay time.Duration
	slept bool
}

func (r *slowUploadBody) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if r.pos == 0 {
		p[0] = r.data[0]
		r.pos = 1
		return 1, nil
	}
	if !r.slept {
		r.slept = true
		time.Sleep(r.delay)
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestArtifactAndAttachmentUploads_ExtendDeadlinesThroughHubMiddleware verifies
// that POST /api/v1/artifacts?name=... and POST /api/v1/chat/attachments both
// extend read and write deadlines past the serving http.Server's ReadTimeout
// and WriteTimeout through the full hub middleware stack (ptone/scion#9648).
func TestArtifactAndAttachmentUploads_ExtendDeadlinesThroughHubMiddleware(t *testing.T) {
	const (
		serverTimeout = 200 * time.Millisecond
		phaseDelay    = 300 * time.Millisecond
	)

	srv, s := testServer(t)
	_, blobs := enableArtifactsForTest(t, srv)
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.Artifacts || e.Name == experiments.AttachmentArtifacts {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	srv.SetStorage(&slowHubBlobStorage{Storage: blobs, delay: phaseDelay})

	// Install web chat + attachment stores for POST /api/v1/chat/attachments.
	wcs, _ := newTestWebChatStoreV2(t)
	srv.SetWebChatStore(wcs)
	las, err := NewLocalDiskAttachmentStore(t.TempDir())
	require.NoError(t, err)
	srv.SetAttachmentStore(las)

	ctx := context.Background()
	owner := &store.User{
		ID:          uuid.NewString(),
		Email:       "deadline-owner@test.com",
		DisplayName: "Deadline Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	proj := &store.Project{
		ID:        uuid.NewString(),
		Name:      "Deadline Project",
		Slug:      "deadline-9648",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	srv.seedProjectCreatorMembership(ctx, proj)

	agent := &store.Agent{
		ID:        uuid.NewString(),
		ProjectID: proj.ID,
		Name:      "deadline-agent",
		Slug:      "deadline-agent",
		Phase:     "running",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	agentToken, err := srv.GenerateAgentToken(agent.ID, agent.ProjectID, agent.Ancestry, AgentRoleFull, nil)
	require.NoError(t, err)

	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Config.ReadTimeout = serverTimeout
	hs.Config.WriteTimeout = serverTimeout
	hs.Start()
	t.Cleanup(hs.Close)

	t.Run("artifact_single_file_publish", func(t *testing.T) {
		payload := []byte("hub-slow-artifact-publish-payload")
		url := hs.URL + "/api/v1/artifacts?name=report.txt&scope=" + proj.ID
		req, err := http.NewRequest(http.MethodPost, url, &slowUploadBody{data: payload, delay: phaseDelay})
		require.NoError(t, err)
		req.ContentLength = int64(len(payload))
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		req.Header.Set("Content-Type", "text/plain")

		start := time.Now()
		resp, err := hs.Client().Do(req)
		require.NoError(t, err, "publish must not fail at server ReadTimeout/WriteTimeout")
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
		require.GreaterOrEqual(t, time.Since(start), 2*phaseDelay,
			"fixture check: both read and blob-upload phases must exceed server timeouts")
	})

	t.Run("chat_attachment_upload", func(t *testing.T) {
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		part, err := mw.CreateFormFile("files", "notes.txt")
		require.NoError(t, err)
		_, err = part.Write([]byte("hub-slow-attachment-upload-payload"))
		require.NoError(t, err)
		require.NoError(t, mw.Close())

		raw := form.Bytes()
		req, err := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/chat/attachments", &slowUploadBody{data: raw, delay: phaseDelay})
		require.NoError(t, err)
		req.ContentLength = int64(len(raw))
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		req.Header.Set("Content-Type", mw.FormDataContentType())

		start := time.Now()
		resp, err := hs.Client().Do(req)
		require.NoError(t, err, "attachment upload must not fail at server ReadTimeout/WriteTimeout")
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
		require.GreaterOrEqual(t, time.Since(start), 2*phaseDelay,
			"fixture check: both read and blob-upload phases must exceed server timeouts")
	})

	t.Run("agent_chat_attachment_upload", func(t *testing.T) {
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		part, err := mw.CreateFormFile("files", "agent-notes.txt")
		require.NoError(t, err)
		_, err = part.Write([]byte("hub-slow-agent-attachment-upload-payload"))
		require.NoError(t, err)
		require.NoError(t, mw.Close())

		raw := form.Bytes()
		req, err := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/chat/attachments", &slowUploadBody{data: raw, delay: phaseDelay})
		require.NoError(t, err)
		req.ContentLength = int64(len(raw))
		req.Header.Set("Authorization", "Bearer "+agentToken)
		req.Header.Set("Content-Type", mw.FormDataContentType())

		start := time.Now()
		resp, err := hs.Client().Do(req)
		require.NoError(t, err, "agent attachment upload must not fail at server ReadTimeout/WriteTimeout")
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
		require.GreaterOrEqual(t, time.Since(start), 2*phaseDelay,
			"fixture check: both read and blob-upload phases must exceed server timeouts")
	})
}
