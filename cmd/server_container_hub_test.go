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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
)

const testIAPCloudRunURL = "https://scion-hub-123456.us-central1.run.app"

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
				Endpoint:                   "http://scion-hub.internal:8080",
				ColocatedPublicHubEndpoint: testIAPCloudRunURL,
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
				Endpoint:                   "http://host.docker.internal:8080",
				ColocatedPublicHubEndpoint: testIAPCloudRunURL,
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
				Endpoint:                   "http://host.docker.internal:8080",
				ColocatedPublicHubEndpoint: testIAPCloudRunURL,
			},
		},
		{
			// Hybrid GKE: the kubernetes default runtime gets no container
			// endpoint and no rewrite, so pods keep the IAP URL.
			name: "IAP-derived public URL on kubernetes is unchanged",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "kubernetes"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{},
		},
		{
			name: "explicit base URL with Caddy routes docker agents at the public domain",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = "https://hub.example.com/"
				in.PublicHubEndpointSource = hubEndpointSourceBaseURLEnv
			}),
			want: containerHubEndpointResult{Endpoint: "https://hub.example.com"},
		},
		{
			name: "explicit public_url with Caddy routes docker agents at the public domain",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = "https://hub.example.com"
				in.PublicHubEndpointSource = hubEndpointSourceConfig
			}),
			want: containerHubEndpointResult{Endpoint: "https://hub.example.com"},
		},
		{
			name: "explicit run.app base URL is trusted as served by this host",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceBaseURLFlag
			}),
			want: containerHubEndpointResult{Endpoint: testIAPCloudRunURL},
		},
		{
			name: "localhost public URL uses the legacy bridge host",
			in: with(func(in *containerHubEndpointInputs) {
				in.PublicHubEndpoint = "http://localhost:8080"
				in.PublicHubEndpointSource = hubEndpointSourceLocalhost
			}),
			want: containerHubEndpointResult{Endpoint: "http://host.docker.internal:8080"},
		},
		{
			name: "localhost public URL on podman uses host.containers.internal",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = "podman"
				in.PublicHubEndpoint = "http://localhost:8080"
				in.PublicHubEndpointSource = hubEndpointSourceLocalhost
			}),
			want: containerHubEndpointResult{Endpoint: "http://host.containers.internal:8080"},
		},
		{
			name: "configured container endpoint wins",
			in: with(func(in *containerHubEndpointInputs) {
				in.Configured = "http://custom:1234"
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{Endpoint: "http://custom:1234"},
		},
		{
			name: "broker-only mode computes nothing",
			in: with(func(in *containerHubEndpointInputs) {
				in.HubEnabled = false
				in.PublicHubEndpoint = testIAPCloudRunURL
				in.PublicHubEndpointSource = hubEndpointSourceIAPAudience
			}),
			want: containerHubEndpointResult{},
		},
		{
			name: "no runtime computes nothing",
			in: with(func(in *containerHubEndpointInputs) {
				in.RuntimeName = ""
			}),
			want: containerHubEndpointResult{},
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
