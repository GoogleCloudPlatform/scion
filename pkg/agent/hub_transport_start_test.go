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

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// startHubTransportFixture starts an agent through the real Manager.Start on
// a mock runtime named runtimeName, carrying a hub-issued agent token, and
// returns the RunConfig the runtime received (nil when Run was never called).
// agentHub and configEnvHub are the agent config's hub.endpoint and
// env.SCION_HUB_ENDPOINT ("" = unset).
func startHubTransportFixture(t *testing.T, runtimeName, hubEndpoint, agentHub, configEnvHub string, extraHosts []string) (*runtime.RunConfig, error) {
	t.Helper()
	t.Setenv(runtime.ForceHostNetworkEnvVar, "")
	const agentName = "hub-transport-agent"
	projectScionDir := setupTrustedHubEndpointTestProject(t, agentName, agentHub, configEnvHub)

	var got *runtime.RunConfig
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			c := cfg
			got = &c
			return "mock-id", nil
		},
	}
	_, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name:               agentName,
		ProjectPath:        projectScionDir,
		BrokerMode:         true,
		NoAuth:             true,
		ExtraHosts:         extraHosts,
		TrustedHubEndpoint: hubEndpoint,
		HubIssuedToken:     fakeHubIssuedToken,
		Env: map[string]string{
			"SCION_AGENT_ID":     "agent-uuid-hub-transport",
			"SCION_HUB_ENDPOINT": hubEndpoint,
			"SCION_HUB_URL":      hubEndpoint,
		},
	})
	return got, err
}

func assertHubTransportRefusedBeforeRun(t *testing.T, cfg *runtime.RunConfig, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Start succeeded, want a refusal for an unencrypted hub endpoint that is not host-local")
	}
	// config.ErrPlaintextNonLoopback, not this package's own sentinel, so
	// this file also compiles against the base revision for red evidence.
	if !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Errorf("error %q does not wrap config.ErrPlaintextNonLoopback", err)
	}
	if strings.Contains(err.Error(), fakeHubIssuedToken) {
		t.Error("the refusal shows the agent token")
	}
	if cfg != nil {
		t.Error("the runtime's Run was called; a refused start must not create a container")
	}
}

// TestStart_HubTransportRefusedPerRuntime: an in-cluster http hub endpoint is
// refused on every network-exposed runtime, before the runtime's Run.
func TestStart_HubTransportRefusedPerRuntime(t *testing.T) {
	for _, rt := range []string{"kubernetes", "k8s", "remote", "substrate", "cloudrun", "mock"} {
		t.Run(rt, func(t *testing.T) {
			cfg, err := startHubTransportFixture(t, rt, "http://scion-hub.scion.svc.cluster.local:8080", "", "", nil)
			assertHubTransportRefusedBeforeRun(t, cfg, err)
		})
	}
}

// TestStart_HubTransportRefusesLocalRuntimeNetworkHost: on a local runtime, a
// plaintext hub endpoint that is not one of its host aliases is refused.
func TestStart_HubTransportRefusesLocalRuntimeNetworkHost(t *testing.T) {
	for _, tc := range []struct{ runtime, endpoint string }{
		{"docker", "http://10.0.0.5:8080"},
		{"docker", "http://workstation.lan:8080"},
		{"podman", "http://172.17.0.1:8080"},
		{"container", "http://192.168.64.1:8080"},
		{"cloudrun-sandbox", "http://10.1.2.3:8080"},
		// A non-canonical spelling of the Docker alias gets neither host
		// networking nor the host-gateway entry (both match the canonical
		// lower-case name only), so it would go to the container's DNS.
		{"docker", "http://Host.Docker.Internal:8080"},
		// A trailing dot gets host networking, whose rewrite yields
		// "localhost.", which is not a literal loopback host; this row
		// guards that path, not the alias rule (the alias table covers it).
		{"docker", "http://host.docker.internal.:8080"},
	} {
		t.Run(tc.runtime+" "+tc.endpoint, func(t *testing.T) {
			cfg, err := startHubTransportFixture(t, tc.runtime, tc.endpoint, "", "", nil)
			assertHubTransportRefusedBeforeRun(t, cfg, err)
		})
	}
}

// TestStart_HubTransportCatchesAgentConfigOverride: the agent config's
// hub.endpoint replaces the broker's endpoint after injection; a plaintext
// network value from there is refused too.
func TestStart_HubTransportCatchesAgentConfigOverride(t *testing.T) {
	cfg, err := startHubTransportFixture(t, "docker", "http://host.docker.internal:8080", "http://agent-config.example:9810", "", nil)
	assertHubTransportRefusedBeforeRun(t, cfg, err)
}

// TestStart_HubTransportCatchesConfigEnvOverride: the config env
// SCION_HUB_ENDPOINT takes final priority; a plaintext network value from
// there is refused too.
func TestStart_HubTransportCatchesConfigEnvOverride(t *testing.T) {
	cfg, err := startHubTransportFixture(t, "docker", "https://hub.example.com", "", "http://config-env.example:9810", nil)
	assertHubTransportRefusedBeforeRun(t, cfg, err)
}

// TestStart_HubTransportAccepts: endpoints the policy allows still start.
func TestStart_HubTransportAccepts(t *testing.T) {
	for _, tc := range []struct {
		runtime, endpoint string
		extraHosts        []string
	}{
		{"kubernetes", "https://hub.example.com", nil},
		{"substrate", "https://hub.scion-system.svc.cluster.local", nil},
		{"kubernetes", "http://127.0.0.1:9810", nil},
		{"docker", "http://host.docker.internal:8080", nil},
		{"docker", "http://scion-hub.internal:8080", []string{"scion-hub.internal:host-gateway"}},
		{"podman", "http://host.containers.internal:8080", nil},
		{"container", "http://host.containers.internal:8080", nil},
		{"cloudrun-sandbox", "http://169.254.1.2:8080", nil},
	} {
		t.Run(tc.runtime+" "+tc.endpoint, func(t *testing.T) {
			cfg, err := startHubTransportFixture(t, tc.runtime, tc.endpoint, "", "", tc.extraHosts)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if cfg == nil {
				t.Fatal("the runtime's Run was never called")
			}
		})
	}
}
