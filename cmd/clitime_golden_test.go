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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden tests for the global --tz/--utc flags. They run the real root
// command, so flag parsing and the PersistentPreRunE zone resolution are
// exercised, and they pin the process zone to Asia/Tokyo so a pass cannot
// come from the host zone happening to match.

// The API's UTC strings. The fractional seconds check that JSON output is
// passed through byte for byte, not reformatted.
const (
	goldenCreatedAt = "2026-10-03T05:04:09.123456789Z" // 14:04 JST, 01:04 EDT
	goldenUpdatedAt = "2026-01-02T23:30:00Z"           // 08:30 JST (next day), 18:30 EST
)

func pinLocalZone(t *testing.T, name string) {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	orig := time.Local
	time.Local = loc
	t.Cleanup(func() {
		time.Local = orig
		clitime.SetZone(nil)
	})
}

func newGoldenTimeHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/messages":
			_, _ = w.Write([]byte(`{"items":[{"id":"msg-0001","projectId":"test-project","sender":"agent:dev","senderId":"a1","recipient":"user:me","recipientId":"u1","msg":"build finished","type":"instruction","read":false,"agentId":"dev","createdAt":"` + goldenCreatedAt + `"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/secrets":
			_, _ = w.Write([]byte(`{"scope":"user","secrets":[{"key":"API_KEY","type":"environment","scope":"user","version":3,"created":"` + goldenCreatedAt + `","updated":"` + goldenUpdatedAt + `"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setupGoldenTimeProject writes a hub-enabled project (with an image
// registry, so commands outside the hub subtree pass the registry check) and
// returns its .scion directory. It also changes into the project.
func setupGoldenTimeProject(t *testing.T, endpoint string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HUB_ENDPOINT", endpoint)
	dir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	data, err := json.Marshal(map[string]interface{}{
		"project_id":     "test-project",
		"image_registry": "registry.example.com/test",
		"hub":            map[string]interface{}{"enabled": true, "endpoint": endpoint},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o644))
	// The project is found from the working directory: "hub secret list"
	// has its own --project scope flag that shadows the global one.
	t.Chdir(filepath.Dir(dir))
	// The mock hub listens on localhost; when the tests run inside an agent
	// container this tells the container guard it is reachable.
	t.Setenv("SCION_NETWORK_MODE", "host")
	return dir
}

// runRootGolden executes the root command with args and returns stdout.
func runRootGolden(t *testing.T, args ...string) (string, error) {
	t.Helper()
	restoreAllSilenceUsage(t)
	origProject, origFormat := projectPath, outputFormat
	origTZ, origUTC := displayTZ, displayUTC
	origShowAll, origMsgJSON := messagesShowAll, messagesJSON
	origSecretJSON, origSecretProject, origSecretBroker := secretOutputJSON, secretProjectScope, secretBrokerScope
	t.Cleanup(func() {
		projectPath, outputFormat = origProject, origFormat
		displayTZ, displayUTC = origTZ, origUTC
		messagesShowAll, messagesJSON = origShowAll, origMsgJSON
		secretOutputJSON, secretProjectScope, secretBrokerScope = origSecretJSON, origSecretProject, origSecretBroker
		rootCmd.SetArgs(nil)
		rootCmd.SetErr(nil)
	})
	// Flag values persist across Execute calls in one process; start clean.
	// Reset the Changed bits too: cobra's --tz/--utc exclusivity group reads
	// them, so a stale bit from an earlier run would fail the next one.
	resetTimeZoneFlagsChanged(t)
	t.Cleanup(func() { resetTimeZoneFlagsChanged(t) })
	displayTZ, displayUTC, outputFormat = "", false, ""
	messagesShowAll, messagesJSON, secretOutputJSON = false, false, false

	var stderr bytes.Buffer
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs(args)
	var err error
	out := captureStdout(t, func() { err = rootCmd.Execute() })
	return out, err
}

func resetTimeZoneFlagsChanged(t *testing.T) {
	t.Helper()
	for _, name := range []string{"tz", "utc"} {
		f := rootCmd.PersistentFlags().Lookup(name)
		require.NotNil(t, f, name)
		f.Changed = false
	}
}

func TestGoldenCLITimes(t *testing.T) {
	pinLocalZone(t, "Asia/Tokyo")
	srv := newGoldenTimeHub(t)
	setupGoldenTimeProject(t, srv.URL)

	messagesHeader := "ID            AGENT           TYPE            TIME                    MESSAGE\n" +
		"------------  --------------  --------------  ----------------------  -------\n"
	secretsHeader := "Secrets (scope: user):\n" +
		"KEY                             TYPE          PROGENY   VERSION   UPDATED\n" +
		"------------------------------  ------------  --------  --------  -----------------------\n"

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "messages local zone",
			args: []string{"messages", "--all"},
			want: messagesHeader +
				"msg-0001      dev             instruction     2026-10-03 14:04 JST    build finished\n",
		},
		{
			name: "messages --tz America/New_York",
			args: []string{"--tz", "America/New_York", "messages", "--all"},
			want: messagesHeader +
				"msg-0001      dev             instruction     2026-10-03 01:04 EDT    build finished\n",
		},
		{
			name: "messages --utc",
			args: []string{"messages", "--all", "--utc"},
			want: messagesHeader +
				"msg-0001      dev             instruction     2026-10-03 05:04 UTC    build finished\n",
		},
		{
			name: "hub secret list local zone",
			args: []string{"hub", "secret", "list"},
			want: secretsHeader +
				"API_KEY                         environment   -         v3        2026-01-03 08:30:00 JST\n",
		},
		{
			name: "hub secret list --tz America/New_York",
			args: []string{"--tz", "America/New_York", "hub", "secret", "list"},
			want: secretsHeader +
				"API_KEY                         environment   -         v3        2026-01-02 18:30:00 EST\n",
		},
		{
			name: "hub secret list --utc",
			args: []string{"--utc", "hub", "secret", "list"},
			want: secretsHeader +
				"API_KEY                         environment   -         v3        2026-01-02 23:30:00 UTC\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runRootGolden(t, tc.args...)
			require.NoError(t, err)
			assert.Equal(t, tc.want, out)
		})
	}
}

func TestGoldenCLITimes_JSONPassthrough(t *testing.T) {
	pinLocalZone(t, "Asia/Tokyo")
	srv := newGoldenTimeHub(t)
	setupGoldenTimeProject(t, srv.URL)

	cases := []struct {
		name string
		args []string
		keys []string
	}{
		{"messages --format json", []string{"--format", "json", "messages", "--all"}, []string{`"createdAt": "` + goldenCreatedAt + `"`}},
		{"messages --format json --tz", []string{"--tz", "America/New_York", "--format", "json", "messages", "--all"}, []string{`"createdAt": "` + goldenCreatedAt + `"`}},
		{"messages --json --utc", []string{"--utc", "messages", "--all", "--json"}, []string{`"createdAt": "` + goldenCreatedAt + `"`}},
		// "hub secret list" JSON carries the table's metadata only, so "updated" is the one timestamp.
		{"hub secret list --json --tz", []string{"--tz", "America/New_York", "hub", "secret", "list", "--json"}, []string{`"updated": "` + goldenUpdatedAt + `"`}},
		{"hub secret list --format json --tz", []string{"--tz", "America/New_York", "--format", "json", "hub", "secret", "list"}, []string{`"updated": "` + goldenUpdatedAt + `"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runRootGolden(t, tc.args...)
			require.NoError(t, err)
			require.True(t, json.Valid([]byte(out)), "output must be JSON: %s", out)
			for _, k := range tc.keys {
				assert.Contains(t, out, k, "JSON must carry the API's UTC string unchanged")
			}
			assert.NotContains(t, out, "JST")
			assert.NotContains(t, out, "EDT")
			assert.NotContains(t, out, "+09:00")
		})
	}
}

func TestGlobalTimeZoneFlags_Errors(t *testing.T) {
	pinLocalZone(t, "Asia/Tokyo")
	srv := newGoldenTimeHub(t)
	setupGoldenTimeProject(t, srv.URL)

	_, err := runRootGolden(t, "--tz", "Mars/Olympus_Mons", "messages", "--all")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid --tz "Mars/Olympus_Mons"`)

	_, err = runRootGolden(t, "--tz", "America/New_York", "--utc", "messages", "--all")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[tz utc] were all set")
}

// The --tz/--utc group is registered on the root's persistent flags; cobra
// must still enforce it when the flags are given after a subcommand.
func TestGlobalTimeZoneFlags_ExclusiveOnSubcommand(t *testing.T) {
	pinLocalZone(t, "Asia/Tokyo")
	srv := newGoldenTimeHub(t)
	setupGoldenTimeProject(t, srv.URL)

	for _, args := range [][]string{
		{"list", "--tz", "UTC", "--utc"},
		{"messages", "--all", "--utc", "--tz", "Asia/Kathmandu"},
		{"--utc", "messages", "--all", "--tz", "UTC"},
	} {
		_, err := runRootGolden(t, args...)
		require.Error(t, err, "%v", args)
		assert.Contains(t, err.Error(), "[tz utc] were all set", "%v", args)
	}

	// Either flag alone after a subcommand is still accepted.
	_, err := runRootGolden(t, "messages", "--all", "--utc")
	require.NoError(t, err)
	_, err = runRootGolden(t, "messages", "--all", "--tz", "UTC")
	require.NoError(t, err)
}

// The root hook fails outside a project; the --tz/--utc conflict must still
// be the reported error, because the hook checks flag groups first.
func TestGlobalTimeZoneFlags_ExclusiveBeforeHookErrors(t *testing.T) {
	setupNoProjectPreRun(t)

	_, err := runRootGolden(t, "list", "--utc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project", "control: the hook must fail here")

	for _, args := range [][]string{
		{"list", "--tz", "UTC", "--utc"},
		{"--utc", "list", "--tz", "Asia/Tokyo"},
	} {
		_, err := runRootGolden(t, args...)
		require.Error(t, err, "%v", args)
		assert.Contains(t, err.Error(), "[tz utc] were all set", "%v", args)
		assert.NotContains(t, err.Error(), "not in a scion project", "%v", args)
	}
}

func TestGlobalTimeZoneFlags_Registered(t *testing.T) {
	tz := rootCmd.PersistentFlags().Lookup("tz")
	require.NotNil(t, tz, "--tz must be a persistent root flag")
	assert.Equal(t, "", tz.DefValue)
	utc := rootCmd.PersistentFlags().Lookup("utc")
	require.NotNil(t, utc, "--utc must be a persistent root flag")
	assert.Equal(t, "false", utc.DefValue)
	assert.True(t, strings.Contains(tz.Usage, "IANA"))
}
