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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
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
	require.False(t, isTemplateNoFilesError(nil))
	require.True(t, isTemplateNoFilesError(errors.New("template base (abc-123) has no files — sync template files first with: scion template sync base")))
	require.True(t, isTemplateNoFilesError(errors.New("template has no files")))
	require.False(t, isTemplateNoFilesError(errors.New("template not found")))
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
