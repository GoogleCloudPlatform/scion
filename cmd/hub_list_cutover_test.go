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
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrokerProcessIgnoresTheHubList: the runtime-broker administration
// commands see the current hub like every hub command, but the command that
// runs the broker process ('runtime-broker start', which runs 'server
// start' in-process with --foreground) and the server commands never read
// the hub list: no hub is filled and no import writes context.yaml. The
// processes resolve their own endpoints from server settings only: with a
// current hub selected, the workstation server still advertises its
// loopback endpoint, its co-located broker reports to it, and a broker-only
// server without server.broker.hub_endpoint reports to no hub.
func TestBrokerProcessIgnoresTheHubList(t *testing.T) {
	isolateServerHubEndpointEnv(t)
	setupContextTest(t)
	enableGlobalHubImport(t)
	writeHomeGlobalSettings(t, "hub:\n  endpoint: "+ctxTestHub+"\n")

	for _, args := range [][]string{{"runtime-broker", "start"}, {"server", "start"}} {
		resetContextTargets()
		require.NoError(t, resolveInvocationTarget(findCommand(t, args...), ModeHuman))
		assert.Empty(t, hubEndpoint, "%v reads no hub list", args)
		_, err := os.Stat(contextFilePath())
		assert.True(t, os.IsNotExist(err), "%v wrote context.yaml", args)
	}

	resetContextTargets()
	require.NoError(t, resolveInvocationTarget(findCommand(t, "runtime-broker", "join"), ModeHuman))
	assert.Equal(t, ctxTestHub, hubEndpoint, "broker administration commands see the current hub")
	assert.Equal(t, targetSourceContext, hubSource)

	cfg := newDeprecationTestConfig()
	got, src := resolveHubEndpointFrom(cfg, workstationMode, "", nil)
	assert.Equal(t, "http://localhost:8080", got, "the workstation server advertises its loopback endpoint")
	assert.Equal(t, hubEndpointSourceLocalhost, src)
	assert.Equal(t, "http://localhost:8080", resolveBrokerHubEndpointFrom(cfg, workstationMode, "", nil))
	assert.Empty(t, resolveBrokerHubEndpointFrom(cfg, brokerOnlyMode, "", nil), "a broker-only server never reports to the current hub")
}

// agentListHub is a fake hub that answers every agent listing with names.
func agentListHub(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/agents") {
			http.NotFound(w, r)
			return
		}
		agents := make([]map[string]string, len(names))
		for i, n := range names {
			agents[i] = map[string]string{"id": n, "name": n}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": agents})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// completionCmd is a command as shell completion sees it: it skips the
// root hook, so nothing filled the hub or the project.
func completionCmd() *cobra.Command {
	c := &cobra.Command{Use: "attach"}
	c.Flags().String("project", "", "")
	c.Flags().Bool("global", false, "")
	return c
}

// TestCompletion_ResolvesTheHubFromTheHubList: agent-name completion, which
// skips the root hook, asks the current hub (not a hub.endpoint in
// settings). It prints nothing and never writes context.yaml: it runs no
// import, and an unreadable context.yaml is silently ignored.
func TestCompletion_ResolvesTheHubFromTheHubList(t *testing.T) {
	setupContextTest(t)
	enableGlobalHubImport(t)
	settingsHub := agentListHub(t, "from-settings")
	currentHub := agentListHub(t, "from-context")
	writeHomeGlobalSettings(t, "hub:\n  endpoint: "+settingsHub.URL+"\n")
	saveTestContext(t, currentHub.URL, "")
	before := readFileT(t, contextFilePath())

	var names []string
	stdout, stderr := captureStdoutStderr(t, func() { names, _ = getAgentNames(completionCmd(), nil, "") })
	assert.Contains(t, names, "from-context")
	assert.NotContains(t, names, "from-settings")
	assert.Empty(t, stdout)
	assert.Empty(t, stderr, "completion prints nothing")
	assert.Equal(t, before, readFileT(t, contextFilePath()), "completion never writes context.yaml (no import)")

	resetContextTargets()
	require.NoError(t, os.WriteFile(contextFilePath(), []byte("version: [1\n"), 0o600))
	stdout, stderr = captureStdoutStderr(t, func() { names, _ = getAgentNames(completionCmd(), nil, "") })
	assert.NotContains(t, names, "from-settings", "an unreadable hub list falls back to no hub, not to settings")
	assert.Empty(t, stdout)
	assert.Empty(t, stderr, "an unreadable context.yaml is ignored silently")
	assert.Equal(t, "version: [1\n", readFileT(t, contextFilePath()))
}

// TestSkillResolverHubOptions_SelectedHub: the hub create uses for skill
// resolution is the hub of the invocation, and a hub from the current
// context never links or registers an already linked project.
func TestSkillResolverHubOptions_SelectedHub(t *testing.T) {
	setupContextTest(t)
	hubEndpoint, hubSource = ctxTestHub, targetSourceContext
	opts := skillResolverHubOptions("")
	assert.Equal(t, ctxTestHub, opts.EndpointOverride)
	assert.True(t, opts.HubFromContext)

	hubEndpoint, hubSource = "https://flag.example.com", ""
	opts = skillResolverHubOptions("")
	assert.Equal(t, "https://flag.example.com", opts.EndpointOverride)
	assert.False(t, opts.HubFromContext)

	hubEndpoint, hubSource = linkEntryTestHub, targetSourceDirectory
	opts = skillResolverHubOptions("")
	assert.Equal(t, linkEntryTestHub, opts.EndpointOverride)
	assert.False(t, opts.HubFromContext)
}

// TestHubStatusJSON_NoSettingsTokenFields: hub status JSON no longer
// reports hub.token or hub.apiKey from settings, which the CLI does not
// read.
func TestHubStatusJSON_NoSettingsTokenFields(t *testing.T) {
	setupContextTest(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	saveTestContext(t, url, "")
	status := findCommand(t, "hub", "status")
	require.NoError(t, resolveInvocationTarget(status, ModeHuman))
	outputFormat = "json"

	out := captureStdout(t, func() { require.NoError(t, runHubStatus(status, nil)) })
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.NotContains(t, got, "hasToken")
	assert.NotContains(t, got, "hasApiKey")
	assert.Equal(t, true, got["configured"])
}

// TestHubStatus_RedactsEndpointCredentials: a hub URL with credentials in
// SCION_HUB_ENDPOINT (a password, or a user name alone, which may be a
// token) is never printed by hub status: not on the Endpoint line, not in
// the JSON, and not in the connection error, which quotes the URL.
func TestHubStatus_RedactsEndpointCredentials(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	for _, tc := range []struct{ userInfo, shown string }{
		{"user:s3cret", "user:xxxxx"},
		{"s3cret", "xxxxx"},
	} {
		t.Run(tc.shown, func(t *testing.T) {
			setupContextTest(t)
			origFormat, origJSON := outputFormat, hubOutputJSON
			t.Cleanup(func() { outputFormat, hubOutputJSON = origFormat, origJSON })
			hubOutputJSON = false
			t.Setenv("SCION_HUB_ENDPOINT", "http://"+tc.userInfo+"@"+hostPort)
			redacted := "http://" + tc.shown + "@" + hostPort
			status := findCommand(t, "hub", "status")
			require.NoError(t, resolveInvocationTarget(status, ModeHuman))

			outputFormat = ""
			var runErr error
			stdout, stderr := captureStdoutStderr(t, func() { runErr = runHubStatus(status, nil) })
			require.NoError(t, runErr)
			assert.Contains(t, stdout, "Endpoint:   "+redacted+"\n")
			assert.Contains(t, stdout, "Connection: failed")
			assert.NotContains(t, stdout+stderr, "s3cret")

			outputFormat = "json"
			stdout, stderr = captureStdoutStderr(t, func() { runErr = runHubStatus(status, nil) })
			require.NoError(t, runErr)
			var got map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
			assert.Equal(t, redacted, got["endpoint"])
			assert.Equal(t, false, got["connected"], "the connection error is reported")
			assert.NotContains(t, stdout+stderr, "s3cret")
		})
	}
}

// TestRedactEndpointIn: the user info of the endpoint is masked wherever
// the text quotes it, in the form url.URL writes and as given; other text
// is unchanged.
func TestRedactEndpointIn(t *testing.T) {
	assert.Equal(t, `Get "https://xxxxx@hub.example.com/healthz": refused`,
		redactEndpointIn(`Get "https://t%40k@hub.example.com/healthz": refused`, "https://t%40k@hub.example.com"))
	assert.Equal(t, "failed: https://xxxxx@hub.example.com/api",
		redactEndpointIn("failed: https://tok@hub.example.com/api", "https://tok@hub.example.com"))
	assert.Equal(t, `Get "https://u:xxxxx@hub.example.com/healthz"`,
		redactEndpointIn(`Get "https://u:xxxxx@hub.example.com/healthz"`, "https://u:pw@hub.example.com"))
	assert.Equal(t, "dial: https://u:xxxxx@hub.example.com",
		redactEndpointIn("dial: https://u:pw@hub.example.com", "https://u:pw@hub.example.com"))
	assert.Equal(t, "plain https://hub.example.com error",
		redactEndpointIn("plain https://hub.example.com error", "https://hub.example.com"))
}

// TestRedactEndpointIn_UnparseableEndpointFailsClosed: an endpoint that
// url.Parse rejects (here, for its invalid port) is redacted whole wherever
// the text quotes it, and the user info of any other URL in the text is
// masked.
func TestRedactEndpointIn_UnparseableEndpointFailsClosed(t *testing.T) {
	endpoint := "http://user:s3cret@127.0.0.1:99999x"
	text := `parse "http://user:s3cret@127.0.0.1:99999x": invalid port; Get "http://user:s3cret@127.0.0.1:99999x/healthz"; see http://tok@hub.example.com/x`
	require.Equal(t, `parse "[redacted hub URL]": invalid port; Get "[redacted hub URL]/healthz"; see http://xxxxx@hub.example.com/x`,
		redactEndpointIn(text, endpoint))
}

// TestRedactEndpointIn_UnparseableEndpointEchoesNothing: an unparseable
// endpoint is replaced whole also where Go quoted it with %q (escaping a
// control character), and a password containing '@' leaves no part of
// itself in the text, in that endpoint or in another URL.
func TestRedactEndpointIn_UnparseableEndpointEchoesNothing(t *testing.T) {
	// url.Parse rejects the control character; Go's error quotes the URL
	// with %q, so the text holds it escaped.
	escaped := "http://user:s3cret@hub.example.com/\x01"
	text := `parse "http://user:s3cret@hub.example.com/\x01": net/url: invalid control character in URL`
	got := redactEndpointIn(text, escaped)
	require.Equal(t, `parse "[redacted hub URL]": net/url: invalid control character in URL`, got)
	assertNoSecretSlice(t, got, "s3cret")

	// url.Parse rejects the port; the password holds an '@'.
	atPassword := "http://user:s3@cret@127.0.0.1:99999x"
	text = `parse "http://user:s3@cret@127.0.0.1:99999x": invalid port; also http://other:s3@cret@hub.example.com/x`
	got = redactEndpointIn(text, atPassword)
	require.Equal(t, `parse "[redacted hub URL]": invalid port; also http://xxxxx@hub.example.com/x`, got)
	assertNoSecretSlice(t, got, "s3@cret")
}

// assertNoSecretSlice fails when out contains any 3-character slice of
// secret.
func assertNoSecretSlice(t *testing.T, out, secret string) {
	t.Helper()
	for i := 0; i+3 <= len(secret); i++ {
		assert.NotContains(t, out, secret[i:i+3], "a slice of the secret is visible")
	}
}

// hubSettingsReadAllowed lists, by file, the lines outside server_*.go
// that the hub settings guard accepts. Each one is not a CLI read of hub
// configuration from settings:
//
//   - broker.go: the broker's own legacy registration (hub.brokerId with
//     the hub.endpoint it was registered on), used to deregister and to
//     report status; broker-owned, not CLI hub configuration;
//   - project_use_select.go: the hub of a candidate in 'scion use', a
//     struct field named Hub, not settings.
var hubSettingsReadAllowed = map[string][]string{
	"broker.go":             {"globalSettings.Hub.Endpoint"},
	"project_use_select.go": {"m.Hub.Endpoint", "candidates[0].Hub.Endpoint"},
}

// hubSettingsReadRe matches a read of the hub settings keys the CLI no
// longer reads (hub.endpoint, hub.token, hub.apiKey, hub.transport).
var hubSettingsReadRe = regexp.MustCompile(`\.Hub\.(Endpoint|Token|APIKey|Transport)\b|GetHubEndpoint\(\)`)

// hubListReadRe matches a use of the CLI's hub resolution.
var hubListReadRe = regexp.MustCompile(`\b(invocationHub|invocationHubEndpoint|hubConfigured|resolveInvocationTarget)\(`)

// hubSettingsKeyRe matches a retired hub settings key as a string literal,
// the form a settings write (config.UpdateSetting and the like) takes.
var hubSettingsKeyRe = regexp.MustCompile(`"hub\.(endpoint|token|apiKey|transport)(\.[a-z]+)?"`)

// TestNoHubSettingsReadsOutsideServerCode is a source guard over cmd and
// pkg/hubsync:
//
//   - outside the server's own code (server_*.go), no file reads
//     hub.endpoint, hub.token, hub.apiKey or hub.transport from settings,
//     except the lines in hubSettingsReadAllowed;
//   - no file, server code included, names one of those keys as a string
//     literal, the form a settings write takes;
//   - the server's code never resolves the CLI's hub (invocationHub and
//     friends). It does register the local hub in the hub list at 'server
//     start' (registerLocalHub), which is intended and not matched here.
func TestNoHubSettingsReadsOutsideServerCode(t *testing.T) {
	var files []string
	for _, pattern := range []string{"*.go", filepath.Join("..", "pkg", "hubsync", "*.go")} {
		matches, err := filepath.Glob(pattern)
		require.NoError(t, err)
		files = append(files, matches...)
	}
	require.NotEmpty(t, files)
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		server := strings.HasPrefix(name, "server_") && filepath.Dir(path) == "."
		f, err := os.Open(path)
		require.NoError(t, err)
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for n := 1; scanner.Scan(); n++ {
			line := scanner.Text()
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			assert.False(t, hubSettingsKeyRe.MatchString(line), "%s:%d: names a retired hub settings key: %s", path, n, line)
			if server {
				assert.False(t, hubListReadRe.MatchString(line), "%s:%d: server code reads the CLI hub list: %s", path, n, line)
				continue
			}
			if !hubSettingsReadRe.MatchString(line) || hubSettingsReadIsAllowed(name, line) {
				continue
			}
			t.Errorf("%s:%d: reads a hub settings key the CLI no longer reads: %s", path, n, strings.TrimSpace(line))
		}
		require.NoError(t, scanner.Err())
		_ = f.Close()
	}
}

// TestHubSettingsReadGuardMatches checks the guard's own matcher: reads of
// the retired keys are caught, allowed expressions are not, and an allowed
// expression does not hide another read on the same line.
func TestHubSettingsReadGuardMatches(t *testing.T) {
	for _, line := range []string{
		"\tep := settings.Hub.Endpoint",
		"\ttok := s.Hub.Token",
		"\tkey := settings.Hub.APIKey",
		"\tts := settings.Hub.Transport",
		"\treturn settings.GetHubEndpoint()",
	} {
		assert.True(t, hubSettingsReadRe.MatchString(line), line)
		assert.False(t, hubSettingsReadIsAllowed("hub.go", line), line)
	}
	assert.True(t, hubSettingsReadIsAllowed("project_use_select.go", "\tendpoint := m.Hub.Endpoint"))
	assert.False(t, hubSettingsReadIsAllowed("project_use_select.go", "\tx := m.Hub.Endpoint + settings.Hub.Endpoint"))
	assert.False(t, hubSettingsReadIsAllowed("hub.go", "\tendpoint := m.Hub.Endpoint"), "allowed only in its own file")
	assert.False(t, hubSettingsReadRe.MatchString("\tep := hubCtx.Endpoint"))
	assert.False(t, hubSettingsReadRe.MatchString("\tid := settings.Hub.BrokerToken"))
	assert.True(t, hubListReadRe.MatchString("\tep := invocationHubEndpoint()"))
	assert.True(t, hubSettingsKeyRe.MatchString(`\t_ = config.UpdateSetting(dir, "hub.endpoint", url, true)`))
	assert.True(t, hubSettingsKeyRe.MatchString(`\t_ = config.UpdateSetting(dir, "hub.transport.mode", m, true)`))
	assert.False(t, hubSettingsKeyRe.MatchString(`\t_ = config.UpdateSetting(dir, "hub.linked", "true", false)`))
}

// hubSettingsReadIsAllowed reports whether every match on line is an
// allowed expression of file name (see hubSettingsReadAllowed).
func hubSettingsReadIsAllowed(name, line string) bool {
	rest := line
	for _, allowed := range hubSettingsReadAllowed[name] {
		rest = strings.ReplaceAll(rest, allowed, "")
	}
	return !hubSettingsReadRe.MatchString(rest)
}
