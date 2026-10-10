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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conflictHub is a mock Hub whose template "conflict-tpl" starts at content
// hash sha256:old. The first `conflicts` finalizes answer 409
// template_conflict, as if another push had committed first; from the first
// conflict on, the template reports the winner's hash and manifest.
type conflictHub struct {
	mu        sync.Mutex
	conflicts int
	// winnerFiles is the manifest (path -> hash) after the competing push.
	winnerFiles map[string]string
	// recorded calls
	finalizeExpected []string
	uploadRequested  [][]string
	finalized        *hubclient.TemplateManifest
	conflicted       bool
	// notFoundFirst makes the first finalize answer the Hub's "file not
	// found" validation error, which sends sync down its re-upload-all
	// retry.
	notFoundFirst bool
}

func (h *conflictHub) server(t *testing.T, initialFiles map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		hash, files := "sha256:old", initialFiles
		if h.conflicted {
			hash, files = "sha256:winner", h.winnerFiles
		}
		tmpl := map[string]interface{}{"id": "conflict-tpl", "name": "base", "scope": "global", "status": "active", "contentHash": hash}

		switch {
		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"templates": []map[string]interface{}{tmpl}}))
		case r.URL.Path == "/api/v1/templates/conflict-tpl" && r.Method == http.MethodGet:
			assert.NoError(t, json.NewEncoder(w).Encode(tmpl))
		case r.URL.Path == "/api/v1/templates/conflict-tpl/download" && r.Method == http.MethodGet:
			var out []map[string]interface{}
			for p, fh := range files {
				out = append(out, map[string]interface{}{"path": p, "hash": fh, "url": "file:///x/" + p})
			}
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"files": out}))
		case r.URL.Path == "/api/v1/templates/conflict-tpl/upload" && r.Method == http.MethodPost:
			var req struct {
				Files []hubclient.FileUploadRequest `json:"files"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var paths []string
			for _, f := range req.Files {
				paths = append(paths, f.Path)
			}
			h.uploadRequested = append(h.uploadRequested, paths)
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []map[string]interface{}{}}))
		case r.URL.Path == "/api/v1/templates/conflict-tpl/finalize" && r.Method == http.MethodPost:
			var req hubclient.FinalizeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.finalizeExpected = append(h.finalizeExpected, req.ExpectedContentHash)
			if h.notFoundFirst {
				h.notFoundFirst = false
				w.WriteHeader(http.StatusBadRequest)
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "validation_error", "message": "file not found: mine.md"},
				}))
				return
			}
			if h.conflicts > 0 {
				h.conflicts--
				h.conflicted = true
				w.WriteHeader(http.StatusConflict)
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": hubclient.TemplateConflictErrorCode, "message": "template was changed by another commit"},
				}))
				return
			}
			h.finalized = req.Manifest
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"id": "conflict-tpl", "name": "base", "status": "active", "contentHash": "sha256:mine"}))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// The losing sync retries once: it re-reads the template, re-diffs against
// the winner's manifest (uploading only what differs from it), and
// finalizes against the winner's content hash (acceptance 1 as amended,
// ptone/scion#4221).
func TestSyncTemplateToHub_RetriesOnceOnConflict(t *testing.T) {
	localPath := t.TempDir()
	writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")
	writeTemplateFile(t, localPath, "mine.md", "mine\n")

	h := &conflictHub{
		conflicts: 1,
		winnerFiles: map[string]string{
			"scion-agent.yaml": transfer.HashBytes([]byte("harness: claude\n")),
			"mine.md":          transfer.HashBytes([]byte("mine\n")),
			"theirs.md":        transfer.HashBytes([]byte("theirs\n")),
		},
	}
	server := h.server(t, map[string]string{"scion-agent.yaml": transfer.HashBytes([]byte("harness: claude\n"))})
	defer server.Close()

	out := captureStdout(t, func() {
		require.NoError(t, syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude"))
	})

	require.Contains(t, out, "changed on the Hub by another push")
	require.Equal(t, []string{"sha256:old", "sha256:winner"}, h.finalizeExpected,
		"the first finalize names the hash the sync diffed against, the retry the winner's")
	require.Len(t, h.uploadRequested, 1, "the retry has nothing new to upload: the winner already stored mine.md")
	require.Equal(t, []string{"mine.md"}, h.uploadRequested[0])
	require.NotNil(t, h.finalized)
	paths := make([]string, 0, len(h.finalized.Files))
	for _, f := range h.finalized.Files {
		paths = append(paths, f.Path)
	}
	require.ElementsMatch(t, []string{"scion-agent.yaml", "mine.md"}, paths, "sync mirrors the local directory")
}

// A second conflict fails with a clear message instead of looping.
func TestSyncTemplateToHub_SecondConflictFails(t *testing.T) {
	localPath := t.TempDir()
	writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")
	writeTemplateFile(t, localPath, "mine.md", "mine\n")

	h := &conflictHub{conflicts: 2, winnerFiles: map[string]string{"scion-agent.yaml": transfer.HashBytes([]byte("harness: claude\n"))}}
	server := h.server(t, map[string]string{"scion-agent.yaml": transfer.HashBytes([]byte("harness: claude\n"))})
	defer server.Close()

	var err error
	captureStdout(t, func() {
		err = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude")
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "changed on the Hub again")
	require.Len(t, h.finalizeExpected, 2, "exactly one retry")
	require.Nil(t, h.finalized)
}

// The agent-start update path (updateHubTemplate) sends the Hub template's
// hash and retries a conflict the same way.
func TestUpdateHubTemplate_RetriesOnceOnConflict(t *testing.T) {
	localPath := t.TempDir()
	writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")
	files, err := hubclient.CollectFiles(localPath, nil)
	require.NoError(t, err)

	h := &conflictHub{conflicts: 1, winnerFiles: map[string]string{"other.md": transfer.HashBytes([]byte("other\n"))}}
	server := h.server(t, map[string]string{})
	defer server.Close()

	hubTemplate := &hubclient.Template{ID: "conflict-tpl", Name: "base", ContentHash: "sha256:old"}
	captureStdout(t, func() {
		res, err := updateHubTemplate(context.Background(), newTemplateSyncHubCtx(t, server), hubTemplate, &config.Template{Name: "base", Path: localPath}, files, "")
		require.NoError(t, err)
		require.Equal(t, "conflict-tpl", res.TemplateID)
	})
	require.Equal(t, []string{"sha256:old", "sha256:winner"}, h.finalizeExpected)
	require.NotNil(t, h.finalized)
}

// A conflict on the re-upload-all retry (after a "file not found" finalize)
// fails with the same clear message as the conflict retry.
func TestSyncTemplateToHub_ConflictAfterMissingFilesRetry(t *testing.T) {
	localPath := t.TempDir()
	writeTemplateFile(t, localPath, "scion-agent.yaml", "harness: claude\n")
	writeTemplateFile(t, localPath, "mine.md", "mine\n")

	h := &conflictHub{notFoundFirst: true, conflicts: 1, winnerFiles: map[string]string{}}
	server := h.server(t, map[string]string{"scion-agent.yaml": transfer.HashBytes([]byte("harness: claude\n"))})
	defer server.Close()

	var err error
	out := captureStdout(t, func() {
		err = syncTemplateToHub(newTemplateSyncHubCtx(t, server), "base", localPath, "global", "claude")
	})
	require.Contains(t, out, "re-uploading all files")
	require.Error(t, err)
	require.Contains(t, err.Error(), "changed on the Hub by another push while re-uploading")
	require.Len(t, h.finalizeExpected, 2)
	require.Nil(t, h.finalized)
}
