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
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// substrateBrokerManifestHubURL is the value substituted for ${HUB_ENDPOINT}
// when the substrate broker manifest is rendered for these tests.
const substrateBrokerManifestHubURL = "https://hub.substrate.example.com"

// substrateBrokerManifest is the part of the rendered
// deploy/substrate/broker.yaml the hub endpoint tests depend on.
type substrateBrokerManifest struct {
	// settings is the settings.yaml held by the broker's ConfigMap.
	settings string
	// args and env are the broker container's arguments and env names.
	args []string
	env  map[string]string
}

// loadSubstrateBrokerManifest renders the real deploy/substrate/broker.yaml
// (${HUB_ENDPOINT} set to substrateBrokerManifestHubURL, every other
// placeholder to a dummy value) and extracts the broker's settings and
// container spec.
func loadSubstrateBrokerManifest(t *testing.T) substrateBrokerManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "deploy", "substrate", "broker.yaml"))
	require.NoError(t, err)
	expanded := os.Expand(string(raw), func(name string) string {
		if name == "HUB_ENDPOINT" {
			return substrateBrokerManifestHubURL
		}
		return "placeholder"
	})

	m := substrateBrokerManifest{env: map[string]string{}}
	foundBroker := false
	dec := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Data     map[string]string     `yaml:"data"`
			Spec     struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string   `yaml:"name"`
							Args []string `yaml:"args"`
							Env  []struct {
								Name  string `yaml:"name"`
								Value string `yaml:"value"`
							} `yaml:"env"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		switch doc.Kind {
		case "ConfigMap":
			if doc.Metadata.Name == "scion-substrate-broker-settings" {
				m.settings = doc.Data["settings.yaml"]
			}
		case "Deployment":
			for _, c := range doc.Spec.Template.Spec.Containers {
				if len(c.Args) < 2 || c.Args[0] != "server" || c.Args[1] != "start" {
					continue
				}
				foundBroker = true
				m.args = c.Args
				for _, e := range c.Env {
					m.env[e.Name] = e.Value
				}
			}
		}
	}
	require.NotEmpty(t, m.settings, "scion-substrate-broker-settings ConfigMap settings.yaml not found in broker.yaml")
	require.True(t, foundBroker, "no `server start` container found in broker.yaml's Deployment")
	return m
}

// TestSubstrateBrokerManifest_HubEndpointFromServerKey pins the shape the
// hub endpoint table test below relies on: the substrate broker runs
// broker-only, takes its hub from server.broker.hub_endpoint in its
// ConfigMap, and no longer sets the legacy SCION_HUB_ENDPOINT fallback.
func TestSubstrateBrokerManifest_HubEndpointFromServerKey(t *testing.T) {
	m := loadSubstrateBrokerManifest(t)

	assert.Contains(t, m.args, "--enable-runtime-broker")
	assert.Contains(t, m.args, "--hosted")
	assert.NotContains(t, m.args, "--enable-hub", "the substrate broker is expected to run broker-only")

	var settings struct {
		Server struct {
			Broker struct {
				HubEndpoint string `yaml:"hub_endpoint"`
			} `yaml:"broker"`
		} `yaml:"server"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(m.settings), &settings))
	assert.Equal(t, substrateBrokerManifestHubURL, settings.Server.Broker.HubEndpoint)

	assert.NotContains(t, m.env, "SCION_HUB_ENDPOINT",
		"the broker takes its hub from server.broker.hub_endpoint; the legacy env fallback should not be set")
}

// TestSubstrateBrokerHubEndpointResolution shows that removing
// SCION_HUB_ENDPOINT from the substrate broker leaves its hub endpoint
// unchanged. It loads the manifest's ConfigMap settings through the same
// loaders server start uses, in broker-only hosted mode, with the env var
// unset (the manifest now), set to the same value (the manifest before),
// and set to a different value:
//   - the broker's own hub (resolveHubEndpointForBroker) is the ConfigMap
//     value in every case;
//   - the public hub endpoint (resolveHubEndpointWithSource) does follow
//     the env var, but a broker-only server does not consume it: the
//     container hub settings derived from it are the same in every case.
func TestSubstrateBrokerHubEndpointResolution(t *testing.T) {
	m := loadSubstrateBrokerManifest(t)

	origEnableHub, origEnableBroker, origEnableWeb := enableHub, enableRuntimeBroker, enableWeb
	origHostedMode, origEnableDebug, origWebBaseURL := hostedMode, enableDebug, webBaseURL
	t.Cleanup(func() {
		enableHub, enableRuntimeBroker, enableWeb = origEnableHub, origEnableBroker, origEnableWeb
		hostedMode, enableDebug, webBaseURL = origHostedMode, origEnableDebug, origWebBaseURL
	})

	const otherHubURL = "https://other-hub.example.com"

	tests := []struct {
		name string
		// env is SCION_HUB_ENDPOINT; "" leaves it unset.
		env            string
		wantPublic     string
		wantPublicFrom hubEndpointSource
	}{
		{
			name:           "env unset (manifest without the fallback)",
			wantPublic:     "",
			wantPublicFrom: hubEndpointSourceNone,
		},
		{
			name:           "env set to the ConfigMap value (manifest with the fallback)",
			env:            substrateBrokerManifestHubURL,
			wantPublic:     substrateBrokerManifestHubURL,
			wantPublicFrom: hubEndpointSourceBrokerSettings,
		},
		{
			name:           "env set to a different value",
			env:            otherHubURL,
			wantPublic:     otherHubURL,
			wantPublicFrom: hubEndpointSourceBrokerSettings,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(m.settings), 0o644))
			// The pod's working directory is not inside a project.
			t.Chdir(home)
			if tt.env != "" {
				t.Setenv("SCION_HUB_ENDPOINT", tt.env)
			} else {
				t.Setenv("SCION_HUB_ENDPOINT", "")
				require.NoError(t, os.Unsetenv("SCION_HUB_ENDPOINT"))
			}

			// server start --hosted --enable-runtime-broker (no --enable-hub).
			enableHub, enableRuntimeBroker, enableWeb = false, true, false
			hostedMode, enableDebug, webBaseURL = true, false, ""

			cfg, err := config.LoadGlobalConfig("")
			require.NoError(t, err)
			settings, _ := loadServerSettings("")
			require.Equal(t, substrateBrokerManifestHubURL, cfg.RuntimeBroker.HubEndpoint,
				"server.broker.hub_endpoint from the ConfigMap should reach the server config")

			brokerHub := resolveHubEndpointForBroker(cfg, settings)
			assert.Equal(t, substrateBrokerManifestHubURL, brokerHub,
				"the broker's own hub endpoint should be the ConfigMap value")

			public, publicFrom := resolveHubEndpointWithSource(cfg, settings)
			assert.Equal(t, tt.wantPublic, public)
			assert.Equal(t, tt.wantPublicFrom, publicFrom)

			// The public endpoint's only broker-only consumer: it is
			// ignored when the hub is disabled.
			got := brokerContainerHubConfig(cfg, brokerContainerHubParams{
				RuntimeName:             "substrate",
				BrokerHubEndpoint:       brokerHub,
				PublicHubEndpoint:       public,
				PublicHubEndpointSource: publicFrom,
				HostGatewayProbe: func() bool {
					t.Fatal("host-gateway probe should not run on a broker-only server")
					return false
				},
			}, nil)
			assert.Equal(t, containerHubEndpointResult{}, got)
		})
	}
}
