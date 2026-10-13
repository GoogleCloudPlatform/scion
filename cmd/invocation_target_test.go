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
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ctxTestHub       = "https://hub.example.com"
	ctxTestProjectID = "1dfdd6c7-0000-4000-8000-000000000001"
)

// resetContextTargets clears what resolveInvocationTarget fills or records
// (and the --project value it fills), as a fresh process would have them.
// Cobra does not reset package flag variables between in-process
// executions, so a value the hook filled in one execution would otherwise
// look like a --hub or --project flag in the next.
func resetContextTargets() {
	hubEndpoint, projectPath = "", ""
	hubSource, projectSource, contextProjectID, hubFlagName = "", "", "", ""
	contextWarned = false
}

// setupContextTest isolates a test of the current context: a fresh HOME, a
// working directory outside any project, human mode, no hub or agent
// variables from the environment, and the root flag variables (including
// the ones resolveInvocationTarget sets) restored at the end. Unlike
// setupNoProjectPreRun it leaves the hub on (noHub = false).
func setupContextTest(t *testing.T) string {
	t.Helper()
	setupNoProjectPreRun(t)
	origHubEndpoint := hubEndpoint
	origUseClear, origUseForce := useClear, useForce
	t.Cleanup(func() {
		resetContextTargets()
		hubEndpoint = origHubEndpoint
		useClear, useForce = origUseClear, origUseForce
	})
	for _, name := range []string{
		"SCION_AGENT_ID", "SCION_HUB_TOKEN", "SCION_AUTH_TOKEN",
		"SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("SCION_CLI_MODE", string(ModeHuman))
	noHub = false
	resetContextTargets()
	useClear, useForce = false, false
	return os.Getenv("HOME")
}

// saveTestContext writes a context whose current hub is endpoint, with
// projectID (when not empty) as its project.
func saveTestContext(t *testing.T, endpoint, projectID string) {
	t.Helper()
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		name, _ := c.EnsureHub(endpoint, "prod")
		c.CurrentHub = name
		if projectID == "" {
			return nil
		}
		return c.SelectProject(name, config.KnownProject{ID: projectID, Slug: "foo", Name: "Foo"}, time.Now())
	}))
}

// makeDirProject creates a directory project (a .scion directory) whose
// settings set hub.endpoint to endpoint ("" for none), and moves into it.
func makeDirProject(t *testing.T, endpoint string) string {
	t.Helper()
	root := t.TempDir()
	dotScion := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(dotScion, 0o755))
	settings := "project_id: dir-project-id\n"
	if endpoint != "" {
		settings += "hub:\n  endpoint: " + endpoint + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settings), 0o644))
	t.Chdir(root)
	return root
}

func findCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	c, _, err := rootCmd.Find(args)
	require.NoError(t, err)
	return c
}

func TestResolveInvocationTarget(t *testing.T) {
	type want struct {
		hub, hubSource, project, projectSource string
	}
	filled := want{ctxTestHub, targetSourceContext, ctxTestProjectID, targetSourceContext}
	hubOnly := want{hub: ctxTestHub, hubSource: targetSourceContext}

	tests := []struct {
		name  string
		setup func(t *testing.T)
		cmd   []string
		mode  CLIMode
		want  want
	}{
		{
			name: "outside a project the context fills hub and project",
			cmd:  []string{"list"},
			want: filled,
		},
		{
			name:  "hub flag beats the context, and a hub without a remembered project injects none",
			setup: func(t *testing.T) { hubEndpoint = "https://other.example.com" },
			cmd:   []string{"list"},
			want:  want{hub: "https://other.example.com"},
		},
		{
			name:  "hub flag naming the context hub uses its remembered project",
			setup: func(t *testing.T) { hubEndpoint = ctxTestHub + "/" },
			cmd:   []string{"list"},
			want:  want{hub: ctxTestHub + "/", project: ctxTestProjectID, projectSource: targetSourceContext},
		},
		{
			name:  "project flag beats the context project",
			setup: func(t *testing.T) { projectPath = "other-slug" },
			cmd:   []string{"list"},
			want:  want{hub: ctxTestHub, hubSource: targetSourceContext, project: "other-slug"},
		},
		{
			name:  "SCION_HUB_ENDPOINT beats the context",
			setup: func(t *testing.T) { t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.com") },
			cmd:   []string{"list"},
		},
		{
			name:  "SCION_HUB_ENDPOINT equal to the context hub uses its remembered project",
			setup: func(t *testing.T) { t.Setenv("SCION_HUB_ENDPOINT", ctxTestHub) },
			cmd:   []string{"list"},
			want:  want{project: ctxTestProjectID, projectSource: targetSourceContext},
		},
		{
			name:  "SCION_HUB_URL beats the context",
			setup: func(t *testing.T) { t.Setenv("SCION_HUB_URL", "https://env.example.com") },
			cmd:   []string{"list"},
		},
		{
			name:  "directory project with an endpoint beats the context",
			setup: func(t *testing.T) { makeDirProject(t, "http://dir.example.com") },
			cmd:   []string{"list"},
		},
		{
			name:  "directory project without an endpoint uses the context hub but not its project",
			setup: func(t *testing.T) { makeDirProject(t, "") },
			cmd:   []string{"list"},
			want:  hubOnly,
		},
		{
			name: "global settings endpoint inside a directory project counts as the directory's",
			setup: func(t *testing.T) {
				writeHomeGlobalSettings(t, "hub:\n  endpoint: http://global.example.com\n")
				makeDirProject(t, "")
			},
			cmd: []string{"list"},
		},
		{
			name:  "outside a project the context hub beats the global settings endpoint",
			setup: func(t *testing.T) { writeHomeGlobalSettings(t, "hub:\n  endpoint: http://global.example.com\n") },
			cmd:   []string{"list"},
			want:  filled,
		},
		{
			name:  "--global is not injected",
			setup: func(t *testing.T) { globalMode = true },
			cmd:   []string{"list"},
			want:  hubOnly,
		},
		{
			name: "--project with a path is not injected",
			setup: func(t *testing.T) {
				root := makeDirProject(t, "")
				t.Chdir(t.TempDir())
				projectPath = filepath.Join(root, ".scion")
			},
			cmd:  []string{"list"},
			want: want{hub: ctxTestHub, hubSource: targetSourceContext, project: "<path>"},
		},
		{
			name: "agent mode ignores the context",
			cmd:  []string{"list"},
			mode: ModeAgent,
		},
		{
			name:  "agent container (SCION_HOST_UID) ignores the context",
			setup: func(t *testing.T) { t.Setenv("SCION_HOST_UID", "1000") },
			cmd:   []string{"list"},
		},
		{
			name:  "hub-managed agent ignores the context",
			setup: func(t *testing.T) { t.Setenv("SCION_AGENT_ID", "agent-1") },
			cmd:   []string{"list"},
		},
		{
			name:  "--no-hub ignores the context",
			setup: func(t *testing.T) { noHub = true },
			cmd:   []string{"list"},
		},
		{
			name: "server subtree ignores the context",
			cmd:  []string{"server", "start"},
		},
		{
			name: "runtime-broker subtree ignores the context",
			cmd:  []string{"runtime-broker", "status"},
		},
		{
			name: "broker alias ignores the context",
			cmd:  []string{"broker", "register"},
		},
		{
			name: "hub shadow ignores the context",
			cmd:  []string{"hub", "shadow"},
		},
		{
			name: "hub auth login sees the context hub",
			cmd:  []string{"hub", "auth", "login"},
			want: hubOnly,
		},
		{
			name: "hub auth logout sees the context hub",
			cmd:  []string{"hub", "auth", "logout"},
			want: hubOnly,
		},
		{
			name: "hub-only command gets the hub but no project",
			cmd:  []string{"tunnel"},
			want: hubOnly,
		},
		{
			name: "use gets the hub but no project",
			cmd:  []string{"use"},
			want: hubOnly,
		},
		{
			name: "config gets the hub but no project",
			cmd:  []string{"config", "list"},
			want: hubOnly,
		},
		{
			name: "config set gets the hub but no project",
			cmd:  []string{"config", "set"},
			want: hubOnly,
		},
		{
			name: "doctor gets the hub but no project",
			cmd:  []string{"doctor"},
			want: hubOnly,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupContextTest(t)
			saveTestContext(t, ctxTestHub, ctxTestProjectID)
			if tt.setup != nil {
				tt.setup(t)
			}
			want := tt.want
			if want.project == "<path>" {
				want.project = projectPath
			}
			mode := tt.mode
			if mode == "" {
				mode = ModeHuman
			}
			require.NoError(t, resolveInvocationTarget(findCommand(t, tt.cmd...), mode))
			assert.Equal(t, want.hub, hubEndpoint, "hubEndpoint")
			assert.Equal(t, want.hubSource, hubSource, "hubSource")
			assert.Equal(t, want.project, projectPath, "projectPath")
			assert.Equal(t, want.projectSource, projectSource, "projectSource")
		})
	}
}

func writeHomeGlobalSettings(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644))
}

func TestResolveInvocationTarget_NoContextFile(t *testing.T) {
	setupContextTest(t)
	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.Empty(t, hubEndpoint)
	assert.Empty(t, projectPath)
	assert.Empty(t, hubSource)
	assert.Empty(t, projectSource)
}

func TestResolveInvocationTarget_HubWithoutProject(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, "")
	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.Equal(t, ctxTestHub, hubEndpoint)
	assert.Empty(t, projectPath)
}

func TestResolveInvocationTarget_CorruptContextIsAWarning(t *testing.T) {
	home := setupContextTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "context.yaml"), []byte("version: [1\n"), 0o600))
	stderr := captureStderr(t, func() {
		require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	})
	assert.Contains(t, stderr, "Warning: ignoring the current context")
	assert.Empty(t, hubEndpoint)
	assert.Empty(t, projectPath)
}

// TestPersistentPreRun_ContextProjectSatisfiesProjectRequirement runs the
// root hook for 'scion list' outside any project: without a context it
// fails with "not in a scion project"; with a selected project it passes
// and targets that project's UUID.
func TestPersistentPreRun_ContextProjectSatisfiesProjectRequirement(t *testing.T) {
	setupContextTest(t)
	list := findCommand(t, "list")
	restoreSilenceUsage(t, list)

	err := rootCmd.PersistentPreRunE(list, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")

	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	require.NoError(t, rootCmd.PersistentPreRunE(list, nil))
	assert.Equal(t, ctxTestProjectID, projectPath)
	assert.Equal(t, ctxTestHub, hubEndpoint)
	assert.True(t, isContextProject(projectPath))
	assert.False(t, isContextProject("some-other-ref"))
}

// TestPersistentPreRun_AgentContainerIgnoresContext covers the acceptance
// item: inside an agent container with a context.yaml in HOME, nothing
// changes. The container's hub is the context hub, so without the
// SCION_HOST_UID guard the remembered project would be injected through the
// environment hub (see the "SCION_HUB_ENDPOINT equal to the context hub"
// case of TestResolveInvocationTarget).
func TestPersistentPreRun_AgentContainerIgnoresContext(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	t.Setenv("SCION_HOST_UID", "1000")
	t.Setenv("SCION_HUB_ENDPOINT", ctxTestHub)
	list := findCommand(t, "list")
	restoreSilenceUsage(t, list)
	require.NoError(t, rootCmd.PersistentPreRunE(list, nil))
	assert.Empty(t, hubEndpoint)
	assert.Empty(t, projectPath)
	assert.Empty(t, projectSource)
}

func TestCommandRequiresProject_TopLevelUse(t *testing.T) {
	assert.False(t, commandRequiresProject(useCmd))
	assert.False(t, commandRequiresProject(projectUseCmd))
	// A nested command named "use" elsewhere is not exempt by name alone.
	parent := &cobra.Command{Use: "other"}
	nested := &cobra.Command{Use: "use"}
	parent.AddCommand(nested)
	root := &cobra.Command{Use: "scion"}
	root.AddCommand(parent)
	assert.True(t, commandRequiresProject(nested))
}

// TestPersistentPreRun_ConfigSubcommandGlobalSeesGlobalEndpoint: a config
// subcommand's own --global counts as --global for the hook, so the global
// settings endpoint beats the context hub, as with the root --global.
func TestPersistentPreRun_ConfigSubcommandGlobalSeesGlobalEndpoint(t *testing.T) {
	setupContextTest(t)
	restoreAllSilenceUsage(t)
	resetConfigSetFlags(t)
	writeHomeGlobalSettings(t, "hub:\n  endpoint: http://global.example.com\n")
	saveTestContext(t, ctxTestHub, ctxTestProjectID)

	rootCmd.SetArgs([]string{"config", "set", "--global", "image_registry", "registry.example.test/team"})
	require.NoError(t, rootCmd.Execute())
	assert.Empty(t, hubEndpoint, "the context hub must not fill in under --global")
	assert.Empty(t, hubSource)
}

// TestPersistentPreRun_ConfigOutsideProjectKeepsError: outside any project,
// a config subcommand that needs a project fails exactly as without a
// current context, rather than acting on the context project's UUID as a
// path.
func TestPersistentPreRun_ConfigOutsideProjectKeepsError(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	list := findCommand(t, "config", "list")
	restoreSilenceUsage(t, list)

	err := rootCmd.PersistentPreRunE(list, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")
	assert.Empty(t, projectPath, "no project is injected for config")
	assert.Equal(t, ctxTestHub, hubEndpoint, "the context hub still applies")
}

// TestResolveInvocationTarget_WarningToCommandErr: the warning about an
// unreadable context.yaml goes to the command's stderr.
func TestResolveInvocationTarget_WarningToCommandErr(t *testing.T) {
	home := setupContextTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "context.yaml"), []byte("version: [1\n"), 0o600))
	list := findCommand(t, "list")
	var errOut bytes.Buffer
	list.SetErr(&errOut)
	t.Cleanup(func() { list.SetErr(nil) })

	stderr := captureStderr(t, func() { require.NoError(t, resolveInvocationTarget(list, ModeHuman)) })
	assert.Contains(t, errOut.String(), "Warning: ignoring the current context")
	assert.Empty(t, stderr)
}
