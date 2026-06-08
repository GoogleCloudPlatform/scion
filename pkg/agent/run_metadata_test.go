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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestColocatedDockerNetworkComposition mirrors how run.go assembles a
// container's RunConfig.NetworkMode and ExtraHosts (run.go:672, 891-892) for a
// colocated Docker agent. The broker supplies the public-domain host-gateway
// mapping via opts.ExtraHosts (from colocatedExtraHosts); the agent path
// derives NetworkMode from ResolveDockerNetworking and merges BridgeExtraHosts.
func TestColocatedDockerNetworkComposition(t *testing.T) {
	const domainHostGateway = "hub.example.com:host-gateway"

	tests := []struct {
		name           string
		forceHost      bool
		hubEndpoint    string
		brokerExtra    []string // opts.ExtraHosts supplied by the broker
		wantNetMode    string
		wantExtraHosts []string
	}{
		{
			name:           "colocated docker domain uses bridge with host-gateway",
			hubEndpoint:    "https://hub.example.com",
			brokerExtra:    []string{domainHostGateway},
			wantNetMode:    "",
			wantExtraHosts: []string{domainHostGateway},
		},
		{
			name:           "force-host falls back to host networking",
			forceHost:      true,
			hubEndpoint:    "https://hub.example.com",
			brokerExtra:    []string{domainHostGateway},
			wantNetMode:    "host",
			wantExtraHosts: []string{domainHostGateway},
		},
		{
			// Legacy fallback: ResolveDockerNetworking rewrites the bridge
			// hostname back to localhost (reachable under host networking), so
			// by the time agentEnv is built no host-gateway add-host is needed.
			name:           "host.docker.internal fallback uses host networking",
			hubEndpoint:    "http://host.docker.internal:8080",
			brokerExtra:    nil,
			wantNetMode:    "host",
			wantExtraHosts: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.forceHost {
				t.Setenv(runtime.ForceHostNetworkEnvVar, "1")
			}
			env := map[string]string{"SCION_HUB_ENDPOINT": tt.hubEndpoint}
			gotMode := runtime.ResolveDockerNetworking("docker", env)
			if gotMode != tt.wantNetMode {
				t.Errorf("NetworkMode = %q, want %q", gotMode, tt.wantNetMode)
			}

			// Mirror run.go: agentEnv is built from opts.Env after the rewrite.
			agentEnv := []string{"SCION_HUB_ENDPOINT=" + env["SCION_HUB_ENDPOINT"]}
			gotExtra := mergeExtraHosts(tt.brokerExtra, runtime.BridgeExtraHosts("docker", agentEnv))
			if len(gotExtra) != len(tt.wantExtraHosts) {
				t.Fatalf("ExtraHosts = %v, want %v", gotExtra, tt.wantExtraHosts)
			}
			for i := range gotExtra {
				if gotExtra[i] != tt.wantExtraHosts[i] {
					t.Errorf("ExtraHosts[%d] = %q, want %q", i, gotExtra[i], tt.wantExtraHosts[i])
				}
			}
		})
	}
}

func TestMergeExtraHosts(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want []string
	}{
		{name: "both nil", a: nil, b: nil, want: nil},
		{name: "a only", a: []string{"foo:1.2.3.4"}, b: nil, want: []string{"foo:1.2.3.4"}},
		{name: "b only", a: nil, b: []string{"bar:host-gateway"}, want: []string{"bar:host-gateway"}},
		{
			name: "no overlap",
			a:    []string{"foo:1.2.3.4"},
			b:    []string{"bar:host-gateway"},
			want: []string{"foo:1.2.3.4", "bar:host-gateway"},
		},
		{
			name: "a takes precedence on overlap",
			a:    []string{"hub.example.com:host-gateway"},
			b:    []string{"hub.example.com:172.17.0.1", "other:host-gateway"},
			want: []string{"hub.example.com:host-gateway", "other:host-gateway"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeExtraHosts(tt.a, tt.b)
			if len(got) != len(tt.want) {
				t.Fatalf("mergeExtraHosts() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("mergeExtraHosts()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestHasMetadataInterception(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want bool
	}{
		{
			name: "assign mode",
			env:  []string{"FOO=bar", "SCION_METADATA_MODE=assign", "BAZ=qux"},
			want: true,
		},
		{
			name: "block mode",
			env:  []string{"SCION_METADATA_MODE=block"},
			want: true,
		},
		{
			name: "passthrough mode",
			env:  []string{"SCION_METADATA_MODE=passthrough"},
			want: false,
		},
		{
			name: "no metadata mode",
			env:  []string{"FOO=bar", "BAZ=qux"},
			want: false,
		},
		{
			name: "empty env",
			env:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasMetadataInterception(tt.env)
			if got != tt.want {
				t.Errorf("hasMetadataInterception(%v) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}
