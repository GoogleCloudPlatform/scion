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
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/require"
)

// configYAMLHashCodex is the SHA-256 hash of the canonical "harness: codex\n"
// payload used by the local-storage hub mocks. Phase 3 verifies this hash
// during pull, so the mock server must announce a matching value.
var configYAMLHashCodex = transfer.HashBytes([]byte("harness: codex\n"))

func newMockHubServerForLocalStorageHarnessConfig(t *testing.T, uploadedPaths *[]string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/api/v1/harness-configs" && r.Method == http.MethodPost:
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"harnessConfig": map[string]interface{}{
					"id":      "local-storage-hc-id",
					"name":    "codex",
					"harness": "codex",
				},
			}))

		case r.URL.Path == "/api/v1/harness-configs" && r.Method == http.MethodGet:
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"harnessConfigs": []map[string]interface{}{},
			}))

		case r.URL.Path == "/api/v1/harness-configs/local-storage-hc-id/upload" && r.Method == http.MethodPost:
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"uploadUrls": []map[string]interface{}{
					{
						"path":   "config.yaml",
						"url":    "file:///home/scion/.scion/storage/harness-configs/global/codex/config.yaml",
						"method": "PUT",
					},
				},
			}))

		case r.URL.Path == "/api/v1/harness-configs/local-storage-hc-id/files" && r.Method == http.MethodPost:
			require.NoError(t, r.ParseMultipartForm(10<<20))
			require.NotNil(t, r.MultipartForm)
			for field, headers := range r.MultipartForm.File {
				*uploadedPaths = append(*uploadedPaths, field)
				for _, fh := range headers {
					file, err := fh.Open()
					require.NoError(t, err)
					_, err = io.ReadAll(file)
					require.NoError(t, err)
					require.NoError(t, file.Close())
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":          "local-storage-hc-id",
				"name":        "codex",
				"harness":     "codex",
				"status":      "active",
				"contentHash": "sha256:abc123",
			})

		case r.URL.Path == "/api/v1/harness-configs/local-storage-hc-id/download" && r.Method == http.MethodGet:
			// Use the real SHA-256 of "harness: codex\n" so Phase 3 hash
			// validation passes. Hard-coded value computed via:
			//   echo -n "harness: codex\n" | sha256sum
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]interface{}{
					{
						"path": "config.yaml",
						"hash": configYAMLHashCodex,
						"url":  "file:///home/scion/.scion/storage/harness-configs/global/codex/config.yaml",
					},
				},
			})

		case r.URL.Path == "/api/v1/harness-configs/local-storage-hc-id/files/config.yaml" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"path":     "config.yaml",
				"content":  "harness: codex\n",
				"size":     16,
				"modTime":  "2026-04-10T00:00:00Z",
				"encoding": "utf-8",
				"hash":     configYAMLHashCodex,
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

	err = syncHarnessConfigToHub(hubCtx, "codex", localPath, "global", "", "codex")
	require.NoError(t, err)
	require.Contains(t, uploadedPaths, "config.yaml")
}

func TestSyncHarnessConfigToHub_SkipsBackupAndTempFiles(t *testing.T) {
	localPath := t.TempDir()
	write := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(localPath, name), []byte(content), 0644))
	}
	write("config.yaml", "harness: codex\n")
	write("dialect.yaml", "dialect: codex\n")
	write("config.yaml.bak.20261003T193320Z", "old\n")
	write("provision.py.bak.20261003T193320Z", "old\n")
	write(".provision.py.tmp-123456", "partial\n")

	var uploadedPaths []string
	server := newMockHubServerForLocalStorageHarnessConfig(t, &uploadedPaths)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	require.NoError(t, syncHarnessConfigToHub(hubCtx, "codex", localPath, "global", "", "codex"))
	require.ElementsMatch(t, []string{"config.yaml", "dialect.yaml"}, uploadedPaths)
}

// existingHarnessConfigCalls records which mutating calls a sync made against
// newMockHubServerForExistingHarnessConfig.
type existingHarnessConfigCalls struct {
	uploadRequests  int
	uploadRequested []string
	finalized       *hubclient.HarnessConfigManifest
}

// newMockHubServerForExistingHarnessConfig serves an existing "codex"
// harness-config whose stored files are remoteFiles (path -> hash).
func newMockHubServerForExistingHarnessConfig(t *testing.T, remoteFiles map[string]string, calls *existingHarnessConfigCalls) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/api/v1/harness-configs" && r.Method == http.MethodGet:
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"harnessConfigs": []map[string]interface{}{{
					"id":          "existing-hc-id",
					"name":        "codex",
					"harness":     "codex",
					"status":      "active",
					"contentHash": "sha256:old",
				}},
			}))

		case r.URL.Path == "/api/v1/harness-configs/existing-hc-id/download" && r.Method == http.MethodGet:
			var files []map[string]interface{}
			for path, hash := range remoteFiles {
				files = append(files, map[string]interface{}{
					"path": path,
					"hash": hash,
					"url":  "file:///storage/harness-configs/global/codex/" + path,
				})
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"files": files}))

		case r.URL.Path == "/api/v1/harness-configs/existing-hc-id/upload" && r.Method == http.MethodPost:
			calls.uploadRequests++
			var req struct {
				Files []hubclient.FileUploadRequest `json:"files"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			for _, f := range req.Files {
				calls.uploadRequested = append(calls.uploadRequested, f.Path)
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []map[string]interface{}{}}))

		case r.URL.Path == "/api/v1/harness-configs/existing-hc-id/finalize" && r.Method == http.MethodPost:
			var req hubclient.HarnessConfigFinalizeRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			calls.finalized = req.Manifest
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"id":          "existing-hc-id",
				"name":        "codex",
				"harness":     "codex",
				"status":      "active",
				"contentHash": "sha256:new",
			}))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestSyncHarnessConfigToHub_DropsStaleBackupEntriesFromHub(t *testing.T) {
	localPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "config.yaml"), []byte("harness: codex\n"), 0644))
	// The backup is still on disk locally, but excluded from sync.
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "config.yaml.bak.20261003T193320Z"), []byte("old\n"), 0644))

	var calls existingHarnessConfigCalls
	server := newMockHubServerForExistingHarnessConfig(t, map[string]string{
		"config.yaml":                      configYAMLHashCodex,
		"config.yaml.bak.20261003T193320Z": transfer.HashBytes([]byte("old\n")),
	}, &calls)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	require.NoError(t, syncHarnessConfigToHub(hubCtx, "codex", localPath, "global", "", "codex"))

	require.Equal(t, 0, calls.uploadRequests, "nothing changed, so nothing should be uploaded")
	require.NotNil(t, calls.finalized, "stale backup entry must trigger Finalize")
	var paths []string
	for _, f := range calls.finalized.Files {
		paths = append(paths, f.Path)
	}
	require.Equal(t, []string{"config.yaml"}, paths)
}

// finalizedPaths returns the file paths of the manifest a sync finalized.
func finalizedPaths(t *testing.T, calls *existingHarnessConfigCalls) []string {
	t.Helper()
	require.NotNil(t, calls.finalized, "sync must Finalize")
	var paths []string
	for _, f := range calls.finalized.Files {
		paths = append(paths, f.Path)
	}
	return paths
}

// TestSyncHarnessConfigToHub_UpToDateDoesNotFinalize guards the true no-op
// path: when the local files match the Hub record exactly, sync neither
// uploads nor finalizes.
func TestSyncHarnessConfigToHub_UpToDateDoesNotFinalize(t *testing.T) {
	localPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "config.yaml"), []byte("harness: codex\n"), 0644))
	// A local backup is excluded from sync, so it does not make the
	// local manifest differ from the remote one.
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "config.yaml.bak.20261003T193320Z"), []byte("old\n"), 0644))

	var calls existingHarnessConfigCalls
	server := newMockHubServerForExistingHarnessConfig(t, map[string]string{
		"config.yaml": configYAMLHashCodex,
	}, &calls)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	out := captureStdout(t, func() {
		require.NoError(t, syncHarnessConfigToHub(hubCtx, "codex", localPath, "global", "", "codex"))
	})

	require.Equal(t, 0, calls.uploadRequests)
	require.Nil(t, calls.finalized, "up-to-date config must not be finalized")
	require.Contains(t, out, "already up to date")
}

// TestSyncHarnessConfigToHub_DropsLocallyDeletedFileFromHub verifies that a
// non-transient file deleted locally is removed from the Hub record: sync
// finalizes with exactly the local manifest, without an upload request, and
// names the removed file.
func TestSyncHarnessConfigToHub_DropsLocallyDeletedFileFromHub(t *testing.T) {
	localPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "config.yaml"), []byte("harness: codex\n"), 0644))

	var calls existingHarnessConfigCalls
	server := newMockHubServerForExistingHarnessConfig(t, map[string]string{
		"config.yaml":          configYAMLHashCodex,
		"extra.yaml":           transfer.HashBytes([]byte("extra\n")),
		"scripts/provision.py": transfer.HashBytes([]byte("print()\n")),
	}, &calls)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	out := captureStdout(t, func() {
		require.NoError(t, syncHarnessConfigToHub(hubCtx, "codex", localPath, "global", "", "codex"))
	})

	require.Equal(t, 0, calls.uploadRequests, "nothing changed locally, so nothing should be uploaded")
	require.Equal(t, []string{"config.yaml"}, finalizedPaths(t, &calls))
	require.NotContains(t, out, "already up to date")
	require.Contains(t, out, "  - extra.yaml\n")
	require.Contains(t, out, "  - scripts/provision.py\n")
}

// TestSyncHarnessConfigToHub_DeletionWithChangedFile verifies that when a
// file is deleted and another changed, only the changed file is uploaded, the
// finalized manifest is the local one, and the removal is reported.
func TestSyncHarnessConfigToHub_DeletionWithChangedFile(t *testing.T) {
	localPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "config.yaml"), []byte("harness: codex\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "dialect.yaml"), []byte("dialect: new\n"), 0644))

	var calls existingHarnessConfigCalls
	server := newMockHubServerForExistingHarnessConfig(t, map[string]string{
		"config.yaml":  configYAMLHashCodex,
		"dialect.yaml": transfer.HashBytes([]byte("dialect: old\n")),
		"extra.yaml":   transfer.HashBytes([]byte("extra\n")),
	}, &calls)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	out := captureStdout(t, func() {
		require.NoError(t, syncHarnessConfigToHub(hubCtx, "codex", localPath, "global", "", "codex"))
	})

	require.Equal(t, []string{"dialect.yaml"}, calls.uploadRequested)
	require.ElementsMatch(t, []string{"config.yaml", "dialect.yaml"}, finalizedPaths(t, &calls))
	require.Contains(t, out, "  - extra.yaml\n", "the removed file must be named even when other files changed")
}

// TestSyncHarnessConfigToHub_DropsStaleTransientDirectoryFromHub covers a
// transient directory uploaded before transient names were excluded: its
// contents do not have a transient basename, but they are not in the local
// manifest, so sync drops them.
func TestSyncHarnessConfigToHub_DropsStaleTransientDirectoryFromHub(t *testing.T) {
	localPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "config.yaml"), []byte("harness: codex\n"), 0644))
	// The transient directory still exists locally but is excluded from sync.
	backupDir := filepath.Join(localPath, "scripts.bak.20261003T193320Z")
	require.NoError(t, os.MkdirAll(backupDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, "provision.py"), []byte("old\n"), 0644))

	var calls existingHarnessConfigCalls
	server := newMockHubServerForExistingHarnessConfig(t, map[string]string{
		"config.yaml": configYAMLHashCodex,
		"scripts.bak.20261003T193320Z/provision.py": transfer.HashBytes([]byte("old\n")),
	}, &calls)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	require.NoError(t, syncHarnessConfigToHub(hubCtx, "codex", localPath, "global", "", "codex"))

	require.Equal(t, 0, calls.uploadRequests)
	require.Equal(t, []string{"config.yaml"}, finalizedPaths(t, &calls))
}

// TestSyncHarnessConfigToHub_RefusesEmptyLocalDirectory verifies that sync
// refuses a directory with no syncable files (only transient ones here) with
// a clear error, before making any Hub call.
func TestSyncHarnessConfigToHub_RefusesEmptyLocalDirectory(t *testing.T) {
	localPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(localPath, "config.yaml.bak.20261003T193320Z"), []byte("old\n"), 0644))

	hubCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubCalls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	err = syncHarnessConfigToHub(hubCtx, "codex", localPath, "global", "", "codex")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no files to sync")
	require.Equal(t, 0, hubCalls, "sync must not call the Hub for an empty directory")
}
