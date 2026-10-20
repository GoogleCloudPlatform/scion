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

package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uploadIDHub is a mock Hub for one existing "codex" harness-config whose
// stored config.yaml differs from the local one. Each upload-URL request
// returns the next ID in uploadIDs (none once they run out, like a Hub that
// predates upload IDs) and URLs that PUT back to this server. missingOnce
// makes the first finalize answer "file not found".
type uploadIDHub struct {
	uploadIDs   []string
	missingOnce bool

	mu        sync.Mutex
	puts      []string // "<uploadId>:<path>" per staged PUT
	finalized []string // uploadId of each finalize body
}

func (h *uploadIDHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/harness-configs" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"harnessConfigs": []map[string]interface{}{{
					"id": "hc-1", "name": "codex", "harness": "codex", "status": "active", "contentHash": "sha256:old",
				}},
			})
		case r.URL.Path == "/api/v1/harness-configs/hc-1/download" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"files": []map[string]interface{}{{
				"path": "config.yaml", "hash": "sha256:" + strings.Repeat("0", 64), "url": srv.URL + "/blob",
			}}})
		case r.URL.Path == "/api/v1/harness-configs/hc-1/upload" && r.Method == http.MethodPost:
			var req struct {
				Files []hubclient.FileUploadRequest `json:"files"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			id := ""
			if len(h.uploadIDs) > 0 {
				id, h.uploadIDs = h.uploadIDs[0], h.uploadIDs[1:]
			}
			var urls []map[string]interface{}
			for _, f := range req.Files {
				u := srv.URL + "/api/v1/harness-configs/hc-1/files/" + f.Path
				if id != "" {
					u += "?uploadId=" + id
				}
				urls = append(urls, map[string]interface{}{"path": f.Path, "url": u, "method": http.MethodPut})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": urls, "uploadId": id})
		case strings.HasPrefix(r.URL.Path, "/api/v1/harness-configs/hc-1/files/") && r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			h.puts = append(h.puts, r.URL.Query().Get("uploadId")+":"+strings.TrimPrefix(r.URL.Path, "/api/v1/harness-configs/hc-1/files/"))
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/v1/harness-configs/hc-1/finalize" && r.Method == http.MethodPost:
			var req hubclient.HarnessConfigFinalizeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			h.finalized = append(h.finalized, req.UploadID)
			if h.missingOnce {
				h.missingOnce = false
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{
					"code": "validation_error", "message": "file not found: config.yaml",
				}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "hc-1", "name": "codex", "harness": "codex", "status": "active", "contentHash": "sha256:new",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runUploadIDSync(t *testing.T, h *uploadIDHub) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("harness: codex\n"), 0o644))
	srv := h.server(t)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	require.NoError(t, syncHarnessConfigToHub(&HubContext{Client: client, Endpoint: srv.URL}, "codex", dir, "global", "", "codex", ""))
}

// TestSyncHarnessConfigToHub_SendsUploadID: harness-config sync stages its
// files through the upload URLs and sends the upload response's uploadId
// back in the finalize, so the Hub commits exactly that upload
// (ptone/scion#4219).
func TestSyncHarnessConfigToHub_SendsUploadID(t *testing.T) {
	h := &uploadIDHub{uploadIDs: []string{"up-1"}}
	runUploadIDSync(t, h)
	assert.Equal(t, []string{"up-1:config.yaml"}, h.puts)
	assert.Equal(t, []string{"up-1"}, h.finalized)
}

// TestSyncHarnessConfigToHub_OldHubWithoutUploadID: a Hub that predates
// upload IDs returns none, and the finalize then carries none (the Hub
// matches staged files by hash).
func TestSyncHarnessConfigToHub_OldHubWithoutUploadID(t *testing.T) {
	h := &uploadIDHub{}
	runUploadIDSync(t, h)
	assert.Equal(t, []string{":config.yaml"}, h.puts)
	assert.Equal(t, []string{""}, h.finalized)
}

// TestSyncHarnessConfigToHub_MissingFileRetryUsesNewUploadID: on "file not
// found" the sync re-uploads every file under a fresh upload and finalizes
// with that upload's ID.
func TestSyncHarnessConfigToHub_MissingFileRetryUsesNewUploadID(t *testing.T) {
	h := &uploadIDHub{uploadIDs: []string{"up-1", "up-2"}, missingOnce: true}
	runUploadIDSync(t, h)
	assert.Equal(t, []string{"up-1:config.yaml", "up-2:config.yaml"}, h.puts)
	assert.Equal(t, []string{"up-1", "up-2"}, h.finalized)
}
