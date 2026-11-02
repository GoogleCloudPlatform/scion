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
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestCheckHostNetworkMetadata(t *testing.T) {
	stubHostNetworkBlockPolicy(t, "", runtime.CloudHost{})
	tests := []struct {
		name        string
		runtime     string
		networkMode string
		env         []string
		wantErr     bool
		wantWarning bool
	}{
		{name: "docker host assign refused", runtime: "docker", networkMode: "host", env: []string{"SCION_METADATA_MODE=assign"}, wantErr: true},
		{name: "podman host assign refused", runtime: "podman", networkMode: "host", env: []string{"SCION_METADATA_MODE=assign"}, wantErr: true},
		{name: "docker host block warns", runtime: "docker", networkMode: "host", env: []string{"SCION_METADATA_MODE=block"}, wantWarning: true},
		{name: "docker host contradictory duplicate refused", runtime: "docker", networkMode: "host", env: []string{"SCION_METADATA_MODE=block", "SCION_METADATA_MODE=assign"}, wantErr: true},
		{name: "docker host passthrough", runtime: "docker", networkMode: "host", env: []string{"SCION_METADATA_MODE=passthrough"}},
		{name: "docker host no mode", runtime: "docker", networkMode: "host", env: nil},
		{name: "docker bridge assign", runtime: "docker", networkMode: "", env: []string{"SCION_METADATA_MODE=assign"}},
		{name: "docker bridge block", runtime: "docker", networkMode: "", env: []string{"SCION_METADATA_MODE=block"}},
		{name: "kubernetes host assign", runtime: "kubernetes", networkMode: "host", env: []string{"SCION_METADATA_MODE=assign"}},
		{name: "apple host assign", runtime: "container", networkMode: "host", env: []string{"SCION_METADATA_MODE=assign"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warning, err := checkHostNetworkMetadata("agent-1", tt.runtime, tt.networkMode, tt.env)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				for _, want := range []string{`"agent-1"`, `"assign"`, "host networking", "bridge-reachable hub URL"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				if warning != "" {
					t.Errorf("warning = %q alongside an error, want empty", warning)
				}
			}
			if (warning != "") != tt.wantWarning {
				t.Fatalf("warning = %q, wantWarning %v", warning, tt.wantWarning)
			}
			if tt.wantWarning {
				for _, want := range []string{`"block"`, "not enforced", "host networking", "bridge-reachable hub URL"} {
					if !strings.Contains(warning, want) {
						t.Errorf("warning %q does not contain %q", warning, want)
					}
				}
			}
		})
	}
}

func TestMetadataInterceptionGranted(t *testing.T) {
	tests := []struct {
		name        string
		runtime     string
		networkMode string
		env         []string
		want        bool
	}{
		{name: "docker bridge block", runtime: "docker", env: []string{"SCION_METADATA_MODE=block"}, want: true},
		{name: "docker bridge assign", runtime: "docker", env: []string{"SCION_METADATA_MODE=assign"}, want: true},
		{name: "podman bridge block", runtime: "podman", env: []string{"SCION_METADATA_MODE=block"}, want: true},
		{name: "docker host block", runtime: "docker", networkMode: "host", env: []string{"SCION_METADATA_MODE=block"}, want: false},
		{name: "podman host block", runtime: "podman", networkMode: "host", env: []string{"SCION_METADATA_MODE=block"}, want: false},
		{name: "docker bridge passthrough", runtime: "docker", env: []string{"SCION_METADATA_MODE=passthrough"}, want: false},
		{name: "docker bridge no mode", runtime: "docker", want: false},
		{name: "kubernetes block", runtime: "kubernetes", env: []string{"SCION_METADATA_MODE=block"}, want: false},
		{name: "apple block", runtime: "container", env: []string{"SCION_METADATA_MODE=block"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := metadataInterceptionGranted(tt.runtime, tt.networkMode, tt.env); got != tt.want {
				t.Errorf("metadataInterceptionGranted(%q, %q, %v) = %v, want %v", tt.runtime, tt.networkMode, tt.env, got, tt.want)
			}
		})
	}
}

// startHostNetFixture starts an agent through the real Manager.Start on a
// Docker-named mock runtime with the given hub endpoint and metadata mode,
// and returns the start result and the RunConfig the runtime received (nil
// when Run was never called).
func startHostNetFixture(t *testing.T, hubEndpoint, mode string) (*api.AgentInfo, *runtime.RunConfig, error) {
	t.Helper()
	t.Setenv(runtime.ForceHostNetworkEnvVar, "")
	projectScionDir, _ := startTZFixture(t, "", `""`)

	var got *runtime.RunConfig
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			c := cfg
			got = &c
			return "mock-id", nil
		},
	}
	info, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name:        "tz-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		// A hub-dispatched start carries the token the hub issued for it
		// (made-up value); a start without one fails with ErrNoHubToken.
		HubIssuedToken: "fake.hostnet.hub.token",
		Env: map[string]string{
			"SCION_AGENT_ID":      "agent-uuid-hostnet",
			"SCION_HUB_ENDPOINT":  hubEndpoint,
			"SCION_METADATA_MODE": mode,
		},
	})
	return info, got, err
}

func TestStart_HostNetworkAssignRefusedBeforeRun(t *testing.T) {
	_, cfg, err := startHostNetFixture(t, "http://localhost:8080", "assign")
	if err == nil {
		t.Fatal("Start succeeded, want a refusal for assign under host networking")
	}
	if !strings.Contains(err.Error(), "host networking") || !strings.Contains(err.Error(), `"assign"`) {
		t.Errorf("error = %q, want it to name assign and host networking", err)
	}
	if cfg != nil {
		t.Error("the runtime's Run was called; a refused start must not create a container")
	}
}

func TestStart_HostNetworkBlockWarnsAndRelays(t *testing.T) {
	stubHostNetworkBlockPolicy(t, "", runtime.CloudHost{})
	info, cfg, err := startHostNetFixture(t, "http://localhost:8080", "block")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if cfg == nil {
		t.Fatal("the runtime's Run was never called")
	}
	if cfg.NetworkMode != "host" {
		t.Fatalf("NetworkMode = %q, want host (fixture precondition)", cfg.NetworkMode)
	}
	if cfg.MetadataInterception {
		t.Error("MetadataInterception = true under host networking, want false (no NET_ADMIN)")
	}
	if !slices.Contains(info.RelayedWarnings, hostNetworkMetadataWarning) {
		t.Errorf("RelayedWarnings = %v, want the host networking warning", info.RelayedWarnings)
	}
	if !slices.Contains(info.Warnings, hostNetworkMetadataWarning) {
		t.Errorf("Warnings = %v, want the host networking warning", info.Warnings)
	}
}

func TestStart_BridgeBlockGrantsInterception(t *testing.T) {
	info, cfg, err := startHostNetFixture(t, "https://hub.example.com", "block")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if cfg == nil {
		t.Fatal("the runtime's Run was never called")
	}
	if cfg.NetworkMode != "" {
		t.Fatalf("NetworkMode = %q, want bridge (fixture precondition)", cfg.NetworkMode)
	}
	if !cfg.MetadataInterception {
		t.Error("MetadataInterception = false for block on bridge, want true")
	}
	// The fixture is a hub dispatch with a hub-issued token, so on bridge the
	// start relays no warning at all (no host networking warning).
	if len(info.RelayedWarnings) != 0 {
		t.Errorf("RelayedWarnings = %v, want none on bridge with a hub-issued token", info.RelayedWarnings)
	}
	if slices.Contains(info.RelayedWarnings, hostNetworkMetadataWarning) {
		t.Errorf("RelayedWarnings = %v, want no host networking warning on bridge", info.RelayedWarnings)
	}
}
