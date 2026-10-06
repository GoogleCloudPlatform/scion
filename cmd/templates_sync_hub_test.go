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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
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
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
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
				require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
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
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"files": files}))

		case r.URL.Path == "/api/v1/templates/existing-tpl-id/upload" && r.Method == http.MethodPost:
			calls.uploadRequests++
			var req struct {
				Files []hubclient.FileUploadRequest `json:"files"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			for _, f := range req.Files {
				calls.uploadRequested = append(calls.uploadRequested, f.Path)
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []map[string]interface{}{}}))

		case r.URL.Path == "/api/v1/templates/existing-tpl-id/finalize" && r.Method == http.MethodPost:
			var req hubclient.FinalizeRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			calls.finalized = req.Manifest
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
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
