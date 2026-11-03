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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#5294: a project lookup that finds nothing under a
// user access token gives the scoped-token hint, which reads the same
// whether or not the project exists; any other credential keeps the plain
// not-found text and every exit code is unchanged.

const (
	uatLookupProjectID   = "6a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	uatLookupProjectName = "Secret Project"
	uatLookupProjectSlug = "secret"
	uatLookupToken       = "scion_pat_lookuptest"
)

// uatLookupHub is a hub with one project. With hidden set it answers as
// the hub answers a user access token without project:read and
// project:list: GET by ID is 404 and every list is empty, so to the CLI the
// project looks exactly as if it did not exist. With present unset there is
// no project at all. A list without a name or slug filter returns every
// visible project. writes, when set, counts requests other than GET, which
// this hub never expects.
type uatLookupHub struct {
	present bool
	hidden  bool
	writes  *atomic.Int32
}

func (h uatLookupHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet && h.writes != nil {
		h.writes.Add(1)
	}
	visible := h.present && !h.hidden
	p := hubclient.Project{ID: uatLookupProjectID, Name: uatLookupProjectName, Slug: uatLookupProjectSlug}
	switch {
	case r.URL.Path == "/healthz":
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	case r.URL.Path == "/api/v1/projects":
		q := r.URL.Query()
		matches := []hubclient.Project{}
		unfiltered := q.Get("name") == "" && q.Get("slug") == "" && q.Get("gitRemote") == ""
		if visible && (unfiltered || strings.EqualFold(q.Get("name"), p.Name) || q.Get("slug") == p.Slug) {
			matches = append(matches, p)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": matches})
		return
	case visible && r.URL.Path == "/api/v1/projects/"+p.ID:
		_ = json.NewEncoder(w).Encode(p)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"project not found"}}`))
}

// uatLookupPair is the two hubs of an exists / does-not-exist pair: the
// project exists but is hidden from the token, and the project does not
// exist.
func uatLookupPair() map[string]uatLookupHub {
	return map[string]uatLookupHub{
		"exists":         {present: true, hidden: true},
		"does not exist": {present: false},
	}
}

func uatLookupClient(t *testing.T, h uatLookupHub) hubclient.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return client
}

// assertScopedLookupError checks the scoped-token error: it wraps
// ErrScopedTokenProjectNotFound, names ref and the missing scopes, and
// never says the lookup was not permitted.
func assertScopedLookupError(t *testing.T, err error, ref string) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, errors.Is(err, hubsync.ErrScopedTokenProjectNotFound), "err = %v", err)
	for _, want := range []string{"'" + ref + "'", "project:read", "project:list"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NotContains(t, strings.ToLower(err.Error()), "not permitted")
}

func TestAuthSourceIsUserAccessToken(t *testing.T) {
	cases := []struct {
		source string
		token  string
		want   bool
	}{
		{hubAuthSourceEnvHubToken, uatLookupToken, true},
		{hubAuthSourceEnvHubToken, "a-plain-bearer-token", false},
		{hubAuthSourceStoredLogin, uatLookupToken, false},
		{hubAuthSourceAgentTokenFile, uatLookupToken, false},
		{hubAuthSourceEnvAuthToken, uatLookupToken, false},
		{hubAuthSourceDev, uatLookupToken, false},
		{hubAuthSourceHubProfile, uatLookupToken, false},
		{hubAuthSourceNone, uatLookupToken, false},
	}
	for _, tc := range cases {
		t.Run(tc.source+"/"+tc.token, func(t *testing.T) {
			t.Setenv("SCION_HUB_TOKEN", tc.token)
			assert.Equal(t, tc.want, authSourceIsUserAccessToken(tc.source))
		})
	}
}

// TestProjectResolvers_UATExistsAndMissingReadTheSame covers the name-or-ID
// resolver (project skills, clone, rename, hub env, pre-start-hook, broker
// --project, hub projects by UUID) and the name lookup (hub projects by
// name): under a user access token, the project hidden from the token and
// the project that does not exist give the identical scoped-token error.
func TestProjectResolvers_UATExistsAndMissingReadTheSame(t *testing.T) {
	resolvers := map[string]struct {
		ref     string
		resolve func(context.Context, hubclient.Client, string, bool) (*hubclient.Project, error)
	}{
		"resolveProjectByNameOrID slug": {uatLookupProjectSlug, resolveProjectByNameOrID},
		"resolveProjectByNameOrID name": {uatLookupProjectName, resolveProjectByNameOrID},
		"resolveProjectByNameOrID id":   {uatLookupProjectID, resolveProjectByNameOrID},
		"findProjectByName":             {uatLookupProjectName, findProjectByName},
		"resolveProjectNameOrID name":   {uatLookupProjectName, resolveProjectNameOrID},
		"resolveProjectNameOrID id":     {uatLookupProjectID, resolveProjectNameOrID},
	}
	for name, rv := range resolvers {
		t.Run(name, func(t *testing.T) {
			msgs := map[string]string{}
			for pair, h := range uatLookupPair() {
				_, err := rv.resolve(context.Background(), uatLookupClient(t, h), rv.ref, true)
				assertScopedLookupError(t, err, rv.ref)
				msgs[pair] = err.Error()
			}
			assert.Equal(t, msgs["exists"], msgs["does not exist"], "the message must not reveal whether the project exists")

			// Visible to the token: resolved as before.
			p, err := rv.resolve(context.Background(), uatLookupClient(t, uatLookupHub{present: true}), rv.ref, true)
			require.NoError(t, err)
			assert.Equal(t, uatLookupProjectID, p.ID)
		})
	}
}

// TestProjectResolvers_SessionKeepsPlainNotFound pins that a caller without
// a user access token keeps the existing not-found text exactly.
func TestProjectResolvers_SessionKeepsPlainNotFound(t *testing.T) {
	missing := uatLookupClient(t, uatLookupHub{})
	for _, ref := range []string{uatLookupProjectSlug, uatLookupProjectID} {
		_, err := resolveProjectByNameOrID(context.Background(), missing, ref, false)
		require.Error(t, err)
		assert.Equal(t, "project '"+ref+"' not found", err.Error())
	}
	_, err := findProjectByName(context.Background(), missing, uatLookupProjectName, false)
	require.Error(t, err)
	assert.Equal(t, "project '"+uatLookupProjectName+"' not found", err.Error())
	_, err = resolveProjectNameOrID(context.Background(), missing, uatLookupProjectName, false)
	require.Error(t, err)
	assert.Equal(t, "project '"+uatLookupProjectName+"' not found", err.Error())
}

// setupUATCommandTest points the CLI at hub h with no stored login and no
// agent token, so the credential sent is SCION_HUB_TOKEN when it is set.
func setupUATCommandTest(t *testing.T, h uatLookupHub, hubToken string) {
	t.Helper()
	origHome := os.Getenv("HOME")
	origProjectPath := projectPath
	t.Cleanup(func() {
		_ = os.Setenv("HOME", origHome)
		projectPath = origProjectPath
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_HUB_TOKEN", hubToken)
	projectPath = setupEnvProject(t, tmpHome, srv.URL)
}

// TestRunHubProjectsInfo_UATHint covers the command wiring (getHubClientUAT
// through hubProjectsTarget) for hub projects info, by name and by UUID.
func TestRunHubProjectsInfo_UATHint(t *testing.T) {
	for _, ref := range []string{uatLookupProjectName, uatLookupProjectID} {
		t.Run(ref, func(t *testing.T) {
			msgs := map[string]string{}
			for pair, h := range uatLookupPair() {
				setupUATCommandTest(t, h, uatLookupToken)
				err := runHubProjectsInfo(hubProjectsInfoCmd, []string{ref})
				assertScopedLookupError(t, err, ref)
				msgs[pair] = err.Error()
			}
			assert.Equal(t, msgs["exists"], msgs["does not exist"])

			// A bearer token that is not a user access token keeps the
			// plain message.
			setupUATCommandTest(t, uatLookupHub{}, "a-plain-bearer-token")
			err := runHubProjectsInfo(hubProjectsInfoCmd, []string{ref})
			require.Error(t, err)
			assert.Equal(t, "project '"+ref+"' not found", err.Error())
		})
	}
}

// TestRunHubProjectsDelete_UATHint: hub projects delete gives the same
// hint and sends no request other than GET, so it deletes nothing.
func TestRunHubProjectsDelete_UATHint(t *testing.T) {
	origYes := autoConfirm
	t.Cleanup(func() { autoConfirm = origYes })
	autoConfirm = true
	msgs := map[string]string{}
	for pair, h := range uatLookupPair() {
		var writes atomic.Int32
		h.writes = &writes
		setupUATCommandTest(t, h, uatLookupToken)
		err := runHubProjectsDelete(hubProjectsDeleteCmd, []string{uatLookupProjectName})
		assertScopedLookupError(t, err, uatLookupProjectName)
		assert.Zero(t, writes.Load(), "%s: a request other than GET reached the hub", pair)
		msgs[pair] = err.Error()
	}
	assert.Equal(t, msgs["exists"], msgs["does not exist"])

	// A bearer token that is not a user access token keeps the plain
	// message, and deletes nothing either.
	var writes atomic.Int32
	setupUATCommandTest(t, uatLookupHub{writes: &writes}, "a-plain-bearer-token")
	err := runHubProjectsDelete(hubProjectsDeleteCmd, []string{uatLookupProjectName})
	require.Error(t, err)
	assert.Equal(t, "project '"+uatLookupProjectName+"' not found", err.Error())
	assert.Zero(t, writes.Load())
}

// TestRunProjectHealth_UATHint covers scion project status <name>: it lists
// every project and matches the name, slug or ID itself, so under a user
// access token an empty list gives the scoped-token hint, the same whether
// or not the project exists.
func TestRunProjectHealth_UATHint(t *testing.T) {
	origAll, origJSON, origFormat := projectHealthAll, projectHealthJSON, outputFormat
	t.Cleanup(func() { projectHealthAll, projectHealthJSON, outputFormat = origAll, origJSON, origFormat })
	projectHealthAll, projectHealthJSON = false, false
	for _, ref := range []string{uatLookupProjectSlug, uatLookupProjectName, uatLookupProjectID} {
		t.Run(ref, func(t *testing.T) {
			msgs := map[string]string{}
			for pair, h := range uatLookupPair() {
				setupUATCommandTest(t, h, uatLookupToken)
				err := runProjectHealth(projectHealthCmd, []string{ref})
				assertScopedLookupError(t, err, ref)
				msgs[pair] = err.Error()
			}
			assert.Equal(t, msgs["exists"], msgs["does not exist"])

			// A bearer token that is not a user access token keeps the
			// plain message.
			setupUATCommandTest(t, uatLookupHub{}, "a-plain-bearer-token")
			err := runProjectHealth(projectHealthCmd, []string{ref})
			require.Error(t, err)
			assert.Equal(t, "project '"+ref+"' not found on Hub", err.Error())
		})
	}
}

// setupUATUseTest is setupUseTest with hub h in place of useFakeHub.
func setupUATUseTest(t *testing.T, h uatLookupHub) {
	t.Helper()
	setupContextTest(t)
	restoreAllSilenceUsage(t)
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	withTerminal(t, false)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	writeHomeGlobalSettings(t, "hub:\n  endpoint: "+srv.URL+"\n")
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("local", srv.URL)
	}))
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_AUTH_TOKEN", "")
}

// TestUseSelectProject_UATHint covers project use/select: a hub searched
// with a user access token adds the scoped-token hint to the not-found
// error, the same whether or not the project exists.
func TestUseSelectProject_UATHint(t *testing.T) {
	const base = "project '" + uatLookupProjectSlug + "' not found on hubs: local"
	msgs := map[string]string{}
	for pair, h := range uatLookupPair() {
		setupUATUseTest(t, h)
		t.Setenv("SCION_HUB_TOKEN", uatLookupToken)
		_, _, err := runScion(t, "use", "--hub", "local", uatLookupProjectSlug)
		assertScopedLookupError(t, err, uatLookupProjectSlug)
		assert.True(t, strings.HasPrefix(err.Error(), base+"\n\n"), "err = %v", err)
		msgs[pair] = err.Error()
		assertNothingSelected(t)
	}
	assert.Equal(t, msgs["exists"], msgs["does not exist"])

	// A bearer token that is not a user access token keeps the plain
	// message.
	setupUATUseTest(t, uatLookupHub{})
	t.Setenv("SCION_HUB_TOKEN", "a-plain-bearer-token")
	_, _, err := runScion(t, "use", "--hub", "local", uatLookupProjectSlug)
	require.Error(t, err)
	assert.Equal(t, base, err.Error())
}

// TestResolveSSHProjectID_UATHint covers ssh, ssh-config and vscode: under
// a user access token the not-found line carries the one-line hint, the
// same whether or not the project exists, and the exit code stays
// ExitAgentNotFound.
func TestResolveSSHProjectID_UATHint(t *testing.T) {
	stubSSHTransport(t)
	want := "project not found: " + uatLookupProjectSlug + "; " + sshScopedTokenHint
	for pair, h := range uatLookupPair() {
		t.Run(pair, func(t *testing.T) {
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			conn, err := newSSHHubConn(srv.URL, uatLookupToken)
			require.NoError(t, err)
			_, _, err = conn.resolveSSHProjectID(context.Background(), uatLookupProjectSlug)
			require.Error(t, err)
			assert.Equal(t, tunnelclient.ExitAgentNotFound, tunnelclient.CodeOf(err))
			assert.Equal(t, want, err.Error())
			assert.NotContains(t, err.Error(), "\n")
			assert.NotContains(t, strings.ToLower(err.Error()), "not permitted")
		})
	}

	// Another token keeps the plain line and the exit code.
	srv := httptest.NewServer(uatLookupHub{})
	t.Cleanup(srv.Close)
	conn, err := newSSHHubConn(srv.URL, "a-plain-bearer-token")
	require.NoError(t, err)
	_, _, err = conn.resolveSSHProjectID(context.Background(), uatLookupProjectSlug)
	require.Error(t, err)
	assert.Equal(t, tunnelclient.ExitAgentNotFound, tunnelclient.CodeOf(err))
	assert.Equal(t, "project not found: "+uatLookupProjectSlug, err.Error())
}

// TestResolveSSHTarget_BareAgentKeepsUATHint: for a bare agent the default
// project's not-found message replaces resolveSSHProjectID's; under a user
// access token it keeps the hint, with the same exit code.
func TestResolveSSHTarget_BareAgentKeepsUATHint(t *testing.T) {
	stubSSHTransport(t)
	dflt := hubOnlyProject{ref: uatLookupProjectSlug, label: uatLookupProjectSlug, source: hubOnlySourceFlag}
	msgs := map[string]string{}
	for pair, h := range uatLookupPair() {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		conn, err := newSSHHubConn(srv.URL, uatLookupToken)
		require.NoError(t, err)
		_, err = conn.resolveSSHTarget(context.Background(), "agent", dflt)
		require.Error(t, err)
		assert.Equal(t, tunnelclient.ExitAgentNotFound, tunnelclient.CodeOf(err))
		assert.True(t, strings.HasSuffix(err.Error(), "; "+sshScopedTokenHint), "err = %v", err)
		assert.Contains(t, err.Error(), dflt.describe()+" not found on hub ")
		// The hub URL differs between the two servers; compare the rest.
		msgs[pair] = strings.Replace(err.Error(), srv.URL, "HUB", 1)
	}
	assert.Equal(t, msgs["exists"], msgs["does not exist"])

	srv := httptest.NewServer(uatLookupHub{})
	t.Cleanup(srv.Close)
	conn, err := newSSHHubConn(srv.URL, "a-plain-bearer-token")
	require.NoError(t, err)
	_, err = conn.resolveSSHTarget(context.Background(), "agent", dflt)
	require.Error(t, err)
	assert.Equal(t, tunnelclient.ExitAgentNotFound, tunnelclient.CodeOf(err))
	assert.Equal(t, dflt.notFoundError(srv.URL).Error(), err.Error())
}
