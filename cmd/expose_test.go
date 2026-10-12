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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	sciontoolhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const exposeTestAgentID = "agent-expose-id"

// exposeTestHub is a fake Hub port registry for one agent.
type exposeTestHub struct {
	registerStatus int // status for POST; 0 means 201
	deleteStatus   int // status for DELETE; 0 means 204
	ports          []map[string]any
	lastRegister   map[string]any
	lastDeletePath string
}

func (h *exposeTestHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	base := "/api/v1/agents/" + exposeTestAgentID + "/ports"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-token", r.Header.Get("X-Scion-Agent-Token"), "expose must authenticate as the agent")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == base:
			body, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(body, &h.lastRegister))
			if h.registerStatus != 0 && h.registerStatus != http.StatusCreated {
				w.WriteHeader(h.registerStatus)
				_, _ = w.Write([]byte(`{"error":"x"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"port": h.lastRegister["port"], "url": base + "/8080/proxy/", "basePath": base + "/8080/proxy/",
			})
		case r.Method == http.MethodGet && r.URL.Path == base:
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": h.ports})
		case r.Method == http.MethodDelete:
			h.lastDeletePath = r.URL.Path
			status := h.deleteStatus
			if status == 0 {
				status = http.StatusNoContent
			}
			w.WriteHeader(status)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useExposeHub points the expose client factory at srv and resets the
// expose flags for the test.
func useExposeHub(t *testing.T, srv *httptest.Server) {
	t.Helper()
	cleanup := sciontoolhub.SetHubTestSandboxed()
	t.Cleanup(cleanup)
	orig := newExposeHubClient
	newExposeHubClient = func() *sciontoolhub.Client {
		return sciontoolhub.NewClientWithConfig(srv.URL, "test-token", exposeTestAgentID)
	}
	t.Cleanup(func() { newExposeHubClient = orig })
	resetExposeFlags(t)
}

func resetExposeFlags(t *testing.T) {
	t.Helper()
	oldLabel, oldHost, oldList := exposeLabel, exposeHost, exposeList
	exposeLabel, exposeHost, exposeList = "", "127.0.0.1", false
	t.Cleanup(func() { exposeLabel, exposeHost, exposeList = oldLabel, oldHost, oldList })
}

func runExposeTest(t *testing.T, run func(*cobra.Command, []string) error, args ...string) (string, error) {
	t.Helper()
	c := &cobra.Command{}
	c.SetContext(context.Background())
	var out bytes.Buffer
	c.SetOut(&out)
	err := run(c, args)
	return out.String(), err
}

func TestExpose_RegistersPort(t *testing.T) {
	hub := &exposeTestHub{}
	srv := hub.start(t)
	useExposeHub(t, srv)
	exposeLabel = "my-web-app"

	out, err := runExposeTest(t, runExpose, "8080")
	require.NoError(t, err)
	assert.Equal(t, float64(8080), hub.lastRegister["port"])
	assert.Equal(t, "my-web-app", hub.lastRegister["label"])
	assert.Equal(t, "127.0.0.1", hub.lastRegister["host"])
	assert.Contains(t, out, "Port 8080 exposed.")
	assert.Contains(t, out, "URL: "+srv.URL+"/api/v1/agents/"+exposeTestAgentID+"/ports/8080/proxy/")
}

func TestExpose_AlreadyExposedIsSuccess(t *testing.T) {
	hub := &exposeTestHub{
		registerStatus: http.StatusConflict,
		ports:          []map[string]any{{"port": 8080, "url": "/p/8080/", "basePath": "/p/8080/"}},
	}
	srv := hub.start(t)
	useExposeHub(t, srv)

	out, err := runExposeTest(t, runExpose, "8080")
	require.NoError(t, err)
	assert.Contains(t, out, "Port 8080 is already exposed.")
	assert.Contains(t, out, "URL: "+srv.URL+"/p/8080/")
}

// On a 409 the command succeeds even when the port is missing from the
// list or the list call fails.
func TestExpose_AlreadyExposedFallback(t *testing.T) {
	hub := &exposeTestHub{
		registerStatus: http.StatusConflict,
		ports:          []map[string]any{{"port": 9000, "url": "/p/9000/"}},
	}
	useExposeHub(t, hub.start(t))

	out, err := runExposeTest(t, runExpose, "8080")
	require.NoError(t, err)
	assert.Equal(t, "Port 8080 is already exposed.\n", out)
}

func TestExpose_AlreadyExposedListFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	useExposeHub(t, srv)

	out, err := runExposeTest(t, runExpose, "8080")
	require.NoError(t, err)
	assert.Equal(t, "Port 8080 is already exposed.\n", out)
}

func TestExpose_HubErrorReturned(t *testing.T) {
	hub := &exposeTestHub{registerStatus: http.StatusForbidden}
	useExposeHub(t, hub.start(t))

	_, err := runExposeTest(t, runExpose, "8080")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestExpose_List(t *testing.T) {
	t.Setenv("SCION_AUTO_EXPOSE_PORTS", "false")
	hub := &exposeTestHub{ports: []map[string]any{
		{"port": 8080, "label": "web", "url": "/p/8080/"},
		{"port": 9000, "url": "/p/9000/"},
	}}
	srv := hub.start(t)
	useExposeHub(t, srv)
	exposeList = true

	out, err := runExposeTest(t, runExpose)
	require.NoError(t, err)
	assert.Equal(t, "Auto-expose: disabled\n"+
		"8080\tweb\t"+srv.URL+"/p/8080/\n"+
		"9000\t-\t"+srv.URL+"/p/9000/\n", out)
}

func TestExpose_ListAutoExposeEnabled(t *testing.T) {
	t.Setenv("SCION_AUTO_EXPOSE_PORTS", "true")
	t.Setenv("SCION_AUTO_EXPOSE_MODE", "denylist")
	t.Setenv("SCION_AUTO_EXPOSE_INTERVAL", "10s")
	useExposeHub(t, (&exposeTestHub{}).start(t))
	exposeList = true

	out, err := runExposeTest(t, runExpose)
	require.NoError(t, err)
	assert.Equal(t, "Auto-expose: enabled (mode: denylist, interval: 10s)\nNo ports exposed.\n", out)
}

func TestExpose_ListEmpty(t *testing.T) {
	t.Setenv("SCION_AUTO_EXPOSE_PORTS", "false")
	useExposeHub(t, (&exposeTestHub{}).start(t))
	exposeList = true

	out, err := runExposeTest(t, runExpose)
	require.NoError(t, err)
	assert.Equal(t, "Auto-expose: disabled\nNo ports exposed.\n", out)
}

// Both commands check their arguments before the client, so a bad port is
// reported even without an agent hub identity.
func TestExpose_ArgumentsCheckedBeforeClient(t *testing.T) {
	resetExposeFlags(t)
	orig := newExposeHubClient
	newExposeHubClient = func() *sciontoolhub.Client { return nil }
	t.Cleanup(func() { newExposeHubClient = orig })

	_, err := runExposeTest(t, runExpose)
	require.EqualError(t, err, "port is required")
	_, err = runExposeTest(t, runExpose, "abc")
	require.EqualError(t, err, `invalid port "abc"`)
	_, err = runExposeTest(t, runUnexpose, "abc")
	require.EqualError(t, err, `invalid port "abc"`)
}

// Ports outside 1-65535 are refused by both commands before any Hub call.
func TestExpose_PortOutOfRange(t *testing.T) {
	resetExposeFlags(t)
	orig := newExposeHubClient
	newExposeHubClient = func() *sciontoolhub.Client {
		t.Error("the hub client must not be built for an out-of-range port")
		return nil
	}
	t.Cleanup(func() { newExposeHubClient = orig })

	for _, arg := range []string{"0", "65536", "-1"} {
		want := "invalid port " + arg + ": must be between 1 and 65535"
		_, err := runExposeTest(t, runExpose, arg)
		require.EqualError(t, err, want, "expose %s", arg)
		_, err = runExposeTest(t, runUnexpose, arg)
		require.EqualError(t, err, want, "unexpose %s", arg)
	}
}

// The range bounds themselves are accepted.
func TestExpose_PortRangeBounds(t *testing.T) {
	for _, arg := range []string{"1", "65535"} {
		hub := &exposeTestHub{}
		useExposeHub(t, hub.start(t))
		_, err := runExposeTest(t, runExpose, arg)
		require.NoError(t, err, "expose %s", arg)
		_, err = runExposeTest(t, runUnexpose, arg)
		require.NoError(t, err, "unexpose %s", arg)
		assert.Equal(t, "/api/v1/agents/"+exposeTestAgentID+"/ports/"+arg, hub.lastDeletePath)
	}
}

func TestUnexpose_DeletesPort(t *testing.T) {
	hub := &exposeTestHub{}
	useExposeHub(t, hub.start(t))

	out, err := runExposeTest(t, runUnexpose, "8080")
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/agents/"+exposeTestAgentID+"/ports/8080", hub.lastDeletePath)
	assert.Equal(t, "Port 8080 unexposed.\n", out)
}

func TestUnexpose_NotExposedIsSuccess(t *testing.T) {
	useExposeHub(t, (&exposeTestHub{deleteStatus: http.StatusNotFound}).start(t))

	out, err := runExposeTest(t, runUnexpose, "8080")
	require.NoError(t, err)
	assert.Equal(t, "Port 8080 is not currently exposed.\n", out)
}

// Outside an agent container there is no agent hub identity: both commands
// fail with a message saying where they must run.
func TestExpose_RequiresAgentHubIdentity(t *testing.T) {
	resetExposeFlags(t)
	orig := newExposeHubClient
	t.Cleanup(func() { newExposeHubClient = orig })

	for name, factory := range map[string]func() *sciontoolhub.Client{
		"nil client":   func() *sciontoolhub.Client { return nil },
		"unconfigured": func() *sciontoolhub.Client { return sciontoolhub.NewClientWithConfig("", "", "") },
	} {
		t.Run(name, func(t *testing.T) {
			newExposeHubClient = factory
			_, err := runExposeTest(t, runExpose, "8080")
			require.ErrorIs(t, err, errExposeNotConfigured)
			_, err = runExposeTest(t, runUnexpose, "8080")
			require.ErrorIs(t, err, errExposeNotConfigured)
		})
	}
	assert.Equal(t, "must be run inside an agent container with a hub connection", errExposeNotConfigured.Error())
}

// expose and unexpose act only as the calling agent, so they are available
// in agent mode, and they need no local project.
func TestExposeCommands_AgentModeAndNoProject(t *testing.T) {
	// The registered flags and defaults (resetExposeFlags sets the
	// variables directly, so this pins the real registration).
	host := exposeCmd.Flags().Lookup("host")
	require.NotNil(t, host)
	assert.Equal(t, "127.0.0.1", host.DefValue)
	for _, name := range []string{"label", "list"} {
		require.NotNil(t, exposeCmd.Flags().Lookup(name), "expose --%s must be registered", name)
	}
	assert.Equal(t, "", exposeCmd.Flags().Lookup("label").DefValue)
	assert.Equal(t, "false", exposeCmd.Flags().Lookup("list").DefValue)

	for _, name := range []string{"expose", "unexpose"} {
		assert.True(t, agentAllowed[name], "agentAllowed should contain %s", name)
		real := resolveCommandPath(rootCmd, name)
		require.NotNil(t, real, "%s must exist in the real command tree", name)
		assert.False(t, commandRequiresProject(real), "%s must not require a project", name)

		t.Setenv("SCION_CLI_MODE", "agent")
		root := &cobra.Command{Use: "scion"}
		root.AddCommand(cloneCommandShape(real))
		applyModeRestrictions(root, resolveMode())
		assert.NotNil(t, resolveCommandPath(root, name), "%s stays in agent mode", name)
	}
}
