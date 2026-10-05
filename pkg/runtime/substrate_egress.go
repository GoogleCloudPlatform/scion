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
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// hardcodedModelEgressHosts are the harness model API hosts this runtime
// hardcodes (substrate-runtime.md §7): direct Anthropic API access, and the
// Google OAuth2 token endpoint every GCP-authenticated call (including
// Vertex) needs regardless of region. This deliberately does NOT include a
// "*.googleapis.com" wildcard: that covers every Google API — GCS, Compute,
// BigQuery, and hundreds more — not just the ones this runtime actually
// calls, and would let an actor exfiltrate to any GCS bucket or any
// project's Google API. The Vertex AI endpoint itself is added separately,
// by substrateEgressHostnames, derived from the agent's own configured
// region (see addVertexAIHost) rather than hardcoded here, since the
// correct host depends on CLOUD_ML_REGION. The configured telemetry
// endpoint host is likewise added as its own rule from the actual env var
// in play, not assumed from this list — see the telemetry section below.
var hardcodedModelEgressHosts = []string{
	"api.anthropic.com",
	"oauth2.googleapis.com",
}

// vertexRegionLabelPattern matches a single valid DNS label (lowercase
// letters, digits, and interior hyphens only — RFC 1123, not starting or
// ending with a hyphen). CLOUD_ML_REGION must match this exactly, or
// addVertexAIHost refuses it: this is the security-load-bearing check for
// the whole function, not a formatting nicety. A region of "*" fed
// unchecked into "<region>-aiplatform.googleapis.com" would construct
// "*-aiplatform.googleapis.com" — a wildcard host, exactly the class of
// grant hardcodedModelEgressHosts's doc comment explains removing
// "*.googleapis.com" to avoid. A region containing "." could construct an
// extra label Google's own regional endpoints never have. Uppercase is
// refused rather than silently lowercased, since CLOUD_ML_REGION reaching
// here with the wrong case indicates a misconfiguration worth surfacing,
// not papering over.
var vertexRegionLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// addVertexAIHost adds the Vertex AI regional endpoint host
// ("<region>-aiplatform.googleapis.com", or the bare "aiplatform.googleapis.com"
// for the global/unset region) when the resolved env indicates Vertex AI
// auth is in play (CLAUDE_CODE_USE_VERTEX, set by
// pkg/harness/container_script_harness.go's vertex-ai translation, or
// ANTHROPIC_VERTEX_PROJECT_ID/CLOUD_ML_REGION directly, for a harness that
// sets them without going through that translation). Added via add (not
// addTenantHost): the region is agent/template-configurable, but once
// validated against vertexRegionLabelPattern the result always terminates
// in the fixed ".googleapis.com" suffix — a tenant can only choose which
// Google-owned regional endpoint is reached, never redirect to
// infrastructure outside Google's own, so this needs no egress_allow
// coverage gate, the same trust basis as the other entries in
// hardcodedModelEgressHosts. An invalid region is a config error, not a
// silent skip: returning one here must fail Run closed (see its caller),
// since silently dropping the Vertex host would leave a Vertex-configured
// agent unable to reach its model API at all, an availability failure
// that's easier to diagnose as a refused Run than as an opaque egress
// timeout.
//
// This covers ONLY the Claude harness's Vertex AI integration — the sole
// one substrate currently wires through container_script_harness.go's
// vertex-ai env translation (substrate-runtime.md §7). A harness with its
// own model-API host (Vertex-backed or otherwise) needs the same two-part
// treatment — a validated, fixed-Google-suffix host via add(), or a tenant
// host via addTenantHost's coverage gate — added here when that harness is
// wired for substrate.
func addVertexAIHost(add func(string), env map[string]string) error {
	usesVertex := env["CLAUDE_CODE_USE_VERTEX"] != "" ||
		env["ANTHROPIC_VERTEX_PROJECT_ID"] != "" ||
		env["CLOUD_ML_REGION"] != ""
	if !usesVertex {
		return nil
	}
	region := env["CLOUD_ML_REGION"]
	var host string
	if region == "" || region == "global" {
		host = "aiplatform.googleapis.com"
	} else {
		if !vertexRegionLabelPattern.MatchString(region) {
			return fmt.Errorf("CLOUD_ML_REGION %q is not a single valid DNS label; refusing to construct a Vertex AI egress host from it", region)
		}
		host = region + "-aiplatform.googleapis.com"
	}
	normalized, err := substrate.NormalizeEgressAllowEntry(host)
	if err != nil {
		return fmt.Errorf("constructed Vertex AI host %q failed validation: %w", host, err)
	}
	add(normalized)
	return nil
}

// substrateEgressHostnames collects the hostname patterns for the actor's
// EgressPolicy (substrate-runtime.md §7): the hub endpoint host, the git
// clone host, the hardcoded harness model hosts, and egress_allow from
// settings. Duplicates are removed; empty/unresolvable hosts are skipped
// rather than failing Run — a missing hub or git host here means the
// operator will see egress-denied failures on the cluster, which is
// diagnosable, rather than Run refusing to create the actor at all.
//
// Every host added below falls into one of two trust sources, and each is
// handled accordingly:
//
//   - Operator-config: cfg.TrustedHubEndpoint (captured broker/operator-side
//     before an agent/template env override can change it — see RunConfig's
//     own doc comment and pkg/agent/run.go) and hardcodedModelEgressHosts (a
//     fixed literal in this binary, never derived from any input). Neither
//     is validated as a public hostname: an in-cluster hub (e.g.
//     "hub.scion-system.svc.cluster.local") is a legitimate, common
//     deployment shape that substrate.NormalizeEgressAllowEntry's
//     public-hostname grammar would reject outright (it deliberately
//     excludes "svc"/"cluster.local" and every other non-ICANN suffix — see
//     that function's own doc comment). The FINAL env's own
//     SCION_HUB_ENDPOINT/SCION_HUB_URL — which an agent/template override
//     can still change — is read only to detect and log a mismatch against
//     the trusted host; it is never itself added.
//   - Tenant-controllable: cfg.GitClone.URL/SCION_GIT_CLONE_URL (the
//     workload's own configured git remote) and every OTEL_*/SCION_OTEL_*
//     endpoint (agent/template env this function reads from the same env
//     map the actor's own bootstrap payload is built from — see
//     buildBootstrapEnv's callers). Each of these goes through
//     addTenantHost, which admits a tenant host ONLY if an operator
//     egress_allow entry covers it (substrate.EgressAllowCovers: an exact
//     match, or a wildcard entry one label above it) — that coverage gate
//     is the PRIMARY control, so a tenant value can never add a host the
//     operator has not already allowed. As defense-in-depth, the host must
//     first pass the same substrate.NormalizeEgressAllowEntry validator
//     operator-configured egress_allow entries use below, which also
//     rejects every IP-literal/CIDR form (loopback, link-local including
//     the cloud metadata address, private, and any other numeric address)
//     via its own looksLikeIPAttempt check.
func substrateEgressHostnames(cfg RunConfig, env map[string]string, sc config.V1SubstrateConfig) ([]string, error) {
	seen := make(map[string]struct{})
	var hosts []string
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" {
			return
		}
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		hosts = append(hosts, h)
	}
	// operatorEgressAllow is every sc.EgressAllow entry, normalized, computed
	// up front so addTenantHost can check coverage against it. Entries that
	// fail to normalize are skipped here the same way the sc.EgressAllow
	// loop below skips them: substrate.Validate(r.cfg), called at the top of
	// Run before this is ever reached, already rejects a config with an
	// invalid entry, so this is unreachable in practice, not a second
	// enforcement point.
	var operatorEgressAllow []string
	for _, h := range sc.EgressAllow {
		if normalized, err := substrate.NormalizeEgressAllowEntry(h); err == nil && normalized != "" {
			operatorEgressAllow = append(operatorEgressAllow, normalized)
		}
	}

	// addTenantHost admits h (a hostname derived from tenant-controllable
	// input) only if it is both a well-formed public hostname (the same
	// grammar an operator's own egress_allow entry must pass) AND covered by
	// an operator egress_allow entry — an exact match, or a wildcard entry
	// whose remainder it is one label under (substrate.EgressAllowCovers).
	// Well-formedness alone is not enough: an attacker-chosen but otherwise
	// ordinary-looking public hostname (a service like nip.io/sslip.io that
	// resolves an embedded IP octet on request, or any domain the tenant
	// simply registers themselves) would pass the grammar check while still
	// letting the tenant point their own git-clone remote or telemetry
	// endpoint at an address of their choosing — the cloud metadata address
	// chief among them. Requiring operator coverage means only a host the
	// operator has already agreed to allow ever gets a tenant-controlled
	// vote toward being added; everything else is dropped, not added,
	// naming the host and how to allow it. Only the bare host is logged,
	// matching NormalizeEgressAllowEntry's own callers elsewhere: never the
	// userinfo, query, or full URL a caller derived it from, either of which
	// could carry a credential.
	//
	// This function can therefore never WIDEN the resulting policy beyond
	// what sc.EgressAllow already grants on its own: every operatorEgressAllow
	// entry is added unconditionally further below, regardless of whether any
	// tenant-derived host ever matches it. A covered h only ever adds a host
	// already subsumed by that operator entry (or an exact duplicate of it);
	// an uncovered h adds nothing at all. addTenantHost's real value is
	// diagnostic — the WARN naming an uncovered host and how to allow it —
	// not a gate standing between a tenant value and a wider policy.
	addTenantHost := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" {
			return
		}
		normalized, err := substrate.NormalizeEgressAllowEntry(h)
		if err != nil {
			slog.Warn("substrate: dropping invalid tenant-derived egress host", "host", h, "error", err)
			return
		}
		if !substrate.EgressAllowCovers(operatorEgressAllow, normalized) {
			slog.Warn("substrate: dropping tenant-derived egress host not covered by egress_allow",
				"host", normalized, "hint", "add this host (or a wildcard covering it) to runtimes.<name>.substrate.egress_allow to permit it")
			return
		}
		add(normalized)
	}

	// The trusted hub host — resolved from cfg.TrustedHubEndpoint, which
	// RunConfig's own doc comment guarantees is broker/operator-controlled
	// (captured before an agent/template config's own SCION_HUB_ENDPOINT
	// env entry can override it — see pkg/agent/run.go) — is the only hub
	// host ever added, and is never run through addTenantHost/
	// NormalizeEgressAllowEntry: an in-cluster hub (e.g.
	// "hub.scion-system.svc.cluster.local") is legitimate precisely because
	// it comes from the operator, and that validator's public-hostname
	// grammar would reject it outright. If the FINAL env's own
	// SCION_HUB_ENDPOINT/SCION_HUB_URL host (env, not cfg.TrustedHubEndpoint
	// — this is deliberately the value an agent/template override can have
	// changed) differs from the trusted host, the override is never added:
	// widening egress to match an agent-chosen value (the cloud metadata
	// address, the in-cluster Kubernetes API, or any other host) would
	// defeat the whole point of resolving a trusted value separately. The
	// agent's own hub calls may still fail in that case — fail closed, not
	// a wider allowlist — logged here with both hosts, never a scheme,
	// userinfo, path, or query.
	trustedHubHost := hostFromURL(cfg.TrustedHubEndpoint)
	if trustedHubHost != "" {
		if substrate.IsIPLiteralHost(trustedHubHost) {
			// An IP-literal host, added unvalidated (see the comment above
			// on why the trusted hub host skips NormalizeEgressAllowEntry),
			// including the short and mixed-radix inet_aton spellings
			// ("127.1", "0x7f000001", "2130706433") netip alone misses,
			// would otherwise reach CreateActorEgressPolicy as a
			// HostnameRule pattern and fail only there, with an opaque
			// control-plane error — Substrate's HostnameRule explicitly
			// does not support IP addresses, only hostname patterns (see
			// ValidateEgressAllow's own doc comment). Fail here instead,
			// as a clear config error naming the offending value, rather
			// than let that failure surface three layers down.
			return nil, fmt.Errorf("substrate: trusted hub endpoint %q resolves to IP address %q; configure the hub endpoint with a DNS hostname — substrate's egress policy supports hostname patterns only, not IP addresses", cfg.TrustedHubEndpoint, trustedHubHost)
		}
		add(trustedHubHost)
	}
	if finalHubHost := hostFromURLEnv(env, "SCION_HUB_ENDPOINT", "SCION_HUB_URL"); finalHubHost != "" && finalHubHost != trustedHubHost {
		slog.Warn("substrate: ignoring agent env hub endpoint that does not match the operator-configured hub",
			"trusted_host", trustedHubHost, "env_host", finalHubHost)
	}

	if cfg.GitClone != nil && cfg.GitClone.URL != "" {
		if h := hostFromURL(cfg.GitClone.URL); h != "" {
			addTenantHost(h)
		} else {
			// hostFromURL returns "" for a URL it cannot extract a hostname
			// from — notably an scp-style git remote ("git@host:org/r.git",
			// no "://"), which url.Parse does not accept as a URL at all.
			// This fails closed (no host is added, so the agent's own git
			// clone may fail with egress-denied rather than being silently
			// widened), but with no log line until now, that failure mode
			// was indistinguishable from any other unrelated egress-denied
			// outcome.
			slog.Warn("substrate: could not extract a hostname from the configured git clone URL; no egress host added for it", "hint", "scp-style remotes (git@host:org/repo.git) are not supported here — use an https:// URL")
		}
	} else if h := hostFromURLEnv(env, "SCION_GIT_CLONE_URL"); h != "" {
		addTenantHost(h)
	}

	// The configured telemetry endpoint host (substrate-runtime.md §7). No
	// wildcard covers a default here (hardcodedModelEgressHosts carries no
	// "*.googleapis.com" entry): a self-hosted OTLP collector, the hub's
	// own endpoint, or the GCP default Cloud Trace exporter all need their
	// own explicit coverage. Checked independently, not via a single
	// hostFromURLEnv call, because a deployment could point different
	// signals at different collectors.
	for _, key := range []string{
		"SCION_OTEL_ENDPOINT",                // pkg/sciontool/telemetry, pkg/util/logging: scion's own var.
		"OTEL_EXPORTER_OTLP_ENDPOINT",        // OTel SDK standard var, if a harness sets it directly.
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", // OTel SDK per-signal override.
	} {
		if h := hostFromURLEnv(env, key); h != "" {
			addTenantHost(h)
		}
	}

	// ANTHROPIC_VERTEX_BASE_URL (or any other model base-URL override a
	// harness forwards) is NOT a fixed Google host the way
	// addVertexAIHost's constructed endpoint is: it is a URL an
	// agent/template can set outright, so its host goes through
	// addTenantHost's coverage gate like the git-clone and telemetry hosts
	// above, never add().
	if h := hostFromURLEnv(env, "ANTHROPIC_VERTEX_BASE_URL"); h != "" {
		addTenantHost(h)
	}

	for _, h := range hardcodedModelEgressHosts {
		add(h)
	}
	if err := addVertexAIHost(add, env); err != nil {
		return nil, err
	}

	// operatorEgressAllow already holds every sc.EgressAllow entry in the
	// exact normalized form ValidateEgressAllow validated it in, so this
	// sends exactly that string, not a re-normalized or merely trimmed one
	// — see operatorEgressAllow's own doc comment for why that matters.
	for _, h := range operatorEgressAllow {
		add(h)
	}

	sort.Strings(hosts)
	return hosts, nil
}

// hostFromURLEnv returns the hostname portion of the first non-empty env
// value found among keys, or "" if none are set or parseable.
func hostFromURLEnv(env map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := env[k]; v != "" {
			if h := hostFromURL(v); h != "" {
				return h
			}
		}
	}
	return ""
}

// hostFromURL extracts the hostname from a URL string, tolerating bare
// host[:port] values (no scheme) as well as full URLs.
func hostFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	u, err := url.Parse(candidate)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return u.Hostname()
}

// buildEgressPolicy builds the CreateActorEgressPolicyRequest for actor
// atespace/actorName, with a single EgressRule matching any of hostnames
// (substrate-runtime.md §7). "default" is the only permitted egress
// policy resource name per the EgressPolicy proto comment.
func buildEgressPolicy(atespace, actorName string, hostnames []string) *ateapipb.CreateActorEgressPolicyRequest {
	return &ateapipb.CreateActorEgressPolicyRequest{
		Actor: &ateapipb.ObjectRef{Atespace: atespace, Name: actorName},
		EgressPolicy: &ateapipb.EgressPolicy{
			Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: "default"},
			Rules: []*ateapipb.EgressRule{
				{
					Hostnames: &ateapipb.HostnameRule{Patterns: hostnames},
				},
			},
		},
	}
}
