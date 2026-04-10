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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/require"
)

func newMockHubServerForLocalStorageHarnessConfig(t *testing.T, uploadedPaths *[]string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/api/v1/harness-configs" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"harnessConfig": map[string]interface{}{
					"id":      "local-storage-hc-id",
					"name":    "codex",
					"harness": "codex",
				},
			})

		case r.URL.Path == "/api/v1/harness-configs" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"harnessConfigs": []map[string]interface{}{},
			})

		case r.URL.Path == "/api/v1/harness-configs/local-storage-hc-id/upload" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"uploadUrls": []map[string]interface{}{
					{
						"path":   "config.yaml",
						"url":    "file:///home/scion/.scion/storage/harness-configs/global/codex/config.yaml",
						"method": "PUT",
					},
				},
			})

		case r.URL.Path == "/api/v1/harness-configs/local-storage-hc-id/files" && r.Method == http.MethodPost:
			require.NoError(t, r.ParseMultipartForm(10<<20))
			require.NotNil(t, r.MultipartForm)
			for field, headers := range r.MultipartForm.File {
				*uploadedPaths = append(*uploadedPaths, field)
				for _, fh := range headers {
					file, err := fh.Open()
					require.NoError(t, err)
					_, err = io.ReadAll(file)
					file.Close()
					require.NoError(t, err)
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]interface{}{
					{
						"path":    "config.yaml",
						"size":    16,
						"modTime": "2026-04-10T00:00:00Z",
						"mode":    "0644",
					},
				},
				"hash": "sha256:test-hash",
			})

		case r.URL.Path == "/api/v1/harness-configs/local-storage-hc-id/finalize" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":          "local-storage-hc-id",
				"name":        "codex",
				"harness":     "codex",
				"status":      "active",
				"contentHash": "sha256:abc123",
			})

		case r.URL.Path == "/api/v1/harness-configs/local-storage-hc-id/download" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]interface{}{
					{
						"path": "config.yaml",
						"hash": "sha256:download-hash",
						"url":  "file:///home/scion/.scion/storage/harness-configs/global/codex/config.yaml",
					},
				},
			})

		case r.URL.Path == "/api/v1/harness-configs/local-storage-hc-id/files/config.yaml" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"path":     "config.yaml",
				"content":  "harness: codex\n",
				"size":     16,
				"modTime":  "2026-04-10T00:00:00Z",
				"encoding": "utf-8",
				"hash":     "sha256:download-hash",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestPullHarnessConfigFromHub_FallsBackToHubFileAPIForLocalStorageURLs(t *testing.T) {
	tmpHome := t.TempDir()
	var uploadedPaths []string

	server := newMockHubServerForLocalStorageHarnessConfig(t, &uploadedPaths)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
	}

	hc := &hubclient.HarnessConfig{
		ID:      "local-storage-hc-id",
		Name:    "codex",
		Harness: "codex",
	}

	destPath := filepath.Join(tmpHome, "pulled-harness-config")
	err = pullHarnessConfigFromHub(hubCtx, hc, destPath)
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(destPath, "config.yaml"))
	require.NoError(t, err)
	require.Equal(t, "harness: codex\n", string(content))
}

func TestSyncHarnessConfigToHub_FallsBackToHubFileAPIForLocalStorageURLs(t *testing.T) {
	localPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "config.yaml"), []byte("harness: codex\n"), 0644))

	var uploadedPaths []string
	server := newMockHubServerForLocalStorageHarnessConfig(t, &uploadedPaths)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
	}

	err = syncHarnessConfigToHub(hubCtx, "codex", localPath, "global", "codex")
	require.NoError(t, err)
	require.Contains(t, uploadedPaths, "config.yaml")
}
