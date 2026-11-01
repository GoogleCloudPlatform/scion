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
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestCheckAgentHubTransport(t *testing.T) {
	hostGateway := []string{"scion-hub.internal:host-gateway"}
	tests := []struct {
		name       string
		runtime    string
		extraHosts []string
		env        []string
		wantErr    bool
	}{
		// Accepted on every runtime.
		{name: "https on kubernetes", runtime: "kubernetes", env: []string{"SCION_HUB_ENDPOINT=https://hub.scion.svc:8443"}},
		{name: "wss on substrate", runtime: "substrate", env: []string{"SCION_HUB_ENDPOINT=wss://hub.example.com"}},
		{name: "https on unknown runtime", runtime: "mock", env: []string{"SCION_HUB_ENDPOINT=https://hub.example.com"}},
		{name: "loopback on kubernetes", runtime: "kubernetes", env: []string{"SCION_HUB_ENDPOINT=http://127.0.0.1:9810"}},
		{name: "localhost on docker", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://localhost:8080"}},
		{name: "ipv6 loopback on cloudrun", runtime: "cloudrun", env: []string{"SCION_HUB_ENDPOINT=http://[::1]:8080"}},
		{name: "no hub endpoint", runtime: "kubernetes", env: []string{"SCION_AGENT_ID=a"}},
		{name: "empty hub endpoint", runtime: "kubernetes", env: []string{"SCION_HUB_ENDPOINT="}},

		// Host-local aliases on local runtimes.
		{name: "docker host.docker.internal mapped", runtime: "docker", extraHosts: []string{"host.docker.internal:host-gateway"},
			env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080", "SCION_HUB_URL=http://host.docker.internal:8080"}},
		{name: "docker alias mixed case mapped", runtime: "docker", extraHosts: []string{"host.docker.internal:host-gateway"},
			env: []string{"SCION_HUB_ENDPOINT=http://Host.Docker.Internal:8080"}},
		{name: "docker alias mapped with mixed-case entry", runtime: "docker", extraHosts: []string{"Host.Docker.Internal:Host-Gateway"},
			env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"}},
		{name: "docker scion-hub.internal mapped", runtime: "docker", extraHosts: hostGateway, env: []string{"SCION_HUB_ENDPOINT=http://scion-hub.internal:8080"}},
		{name: "podman host.containers.internal", runtime: "podman", env: []string{"SCION_HUB_ENDPOINT=http://host.containers.internal:8080"}},
		{name: "apple host.containers.internal", runtime: "container", env: []string{"SCION_HUB_ENDPOINT=http://host.containers.internal:8080"}},
		{name: "cloudrun-sandbox link-local", runtime: "cloudrun-sandbox", env: []string{"SCION_HUB_ENDPOINT=http://169.254.1.2:8080"}},
		{name: "docker ws alias", runtime: "docker", extraHosts: []string{"host.docker.internal:host-gateway"},
			env: []string{"SCION_HUB_ENDPOINT=ws://host.docker.internal:8080"}},

		// Refused: network-exposed runtimes.
		{name: "kubernetes in-cluster http", runtime: "kubernetes", env: []string{"SCION_HUB_ENDPOINT=http://scion-hub.scion.svc.cluster.local:8080"}, wantErr: true},
		{name: "k8s in-cluster http", runtime: "k8s", env: []string{"SCION_HUB_ENDPOINT=http://scion-hub:8080"}, wantErr: true},
		{name: "remote in-cluster http", runtime: "remote", env: []string{"SCION_HUB_ENDPOINT=http://10.4.0.12:8080"}, wantErr: true},
		{name: "substrate in-cluster http", runtime: "substrate", env: []string{"SCION_HUB_ENDPOINT=http://hub.scion-system.svc.cluster.local"}, wantErr: true},
		{name: "cloudrun http", runtime: "cloudrun", env: []string{"SCION_HUB_ENDPOINT=http://hub.example.com"}, wantErr: true},
		{name: "kubernetes alias", runtime: "kubernetes", env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"}, wantErr: true},
		{name: "kubernetes link-local", runtime: "kubernetes", env: []string{"SCION_HUB_ENDPOINT=http://169.254.1.2:8080"}, wantErr: true},
		{name: "unknown runtime alias", runtime: "mock", env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"}, wantErr: true},
		{name: "empty runtime alias", runtime: "", env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"}, wantErr: true},

		// Refused: local runtimes, not an alias.
		{name: "docker LAN IP", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://10.0.0.5:8080"}, wantErr: true},
		{name: "docker docker0 IP", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://172.17.0.1:8080"}, wantErr: true},
		{name: "docker LAN name", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://workstation.lan:8080"}, wantErr: true},
		{name: "docker IP mapped to host-gateway", runtime: "docker", extraHosts: []string{"10.0.0.5:host-gateway"}, env: []string{"SCION_HUB_ENDPOINT=http://10.0.0.5:8080"}, wantErr: true},
		{name: "docker domain mapped to host-gateway", runtime: "docker", extraHosts: []string{"hub.example.com:host-gateway"}, env: []string{"SCION_HUB_ENDPOINT=http://hub.example.com"}, wantErr: true},
		{name: "docker link-local", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://169.254.1.2:8080"}, wantErr: true},
		{name: "docker host.docker.internal unmapped", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"}, wantErr: true},
		{name: "docker alias mixed case unmapped", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://Host.Docker.Internal:8080"}, wantErr: true},
		{name: "docker alias trailing dot mapped", runtime: "docker", extraHosts: []string{"host.docker.internal:host-gateway"},
			env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal.:8080"}, wantErr: true},
		{name: "docker alias trailing dot with trailing-dot entry", runtime: "docker", extraHosts: []string{"host.docker.internal.:host-gateway"},
			env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal.:8080"}, wantErr: true},
		{name: "podman alias trailing dot", runtime: "podman", env: []string{"SCION_HUB_ENDPOINT=http://host.containers.internal.:8080"}, wantErr: true},
		{name: "apple alias trailing dot", runtime: "container", env: []string{"SCION_HUB_ENDPOINT=http://host.containers.internal.:8080"}, wantErr: true},
		{name: "docker scion-hub.internal unmapped", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://scion-hub.internal:8080"}, wantErr: true},
		{name: "docker scion-hub.internal mapped to IP", runtime: "docker", extraHosts: []string{"scion-hub.internal:10.0.0.5"}, env: []string{"SCION_HUB_ENDPOINT=http://scion-hub.internal:8080"}, wantErr: true},
		{name: "docker alias mapped to IP", runtime: "docker", extraHosts: []string{"host.docker.internal:10.0.0.5"}, env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"}, wantErr: true},
		{name: "docker podman alias", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://host.containers.internal:8080"}, wantErr: true},
		{name: "podman docker alias", runtime: "podman", env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"}, wantErr: true},
		{name: "podman scion-hub.internal", runtime: "podman", extraHosts: hostGateway, env: []string{"SCION_HUB_ENDPOINT=http://scion-hub.internal:8080"}, wantErr: true},
		{name: "apple scion-hub.internal", runtime: "container", extraHosts: hostGateway, env: []string{"SCION_HUB_ENDPOINT=http://scion-hub.internal:8080"}, wantErr: true},
		{name: "apple IP", runtime: "container", env: []string{"SCION_HUB_ENDPOINT=http://192.168.64.1:8080"}, wantErr: true},
		{name: "alias suffix trick", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal.example.com:8080"}, wantErr: true},
		{name: "alias prefix trick", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://evil.host.docker.internal:8080"}, wantErr: true},
		{name: "cloudrun-sandbox non-link-local", runtime: "cloudrun-sandbox", env: []string{"SCION_HUB_ENDPOINT=http://10.1.2.3:8080"}, wantErr: true},
		{name: "cloudrun-sandbox alias", runtime: "cloudrun-sandbox", env: []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"}, wantErr: true},
		{name: "cloudrun-sandbox ipv6 link-local", runtime: "cloudrun-sandbox", env: []string{"SCION_HUB_ENDPOINT=http://[fe80::1]:8080"}, wantErr: true},

		// Refused: scheme and shape.
		{name: "ftp alias", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=ftp://host.docker.internal:8080"}, wantErr: true},
		{name: "no scheme", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=host.docker.internal:8080"}, wantErr: true},
		{name: "unparsable", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http://[::1"}, wantErr: true},
		{name: "no host", runtime: "docker", env: []string{"SCION_HUB_ENDPOINT=http:///path"}, wantErr: true},

		// Both keys are checked.
		{name: "SCION_HUB_URL bad, ENDPOINT good", runtime: "kubernetes",
			env: []string{"SCION_HUB_ENDPOINT=https://hub.example.com", "SCION_HUB_URL=http://scion-hub:8080"}, wantErr: true},
		{name: "SCION_HUB_ENDPOINT bad, URL good", runtime: "kubernetes",
			env: []string{"SCION_HUB_ENDPOINT=http://scion-hub:8080", "SCION_HUB_URL=https://hub.example.com"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkAgentHubTransport("a1", tt.runtime, tt.extraHosts, tt.env)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("checkAgentHubTransport: %v, want accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checkAgentHubTransport accepted the endpoint, want a refusal")
			}
			if !errors.Is(err, ErrPlaintextAgentHubEndpoint) || !errors.Is(err, config.ErrPlaintextNonLoopback) {
				t.Errorf("error %q does not wrap both ErrPlaintextAgentHubEndpoint and config.ErrPlaintextNonLoopback", err)
			}
			if !strings.Contains(err.Error(), `"`+tt.runtime+`" runtime`) {
				t.Errorf("error %q does not name the runtime", err)
			}
			if !strings.Contains(err.Error(), agentHubTransportFix) {
				t.Errorf("error %q does not name the remedy", err)
			}
		})
	}
}

func TestCheckAgentHubTransportRedactsCredentials(t *testing.T) {
	for _, raw := range []string{
		"http://user:s3cr3t-pass@scion-hub:8080",
		"http://user:s3cr3t-pass@[::1",
	} {
		err := checkAgentHubTransport("a1", "kubernetes", nil, []string{"SCION_HUB_ENDPOINT=" + raw})
		if err == nil {
			t.Fatalf("%q accepted, want a refusal", raw)
		}
		if strings.Contains(err.Error(), "s3cr3t-pass") {
			t.Errorf("refusal for a URL with userinfo shows the password: %q", err)
		}
		if !strings.Contains(err.Error(), "SCION_HUB_ENDPOINT") {
			t.Errorf("refusal %q does not name the env key", err)
		}
	}
}

// TestAppleAliasesNeedNoMapping pins that no Apple container alias requires
// a host-gateway mapping: Apple drops --add-host, so such an alias could
// never be accepted.
func TestAppleAliasesNeedNoMapping(t *testing.T) {
	for _, a := range hostLocalAliasesByRuntime["container"] {
		if a.requiresMapping {
			t.Errorf("Apple container alias %q requires a mapping", a.name)
		}
	}
}

// TestCheckAgentHubTransportPlaintextMessage pins the refusal text for a
// plaintext endpoint: it names the redacted URL, the scheme and the host,
// and gives one remedy.
func TestCheckAgentHubTransportPlaintextMessage(t *testing.T) {
	err := checkAgentHubTransport("a1", "kubernetes", nil, []string{"SCION_HUB_URL=http://scion-hub:8080"})
	if err == nil {
		t.Fatal("accepted, want a refusal")
	}
	msg := err.Error()
	for _, want := range []string{`cannot start agent "a1"`, "SCION_HUB_URL", "http://scion-hub:8080", `uses http to host "scion-hub"`, "neither a loopback address nor a host alias", agentHubTransportFix} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "SSH port forward") {
		t.Errorf("refusal %q carries the broker-channel remedy text", msg)
	}
}

// TestHostLocalAliasesAreAgentHostAliases pins that every alias this policy
// accepts is also a config.IsAgentHostAliasHost name, so the agent-side CLI
// exemption never refuses an endpoint the broker gave the agent.
func TestHostLocalAliasesAreAgentHostAliases(t *testing.T) {
	for rt, aliases := range hostLocalAliasesByRuntime {
		for _, a := range aliases {
			if !config.IsAgentHostAliasHost(a.name) {
				t.Errorf("runtime %q alias %q is not a config.IsAgentHostAliasHost name", rt, a.name)
			}
		}
	}
}
