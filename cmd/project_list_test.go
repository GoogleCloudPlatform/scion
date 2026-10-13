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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectListLocalArgs are the arguments of the machine-local project
// list, the listing of project directories on this machine. Before
// 'project list' listed projects across hubs, this was plain
// 'project list', which wrote the golden files.
var projectListLocalArgs = []string{"project", "list", "--local"}

// projectListGoldenEnv names the variable that makes
// TestProjectListLocal_Golden write its golden files instead of comparing
// against them. It is a SCION_TEST_* variable because the cmd tests' TestMain
// clears every other SCION_* variable.
const projectListGoldenEnv = "SCION_TEST_UPDATE_GOLDEN"

// writeLocalProjectFixture creates, under HOME, one project directory of
// each kind the machine-local list shows: the global project, an external
// project whose workspace is gone, a git project with agents, a git project
// with no agents and no recorded workspace (unverified), a legacy git project
// (agents next to the config directory) and an orphaned leftover.
func writeLocalProjectFixture(t *testing.T) {
	t.Helper()
	home := os.Getenv("HOME")
	global := filepath.Join(home, ".scion")
	configs := filepath.Join(global, "project-configs")
	for _, dir := range []string{
		filepath.Join(global, "agents", "a1"),
		filepath.Join(configs, "ext__9abc", ".scion"),
		filepath.Join(configs, "gitcfg__def0", ".scion", "agents", "a"),
		filepath.Join(configs, "gitcfg__def0", ".scion", "agents", "b"),
		filepath.Join(configs, "idle__4321", ".scion", "agents"),
		filepath.Join(configs, "legacy__5678", "agents", "bob"),
		filepath.Join(configs, "orphan__1234"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(configs, "ext__9abc", ".scion", "settings.yaml"),
		[]byte("workspace_path: "+filepath.Join(home, "missing")+"\nproject_id: 11111111-0000-4000-8000-000000000001\n"), 0o644))
}

// TestProjectListLocal_Golden pins the machine-local project list, text and
// JSON, byte for byte. The golden files were written from the listing as it
// was before 'project list' became the list across hubs; run the test with
// SCION_TEST_UPDATE_GOLDEN=1 to write them again.
func TestProjectListLocal_Golden(t *testing.T) {
	// The setup below changes the working directory; the golden files live
	// in this package's testdata directory.
	pkgDir, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		args   []string
		golden string
	}{
		{"text", nil, "project-list-local.txt"},
		{"json", []string{"--format", "json"}, "project-list-local.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			restoreAllSilenceUsage(t)
			t.Cleanup(func() { rootCmd.SetArgs(nil) })
			writeLocalProjectFixture(t)

			args := append(append([]string{}, projectListLocalArgs...), tc.args...)
			stdout, _, err := runScion(t, args...)
			require.NoError(t, err)
			got := strings.ReplaceAll(stdout, os.Getenv("HOME"), "$HOME")

			path := filepath.Join(pkgDir, "testdata", tc.golden)
			if os.Getenv(projectListGoldenEnv) == "1" {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(want), got)
		})
	}
}

// collapseColumns turns the columns of a table line into "|"-separated
// fields: a run of two or more spaces is one separator.
func collapseColumns(line string) string {
	return regexp.MustCompile(` {2,}`).ReplaceAllString(line, "|")
}

// writeProjectConfig writes a project directory's settings under
// ~/.scion/project-configs/<dirName>/.scion.
func writeProjectConfig(t *testing.T, dirName, settings string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".scion", "project-configs", dirName, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o644))
}

// setupProjectListTest prepares a known-project list on hub prod (the
// selected project foo, used two minutes ago, and web from a listing), a
// directory linked to foo and an unlinked directory.
func setupProjectListTest(t *testing.T) (now time.Time) {
	t.Helper()
	setupRegistryTest(t)
	writeHomeGlobalSettings(t, "")
	now = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return now }))
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		if err := c.AddHub("prod", ctxTestHub); err != nil {
			return err
		}
		if err := c.SelectProject("prod", config.KnownProject{ID: "p-foo", Slug: "foo", Name: "Foo"}, now.Add(-2*time.Minute)); err != nil {
			return err
		}
		c.ReplaceRefreshed("prod", []config.KnownProject{{ID: "p-web", Slug: "web", Name: "Web frontend"}}, now)
		return nil
	}))
	home := os.Getenv("HOME")
	writeProjectConfig(t, "foo__aaaa", "schema_version: \"1\"\nworkspace_path: "+filepath.Join(home, "src", "foo")+
		"\nhub:\n  endpoint: "+ctxTestHub+"/\n  project_id: p-foo\n")
	writeProjectConfig(t, "scratch__bbbb", "schema_version: \"1\"\nworkspace_path: "+filepath.Join(home, "tmp", "scratch")+"\n")
	return now
}

// The default list merges the known projects with the directories on this
// machine: the directory linked to foo is foo's row, with its local path;
// unlinked directories have no hub.
func TestProjectList_MergesLinkedDirectory(t *testing.T) {
	now := setupProjectListTest(t)

	for _, args := range [][]string{{"project", "list"}, {"projects", "list"}} {
		stdout, _, err := runScion(t, args...)
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		require.Len(t, lines, 5, stdout)
		assert.Equal(t, "CURRENT  HUB   PROJECT  NAME          LOCAL PATH     LAST USED", lines[0])
		var rows []string
		for _, l := range lines[1:] {
			rows = append(rows, collapseColumns(l))
		}
		assert.Equal(t, []string{
			"*|prod|foo|Foo|~/src/foo|2m ago",
			"|prod|web|Web frontend|-|-",
			"|-|global|global|~/.scion|-",
			"|-|scratch|scratch|~/tmp/scratch|-",
		}, rows)
	}

	stdout, _, err := runScion(t, "project", "list", "--format", "json")
	require.NoError(t, err)
	var got []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
	home := os.Getenv("HOME")
	prod := map[string]interface{}{"name": "prod", "endpoint": ctxTestHub}
	assert.Equal(t, []map[string]interface{}{
		{"current": true, "hub": prod, "id": "p-foo", "slug": "foo", "name": "Foo",
			"local_path": filepath.Join(home, "src", "foo"), "last_used": now.Add(-2 * time.Minute).Format(time.RFC3339), "source": "use"},
		{"current": false, "hub": prod, "id": "p-web", "slug": "web", "name": "Web frontend",
			"local_path": "", "last_used": nil, "source": "refresh"},
		{"current": false, "hub": nil, "id": "", "slug": "global", "name": "global",
			"local_path": filepath.Join(home, ".scion"), "last_used": nil, "source": "directory"},
		{"current": false, "hub": nil, "id": "", "slug": "scratch", "name": "scratch",
			"local_path": filepath.Join(home, "tmp", "scratch"), "last_used": nil, "source": "directory"},
	}, got)
}

// --hub shows only that hub's projects, by nickname or URL.
func TestProjectList_HubFilter(t *testing.T) {
	setupProjectListTest(t)
	for _, hub := range []string{"prod", ctxTestHub + "/"} {
		stdout, _, err := runScion(t, "project", "list", "--hub", hub, "--format", "json")
		require.NoError(t, err)
		var got []map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
		var slugs []string
		for _, r := range got {
			slugs = append(slugs, r["slug"].(string))
		}
		assert.Equal(t, []string{"foo", "web"}, slugs, "--hub %s", hub)
	}

	stdout, _, err := runScion(t, "project", "list", "--hub", "https://other.example.com")
	require.NoError(t, err)
	assert.Equal(t, "No projects known on hub 'https://other.example.com'. Run 'scion project list --refresh --hub https://other.example.com' to fetch its projects.\n", stdout)
}

// --refresh replaces each listed hub's refresh entries and keeps the
// selected and linked ones; a hub that does not answer keeps its cached
// entries and is named in a warning.
func TestProjectList_RefreshReplacesOnlyRefreshEntries(t *testing.T) {
	setupRegistryTest(t)
	writeHomeGlobalSettings(t, "")
	prod := newFakeProjectHub(t,
		hubclient.Project{ID: "p-web", Slug: "web", Name: "Web v2"},
		hubclient.Project{ID: "p-api", Slug: "api", Name: "API"},
		hubclient.Project{ID: "p-lnk", Slug: "lnk", Name: "Linked"},
	)
	staging := newFakeProjectHub(t)
	before := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		if err := c.AddHub("prod", prod.URL()); err != nil {
			return err
		}
		if err := c.AddHub("staging", staging.URL()); err != nil {
			return err
		}
		c.Projects = []config.KnownProject{
			{Hub: "prod", ID: "p-foo", Slug: "foo", Source: config.KnownProjectSourceUse},
			{Hub: "prod", ID: "p-old", Slug: "old", Source: config.KnownProjectSourceRefresh},
			{Hub: "prod", ID: "p-web", Slug: "web", Name: "Web", Source: config.KnownProjectSourceRefresh},
			{Hub: "prod", ID: "p-lnk", Slug: "lnk", Source: config.KnownProjectSourceLink, LocalPath: "/src/lnk"},
			{Hub: "staging", ID: "s-x", Slug: "x", Source: config.KnownProjectSourceRefresh},
		}
		c.Refreshed = map[string]time.Time{"staging": before}
		return nil
	}))
	storeTestLogin(t, prod.URL())
	storeTestLogin(t, staging.URL())
	staging.srv.Close()

	_, stderr, err := runScion(t, "project", "list", "--refresh")
	require.NoError(t, err)
	assert.Contains(t, stderr, "Warning: not refreshed: staging: unreachable: ")
	assert.Equal(t, int32(3), prod.lists.Load(), "three pages")

	c := readTestContext(t)
	assert.Equal(t, []config.KnownProject{
		{Hub: "prod", ID: "p-foo", Slug: "foo", Source: config.KnownProjectSourceUse},
		{Hub: "prod", ID: "p-lnk", Slug: "lnk", Name: "Linked", Source: config.KnownProjectSourceLink, LocalPath: "/src/lnk"},
		{Hub: "staging", ID: "s-x", Slug: "x", Source: config.KnownProjectSourceRefresh},
		{Hub: "prod", ID: "p-web", Slug: "web", Name: "Web v2", Source: config.KnownProjectSourceRefresh},
		{Hub: "prod", ID: "p-api", Slug: "api", Name: "API", Source: config.KnownProjectSourceRefresh},
	}, c.Projects)
	assert.False(t, c.Refreshed["prod"].IsZero())
	assert.True(t, c.Refreshed["staging"].Equal(before), "the unreachable hub keeps its refresh time")
}

// --refresh --hub lists only that hub; a hub without a login is skipped
// with a warning.
func TestProjectList_RefreshOneHubAndNotLoggedIn(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	_, _, err := runScion(t, "project", "list", "--refresh", "--hub", "prod")
	require.NoError(t, err)
	assert.Positive(t, prod.lists.Load())
	assert.Zero(t, staging.calls())

	other := newFakeProjectHub(t)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("other", other.URL())
	}))
	_, stderr, err := runScion(t, "project", "list", "--refresh")
	require.NoError(t, err)
	assert.Contains(t, stderr, "Warning: not refreshed: other: not logged in")
	assert.Zero(t, other.calls())

	_, _, err = runScion(t, "project", "list", "--refresh", "--hub", "https://unknown.example.com")
	require.EqualError(t, err, "hub https://unknown.example.com is not in the hub list; add it with 'scion hub add <name> https://unknown.example.com' to refresh its projects")
}

func TestProjectList_LocalFlagConflicts(t *testing.T) {
	setupProjectListTest(t)
	_, _, err := runScion(t, "project", "list", "--local", "--refresh")
	require.EqualError(t, err, "--local cannot be combined with --refresh")
	_, _, err = runScion(t, "project", "list", "--local", "--hub", "prod")
	require.EqualError(t, err, "--local cannot be combined with --hub")
}

// With SCION_HUB_ENDPOINT set, the merged settings of a linked directory
// name the environment's hub; the directory still joins its project's row
// by the hub project ID.
func TestProjectList_EnvEndpointKeepsLinkedRow(t *testing.T) {
	setupProjectListTest(t)
	t.Setenv("SCION_HUB_ENDPOINT", "https://other.example.com")
	stdout, _, err := runScion(t, "project", "list", "--format", "json")
	require.NoError(t, err)
	var got []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
	require.Len(t, got, 4, stdout)
	assert.Equal(t, "p-foo", got[0]["id"])
	assert.Equal(t, filepath.Join(os.Getenv("HOME"), "src", "foo"), got[0]["local_path"])
}
