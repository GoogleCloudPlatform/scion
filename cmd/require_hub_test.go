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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unregisteredLinkHub is a hub on which the project is not registered until
// the CLI registers it; no project matches by name. It counts registers.
func unregisteredLinkHub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var registers atomic.Int32
	var registered atomic.Value
	registered.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/register":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["id"].(string)
			registered.Store(id)
			registers.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]string{"id": id, "name": "global"},
				"created": true,
			})
		case r.URL.Path == "/api/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		case strings.HasSuffix(rest, "/providers"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"providers": []interface{}{}})
		case rest != r.URL.Path && rest != "" && rest == registered.Load().(string):
			_ = json.NewEncoder(w).Encode(map[string]string{"id": rest, "name": "global"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &registers
}

// closedLoopbackEndpoint returns a loopback URL with nothing listening.
func closedLoopbackEndpoint(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return "http://" + addr
}

// TestRequireHub covers the hub-always contract: requireHub never returns a
// nil context without an error, and there is no auto-start.
func TestRequireHub(t *testing.T) {
	const projectID = "require-hub-project"
	tests := []struct {
		name string
		// endpoint returns the hub.endpoint to write ("" for none).
		endpoint func(t *testing.T) (string, *atomic.Int32)
		devToken string // SCION_DEV_TOKEN: makes a loopback endpoint the local workstation hub
		hubToken string // SCION_HUB_TOKEN: a non-dev credential (a remote-style hub)
		linked   string // hub.linked in the settings ("" for unset)
		yes      bool
		check    func(t *testing.T, ctx *HubContext, err error, registers *atomic.Int32)
	}{
		{
			name:     "no endpoint",
			endpoint: func(t *testing.T) (string, *atomic.Int32) { return "", nil },
			check: func(t *testing.T, ctx *HubContext, err error, _ *atomic.Int32) {
				require.ErrorIs(t, err, errHubNotConfigured)
				assert.Equal(t, "No hub configured. Start the local hub with 'scion server start' (first run opens setup), "+
					"or set a remote one with 'scion config set hub.endpoint <url>'.", err.Error())
			},
		},
		{
			name:     "remote endpoint unreachable",
			endpoint: func(t *testing.T) (string, *atomic.Int32) { return closedLoopbackEndpoint(t), nil },
			hubToken: "bearer-test-token",
			check: func(t *testing.T, ctx *HubContext, err error, _ *atomic.Int32) {
				require.Error(t, err)
				var notRunning *hubsync.LocalServerNotRunningError
				assert.False(t, errors.As(err, &notRunning), "a remote-style hub does not get the local-server error")
				assert.Contains(t, err.Error(), "is not responding")
				assert.Contains(t, err.Error(), "scion hub status")
			},
		},
		{
			name:     "local workstation endpoint not answering",
			endpoint: func(t *testing.T) (string, *atomic.Int32) { return closedLoopbackEndpoint(t), nil },
			devToken: "scion_dev_test",
			check: func(t *testing.T, ctx *HubContext, err error, _ *atomic.Int32) {
				var notRunning *hubsync.LocalServerNotRunningError
				require.True(t, errors.As(err, &notRunning), "error = %v", err)
				assert.Equal(t, "Local scion server is not running. Start it with 'scion server start'.", err.Error())
			},
		},
		{
			name: "unlinked project",
			endpoint: func(t *testing.T) (string, *atomic.Int32) {
				srv, regs := unregisteredLinkHub(t)
				return srv.URL, regs
			},
			devToken: "scion_dev_test",
			linked:   "false",
			yes:      true,
			check: func(t *testing.T, ctx *HubContext, err error, registers *atomic.Int32) {
				require.ErrorIs(t, err, hubsync.ErrProjectUnlinked)
				assert.Zero(t, registers.Load(), "an unlinked project is not linked again")
			},
		},
		{
			name: "local hub, unregistered project is auto-linked",
			endpoint: func(t *testing.T) (string, *atomic.Int32) {
				srv, regs := unregisteredLinkHub(t)
				return srv.URL, regs
			},
			devToken: "scion_dev_test",
			check: func(t *testing.T, ctx *HubContext, err error, registers *atomic.Int32) {
				require.NoError(t, err)
				require.NotNil(t, ctx)
				assert.EqualValues(t, 1, registers.Load(), "linked without a prompt (no terminal, no --yes)")
			},
		},
		{
			name: "remote hub, unregistered project keeps the prompt",
			endpoint: func(t *testing.T) (string, *atomic.Int32) {
				srv, regs := unregisteredLinkHub(t)
				return srv.URL, regs
			},
			hubToken: "bearer-test-token",
			check: func(t *testing.T, ctx *HubContext, err error, registers *atomic.Int32) {
				require.Error(t, err, "no terminal and no --yes: the prompt declines")
				assert.Contains(t, err.Error(), "must be linked")
				assert.Zero(t, registers.Load())
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hermeticCLIEnv(t)
			t.Setenv("SCION_DEV_TOKEN", tc.devToken)
			t.Setenv("SCION_HUB_TOKEN", tc.hubToken)
			home := os.Getenv("HOME")
			t.Chdir(home)
			origProjectPath, origGlobal, origYes, origNI, origEP := projectPath, globalMode, autoConfirm, nonInteractive, hubEndpoint
			t.Cleanup(func() {
				projectPath, globalMode, autoConfirm, nonInteractive, hubEndpoint = origProjectPath, origGlobal, origYes, origNI, origEP
			})
			projectPath, globalMode, autoConfirm, nonInteractive, hubEndpoint = "global", true, tc.yes, false, ""

			endpoint, registers := tc.endpoint(t)
			settings := fmt.Sprintf("project_id: %s\n", projectID)
			if endpoint != "" || tc.linked != "" {
				settings += "hub:\n"
				if endpoint != "" {
					settings += "  endpoint: " + endpoint + "\n"
				}
				if tc.linked != "" {
					settings += "  linked: " + tc.linked + "\n"
				}
			}
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(settings), 0o644))

			var ctx *HubContext
			var err error
			captureStdIO(t, func() { ctx, err = requireHub(projectPath) })
			if err != nil {
				assert.Nil(t, ctx, "an error comes with no context")
			} else {
				assert.NotNil(t, ctx, "requireHub never returns (nil, nil)")
			}
			tc.check(t, ctx, err, registers)
		})
	}
}

// TestNoHubFlagRemoved checks that the hidden --no-hub flag fails with the
// removal guidance before any command runs.
func TestNoHubFlagRemoved(t *testing.T) {
	setupNoProjectPreRun(t)
	noHub = true
	err := rootCmd.PersistentPreRunE(versionCmd, []string{})
	require.ErrorIs(t, err, errNoHubRemoved)
	assert.Contains(t, err.Error(), "--no-hub was removed together with local-only mode; scion always uses a hub")
	assert.Contains(t, err.Error(), hubsync.LocalModeRemovalDocsURL)

	f := rootCmd.PersistentFlags().Lookup("no-hub")
	require.NotNil(t, f, "the flag stays for one release")
	assert.True(t, f.Hidden)
}

// TestHubEnableDisableRemoved checks that 'scion hub enable' and 'scion hub
// disable' are hidden and fail with guidance.
func TestHubEnableDisableRemoved(t *testing.T) {
	assert.True(t, hubEnableCmd.Hidden)
	assert.True(t, hubDisableCmd.Hidden)

	err := hubEnableCmd.RunE(hubEnableCmd, nil)
	require.ErrorIs(t, err, errHubEnableRemoved)
	assert.Contains(t, err.Error(), "scion config set hub.endpoint <url>")

	err = hubDisableCmd.RunE(hubDisableCmd, nil)
	require.ErrorIs(t, err, errHubDisableRemoved)
	assert.Contains(t, err.Error(), "scion server stop")
}

// TestPromptHubRegistration_NoHubIsANote checks that 'scion init' does not
// fail without a hub: it returns a note pointing at 'scion server start'
// (the caller prints it as "Note: ..."), and the project links on first use.
func TestPromptHubRegistration_NoHubIsANote(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings func(t *testing.T) string
		devToken string
		want     string
	}{
		{
			name:     "no endpoint",
			settings: func(t *testing.T) string { return "project_id: init-p\n" },
			want:     "no hub is configured yet; this project links on first use. Start the local hub with 'scion server start'",
		},
		{
			name: "local server not running",
			settings: func(t *testing.T) string {
				return "project_id: init-p\nhub:\n  endpoint: " + closedLoopbackEndpoint(t) + "\n"
			},
			devToken: "scion_dev_test",
			want:     "the local scion server is not running; this project links on first use. Start it with 'scion server start'",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hermeticCLIEnv(t)
			t.Setenv("SCION_DEV_TOKEN", tc.devToken)
			home := os.Getenv("HOME")
			t.Chdir(home)
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(tc.settings(t)), 0o644))
			origEP := hubEndpoint
			t.Cleanup(func() { hubEndpoint = origEP })
			hubEndpoint = ""

			err := promptHubRegistration(true)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
