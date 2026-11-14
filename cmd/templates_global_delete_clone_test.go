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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for design §10 (ptone/scion#1713): 'scion --global templates
// delete' and 'scion --global templates clone' with a hub endpoint and a hub token
// holding only the template selectors (no project:read) reach the template
// API from a fresh HOME without reading any project. Project scope keeps
// its project lookup.
//
// The hub is the current hub of the CLI context (tuHubSettings): both
// commands define their own boolean --hub ("only search Hub"), which
// shadows the root --hub <url>, and SCION_HUB_ENDPOINT would make the run
// an agent hub context, which skips the project checks.

// runTemplatesRoot runs 'scion <args>' through the root command, then
// resets the templates delete/clone flags and the root flags it may have
// set.
func runTemplatesRoot(t *testing.T, args ...string) error {
	t.Helper()
	restoreAllSilenceUsage(t)
	resetFlags := func(fs *pflag.FlagSet, names ...string) {
		for _, name := range names {
			if f := fs.Lookup(name); f != nil {
				require.NoError(t, f.Value.Set(f.DefValue))
				f.Changed = false
			}
		}
	}
	reset := func() {
		resetFlags(templatesDeleteCmd.Flags(), "local", "hub")
		resetFlags(templatesCloneCmd.Flags(), "local", "hub")
		resetFlags(rootCmd.PersistentFlags(), "global", "hub", "project", "yes", "non-interactive")
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

// tuHubSettings selects the hub endpoint as the current hub in the CLI
// context (~/.scion/context.yaml), as 'scion hub add' plus 'scion use' do.
func tuHubSettings(t *testing.T, endpoint string) {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.DotScion, "context.yaml"),
		[]byte("version: 1\ncurrent_hub: ci\nhubs:\n  ci:\n    endpoint: "+endpoint+"\n"), 0o600))
}

func TestTemplatesDelete_GlobalWithHubTokenSkipsProject(t *testing.T) {
	hub, srv := newTUHub(t)
	tuSetup(t)
	tuHubSettings(t, srv.URL)
	err := runTemplatesRoot(t, "--yes", "--non-interactive",
		"--global", "templates", "delete", "base")
	requests, _, projectHits := hub.snapshot()
	require.NoError(t, err, "requests: %v", requests)
	assert.Empty(t, projectHits, "a --global delete must not read any project")
	hub.mu.Lock()
	defer hub.mu.Unlock()
	assert.Equal(t, []string{tuGlobalTemplateID}, hub.deleted)
}

func TestTemplatesClone_GlobalWithHubTokenSkipsProject(t *testing.T) {
	hub, srv := newTUHub(t)
	tuSetup(t)
	tuHubSettings(t, srv.URL)
	err := runTemplatesRoot(t, "--yes", "--non-interactive",
		"--global", "templates", "clone", "base", "base-copy")
	requests, _, projectHits := hub.snapshot()
	require.NoError(t, err, "requests: %v", requests)
	assert.Empty(t, projectHits, "a --global clone must not read any project")
	hub.mu.Lock()
	defer hub.mu.Unlock()
	assert.Equal(t, []string{tuGlobalTemplateID}, hub.downloaded)
	home, _ := os.UserHomeDir()
	assert.DirExists(t, filepath.Join(home, config.DotScion, "templates", "base-copy"))
}

// TestTemplatesDelete_ProjectScopeStillLinksProject: without --global,
// delete keeps the project link and admission flow: a project the token
// cannot read stops it before any delete, and a readable linked project is
// looked up and its template deleted.
func TestTemplatesDelete_ProjectScopeStillLinksProject(t *testing.T) {
	t.Run("unreadable project fails before delete", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, "unreadable-project-id")
		tuHubSettings(t, srv.URL)
		err := runTemplatesRoot(t, "--yes", "--non-interactive",
			"templates", "delete", "base")
		require.Error(t, err)
		_, _, projectHits := hub.snapshot()
		assert.NotEmpty(t, projectHits)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted)
	})

	t.Run("linked project is looked up", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)
		err := runTemplatesRoot(t, "--yes", "--non-interactive",
			"templates", "delete", "base")
		requests, _, projectHits := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		assert.Contains(t, projectHits, "GET /api/v1/projects/"+tuProjectID)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		// Without --global the project template is found (and, with
		// --yes, every match is deleted).
		assert.Contains(t, hub.deleted, "tmpl-project-1")
	})
}

// TestTemplatesGlobal_InGitCheckoutSkipsProject runs --global delete and
// clone from a git checkout with a remote, where a project lookup by git
// remote is possible. A --global delete searches the global scope only and
// makes no project request at all; clone, whose source search covers all
// scopes, makes no project-by-ID request.
func TestTemplatesGlobal_InGitCheckoutSkipsProject(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		pfGitInit(t, cwd, "https://github.com/acme/repo.git")
		tuHubSettings(t, srv.URL)
		err := runTemplatesRoot(t, "--yes", "--non-interactive", "--global", "templates", "delete", "base")
		requests, _, projectHits := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		assert.Empty(t, projectHits, "a --global delete must not read any project")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{tuGlobalTemplateID}, hub.deleted)
	})

	t.Run("clone", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		pfGitInit(t, cwd, "https://github.com/acme/repo.git")
		tuHubSettings(t, srv.URL)
		err := runTemplatesRoot(t, "--yes", "--non-interactive", "--global", "templates", "clone", "base", "base-copy")
		requests, _, projectHits := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		for _, hit := range projectHits {
			assert.NotRegexp(t, `^GET /api/v1/projects/[^?]`, hit, "a --global clone must not read a project by ID")
		}
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{tuGlobalTemplateID}, hub.downloaded)
	})
}
