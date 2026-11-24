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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// existingTemplateCalls records which mutating calls a sync made against
// newMockHubServerForExistingTemplate.
type existingTemplateCalls struct {
	uploadRequests  int
	uploadRequested []string
	finalized       *hubclient.TemplateManifest
}

// newMockHubServerForExistingTemplate serves an existing global "base"
// template. When remoteFiles is nil, the download endpoint answers with the
// Hub's "has no files" validation error; otherwise it lists remoteFiles
// (path -> hash) as the stored files.
func newMockHubServerForExistingTemplate(t *testing.T, remoteFiles map[string]string, calls *existingTemplateCalls) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"templates": []map[string]interface{}{{
					"id":          "existing-tpl-id",
					"name":        "base",
					"scope":       "global",
					"status":      "active",
					"contentHash": "sha256:old",
				}},
			}))

		case r.URL.Path == "/api/v1/templates/existing-tpl-id/download" && r.Method == http.MethodGet:
			if remoteFiles == nil {
				// Mirrors the Hub's message, which embeds the template name and ID.
				w.WriteHeader(http.StatusBadRequest)
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    "validation_error",
						"message": "template base (existing-tpl-id) has no files — sync template files first with: scion template sync base",
					},
				}))
				return
			}
			var files []map[string]interface{}
			for path, hash := range remoteFiles {
				files = append(files, map[string]interface{}{
					"path": path,
					"hash": hash,
					"url":  "file:///storage/templates/global/base/" + path,
				})
			}
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"files": files}))

		case r.URL.Path == "/api/v1/templates/existing-tpl-id/upload" && r.Method == http.MethodPost:
			calls.uploadRequests++
			var req struct {
				Files []hubclient.FileUploadRequest `json:"files"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			for _, f := range req.Files {
				calls.uploadRequested = append(calls.uploadRequested, f.Path)
			}
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []map[string]interface{}{}}))

		case r.URL.Path == "/api/v1/templates/existing-tpl-id/finalize" && r.Method == http.MethodPost:
			var req hubclient.FinalizeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			calls.finalized = req.Manifest
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
				"id":          "existing-tpl-id",
				"name":        "base",
				"status":      "active",
				"contentHash": "sha256:new",
			}))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTemplateSyncHubCtx(t *testing.T, server *httptest.Server) *HubContext {
	t.Helper()
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: server.URL}
}

func writeTemplateFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0644))
}

func TestIsTemplateNoFilesError(t *testing.T) {
	noFiles := &apiclient.APIError{
		StatusCode: http.StatusBadRequest,
		Code:       apiclient.ErrCodeValidationError,
		Message:    "template base (abc-123) has no files — sync template files first with: scion template sync base",
	}
	require.False(t, isTemplateNoFilesError(nil))
	require.True(t, isTemplateNoFilesError(noFiles))
	require.True(t, isTemplateNoFilesError(fmt.Errorf("wrapped: %w", noFiles)))
	// Another validation error, a different code, or a plain error with the
	// same text are not the no-files rejection.
	require.False(t, isTemplateNoFilesError(&apiclient.APIError{Code: apiclient.ErrCodeValidationError, Message: "invalid template name"}))
	require.False(t, isTemplateNoFilesError(&apiclient.APIError{Code: "not_found", Message: "template base has no files"}))
	require.False(t, isTemplateNoFilesError(errors.New("template base (abc-123) has no files")))
}

// TestSyncTemplateToHub_NoFilesTemplateUploadsAllFiles verifies the 0-file
// recovery path (ptone/scion#2081): when the Hub reports that the existing
// template record has no files, using its real message with the template
// name and ID embedded, sync uploads every local file and finalizes.
func TestSyncTemplateToHub_NoFilesTemplateUploadsAllFiles(t *testing.T) {
	localPath := t.TempDir()
	writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")
	writeTemplateFile(t, localPath, "home/.bashrc", "# rc\n")

	var calls existingTemplateCalls
	server := newMockHubServerForExistingTemplate(t, nil, &calls)
	defer server.Close()

	out := captureStdout(t, func() {
		require.NoError(t, syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude"))
	})

	require.Contains(t, out, "exists but has no files")
	require.ElementsMatch(t, []string{"scion-agent.yaml", "home/.bashrc"}, calls.uploadRequested)
	require.NotNil(t, calls.finalized, "sync must finalize after the full upload")
	require.Len(t, calls.finalized.Files, 2)
}

// templateFinalizedPaths returns the file paths of the manifest a sync finalized.
func templateFinalizedPaths(t *testing.T, calls *existingTemplateCalls) []string {
	t.Helper()
	require.NotNil(t, calls.finalized, "sync must Finalize")
	var paths []string
	for _, f := range calls.finalized.Files {
		paths = append(paths, f.Path)
	}
	return paths
}

// TestSyncTemplateToHub_UpToDateDoesNotFinalize guards the no-op path: when
// the local files match the Hub record exactly, sync neither uploads nor
// finalizes.
func TestSyncTemplateToHub_UpToDateDoesNotFinalize(t *testing.T) {
	localPath := t.TempDir()
	writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")

	var calls existingTemplateCalls
	server := newMockHubServerForExistingTemplate(t, map[string]string{
		"scion-agent.yaml": transfer.HashBytes([]byte("harness: claude\n")),
	}, &calls)
	defer server.Close()

	out := captureStdout(t, func() {
		require.NoError(t, syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude"))
	})

	require.Equal(t, 0, calls.uploadRequests)
	require.Nil(t, calls.finalized, "up-to-date template must not be finalized")
	require.Contains(t, out, "already up to date")
}

// TestSyncTemplateToHub_DropsLocallyDeletedFileFromHub verifies that files
// deleted locally are removed from the Hub record (ptone/scion#3163): sync
// finalizes with exactly the local manifest, without an upload request, and
// names the removed files.
func TestSyncTemplateToHub_DropsLocallyDeletedFileFromHub(t *testing.T) {
	localPath := t.TempDir()
	writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")

	var calls existingTemplateCalls
	server := newMockHubServerForExistingTemplate(t, map[string]string{
		"scion-agent.yaml": transfer.HashBytes([]byte("harness: claude\n")),
		"agents.md":        transfer.HashBytes([]byte("# old\n")),
		"home/.bashrc":     transfer.HashBytes([]byte("# rc\n")),
	}, &calls)
	defer server.Close()

	out := captureStdout(t, func() {
		require.NoError(t, syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude"))
	})

	require.Equal(t, 0, calls.uploadRequests, "nothing changed locally, so nothing should be uploaded")
	require.Equal(t, []string{"scion-agent.yaml"}, templateFinalizedPaths(t, &calls))
	require.NotContains(t, out, "already up to date")
	require.Contains(t, out, "Removing 2 file(s) no longer present locally from the Hub:\n  - agents.md\n  - home/.bashrc\n")
}

// TestSyncTemplateToHub_DeletionWithChangedFile verifies that when one file
// is deleted and another changed, only the changed file is uploaded, the
// finalized manifest is the local one, and the removal is reported.
func TestSyncTemplateToHub_DeletionWithChangedFile(t *testing.T) {
	localPath := t.TempDir()
	writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")
	writeTemplateFile(t, localPath, "agents.md", "# new\n")

	var calls existingTemplateCalls
	server := newMockHubServerForExistingTemplate(t, map[string]string{
		"scion-agent.yaml": transfer.HashBytes([]byte("harness: claude\n")),
		"agents.md":        transfer.HashBytes([]byte("# old\n")),
		"extra.md":         transfer.HashBytes([]byte("extra\n")),
	}, &calls)
	defer server.Close()

	out := captureStdout(t, func() {
		require.NoError(t, syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude"))
	})

	require.Equal(t, []string{"agents.md"}, calls.uploadRequested)
	require.ElementsMatch(t, []string{"scion-agent.yaml", "agents.md"}, templateFinalizedPaths(t, &calls))
	require.Contains(t, out, "  - extra.md\n", "the removed file must be named even when other files changed")
}

// TestSyncTemplateToHub_RefusesEmptyLocalDirectory verifies that sync refuses
// a directory with no files before making any Hub call, since mirroring it
// would empty the Hub record.
func TestSyncTemplateToHub_RefusesEmptyLocalDirectory(t *testing.T) {
	localPath := t.TempDir()

	hubCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubCalls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	err := syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no files to sync")
	require.Equal(t, 0, hubCalls, "sync must not call the Hub for an empty directory")
}

// TestSyncTemplateToHub_InvalidAgentConfigWarnsAndSurfacesHub422 verifies that
// syncTemplateToHub warns on stderr when the local agent config is schema-invalid
// (D5), surfaces a Hub 422 invalid_agent_config error from Finalize, cleans up
// a newly created pending template row on failure so retries succeed, and never
// deletes an existing active template on failure.
func TestSyncTemplateToHub_InvalidAgentConfigWarnsAndSurfacesHub422(t *testing.T) {
	t.Run("new_template_cleans_up_pending_row", func(t *testing.T) {
		localPath := t.TempDir()
		writeTemplateFile(t, localPath, "scion-agent.yaml", "default_harness_config: claude\nmax_duration: \"2 hours\"\n")

		var deletedIDs []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": []interface{}{}})
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"template": map[string]interface{}{"id": "new-tpl-id", "name": "bad", "status": "pending"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/upload" && r.Method == http.MethodPost:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []interface{}{}})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/finalize" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    "invalid_agent_config",
						"message": `template bad: agent config scion-agent.yaml is invalid: max_duration: '2 hours' does not match pattern`,
					},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodDelete:
				deletedIDs = append(deletedIDs, "new-tpl-id")
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		var syncErr error
		stderrOut := captureStderr(t, func() {
			_ = captureStdout(t, func() {
				syncErr = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "bad", localPath, "global", "claude")
			})
		})
		require.Contains(t, stderrOut, "Warning:")
		require.Contains(t, stderrOut, "max_duration")
		require.Error(t, syncErr)
		require.Contains(t, syncErr.Error(), "max_duration")
		require.Equal(t, []string{"new-tpl-id"}, deletedIDs, "failed first sync must delete the orphaned pending row")
	})

	t.Run("existing_active_template_not_deleted_on_422", func(t *testing.T) {
		localPath := t.TempDir()
		writeTemplateFile(t, localPath, "scion-agent.yaml", "default_harness_config: claude\nmax_duration: \"2 hours\"\n")

		var deletedIDs []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"templates": []map[string]interface{}{{
						"id":          "existing-tpl-id",
						"name":        "bad",
						"scope":       "global",
						"status":      "active",
						"contentHash": "sha256:old",
					}},
				})
			case r.URL.Path == "/api/v1/templates/existing-tpl-id/download" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"files": []map[string]interface{}{{
						"path": "scion-agent.yaml",
						"hash": "sha256:oldfile",
						"url":  "file:///storage/templates/global/bad/scion-agent.yaml",
					}},
				})
			case r.URL.Path == "/api/v1/templates/existing-tpl-id/upload" && r.Method == http.MethodPost:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []interface{}{}})
			case r.URL.Path == "/api/v1/templates/existing-tpl-id/finalize" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    "invalid_agent_config",
						"message": `template bad: agent config scion-agent.yaml is invalid: max_duration: '2 hours' does not match pattern`,
					},
				})
			case r.URL.Path == "/api/v1/templates/existing-tpl-id" && r.Method == http.MethodDelete:
				deletedIDs = append(deletedIDs, "existing-tpl-id")
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		var syncErr error
		_ = captureStderr(t, func() {
			_ = captureStdout(t, func() {
				syncErr = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "bad", localPath, "global", "claude")
			})
		})
		require.Error(t, syncErr)
		require.Contains(t, syncErr.Error(), "max_duration")
		require.Empty(t, deletedIDs, "existing active template must never be deleted on finalize failure")
	})
}

func TestSyncTemplateToHub_PendingCleanup(t *testing.T) {
	t.Run("ambiguous_finalize_transport_error_leaves_active_template", func(t *testing.T) {
		localPath := t.TempDir()
		writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")

		var getCalls, deleteCalls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": []interface{}{}})
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"template": map[string]interface{}{"id": "new-tpl-id", "name": "base", "status": "pending"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/upload" && r.Method == http.MethodPost:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []interface{}{}})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/finalize" && r.Method == http.MethodPost:
				// Simulate the Hub committing the template, then the connection dropping
				// before the response reaches the CLI.
				hj, ok := w.(http.Hijacker)
				require.True(t, ok)
				conn, _, err := hj.Hijack()
				require.NoError(t, err)
				_ = conn.Close()
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodGet:
				getCalls++
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":     "new-tpl-id",
					"name":   "base",
					"status": "active",
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodDelete:
				deleteCalls++
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		var syncErr error
		_ = captureStdout(t, func() {
			syncErr = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude")
		})
		require.Error(t, syncErr)
		require.Equal(t, 1, getCalls, "ambiguous finalize error must check template status via Get")
		require.Equal(t, 0, deleteCalls, "active template must not be deleted after ambiguous finalize error")
	})

	t.Run("ambiguous_finalize_5xx_leaves_active_template", func(t *testing.T) {
		localPath := t.TempDir()
		writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")

		var getCalls, deleteCalls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": []interface{}{}})
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"template": map[string]interface{}{"id": "new-tpl-id", "name": "base", "status": "pending"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/upload" && r.Method == http.MethodPost:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []interface{}{}})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/finalize" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "unavailable", "message": "gateway timeout"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodGet:
				getCalls++
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":     "new-tpl-id",
					"name":   "base",
					"status": "active",
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodDelete:
				deleteCalls++
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		var syncErr error
		_ = captureStdout(t, func() {
			syncErr = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude")
		})
		require.Error(t, syncErr)
		require.Equal(t, 1, getCalls, "5xx finalize error must check template status via Get")
		require.Equal(t, 0, deleteCalls, "active template must not be deleted after 5xx finalize error")
	})

	t.Run("ambiguous_finalize_error_with_get_failure_leaves_template", func(t *testing.T) {
		localPath := t.TempDir()
		writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")

		var getCalls, deleteCalls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": []interface{}{}})
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"template": map[string]interface{}{"id": "new-tpl-id", "name": "base", "status": "pending"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/upload" && r.Method == http.MethodPost:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []interface{}{}})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/finalize" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusBadGateway)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "internal_error", "message": "bad gateway"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodGet:
				getCalls++
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "unavailable", "message": "hub unavailable"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodDelete:
				deleteCalls++
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		var syncErr error
		_ = captureStdout(t, func() {
			syncErr = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude")
		})
		require.Error(t, syncErr)
		require.Equal(t, 1, getCalls)
		require.Equal(t, 0, deleteCalls, "must not delete when Get cannot confirm pending status")
	})

	t.Run("ambiguous_finalize_error_deletes_when_get_confirms_pending", func(t *testing.T) {
		localPath := t.TempDir()
		writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")

		var getCalls int
		var deletedIDs []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": []interface{}{}})
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"template": map[string]interface{}{"id": "new-tpl-id", "name": "base", "status": "pending"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/upload" && r.Method == http.MethodPost:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []interface{}{}})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/finalize" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "internal_error", "message": "internal error"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodGet:
				getCalls++
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":     "new-tpl-id",
					"name":   "base",
					"status": "pending",
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodDelete:
				deletedIDs = append(deletedIDs, "new-tpl-id")
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		var syncErr error
		_ = captureStdout(t, func() {
			syncErr = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude")
		})
		require.Error(t, syncErr)
		require.Equal(t, 1, getCalls, "ambiguous finalize error must check template status via Get")
		require.Equal(t, []string{"new-tpl-id"}, deletedIDs, "pending template confirmed by Get must be deleted")
	})

	t.Run("definitive_4xx_upload_failure_deletes_pending_without_get", func(t *testing.T) {
		localPath := t.TempDir()
		writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")

		var getCalls int
		var deletedIDs []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": []interface{}{}})
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"template": map[string]interface{}{"id": "new-tpl-id", "name": "base", "status": "pending"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/upload" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "validation_error", "message": "invalid file path"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodGet:
				getCalls++
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":     "new-tpl-id",
					"name":   "base",
					"status": "pending",
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodDelete:
				deletedIDs = append(deletedIDs, "new-tpl-id")
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		var syncErr error
		_ = captureStdout(t, func() {
			syncErr = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude")
		})
		require.Error(t, syncErr)
		require.Equal(t, 0, getCalls, "definitive 4xx upload error does not need an extra Get")
		require.Equal(t, []string{"new-tpl-id"}, deletedIDs, "definitive 4xx upload error must delete the pending row")
	})

	t.Run("resync_after_conflict_4xx_leaves_active_template", func(t *testing.T) {
		localPath := t.TempDir()
		writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")

		var finalizeCalls, getCalls, deleteCalls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": []interface{}{}})
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"template": map[string]interface{}{"id": "new-tpl-id", "name": "base", "status": "pending"},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/upload" && r.Method == http.MethodPost:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []interface{}{}})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/download" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"files": []map[string]interface{}{{
						"path": "scion-agent.yaml",
						"hash": transfer.HashBytes([]byte("harness: claude\n")),
						"url":  "file:///x/scion-agent.yaml",
					}},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id/finalize" && r.Method == http.MethodPost:
				finalizeCalls++
				if finalizeCalls == 1 {
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"error": map[string]interface{}{
							"code":    hubclient.TemplateConflictErrorCode,
							"message": "template was changed by another commit",
						},
					})
					return
				}
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    "invalid_agent_config",
						"message": "invalid config on retry",
					},
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodGet:
				getCalls++
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":          "new-tpl-id",
					"name":        "base",
					"status":      "active",
					"contentHash": "sha256:winner",
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodDelete:
				deleteCalls++
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		var syncErr error
		_ = captureStdout(t, func() {
			syncErr = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude")
		})
		require.Error(t, syncErr)
		require.GreaterOrEqual(t, getCalls, 2, "resync and cleanup must both inspect template status via Get")
		require.Equal(t, 0, deleteCalls, "resync failure must not delete a template activated by a competing commit")
	})
}

func TestIsDefinitivePendingSyncError(t *testing.T) {
	require.False(t, isDefinitivePendingSyncError(nil))
	require.False(t, isDefinitivePendingSyncError(errors.New("connection reset by peer")))
	require.True(t, isDefinitivePendingSyncError(&apiclient.APIError{StatusCode: http.StatusBadRequest}))
	require.True(t, isDefinitivePendingSyncError(&apiclient.APIError{StatusCode: http.StatusForbidden}))
	require.True(t, isDefinitivePendingSyncError(&apiclient.APIError{StatusCode: http.StatusUnprocessableEntity}))
	require.True(t, isDefinitivePendingSyncError(fmt.Errorf("wrapped: %w", &apiclient.APIError{StatusCode: http.StatusUnprocessableEntity})))
	require.False(t, isDefinitivePendingSyncError(&apiclient.APIError{StatusCode: http.StatusRequestTimeout}))
	require.False(t, isDefinitivePendingSyncError(&apiclient.APIError{StatusCode: http.StatusConflict}))
	require.False(t, isDefinitivePendingSyncError(&apiclient.APIError{StatusCode: http.StatusInternalServerError}))
	require.False(t, isDefinitivePendingSyncError(&apiclient.APIError{StatusCode: http.StatusServiceUnavailable}))
}

func TestCleanupPendingTemplate_DecouplesCanceledContext(t *testing.T) {
	t.Run("deletes_pending_template_when_parent_context_canceled", func(t *testing.T) {
		var getCalls, deleteCalls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodGet:
				getCalls++
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":     "new-tpl-id",
					"name":   "base",
					"status": "pending",
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodDelete:
				deleteCalls++
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		hubCtx := newTemplateSyncHubCtx(t, server)
		cleanupPendingTemplate(ctx, hubCtx.Client.Templates(), "new-tpl-id", context.DeadlineExceeded, false)

		require.Equal(t, 1, getCalls, "cleanup must query template status even when parent context is canceled")
		require.Equal(t, 1, deleteCalls, "cleanup must delete confirmed pending template even when parent context is canceled")
	})

	t.Run("preserves_active_template_when_parent_context_canceled", func(t *testing.T) {
		var getCalls, deleteCalls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodGet:
				getCalls++
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":     "new-tpl-id",
					"name":   "base",
					"status": "active",
				})
			case r.URL.Path == "/api/v1/templates/new-tpl-id" && r.Method == http.MethodDelete:
				deleteCalls++
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		hubCtx := newTemplateSyncHubCtx(t, server)
		cleanupPendingTemplate(ctx, hubCtx.Client.Templates(), "new-tpl-id", context.DeadlineExceeded, false)

		require.Equal(t, 1, getCalls, "cleanup must query template status even when parent context is canceled")
		require.Equal(t, 0, deleteCalls, "cleanup must not delete an active template")
	})
}

func TestSyncAllTemplatesToHub_RespectsProjectFlag(t *testing.T) {
	origProjectPath := projectPath
	origGlobalMode := globalMode
	t.Cleanup(func() {
		projectPath = origProjectPath
		globalMode = origGlobalMode
	})

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	writeTemplateFile(t, filepath.Join(homeDir, config.GlobalDir, "templates", "global-tpl"), "scion-agent.yaml", "harness: codex\n")

	cwdProj := t.TempDir()
	writeTemplateFile(t, filepath.Join(cwdProj, config.DotScion, "templates", "cwd-tpl"), "scion-agent.yaml", "harness: claude\n")

	otherProj := t.TempDir()
	writeTemplateFile(t, filepath.Join(otherProj, config.DotScion, "templates", "other-tpl"), "scion-agent.yaml", "harness: gemini-cli\n")

	origWd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	require.NoError(t, os.Chdir(cwdProj))

	newRecordingServer := func(t *testing.T) (*httptest.Server, *[]string) {
		t.Helper()
		var created []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]any{"templates": []any{}, "totalCount": 0})
			case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
				var req struct {
					Name string `json:"name"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				created = append(created, req.Name)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"template":   map[string]any{"id": "tpl-" + req.Name, "name": req.Name, "status": "pending"},
					"uploadUrls": []any{},
				})
			case r.Method == http.MethodPost && filepath.Base(r.URL.Path) == "upload":
				_ = json.NewEncoder(w).Encode(map[string]any{"uploadUrls": []any{}})
			case r.Method == http.MethodPost && filepath.Base(r.URL.Path) == "finalize":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":          "tpl-id",
					"name":        "ok",
					"status":      "active",
					"contentHash": "sha256:abc",
				})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		return srv, &created
	}

	t.Run("local project path reads target project instead of CWD", func(t *testing.T) {
		srv, created := newRecordingServer(t)
		hubCtx := newTemplateSyncHubCtx(t, srv)
		hubCtx.ProjectID = "other-proj-id"
		hubCtx.ProjectPath = filepath.Join(otherProj, config.DotScion)

		projectPath = otherProj
		globalMode = false

		out := captureStdout(t, func() {
			require.NoError(t, syncAllTemplatesToHub(hubCtx, "project"))
		})
		assert.Equal(t, []string{"other-tpl"}, *created, "must upload templates from --project dir, not CWD")
		assert.Contains(t, out, "1 template(s) synced")
	})

	t.Run("hub project slug does not upload CWD project-local templates", func(t *testing.T) {
		srv, created := newRecordingServer(t)
		hubCtx := newTemplateSyncHubCtx(t, srv)
		hubCtx.ProjectID = "hub-slug-id"

		projectPath = "other-hub-slug"
		globalMode = false

		out := captureStdout(t, func() {
			require.NoError(t, syncAllTemplatesToHub(hubCtx, "project"))
		})
		assert.Empty(t, *created, "must not upload CWD project-local templates into another Hub project")
		assert.Contains(t, out, "No local project templates found to sync.")
	})
}
