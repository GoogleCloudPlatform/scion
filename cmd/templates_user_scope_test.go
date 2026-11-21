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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Tests for ptone/scion#8643: --scope global|project|user on templates
// sync/push (and singular aliases), --scope user on templates delete, Hub
// user-scope resolution in template_helpers.go (for show/pull/clone), and
// project-gate bypass for --scope user.

const tuUserTemplateID = "tmpl-user-1"

type userScopeHub struct {
	mu              sync.Mutex
	requests        []string
	projectHits     []string
	created         []hubclient.CreateTemplateRequest
	finalizedIDs    []string
	deleted         []string
	downloaded      []string
	reimported      []string
	userScopeStatus int // if non-zero, GET /api/v1/templates?scope=user returns this status
	templates       []map[string]string
}

func newUserScopeHub(t *testing.T, templates []map[string]string) (*userScopeHub, *httptest.Server) {
	t.Helper()
	h := &userScopeHub{templates: templates}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.RequestURI())
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects") {
			h.projectHits = append(h.projectHits, r.Method+" "+r.URL.RequestURI())
		}
		userStatus := h.userScopeStatus
		h.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+tuProjectID:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": tuProjectID, "name": "tu", "slug": "tu"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "not_found", "message": "project not found"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/templates":
			scopeFilter := r.URL.Query().Get("scope")
			if scopeFilter == "user" && userStatus != 0 {
				w.WriteHeader(userStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "unauthorized", "message": "user identity required"}})
				return
			}
			nameFilter := r.URL.Query().Get("name")
			h.mu.Lock()
			var out []map[string]string
			for _, tmpl := range h.templates {
				if scopeFilter != "" && tmpl["scope"] != scopeFilter {
					continue
				}
				if nameFilter != "" && tmpl["name"] != nameFilter {
					continue
				}
				out = append(out, tmpl)
			}
			h.mu.Unlock()
			if out == nil {
				out = []map[string]string{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"templates": out})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/templates":
			var req hubclient.CreateTemplateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			id := "tmpl-created-" + req.Scope + "-" + req.Name
			h.mu.Lock()
			h.created = append(h.created, req)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"template": map[string]string{
					"id":     id,
					"name":   req.Name,
					"slug":   req.Name,
					"scope":  req.Scope,
					"status": "pending",
				},
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/templates/") && strings.HasSuffix(r.URL.Path, "/upload"):
			_ = json.NewEncoder(w).Encode(map[string]any{"uploadUrls": []map[string]any{}})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/templates/") && strings.HasSuffix(r.URL.Path, "/finalize"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/templates/"), "/finalize")
			h.mu.Lock()
			h.finalizedIDs = append(h.finalizedIDs, id)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          id,
				"name":        "synced",
				"status":      "active",
				"contentHash": "sha256:finalized",
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/templates/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/templates/")
			h.mu.Lock()
			h.deleted = append(h.deleted, id)
			h.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/templates/") && strings.HasSuffix(r.URL.Path, "/download"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/templates/"), "/download")
			h.mu.Lock()
			h.downloaded = append(h.downloaded, id)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/templates/") && strings.HasSuffix(r.URL.Path, "/reimport"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/templates/"), "/reimport")
			h.mu.Lock()
			h.reimported = append(h.reimported, id)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"templates": []string{"base"}, "count": 1})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "not_found", "message": "not found"}})
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

func TestTemplatesSyncAndPush_ScopeUser(t *testing.T) {
	cases := []struct {
		name      string
		cmdArgs   []string
		inProject bool
	}{
		{"templates sync --scope user outside project", []string{"templates", "sync", "./my-tpl", "--scope", "user"}, false},
		{"templates sync --scope user inside unreadable project", []string{"templates", "sync", "./my-tpl", "--scope", "user"}, true},
		{"templates push --scope user outside project", []string{"templates", "push", "./my-tpl", "--scope", "user"}, false},
		{"template sync --scope user outside project", []string{"template", "sync", "./my-tpl", "--scope", "user"}, false},
		{"template push --scope user outside project", []string{"template", "push", "./my-tpl", "--scope", "user"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, srv := newUserScopeHub(t, nil)
			cwd := tuSetup(t)
			if tc.inProject {
				writeTUProjectSettings(t, cwd, "unreadable-project-id")
			}
			tuHubSettings(t, srv.URL)

			tplDir := filepath.Join(cwd, "my-tpl")
			writeTemplateFile(t, tplDir, "scion-agent.yaml", "harness: claude\n")

			err := runTemplatesRoot(t, tc.cmdArgs...)
			require.NoError(t, err)

			hub.mu.Lock()
			defer hub.mu.Unlock()
			assert.Empty(t, hub.projectHits, "--scope user must not link or read any project")
			require.Len(t, hub.created, 1)
			assert.Equal(t, "my-tpl", hub.created[0].Name)
			assert.Equal(t, "user", hub.created[0].Scope)
			assert.Empty(t, hub.created[0].ProjectID)
			assert.Equal(t, []string{"tmpl-created-user-my-tpl"}, hub.finalizedIDs)
		})
	}
}

func TestTemplatesSync_ScopeGlobalAndGlobalFlag(t *testing.T) {
	for name, args := range map[string][]string{
		"--scope global":          {"templates", "sync", "./my-tpl", "--scope", "global"},
		"--global":                {"--global", "templates", "sync", "./my-tpl"},
		"--global --scope global": {"--global", "templates", "sync", "./my-tpl", "--scope", "global"},
		"push --scope global":     {"templates", "push", "./my-tpl", "--scope", "global"},
	} {
		t.Run(name, func(t *testing.T) {
			hub, srv := newUserScopeHub(t, nil)
			cwd := tuSetup(t)
			tuHubSettings(t, srv.URL)
			writeTemplateFile(t, filepath.Join(cwd, "my-tpl"), "scion-agent.yaml", "harness: claude\n")

			require.NoError(t, runTemplatesRoot(t, args...))

			hub.mu.Lock()
			defer hub.mu.Unlock()
			assert.Empty(t, hub.projectHits, "global sync must not link or read any project")
			require.Len(t, hub.created, 1)
			assert.Equal(t, "global", hub.created[0].Scope)
			assert.Empty(t, hub.created[0].ProjectID)
		})
	}
}

func TestTemplatesSyncAndPush_ScopeValidationAndConflicts(t *testing.T) {
	for name, args := range map[string][]string{
		"sync unknown scope":             {"templates", "sync", "./my-tpl", "--scope", "bogus"},
		"push unknown scope":             {"templates", "push", "./my-tpl", "--scope", "bogus"},
		"sync --global vs project":       {"--global", "templates", "sync", "./my-tpl", "--scope", "project"},
		"sync --global vs user":          {"--global", "templates", "sync", "./my-tpl", "--scope", "user"},
		"push --global vs user":          {"--global", "templates", "push", "./my-tpl", "--scope", "user"},
		"sync --all with --scope user":   {"templates", "sync", "--all", "--scope", "user"},
		"push --all with --scope user":   {"templates", "push", "--all", "--scope", "user"},
		"singular sync --all user scope": {"template", "sync", "--all", "--scope", "user"},
		"singular push --all user scope": {"template", "push", "--all", "--scope", "user"},
	} {
		t.Run(name, func(t *testing.T) {
			hub, srv := newUserScopeHub(t, nil)
			cwd := tuSetup(t)
			tuHubSettings(t, srv.URL)
			writeTemplateFile(t, filepath.Join(cwd, "my-tpl"), "scion-agent.yaml", "harness: claude\n")

			err := runTemplatesRoot(t, args...)
			require.Error(t, err)
			assert.True(t, isUsageError(err), "expected usage error, got: %v", err)

			hub.mu.Lock()
			defer hub.mu.Unlock()
			assert.Empty(t, hub.requests, "validation must fail before any Hub call")
		})
	}

	t.Run("sync --scope project outside a project still fails project gate", func(t *testing.T) {
		hub, srv := newUserScopeHub(t, nil)
		cwd := tuSetup(t)
		tuHubSettings(t, srv.URL)
		writeTemplateFile(t, filepath.Join(cwd, "my-tpl"), "scion-agent.yaml", "harness: claude\n")

		err := runTemplatesRoot(t, "templates", "sync", "./my-tpl", "--scope", "project")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not in a scion project")

		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.requests)
	})
}

func TestTemplatesDelete_ScopeUser(t *testing.T) {
	allTemplates := []map[string]string{
		{"id": tuGlobalTemplateID, "name": "base", "slug": "base", "scope": "global", "status": "active"},
		{"id": "tmpl-project-1", "name": "base", "slug": "base", "scope": "project", "scopeId": tuProjectID, "projectId": tuProjectID, "status": "active"},
		{"id": tuUserTemplateID, "name": "base", "slug": "base", "scope": "user", "status": "active"},
	}

	for _, tc := range []struct {
		name      string
		cmdArgs   []string
		inProject bool
	}{
		{"templates delete --scope user outside project", []string{"--non-interactive", "templates", "delete", "base", "--scope", "user"}, false},
		{"templates delete --scope user inside unreadable project", []string{"--non-interactive", "templates", "delete", "base", "--scope", "user"}, true},
		{"singular template delete --scope user outside project", []string{"--non-interactive", "template", "delete", "base", "--scope", "user"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, srv := newUserScopeHub(t, allTemplates)
			cwd := tuSetup(t)
			if tc.inProject {
				writeTUProjectSettings(t, cwd, "unreadable-project-id")
			}
			tuHubSettings(t, srv.URL)

			err := runTemplatesRoot(t, tc.cmdArgs...)
			require.NoError(t, err)

			hub.mu.Lock()
			defer hub.mu.Unlock()
			assert.Empty(t, hub.projectHits, "--scope user delete must not read any project")
			assert.Equal(t, []string{tuUserTemplateID}, hub.deleted)
		})
	}

	t.Run("not found in user scope reports user scope", func(t *testing.T) {
		_, srv := newUserScopeHub(t, allTemplates[:2]) // only global and project
		tuSetup(t)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "templates", "delete", "base", "--scope", "user")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in user scope")
	})

	t.Run("401 on explicit --scope user is surfaced", func(t *testing.T) {
		hub, srv := newUserScopeHub(t, allTemplates)
		hub.mu.Lock()
		hub.userScopeStatus = http.StatusUnauthorized
		hub.mu.Unlock()
		tuSetup(t)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "templates", "delete", "base", "--scope", "user")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to search Hub user templates")
	})

	t.Run("same-named local templates are untouched by --scope user delete", func(t *testing.T) {
		hub, srv := newUserScopeHub(t, allTemplates)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		projTpl := createProjectTestTemplate(t, cwd, "base")
		globTpl := createTestTemplate(t, home, "base")

		err = runTemplatesRoot(t, "--non-interactive", "templates", "delete", "base", "--scope", "user")
		require.NoError(t, err)

		assert.DirExists(t, projTpl, "--scope user delete must not touch local project template")
		assert.DirExists(t, globTpl, "--scope user delete must not touch local global template")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{tuUserTemplateID}, hub.deleted)
	})
}

func TestFindTemplateAllLocations_HubUserScope(t *testing.T) {
	userOnlyTemplates := []map[string]string{
		{"id": tuUserTemplateID, "name": "personal-role", "slug": "personal-role", "scope": "user", "status": "active"},
	}

	t.Run("unscoped search finds Hub user template", func(t *testing.T) {
		_, srv := newUserScopeHub(t, userOnlyTemplates)
		tuSetup(t)
		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)
		hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: tuProjectID}

		matches, err := FindTemplateAllLocations(context.Background(), "personal-role", hubCtx, &ResolveOpts{HubOnly: true})
		require.NoError(t, err)
		require.Len(t, matches, 1)
		assert.Equal(t, LocationHubUser, matches[0].Location)
		assert.True(t, matches[0].IsHub())
		assert.False(t, matches[0].IsLocal())
		assert.Equal(t, "user", matches[0].Scope())
		assert.Contains(t, matches[0].DisplayLocation(), "Hub User")
		assert.Contains(t, matches[0].DisplayLocation(), "scope=user")
	})

	t.Run("unscoped search ignores 401 on user scope and still finds global template", func(t *testing.T) {
		hub, srv := newUserScopeHub(t, []map[string]string{
			{"id": tuGlobalTemplateID, "name": "base", "slug": "base", "scope": "global", "status": "active"},
		})
		hub.mu.Lock()
		hub.userScopeStatus = http.StatusUnauthorized
		hub.mu.Unlock()
		tuSetup(t)
		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)
		hubCtx := &HubContext{Client: client, Endpoint: srv.URL}

		matches, err := FindTemplateAllLocations(context.Background(), "base", hubCtx, &ResolveOpts{HubOnly: true})
		require.NoError(t, err)
		require.Len(t, matches, 1)
		assert.Equal(t, LocationHubGlobal, matches[0].Location)
	})

	t.Run("show, pull, and clone resolve Hub user template without new flags", func(t *testing.T) {
		hub, srv := newUserScopeHub(t, userOnlyTemplates)
		cwd := tuSetup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(cwd, config.DotScion), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(cwd, config.DotScion, "settings.yaml"),
			[]byte("schema_version: \"1\"\nproject_id: "+tuProjectID+"\n"),
			0o644,
		))
		tuHubSettings(t, srv.URL)

		require.NoError(t, runTemplatesRoot(t, "--non-interactive", "templates", "show", "personal-role"))

		pullDest := filepath.Join(cwd, "pulled-personal")
		require.NoError(t, runTemplatesRoot(t, "--non-interactive", "templates", "pull", "personal-role", "--to", pullDest))
		assert.DirExists(t, pullDest)

		require.NoError(t, runTemplatesRoot(t, "--non-interactive", "templates", "clone", "personal-role", "cloned-personal"))
		assert.DirExists(t, filepath.Join(cwd, config.DotScion, "templates", "cloned-personal"))

		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{tuUserTemplateID, tuUserTemplateID}, hub.downloaded)
	})
}

func TestTemplatesUpdate_ScopeUserSkipsProject(t *testing.T) {
	hub, srv := newUserScopeHub(t, []map[string]string{
		{
			"id":        tuUserTemplateID,
			"name":      "base",
			"slug":      "base",
			"scope":     "user",
			"status":    "active",
			"sourceUrl": "https://github.com/acme/repo/tree/main/.scion/templates/base",
		},
	})
	tuSetup(t)

	err := runTemplatesUpdateRoot(t, "--hub", srv.URL, "--yes", "--non-interactive", "templates", "update", "base", "--scope", "user")
	require.NoError(t, err)

	hub.mu.Lock()
	defer hub.mu.Unlock()
	assert.Empty(t, hub.projectHits, "--scope user update must not read any project")
	assert.Equal(t, []string{tuUserTemplateID}, hub.reimported)
}
