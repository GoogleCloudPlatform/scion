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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setServerEndpointFlags sets the 'server start' flags the hub endpoint
// resolution reads and restores them when the test ends.
func setServerEndpointFlags(t *testing.T, hubOn, webOn bool, port int, baseURL string) {
	t.Helper()
	savedHub, savedBroker, savedWeb, savedHosted, savedPort, savedBase, savedDebug := enableHub, enableRuntimeBroker, enableWeb, hostedMode, webPort, webBaseURL, enableDebug
	t.Cleanup(func() {
		enableHub, enableRuntimeBroker, enableWeb, hostedMode, webPort, webBaseURL, enableDebug = savedHub, savedBroker, savedWeb, savedHosted, savedPort, savedBase, savedDebug
	})
	enableHub, enableRuntimeBroker, enableWeb, hostedMode, webPort, webBaseURL, enableDebug = hubOn, true, webOn, false, port, baseURL, false
}

// TestResolveHubEndpointFrom_OperatorPrecedence checks the operator
// endpoint order flag > environment > file (ptone/scion#5362) end to end:
// the server config is loaded from settings.yaml and the environment as at
// startup, then the endpoint in effect is resolved against a value stored
// in the settings database. It checks the source label, the pin, the
// endpoint in effect and the OIDC issuer and transport audience defaults
// derived from it.
func TestResolveHubEndpointFrom_OperatorPrecedence(t *testing.T) {
	const (
		fileURL    = "https://file.example.com"
		envHubURL  = "https://env-hub.example.com"
		envBaseURL = "https://env-base.example.com"
		flagURL    = "https://flag.example.com"
		dbURL      = "https://db.example.com"
		localURL   = "http://localhost:8080"
	)
	tests := []struct {
		name                string
		file, envHub        string
		envBase, flag, db   string
		wantOperator        string
		wantSource          hubEndpointSource
		wantLabel, wantPin  string
		wantPublic, wantPub string // endpoint in effect and its source
	}{
		{name: "file only", file: fileURL,
			wantOperator: fileURL, wantSource: hubEndpointSourceConfig, wantLabel: "file", wantPin: "",
			wantPublic: fileURL, wantPub: "file"},
		{name: "file, database value replaces it", file: fileURL, db: dbURL,
			wantOperator: fileURL, wantSource: hubEndpointSourceConfig, wantLabel: "file", wantPin: "",
			wantPublic: dbURL, wantPub: "database"},
		{name: "flag outranks file", file: fileURL, flag: flagURL,
			wantOperator: flagURL, wantSource: hubEndpointSourceBaseURLFlag, wantLabel: "flag", wantPin: "--base-url",
			wantPublic: flagURL, wantPub: "flag"},
		{name: "flag outranks file and pins against the database", file: fileURL, flag: flagURL, db: dbURL,
			wantOperator: flagURL, wantSource: hubEndpointSourceBaseURLFlag, wantLabel: "flag", wantPin: "--base-url",
			wantPublic: flagURL, wantPub: "flag"},
		{name: "flag trailing slash trimmed", file: fileURL, flag: flagURL + "/",
			wantOperator: flagURL, wantSource: hubEndpointSourceBaseURLFlag, wantLabel: "flag", wantPin: "--base-url",
			wantPublic: flagURL, wantPub: "flag"},
		{name: "SCION_SERVER_BASE_URL outranks file", file: fileURL, envBase: envBaseURL, db: dbURL,
			wantOperator: envBaseURL, wantSource: hubEndpointSourceBaseURLEnv, wantLabel: "env", wantPin: "SCION_SERVER_BASE_URL",
			wantPublic: envBaseURL, wantPub: "env"},
		{name: "SCION_SERVER_HUB_ENDPOINT outranks file", file: fileURL, envHub: envHubURL, db: dbURL,
			wantOperator: envHubURL, wantSource: hubEndpointSourceConfig, wantLabel: "env", wantPin: "SCION_SERVER_HUB_ENDPOINT",
			wantPublic: envHubURL, wantPub: "env"},
		{name: "SCION_SERVER_HUB_ENDPOINT outranks SCION_SERVER_BASE_URL", envHub: envHubURL, envBase: envBaseURL,
			wantOperator: envHubURL, wantSource: hubEndpointSourceConfig, wantLabel: "env", wantPin: "SCION_SERVER_HUB_ENDPOINT",
			wantPublic: envHubURL, wantPub: "env"},
		{name: "flag outranks SCION_SERVER_HUB_ENDPOINT", envHub: envHubURL, flag: flagURL, db: dbURL,
			wantOperator: flagURL, wantSource: hubEndpointSourceBaseURLFlag, wantLabel: "flag", wantPin: "--base-url",
			wantPublic: flagURL, wantPub: "flag"},
		{name: "flag outranks every other input", file: fileURL, envHub: envHubURL, envBase: envBaseURL, flag: flagURL, db: dbURL,
			wantOperator: flagURL, wantSource: hubEndpointSourceBaseURLFlag, wantLabel: "flag", wantPin: "--base-url",
			wantPublic: flagURL, wantPub: "flag"},
		{name: "no operator value, database value replaces the localhost fallback", db: dbURL,
			wantOperator: localURL, wantSource: hubEndpointSourceLocalhost, wantLabel: "localhost", wantPin: "",
			wantPublic: dbURL, wantPub: "database"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globalDir := isolateServerHubEndpointEnv(t)
			if tt.file != "" {
				writeServerHubEndpointTestSettings(t, globalDir, "server:\n  hub:\n    public_url: "+tt.file+"\n")
			}
			if tt.envHub != "" {
				t.Setenv("SCION_SERVER_HUB_ENDPOINT", tt.envHub)
			}
			if tt.envBase != "" {
				t.Setenv("SCION_SERVER_BASE_URL", tt.envBase)
			}
			setServerEndpointFlags(t, true, true, 8080, tt.flag)
			cfg, err := config.LoadGlobalConfig("")
			require.NoError(t, err)

			url, src := resolveHubEndpointFrom(cfg, currentServerEndpointMode(), "", nil)
			assert.Equal(t, tt.wantOperator, url, "operator endpoint")
			assert.Equal(t, tt.wantSource, src, "operator source")
			op := operatorHubEndpoint{URL: url, Source: src}
			assert.Equal(t, tt.wantLabel, op.sourceLabel(), "operator source label")

			st := &endpointsTestStore{}
			if tt.db != "" {
				st = newEndpointsTestStore(t, `{"public_url":"`+tt.db+`"}`, "admin@example.com", "managed")
			}
			e, err := resolveHubEndpoints(context.Background(), st, op)
			require.NoError(t, err)
			assert.Equal(t, tt.wantPin, e.Pin, "pin")
			assert.Equal(t, tt.wantPublic, e.Public, "endpoint in effect")
			assert.Equal(t, tt.wantPub, e.PublicSource, "source of the endpoint in effect")
			assert.Equal(t, tt.wantPublic, e.For(hub.HubEndpointForOIDCIssuer), "OIDC issuer default")
			assert.Equal(t, tt.wantPublic, resolveTransportAudience("", "cloudrun_invoker", e.For(hub.HubEndpointForTransportAudience)), "transport audience default")
		})
	}
}

// TestResolveHubEndpointFrom_BrokerOnlyUnchanged checks that a server that
// runs only the Runtime Broker still takes server.hub.public_url first and
// ignores --base-url and SCION_SERVER_BASE_URL, and that the broker's own
// reporting endpoint never comes from them.
func TestResolveHubEndpointFrom_BrokerOnlyUnchanged(t *testing.T) {
	globalDir := isolateServerHubEndpointEnv(t)
	writeServerHubEndpointTestSettings(t, globalDir, "server:\n  hub:\n    public_url: https://file.example.com\n")
	t.Setenv("SCION_SERVER_BASE_URL", "https://env-base.example.com")
	setServerEndpointFlags(t, false, false, 8080, "https://flag.example.com")
	cfg, err := config.LoadGlobalConfig("")
	require.NoError(t, err)

	url, src := resolveHubEndpointFrom(cfg, currentServerEndpointMode(), "https://legacy.example.com", nil)
	assert.Equal(t, "https://file.example.com", url)
	assert.Equal(t, hubEndpointSourceConfig, src)

	cfg.Hub.Endpoint = ""
	url, src = resolveHubEndpointFrom(cfg, currentServerEndpointMode(), "https://legacy.example.com", nil)
	assert.Equal(t, "https://legacy.example.com", url)
	assert.Equal(t, hubEndpointSourceBrokerSettings, src)

	assert.Equal(t, "https://legacy.example.com", resolveBrokerHubEndpointFrom(cfg, currentServerEndpointMode(), "https://legacy.example.com", nil))
}

// TestServerSettings_GlobalOnlyFromProjectDir checks that starting the
// server from inside a project directory does not change the settings it
// loads or the hub endpoints it resolves from them (ptone/scion#5362): the
// project's hub.endpoint and active_profile are not merged.
func TestServerSettings_GlobalOnlyFromProjectDir(t *testing.T) {
	globalDir := isolateServerHubEndpointEnv(t)
	home := filepath.Dir(globalDir)
	writeServerHubEndpointTestSettings(t, globalDir,
		"active_profile: global-profile\nprofiles:\n  global-profile:\n    runtime: docker\nhub:\n  endpoint: https://global.example.com\n")

	projectDir := t.TempDir()
	projectScion := filepath.Join(projectDir, ".scion")
	require.NoError(t, os.MkdirAll(projectScion, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectScion, "settings.yaml"), []byte(
		"schema_version: \"1\"\nactive_profile: project-profile\nprofiles:\n  project-profile:\n    runtime: docker\nhub:\n  endpoint: https://project.example.com\n"), 0o644))

	type resolved struct {
		hubEndpoint, brokerOnly, brokerReport, colocatedReport string
		defaultProfile                                         string
	}
	resolveFrom := func(t *testing.T, dir string) resolved {
		t.Helper()
		t.Chdir(dir)
		settings, dp := loadServerSettings()
		require.NotNil(t, settings)
		require.NotNil(t, dp)
		cfg := newDeprecationTestConfig()
		hubEP, _ := resolveHubEndpointFrom(cfg, hubOnlyMode, settings.GetHubEndpoint(), nil)
		brokerEP, _ := resolveHubEndpointFrom(cfg, brokerOnlyMode, settings.GetHubEndpoint(), nil)
		return resolved{
			hubEndpoint:     hubEP,
			brokerOnly:      brokerEP,
			brokerReport:    resolveBrokerHubEndpointFrom(cfg, brokerOnlyMode, settings.GetHubEndpoint(), nil),
			colocatedReport: resolveBrokerHubEndpointFrom(cfg, workstationMode, settings.GetHubEndpoint(), nil),
			defaultProfile:  *dp,
		}
	}

	// The project layer is really there: a cwd-resolved load merges it.
	t.Chdir(projectDir)
	merged, err := config.LoadSettings("")
	require.NoError(t, err)
	require.Equal(t, "https://project.example.com", merged.GetHubEndpoint(), "test setup: the project settings must be visible to a cwd-resolved load")

	fromHome := resolveFrom(t, home)
	fromProject := resolveFrom(t, projectDir)
	assert.Equal(t, resolved{
		hubEndpoint:     "https://global.example.com",
		brokerOnly:      "https://global.example.com",
		brokerReport:    "https://global.example.com",
		colocatedReport: "http://localhost:8080",
		defaultProfile:  "global-profile",
	}, fromHome)
	assert.Equal(t, fromHome, fromProject, "starting from inside a project changed the server's settings")
}
