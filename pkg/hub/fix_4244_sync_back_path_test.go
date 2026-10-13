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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#4244: the hub-managed sync-back after a sync-from downloads
// exactly what the sync-from upload wrote. The fake broker writes the
// agent's files under the upload request's StoragePath; the fake
// downloader copies whatever storage holds under the prefix it is given
// into the hub workspace. Stale content at the project-level path must
// not be read.
func TestWorkspaceSyncFrom_SyncBackReadsWhatUploadWrote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, s, project := setupCreateAgentServer(t, &slowLaunchDispatcher{})
	shortenSyncDispatchTimeout(t, 3*time.Second)
	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)

	// Stale content at the project-level path, which the sync-from
	// upload never writes.
	staleObj := storage.ProjectWorkspaceStoragePath(srv.HubID(), project.ID) + "/files/notes.txt"
	stor.content[staleObj] = []byte("stale")
	stor.objects[staleObj] = &storage.Object{Name: staleObj, Size: 5}

	var downloadPrefix string
	srv.setHubWorkspaceDownloader(func(_ context.Context, _, prefix, localPath string) error {
		downloadPrefix = prefix
		for name, data := range stor.content {
			rel, ok := strings.CutPrefix(name, prefix+"/")
			if !ok {
				continue
			}
			dst := filepath.Join(localPath, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return err
			}
		}
		return nil
	})

	agent := createSiteAgent(t, s, project, "sync-back-path", state.PhaseRunning, store.RunIntentRunning)
	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	agentFiles := map[string]string{"notes.txt": "fresh from agent", "src/main.go": "package main\n"}
	uploads := make(chan string, 1)
	go func() {
		for {
			var env wsprotocol.RequestEnvelope
			if err := broker.ws.ReadJSON(&env); err != nil {
				return
			}
			if env.Type != wsprotocol.TypeRequest {
				continue
			}
			var req RuntimeBrokerWorkspaceUploadRequest
			_ = json.Unmarshal(env.Body, &req)
			manifest := &transfer.Manifest{Version: "1.0"}
			for p, c := range agentFiles {
				obj := req.StoragePath + "/files/" + p
				stor.content[obj] = []byte(c)
				stor.objects[obj] = &storage.Object{Name: obj, Size: int64(len(c))}
				manifest.Files = append(manifest.Files, transfer.FileInfo{Path: p, Size: int64(len(c))})
			}
			uploads <- req.StoragePath
			body, _ := json.Marshal(RuntimeBrokerWorkspaceUploadResponse{Manifest: manifest})
			_ = broker.ws.WriteJSON(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"}, body))
			return
		}
	}()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-from", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var uploadPath string
	select {
	case uploadPath = <-uploads:
	default:
		t.Fatal("fixture check: the upload never reached the broker")
	}
	assert.Equal(t, storage.WorkspaceStoragePath(srv.HubID(), project.ID, agent.ID), uploadPath,
		"sync-from uploads to the agent's workspace path")
	assert.Equal(t, uploadPath+"/files", downloadPrefix, "the sync-back reads the path the upload wrote")

	workspacePath, err := srv.hubManagedProjectPath(project.Slug)
	require.NoError(t, err)
	for p, want := range agentFiles {
		got, err := os.ReadFile(filepath.Join(workspacePath, filepath.FromSlash(p)))
		require.NoError(t, err, p)
		assert.Equal(t, want, string(got), p)
	}
}
