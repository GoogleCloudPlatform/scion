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

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

const sharedTestProjectID = "0f9e8d7c-6b5a-4321-8fed-cba987654321"

// sharedProjectFixture creates a hub-seeded shared project configuration
// <mount>/projects/<id>/.scion with project-id and, when slug is set,
// project-slug, under a temporary HOME. It returns the .scion dir and HOME.
func sharedProjectFixture(t *testing.T, slug string) (scionDir, home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	scionDir = filepath.Join(t.TempDir(), "share-a", "projects", sharedTestProjectID, DotScion)
	mustWrite(t, filepath.Join(scionDir, projectkeys.ProjectIDFile), sharedTestProjectID+"\n")
	if slug != "" {
		mustWrite(t, filepath.Join(scionDir, projectkeys.ProjectSlugFile), slug+"\n")
	}
	return scionDir, home
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// externalRoot is ~/.scion/project-configs/<slug>__<id8>/.scion.
func externalRoot(home, slug string) string {
	return filepath.Join(home, GlobalDir, ProjectConfigsDir, slug+"__0f9e8d7c", DotScion)
}

func TestIsSharedProjectConfigDir(t *testing.T) {
	scionDir, _ := sharedProjectFixture(t, "web")
	if !IsSharedProjectConfigDir(scionDir) {
		t.Fatal("hub-seeded <ProjectRoot>/.scion should be a shared project configuration")
	}
	if IsSharedProjectConfigDir(filepath.Dir(scionDir)) {
		t.Error("the project root itself is not the configuration dir")
	}

	other := filepath.Join(t.TempDir(), "projects", sharedTestProjectID, DotScion)
	mustWrite(t, filepath.Join(other, projectkeys.ProjectIDFile), "11111111-2222-3333-4444-555555555555\n")
	if IsSharedProjectConfigDir(other) {
		t.Error("a project-id naming another project must not match")
	}

	noID := filepath.Join(t.TempDir(), "projects", sharedTestProjectID, DotScion)
	if err := os.MkdirAll(noID, 0o755); err != nil {
		t.Fatal(err)
	}
	if IsSharedProjectConfigDir(noID) {
		t.Error("a .scion without project-id must not match")
	}

	notUUID := filepath.Join(t.TempDir(), "my-repo", DotScion)
	mustWrite(t, filepath.Join(notUUID, projectkeys.ProjectIDFile), sharedTestProjectID+"\n")
	if IsSharedProjectConfigDir(notUUID) {
		t.Error("an ordinary git project's .scion (parent not named by the ID) must not match")
	}

	outside := filepath.Join(t.TempDir(), "id")
	mustWrite(t, outside, sharedTestProjectID+"\n")
	linked := filepath.Join(t.TempDir(), "projects", sharedTestProjectID, DotScion)
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linked, projectkeys.ProjectIDFile)); err != nil {
		t.Fatal(err)
	}
	if IsSharedProjectConfigDir(linked) {
		t.Error("a project-id symlink leading out of the directory must not be followed")
	}

	dirLink := filepath.Join(t.TempDir(), "projects", sharedTestProjectID)
	if err := os.MkdirAll(dirLink, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(scionDir, filepath.Join(dirLink, DotScion)); err != nil {
		t.Fatal(err)
	}
	if IsSharedProjectConfigDir(filepath.Join(dirLink, DotScion)) {
		t.Error("a .scion that is itself a symlink must not match")
	}
}

// The project name is the hub-written slug; without one it falls back to
// the directory name (cosmetic only: brokers refuse such a dispatch).
func TestSharedProjectName(t *testing.T) {
	scionDir, _ := sharedProjectFixture(t, "web")
	if got := GetProjectName(scionDir); got != "web" {
		t.Errorf("GetProjectName = %q, want the project-slug %q", got, "web")
	}
	mustWrite(t, filepath.Join(scionDir, projectkeys.ProjectSlugFile), "../evil\n")
	if got := GetProjectName(scionDir); got != sharedTestProjectID {
		t.Errorf("GetProjectName with an invalid slug = %q, want the directory name", got)
	}
	if err := os.Remove(filepath.Join(scionDir, projectkeys.ProjectSlugFile)); err != nil {
		t.Fatal(err)
	}
	if got := GetProjectName(scionDir); got != sharedTestProjectID {
		t.Errorf("GetProjectName without project-slug = %q, want the directory name", got)
	}
	if _, err := ReadProjectSlug(scionDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadProjectSlug of a missing file = %v, want not-exist", err)
	}
}

// Configuration is the shared directory itself (ptone/scion#5823): no
// external config dir, and its settings.yaml wins over a broker-local
// project-configs file of the same name.
func TestSharedProjectConfigIsTheDirectory(t *testing.T) {
	// The hub endpoint compared below comes from the settings files only:
	// an endpoint in the test's environment (as inside an agent container)
	// would override both. An empty value is dropped by the settings loader.
	t.Setenv("SCION_HUB_ENDPOINT", "")
	scionDir, home := sharedProjectFixture(t, "web")
	if ext, err := GetGitProjectExternalConfigDir(scionDir); err != nil || ext != "" {
		t.Fatalf("GetGitProjectExternalConfigDir = (%q, %v), want (\"\", nil)", ext, err)
	}
	if got := GetProjectConfigDir(scionDir); got != scionDir {
		t.Fatalf("GetProjectConfigDir = %q, want the shared dir %q", got, scionDir)
	}
	mustWrite(t, filepath.Join(scionDir, "settings.yaml"), "schema_version: \"1\"\nhub:\n  endpoint: https://shared.example\n")
	mustWrite(t, filepath.Join(externalRoot(home, "web"), "settings.yaml"), "schema_version: \"1\"\nhub:\n  endpoint: https://broker-local.example\n")
	vs, err := LoadVersionedSettings(scionDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := vs.GetHubEndpoint(); got != "https://shared.example" {
		t.Fatalf("hub endpoint = %q, want the shared configuration's", got)
	}
}

// Every agent's state root is the broker-local external root, whatever the
// sharing mode, and only for the project the directory names.
func TestSharedProjectAgentStateRoot(t *testing.T) {
	scionDir, home := sharedProjectFixture(t, "web")
	want := filepath.Join(externalRoot(home, "web"), "agents")
	for _, shared := range []bool{false, true} {
		if got := SelectAgentsRoot(scionDir, shared); got != want {
			t.Errorf("SelectAgentsRoot(shared=%v) = %q, want %q", shared, got, want)
		}
		got, err := AgentsRootForProject(scionDir, shared, sharedTestProjectID)
		if err != nil || got != want {
			t.Errorf("AgentsRootForProject(shared=%v) = (%q, %v), want %q", shared, got, err, want)
		}
		if got := GetAgentDir(scionDir, "a1", shared); got != filepath.Join(want, "a1") {
			t.Errorf("GetAgentDir(shared=%v) = %q", shared, got)
		}
	}
	if got, err := AgentsRootForProject(scionDir, false, "11111111-2222-3333-4444-555555555555"); err == nil || !errors.Is(err, ErrAgentStateDirUnavailable) {
		t.Errorf("AgentsRootForProject for another project = (%q, %v), want ErrAgentStateDirUnavailable", got, err)
	}
	if got := GetAgentHomePath(scionDir, "a1"); got != filepath.Join(want, "a1", "home") {
		t.Errorf("GetAgentHomePath = %q, want under the external root", got)
	}
}

// The state root depends only on (project ID, slug): rewriting the
// configuration's content (as migration step 4 does when it moves the seed)
// leaves it unchanged.
func TestSharedProjectAgentStateRootStableAcrossSeedMove(t *testing.T) {
	scionDir, _ := sharedProjectFixture(t, "web")
	before, err := AgentDirForProject(scionDir, "a1", false, sharedTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(scionDir, "settings.yaml"), "schema_version: \"1\"\n")
	mustWrite(t, filepath.Join(scionDir, "templates", "t", "scion-agent.yaml"), "harness: claude\n")
	mustWrite(t, filepath.Join(scionDir, projectkeys.ProjectSlugFile), "web\n")
	after, err := AgentDirForProject(scionDir, "a1", false, sharedTestProjectID)
	if err != nil || after != before {
		t.Fatalf("state dir after the seed move = (%q, %v), want %q", after, err, before)
	}
}

// An agent provisioned before the move keeps its in-project state dir under
// ~/.scion/projects/<slug>/.scion/agents (look-up fallback) until
// it has a scion-agent.json in the external root.
func TestSharedProjectLegacyAgentFallback(t *testing.T) {
	scionDir, home := sharedProjectFixture(t, "web")
	legacyScion := filepath.Join(home, GlobalDir, ProjectsDir, "web", DotScion)
	mustWrite(t, filepath.Join(legacyScion, projectkeys.ProjectIDFile), sharedTestProjectID+"\n")
	legacy := filepath.Join(legacyScion, "agents", "old")
	mustWrite(t, filepath.Join(legacy, "scion-agent.json"), "{}")
	ext := filepath.Join(externalRoot(home, "web"), "agents")

	if got := LegacySharedProjectAgentDir(scionDir, "old"); got != legacy {
		t.Fatalf("LegacySharedProjectAgentDir = %q, want %q", got, legacy)
	}
	if got, err := AgentDirForProject(scionDir, "old", false, sharedTestProjectID); err != nil || got != legacy {
		t.Errorf("AgentDirForProject = (%q, %v), want the legacy dir", got, err)
	}
	if got := ResolveAgentDir(scionDir, "old"); got != legacy {
		t.Errorf("ResolveAgentDir = %q, want the legacy dir", got)
	}
	if got := GetAgentDir(scionDir, "old", false); got != legacy {
		t.Errorf("GetAgentDir = %q, want the legacy dir", got)
	}
	if got := GetAgentDir(scionDir, "new", false); got != filepath.Join(ext, "new") {
		t.Errorf("GetAgentDir for a new agent = %q, want the external root", got)
	}

	// The external root wins once it holds the agent's scion-agent.json.
	mustWrite(t, filepath.Join(ext, "old", "scion-agent.json"), "{}")
	if got := ResolveAgentDir(scionDir, "old"); got != filepath.Join(ext, "old") {
		t.Errorf("ResolveAgentDir with external state = %q, want the external dir", got)
	}

	// A legacy .scion naming another project is never used.
	other := filepath.Join(home, GlobalDir, ProjectsDir, "web2", DotScion)
	mustWrite(t, filepath.Join(other, projectkeys.ProjectIDFile), "11111111-2222-3333-4444-555555555555\n")
	mustWrite(t, filepath.Join(other, "agents", "x", "scion-agent.json"), "{}")
	mustWrite(t, filepath.Join(scionDir, projectkeys.ProjectSlugFile), "web2\n")
	if got := LegacySharedProjectAgentDir(scionDir, "x"); got != "" {
		t.Errorf("LegacySharedProjectAgentDir across projects = %q, want \"\"", got)
	}
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if got := LegacySharedProjectAgentDir(scionDir, bad); got != "" {
			t.Errorf("LegacySharedProjectAgentDir(%q) = %q, want \"\"", bad, got)
		}
	}
}

// Local backend unchanged: an ordinary git project's .scion keeps split storage unchanged.
func TestSharedProjectConfigLeavesGitProjectsAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionDir := filepath.Join(t.TempDir(), "repo", DotScion)
	mustWrite(t, filepath.Join(scionDir, projectkeys.ProjectIDFile), sharedTestProjectID+"\n")
	ext, err := GetGitProjectExternalConfigDir(scionDir)
	if err != nil || ext != filepath.Join(home, GlobalDir, ProjectConfigsDir, "repo__0f9e8d7c", DotScion) {
		t.Fatalf("GetGitProjectExternalConfigDir = (%q, %v)", ext, err)
	}
	if got := SelectAgentsRoot(scionDir, false); got != filepath.Join(scionDir, "agents") {
		t.Errorf("SelectAgentsRoot(non-shared) = %q, want in-project", got)
	}
	if got := GetProjectName(scionDir); got != "repo" {
		t.Errorf("GetProjectName = %q, want %q", got, "repo")
	}
}

// Without a usable project-slug the agent-state root is unavailable to a
// start (ErrAgentStateDirUnavailable), and no agent-dir helper ever returns
// a path in the configuration directory on the share or one named by the
// UUID.
func TestSharedProjectMissingSlugStateRoot(t *testing.T) {
	for _, slug := range []string{"", "../bad"} {
		scionDir, home := sharedProjectFixture(t, "")
		if slug != "" {
			mustWrite(t, filepath.Join(scionDir, projectkeys.ProjectSlugFile), slug+"\n")
		}
		if got, err := AgentsRootForProject(scionDir, false, sharedTestProjectID); err == nil || !errors.Is(err, ErrAgentStateDirUnavailable) {
			t.Errorf("slug %q: AgentsRootForProject = (%q, %v), want ErrAgentStateDirUnavailable", slug, got, err)
		}
		uuidRoot := filepath.Join(home, GlobalDir, ProjectConfigsDir, sharedTestProjectID+"__0f9e8d7c")
		for name, got := range map[string]string{
			"SelectAgentsRoot": SelectAgentsRoot(scionDir, false),
			"GetAgentDir":      GetAgentDir(scionDir, "a1", false),
			"ResolveAgentDir":  ResolveAgentDir(scionDir, "a1"),
		} {
			if strings.HasPrefix(got, scionDir) || strings.HasPrefix(got, uuidRoot) || got == "" {
				t.Errorf("slug %q: %s = %q, must be neither on the share nor UUID-named", slug, name, got)
			}
		}
	}
}
