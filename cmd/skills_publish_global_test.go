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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for design §10 (ptone/scion#1713): 'scion skills publish --scope
// global' with a hub token holding only the skill selectors (no
// project:read or project:list) publishes from inside a linked project
// without reading any project. Project scope keeps its project lookup.

// spHub is a fake hub with one global skill "lint". Project reads answer
// 404, as they do for a token without project:read, except for the
// project tuProjectID.
type spHub struct {
	mu          sync.Mutex
	requests    []string
	projectHits []string
	finalized   []string
	created     []map[string]any
	// ownUserSkill makes the caller (tuCallerID) own a user skill "lint".
	// Another user's user skill "lint" always exists, and a scope=user list
	// returns it unless scopeId narrows the list to the caller, as the hub
	// does for an unrestricted (hub-admin) caller.
	ownUserSkill bool
	// meStatus and meBody, when set, replace the /api/v1/auth/me answer.
	meStatus int
	meBody   map[string]string
	// listStatus, when set, makes GET /api/v1/skills fail with it.
	listStatus int
	versions   []string
}

const tuCallerID = "user-caller"

// versionPosts returns the skill IDs that got a POST .../versions.
func (h *spHub) versionPosts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.versions...)
}

func (h *spHub) snapshot() (requests, projectHits, finalized []string, created []map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...), append([]string(nil), h.projectHits...),
		append([]string(nil), h.finalized...), append([]map[string]any(nil), h.created...)
}

func newSPHub(t *testing.T) (*spHub, *httptest.Server) {
	t.Helper()
	h := &spHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.RequestURI())
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects") {
			h.projectHits = append(h.projectHits, r.Method+" "+r.URL.RequestURI())
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodGet && path == "/api/v1/projects/"+tuProjectID:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": tuProjectID, "name": "tu", "slug": "tu"})
		case strings.HasPrefix(path, "/api/v1/projects"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "not_found", "message": "project not found"}})
		case r.Method == http.MethodGet && path == "/api/v1/auth/me":
			if h.meStatus != 0 {
				w.WriteHeader(h.meStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "forbidden", "message": "forbidden"}})
				return
			}
			body := map[string]string{"id": tuCallerID, "email": "caller@example.com"}
			if h.meBody != nil {
				body = h.meBody
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodGet && path == "/api/v1/skills" && h.listStatus != 0:
			w.WriteHeader(h.listStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "internal", "message": "list failed"}})
		case r.Method == http.MethodGet && path == "/api/v1/skills":
			out := []map[string]string{}
			q := r.URL.Query()
			if s := q.Get("scope"); s == "" || s == "global" {
				out = append(out, map[string]string{"id": "skill-global", "name": "lint", "scope": "global", "status": "active"})
			}
			if s := q.Get("scope"); s == "" || s == "user" {
				if id := q.Get("scopeId"); id == "" || id == "user-other" {
					out = append(out, map[string]string{"id": "skill-other-user", "name": "lint", "scope": "user", "scopeId": "user-other", "status": "active"})
				}
				if id := q.Get("scopeId"); h.ownUserSkill && (id == "" || id == tuCallerID) {
					out = append(out, map[string]string{"id": "skill-own-user", "name": "lint", "scope": "user", "scopeId": tuCallerID, "status": "active"})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"skills": out})
		case r.Method == http.MethodPost && path == "/api/v1/skills":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.mu.Lock()
			h.created = append(h.created, body)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"skill": map[string]any{"id": "skill-new", "name": body["name"], "scope": body["scope"]}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/versions"):
			h.mu.Lock()
			h.versions = append(h.versions, strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/skills/"), "/versions"))
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"version": map[string]string{"id": "v1", "version": "1.0.0", "status": "draft"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/finalize"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/skills/"), "/finalize")
			h.mu.Lock()
			h.finalized = append(h.finalized, id)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "v1", "skillId": id, "version": "1.0.0", "status": "published", "contentHash": "sha256:x"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "not_found", "message": "not found"}})
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

// writeSkillDir writes a minimal skill directory named lint and returns it.
func writeSkillDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "lint")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: lint\n---\n# lint\n"), 0o644))
	return dir
}

// runSkillsPublishRoot runs 'scion <args>' through the root command and
// resets the flags it may have set.
func runSkillsPublishRoot(t *testing.T, args ...string) error {
	t.Helper()
	restoreAllSilenceUsage(t)
	reset := func() {
		for _, name := range []string{"scope", "version", "skill-id"} {
			f := skillsPublishCmd.Flags().Lookup(name)
			require.NotNil(t, f, name)
			require.NoError(t, f.Value.Set(f.DefValue))
			f.Changed = false
		}
		for _, name := range []string{"global", "hub", "project", "yes", "non-interactive"} {
			if f := rootCmd.PersistentFlags().Lookup(name); f != nil {
				require.NoError(t, f.Value.Set(f.DefValue))
				f.Changed = false
			}
		}
		rootCmd.SetArgs(nil)
		rootCmd.SetErr(nil)
	}
	reset()
	t.Cleanup(reset)
	rootCmd.SetErr(&strings.Builder{})
	rootCmd.SetArgs(args)
	var err error
	_ = captureStdout(t, func() { err = rootCmd.Execute() })
	return err
}

// TestSkillsPublish_GlobalScopeWithHubTokenSkipsProject: inside a project
// linked to a project the token cannot read, a --scope global publish
// reads no project and finalizes the global skill's version.
func TestSkillsPublish_GlobalScopeWithHubTokenSkipsProject(t *testing.T) {
	for _, linked := range []string{"unreadable-project-id", tuProjectID} {
		t.Run(linked, func(t *testing.T) {
			hub, srv := newSPHub(t)
			cwd := tuSetup(t)
			writeTUProjectSettings(t, cwd, linked)
			err := runSkillsPublishRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
				"skills", "publish", writeSkillDir(t), "--version", "1.0.0", "--scope", "global")
			requests, projectHits, finalized, created := hub.snapshot()
			require.NoError(t, err, "requests: %v", requests)
			assert.Empty(t, projectHits, "a global-scope publish must not read any project")
			assert.Equal(t, []string{"skill-global"}, finalized)
			assert.Empty(t, created)
		})
	}
}

// TestSkillsPublish_ProjectScopeStillReadsProject: a --scope project
// publish keeps the project lookup and admission flow.
func TestSkillsPublish_ProjectScopeStillReadsProject(t *testing.T) {
	t.Run("unreadable project fails before publishing", func(t *testing.T) {
		hub, srv := newSPHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, "unreadable-project-id")
		err := runSkillsPublishRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
			"skills", "publish", writeSkillDir(t), "--version", "1.0.0", "--scope", "project")
		require.Error(t, err)
		_, projectHits, finalized, created := hub.snapshot()
		assert.NotEmpty(t, projectHits)
		assert.Empty(t, finalized)
		assert.Empty(t, created)
	})

	t.Run("linked project creates the skill in that project", func(t *testing.T) {
		hub, srv := newSPHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		err := runSkillsPublishRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
			"skills", "publish", writeSkillDir(t), "--version", "1.0.0", "--scope", "project")
		requests, projectHits, finalized, created := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		assert.Contains(t, projectHits, "GET /api/v1/projects/"+tuProjectID)
		require.Len(t, created, 1, "the same-named global skill must not be reused")
		assert.Equal(t, "project", created[0]["scope"])
		assert.Equal(t, tuProjectID, created[0]["scopeId"])
		assert.Equal(t, []string{"skill-new"}, finalized)
	})
}

// TestSkillsPublish_UserScopeNarrowsToCaller: a --scope user publish by a
// caller who can list every user's skills never reuses another user's
// same-named skill: the lookup sends the caller's ID (from
// /api/v1/auth/me) as scopeId.
func TestSkillsPublish_UserScopeNarrowsToCaller(t *testing.T) {
	for _, own := range []bool{false, true} {
		name := "no own skill creates one"
		if own {
			name = "own skill is reused"
		}
		t.Run(name, func(t *testing.T) {
			hub, srv := newSPHub(t)
			hub.ownUserSkill = own
			cwd := tuSetup(t)
			writeTUProjectSettings(t, cwd, tuProjectID)
			err := runSkillsPublishRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
				"skills", "publish", writeSkillDir(t), "--version", "1.0.0", "--scope", "user")
			requests, _, finalized, created := hub.snapshot()
			require.NoError(t, err, "requests: %v", requests)
			assert.Contains(t, requests, "GET /api/v1/skills?name=lint&scope=user&scopeId="+tuCallerID)
			assert.NotContains(t, finalized, "skill-other-user", "another user's skill must not receive the version")
			if own {
				assert.Equal(t, []string{"skill-own-user"}, finalized)
				assert.Empty(t, created)
			} else {
				require.Len(t, created, 1)
				assert.Equal(t, "user", created[0]["scope"])
				assert.Equal(t, []string{"skill-new"}, finalized)
			}
		})
	}
}

// TestSkillsPublish_UserScopeFailsClosedWithoutCaller: when the hub does
// not identify the caller, a --scope user publish stops before any lookup,
// create or version.
func TestSkillsPublish_UserScopeFailsClosedWithoutCaller(t *testing.T) {
	for name, configure := range map[string]func(*spHub){
		"auth me forbidden": func(h *spHub) { h.meStatus = http.StatusForbidden },
		"auth me empty id":  func(h *spHub) { h.meBody = map[string]string{"id": ""} },
	} {
		t.Run(name, func(t *testing.T) {
			hub, srv := newSPHub(t)
			configure(hub)
			cwd := tuSetup(t)
			writeTUProjectSettings(t, cwd, tuProjectID)
			err := runSkillsPublishRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
				"skills", "publish", writeSkillDir(t), "--version", "1.0.0", "--scope", "user")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot publish user-scoped skill")
			_, _, finalized, created := hub.snapshot()
			assert.Empty(t, created, "no skill may be created")
			assert.Empty(t, hub.versionPosts(), "no version may be published")
			assert.Empty(t, finalized)
		})
	}
}

// TestSkillsPublish_ScopedLookupFailureStops: with --scope a failed skill
// lookup is returned instead of creating a possibly duplicate skill;
// without --scope the lookup error still falls through to create.
func TestSkillsPublish_ScopedLookupFailureStops(t *testing.T) {
	t.Run("scoped lookup failure is returned", func(t *testing.T) {
		hub, srv := newSPHub(t)
		hub.listStatus = http.StatusInternalServerError
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		err := runSkillsPublishRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
			"skills", "publish", writeSkillDir(t), "--version", "1.0.0", "--scope", "global")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to look up skill")
		_, _, _, created := hub.snapshot()
		assert.Empty(t, created)
		assert.Empty(t, hub.versionPosts())
	})

	t.Run("unscoped lookup failure still creates", func(t *testing.T) {
		hub, srv := newSPHub(t)
		hub.listStatus = http.StatusInternalServerError
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		err := runSkillsPublishRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
			"skills", "publish", writeSkillDir(t), "--version", "1.0.0")
		requests, _, finalized, created := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		require.Len(t, created, 1)
		assert.Equal(t, "global", created[0]["scope"])
		assert.Equal(t, []string{"skill-new"}, finalized)
	})
}
