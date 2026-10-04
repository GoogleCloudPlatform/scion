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
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
			// A tenant-derived host is only ever added when an operator
			// egress_allow entry covers it; cover the exact host under test
			// so this test still exercises whichever env var derives it,
			// not the coverage gate itself (see
			// TestSubstrateEgressHostnames_TenantHostRequiresEgressAllowCoverage
			// for that).
			sc := config.V1SubstrateConfig{EgressAllow: []string{tc.want}}
			hosts, hostsErr := substrateEgressHostnames(RunConfig{}, tc.env, sc)
			if hostsErr != nil {
				t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
			}
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
	sc := config.V1SubstrateConfig{EgressAllow: []string{"cloud-otel.example.com", "self-hosted-otel.example.com"}}
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, sc)
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	if !containsHost(hosts, "cloud-otel.example.com") {
		t.Errorf("substrateEgressHostnames() = %v, missing SCION_OTEL_ENDPOINT host", hosts)
	}
	if !containsHost(hosts, "self-hosted-otel.example.com") {
		t.Errorf("substrateEgressHostnames() = %v, missing OTEL_EXPORTER_OTLP_ENDPOINT host", hosts)
	}
}

func TestSubstrateEgressHostnames_NoTelemetryEnv(t *testing.T) {
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, map[string]string{}, config.V1SubstrateConfig{})
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	// The hardcoded model hosts are always present regardless of telemetry
	// env.
	if !containsHost(hosts, "oauth2.googleapis.com") {
		t.Errorf("substrateEgressHostnames() = %v, want the hardcoded model hosts present even with no telemetry env", hosts)
	}
}

// TestSubstrateEgressHostnames_NoWildcardGoogleapis proves the runtime no
// longer grants blanket access to every Google API: a "*.googleapis.com"
// entry would admit GCS, Compute, BigQuery, and hundreds of other services
// this runtime has no business reaching, not just the oauth2/Vertex hosts
// it actually uses.
func TestSubstrateEgressHostnames_NoWildcardGoogleapis(t *testing.T) {
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, map[string]string{}, config.V1SubstrateConfig{})
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	if containsHost(hosts, "*.googleapis.com") {
		t.Errorf("substrateEgressHostnames() = %v, must not contain the *.googleapis.com wildcard", hosts)
	}
}

// TestSubstrateEgressHostnames_VertexAIHost proves the Vertex AI endpoint
// is derived from the agent's own configured region, added only when
// Vertex auth is actually in play — never a blanket grant to every Google
// API, and never present at all for a plain (non-Vertex) Anthropic API
// agent.
func TestSubstrateEgressHostnames_VertexAIHost(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "CLAUDE_CODE_USE_VERTEX with region",
			env:  map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "CLOUD_ML_REGION": "us-east5"},
			want: "us-east5-aiplatform.googleapis.com",
		},
		{
			name: "ANTHROPIC_VERTEX_PROJECT_ID with region",
			env:  map[string]string{"ANTHROPIC_VERTEX_PROJECT_ID": "my-project", "CLOUD_ML_REGION": "us-central1"},
			want: "us-central1-aiplatform.googleapis.com",
		},
		{
			name: "Vertex indicated with no region set",
			env:  map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"},
			want: "aiplatform.googleapis.com",
		},
		{
			name: "explicit global region",
			env:  map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "CLOUD_ML_REGION": "global"},
			want: "aiplatform.googleapis.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hosts, hostsErr := substrateEgressHostnames(RunConfig{}, tc.env, config.V1SubstrateConfig{})
			if hostsErr != nil {
				t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
			}
			if !containsHost(hosts, tc.want) {
				t.Errorf("substrateEgressHostnames() = %v, want %q present", hosts, tc.want)
			}
		})
	}
}

// TestSubstrateEgressHostnames_NoVertexHostWithoutVertexAuth proves a
// plain (non-Vertex, direct Anthropic API) agent gets no aiplatform host
// at all — not even the bare global one.
func TestSubstrateEgressHostnames_NoVertexHostWithoutVertexAuth(t *testing.T) {
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, map[string]string{}, config.V1SubstrateConfig{})
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	for _, h := range hosts {
		if strings.Contains(h, "aiplatform") {
			t.Errorf("substrateEgressHostnames() = %v, must not contain an aiplatform host without Vertex auth indicated", hosts)
		}
	}
}

// TestSubstrateEgressHostnames_InvalidVertexRegionRefused is the
// security-load-bearing proof for addVertexAIHost's region validation: an
// invalid CLOUD_ML_REGION must fail Run closed — never silently construct
// a malformed or wildcard-shaped host, and never silently skip the Vertex
// host while admitting the rest of the egress policy. "*" is the
// specifically dangerous case: fed unchecked into
// "<region>-aiplatform.googleapis.com", it would construct
// "*-aiplatform.googleapis.com", a wildcard host — exactly the class of
// grant item 4 exists to remove.
func TestSubstrateEgressHostnames_InvalidVertexRegionRefused(t *testing.T) {
	cases := []struct {
		name   string
		region string
	}{
		{name: "wildcard", region: "*"},
		{name: "embedded dot", region: "a.b"},
		{name: "uppercase", region: "US-EAST1"},
		{name: "leading hyphen", region: "-us"},
		{name: "trailing hyphen", region: "us-"},
		{name: "empty trailing label", region: "us."},
		{name: "empty leading label", region: ".us"},
		{name: "empty inner label", region: "us..east1"},
		{name: "label over 63 chars", region: strings.Repeat("a", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "CLOUD_ML_REGION": tc.region}
			hosts, err := substrateEgressHostnames(RunConfig{}, env, config.V1SubstrateConfig{})
			if err == nil {
				t.Fatalf("substrateEgressHostnames() with CLOUD_ML_REGION=%q: expected an error, got hosts = %v", tc.region, hosts)
			}
			for _, h := range hosts {
				if strings.Contains(h, "*") {
					t.Errorf("a wildcard host %q leaked into the result despite the error", h)
				}
			}
		})
	}
}

// TestSubstrateEgressHostnames_VertexBaseURLOverrideGoesThroughCoverageGate
// proves ANTHROPIC_VERTEX_BASE_URL — a model base-URL override an
// agent/template can set outright, unlike the fixed-suffix host
// addVertexAIHost constructs — is treated as a tenant host: admitted only
// when an operator egress_allow entry covers it, dropped otherwise.
func TestSubstrateEgressHostnames_VertexBaseURLOverrideGoesThroughCoverageGate(t *testing.T) {
	env := map[string]string{"ANTHROPIC_VERTEX_BASE_URL": "https://attacker-controlled.example.com/v1"}

	t.Run("uncovered, dropped", func(t *testing.T) {
		hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, config.V1SubstrateConfig{})
		if hostsErr != nil {
			t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
		}
		if containsHost(hosts, "attacker-controlled.example.com") {
			t.Errorf("substrateEgressHostnames() = %v, want the uncovered ANTHROPIC_VERTEX_BASE_URL host dropped", hosts)
		}
	})

	t.Run("covered, added", func(t *testing.T) {
		sc := config.V1SubstrateConfig{EgressAllow: []string{"attacker-controlled.example.com"}}
		hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, sc)
		if hostsErr != nil {
			t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
		}
		if !containsHost(hosts, "attacker-controlled.example.com") {
			t.Errorf("substrateEgressHostnames() = %v, want the operator-covered ANTHROPIC_VERTEX_BASE_URL host present", hosts)
		}
	})
}

// TestSubstrateEgressHostnames_SendsNormalizedEgressAllowEntries confirms
// "validate what you send": ValidateEgressAllow accepts egress_allow
// entries after normalizing them (lowercase, at most one trailing dot), but
// Substrate's own HostnameRule requires exactly that normalized form
// (lowercase, no trailing dot). Sending the merely-trimmed raw entry
// instead would validate fine locally and then fail at the Substrate API.
func TestSubstrateEgressHostnames_SendsNormalizedEgressAllowEntries(t *testing.T) {
	sc := config.V1SubstrateConfig{EgressAllow: []string{"GitHub.COM.", "  Registry.NPMJS.org  "}}
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, map[string]string{}, sc)
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}

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
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, map[string]string{}, sc)
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}

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
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, map[string]string{}, sc)
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	req := buildEgressPolicy("scion-proj", "agent-a", hosts)
	patterns := req.GetEgressPolicy().GetRules()[0].GetHostnames().GetPatterns()

	if containsHost(patterns, "8.8.8.8") || containsHost(patterns, "10.0.0.0/8") {
		t.Errorf("HostnameRule.patterns = %v, an IP/CIDR egress_allow entry reached it", patterns)
	}
	if !containsHost(patterns, "github.com") {
		t.Errorf("HostnameRule.patterns = %v, want the normalized form of GitHub.COM.", patterns)
	}
}

// TestSubstrateEgressHostnames_TenantSourcesDropInvalidHosts proves the
// cloud metadata IP, a Kubernetes-API-shaped in-cluster host, and a bare
// in-cluster suffix — each derived from a tenant-controllable source
// (GitClone.URL or an OTEL endpoint env var) — never reach the actor's
// EgressPolicy.
// TestSubstrateEgressHostnames_ScpStyleGitURLSilentlyDropped proves an
// scp-style git remote ("git@host:org/repo.git", no "://") — which
// url.Parse does not accept as a URL at all, so hostFromURL returns "" —
// fails closed: no host is added for it, specifically never the literal
// "git@host:org/repo.git" string or any fragment of it, and no other
// tenant-derived host added by the same call leaks it either.
func TestSubstrateEgressHostnames_ScpStyleGitURLSilentlyDropped(t *testing.T) {
	cfg := RunConfig{GitClone: &api.GitCloneConfig{URL: "git@github.com:acme/repo.git"}}
	hosts, hostsErr := substrateEgressHostnames(cfg, map[string]string{}, config.V1SubstrateConfig{})
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	if containsHost(hosts, "github.com") {
		t.Errorf("substrateEgressHostnames() = %v, must not extract a host from an scp-style git URL", hosts)
	}
	for _, h := range hosts {
		if strings.Contains(h, "git@") || strings.Contains(h, "repo.git") {
			t.Errorf("substrateEgressHostnames() = %v, leaked the raw scp-style URL", hosts)
		}
	}
}

// TestSubstrateEgressHostnames_TenantHostCannotWidenToArbitraryDomain
// proves the coverage gate's actual purpose directly: a tenant-controlled
// host (git clone, telemetry) that happens to be a well-formed public
// hostname — not an IP, not an in-cluster suffix — is still refused unless
// an operator's own egress_allow entry covers it. "*.example.com" in
// egress_allow covers "ci.example.com" but never authorizes an unrelated
// tenant domain the operator never listed.
func TestSubstrateEgressHostnames_TenantHostCannotWidenToArbitraryDomain(t *testing.T) {
	sc := config.V1SubstrateConfig{EgressAllow: []string{"*.example.com"}}

	t.Run("covered subdomain is added", func(t *testing.T) {
		cfg := RunConfig{GitClone: &api.GitCloneConfig{URL: "https://ci.example.com/acme/repo.git"}}
		hosts, hostsErr := substrateEgressHostnames(cfg, map[string]string{}, sc)
		if hostsErr != nil {
			t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
		}
		if !containsHost(hosts, "ci.example.com") {
			t.Errorf("substrateEgressHostnames() = %v, want ci.example.com covered by *.example.com", hosts)
		}
	})

	t.Run("unrelated tenant domain is refused despite being well-formed", func(t *testing.T) {
		cfg := RunConfig{GitClone: &api.GitCloneConfig{URL: "https://attacker-controlled.net/acme/repo.git"}}
		hosts, hostsErr := substrateEgressHostnames(cfg, map[string]string{}, sc)
		if hostsErr != nil {
			t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
		}
		if containsHost(hosts, "attacker-controlled.net") {
			t.Errorf("substrateEgressHostnames() = %v, a tenant-controlled public hostname must not widen egress beyond what egress_allow covers", hosts)
		}
	})
}

func TestSubstrateEgressHostnames_TenantSourcesDropInvalidHosts(t *testing.T) {
	cases := []struct {
		name    string
		cfg     RunConfig
		env     map[string]string
		refused string
	}{
		{
			name:    "GitClone.URL is the cloud metadata IP",
			cfg:     RunConfig{GitClone: &api.GitCloneConfig{URL: "https://169.254.169.254/repo.git"}},
			refused: "169.254.169.254",
		},
		{
			name:    "SCION_GIT_CLONE_URL is a Kubernetes API host",
			env:     map[string]string{"SCION_GIT_CLONE_URL": "https://kubernetes.default.svc.cluster.local/repo.git"},
			refused: "kubernetes.default.svc.cluster.local",
		},
		{
			name:    "SCION_OTEL_ENDPOINT is a bare in-cluster suffix",
			env:     map[string]string{"SCION_OTEL_ENDPOINT": "collector.otel-system.svc:4317"},
			refused: "collector.otel-system.svc",
		},
		{
			name:    "OTEL_EXPORTER_OTLP_ENDPOINT is a loopback address",
			env:     map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"},
			refused: "127.0.0.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hosts, hostsErr := substrateEgressHostnames(tc.cfg, tc.env, config.V1SubstrateConfig{})
			if hostsErr != nil {
				t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
			}
			if containsHost(hosts, tc.refused) {
				t.Errorf("substrateEgressHostnames() = %v, must refuse tenant-derived host %q", hosts, tc.refused)
			}
		})
	}
}

// TestSubstrateEgressHostnames_InClusterHubStillAllowed proves the operator-
// config side of the trust split still works: an in-cluster hub endpoint
// (the exact shape substrate.NormalizeEgressAllowEntry's public-hostname
// grammar would reject) reaches the actor's EgressPolicy unchanged, because
// the hub host is never routed through that validator.
func TestSubstrateEgressHostnames_InClusterHubStillAllowed(t *testing.T) {
	cfg := RunConfig{TrustedHubEndpoint: "https://hub.scion-system.svc.cluster.local:8443"}
	hosts, hostsErr := substrateEgressHostnames(cfg, map[string]string{}, config.V1SubstrateConfig{})
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	if !containsHost(hosts, "hub.scion-system.svc.cluster.local") {
		t.Errorf("substrateEgressHostnames() = %v, want the trusted in-cluster hub host present", hosts)
	}
}

// TestSubstrateEgressHostnames_IPLiteralTrustedHubRefused proves an
// IP-literal trusted hub endpoint fails here, as a clear config error naming
// the offending value, rather than reaching CreateActorEgressPolicy
// unvalidated (the trusted hub host otherwise skips
// substrate.NormalizeEgressAllowEntry entirely — see
// TestSubstrateEgressHostnames_InClusterHubStillAllowed) and failing only at
// actor creation with Substrate's own opaque "HostnameRule does not support
// IP addresses" control-plane error.
func TestSubstrateEgressHostnames_IPLiteralTrustedHubRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
	}{
		{"IPv4", "https://10.0.0.5:8443"},
		{"IPv6", "https://[fd00::1]:8443"},
		{"bare IPv4 no scheme", "10.0.0.5:8443"},
		// inet_aton short, hex, octal and single-integer spellings
		// net/netip does not parse; refused by IsIPLiteralHost's
		// "ends in a number" test.
		{"short form 127.1", "https://127.1:8443"},
		{"short form 10.1", "https://10.1"},
		{"single hex integer", "https://0x7f000001"},
		{"single hex integer uppercase", "https://0X7F000001"},
		{"hex first octet", "https://0x7f.0.0.1"},
		{"octal first octet", "https://0177.0.0.1"},
		{"single octal integer", "https://017700000001"},
		{"single decimal integer", "https://2130706433"},
		{"short form with trailing dot", "https://127.1."},
		{"short form bare no scheme", "127.1:8443"},
		{"IPv6 with percent-encoded zone", "https://[fe80::1%25eth0]:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := RunConfig{TrustedHubEndpoint: tc.endpoint}
			hosts, hostsErr := substrateEgressHostnames(cfg, map[string]string{}, config.V1SubstrateConfig{})
			if hostsErr == nil {
				t.Fatalf("substrateEgressHostnames() error = nil, want a config error for IP-literal trusted hub endpoint %q (hosts = %v)", tc.endpoint, hosts)
			}
			if !strings.Contains(hostsErr.Error(), "resolves to IP address") || !strings.Contains(hostsErr.Error(), "hostname") {
				t.Errorf("substrateEgressHostnames() error = %q, want the IP-address refusal explaining that only hostname patterns are supported", hostsErr.Error())
			}
			if len(hosts) != 0 {
				t.Errorf("substrateEgressHostnames() returned hosts %v alongside its error, want none", hosts)
			}
		})
	}
}

// TestSubstrateEgressHostnames_UnencodedZoneTrustedHubAddsNoHost: a
// trusted hub endpoint with a raw (not percent-encoded) IPv6 zone is not a
// parseable URL, so no hub host is extracted and none is added — it fails
// closed by omission rather than reaching the policy in any form.
func TestSubstrateEgressHostnames_UnencodedZoneTrustedHubAddsNoHost(t *testing.T) {
	cfg := RunConfig{TrustedHubEndpoint: "https://[fe80::1%eth0]:8443"}
	hosts, hostsErr := substrateEgressHostnames(cfg, map[string]string{}, config.V1SubstrateConfig{})
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	for _, h := range hosts {
		if strings.Contains(h, "fe80") || strings.Contains(h, "eth0") {
			t.Errorf("substrateEgressHostnames() = %v, the zoned IPv6 hub host %q reached the policy", hosts, h)
		}
	}
}

// TestSubstrateEgressHostnames_HostnameTrustedHubStillAdded is the
// positive control for the IP-literal refusal: ordinary hub hostnames,
// including ones with digits in a non-final label, are still added,
// because only the last label decides whether a host is a number.
func TestSubstrateEgressHostnames_HostnameTrustedHubStillAdded(t *testing.T) {
	for endpoint, want := range map[string]string{
		"https://api2.example.com":                       "api2.example.com",
		"https://1.example.com:8443":                     "1.example.com",
		"https://10.0.0.1.nip.example.com":               "10.0.0.1.nip.example.com",
		"https://cafe.de":                                "cafe.de",
		"hub.scion-system.svc.cluster.local:8443":        "hub.scion-system.svc.cluster.local",
		"https://hub.scion-system.svc.cluster.local:443": "hub.scion-system.svc.cluster.local",
	} {
		cfg := RunConfig{TrustedHubEndpoint: endpoint}
		hosts, hostsErr := substrateEgressHostnames(cfg, map[string]string{}, config.V1SubstrateConfig{})
		if hostsErr != nil {
			t.Errorf("TrustedHubEndpoint %q: error = %v, want the hostname accepted", endpoint, hostsErr)
			continue
		}
		if !containsHost(hosts, want) {
			t.Errorf("TrustedHubEndpoint %q: hosts = %v, want %q present", endpoint, hosts, want)
		}
	}
}

// TestSubstrateEgressHostnames_IPLiteralTenantHostsDropped covers the
// tenant-derived hosts (ANTHROPIC_VERTEX_BASE_URL, the git clone URL, an
// OTEL endpoint), which go through addTenantHost and so
// substrate.NormalizeEgressAllowEntry: none of the inet_aton or zoned
// forms reaches the policy. The validator-level refusal reason for each
// form is pinned in pkg/runtime/substrate's TestValidateEgressAllow_RejectsAllIPAndCIDR
// and TestNormalizeEgressAllowEntry_ZonedIPv6Refused.
func TestSubstrateEgressHostnames_IPLiteralTenantHostsDropped(t *testing.T) {
	for _, host := range []string{
		"127.1", "10.1", "0x7f000001", "0x7f.0.0.1", "0177.0.0.1",
		"017700000001", "2130706433", "[fe80::1%25eth0]",
	} {
		env := map[string]string{
			"ANTHROPIC_VERTEX_BASE_URL":   "https://" + host + "/v1",
			"SCION_GIT_CLONE_URL":         "https://" + host + "/org/repo.git",
			"OTEL_EXPORTER_OTLP_ENDPOINT": "https://" + host + ":4317",
		}
		hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, config.V1SubstrateConfig{EgressAllow: []string{"api.example.com"}})
		if hostsErr != nil {
			t.Errorf("tenant host %q: error = %v, want it dropped, not a Run failure", host, hostsErr)
			continue
		}
		for _, h := range hosts {
			if h != "api.example.com" && !slices.Contains(hardcodedModelEgressHosts, h) {
				t.Errorf("tenant host %q: hosts = %v, unexpected entry %q reached the policy", host, hosts, h)
			}
		}
	}
}

// TestSubstrateEgressHostnames_HubEndpointOverrideIgnored proves that an
// agent/template config can still override SCION_HUB_ENDPOINT in the FINAL
// env (pkg/agent/run.go's own "final priority" override), but that override
// host never reaches the egress allowlist — only cfg.TrustedHubEndpoint
// (captured before the override applies) does. A template pointing
// SCION_HUB_ENDPOINT at the cloud metadata address, the in-cluster
// Kubernetes API, or any other arbitrary public host does not widen egress
// to reach it.
func TestSubstrateEgressHostnames_HubEndpointOverrideIgnored(t *testing.T) {
	const trusted = "https://hub.scion-system.svc.cluster.local:8443"
	cases := []struct {
		name     string
		override string
		refused  string
	}{
		{"override to the cloud metadata IP", "http://169.254.169.254/", "169.254.169.254"},
		{"override to the Kubernetes API", "https://kubernetes.default.svc:443", "kubernetes.default.svc"},
		{"override to an arbitrary public host", "https://attacker.example.com", "attacker.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := RunConfig{TrustedHubEndpoint: trusted}
			env := map[string]string{"SCION_HUB_ENDPOINT": tc.override}
			hosts, hostsErr := substrateEgressHostnames(cfg, env, config.V1SubstrateConfig{})
			if hostsErr != nil {
				t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
			}
			if containsHost(hosts, tc.refused) {
				t.Errorf("substrateEgressHostnames() = %v, must not add the overridden hub host %q", hosts, tc.refused)
			}
			if !containsHost(hosts, "hub.scion-system.svc.cluster.local") {
				t.Errorf("substrateEgressHostnames() = %v, want the trusted hub host still present", hosts)
			}
		})
	}
}

// TestSubstrateEgressHostnames_HubEndpointOverrideEqualToTrustedNoDup proves
// the equal-value case is still allowed, exactly once: when the final env's
// SCION_HUB_ENDPOINT happens to match cfg.TrustedHubEndpoint (the common
// case — no override in effect), the host appears once, not twice.
func TestSubstrateEgressHostnames_HubEndpointOverrideEqualToTrustedNoDup(t *testing.T) {
	cfg := RunConfig{TrustedHubEndpoint: "https://hub.scion-system.svc.cluster.local:8443"}
	env := map[string]string{"SCION_HUB_ENDPOINT": "https://hub.scion-system.svc.cluster.local:8443"}
	hosts, hostsErr := substrateEgressHostnames(cfg, env, config.V1SubstrateConfig{})
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	count := 0
	for _, h := range hosts {
		if h == "hub.scion-system.svc.cluster.local" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("substrateEgressHostnames() = %v, want the hub host exactly once, got %d", hosts, count)
	}
}

// TestSubstrateEgressHostnames_EmptyTrustedHubAddsNoHubHost proves the
// invariant the trusted-hub capture (pkg/agent/run.go) exists to protect:
// when cfg.TrustedHubEndpoint is empty (BrokerMode with no operator-derived
// hub endpoint at all), no hub host is added — not the empty trusted value,
// and not the final env's own SCION_HUB_ENDPOINT/SCION_HUB_URL, however
// tenant-influenced those may be. substrateEgressHostnames must never fall
// back to the env host when the trusted host is empty.
func TestSubstrateEgressHostnames_EmptyTrustedHubAddsNoHubHost(t *testing.T) {
	cfg := RunConfig{TrustedHubEndpoint: ""}
	env := map[string]string{
		"SCION_HUB_ENDPOINT": "http://169.254.169.254/",
		"SCION_HUB_URL":      "https://kubernetes.default.svc",
	}
	hosts, hostsErr := substrateEgressHostnames(cfg, env, config.V1SubstrateConfig{})
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	for _, refused := range []string{"169.254.169.254", "kubernetes.default.svc"} {
		if containsHost(hosts, refused) {
			t.Errorf("substrateEgressHostnames() = %v, must not add env hub host %q when TrustedHubEndpoint is empty", hosts, refused)
		}
	}
	want := append([]string{}, hardcodedModelEgressHosts...)
	sort.Strings(want)
	got := append([]string{}, hosts...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("substrateEgressHostnames() = %v, want exactly the hardcoded model hosts %v (no hub host at all)", hosts, want)
	}
}

// TestSubstrateEgressHostnames_ValidPublicOTELHostAllowed proves the
// tenant-controllable path is refuse-invalid, not refuse-everything: an
// ordinary public OTLP collector hostname still passes through
// addTenantHost and reaches the result, normalized.
func TestSubstrateEgressHostnames_ValidPublicOTELHostAllowed(t *testing.T) {
	env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://Otel-Collector.Example.COM:4318"}
	sc := config.V1SubstrateConfig{EgressAllow: []string{"otel-collector.example.com"}}
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, sc)
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	if !containsHost(hosts, "otel-collector.example.com") {
		t.Errorf("substrateEgressHostnames() = %v, want the normalized public OTEL host present", hosts)
	}
}

// TestSubstrateEgressHostnames_TenantHostRequiresEgressAllowCoverage proves
// a well-formed, public tenant-derived host (one that passes
// NormalizeEgressAllowEntry) is still dropped unless an operator
// egress_allow entry covers it. A well-formed public hostname is not
// automatically trustworthy — a service like nip.io/sslip.io, or simply a
// domain the tenant registers themselves, can resolve to any address the
// tenant chooses, the cloud metadata address included.
func TestSubstrateEgressHostnames_TenantHostRequiresEgressAllowCoverage(t *testing.T) {
	env := map[string]string{"SCION_GIT_CLONE_URL": "https://uncovered.example.com/repo.git"}
	// No egress_allow entry covers uncovered.example.com.
	sc := config.V1SubstrateConfig{EgressAllow: []string{"covered.example.com"}}
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, sc)
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	if containsHost(hosts, "uncovered.example.com") {
		t.Errorf("substrateEgressHostnames() = %v, want uncovered.example.com dropped (no egress_allow entry covers it)", hosts)
	}
}

// TestSubstrateEgressHostnames_TenantHostCoveredByEgressAllow proves the
// other side: a tenant-derived host IS added when an operator egress_allow
// entry covers it, both via an exact match and via a wildcard entry one
// label above it.
func TestSubstrateEgressHostnames_TenantHostCoveredByEgressAllow(t *testing.T) {
	t.Run("exact match", func(t *testing.T) {
		env := map[string]string{"SCION_GIT_CLONE_URL": "https://git.example.com/repo.git"}
		sc := config.V1SubstrateConfig{EgressAllow: []string{"git.example.com"}}
		hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, sc)
		if hostsErr != nil {
			t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
		}
		if !containsHost(hosts, "git.example.com") {
			t.Errorf("substrateEgressHostnames() = %v, want git.example.com present (exact egress_allow match)", hosts)
		}
	})
	t.Run("wildcard match", func(t *testing.T) {
		env := map[string]string{"SCION_GIT_CLONE_URL": "https://git.example.com/repo.git"}
		sc := config.V1SubstrateConfig{EgressAllow: []string{"*.example.com"}}
		hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, sc)
		if hostsErr != nil {
			t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
		}
		if !containsHost(hosts, "git.example.com") {
			t.Errorf("substrateEgressHostnames() = %v, want git.example.com present (covered by the *.example.com wildcard)", hosts)
		}
	})
}

// TestSubstrateEgressHostnames_NipIoStyleHostRequiresCoverage proves a
// nip.io-style host (a public, well-formed hostname that resolves an
// embedded IP octet on request — including the cloud metadata address) is
// dropped the same as any other uncovered tenant host, and only added when
// an operator egress_allow entry explicitly covers it.
func TestSubstrateEgressHostnames_NipIoStyleHostRequiresCoverage(t *testing.T) {
	cases := []struct {
		name string
		host string
	}{
		{"cloud metadata address embedded in nip.io", "169.254.169.254.nip.io"},
		{"private address embedded in nip.io", "10.0.0.1.nip.io"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"SCION_GIT_CLONE_URL": "https://" + tc.host + "/repo.git"}

			t.Run("uncovered, dropped", func(t *testing.T) {
				hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, config.V1SubstrateConfig{})
				if hostsErr != nil {
					t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
				}
				if containsHost(hosts, tc.host) {
					t.Errorf("substrateEgressHostnames() = %v, want %q dropped (not covered by any egress_allow entry)", hosts, tc.host)
				}
			})
			t.Run("covered, added", func(t *testing.T) {
				sc := config.V1SubstrateConfig{EgressAllow: []string{tc.host}}
				hosts, hostsErr := substrateEgressHostnames(RunConfig{}, env, sc)
				if hostsErr != nil {
					t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
				}
				if !containsHost(hosts, tc.host) {
					t.Errorf("substrateEgressHostnames() = %v, want %q present (operator explicitly covered it)", hosts, tc.host)
				}
			})
		})
	}
}

// TestSubstrateEgressHostnames_HardcodedModelHostsUnaffectedByCoverage
// proves hardcodedModelEgressHosts is unaffected by the egress_allow
// coverage gate addTenantHost applies: these hosts are a fixed literal in
// the binary, never tenant-derived, and are always present regardless of
// egress_allow.
func TestSubstrateEgressHostnames_HardcodedModelHostsUnaffectedByCoverage(t *testing.T) {
	hosts, hostsErr := substrateEgressHostnames(RunConfig{}, map[string]string{}, config.V1SubstrateConfig{})
	if hostsErr != nil {
		t.Fatalf("substrateEgressHostnames() error = %v", hostsErr)
	}
	for _, want := range hardcodedModelEgressHosts {
		if !containsHost(hosts, want) {
			t.Errorf("substrateEgressHostnames() = %v, want hardcoded model host %q present with no egress_allow entries at all", hosts, want)
		}
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
