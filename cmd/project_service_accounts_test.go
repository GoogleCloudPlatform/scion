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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile/hubprofiletest"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetFlagChanged clears the Changed bit on the named flags so a shared,
// package-level cobra command can be reused across test cases without
// carrying state from a previous ParseFlags call.
func resetFlagChanged(fs *pflag.FlagSet, names ...string) {
	for _, name := range names {
		if f := fs.Lookup(name); f != nil {
			f.Changed = false
		}
	}
}

// assertNoLocalProjectFlag asserts that cmd does not register its own
// --project flag distinct from the root's persistent --project/-g, and does
// not inherit one from an intermediate parent's PersistentFlags either. It
// forces cobra's mergePersistentFlags via InheritedFlags before looking, so
// the check does not depend on some other test having already triggered
// that merge: mergePersistentFlags copies both cmd's own PersistentFlags
// and every parent's PersistentFlags into cmd's own FlagSet (see
// Command.mergePersistentFlags / Command.updateParentsPflags in
// spf13/cobra, which visits the nearest parent first), and cmd.Flags()
// only reflects that once the merge has run. After forcing it, "project"
// must resolve to a flag, and that flag must be the exact same object
// (pointer identity) as the root's persistent --project — not a distinct
// local flag on cmd itself or on an intermediate parent such as
// projectServiceAccountsCmd. pflag's FlagSet.AddFlagSet only fills in names
// not already present, so any of those three real overrides (the
// regression this guards against) always keeps its own place and is never
// silently replaced by the merge, regardless of run order.
func assertNoLocalProjectFlag(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	_ = cmd.InheritedFlags() // forces mergePersistentFlags
	f := cmd.Flags().Lookup("project")
	require.NotNil(t, f, "%s should inherit the root's --project/-g flag", cmd.Name())
	assert.Same(t, rootCmd.PersistentFlags().Lookup("project"), f,
		"%s must not register or inherit a --project flag distinct from the root's --project/-g", cmd.Name())
}

// TestSAAddCmd_GCPProjectFlagRenamed locks in the rename: the local --project
// flag (GCP project ID) became --gcp-project so it no longer shadows the root
// --project/-g scion-project selector. No alias for the old name is
// registered: an alias called "project" would recreate the shadowing.
func TestSAAddCmd_GCPProjectFlagRenamed(t *testing.T) {
	assert.NotNil(t, saAddCmd.Flags().Lookup("gcp-project"), "add command should register --gcp-project")
	assertNoLocalProjectFlag(t, saAddCmd)
}

// TestSAAddCmd_RootProjectAndGCPProjectFlagsParseIndependently is the
// regression test for the flag shadowing: with the local flag renamed,
// `-g <scion-project> --gcp-project <gcp>` must parse both values into
// their own variables instead of one shadowing the other.
func TestSAAddCmd_RootProjectAndGCPProjectFlagsParseIndependently(t *testing.T) {
	origProjectPath, origSAProjectID := projectPath, saProjectID
	defer func() { projectPath, saProjectID = origProjectPath, origSAProjectID }()
	defer resetFlagChanged(saAddCmd.Flags(), "project", "gcp-project")

	err := saAddCmd.ParseFlags([]string{"-g", "my-scion-project", "--gcp-project", "my-gcp-project"})
	require.NoError(t, err)

	assert.Equal(t, "my-scion-project", projectPath)
	assert.Equal(t, "my-gcp-project", saProjectID)
}

// TestSAAddCmd_OldProjectFlagHint verifies that `add ... --project my-gcp`
// (the pre-rename invocation) produces the targeted hint rather than a
// generic "required flag(s) ... not set" error.
func TestSAAddCmd_OldProjectFlagHint(t *testing.T) {
	origProjectPath, origSAProjectID := projectPath, saProjectID
	defer func() { projectPath, saProjectID = origProjectPath, origSAProjectID }()
	defer resetFlagChanged(saAddCmd.Flags(), "project", "gcp-project")

	err := saAddCmd.ParseFlags([]string{"--project", "my-gcp-project"})
	require.NoError(t, err)

	require.NotNil(t, saAddCmd.Args, "add command must wire the hint check into Args for it to run before the required-flag check")
	err = saAddCmd.Args(saAddCmd, []string{"sa@example.iam.gserviceaccount.com"})
	require.Error(t, err)
	assert.Equal(t, gcpProjectFlagHint, err.Error())
}

// TestSAAddCmd_NoHintWhenGCPProjectSet ensures the hint only fires for the
// old-flag case, not whenever --project happens to be set alongside a
// correctly-specified --gcp-project.
func TestSAAddCmd_NoHintWhenGCPProjectSet(t *testing.T) {
	origProjectPath, origSAProjectID := projectPath, saProjectID
	defer func() { projectPath, saProjectID = origProjectPath, origSAProjectID }()
	defer resetFlagChanged(saAddCmd.Flags(), "project", "gcp-project")

	err := saAddCmd.ParseFlags([]string{"-g", "my-scion-project", "--gcp-project", "my-gcp-project"})
	require.NoError(t, err)

	require.NotNil(t, saAddCmd.Args)
	require.NoError(t, saAddCmd.Args(saAddCmd, []string{"sa@example.iam.gserviceaccount.com"}))
}

// TestSAAddCmd_NoHintWhenNoProjectFlags ensures a bare invocation, with
// neither --project nor --gcp-project set, does not trigger the hint: that
// case must fall through to cobra's normal required-flag error instead.
func TestSAAddCmd_NoHintWhenNoProjectFlags(t *testing.T) {
	origProjectPath, origSAProjectID := projectPath, saProjectID
	defer func() { projectPath, saProjectID = origProjectPath, origSAProjectID }()
	defer resetFlagChanged(saAddCmd.Flags(), "project", "gcp-project")

	err := saAddCmd.ParseFlags([]string{})
	require.NoError(t, err)

	require.NotNil(t, saAddCmd.Args)
	require.NoError(t, saAddCmd.Args(saAddCmd, []string{"sa@example.iam.gserviceaccount.com"}))
}

type saResolverMockHub struct {
	mu                sync.Mutex
	requests          []string
	mintBodies        []map[string]any
	forbidProjectRead bool
}

func (m *saResolverMockHub) recordedRequests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.requests...)
}

func (m *saResolverMockHub) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = nil
	m.mintBodies = nil
}

func newSAResolverMockServer(t *testing.T) (*saResolverMockHub, *httptest.Server) {
	t.Helper()
	mock := &saResolverMockHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mock.mu.Lock()
		mock.requests = append(mock.requests, r.Method+" "+r.URL.RequestURI())
		forbid := mock.forbidProjectRead
		mock.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+testHubProjectID:
			if forbid {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"missing project:read"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(hubclient.Project{
				ID:   testHubProjectID,
				Name: "Scion Fork",
				Slug: "scion-fork",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+testOtherProjectID:
			if forbid {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"missing project:read"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(hubclient.Project{
				ID:   testOtherProjectID,
				Name: "Other Project",
				Slug: "other-proj",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			q := r.URL.Query()
			var projects []hubclient.Project
			switch {
			case q.Get("slug") == "scion-fork" || q.Get("name") == "scion-fork":
				projects = append(projects, hubclient.Project{
					ID:   testHubProjectID,
					Name: "Scion Fork",
					Slug: "scion-fork",
				})
			case q.Get("slug") == "other-proj" || q.Get("name") == "other-proj":
				projects = append(projects, hubclient.Project{
					ID:   testOtherProjectID,
					Name: "Other Project",
					Slug: "other-proj",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": projects})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/gcp-service-accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []hubclient.GCPServiceAccount{{
					ID:        "sa-listed-1",
					Scope:     "project",
					ScopeID:   r.URL.Query().Get("scopeId"),
					Email:     "listed@gcp-proj.iam.gserviceaccount.com",
					ProjectID: "gcp-proj",
					Verified:  true,
				}},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status") && strings.Contains(r.URL.Path, "/gcp-service-accounts/"):
			_, _ = w.Write([]byte(saStatusCLIBody))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/gcp-service-accounts/mint"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mock.mu.Lock()
			mock.mintBodies = append(mock.mintBodies, body)
			mock.mu.Unlock()
			_ = json.NewEncoder(w).Encode(hubclient.GCPServiceAccount{
				ID:          "sa-minted-1",
				Scope:       "project",
				Email:       "scion-my-pipe@hub-gcp.iam.gserviceaccount.com",
				ProjectID:   "hub-gcp",
				DisplayName: "My Pipe",
				Verified:    true,
				Managed:     true,
				Warnings:    []string{"no broker profile maps this service account"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return mock, srv
}

func isolateProjectSACLIState(t *testing.T) string {
	t.Helper()
	setupNoProjectPreRun(t)
	restoreAllSilenceUsage(t)

	origHubEndpoint, origHubSource := hubEndpoint, hubSource
	origProjectSource, origContextSlug := projectSource, contextProjectSlug
	origSAJSON, origSAMintID, origSAName, origSAProj := saOutputJSON, saMintID, saDisplayName, saProjectID
	origHubProfile, origHubAs, origHubRun := hubProfileFlag, hubProfileAs, hubProfileRun
	t.Cleanup(func() {
		hubEndpoint, hubSource = origHubEndpoint, origHubSource
		projectSource, contextProjectSlug = origProjectSource, origContextSlug
		saOutputJSON, saMintID, saDisplayName, saProjectID = origSAJSON, origSAMintID, origSAName, origSAProj
		hubProfileFlag, hubProfileAs, hubProfileRun = origHubProfile, origHubAs, origHubRun
		resetProjectSAFlags()
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	for _, k := range []string{
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID",
		"SCION_PROJECT", "SCION_AUTH_TOKEN", "SCION_HUB_TOKEN", "SCION_DEV_TOKEN",
		"SCION_DEV_TOKEN_FILE", "SCION_CLI_MODE", "SCION_HOST_UID", "SCION_NETWORK_MODE",
		"SCION_HUB_PROFILE", "SCION_HUB_RUN",
	} {
		unsetEnvForTest(t, k)
	}

	cwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(cwd)
	return cwd
}

func resetProjectSAFlags() {
	for _, c := range []*cobra.Command{projectServiceAccountsCmd, saAddCmd, saListCmd, saShowCmd, saRemoveCmd, saVerifyCmd, saMintCmd} {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
	rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	projectPath, projectSource, contextProjectSlug = "", "", ""
	globalMode = false
	hubEndpoint, hubSource = "", ""
	outputFormat = ""
	saOutputJSON = false
	saMintID, saDisplayName, saProjectID = "", "", ""
}

func executeProjectSACLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	resetProjectSAFlags()
	rootCmd.SetArgs(append([]string{"project", "service-accounts"}, args...))
	stdout, stderr = captureStdoutStderr(t, func() {
		err = rootCmd.Execute()
	})
	return stdout, stderr, err
}

// TestProjectServiceAccounts_NoLocalProjectFlagOnAnySubcommand ensures none of
// the project service-accounts commands shadow rootCmd's persistent --project/-g.
func TestProjectServiceAccounts_NoLocalProjectFlagOnAnySubcommand(t *testing.T) {
	for _, c := range []*cobra.Command{projectServiceAccountsCmd, saAddCmd, saListCmd, saShowCmd, saRemoveCmd, saVerifyCmd, saMintCmd} {
		assertNoLocalProjectFlag(t, c)
	}
}

// TestSAMintCmd_JSONFlag verifies ptone/scion#8301: 'scion project service-accounts mint'
// registers --json (matching list and show), writes advisory warnings to stderr,
// and emits pure JSON on stdout without requiring a local .scion/settings.yaml.
func TestSAMintCmd_JSONFlag(t *testing.T) {
	_ = isolateProjectSACLIState(t)

	require.NotNil(t, saMintCmd.Flags().Lookup("json"), "mint command must register --json")

	mock, srv := newSAResolverMockServer(t)
	t.Setenv("SCION_HUB_TOKEN", "user-token")

	stdout, stderr, err := executeProjectSACLI(t,
		"mint",
		"--hub", srv.URL,
		"--project", "scion-fork",
		"--account-id", "my-pipe",
		"--name", "My Pipe",
		"--json",
	)
	require.NoError(t, err)
	assert.Contains(t, stderr, "Warning: no broker profile maps this service account")

	var got hubclient.GCPServiceAccount
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must be valid JSON: %q", stdout)
	assert.Equal(t, "sa-minted-1", got.ID)
	assert.Equal(t, "scion-my-pipe@hub-gcp.iam.gserviceaccount.com", got.Email)
	assert.True(t, got.Managed)
	assert.Contains(t, mock.recordedRequests(), "POST /api/v1/projects/"+testHubProjectID+"/gcp-service-accounts/mint")
	require.Len(t, mock.mintBodies, 1)
	assert.Equal(t, "my-pipe", mock.mintBodies[0]["account_id"])
	assert.Equal(t, "My Pipe", mock.mintBodies[0]["display_name"])

	// Plain-text mode (without --json) still prints human-readable output.
	mock.reset()
	plainOut, _, err := executeProjectSACLI(t,
		"mint",
		"--hub", srv.URL,
		"--project", "scion-fork",
	)
	require.NoError(t, err)
	assert.Contains(t, plainOut, "Minted service account: scion-my-pipe@hub-gcp.iam.gserviceaccount.com")
}

// TestProjectServiceAccounts_ResolvesProjectWithoutLocalSettings verifies
// ptone/scion#8301 outside any local .scion directory: --project/-g with a slug
// or UUID (including when project:read is forbidden on a UUID), and 'scion use'
// context resolution without a local .scion/settings.yaml.
func TestProjectServiceAccounts_ResolvesProjectWithoutLocalSettings(t *testing.T) {
	cwd := isolateProjectSACLIState(t)
	home := os.Getenv("HOME")

	mock, srv := newSAResolverMockServer(t)
	t.Setenv("SCION_HUB_TOKEN", "user-token")

	// Confirm neither HOME/.scion/settings.yaml nor cwd/.scion exists.
	_, err := os.Stat(filepath.Join(home, ".scion", "settings.yaml"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(cwd, ".scion"))
	require.True(t, os.IsNotExist(err))

	// 1. --project <slug> and -g <slug> resolve via Hub API to testHubProjectID.
	for _, flagArgs := range [][]string{{"--project", "scion-fork"}, {"-g", "scion-fork"}} {
		mock.reset()
		args := append([]string{"list", "--hub", srv.URL, "--json"}, flagArgs...)
		stdout, _, err := executeProjectSACLI(t, args...)
		require.NoError(t, err)
		assert.Contains(t, mock.recordedRequests(), "GET /api/v1/gcp-service-accounts?scope=project&scopeId="+testHubProjectID)
		var sas []hubclient.GCPServiceAccount
		require.NoError(t, json.Unmarshal([]byte(stdout), &sas))
		require.Len(t, sas, 1)
		assert.Equal(t, testHubProjectID, sas[0].ScopeID)
	}

	// 2. --project <uuid> works even if GET /api/v1/projects/<uuid> returns 403 Forbidden.
	mock.reset()
	mock.forbidProjectRead = true
	stdout, _, err := executeProjectSACLI(t, "show", "Worker SA", "--hub", srv.URL, "--project", testHubProjectID)
	mock.forbidProjectRead = false
	require.NoError(t, err)
	assert.Contains(t, mock.recordedRequests(), "GET /api/v1/projects/"+testHubProjectID+"/gcp-service-accounts/Worker%20SA/status")
	assert.Contains(t, stdout, "Service account: Worker")

	// 3. 'scion use' context resolves both Hub endpoint and project ID without any flags or settings.yaml.
	mock.reset()
	saveTestContext(t, srv.URL, testHubProjectID)
	stdout, _, err = executeProjectSACLI(t, "list", "--json")
	require.NoError(t, err)
	assert.Contains(t, mock.recordedRequests(), "GET /api/v1/gcp-service-accounts?scope=project&scopeId="+testHubProjectID)
	var sas []hubclient.GCPServiceAccount
	require.NoError(t, json.Unmarshal([]byte(stdout), &sas))
	require.Len(t, sas, 1)
	assert.Equal(t, testHubProjectID, sas[0].ScopeID)

	// 4. Explicit --project <other-slug> overrides the ambient 'scion use' context project.
	mock.reset()
	stdout, _, err = executeProjectSACLI(t, "list", "--project", "other-proj", "--json")
	require.NoError(t, err)
	assert.Contains(t, mock.recordedRequests(), "GET /api/v1/gcp-service-accounts?scope=project&scopeId="+testOtherProjectID)
	sas = nil
	require.NoError(t, json.Unmarshal([]byte(stdout), &sas))
	require.Len(t, sas, 1)
	assert.Equal(t, testOtherProjectID, sas[0].ScopeID)

	// Nothing created in cwd.
	assertNoEntries(t, cwd)
}

// TestProjectServiceAccounts_HubProfileResolution verifies ptone/scion#8301 for
// --hub-profile: the resolver uses the profile client (reading no local .scion
// settings even if unreadable on disk), requires --project, rejects --global,
// and resolves --project <slug-or-id> on the target Hub.
func TestProjectServiceAccounts_HubProfileResolution(t *testing.T) {
	cwd := isolateProjectSACLIState(t)
	home := os.Getenv("HOME")

	clk := hubprofiletest.NewClock()
	gcp := hubprofiletest.NewGCP(t, clk)
	target := hubprofiletest.NewTarget(t, clk, false)

	var seenScopeID string
	target.Handle = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			var out []hubclient.Project
			q := r.URL.Query()
			if q.Get("slug") == x3bProjectName || q.Get("name") == x3bProjectName {
				out = append(out, hubclient.Project{ID: x3bProjectID, Name: x3bProjectName, Slug: x3bProjectName})
			}
			x3bJSON(w, http.StatusOK, map[string]any{"projects": out})
			return true
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+x3bProjectID:
			x3bJSON(w, http.StatusOK, hubclient.Project{ID: x3bProjectID, Name: x3bProjectName, Slug: x3bProjectName})
			return true
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
			x3bJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "project not found"}})
			return true
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/gcp-service-accounts":
			seenScopeID = r.URL.Query().Get("scopeId")
			x3bJSON(w, http.StatusOK, map[string]any{"items": []hubclient.GCPServiceAccount{}})
			return true
		}
		return false
	}

	for k, v := range gcp.Env() {
		t.Setenv(k, v)
	}
	origDeps := hubProfileDeps
	hubProfileDeps = func() hubprofile.Deps { return hubprofile.Deps{IAMCredentialsURL: gcp.IAM.URL} }
	t.Cleanup(func() { hubProfileDeps = origDeps })

	require.NoError(t, hubprofile.Save(home, &hubprofile.Profile{
		Name:                 "t1",
		Endpoint:             target.Srv.URL,
		Transport:            hubprofile.TransportNone,
		IssuerServiceAccount: hubprofiletest.IssuerSA,
		IssuerDelegations:    map[string]string{"member": hubprofiletest.Delegation},
	}, false))

	// Put an unreadable local .scion/settings.json in cwd to prove profile mode never reads it.
	dotScion := filepath.Join(cwd, ".scion")
	require.NoError(t, os.MkdirAll(dotScion, 0o755))
	settingsFile := filepath.Join(dotScion, "settings.json")
	require.NoError(t, os.WriteFile(settingsFile, []byte(`{"hub":{"enabled":true,"endpoint":"http://127.0.0.1:1"}}`), 0o000))
	t.Cleanup(func() { _ = os.Chmod(settingsFile, 0o600) })

	hubProfileFlag = "t1"
	hubProfileRun = "r1"
	sess, err := openHubProfileIssuerSession()
	require.NoError(t, err)
	_, err = hubprofile.IssueToCache(t.Context(), sess, hubprofile.IssueRequest{Alias: "owner"})
	require.NoError(t, err)
	hubProfileAs = "owner"

	// 1. Without --project: fails asking for --project, not 'scion hub link'.
	projectPath = ""
	globalMode = false
	_, _, err = resolveLinkedProjectForSA()
	require.ErrorIs(t, err, errProfileNeedsProject)

	// 2. With --global: fails rejecting --global under a hub profile.
	globalMode = true
	_, _, err = resolveLinkedProjectForSA()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--global does not apply with a hub profile")
	globalMode = false

	// 3. With --project <slug>: resolves slug on target hub and runs list cleanly.
	projectPath = x3bProjectName
	saOutputJSON = true
	out := captureStdout(t, func() {
		require.NoError(t, runSAList(nil, nil))
	})
	assert.Equal(t, x3bProjectID, seenScopeID)
	assert.Contains(t, out, "[]")
}

// TestProjectServiceAccounts_UnlinkedDirectoryAndContainerEnv verifies that an
// unlinked checkout fails closed instead of sending its local project-id or
// leaking an ambient 'scion use' project, while inside an agent container
// SCION_PROJECT_ID takes precedence over the cloned checkout's .scion/project-id.
func TestProjectServiceAccounts_UnlinkedDirectoryAndContainerEnv(t *testing.T) {
	t.Run("unlinked directory fails closed", func(t *testing.T) {
		cwd := isolateProjectSACLIState(t)
		repoDir := filepath.Join(cwd, "unlinked-repo")
		dotScion := filepath.Join(repoDir, ".scion")
		require.NoError(t, os.MkdirAll(dotScion, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dotScion, "settings.yaml"),
			[]byte("schema_version: \"1\"\nhub:\n  project_id: "+testCheckoutLocalID+"\n"), 0o644))
		t.Chdir(repoDir)

		mock, srv := newSAResolverMockServer(t)
		saveTestContext(t, srv.URL, testHubProjectID)
		t.Setenv("SCION_HUB_TOKEN", "user-token")

		_, _, err := executeProjectSACLI(t, "list")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not linked to a hub")
		assert.Empty(t, mock.recordedRequests(), "unlinked directory must fail before making any Hub API call")
	})

	t.Run("agent container uses SCION_PROJECT_ID over checkout project-id", func(t *testing.T) {
		cwd := isolateProjectSACLIState(t)
		dotScion := filepath.Join(cwd, ".scion")
		require.NoError(t, os.MkdirAll(dotScion, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dotScion, "project-id"), []byte(testCheckoutLocalID+"\n"), 0o644))

		mock, srv := newSAResolverMockServer(t)
		t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
		t.Setenv("SCION_PROJECT_ID", testHubProjectID)
		t.Setenv("SCION_PROJECT", "scion-fork")
		t.Setenv("SCION_AUTH_TOKEN", "agent-token")
		t.Setenv("SCION_CLI_MODE", "agent")
		t.Setenv("SCION_HOST_UID", "1000")
		t.Setenv("SCION_NETWORK_MODE", "host")

		stdout, _, err := executeProjectSACLI(t, "show", "Worker SA")
		require.NoError(t, err)
		assert.Contains(t, mock.recordedRequests(), "GET /api/v1/projects/"+testHubProjectID+"/gcp-service-accounts/Worker%20SA/status")
		assert.Contains(t, stdout, "Service account: Worker")
	})
}
