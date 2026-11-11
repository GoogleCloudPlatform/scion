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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for ptone/scion#5217: --project <slug> names a hub project and
// binds to it; nothing is created under the working directory and no
// project is registered. A --global upload does not link or register the
// global project.

const (
	pfExistingSlug = "aplive"
	pfExistingID   = "aplive-hub-id"
	pfContextID    = "6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f"
)

// pfHub is a fake hub with one existing project (slug "aplive", and a
// project with ID pfContextID), a reserved-slug Global project, and
// optionally a project named like the cwd repository. It records project
// registrations and every other project write, and answers the
// harness-config calls through harnessConfigScopeHubHandler.
type pfHub struct {
	rec *hcScopeRecorder

	mu            sync.Mutex
	registers     int
	projectWrites []string
}

func (h *pfHub) counts() (int, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.registers, append([]string(nil), h.projectWrites...)
}

func newPFHub(t *testing.T, repoName string) (*pfHub, *httptest.Server) {
	t.Helper()
	h := &pfHub{rec: &hcScopeRecorder{}}
	hc := harnessConfigScopeHubHandler(t, h.rec)
	project := func(id, name, slug string) map[string]string {
		return map[string]string{"id": id, "name": name, "slug": slug}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/harness-configs") {
			hc(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects") && r.Method != http.MethodGet {
			h.mu.Lock()
			h.projectWrites = append(h.projectWrites, r.Method+" "+r.URL.Path)
			if r.URL.Path == "/api/v1/projects/register" {
				h.registers++
			}
			h.mu.Unlock()
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/register":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["id"].(string)
			name, _ := body["name"].(string)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]interface{}{"id": id, "name": name},
				"created": true,
			})
		case r.URL.Path == "/api/v1/projects":
			var list []map[string]string
			switch {
			case q.Get("slug") == pfExistingSlug || q.Get("name") == pfExistingSlug:
				list = append(list, project(pfExistingID, pfExistingSlug, pfExistingSlug))
			case q.Get("slug") == "global" || q.Get("name") == "global":
				list = append(list, project("global-hub-id", "Global", "global"))
			case repoName != "" && (q.Get("name") == repoName || q.Get("gitRemote") != ""):
				list = append(list, project("repo-hub-id", repoName, repoName))
			}
			if list == nil {
				list = []map[string]string{}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": list})
		case r.URL.Path == "/api/v1/projects/"+pfExistingID:
			_ = json.NewEncoder(w).Encode(project(pfExistingID, pfExistingSlug, pfExistingSlug))
		case r.URL.Path == "/api/v1/projects/"+pfContextID:
			_ = json.NewEncoder(w).Encode(project(pfContextID, "ctxproj", "ctxproj"))
		case strings.HasSuffix(r.URL.Path, "/providers"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"providers": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

// pfSetup isolates HOME (with a global .scion), moves into a fresh
// non-project directory, points the CLI at srv as if given with --hub, and
// restores the package flags afterwards. It returns the cwd and the global
// directory.
func pfSetup(t *testing.T, srv *httptest.Server) (cwd, globalDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID", "SCION_HUB_TOKEN", "SCION_AUTH_TOKEN", "SCION_AGENT_ID", "SCION_DEV_TOKEN_FILE", "SCION_HUB_PROFILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_pftest")
	globalDir = filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("hub:\n  enabled: true\n"), 0o644))
	// Resolve symlinks so path comparisons hold on hosts with a symlinked
	// temp directory.
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(cwd)

	origProject, origGlobal, origHub, origSource := projectPath, globalMode, hubEndpoint, hubSource
	origAuto, origNonInt, origProfile := autoConfirm, nonInteractive, hubProfileFlag
	t.Cleanup(func() {
		projectPath, globalMode, hubEndpoint, hubSource = origProject, origGlobal, origHub, origSource
		autoConfirm, nonInteractive, hubProfileFlag = origAuto, origNonInt, origProfile
	})
	projectPath, globalMode, hubEndpoint, hubSource = "", false, srv.URL, ""
	autoConfirm, nonInteractive, hubProfileFlag = true, true, ""
	return cwd, globalDir
}

// pfInstall runs 'harness-config install file://<src>' with the current
// package flags.
func pfInstall(t *testing.T) error {
	t.Helper()
	src := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "config.yaml"), []byte(pfHCConfig("codex")), 0o644))
	c := &cobra.Command{}
	c.Flags().String("name", "", "")
	c.Flags().Bool("force", false, "")
	return runHarnessConfigInstall(c, []string{"file://" + src})
}

// pfHCConfig is a minimal valid harness-config config.yaml.
func pfHCConfig(harness string) string {
	return "harness: " + harness + "\nimage: test:latest\nuser: scion\n"
}

func pfGitInit(t *testing.T, dir, remote string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}

func assertNoEntries(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		if e.Name() != ".git" {
			names = append(names, e.Name())
		}
	}
	assert.Empty(t, names, "nothing may be created in %s", dir)
}

// TestHarnessConfigInstall_ProjectSlugBindsExistingProject: --project with
// an existing hub slug, from a non-project directory, installs into that
// project. Nothing is registered and nothing is created in the cwd. From a
// git checkout whose repository name matches another hub project, the
// install still goes to the named project (no name-match auto-link).
func TestHarnessConfigInstall_ProjectSlugBindsExistingProject(t *testing.T) {
	for _, tc := range []struct {
		name string
		git  bool
	}{
		{name: "non-git cwd"},
		{name: "git cwd whose repo name matches a hub project", git: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoName := ""
			if tc.git {
				repoName = "myrepo"
			}
			hub, srv := newPFHub(t, repoName)
			cwd, _ := pfSetup(t, srv)
			if tc.git {
				pfGitInit(t, cwd, "https://github.com/acme/myrepo.git")
			}
			projectPath = pfExistingSlug

			require.NoError(t, pfInstall(t))

			require.Len(t, hub.rec.createReqs, 1)
			assert.Equal(t, "project", hub.rec.createReqs[0].Scope)
			assert.Equal(t, pfExistingID, hub.rec.createReqs[0].ScopeID)
			registers, writes := hub.counts()
			assert.Zero(t, registers, "no project may be registered")
			assert.Empty(t, writes, "no project write may be sent")
			assertNoEntries(t, cwd)
		})
	}
}

// TestHarnessConfigInstall_ContextProjectIDBinds: a project selected with
// 'scion use' reaches the command as its ID in projectPath (see
// resolveInvocationTarget). It binds to that hub project; nothing is
// created in the cwd and nothing is registered.
func TestHarnessConfigInstall_ContextProjectIDBinds(t *testing.T) {
	hub, srv := newPFHub(t, "")
	cwd, _ := pfSetup(t, srv)
	projectPath = pfContextID

	require.NoError(t, pfInstall(t))

	require.Len(t, hub.rec.createReqs, 1)
	assert.Equal(t, pfContextID, hub.rec.createReqs[0].ScopeID)
	registers, writes := hub.counts()
	assert.Zero(t, registers)
	assert.Empty(t, writes)
	assertNoEntries(t, cwd)
}

// TestHarnessConfigInstall_UnknownProjectSlug: --project with a slug the
// hub does not know fails with the hub's not-found error; nothing is
// installed, registered or created.
func TestHarnessConfigInstall_UnknownProjectSlug(t *testing.T) {
	hub, srv := newPFHub(t, "")
	cwd, _ := pfSetup(t, srv)
	projectPath = "nosuchproject"

	err := pfInstall(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project 'nosuchproject' not found on hub")
	assert.Empty(t, hub.rec.createReqs)
	registers, writes := hub.counts()
	assert.Zero(t, registers)
	assert.Empty(t, writes)
	assertNoEntries(t, cwd)
}

// TestHarnessConfigInstall_GlobalDoesNotLinkGlobalProject: a --global
// install from a non-project directory, with an unlinked global project
// whose name matches the hub's Global project, uploads in the global scope
// without linking or registering anything and without changing the global
// settings.
func TestHarnessConfigInstall_GlobalDoesNotLinkGlobalProject(t *testing.T) {
	hub, srv := newPFHub(t, "")
	cwd, globalDir := pfSetup(t, srv)
	before, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	require.NoError(t, err)
	globalMode, projectPath = true, "global"

	require.NoError(t, pfInstall(t))

	require.Len(t, hub.rec.createReqs, 1)
	assert.Equal(t, "global", hub.rec.createReqs[0].Scope)
	assert.Empty(t, hub.rec.createReqs[0].ScopeID)
	registers, writes := hub.counts()
	assert.Zero(t, registers)
	assert.Empty(t, writes, "no project write (register, provider) may be sent")
	after, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "global settings must not change")
	assertNoEntries(t, cwd)
}

// TestRequireHubForUpload_GlobalWithoutTerminal: without --yes or
// --non-interactive (as 'templates sync --global' runs in a script), a
// --global upload does not reach the matching-projects prompt.
func TestRequireHubForUpload_GlobalWithoutTerminal(t *testing.T) {
	hub, srv := newPFHub(t, "")
	pfSetup(t, srv)
	autoConfirm, nonInteractive = false, false
	globalMode, projectPath = true, "global"

	hubCtx, err := requireHubForUpload(projectPath)
	require.NoError(t, err)
	assert.Empty(t, hubCtx.ProjectID)
	registers, writes := hub.counts()
	assert.Zero(t, registers)
	assert.Empty(t, writes)
}

// TestRequireHubForUpload_ProjectGlobalKeepsLinkFlow: --project global (or
// -g global) without --global is a project-scope target, so the existing
// link flow runs: the global project is linked to the hub's Global project.
func TestRequireHubForUpload_ProjectGlobalKeepsLinkFlow(t *testing.T) {
	hub, srv := newPFHub(t, "")
	pfSetup(t, srv)
	projectPath = "global"

	_, err := requireHubForUpload(projectPath)
	require.NoError(t, err)
	registers, _ := hub.counts()
	assert.Equal(t, 1, registers, "the link flow must run for a project-scope target")
}

// TestRequireHubForUpload_HubProfileRefusesGlobal: with a hub profile,
// --global is still refused before any hub call.
func TestRequireHubForUpload_HubProfileRefusesGlobal(t *testing.T) {
	hub, srv := newPFHub(t, "")
	pfSetup(t, srv)
	hubProfileFlag = "someprofile"
	globalMode, projectPath = true, "global"

	_, err := requireHubForUpload(projectPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--global does not apply with a hub profile")
	registers, writes := hub.counts()
	assert.Zero(t, registers)
	assert.Empty(t, writes)
	assert.Empty(t, hub.rec.createReqs)
}

// TestRequireHubForUpload_PathTargetsStillResolve: --project values that
// are paths (relative with ./, absolute) still name a local project
// directory.
func TestRequireHubForUpload_PathTargetsStillResolve(t *testing.T) {
	hub, srv := newPFHub(t, "")
	cwd, _ := pfSetup(t, srv)
	scionDir := filepath.Join(cwd, "proj", config.DotScion)
	require.NoError(t, os.MkdirAll(scionDir, 0o755))
	globalMode = true // global scope: no link flow, so only the resolution is checked

	for _, p := range []string{"./proj", filepath.Join(cwd, "proj")} {
		projectPath = p
		assert.Equal(t, scionDir, localProjectDir(), "localProjectDir(%q)", p)
		hubCtx, err := requireHubForUpload(projectPath)
		require.NoError(t, err, p)
		assert.Equal(t, scionDir, hubCtx.ProjectPath, "hub context project path for %q", p)
	}
	registers, _ := hub.counts()
	assert.Zero(t, registers)
}

// TestHarnessConfigSync_HubRefReadsGlobalOnly: inside a project with its
// own harness-config "foo", 'harness-config sync foo --project <slug>'
// uploads the global "foo" into the slug's project, so the walked
// project's own "foo" is never uploaded into another project's scope.
func TestHarnessConfigSync_HubRefReadsGlobalOnly(t *testing.T) {
	hub, srv := newPFHub(t, "")
	cwd, globalDir := pfSetup(t, srv)
	writeHC := func(base, harness string) {
		dir := filepath.Join(base, "harness-configs", "foo")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(pfHCConfig(harness)), 0o644))
	}
	writeHC(filepath.Join(cwd, config.DotScion), "claude")
	writeHC(globalDir, "codex")
	projectPath = pfExistingSlug

	assert.Equal(t, "", localProjectDir())
	c := &cobra.Command{}
	c.Flags().String("name", "", "")
	require.NoError(t, harnessConfigSyncCmd.RunE(c, []string{"foo"}))

	require.Len(t, hub.rec.createReqs, 1)
	assert.Equal(t, "codex", hub.rec.createReqs[0].Harness, "the global foo must be uploaded, not the walked project's")
	assert.Equal(t, "project", hub.rec.createReqs[0].Scope)
	assert.Equal(t, pfExistingID, hub.rec.createReqs[0].ScopeID)
	registers, writes := hub.counts()
	assert.Zero(t, registers)
	assert.Empty(t, writes)
}

// TestSyncBuildToHub_GlobalDoesNotLink: the post-build sync of 'scion build
// --global' uploads in the global scope without linking or registering.
func TestSyncBuildToHub_GlobalDoesNotLink(t *testing.T) {
	hub, srv := newPFHub(t, "")
	pfSetup(t, srv)
	globalMode, projectPath = true, "global"
	src := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "config.yaml"), []byte(pfHCConfig("codex")), 0o644))
	hcDir, err := config.LoadHarnessConfigDir(src)
	require.NoError(t, err)

	syncBuildToHub("codex", hcDir)

	require.Len(t, hub.rec.createReqs, 1)
	assert.Equal(t, "global", hub.rec.createReqs[0].Scope)
	registers, writes := hub.counts()
	assert.Zero(t, registers)
	assert.Empty(t, writes)
}
