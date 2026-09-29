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

package runtime

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestSubstrateEgressHostnames_TelemetryHost(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "SCION_OTEL_ENDPOINT",
			env:  map[string]string{"SCION_OTEL_ENDPOINT": "otel-collector.example.com:4317"},
			want: "otel-collector.example.com",
		},
		{
			name: "OTEL_EXPORTER_OTLP_ENDPOINT",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.com:4318"},
			want: "otlp.example.com",
		},
		{
			name: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://traces.example.com:4318/v1/traces"},
			want: "traces.example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hosts := substrateEgressHostnames(RunConfig{}, tc.env, config.V1SubstrateConfig{})
			if !containsHost(hosts, tc.want) {
				t.Errorf("substrateEgressHostnames() = %v, want it to contain %q (from %s)", hosts, tc.want, tc.name)
			}
		})
	}
}

func TestSubstrateEgressHostnames_TelemetryHostsAreIndependent(t *testing.T) {
	// A deployment could point different OTel signals at different
	// collectors; both must survive, not just the first one found.
	env := map[string]string{
		"SCION_OTEL_ENDPOINT":         "cloud-otel.example.com:4317",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "self-hosted-otel.example.com:4318",
	}
	hosts := substrateEgressHostnames(RunConfig{}, env, config.V1SubstrateConfig{})
	if !containsHost(hosts, "cloud-otel.example.com") {
		t.Errorf("substrateEgressHostnames() = %v, missing SCION_OTEL_ENDPOINT host", hosts)
	}
	if !containsHost(hosts, "self-hosted-otel.example.com") {
		t.Errorf("substrateEgressHostnames() = %v, missing OTEL_EXPORTER_OTLP_ENDPOINT host", hosts)
	}
}

func TestSubstrateEgressHostnames_NoTelemetryEnv(t *testing.T) {
	hosts := substrateEgressHostnames(RunConfig{}, map[string]string{}, config.V1SubstrateConfig{})
	// The hardcoded model hosts (including *.googleapis.com, which happens
	// to cover the Cloud Trace default) are always present regardless.
	if !containsHost(hosts, "*.googleapis.com") {
		t.Errorf("substrateEgressHostnames() = %v, want the hardcoded model hosts present even with no telemetry env", hosts)
	}
}

// TestSubstrateEgressHostnames_SendsNormalizedEgressAllowEntries confirms
// "validate what you send": ValidateEgressAllow accepts egress_allow
// entries after normalizing them (lowercase, at most one trailing dot), but
// Substrate's own HostnameRule requires exactly that normalized form
// (lowercase, no trailing dot). Sending the merely-trimmed raw entry
// instead would validate fine locally and then fail at the Substrate API.
func TestSubstrateEgressHostnames_SendsNormalizedEgressAllowEntries(t *testing.T) {
	sc := config.V1SubstrateConfig{EgressAllow: []string{"GitHub.COM.", "  Registry.NPMJS.org  "}}
	hosts := substrateEgressHostnames(RunConfig{}, map[string]string{}, sc)

	if containsHost(hosts, "GitHub.COM.") || containsHost(hosts, "  Registry.NPMJS.org  ") {
		t.Errorf("substrateEgressHostnames() = %v, sent an unnormalized egress_allow entry", hosts)
	}
	if !containsHost(hosts, "github.com") {
		t.Errorf("substrateEgressHostnames() = %v, want the normalized form \"github.com\"", hosts)
	}
	if !containsHost(hosts, "registry.npmjs.org") {
		t.Errorf("substrateEgressHostnames() = %v, want the normalized form \"registry.npmjs.org\"", hosts)
	}
}

// TestSubstrateEgressHostnames_RejectsIPShapedEgressAllow confirms no IP or
// CIDR egress_allow entry ever reaches substrateEgressHostnames' output,
// because substrate.NormalizeEgressAllowEntry (called for every
// sc.EgressAllow entry) rejects all of them outright. See
// TestBuildEgressPolicy_PatternsMatchInputVerbatim for the other half —
// that buildEgressPolicy sends exactly this list, unmodified, so nothing
// IP-shaped can be reintroduced downstream either.
func TestSubstrateEgressHostnames_RejectsIPShapedEgressAllow(t *testing.T) {
	sc := config.V1SubstrateConfig{EgressAllow: []string{
		"8.8.8.8",
		"10.0.0.0/8",
		"2001:4860:4860::8888",
		"api.example.com",
	}}
	hosts := substrateEgressHostnames(RunConfig{}, map[string]string{}, sc)

	for _, h := range hosts {
		if _, err := netip.ParseAddr(h); err == nil {
			t.Errorf("substrateEgressHostnames() = %v, contains an IP address %q", hosts, h)
		}
		if strings.Contains(h, "/") {
			t.Errorf("substrateEgressHostnames() = %v, contains a CIDR-shaped entry %q", hosts, h)
		}
	}
	if containsHost(hosts, "8.8.8.8") || containsHost(hosts, "10.0.0.0/8") || containsHost(hosts, "2001:4860:4860::8888") {
		t.Errorf("substrateEgressHostnames() = %v, an IP/CIDR egress_allow entry reached the pattern list", hosts)
	}
	if !containsHost(hosts, "api.example.com") {
		t.Errorf("substrateEgressHostnames() = %v, want the valid hostname entry present", hosts)
	}
}

// TestBuildEgressPolicy_PatternsMatchInputVerbatim confirms every string in
// hostnames lands in HostnameRule.patterns, in order, unmodified — no
// re-normalization, no mangling, and (paired with
// TestSubstrateEgressHostnames_RejectsIPShapedEgressAllow, which keeps
// IP/CIDR entries out of that input list in the first place) nothing
// IP-shaped ever reaches Patterns. Also confirms CIDRRule is never set:
// everything goes through HostnameRule.patterns or not at all.
func TestBuildEgressPolicy_PatternsMatchInputVerbatim(t *testing.T) {
	hostnames := []string{"api.example.com", "*.github.com", "registry.npmjs.org"}
	req := buildEgressPolicy("scion-proj", "agent-a", hostnames)

	rules := req.GetEgressPolicy().GetRules()
	if len(rules) != 1 {
		t.Fatalf("buildEgressPolicy() = %d rules, want 1", len(rules))
	}
	if rules[0].GetCidrs() != nil {
		t.Errorf("buildEgressPolicy() set a CIDRRule; it never sends one")
	}
	patterns := rules[0].GetHostnames().GetPatterns()
	if len(patterns) != len(hostnames) {
		t.Fatalf("buildEgressPolicy() patterns = %v, want exactly %v", patterns, hostnames)
	}
	for i, want := range hostnames {
		if patterns[i] != want {
			t.Errorf("buildEgressPolicy() pattern[%d] = %q, want %q (verbatim passthrough)", i, patterns[i], want)
		}
	}
}

// TestSubstrateEgressPolicy_EndToEndNoIPPatterns wires
// substrateEgressHostnames straight into buildEgressPolicy, the same way
// Run does, and confirms end to end that an operator's IP/CIDR
// egress_allow entries never appear in the actor's EgressPolicy, while a
// hostname entry does — normalized.
func TestSubstrateEgressPolicy_EndToEndNoIPPatterns(t *testing.T) {
	sc := config.V1SubstrateConfig{EgressAllow: []string{
		"8.8.8.8",
		"10.0.0.0/8",
		"GitHub.COM.",
	}}
	hosts := substrateEgressHostnames(RunConfig{}, map[string]string{}, sc)
	req := buildEgressPolicy("scion-proj", "agent-a", hosts)
	patterns := req.GetEgressPolicy().GetRules()[0].GetHostnames().GetPatterns()

	if containsHost(patterns, "8.8.8.8") || containsHost(patterns, "10.0.0.0/8") {
		t.Errorf("HostnameRule.patterns = %v, an IP/CIDR egress_allow entry reached it", patterns)
	}
	if !containsHost(patterns, "github.com") {
		t.Errorf("HostnameRule.patterns = %v, want the normalized form of GitHub.COM.", patterns)
	}
}

func containsHost(hosts []string, want string) bool {
	for _, h := range hosts {
		if h == want {
			return true
		}
	}
	return false
}
