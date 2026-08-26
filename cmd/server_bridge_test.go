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
	"net/url"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestContainerBridgeEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		hubEndpoint string
		runtimeName string
		want        string
	}{
		{
			name:        "podman with localhost",
			hubEndpoint: "http://localhost:8080",
			runtimeName: "podman",
			want:        "http://host.containers.internal:8080",
		},
		{
			name:        "docker with localhost",
			hubEndpoint: "http://localhost:8080",
			runtimeName: "docker",
			want:        "http://host.docker.internal:8080",
		},
		{
			name:        "podman with 127.0.0.1",
			hubEndpoint: "http://127.0.0.1:9090",
			runtimeName: "podman",
			want:        "http://host.containers.internal:9090",
		},
		{
			name:        "docker with ipv6 loopback",
			hubEndpoint: "http://[::1]:8080",
			runtimeName: "docker",
			want:        "http://host.docker.internal:8080",
		},
		{
			name:        "non-localhost endpoint unchanged",
			hubEndpoint: "https://hub.example.com:443",
			runtimeName: "podman",
			want:        "",
		},
		{
			name:        "kubernetes returns empty",
			hubEndpoint: "http://localhost:8080",
			runtimeName: "kubernetes",
			want:        "",
		},
		{
			name:        "apple container with localhost",
			hubEndpoint: "http://localhost:8080",
			runtimeName: "container",
			want:        "http://host.containers.internal:8080",
		},
		{
			name:        "empty runtime returns empty",
			hubEndpoint: "http://localhost:8080",
			runtimeName: "",
			want:        "",
		},
		{
			name:        "empty endpoint returns empty",
			hubEndpoint: "",
			runtimeName: "podman",
			want:        "",
		},
		{
			name:        "invalid URL returns empty",
			hubEndpoint: "://not-a-url",
			runtimeName: "podman",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containerBridgeEndpoint(tt.hubEndpoint, tt.runtimeName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveHubEndpointSettingsFallback(t *testing.T) {
	// Save and restore global state
	origEnableHub := enableHub
	origEnableDebug := enableDebug
	origWebBaseURL := webBaseURL
	origEnableWeb := enableWeb
	origWebPort := webPort
	defer func() {
		enableHub = origEnableHub
		enableDebug = origEnableDebug
		webBaseURL = origWebBaseURL
		enableWeb = origEnableWeb
		webPort = origWebPort
	}()

	t.Run("settings endpoint used when hub enabled and no server config", func(t *testing.T) {
		enableHub = true
		enableDebug = false
		webBaseURL = ""
		enableWeb = true
		webPort = 8080
		t.Setenv("SCION_SERVER_BASE_URL", "")

		cfg := &config.GlobalConfig{}
		settings := &config.Settings{
			Hub: &config.HubClientConfig{
				Endpoint: "https://hub.demo.scion-ai.dev",
			},
		}

		got := resolveHubEndpoint(cfg, settings)
		assert.Equal(t, "https://hub.demo.scion-ai.dev", got)
	})

	t.Run("server config takes priority over settings", func(t *testing.T) {
		enableHub = true
		enableDebug = false
		webBaseURL = ""
		enableWeb = true
		webPort = 8080
		t.Setenv("SCION_SERVER_BASE_URL", "")

		cfg := &config.GlobalConfig{
			Hub: config.HubServerConfig{
				Endpoint: "https://server-config.example.com",
			},
		}
		settings := &config.Settings{
			Hub: &config.HubClientConfig{
				Endpoint: "https://settings.example.com",
			},
		}

		got := resolveHubEndpoint(cfg, settings)
		assert.Equal(t, "https://server-config.example.com", got)
	})

	t.Run("falls back to localhost when no settings", func(t *testing.T) {
		enableHub = true
		enableDebug = false
		webBaseURL = ""
		enableWeb = true
		webPort = 8080
		t.Setenv("SCION_SERVER_BASE_URL", "")

		cfg := &config.GlobalConfig{}
		settings := &config.Settings{}

		got := resolveHubEndpoint(cfg, settings)
		assert.Equal(t, "http://localhost:8080", got)
	})
}

func TestResolveHubEndpointForBroker(t *testing.T) {
	origEnableHub := enableHub
	origEnableDebug := enableDebug
	origEnableWeb := enableWeb
	origWebPort := webPort
	defer func() {
		enableHub = origEnableHub
		enableDebug = origEnableDebug
		enableWeb = origEnableWeb
		webPort = origWebPort
	}()

	t.Run("co-located always uses localhost even with SCION_SERVER_BASE_URL", func(t *testing.T) {
		enableHub = true
		enableDebug = false
		enableWeb = true
		webPort = 8080
		t.Setenv("SCION_SERVER_BASE_URL", "https://scionduet03.ameer.cloud")

		cfg := &config.GlobalConfig{}
		settings := &config.Settings{}

		got := resolveHubEndpointForBroker(cfg, settings)
		assert.Equal(t, "http://localhost:8080", got)
	})

	t.Run("co-located uses hub port when web disabled", func(t *testing.T) {
		enableHub = true
		enableDebug = false
		enableWeb = false
		webPort = 8080

		cfg := &config.GlobalConfig{
			Hub: config.HubServerConfig{Port: 9810},
		}
		settings := &config.Settings{}

		got := resolveHubEndpointForBroker(cfg, settings)
		assert.Equal(t, "http://localhost:9810", got)
	})

	t.Run("explicit RuntimeBroker.HubEndpoint takes priority", func(t *testing.T) {
		enableHub = true
		enableDebug = false
		enableWeb = true
		webPort = 8080

		cfg := &config.GlobalConfig{
			RuntimeBroker: config.RuntimeBrokerConfig{
				HubEndpoint: "https://custom-hub.example.com",
			},
		}
		settings := &config.Settings{}

		got := resolveHubEndpointForBroker(cfg, settings)
		assert.Equal(t, "https://custom-hub.example.com", got)
	})

	t.Run("non-hub mode uses settings endpoint", func(t *testing.T) {
		enableHub = false
		enableDebug = false
		enableWeb = false

		cfg := &config.GlobalConfig{}
		settings := &config.Settings{
			Hub: &config.HubClientConfig{
				Endpoint: "https://remote-hub.example.com",
			},
		}

		got := resolveHubEndpointForBroker(cfg, settings)
		assert.Equal(t, "https://remote-hub.example.com", got)
	})

	t.Run("non-hub mode returns empty when no settings", func(t *testing.T) {
		enableHub = false
		enableDebug = false
		enableWeb = false

		cfg := &config.GlobalConfig{}
		settings := &config.Settings{}

		got := resolveHubEndpointForBroker(cfg, settings)
		assert.Equal(t, "", got)
	})
}

func TestIsLocalhostURL(t *testing.T) {
	tests := []struct {
		endpoint string
		want     bool
	}{
		{"http://localhost:8080", true},
		{"http://127.0.0.1:9810", true},
		{"http://[::1]:8080", true},
		{"https://hub.example.com", false},
		{"https://hub.demo.scion-ai.dev", false},
		{"http://10.0.0.1:8080", false},
		{"", false},
		{"://invalid", false},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			assert.Equal(t, tt.want, isLocalhostURL(tt.endpoint))
		})
	}
}

func TestIAPAudienceToCloudRunURL(t *testing.T) {
	tests := []struct {
		name     string
		audience string
		want     string
	}{
		{
			name:     "valid audience",
			audience: "/projects/721899303052/locations/us-central1/services/scion-hub",
			want:     "https://scion-hub-721899303052.us-central1.run.app",
		},
		{
			name:     "valid audience with trailing slash",
			audience: "/projects/721899303052/locations/us-central1/services/scion-hub/",
			want:     "https://scion-hub-721899303052.us-central1.run.app",
		},
		{
			name:     "valid audience with whitespace",
			audience: "  /projects/123456/locations/europe-west1/services/my-svc  ",
			want:     "https://my-svc-123456.europe-west1.run.app",
		},
		{
			name:     "missing leading slash",
			audience: "projects/123456/locations/us-central1/services/scion-hub",
			want:     "",
		},
		{
			name:     "wrong structure",
			audience: "/projects/123456/regions/us-central1/services/scion-hub",
			want:     "",
		},
		{
			name:     "empty string",
			audience: "",
			want:     "",
		},
		{
			name:     "too few parts",
			audience: "/projects/123456/locations/us-central1",
			want:     "",
		},
		{
			name:     "empty service name",
			audience: "/projects/123456/locations/us-central1/services/",
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := iapAudienceToCloudRunURL(tt.audience)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestResolveHubListenPort_HostedTier pins the contract that
// resolveHubListenPort returns a non-zero port in co-located hosted mode.
// cloudrunSandboxHubEndpoint depends on this: it takes HubListenPort and
// uses it to build http://<link-local>:<port>. If someone later changes
// resolveHubListenPort to return 0 in hosted mode, every cloudrun-sandbox
// agent start will 500 — and the failing test will be here, next to the
// change, not in a distant package.
func TestResolveHubListenPort_HostedTier(t *testing.T) {
	origEnableHub := enableHub
	origEnableWeb := enableWeb
	origWebPort := webPort
	defer func() {
		enableHub = origEnableHub
		enableWeb = origEnableWeb
		webPort = origWebPort
	}()

	t.Run("combined web+API mode returns web port", func(t *testing.T) {
		enableHub = true
		enableWeb = true
		webPort = 8080

		cfg := &config.GlobalConfig{Hub: config.HubServerConfig{Port: 9810}}
		port := resolveHubListenPort(cfg)
		assert.Equal(t, 8080, port, "should use webPort in combined mode")
	})

	t.Run("standalone hub mode returns hub port", func(t *testing.T) {
		enableHub = true
		enableWeb = false
		webPort = 8080

		cfg := &config.GlobalConfig{Hub: config.HubServerConfig{Port: 9810}}
		port := resolveHubListenPort(cfg)
		assert.Equal(t, 9810, port, "should use cfg.Hub.Port in standalone mode")
	})

	t.Run("non-colocated returns zero", func(t *testing.T) {
		enableHub = false
		enableWeb = true
		webPort = 8080

		cfg := &config.GlobalConfig{Hub: config.HubServerConfig{Port: 9810}}
		port := resolveHubListenPort(cfg)
		assert.Equal(t, 0, port, "should be 0 when hub is not colocated")
	})
}

// TestResolveHubEndpointForBroker_ExplicitPort pins the contract that the
// broker's hub endpoint always carries an explicit port in co-located mode.
// cloudrunSandboxHubEndpoint (in pkg/runtimebroker) parses this via
// url.Port() to determine what port the hub is listening on. If
// resolveHubEndpointForBroker ever emits a URL without an explicit port
// (e.g. scheme-default), url.Port() returns "" and sandbox agent starts fail.
func TestResolveHubEndpointForBroker_ExplicitPort(t *testing.T) {
	origEnableHub := enableHub
	origEnableWeb := enableWeb
	origEnableDebug := enableDebug
	origWebPort := webPort
	defer func() {
		enableHub = origEnableHub
		enableWeb = origEnableWeb
		enableDebug = origEnableDebug
		webPort = origWebPort
	}()

	enableHub = true
	enableWeb = true
	enableDebug = false
	webPort = 8080

	cfg := &config.GlobalConfig{Hub: config.HubServerConfig{Port: 9810}}
	settings := &config.Settings{}

	endpoint := resolveHubEndpointForBroker(cfg, settings)

	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("endpoint %q is not a valid URL: %v", endpoint, err)
	}
	if u.Port() == "" {
		t.Fatalf("resolveHubEndpointForBroker returned %q which has no explicit port — "+
			"cloudrunSandboxHubEndpoint depends on url.Port() returning a non-empty value "+
			"in co-located mode", endpoint)
	}
	assert.Equal(t, "8080", u.Port(), "port should match webPort")
}
