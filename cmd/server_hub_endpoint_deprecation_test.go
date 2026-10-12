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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateServerHubEndpointEnv clears every variable the server's hub
// endpoint resolution reads and gives the test a fresh HOME.
func isolateServerHubEndpointEnv(t *testing.T) (globalDir string) {
	t.Helper()
	for _, k := range []string{
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_SERVER_BASE_URL",
		"SCION_SERVER_HUB_ENDPOINT", "SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT",
		"SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE", "K_SERVICE",
		"SCION_PROJECT", "SCION_PROJECT_ID",
	} {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir = filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	return globalDir
}

func newDeprecationTestConfig() *config.GlobalConfig {
	cfg := config.DefaultGlobalConfig()
	return &cfg
}

// Modes the server runs in. The web port matches the workstation default.
var (
	workstationMode = serverEndpointMode{hubEnabled: true, brokerEnabled: true, webEnabled: true, webPort: 8080}
	hubOnlyMode     = serverEndpointMode{hubEnabled: true, hosted: true}
	brokerOnlyMode  = serverEndpointMode{brokerEnabled: true}
)

func TestServerHubEndpointDeprecated(t *testing.T) {
	tests := []struct {
		name   string
		mode   serverEndpointMode
		legacy string
		setup  func(cfg *config.GlobalConfig)
		env    map[string]string
		want   bool
	}{
		{name: "no legacy value", mode: workstationMode, legacy: "", want: false},
		{name: "advertised path takes a remote legacy value", mode: workstationMode, legacy: "https://hub.example.com", want: true},
		{name: "hub-only advertised path takes a legacy value", mode: hubOnlyMode, legacy: "https://hub.example.com", want: true},
		{name: "workstation self-written loopback on the same port", mode: workstationMode, legacy: "http://127.0.0.1:8080", want: false},
		{name: "workstation localhost spelling on the same port", mode: workstationMode, legacy: "http://localhost:8080/", want: false},
		{name: "loopback on a different port", mode: workstationMode, legacy: "http://127.0.0.1:9999", want: true},
		{
			name: "server.hub.public_url wins", mode: workstationMode, legacy: "https://hub.example.com",
			setup: func(cfg *config.GlobalConfig) { cfg.Hub.Endpoint = "https://public.example.com" },
			want:  false,
		},
		{
			name: "--base-url wins", mode: serverEndpointMode{hubEnabled: true, baseURLFlag: "https://base.example.com/"},
			legacy: "https://hub.example.com", want: false,
		},
		{
			name: "SCION_SERVER_BASE_URL wins", mode: workstationMode, legacy: "https://hub.example.com",
			env:  map[string]string{"SCION_SERVER_BASE_URL": "https://base.example.com"},
			want: false,
		},
		{name: "broker-only takes the legacy value (fallbacks 1 and 3)", mode: brokerOnlyMode, legacy: "https://hub.example.com", want: true},
		{name: "broker-only loopback still counts", mode: brokerOnlyMode, legacy: "http://localhost:8080", want: true},
		{
			name: "broker-only with both server keys set", mode: brokerOnlyMode, legacy: "https://hub.example.com",
			setup: func(cfg *config.GlobalConfig) {
				cfg.Hub.Endpoint = "https://public.example.com"
				cfg.RuntimeBroker.HubEndpoint = "https://hub.example.com"
			},
			want: false,
		},
		{
			name: "broker-only with only server.broker.hub_endpoint: the advertised fallback is not checked",
			mode: brokerOnlyMode, legacy: "https://hub.example.com",
			setup: func(cfg *config.GlobalConfig) { cfg.RuntimeBroker.HubEndpoint = "https://hub.example.com" },
			want:  false,
		},
		{
			name: "broker-only with a different server.broker.hub_endpoint", mode: brokerOnlyMode, legacy: "https://old.example.com",
			setup: func(cfg *config.GlobalConfig) { cfg.RuntimeBroker.HubEndpoint = "https://hub.example.com" },
			want:  false,
		},
		{name: "hub disabled and broker disabled", mode: serverEndpointMode{}, legacy: "https://hub.example.com", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateServerHubEndpointEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg := newDeprecationTestConfig()
			if tt.setup != nil {
				tt.setup(cfg)
			}
			assert.Equal(t, tt.want, serverHubEndpointDeprecated(cfg, tt.mode, tt.legacy))
		})
	}
}

func TestSameServerHubEndpoint(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"http://localhost:8080", "http://localhost:8080", true},
		{"http://localhost:8080/", "http://localhost:8080", true},
		{"http://127.0.0.1:8080", "http://localhost:8080", true},
		{"http://[::1]:8080", "http://localhost:8080", true},
		{"http://127.0.0.1:8081", "http://localhost:8080", false},
		{"https://127.0.0.1:8080", "http://localhost:8080", false},
		{"http://hub.example.com:8080", "http://localhost:8080", false},
		{"https://a.example.com", "https://b.example.com", false},
		{"http://localhost", "http://127.0.0.1:80", true},
		{"https://hub.example.com", "", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s=%s", tt.a, tt.b), func(t *testing.T) {
			assert.Equal(t, tt.want, sameServerHubEndpoint(tt.a, tt.b))
		})
	}
}

// writeGlobalSettings writes a v1 settings file into globalDir.
func writeGlobalSettings(t *testing.T, globalDir, body string) string {
	t.Helper()
	path := filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\n"+body), 0644))
	return path
}

// collectLogf returns a logf that records each formatted line.
func collectLogf() (func(string, ...any), *[]string) {
	var lines []string
	return func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }, &lines
}

// TestWarnDeprecatedServerHubEndpoint_Sources loads settings and server
// config the way server startup does and checks that the warning is logged
// once, with the exact text, for each legacy source, and not for the
// server keys, their env vars or the workstation localhost fallback.
func TestWarnDeprecatedServerHubEndpoint_Sources(t *testing.T) {
	tests := []struct {
		name     string
		settings string
		env      map[string]string
		mode     serverEndpointMode
		want     bool
	}{
		{name: "hub.endpoint in settings, advertised path", settings: "hub:\n  endpoint: https://hub.example.com\n", mode: hubOnlyMode, want: true},
		{name: "SCION_HUB_ENDPOINT, advertised path", env: map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com"}, mode: hubOnlyMode, want: true},
		{name: "hub.endpoint in settings, broker path", settings: "hub:\n  endpoint: https://hub.example.com\n", mode: brokerOnlyMode, want: true},
		{name: "SCION_HUB_ENDPOINT, broker path", env: map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com"}, mode: brokerOnlyMode, want: true},
		{
			name: "both paths use the legacy value: still one line", settings: "hub:\n  endpoint: https://hub.example.com\n",
			mode: workstationMode, want: true,
		},
		{
			name:     "server.hub.public_url in settings",
			settings: "hub:\n  endpoint: https://hub.example.com\nserver:\n  hub:\n    public_url: https://hub.example.com\n",
			mode:     hubOnlyMode, want: false,
		},
		{
			name: "SCION_SERVER_HUB_ENDPOINT",
			env:  map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com", "SCION_SERVER_HUB_ENDPOINT": "https://hub.example.com"},
			mode: hubOnlyMode, want: false,
		},
		{
			name: "server.broker.hub_endpoint and server.hub.public_url in settings, broker-only",
			settings: "hub:\n  endpoint: https://hub.example.com\nserver:\n  hub:\n    public_url: https://hub.example.com\n" +
				"  broker:\n    hub_endpoint: https://hub.example.com\n",
			mode: brokerOnlyMode, want: false,
		},
		{
			name: "SCION_SERVER_HUB_ENDPOINT and SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT, broker-only",
			env: map[string]string{
				"SCION_HUB_ENDPOINT":                     "https://hub.example.com",
				"SCION_SERVER_HUB_ENDPOINT":              "https://hub.example.com",
				"SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT": "https://hub.example.com",
			},
			mode: brokerOnlyMode, want: false,
		},
		{
			name:     "substrate shape: server.broker.hub_endpoint in settings plus SCION_HUB_ENDPOINT, broker-only",
			settings: "server:\n  broker:\n    hub_endpoint: https://hub.example.com\n",
			env:      map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com"},
			mode:     brokerOnlyMode, want: false,
		},
		{
			name: "substrate shape without server.broker.hub_endpoint, broker-only",
			env:  map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com"},
			mode: brokerOnlyMode, want: true,
		},
		{
			name: "SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT plus SCION_HUB_ENDPOINT, broker-only",
			env:  map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com", "SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT": "https://hub.example.com"},
			mode: brokerOnlyMode, want: false,
		},
		{name: "workstation localhost fallback, no legacy value", mode: workstationMode, want: false},
		{name: "workstation self-written hub.endpoint", settings: "hub:\n  endpoint: http://127.0.0.1:8080\n", mode: workstationMode, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globalDir := isolateServerHubEndpointEnv(t)
			if tt.settings != "" {
				writeGlobalSettings(t, globalDir, tt.settings)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := config.LoadGlobalConfig("")
			require.NoError(t, err)
			settings, err := config.LoadSettings("")
			require.NoError(t, err)

			logf, lines := collectLogf()
			warnDeprecatedServerHubEndpoint(cfg, tt.mode, settings, logf)
			if tt.want {
				assert.Equal(t, []string{serverHubEndpointDeprecationWarning}, *lines)
			} else {
				assert.Empty(t, *lines)
			}
		})
	}
}

// TestResolveServerHubEndpoint_StartupWarning checks startup's endpoint
// resolution: it reads the 'server start' mode, returns the resolved
// endpoint, and logs the exact A1.2 text once; with server.hub.public_url
// set it logs nothing.
func TestResolveServerHubEndpoint_StartupWarning(t *testing.T) {
	savedHub, savedBroker, savedWeb, savedHosted, savedPort, savedBase := enableHub, enableRuntimeBroker, enableWeb, hostedMode, webPort, webBaseURL
	t.Cleanup(func() {
		enableHub, enableRuntimeBroker, enableWeb, hostedMode, webPort, webBaseURL = savedHub, savedBroker, savedWeb, savedHosted, savedPort, savedBase
	})
	enableHub, enableRuntimeBroker, enableWeb, hostedMode, webPort, webBaseURL = true, true, true, true, 8080, ""

	var buf bytes.Buffer
	savedOut, savedFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(savedOut); log.SetFlags(savedFlags) })

	t.Run("legacy env value", func(t *testing.T) {
		buf.Reset()
		isolateServerHubEndpointEnv(t)
		t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")
		settings, err := config.LoadSettings("")
		require.NoError(t, err)

		endpoint, src := resolveServerHubEndpoint(newDeprecationTestConfig(), settings)
		assert.Equal(t, "https://hub.example.com", endpoint)
		assert.Equal(t, hubEndpointSourceSettings, src)
		assert.Equal(t, serverHubEndpointDeprecationWarning+"\n", buf.String())
		assert.Equal(t, 1, strings.Count(buf.String(), "deprecated:"))
	})
	t.Run("server.hub.public_url set", func(t *testing.T) {
		buf.Reset()
		isolateServerHubEndpointEnv(t)
		t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")
		settings, err := config.LoadSettings("")
		require.NoError(t, err)
		cfg := newDeprecationTestConfig()
		cfg.Hub.Endpoint = "https://public.example.com"

		endpoint, _ := resolveServerHubEndpoint(cfg, settings)
		assert.Equal(t, "https://public.example.com", endpoint)
		assert.Empty(t, buf.String())
	})
}

// TestResolveHubEndpointFrom_LegacyRemoved checks the counterfactual the
// warning relies on: without the legacy value the advertised endpoint falls
// through to the next tier and the broker-only endpoint is empty.
func TestResolveHubEndpointFrom_LegacyRemoved(t *testing.T) {
	isolateServerHubEndpointEnv(t)
	cfg := newDeprecationTestConfig()

	got, src := resolveHubEndpointFrom(cfg, workstationMode, "https://hub.example.com", nil)
	assert.Equal(t, "https://hub.example.com", got)
	assert.Equal(t, hubEndpointSourceSettings, src)
	got, src = resolveHubEndpointFrom(cfg, workstationMode, "", nil)
	assert.Equal(t, "http://localhost:8080", got)
	assert.Equal(t, hubEndpointSourceLocalhost, src)

	got, src = resolveHubEndpointFrom(cfg, brokerOnlyMode, "https://hub.example.com", nil)
	assert.Equal(t, "https://hub.example.com", got)
	assert.Equal(t, hubEndpointSourceBrokerSettings, src)
	assert.Equal(t, "https://hub.example.com", resolveBrokerHubEndpointFrom(cfg, brokerOnlyMode, "https://hub.example.com", nil))
	assert.Empty(t, resolveBrokerHubEndpointFrom(cfg, brokerOnlyMode, "", nil))
	assert.Equal(t, "http://localhost:8080", resolveBrokerHubEndpointFrom(cfg, workstationMode, "https://hub.example.com", nil),
		"the co-located broker ignores the legacy value")
}
