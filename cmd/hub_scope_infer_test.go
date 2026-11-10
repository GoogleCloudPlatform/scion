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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopeResolverCase is hub env or hub secret: its scope resolver and a
// command with its bare --project flag set, as 'set --project' parses it.
type scopeResolverCase struct {
	name    string
	resolve hubScopeResolver
	newCmd  func(t *testing.T) *cobra.Command
}

func scopeResolverCases() []scopeResolverCase {
	bareProjectCmd := func(t *testing.T, target *string) *cobra.Command {
		t.Helper()
		orig := *target
		t.Cleanup(func() { *target = orig })
		// No hub profile, unless a test selects one.
		origProfile := hubProfileFlag
		t.Cleanup(func() { hubProfileFlag = origProfile })
		hubProfileFlag = ""
		t.Setenv(hubprofile.EnvHubProfile, "")
		c := &cobra.Command{Use: "set"}
		c.Flags().StringVar(target, "project", "", "")
		c.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel
		require.NoError(t, c.Flags().Set("project", scopeInferSentinel))
		return c
	}
	return []scopeResolverCase{
		{"env", resolveEnvScope, func(t *testing.T) *cobra.Command { return bareProjectCmd(t, &envProjectScope) }},
		{"secret", resolveSecretScope, func(t *testing.T) *cobra.Command { return bareProjectCmd(t, &secretProjectScope) }},
	}
}

// A bare --project takes the project selected with 'scion use' when that is
// the only project that applies: no directory project and no hub project
// in the settings.
func TestInferScopeProject_ContextProjectOnly(t *testing.T) {
	for _, tc := range scopeResolverCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			projectPath, projectSource = ctxTestProjectID, targetSourceContext

			got, err := tc.resolve(tc.newCmd(t), &config.Settings{})
			require.NoError(t, err)
			assert.Equal(t, hubScopeTarget{scope: "project", id: ctxTestProjectID, source: scopeSourceContext}, got)
		})
	}
}

// A directory project linked to a hub gives its hub id (linkedHubProjectID),
// whatever the loaded settings say.
func TestInferScopeProject_LinkedDirectory(t *testing.T) {
	for _, tc := range scopeResolverCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			makeHubOnlyDirProject(t, "project_id: local-id\nhub:\n  projectId: hub-id\n")

			got, err := tc.resolve(tc.newCmd(t), &config.Settings{ProjectID: "local-id"})
			require.NoError(t, err)
			assert.Equal(t, hubScopeTarget{scope: "project", id: "hub-id", source: scopeSourceDirectory}, got)
		})
	}
}

// A directory project that is not linked to a hub is an error, even though
// it has a local project_id and the settings view carries it: a local
// project id is never used as a hub project.
func TestInferScopeProject_UnlinkedDirectoryIsAnError(t *testing.T) {
	for _, tc := range scopeResolverCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			t.Setenv("SCION_HUB_PROJECT_ID", "")
			makeHubOnlyDirProject(t, "project_id: local-id\n")
			settings := &config.Settings{ProjectID: "local-id", Hub: &config.HubClientConfig{ProjectID: "local-id"}}

			got, err := tc.resolve(tc.newCmd(t), settings)
			require.Error(t, err)
			assert.Equal(t, hubScopeTarget{}, got)
			assert.Contains(t, err.Error(), "directory project myrepo is not linked to a hub")
			assert.Contains(t, err.Error(), "scion hub link")
			assert.Contains(t, err.Error(), "--project=<hub project id>")
		})
	}
}

// An unlinked directory project is refused with settings loaded as the
// command loads them, from a versioned file whose hub.project_id is the
// project's own id, with the hub selected by SCION_HUB_ENDPOINT. The
// refusal holds inside a hub-managed agent too (SCION_AGENT_ID and
// SCION_PROJECT_ID set): an agent passes --project=<id> explicitly.
func TestInferScopeProject_UnlinkedVersionedProjectRefusedWithLoadedSettings(t *testing.T) {
	for _, tc := range scopeResolverCases() {
		for _, agent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/agent=%v", tc.name, agent), func(t *testing.T) {
				setupContextTest(t)
				t.Setenv("SCION_HUB_PROJECT_ID", "")
				t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.test")
				if agent {
					t.Setenv("SCION_AGENT_ID", "agent-1")
					t.Setenv("SCION_PROJECT_ID", "env-project-id")
				}
				makeHubOnlyDirProject(t, "schema_version: \"1\"\nhub:\n  project_id: local-id\n")
				resolvedPath, _, err := config.ResolveProjectPath("")
				require.NoError(t, err)
				settings, err := loadSettingsForTarget(resolvedPath)
				require.NoError(t, err)
				assert.Equal(t, "local-id", settings.GetHubProjectID(), "premise: the loaded view carries the project's own id")

				got, err := tc.resolve(tc.newCmd(t), settings)
				require.Error(t, err)
				assert.Equal(t, hubScopeTarget{}, got)
				assert.Contains(t, err.Error(), "directory project myrepo is not linked to a hub")
				assert.NotContains(t, err.Error(), "local-id")
				assert.NotContains(t, err.Error(), "env-project-id")
			})
		}
	}
}

// With a hub profile selected, no project is inferred from local state: not
// from the scion use context, not from a linked directory project.
func TestInferScopeProject_HubProfileInfersNothing(t *testing.T) {
	for _, tc := range scopeResolverCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			cmd := tc.newCmd(t) // resets the profile flag; restored at cleanup
			hubProfileFlag = "other-hub"
			makeHubOnlyDirProject(t, "project_id: local-id\nhub:\n  projectId: hub-id\n")
			projectPath, projectSource = ctxTestProjectID, targetSourceContext

			_, err := tc.resolve(cmd, &config.Settings{Hub: &config.HubClientConfig{ProjectID: "hub-id"}})
			require.Error(t, err)
			assert.True(t, errors.Is(err, errCannotInferProject))
		})
	}
}

// With no directory project and no context project, the hub project id of
// the loaded settings applies; the write path still looks it up on the hub.
func TestInferScopeProject_HubSettingsWithoutDirectory(t *testing.T) {
	settings := &config.Settings{ProjectID: "local-id", Hub: &config.HubClientConfig{ProjectID: "hub-id"}}
	want := hubScopeTarget{scope: "project", id: "hub-id", source: scopeSourceHubSettings}
	for _, tc := range scopeResolverCases() {
		t.Run(tc.name+"/no directory", func(t *testing.T) {
			setupContextTest(t)

			got, err := tc.resolve(tc.newCmd(t), settings)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

// With no project at all, or only a local project id outside any directory
// project, the error remains.
func TestInferScopeProject_NothingSetIsAnError(t *testing.T) {
	for _, tc := range scopeResolverCases() {
		for name, settings := range map[string]*config.Settings{
			"empty":    {},
			"local id": {ProjectID: "local-id"},
		} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				setupContextTest(t)

				_, err := tc.resolve(tc.newCmd(t), settings)
				require.Error(t, err)
				assert.True(t, errors.Is(err, errCannotInferProject))
				assert.Contains(t, err.Error(), "no project selected")
				assert.Contains(t, err.Error(), "scion use")
			})
		}
	}
}

// End to end from the root hook: a HOME holding only the 'scion use'
// context (no project directory, no project settings, no hub-link file)
// lets 'hub env set --project' and 'hub secret set --project' infer the
// context project from settings loaded the way resolveHubScope loads them.
// Without a context project the error still appears.
func TestInferScopeProject_FromUseContextThroughPreRun(t *testing.T) {
	for _, tc := range scopeResolverCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			setCmd := findCommand(t, "hub", tc.name, "set")
			restoreSilenceUsage(t, setCmd)

			resolveFromPreRun := func() (hubScopeTarget, error) {
				require.NoError(t, resolveInvocationTargetWith(setCmd, ModeHuman, invocationResolution(setCmd.ErrOrStderr())))
				resolvedPath, _, err := config.ResolveProjectPath(projectPath)
				require.NoError(t, err)
				settings, err := loadSettingsForTarget(resolvedPath)
				require.NoError(t, err)
				return tc.resolve(tc.newCmd(t), settings)
			}

			_, err := resolveFromPreRun()
			require.Error(t, err, "no context project: the error remains")
			assert.True(t, errors.Is(err, errCannotInferProject))

			resetContextTargets()
			saveTestContext(t, ctxTestHub, ctxTestProjectID)
			got, err := resolveFromPreRun()
			require.NoError(t, err)
			assert.Equal(t, hubScopeTarget{scope: "project", id: ctxTestProjectID, source: scopeSourceContext}, got)
			assert.Equal(t, targetSourceContext, projectSource)
		})
	}
}

// scopeWriteFakeHub is a fake hub with project "foo" (ctxTestProjectID). It
// records the scopeId of each env or secret write, update or delete, in
// order, and the path of every API request.
func scopeWriteFakeHub(t *testing.T, scopeIDs, apiPaths *[]string) *httptest.Server {
	t.Helper()
	foo := map[string]string{"id": ctxTestProjectID, "name": "Foo", "slug": "foo"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			*apiPaths = append(*apiPaths, r.URL.Path)
		}
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/"+ctxTestProjectID && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(foo)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/env/"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["scopeId"].(string)
			*scopeIDs = append(*scopeIDs, id)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"envVar":  map[string]interface{}{"key": "FOO", "scope": "project", "injectionMode": "as_needed"},
				"created": true,
			})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/secrets/"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["scopeId"].(string)
			*scopeIDs = append(*scopeIDs, id)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"secret":  map[string]interface{}{"key": "FOO", "scope": "project", "type": "environment", "version": 1},
				"created": true,
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/secrets/"):
			*scopeIDs = append(*scopeIDs, r.URL.Query().Get("scopeId"))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"key": "FOO", "scope": "project", "type": "environment", "version": 2})
		case r.Method == http.MethodDelete && (strings.HasPrefix(r.URL.Path, "/api/v1/env/") || strings.HasPrefix(r.URL.Path, "/api/v1/secrets/")):
			*scopeIDs = append(*scopeIDs, r.URL.Query().Get("scopeId"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// scopeWriteCase is one writing or clearing subcommand of hub env or hub
// secret, run on a bare --project command after the root hook resolved the
// invocation.
type scopeWriteCase struct {
	kind string // env or secret
	sub  string // set, update or clear
	run  func(cmd *cobra.Command) error
}

func (c scopeWriteCase) name() string { return c.kind + "/" + c.sub }

func scopeWriteCases() []scopeWriteCase {
	return []scopeWriteCase{
		{"env", "set", func(cmd *cobra.Command) error { return runEnvSet(cmd, []string{"FOO=bar"}) }},
		{"env", "clear", func(cmd *cobra.Command) error { return runEnvClear(cmd, []string{"FOO"}) }},
		{"secret", "set", func(cmd *cobra.Command) error { return runSecretSet(cmd, []string{"FOO", "bar"}) }},
		{"secret", "update", func(cmd *cobra.Command) error { return runSecretUpdate(cmd, []string{"FOO"}) }},
		{"secret", "clear", func(cmd *cobra.Command) error { return runSecretClear(cmd, []string{"FOO"}) }},
	}
}

// runScopeWrite runs tc's subcommand with a bare --project against a fake
// hub, with the context set to contextProject on that hub ("" for none),
// from the current directory.
func runScopeWrite(t *testing.T, tc scopeWriteCase, contextProject string) scopeWriteResult {
	t.Helper()
	var r scopeWriteResult
	srv := scopeWriteFakeHub(t, &r.scopeIDs, &r.apiPaths)
	saveTestContext(t, srv.URL, contextProject)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	t.Setenv("SCION_HUB_PROJECT_ID", "")
	origEnv := saveEnvTestState()
	origSecret := saveSecretTestState()
	t.Cleanup(func() { origEnv.restore(); origSecret.restore() })
	envAlways, envAsNeeded, envSecret = false, false, false
	secretType, secretTarget, secretAllowProgeny = "", "", false

	realCmd := findCommand(t, "hub", tc.kind, tc.sub)
	require.NoError(t, resolveInvocationTargetWith(realCmd, ModeHuman, invocationResolution(realCmd.ErrOrStderr())))

	var newCmd func(t *testing.T) *cobra.Command
	for _, rc := range scopeResolverCases() {
		if rc.name == tc.kind {
			newCmd = rc.newCmd
		}
	}
	cmd := newCmd(t)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	r.err = tc.run(cmd)
	r.stdout, r.stderr = out.String(), errOut.String()
	return r
}

// scopeWriteResult is what runScopeWrite observed: the command's error, the
// scopeIds written, the API paths requested, stdout and stderr.
type scopeWriteResult struct {
	err            error
	scopeIDs       []string
	apiPaths       []string
	stdout, stderr string
}

// Every writing or clearing subcommand with a bare --project and only the
// 'scion use' context acts on that project, and first names it (name, then
// slug) and where it came from on stderr, so a stale context project is
// never written or cleared silently. stdout keeps only the command's own
// result.
func TestScopeWrite_NamesResolvedProjectFromUseContext(t *testing.T) {
	for _, tc := range scopeWriteCases() {
		t.Run(tc.name(), func(t *testing.T) {
			setupContextTest(t)
			r := runScopeWrite(t, tc, ctxTestProjectID)
			require.Equal(t, targetSourceContext, projectSource)

			require.NoError(t, r.err)
			assert.Equal(t, []string{ctxTestProjectID}, r.scopeIDs, "the write goes to the context project")
			assert.Contains(t, r.stderr, "Using project Foo (foo) (from the scion use context)\n")
			assert.NotContains(t, r.stdout, "Using project")
		})
	}
}

// A context project the hub does not know (deleted since 'scion use') is an
// error and nothing is written or cleared, although its ID parses as a UUID:
// no writing or clearing subcommand passes a UUID through unchecked.
func TestScopeWrite_UnknownContextProjectFailsClosed(t *testing.T) {
	const gone = "99999999-8888-7777-6666-555555555555"
	for _, tc := range scopeWriteCases() {
		t.Run(tc.name(), func(t *testing.T) {
			setupContextTest(t)
			r := runScopeWrite(t, tc, gone)
			require.Equal(t, targetSourceContext, projectSource)

			require.Error(t, r.err)
			assert.Contains(t, r.err.Error(), "project "+gone+" (from the scion use context) was not found on this hub, so nothing was written")
			assert.Empty(t, r.scopeIDs, "nothing is written")
			assert.NotContains(t, r.stderr, "Using project")
		})
	}
}

// In a directory project that is not linked to a hub, a bare --project is an
// error before any hub request: neither the local project_id nor the
// context project (which a directory project hides) is written to.
func TestScopeWrite_UnlinkedDirectoryMakesNoHubCall(t *testing.T) {
	fixtures := map[string]string{
		"legacy project_id":        "project_id: local-id\n",
		"versioned hub.project_id": "schema_version: \"1\"\nhub:\n  project_id: local-id\n",
	}
	for _, tc := range scopeWriteCases() {
		for fname, settings := range fixtures {
			t.Run(tc.name()+"/"+fname, func(t *testing.T) {
				setupContextTest(t)
				t.Setenv("SCION_HUB_PROJECT_ID", "")
				makeHubOnlyDirProject(t, settings)
				r := runScopeWrite(t, tc, ctxTestProjectID)

				require.Error(t, r.err)
				assert.Contains(t, r.err.Error(), "is not linked to a hub")
				assert.Empty(t, r.scopeIDs, "nothing is written or cleared")
				assert.Empty(t, r.apiPaths, "no hub API request is made")
				assert.NotContains(t, r.stderr, "Using project")
			})
		}
	}
}
