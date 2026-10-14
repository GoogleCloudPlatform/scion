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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const shadowTestProjectID = "aabb1122-0000-4000-8000-000000000002"

// shadowNoticeMarker is a fragment that only the shadow notice prints.
const shadowNoticeMarker = "'scion hub shadow' was removed. Delete "

// makeShadowDir creates what the removed 'hub shadow' left behind: a .scion
// marker file of type shadow in a new directory, and the external settings
// (project type shadow, hub enabled, endpoint and hub project ID) under
// HOME. It returns the shadow directory and the external config directory.
func makeShadowDir(t *testing.T, endpoint string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	marker := &config.ProjectMarker{
		ProjectID:   shadowTestProjectID,
		ProjectName: "Remote Project",
		ProjectSlug: "remote-project",
		Type:        "shadow",
	}
	require.NoError(t, config.WriteProjectMarker(filepath.Join(dir, config.DotScion), marker))
	configDir := filepath.Join(os.Getenv("HOME"), config.GlobalDir, config.ProjectConfigsDir, marker.DirName(), config.DotScion)
	enabled := true
	require.NoError(t, config.SaveVersionedSettings(configDir, &config.VersionedSettings{
		SchemaVersion: "1",
		ProjectType:   string(config.ProjectTypeShadow),
		WorkspacePath: dir,
		Hub: &config.V1HubClientConfig{
			Enabled:   &enabled,
			Endpoint:  endpoint,
			ProjectID: shadowTestProjectID,
		},
	}))
	return dir, configDir
}

// shadowFakeHub is a hub that knows the shadow's project, with no agents.
// It records the path of every request.
func shadowFakeHub(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/api/v1/projects/" + shadowTestProjectID:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": shadowTestProjectID, "name": "Remote Project", "slug": "remote-project"})
		case "/api/v1/projects/" + shadowTestProjectID + "/agents":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// runShadowRoot runs rootCmd in-process with args and returns its output.
func runShadowRoot(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	restoreAllSilenceUsage(t)
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	rootCmd.SetArgs(args)
	stdout, stderr = captureStdoutStderr(t, func() { err = rootCmd.Execute() })
	return stdout, stderr, err
}

// TestHubShadowStub: 'scion hub shadow' is a hidden command that names
// 'scion use' and fails, with any arguments and flags, outside any project
// and inside a shadow directory (where it prints no notice: it skips root's
// hook), without a usage block; 'hub unshadow' no longer exists and
// 'hub --help' lists neither.
func TestHubShadowStub(t *testing.T) {
	setupContextTest(t)
	for _, args := range [][]string{
		{"hub", "shadow"},
		{"hub", "shadow", "my-project", "--hub", "https://hub.example.com", "--unknown"},
	} {
		_, _, err := runShadowRoot(t, args...)
		require.ErrorIs(t, err, errHubShadowRemoved, "%v", args)
		assert.Contains(t, err.Error(), "scion use <project>")
	}

	dir, _ := makeShadowDir(t, "https://hub.example.com")
	t.Chdir(dir)
	stdout, stderr, err := runShadowRoot(t, "hub", "shadow", "my-project")
	require.ErrorIs(t, err, errHubShadowRemoved)
	assert.NotContains(t, stderr, shadowNoticeMarker)
	assert.NotContains(t, stdout, shadowNoticeMarker)

	stub := findCommand(t, "hub", "shadow")
	assert.Same(t, hubShadowRemovedCmd, stub)
	assert.True(t, stub.Hidden)
	assert.False(t, showUsageForError(stub, errHubShadowRemoved, true), "no usage block under the pointer")
	for _, c := range hubCmd.Commands() {
		assert.NotEqual(t, "unshadow", c.Name(), "hub unshadow is removed")
	}
	assert.NotContains(t, hubCmd.UsageString(), "shadow")
}

// TestShadowDir_ListRoutesToHubProjectWithNotice: in a directory left by
// the removed 'hub shadow', 'scion list' still lists the shadow's hub
// project on the shadow's hub, and prints the cleanup notice once.
func TestShadowDir_ListRoutesToHubProjectWithNotice(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	srv, paths := shadowFakeHub(t)
	dir, _ := makeShadowDir(t, srv.URL)
	t.Chdir(dir)

	stdout, stderr, err := runShadowRoot(t, "list")
	require.NoError(t, err)
	assert.NotContains(t, stdout, shadowNoticeMarker, "the notice never goes to stdout")

	assert.Contains(t, paths(), "GET /api/v1/projects/"+shadowTestProjectID+"/agents",
		"list asks the shadow's hub for the shadow's project")
	assert.Equal(t, 1, strings.Count(stderr, shadowNoticeMarker), "stderr:\n%s", stderr)
	assert.Contains(t, stderr, "This directory is a shadow of hub project 'Remote Project'; 'scion hub shadow' was removed. "+
		"Delete ./.scion and run 'scion use remote-project' (works from any directory). "+
		"'scion project prune' then cleans up its settings.\n")
}

// TestPrintShadowNotice covers when root's pre-run hook prints the notice:
// in a shadow directory and below it (naming the marker's relative path),
// and while the marker remains after its external settings were deleted;
// not with --global, in a directory project, or in a non-shadow marker
// directory.
func TestPrintShadowNotice(t *testing.T) {
	preRunCmd := func(t *testing.T, args ...string) string {
		t.Helper()
		c := findCommand(t, args...)
		restoreSilenceUsage(t, c)
		var err error
		stderr := captureStderr(t, func() { err = rootCmd.PersistentPreRunE(c, nil) })
		require.NoError(t, err)
		return stderr
	}
	preRun := func(t *testing.T) string {
		t.Helper()
		return preRunCmd(t, "list")
	}

	t.Run("subdirectory names the marker's relative path", func(t *testing.T) {
		setupNoProjectPreRun(t)
		dir, _ := makeShadowDir(t, "https://hub.example.com")
		sub := filepath.Join(dir, "a", "b")
		require.NoError(t, os.MkdirAll(sub, 0o755))
		t.Chdir(sub)
		stderr := preRun(t)
		assert.Equal(t, 1, strings.Count(stderr, shadowNoticeMarker), "stderr:\n%s", stderr)
		assert.Contains(t, stderr, "Delete "+filepath.Join("..", "..", config.DotScion)+" and run")
	})

	t.Run("a marker without its external settings still prints it", func(t *testing.T) {
		setupNoProjectPreRun(t)
		dir, configDir := makeShadowDir(t, "https://hub.example.com")
		require.NoError(t, os.RemoveAll(filepath.Dir(configDir)))
		t.Chdir(dir)
		stderr := preRunCmd(t, "hub", "status")
		assert.Equal(t, 1, strings.Count(stderr, shadowNoticeMarker), "stderr:\n%s", stderr)
	})

	t.Run("--global prints nothing", func(t *testing.T) {
		setupNoProjectPreRun(t)
		dir, _ := makeShadowDir(t, "https://hub.example.com")
		t.Chdir(dir)
		globalMode = true
		assert.NotContains(t, preRun(t), shadowNoticeMarker)
	})

	// The negative cases use 'hub status': it needs no project and no
	// image registry, and reaches the same notice condition.
	t.Run("a directory project prints nothing", func(t *testing.T) {
		setupNoProjectPreRun(t)
		makeDirProject(t, "")
		assert.NotContains(t, preRunCmd(t, "hub", "status"), shadowNoticeMarker)
	})

	t.Run("a non-shadow marker prints nothing", func(t *testing.T) {
		setupNoProjectPreRun(t)
		dir := t.TempDir()
		marker := &config.ProjectMarker{ProjectID: shadowTestProjectID, ProjectName: "Plain", ProjectSlug: "plain"}
		require.NoError(t, config.WriteProjectMarker(filepath.Join(dir, config.DotScion), marker))
		ext, err := marker.ExternalProjectPath()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(ext, 0o755))
		t.Chdir(dir)
		assert.NotContains(t, preRunCmd(t, "hub", "status"), shadowNoticeMarker)
	})
}

// TestShadowDir_PruneAfterMarkerDeleted backs the notice's last sentence:
// once the marker is deleted, 'scion project prune' finds the shadow's
// external settings orphaned.
func TestShadowDir_PruneAfterMarkerDeleted(t *testing.T) {
	setupNoProjectPreRun(t)
	dir, configDir := makeShadowDir(t, "https://hub.example.com")

	orphanedConfigs := func() []string {
		orphaned, err := config.FindOrphanedProjectConfigs()
		require.NoError(t, err)
		var out []string
		for _, p := range orphaned {
			out = append(out, p.ConfigPath)
		}
		return out
	}
	require.NotContains(t, orphanedConfigs(), configDir, "a shadow with its marker is not orphaned")

	require.NoError(t, os.Remove(filepath.Join(dir, config.DotScion)))
	assert.Contains(t, orphanedConfigs(), configDir)
}
