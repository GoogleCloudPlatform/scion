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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// hcScopeRecorder records what a mock hub was asked for, for the
// ptone/scion#1913 scope and --force tests.
type hcScopeRecorder struct {
	mu          sync.Mutex
	listScopes  []string // "scope|scopeId" per GET /harness-configs
	createReqs  []hubclient.CreateHarnessConfigRequest
	finalized   int
	existingIDs map[string]string // "scope|scopeId|name" -> id
}

func (r *hcScopeRecorder) key(scope, scopeID, name string) string {
	return scope + "|" + scopeID + "|" + name
}

// newHarnessConfigScopeHub is a mock hub that answers the harness-config
// list/create/upload/finalize calls made by syncHarnessConfigToHub. A config
// exists only if rec.existingIDs has an entry for its exact scope.
func newHarnessConfigScopeHub(t *testing.T, rec *hcScopeRecorder) *httptest.Server {
	t.Helper()
	const newID = "hc-new"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rec.mu.Lock()
		defer rec.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/harness-configs" && r.Method == http.MethodGet:
			q := r.URL.Query()
			rec.listScopes = append(rec.listScopes, q.Get("scope")+"|"+q.Get("scopeId"))
			var list []map[string]interface{}
			if id, ok := rec.existingIDs[rec.key(q.Get("scope"), q.Get("scopeId"), q.Get("name"))]; ok {
				list = append(list, map[string]interface{}{
					"id": id, "name": q.Get("name"), "scope": q.Get("scope"), "status": "active",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"harnessConfigs": list})

		case r.URL.Path == "/api/v1/harness-configs" && r.Method == http.MethodPost:
			var req hubclient.CreateHarnessConfigRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			rec.createReqs = append(rec.createReqs, req)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"harnessConfig": map[string]interface{}{"id": newID, "name": req.Name, "harness": req.Harness},
			})

		case r.Method == http.MethodGet && filepath.Base(r.URL.Path) == "download":
			// Existing config with no files: forces a full upload.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"harness config has no files"}}`))

		case r.Method == http.MethodPost && filepath.Base(r.URL.Path) == "upload":
			id := filepath.Base(filepath.Dir(r.URL.Path))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"uploadUrls": []map[string]interface{}{
					{"path": "config.yaml", "url": "file:///nonexistent/" + id + "/config.yaml", "method": "PUT"},
				},
			})

		case r.Method == http.MethodPost && filepath.Base(r.URL.Path) == "files":
			require.NoError(t, r.ParseMultipartForm(10<<20))
			for _, headers := range r.MultipartForm.File {
				for _, fh := range headers {
					f, err := fh.Open()
					require.NoError(t, err)
					_, _ = io.ReadAll(f)
					_ = f.Close()
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]interface{}{{"path": "config.yaml", "size": 16, "mode": "0644"}},
				"hash":  "sha256:test",
			})

		case r.Method == http.MethodPost && filepath.Base(r.URL.Path) == "finalize":
			rec.finalized++
			id := filepath.Base(filepath.Dir(r.URL.Path))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": id, "name": "codex", "harness": "codex", "status": "active", "contentHash": "sha256:abc",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newHCScopeTestEnv(t *testing.T, rec *hcScopeRecorder) (*HubContext, string) {
	t.Helper()
	server := newHarnessConfigScopeHub(t, rec)
	t.Cleanup(server.Close)
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("harness: codex\n"), 0o644))
	return &HubContext{Client: client, Endpoint: server.URL, ProjectID: "proj-1913"}, dir
}

func setGlobalMode(t *testing.T, v bool) {
	t.Helper()
	orig := globalMode
	globalMode = v
	t.Cleanup(func() { globalMode = orig })
}

func TestHarnessConfigHubScope(t *testing.T) {
	hubCtx := &HubContext{ProjectID: "proj-1913"}

	setGlobalMode(t, false)
	scope, scopeID, err := harnessConfigHubScope(hubCtx)
	require.NoError(t, err)
	assert.Equal(t, "project", scope)
	assert.Equal(t, "proj-1913", scopeID)

	globalMode = true
	scope, scopeID, err = harnessConfigHubScope(hubCtx)
	require.NoError(t, err)
	assert.Equal(t, "global", scope)
	assert.Empty(t, scopeID)
}

func TestSyncLocalHarnessConfigToHub_ScopeSelection(t *testing.T) {
	tests := []struct {
		name        string
		global      bool
		wantScope   string
		wantScopeID string
	}{
		{"default is project scope", false, "project", "proj-1913"},
		{"--global selects global scope", true, "global", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setGlobalMode(t, tt.global)
			rec := &hcScopeRecorder{}
			hubCtx, dir := newHCScopeTestEnv(t, rec)

			require.NoError(t, syncLocalHarnessConfigToHub(hubCtx, "codex", dir, "codex"))

			require.Equal(t, []string{tt.wantScope + "|" + tt.wantScopeID}, rec.listScopes)
			require.Len(t, rec.createReqs, 1)
			assert.Equal(t, tt.wantScope, rec.createReqs[0].Scope)
			assert.Equal(t, tt.wantScopeID, rec.createReqs[0].ScopeID)
			assert.Equal(t, 1, rec.finalized)
		})
	}
}

func TestSyncLocalHarnessConfigToHub_UpdatesExistingWithoutForce(t *testing.T) {
	// sync/push are "create or update": an existing same-scope config is
	// updated in place, no --force needed.
	setGlobalMode(t, false)
	rec := &hcScopeRecorder{}
	rec.existingIDs = map[string]string{rec.key("project", "proj-1913", "codex"): "hc-existing"}
	hubCtx, dir := newHCScopeTestEnv(t, rec)

	require.NoError(t, syncLocalHarnessConfigToHub(hubCtx, "codex", dir, "codex"))
	assert.Empty(t, rec.createReqs)
	assert.Equal(t, 1, rec.finalized)
}

func TestInstallToHub_Force(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")

	t.Run("new name installs without --force", func(t *testing.T) {
		setGlobalMode(t, false)
		rec := &hcScopeRecorder{}
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		require.NoError(t, installToHub(hubCtx, "codex", dir, "codex", false))
		require.Len(t, rec.createReqs, 1)
		assert.Equal(t, "project", rec.createReqs[0].Scope)
		assert.Equal(t, "proj-1913", rec.createReqs[0].ScopeID)
	})

	t.Run("existing name without --force is refused", func(t *testing.T) {
		setGlobalMode(t, false)
		rec := &hcScopeRecorder{}
		rec.existingIDs = map[string]string{rec.key("project", "proj-1913", "codex"): "hc-existing"}
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		err := installToHub(hubCtx, "codex", dir, "codex", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `harness-config "codex" already exists`)
		assert.Contains(t, err.Error(), "project (proj-1913)")
		assert.Contains(t, err.Error(), "--force")
		assert.Empty(t, rec.createReqs)
		assert.Zero(t, rec.finalized, "nothing may be uploaded or finalized without --force")
	})

	t.Run("existing name with --force updates in place", func(t *testing.T) {
		setGlobalMode(t, false)
		rec := &hcScopeRecorder{}
		rec.existingIDs = map[string]string{rec.key("project", "proj-1913", "codex"): "hc-existing"}
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		require.NoError(t, installToHub(hubCtx, "codex", dir, "codex", true))
		assert.Empty(t, rec.createReqs)
		assert.Equal(t, 1, rec.finalized)
	})

	t.Run("same name in global scope does not block a project install", func(t *testing.T) {
		setGlobalMode(t, false)
		rec := &hcScopeRecorder{}
		rec.existingIDs = map[string]string{rec.key("global", "", "codex"): "hc-global"}
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		require.NoError(t, installToHub(hubCtx, "codex", dir, "codex", false))
		require.Len(t, rec.createReqs, 1)
		assert.Equal(t, "project", rec.createReqs[0].Scope)
	})

	t.Run("existing global name with --global and no --force is refused", func(t *testing.T) {
		setGlobalMode(t, true)
		rec := &hcScopeRecorder{}
		rec.existingIDs = map[string]string{rec.key("global", "", "codex"): "hc-global"}
		hubCtx, dir := newHCScopeTestEnv(t, rec)

		err := installToHub(hubCtx, "codex", dir, "codex", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "scope global")
	})
}

func TestHarnessConfigScopeLabel(t *testing.T) {
	assert.Equal(t, "global", harnessConfigScopeLabel("global", ""))
	assert.Equal(t, "global", harnessConfigScopeLabel("", ""))
	assert.Equal(t, "project (p1)", harnessConfigScopeLabel("project", "p1"))
}
