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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// hubAPIPreflightMadeUpToken is a made-up cfg.Auth.Token value; the refusal
// shows only the host.
const hubAPIPreflightMadeUpToken = "scion_dev_madeuptoken0123456789"

func hubAPIPreflightCfg(host string, devAuth bool) *config.GlobalConfig {
	cfg := &config.GlobalConfig{}
	cfg.Hub.Host = host
	cfg.Auth.Enabled = devAuth
	cfg.Auth.Token = hubAPIPreflightMadeUpToken
	return cfg
}

// With dev auth on, a standalone Hub API (hub enabled, web disabled) starts
// on a loopback host and is refused on any other host, an empty host
// included (it binds every interface).
func TestHubAPIStartupPreflight_DevAuthStandalone(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "::1", "localhost"} {
		t.Run("loopback "+host+" starts", func(t *testing.T) {
			require.NoError(t, hubAPIStartupPreflight(hubAPIPreflightCfg(host, true), true, false))
		})
	}
	for _, tc := range []struct{ name, host, shown string }{
		{"all interfaces", "0.0.0.0", "0.0.0.0"},
		{"empty host", "", "0.0.0.0"},
		{"all interfaces v6", "::", "::"},
		{"LAN IP", "192.168.1.10", "192.168.1.10"},
		{"private IP", "10.0.0.5", "10.0.0.5"},
		{"hostname", "hub.example.com", "hub.example.com"},
	} {
		t.Run(tc.name+" refused", func(t *testing.T) {
			err := hubAPIStartupPreflight(hubAPIPreflightCfg(tc.host, true), true, false)
			require.Error(t, err)
			msg := err.Error()
			assert.Contains(t, msg, "dev auth requires a loopback hub host")
			assert.Contains(t, msg, "non-loopback address ("+tc.shown+")")
			assert.Contains(t, msg, "--host 127.0.0.1")
			assert.Contains(t, msg, "server.hub.host")
			assert.Contains(t, msg, "--dev-auth=false")
			assert.Contains(t, msg, "server.auth.dev_mode: false")
			assert.NotContains(t, msg, hubAPIPreflightMadeUpToken)
		})
	}
}

// Dev auth off: a standalone Hub API on a non-loopback host is unchanged.
func TestHubAPIStartupPreflight_DevAuthOffUnchanged(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "", "192.168.1.10", "127.0.0.1"} {
		require.NoError(t, hubAPIStartupPreflight(hubAPIPreflightCfg(host, false), true, false), "host %q", host)
	}
}

// The web path is not this check's: with the web server enabled the Hub API
// is mounted on it and webStartupPreflight decides (TestWebStartupPreflight).
// With the hub disabled there is no Hub API listener.
func TestHubAPIStartupPreflight_OnlyStandaloneHub(t *testing.T) {
	cfg := hubAPIPreflightCfg("0.0.0.0", true)
	assert.NoError(t, hubAPIStartupPreflight(cfg, true, true), "hub+web")
	assert.NoError(t, hubAPIStartupPreflight(cfg, false, true), "web only")
	assert.NoError(t, hubAPIStartupPreflight(cfg, false, false), "no hub, no web")
	// The web path's own check still refuses the same config.
	_, err := webStartupPreflight(cfg, serverEndpointMode{hubEnabled: true, webEnabled: true, webPort: 8080}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev auth cannot be enabled")
}

// Every dev auth source reaches the check through the real config load:
// the workstation default with an explicit --host 0.0.0.0, --dev-auth in
// hosted mode, and server.auth.dev_mode in hosted mode.
func TestHubAPIStartupPreflight_ThroughConfigLoad(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		flags    map[string]string
		wantErr  bool
	}{
		{
			name:     "workstation default dev auth, --host 0.0.0.0, web off",
			settings: "schema_version: \"1\"\nserver:\n  mode: workstation\n",
			flags:    map[string]string{"host": "0.0.0.0", "enable-web": "false"},
			wantErr:  true,
		},
		{
			name:     "workstation default dev auth, default host, web off",
			settings: "schema_version: \"1\"\nserver:\n  mode: workstation\n",
			flags:    map[string]string{"enable-web": "false"},
		},
		{
			name:     "workstation, --host 0.0.0.0, web off, --dev-auth=false",
			settings: "schema_version: \"1\"\nserver:\n  mode: workstation\n",
			flags:    map[string]string{"host": "0.0.0.0", "enable-web": "false", "dev-auth": "false"},
		},
		{
			name:     "hosted, --enable-hub --dev-auth, default host",
			settings: "schema_version: \"1\"\nserver:\n  mode: hosted\n",
			flags:    map[string]string{"enable-hub": "true", "dev-auth": "true"},
			wantErr:  true,
		},
		{
			name:     "hosted, --enable-hub, server.auth.dev_mode, default host",
			settings: "schema_version: \"1\"\nserver:\n  mode: hosted\n  auth:\n    dev_mode: true\n",
			flags:    map[string]string{"enable-hub": "true"},
			wantErr:  true,
		},
		{
			name:     "hosted, --enable-hub --dev-auth, --host 127.0.0.1",
			settings: "schema_version: \"1\"\nserver:\n  mode: hosted\n",
			flags:    map[string]string{"enable-hub": "true", "dev-auth": "true", "host": "127.0.0.1"},
		},
		{
			name:     "hosted, --enable-hub, no dev auth, default host",
			settings: "schema_version: \"1\"\nserver:\n  mode: hosted\n",
			flags:    map[string]string{"enable-hub": "true"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() {
				for name := range tc.flags {
					serverStartCmd.Flags().Lookup(name).Changed = false
				}
				resetServerFlags()
			})
			resetServerFlags()
			for _, k := range []string{
				"SCION_SERVER_MODE", "SCION_SERVER_HUB_HOST",
				"SCION_SERVER_AUTH_DEV_MODE", "SCION_SERVER_AUTH_DEVMODE",
			} {
				t.Setenv(k, "") // restored after the test
				require.NoError(t, os.Unsetenv(k))
			}
			writeServerSettings(t, tc.settings)
			for name, v := range tc.flags {
				require.NoError(t, serverStartCmd.Flags().Set(name, v))
			}

			cfg, err := loadAndReconcileConfig(serverStartCmd)
			require.NoError(t, err)
			err = hubAPIStartupPreflight(cfg, enableHub, enableWeb)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "dev auth requires a loopback hub host")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// runServerStart calls hubAPIStartupPreflight(cfg, enableHub, enableWeb)
// exactly once, as a top-level `if err := ...; err != nil { return err }`
// statement, before the first statement that initialises dev auth
// (initDevAuth) and before the statement that starts the standalone Hub API
// (hubSrv.Start).
func TestRunServerStart_HubAPIPreflightBeforeStart(t *testing.T) {
	fset := token.NewFileSet()
	fg, err := parser.ParseFile(fset, "server_foreground.go", nil, 0)
	require.NoError(t, err)
	var body *ast.BlockStmt
	for _, d := range fg.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "runServerStart" {
			body = fn.Body
		}
	}
	require.NotNil(t, body, "runServerStart not found")

	countCalls := func(n ast.Node, match func(*ast.CallExpr) bool) int {
		c := 0
		ast.Inspect(n, func(x ast.Node) bool {
			if call, ok := x.(*ast.CallExpr); ok && match(call) {
				c++
			}
			return true
		})
		return c
	}
	isPreflight := func(c *ast.CallExpr) bool {
		id, ok := c.Fun.(*ast.Ident)
		return ok && id.Name == "hubAPIStartupPreflight"
	}
	isInitDevAuth := func(c *ast.CallExpr) bool {
		id, ok := c.Fun.(*ast.Ident)
		return ok && id.Name == "initDevAuth"
	}
	isHubStart := func(c *ast.CallExpr) bool {
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Start" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "hubSrv"
	}

	require.Equal(t, 1, countCalls(body, isPreflight), "hubAPIStartupPreflight calls in runServerStart")
	preAt, devAuthAt, startAt := -1, -1, -1
	for i, st := range body.List {
		if preAt < 0 {
			if ifs, ok := st.(*ast.IfStmt); ok && ifs.Init != nil && countCalls(ifs.Init, isPreflight) == 1 {
				preAt = i
				call := ifs.Init.(*ast.AssignStmt).Rhs[0].(*ast.CallExpr)
				var args []string
				for _, a := range call.Args {
					if id, ok := a.(*ast.Ident); ok {
						args = append(args, id.Name)
					}
				}
				assert.Equal(t, []string{"cfg", "enableHub", "enableWeb"}, args, "hubAPIStartupPreflight call arguments")
				require.Len(t, ifs.Body.List, 1)
				ret, ok := ifs.Body.List[0].(*ast.ReturnStmt)
				require.True(t, ok, "the preflight error must be returned")
				require.Len(t, ret.Results, 1)
				rid, ok := ret.Results[0].(*ast.Ident)
				require.True(t, ok)
				assert.Equal(t, "err", rid.Name)
			}
		}
		if devAuthAt < 0 && countCalls(st, isInitDevAuth) > 0 {
			devAuthAt = i
		}
		if startAt < 0 && countCalls(st, isHubStart) > 0 {
			startAt = i
		}
	}
	require.GreaterOrEqual(t, preAt, 0, "top-level hubAPIStartupPreflight statement not found")
	require.GreaterOrEqual(t, devAuthAt, 0, "initDevAuth not found")
	require.GreaterOrEqual(t, startAt, 0, "hubSrv.Start not found")
	assert.Less(t, preAt, devAuthAt, "hubAPIStartupPreflight must run before initDevAuth")
	assert.Less(t, preAt, startAt, "hubAPIStartupPreflight must run before hubSrv.Start")
}
