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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useFakeHub is a fake hub for 'scion use': project "foo" (by UUID or
// slug), two projects named "dupe", and nothing else. It counts requests.
func useFakeHub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	foo := map[string]string{"id": ctxTestProjectID, "name": "Foo", "slug": "foo"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/"+ctxTestProjectID:
			_ = json.NewEncoder(w).Encode(foo)
		case r.URL.Path == "/api/v1/projects" && q.Get("slug") == "foo":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{foo}})
		case r.URL.Path == "/api/v1/projects" && q.Get("name") == "dupe":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []map[string]string{
				{"id": "id-1", "name": "dupe", "slug": "dupe-1"},
				{"id": "id-2", "name": "dupe", "slug": "dupe-2"},
			}})
		case r.URL.Path == "/api/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// setupUseTest is setupContextTest plus a fake hub configured as the global
// hub.endpoint, with dev auth available so the CLI counts as logged in.
func setupUseTest(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	setupContextTest(t)
	restoreAllSilenceUsage(t)
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	// No terminal: 'scion use' never prompts in these tests.
	withTerminal(t, false)
	srv, calls := useFakeHub(t)
	writeHomeGlobalSettings(t, "hub:\n  endpoint: "+srv.URL+"\n")
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	return srv, calls
}

// runScion runs the root command with args and returns stdout, stderr and
// the error. Each run starts like a fresh process: the hub and project the
// hook filled in an earlier run are cleared (see resetContextTargets).
func runScion(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	resetContextTargets()
	for _, name := range []string{"hub", "project"} {
		if f := rootCmd.PersistentFlags().Lookup(name); f != nil {
			f.Changed = false
		}
	}
	outputFormat = ""
	for _, c := range []*cobra.Command{useCmd, projectUseCmd} {
		for _, name := range []string{"clear", "force", "refresh"} {
			if f := c.Flags().Lookup(name); f != nil {
				f.Changed = false
				_ = f.Value.Set(f.DefValue)
			}
		}
	}
	if f := rootCmd.PersistentFlags().Lookup("format"); f != nil {
		f.Changed = false
	}
	for _, name := range []string{"yes", "non-interactive"} {
		if f := rootCmd.PersistentFlags().Lookup(name); f != nil {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	}
	for _, c := range []*cobra.Command{hubAddCmd, hubListCmd, hubRenameCmd, hubRemoveCmd, hubAuthLoginCmd, projectListCmd} {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		})
	}
	var err error
	stdout, stderr := captureStdoutStderr(t, func() {
		rootCmd.SetArgs(args)
		err = rootCmd.Execute()
	})
	return stdout, stderr, err
}

func readTestContext(t *testing.T) *config.CLIContext {
	t.Helper()
	c, err := config.LoadCLIContext()
	require.NoError(t, err)
	return c
}

func TestUseSelectProject(t *testing.T) {
	srv, _ := setupUseTest(t)

	stdout, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'local' ("+srv.URL+").\n", stdout)

	c := readTestContext(t)
	assert.Equal(t, "local", c.CurrentHub)
	require.NotNil(t, c.Hubs["local"])
	assert.Equal(t, srv.URL, c.Hubs["local"].Endpoint)
	require.NotNil(t, c.Hubs["local"].CurrentProject)
	assert.Equal(t, ctxTestProjectID, c.Hubs["local"].CurrentProject.ID)
	require.Len(t, c.Projects, 1)
	assert.Equal(t, "Foo", c.Projects[0].Name)
	assert.Equal(t, config.KnownProjectSourceUse, c.Projects[0].Source)

	// 'scion use' never writes hub.endpoint into settings.
	data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "hub:\n  endpoint: "+srv.URL+"\n", string(data))
}

func TestUseSelectProject_Aliases(t *testing.T) {
	for _, args := range [][]string{{"project", "use", "foo"}, {"projects", "use", "foo"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			srv, _ := setupUseTest(t)
			stdout, _, err := runScion(t, args...)
			require.NoError(t, err)
			assert.Equal(t, "Now using project 'foo' on hub 'local' ("+srv.URL+").\n", stdout)
		})
	}
}

func TestUseSelectProject_JSON(t *testing.T) {
	srv, _ := setupUseTest(t)
	stdout, _, err := runScion(t, "use", "foo", "--format", "json")
	require.NoError(t, err)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, map[string]interface{}{
		"hub":     map[string]interface{}{"name": "local", "endpoint": srv.URL, "source": "context"},
		"project": map[string]interface{}{"id": ctxTestProjectID, "slug": "foo", "name": "Foo", "source": "context"},
	}, got)
}

func TestUseSelectProject_NotFound(t *testing.T) {
	srv, _ := setupUseTest(t)
	_, _, err := runScion(t, "use", "nope")
	require.Error(t, err)
	assert.Equal(t, "project 'nope' not found on hubs: "+srv.URL, err.Error())
	_, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".scion", "context.yaml"))
	assert.True(t, os.IsNotExist(statErr), "nothing is saved on a failed lookup")
}

func TestUseSelectProject_Ambiguous(t *testing.T) {
	srv, _ := setupUseTest(t)
	_, _, err := runScion(t, "use", "dupe")
	require.Error(t, err)
	assert.Equal(t, "project 'dupe' matches several projects on the hub at "+srv.URL+"; choose one by slug:\n"+
		"  scion use dupe-1   (id id-1)\n"+
		"  scion use dupe-2   (id id-2)", err.Error())

	stdout, _, err := runScion(t, "use", "dupe", "--format", "json")
	require.Error(t, err)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, "ambiguous", got["error"])
	assert.Equal(t, []interface{}{
		map[string]interface{}{"endpoint": srv.URL, "id": "id-1", "slug": "dupe-1"},
		map[string]interface{}{"endpoint": srv.URL, "id": "id-2", "slug": "dupe-2"},
	}, got["candidates"])
	_, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".scion", "context.yaml"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestUseSelectProject_NotLoggedIn(t *testing.T) {
	srv, calls := setupUseTest(t)
	t.Setenv("SCION_DEV_TOKEN", "")
	_, _, err := runScion(t, "use", "foo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Log in with: scion hub auth login --hub "+srv.URL)
	assert.Zero(t, calls.Load(), "no hub call without a credential")
}

func TestUseSelectProject_NoHub(t *testing.T) {
	setupUseTest(t)
	writeHomeGlobalSettings(t, "")
	_, _, err := runScion(t, "use", "foo")
	require.ErrorIs(t, err, errHubNotConfigured)
	assert.Equal(t, "No hub selected. Add one with 'scion hub add <name> <url>', start a local hub with 'scion server start', or pick one with 'scion use --hub <name>'.", err.Error())
}

func TestUseSelectProject_InDirectoryProjectNotes(t *testing.T) {
	// The directory project has no endpoint of its own, so the merged
	// global endpoint (the fake hub) is the effective hub.
	setupUseTest(t)
	root := makeDirProject(t, "")
	shownRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	stdout, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Contains(t, stdout, "Now using project 'foo' on hub 'local'")
	assert.Contains(t, stdout, "Note: in this directory, project '"+config.GetProjectName(filepath.Join(shownRoot, ".scion"))+
		"' ("+shownRoot+", hub 'local') takes precedence; 'foo' applies outside scion project directories.")
}

func TestUseSelectProject_CorruptContextNeedsForce(t *testing.T) {
	setupUseTest(t)
	path := filepath.Join(os.Getenv("HOME"), ".scion", "context.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: [1\n"), 0o600))

	_, _, err := runScion(t, "use", "foo")
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrCLIContextCorrupt)
	assert.Contains(t, err.Error(), "--force")

	_, _, err = runScion(t, "use", "foo", "--force")
	require.NoError(t, err)
	assert.Equal(t, "local", readTestContext(t).CurrentHub)
}

func TestUseShow_NothingConfigured(t *testing.T) {
	setupUseTest(t)
	writeHomeGlobalSettings(t, "")
	stdout, _, err := runScion(t, "use")
	require.NoError(t, err)
	assert.Equal(t, noHubSelectedMessage+"\n", stdout)

	stdout, _, err = runScion(t, "use", "--format", "json")
	require.NoError(t, err)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, map[string]interface{}{
		"effective": map[string]interface{}{
			"hub":     map[string]interface{}{"source": "none"},
			"project": map[string]interface{}{"source": "none"},
		},
		"context":    nil,
		"overridden": false,
	}, got)
}

func TestUseShow_ContextJSON(t *testing.T) {
	srv, calls := setupUseTest(t)
	_, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	before := calls.Load()

	stdout, _, err := runScion(t, "use", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, before, calls.Load(), "bare 'scion use' makes no network calls")
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, map[string]interface{}{
		"effective": map[string]interface{}{
			"hub":     map[string]interface{}{"name": "local", "endpoint": srv.URL, "source": "context"},
			"project": map[string]interface{}{"id": ctxTestProjectID, "slug": "foo", "name": "Foo", "source": "context"},
			"auth":    map[string]interface{}{"method": "devauth"},
		},
		"context":    map[string]interface{}{"hub": "local", "project": map[string]interface{}{"id": ctxTestProjectID, "slug": "foo"}},
		"overridden": false,
	}, got)

	stdout, _, err = runScion(t, "use")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.Len(t, lines, 3, stdout)
	assert.Regexp(t, `^Hub:\s+local\s+`+srv.URL+`\s+\(current context\)$`, lines[0])
	assert.Regexp(t, `^Project:\s+foo\s+`+ctxTestProjectID+`\s+\(current context\)$`, lines[1])
	assert.True(t, strings.HasPrefix(lines[2], "Auth:     Dev auth ("), lines[2])
}

func TestUseShow_DirectoryOverridesContext(t *testing.T) {
	setupUseTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	root := makeDirProject(t, "http://dir.example.com")
	root, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	stdout, _, err := runScion(t, "use")
	require.NoError(t, err)
	assert.Contains(t, stdout, "(project settings: "+root+")")
	assert.Contains(t, stdout, "(directory: "+root+")")
	assert.Contains(t, stdout, "Saved context (applies outside scion project directories): prod / foo")

	stdout, _, err = runScion(t, "use", "--format", "json")
	require.NoError(t, err)
	var got useShowJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.True(t, got.Overridden)
	assert.Equal(t, "project_settings", got.Effective.Hub.Source)
	assert.Equal(t, "directory", got.Effective.Project.Source)
	assert.Equal(t, "dir-project-id", got.Effective.Project.ID)
}

func TestUseShow_EnvOverridesContext(t *testing.T) {
	setupUseTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.com")

	stdout, _, err := runScion(t, "use")
	require.NoError(t, err)
	assert.Contains(t, stdout, "(SCION_HUB_ENDPOINT)")
	assert.Contains(t, stdout, "Note: SCION_HUB_ENDPOINT is set and overrides the current hub in this shell.")
	assert.Contains(t, stdout, "Saved context (applies outside scion project directories): prod / foo")

	stdout, _, err = runScion(t, "use", "--format", "json")
	require.NoError(t, err)
	var got useShowJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, "env", got.Effective.Hub.Source)
	assert.Equal(t, "none", got.Effective.Project.Source, "the context project is not applied to another hub")
	assert.True(t, got.Overridden)
}

func TestUseClear(t *testing.T) {
	setupUseTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)

	stdout, _, err := runScion(t, "use", "--clear")
	require.NoError(t, err)
	assert.Equal(t, "Cleared the current project 'foo' on hub 'prod'.\n", stdout)
	c := readTestContext(t)
	assert.Equal(t, "prod", c.CurrentHub, "the hub stays current")
	assert.Nil(t, c.Hubs["prod"].CurrentProject)

	stdout, _, err = runScion(t, "use", "--clear")
	require.NoError(t, err)
	assert.Equal(t, "No project selected on hub 'prod'.\n", stdout)

	_, _, err = runScion(t, "use", "--clear", "foo")
	require.Error(t, err)
	assert.Equal(t, "--clear takes no project", err.Error())
}

func TestUseClear_NoContext(t *testing.T) {
	setupUseTest(t)
	stdout, _, err := runScion(t, "use", "--clear")
	require.NoError(t, err)
	assert.Equal(t, "No hub selected; nothing to clear.\n", stdout)
	_, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".scion", "context.yaml"))
	assert.True(t, os.IsNotExist(statErr), "--clear with nothing to clear writes nothing")
}

// TestUseThenHubAvailability runs 'scion use foo', then the root hook and
// the hub resolution a command like 'scion list' runs, from a directory
// outside any project and with hub.enabled unset: the selected project is
// resolved on the selected hub.
func TestUseThenHubAvailability(t *testing.T) {
	setupUseTest(t)
	_, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)

	list := findCommand(t, "list")
	restoreSilenceUsage(t, list)
	projectPath, hubEndpoint = "", ""
	require.NoError(t, rootCmd.PersistentPreRunE(list, nil))
	require.Equal(t, ctxTestProjectID, projectPath)

	hubCtx, err := CheckHubAvailability(projectPath)
	require.NoError(t, err)
	require.NotNil(t, hubCtx)
	assert.Equal(t, ctxTestProjectID, hubCtx.ProjectID)
}

func TestUseNotAgentAllowed(t *testing.T) {
	assert.False(t, agentAllowed["use"])
	assert.False(t, agentAllowed["project.use"])
}

func TestUseRefusedInsideAgent(t *testing.T) {
	setupUseTest(t)
	t.Setenv("SCION_AGENT_ID", "agent-1")
	err := runProjectUse(useCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available inside an agent")
}

// TestUseShow_CorruptContextWarnsOnce: an unreadable context.yaml is
// reported once per invocation, not by both the hook and 'scion use'.
func TestUseShow_CorruptContextWarnsOnce(t *testing.T) {
	setupUseTest(t)
	path := filepath.Join(os.Getenv("HOME"), ".scion", "context.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: [1\n"), 0o600))
	_, stderr, err := runScion(t, "use")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(stderr, "Warning: ignoring the current context"), stderr)
}

// TestUseSelectProject_EnvHubNote: with SCION_HUB_ENDPOINT set, the project
// is selected on that hub, which becomes the saved current hub, and a note
// says so.
func TestUseSelectProject_EnvHubNote(t *testing.T) {
	srv, _ := setupUseTest(t)
	writeHomeGlobalSettings(t, "")
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	stdout, stderr, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'local' ("+srv.URL+").\n", stdout)
	assert.Contains(t, stderr, "Note: selected on the hub from SCION_HUB_ENDPOINT; it is now the current hub.")
	assert.NotContains(t, stderr, "overrides the current hub")
	assert.Equal(t, "local", readTestContext(t).CurrentHub)
}

// TestUseShow_RepeatedRunsKeepContextSource: bare 'scion use' run several
// times in one process (each run starting like a fresh process) reports the
// hub and project from the current context every time.
func TestUseShow_RepeatedRunsKeepContextSource(t *testing.T) {
	srv, _ := setupUseTest(t)
	_, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		stdout, _, err := runScion(t, "use", "--format", "json")
		require.NoError(t, err)
		var got useShowJSON
		require.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
		assert.Equal(t, "context", got.Effective.Hub.Source, "run %d", i+1)
		assert.Equal(t, srv.URL, got.Effective.Hub.Endpoint, "run %d", i+1)
		assert.Equal(t, "context", got.Effective.Project.Source, "run %d", i+1)
		stdout, _, err = runScion(t, "use")
		require.NoError(t, err)
		assert.Contains(t, stdout, "(current context)", "run %d", i+1)
	}
}

// TestUseShow_WritesToCommandWriters: bare 'scion use' writes its output to
// the command's stdout and its warnings to the command's stderr.
func TestUseShow_WritesToCommandWriters(t *testing.T) {
	setupUseTest(t)
	writeHomeGlobalSettings(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), ".scion", "context.yaml"), []byte("version: [1\n"), 0o600))
	var out, errOut strings.Builder
	useCmd.SetOut(&out)
	useCmd.SetErr(&errOut)
	t.Cleanup(func() { useCmd.SetOut(nil); useCmd.SetErr(nil) })

	stdout, stderr, err := runScion(t, "use")
	require.NoError(t, err)
	assert.Equal(t, noHubSelectedMessage+"\n", out.String())
	assert.Equal(t, 1, strings.Count(errOut.String(), "Warning: ignoring the current context"), errOut.String())
	assert.Empty(t, stdout, "nothing goes to the process stdout")
	assert.NotContains(t, stderr, "Warning: ignoring the current context")
}

// TestUseSelectProject_UsesCommandContext: 'scion use <project>' calls the
// hub with the command's context, so a cancelled context stops it before
// anything is saved.
func TestUseSelectProject_UsesCommandContext(t *testing.T) {
	setupUseTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &cobra.Command{}
	c.SetContext(ctx)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	err := runUseSelect(c, "foo")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	_, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".scion", "context.yaml"))
	assert.True(t, os.IsNotExist(statErr), "nothing is saved")
}
