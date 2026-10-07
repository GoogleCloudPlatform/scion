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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/stretchr/testify/assert"
)

const testIAPCloudRunURL = "https://scion-hub-123456.us-central1.run.app"

// iapTargets is the per-runtime rewrite of an IAP-derived public URL: docker
// at dockerTarget, podman at Podman's native host alias.
func iapTargets(dockerTarget string) map[string]string {
	return map[string]string{
		"docker": dockerTarget,
		"podman": "http://host.containers.internal:8080",
	}
}

func TestComputeContainerHubEndpoint(t *testing.T) {
	base := containerHubEndpointInputs{
		HubEnabled:        true,
		BrokerHubEndpoint: "http://localhost:8080",
		RuntimeName:       "docker",
		HubListenPort:     8080,
	}
	with := func(mod func(*containerHubEndpointInputs)) containerHubEndpointInputs {
		in := base
		mod(&in)
		return in
	}

	tests := []struct {
		name string
		in   containerHubEndpointInputs
		want containerHubEndpointResult
	}{
		{
			name: "IAP-derived public URL on docker uses the local hub alias",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{
				Endpoint:                     "http://scion-hub.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://scion-hub.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name: "IAP-derived alias uses the hub listen port",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = testIAPCloudRunURL + "/"
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
				in.HubListenPort = 9810
			}),
			want: containerHubEndpointResult{
				Endpoint:                   "http://scion-hub.internal:9810",
				ColocatedPublicHubEndpoint: testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: map[string]string{
					"docker": "http://scion-hub.internal:9810",
					"podman": "http://host.containers.internal:9810",
				},
				HubListenPort: 9810,
			},
		},
		{
			name: "IAP-derived public URL with forced host networking rewrites to the bridge host",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
				in.ForceHostNetwork = true
			}),
			want: containerHubEndpointResult{
				Endpoint:                     "http://host.docker.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://host.docker.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name: "IAP-derived public URL with unknown listen port never routes at the IAP URL",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
				in.HubListenPort = 0
			}),
			want: containerHubEndpointResult{
				Endpoint:                     "http://host.docker.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://host.docker.internal:8080"),
				HubListenPort:                0,
			},
		},
		{
			name: "IAP-derived public URL without host-gateway support rewrites to the bridge host",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
				in.HostGatewaySupported = func() bool { return false }
			}),
			want: containerHubEndpointResult{
				Endpoint:                     "http://host.docker.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://host.docker.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			// Hybrid GKE (ptone/scion#3635): the kubernetes default runtime
			// gets no container endpoint, so pods keep the IAP URL, but
			// agents dispatched through a docker or podman profile are
			// still rewritten.
			name: "IAP-derived public URL on kubernetes default still rewrites docker and podman profiles",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "kubernetes"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://scion-hub.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name: "IAP-derived public URL on kubernetes default without host-gateway uses the docker bridge host",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "kubernetes"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
				in.HostGatewaySupported = func() bool { return false }
			}),
			want: containerHubEndpointResult{
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://host.docker.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			// ptone/scion#3635: the podman default runtime rewrites the
			// IAP-derived URL to host.containers.internal.
			name: "IAP-derived public URL on podman default rewrites to the podman host alias",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "podman"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{
				Endpoint:                     "http://host.containers.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://scion-hub.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name: "IAP-derived public URL on podman default with forced host networking",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "podman"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
				in.ForceHostNetwork = true
			}),
			want: containerHubEndpointResult{
				Endpoint:                     "http://host.containers.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://host.docker.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name: "IAP-derived public URL with a non-localhost broker endpoint has no bridge targets",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "kubernetes"
				in.BrokerHubEndpoint = "http://10.0.0.5:8080"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
				in.HubListenPort = 0
			}),
			want: containerHubEndpointResult{},
		},
		{
			// A non-localhost runtime_broker.hub_endpoint must not drop the
			// targets: they are built from the hub listen port.
			name: "IAP-derived public URL with a non-localhost broker endpoint uses the listen port",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "kubernetes"
				in.BrokerHubEndpoint = "http://10.0.0.5:9000"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://scion-hub.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name: "IAP-derived public URL with a non-localhost broker endpoint and host networking uses the listen port",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "kubernetes"
				in.BrokerHubEndpoint = "http://10.0.0.5:9000"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
				in.ForceHostNetwork = true
			}),
			want: containerHubEndpointResult{
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://host.docker.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name: "IAP-derived public URL on Apple container keeps its own endpoint and adds per-runtime targets only",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "container"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{
				Endpoint:                     "http://host.containers.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://scion-hub.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name: "explicit public URL on kubernetes default adds no rewrite",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "kubernetes"
				in.PublicHubEndpoint = "https://hub.example.com"
				in.PublicHubEndpointSource = hubEndpointSourceConfig
			}),
			want: containerHubEndpointResult{HubListenPort: 8080},
		},
		{
			name: "explicit base URL with Caddy routes docker agents at the public domain",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = "https://hub.example.com/"
				in.PublicHubEndpointSource = hubEndpointSourceBaseURLEnv
			}),
			want: containerHubEndpointResult{Endpoint: "https://hub.example.com", HubListenPort: 8080},
		},
		{
			name: "explicit public_url with Caddy routes docker agents at the public domain",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = "https://hub.example.com"
				in.PublicHubEndpointSource = hubEndpointSourceConfig
			}),
			want: containerHubEndpointResult{Endpoint: "https://hub.example.com", HubListenPort: 8080},
		},
		{
			name: "explicit run.app base URL is trusted as served by this host",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceBaseURLFlag
			}),
			want: containerHubEndpointResult{Endpoint: testIAPCloudRunURL, HubListenPort: 8080},
		},
		{
			name: "localhost public URL uses the legacy bridge host",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = "http://localhost:8080"
				in.PublicHubEndpointSource = hubEndpointSourceLocalhost
			}),
			want: containerHubEndpointResult{Endpoint: "http://host.docker.internal:8080", HubListenPort: 8080},
		},
		{
			name: "localhost public URL on podman uses host.containers.internal",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "podman"
				in.PublicHubEndpoint = "http://localhost:8080"
				in.PublicHubEndpointSource = hubEndpointSourceLocalhost
			}),
			want: containerHubEndpointResult{Endpoint: "http://host.containers.internal:8080", HubListenPort: 8080},
		},
		{
			name: "configured container endpoint wins",
			in: with(func(in *containerHubEndpointInputs) {
				in.Configured = "http://custom:1234"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{Endpoint: "http://custom:1234", HubListenPort: 8080},
		},
		{
			name: "broker-only mode computes nothing",
			in: with(func(in *containerHubEndpointInputs) {
				in.HubEnabled = false
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{HubListenPort: 8080},
		},
		{
			name: "no runtime computes nothing",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = ""
			}),
			want: containerHubEndpointResult{HubListenPort: 8080},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeContainerHubEndpoint(tt.in, nil)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveHubEndpointWithSource(t *testing.T) {
	origEnableHub, origEnableDebug, origWebBaseURL := enableHub, enableDebug, webBaseURL
	origEnableWeb, origWebPort, origHostedMode := enableWeb, webPort, hostedMode
	defer func() {
		enableHub, enableDebug, webBaseURL = origEnableHub, origEnableDebug, origWebBaseURL
		enableWeb, webPort, hostedMode = origEnableWeb, origWebPort, origHostedMode
	}()

	iapCfg := func() *config.GlobalConfig {
		return &config.GlobalConfig{
			Auth: config.DevAuthConfig{
				Proxy: &config.ProxyAuthConfig{
					IAP: &config.IAPAuthConfig{
						Audience: "/projects/123456/locations/us-central1/services/scion-hub",
					},
				},
			},
		}
	}

	tests := []struct {
		name       string
		baseURL    string
		envBaseURL string
		cfg        *config.GlobalConfig
		settings   *config.Settings
		wantURL    string
		wantSource hubEndpointSource
	}{
		{
			name:       "IAP audience in hosted mode",
			cfg:        iapCfg(),
			wantURL:    testIAPCloudRunURL,
			wantSource: hubEndpointSourceIAPAudience,
		},
		{
			name:       "explicit base URL beats IAP audience",
			envBaseURL: "https://hub.example.com/",
			cfg:        iapCfg(),
			wantURL:    "https://hub.example.com",
			wantSource: hubEndpointSourceBaseURLEnv,
		},
		{
			name:       "base-url flag",
			baseURL:    "https://flag.example.com",
			cfg:        iapCfg(),
			wantURL:    "https://flag.example.com",
			wantSource: hubEndpointSourceBaseURLFlag,
		},
		{
			name: "server config endpoint",
			cfg: &config.GlobalConfig{
				Hub: config.HubServerConfig{Endpoint: "https://server-config.example.com"},
			},
			wantURL:    "https://server-config.example.com",
			wantSource: hubEndpointSourceConfig,
		},
		{
			name:       "settings endpoint",
			cfg:        iapCfg(),
			settings:   &config.Settings{Hub: &config.HubClientConfig{Endpoint: "https://settings.example.com"}},
			wantURL:    "https://settings.example.com",
			wantSource: hubEndpointSourceSettings,
		},
		{
			name:       "localhost fallback",
			cfg:        &config.GlobalConfig{},
			wantURL:    "http://localhost:8080",
			wantSource: hubEndpointSourceLocalhost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enableHub, enableDebug, enableWeb, webPort, hostedMode = true, false, true, 8080, true
			webBaseURL = tt.baseURL
			t.Setenv("SCION_SERVER_BASE_URL", tt.envBaseURL)
			settings := tt.settings
			if settings == nil {
				settings = &config.Settings{}
			}

			gotURL, gotSource := resolveHubEndpointWithSource(tt.cfg, settings)
			assert.Equal(t, tt.wantURL, gotURL)
			assert.Equal(t, tt.wantSource, gotSource)
			// The URL-only wrapper must return the same URL: links, the OIDC
			// issuer and the transport audience depend on it.
			assert.Equal(t, tt.wantURL, resolveHubEndpoint(tt.cfg, settings))
		})
	}
}

func TestBrokerContainerHubConfig(t *testing.T) {
	origEnableHub, origEnableWeb, origWebPort := enableHub, enableWeb, webPort
	defer func() { enableHub, enableWeb, webPort = origEnableHub, origEnableWeb, origWebPort }()
	enableHub, enableWeb, webPort = true, true, 8080

	const brokerHub = "http://localhost:8080"
	tests := []struct {
		name        string
		cfg         *config.GlobalConfig
		runtimeName string
		hubEndpoint string
		src         hubEndpointSource
		forceHost   bool
		probe       bool // host-gateway support reported by the probe
		wantProbed  bool
		want        containerHubEndpointResult
	}{
		{
			name:        "IAP source on docker returns the alias and the rewrite target",
			cfg:         &config.GlobalConfig{},
			runtimeName: "docker",
			hubEndpoint: testIAPCloudRunURL,
			src:         hubEndpointSourceIAPAudience,
			probe:       true,
			wantProbed:  true,
			want: containerHubEndpointResult{
				Endpoint:                     "http://scion-hub.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://scion-hub.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name:        "IAP source on docker without host-gateway uses the bridge host",
			cfg:         &config.GlobalConfig{},
			runtimeName: "docker",
			hubEndpoint: testIAPCloudRunURL,
			src:         hubEndpointSourceIAPAudience,
			probe:       false,
			wantProbed:  true,
			want: containerHubEndpointResult{
				Endpoint:                     "http://host.docker.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://host.docker.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name:        "forced host networking skips the probe",
			cfg:         &config.GlobalConfig{},
			runtimeName: "docker",
			hubEndpoint: testIAPCloudRunURL,
			src:         hubEndpointSourceIAPAudience,
			forceHost:   true,
			want: containerHubEndpointResult{
				Endpoint:                     "http://host.docker.internal:8080",
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://host.docker.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name:        "configured container endpoint skips the probe",
			cfg:         &config.GlobalConfig{RuntimeBroker: config.RuntimeBrokerConfig{ContainerHubEndpoint: "http://custom:1234"}},
			runtimeName: "docker",
			hubEndpoint: testIAPCloudRunURL,
			src:         hubEndpointSourceIAPAudience,
			want:        containerHubEndpointResult{Endpoint: "http://custom:1234", HubListenPort: 8080},
		},
		{
			name:        "kubernetes default runtime probes for the docker profile target",
			cfg:         &config.GlobalConfig{},
			runtimeName: "kubernetes",
			hubEndpoint: testIAPCloudRunURL,
			src:         hubEndpointSourceIAPAudience,
			probe:       true,
			wantProbed:  true,
			want: containerHubEndpointResult{
				ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
				ColocatedRuntimeHubEndpoints: iapTargets("http://scion-hub.internal:8080"),
				HubListenPort:                8080,
			},
		},
		{
			name:        "kubernetes default runtime with an explicit public URL skips the probe",
			cfg:         &config.GlobalConfig{},
			runtimeName: "kubernetes",
			hubEndpoint: "https://hub.example.com",
			src:         hubEndpointSourceConfig,
			want:        containerHubEndpointResult{HubListenPort: 8080},
		},
		{
			name:        "podman default runtime with a localhost hub skips the probe",
			cfg:         &config.GlobalConfig{},
			runtimeName: "podman",
			hubEndpoint: "http://localhost:8080",
			src:         hubEndpointSourceLocalhost,
			want:        containerHubEndpointResult{Endpoint: "http://host.containers.internal:8080", HubListenPort: 8080},
		},
		{
			name:        "explicit base URL on docker routes at the public domain",
			cfg:         &config.GlobalConfig{},
			runtimeName: "docker",
			hubEndpoint: "https://hub.example.com",
			src:         hubEndpointSourceBaseURLEnv,
			probe:       true,
			wantProbed:  true,
			want:        containerHubEndpointResult{Endpoint: "https://hub.example.com", HubListenPort: 8080},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.forceHost {
				t.Setenv(runtime.ForceHostNetworkEnvVar, "1")
			} else {
				t.Setenv(runtime.ForceHostNetworkEnvVar, "")
			}
			probed := false
			probe := func() bool { probed = true; return tt.probe }

			got := brokerContainerHubConfig(tt.cfg, brokerContainerHubParams{
				RuntimeName:             tt.runtimeName,
				BrokerHubEndpoint:       brokerHub,
				PublicHubEndpoint:       tt.hubEndpoint,
				PublicHubEndpointSource: tt.src,
				HostGatewayProbe:        probe,
			}, nil)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantProbed, probed, "host-gateway probe called")
		})
	}
}

func TestContainerHubEndpointResultApplyTo(t *testing.T) {
	cfg := runtimebroker.ServerConfig{BrokerID: "keep"}
	containerHubEndpointResult{
		Endpoint:                     "http://scion-hub.internal:8080",
		ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
		ColocatedRuntimeHubEndpoints: iapTargets("http://scion-hub.internal:8080"),
		HubListenPort:                8080,
	}.applyTo(&cfg)
	assert.Equal(t, "http://scion-hub.internal:8080", cfg.ContainerHubEndpoint)
	assert.Equal(t, testIAPCloudRunURL, cfg.ColocatedPublicHubEndpoint)
	assert.Equal(t, iapTargets("http://scion-hub.internal:8080"), cfg.ColocatedRuntimeHubEndpoints)
	assert.Equal(t, 8080, cfg.HubListenPort)
	assert.Equal(t, "keep", cfg.BrokerID)
}

// TestBrokerContainerHubConfigPodmanOnlyBroker: a podman-only broker (no
// docker binary) on an IAP-derived hub runs the real Docker host-gateway
// probe at startup. The probe must fail fast rather than delay startup, and
// podman agents must still get the podman target.
func TestBrokerContainerHubConfigPodmanOnlyBroker(t *testing.T) {
	origEnableHub, origEnableWeb, origWebPort := enableHub, enableWeb, webPort
	defer func() { enableHub, enableWeb, webPort = origEnableHub, origEnableWeb, origWebPort }()
	enableHub, enableWeb, webPort = true, true, 8080
	t.Setenv(runtime.ForceHostNetworkEnvVar, "")
	t.Setenv("PATH", t.TempDir()) // no docker binary

	probed := false
	start := time.Now()
	got := brokerContainerHubConfig(&config.GlobalConfig{}, brokerContainerHubParams{
		RuntimeName:             "podman",
		BrokerHubEndpoint:       "http://localhost:8080",
		PublicHubEndpoint:       testIAPCloudRunURL,
		PublicHubEndpointSource: hubEndpointSourceIAPAudience,
		HostGatewayProbe: func() bool {
			probed = true
			return runtime.DockerSupportsHostGateway(context.Background(), "")
		},
	}, nil)
	elapsed := time.Since(start)

	assert.True(t, probed, "host-gateway probe called")
	assert.Less(t, elapsed, 2*time.Second, "probe without a docker binary must fail fast")
	assert.Equal(t, containerHubEndpointResult{
		Endpoint:                     "http://host.containers.internal:8080",
		ColocatedPublicHubEndpoint:   testIAPCloudRunURL,
		ColocatedRuntimeHubEndpoints: iapTargets("http://scion-hub.internal:8080"),
		HubListenPort:                8080,
	}, got)
}

// TestComputeContainerHubEndpointHostGatewayLogLevel: a failed host-gateway
// probe is a warning on a docker default runtime. On a podman default (for
// example with the podman-docker shim, where `docker version` reports
// Podman's version) it only affects docker profiles and is logged as info.
func TestComputeContainerHubEndpointHostGatewayLogLevel(t *testing.T) {
	for _, tt := range []struct {
		runtimeName string
		wantPrefix  string
	}{
		{runtimeName: "docker", wantPrefix: "WARNING: host-gateway support not detected via docker"},
		{runtimeName: "podman", wantPrefix: "INFO: host-gateway support not detected via docker"},
	} {
		t.Run(tt.runtimeName, func(t *testing.T) {
			var logs []string
			computeContainerHubEndpoint(containerHubEndpointInputs{
				HubEnabled:              true,
				BrokerHubEndpoint:       "http://localhost:8080",
				RuntimeName:             tt.runtimeName,
				HubListenPort:           8080,
				PublicHubEndpoint:       testIAPCloudRunURL,
				PublicHubEndpointSource: hubEndpointSourceIAPAudience,
				HostGatewaySupported:    func() bool { return false },
			}, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
			found := false
			for _, l := range logs {
				if strings.HasPrefix(l, tt.wantPrefix) {
					found = true
				}
				assert.NotContains(t, l, "Docker daemon lacks host-gateway")
			}
			assert.True(t, found, "logs %v lack %q", logs, tt.wantPrefix)
		})
	}
}
