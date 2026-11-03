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
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ErrPlaintextAgentHubEndpoint is returned (wrapped) by Start when the hub
// endpoint in the agent's final environment would carry the agent's hub
// credential unencrypted to a host other than the container's own host.
var ErrPlaintextAgentHubEndpoint = errors.New("agent hub endpoint is unencrypted and not host-local")

// agentHubEndpointEnvKeys are the env keys an agent reads its hub endpoint
// from (sciontool and the agent-mode CLI read both). Each is checked.
var agentHubEndpointEnvKeys = []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"}

// agentHubTransportFix is the remedy named in the refusal.
const agentHubTransportFix = "give agents an https:// hub endpoint (a TLS-terminating proxy, ingress or Service in front of the hub), " +
	"a loopback endpoint, or, on a local container runtime, the runtime's host alias"

// hostGatewayTarget is the --add-host target Docker and Podman map to the
// container's own host.
const hostGatewayTarget = "host-gateway"

// hostLocalAlias is a host alias accepted for a runtime. requiresMapping is
// set for a name that is host-local only through a name:host-gateway entry
// in the container's --add-host list, because the runtime does not always
// resolve it itself.
type hostLocalAlias struct {
	name            string
	requiresMapping bool
}

// hostLocalAliasesByRuntime lists, per local container runtime, the host
// aliases that reach the container's own host. Every name is also an
// config.IsAgentHostAliasHost name, so the agent-side CLI's exemption accepts
// every endpoint this policy accepts. Runtimes not listed have no host-local
// alias: they run agents off the hub's host (kubernetes, substrate, cloudrun)
// or are unknown, and fail closed.
//   - docker: host.docker.internal and scion-hub.internal, each only with
//     its host-gateway entry. Docker Engine on Linux does not resolve
//     host.docker.internal itself; the agent manager maps it
//     (runtime.BridgeExtraHosts, which matches the canonical lower-case
//     spelling only), and the co-located broker maps scion-hub.internal
//     (colocatedExtraHosts in pkg/runtimebroker). Without the entry the
//     name would go to the container's DNS resolver.
//   - podman: host.containers.internal, which Podman maps to the host.
//   - container (Apple): host.containers.internal, through the DNS rule the
//     server installs (runtime.AppleDNSHostname). Apple strips --add-host,
//     so an alias there must never require a mapping
//     (TestAppleAliasesNeedNoMapping).
var hostLocalAliasesByRuntime = map[string][]hostLocalAlias{
	"docker": {
		{name: "host.docker.internal", requiresMapping: true},
		{name: "scion-hub.internal", requiresMapping: true},
	},
	"podman":    {{name: "host.containers.internal"}},
	"container": {{name: "host.containers.internal"}},
}

// checkAgentHubTransport applies the transport policy for the hub endpoint
// an agent is started with to every SCION_HUB_ENDPOINT / SCION_HUB_URL value
// in env, the agent's FINAL environment (after every override: the
// agent-level hub config, the config env layer, host networking's rewrite).
//
// An endpoint is accepted when config.CheckHubTransport accepts it (https,
// wss, or http/ws to a literal loopback host), or when it is http/ws to a
// host-local host for runtimeName (isHostLocalAgentHubHost). Everything else
// is refused with an error wrapping ErrPlaintextAgentHubEndpoint and
// config.ErrPlaintextNonLoopback.
//
// runtimeName must be the agent manager's own runtime (Runtime.Name()) and
// extraHosts the --add-host entries the broker and agent manager build for
// the container; neither may come from a request, template or agent config,
// since they decide what counts as host-local.
func checkAgentHubTransport(agentName, runtimeName string, extraHosts, env []string) error {
	for _, e := range env {
		key, value, ok := strings.Cut(e, "=")
		if !ok || value == "" || !isAgentHubEndpointEnvKey(key) {
			continue
		}
		if err := checkAgentHubEndpoint(runtimeName, extraHosts, value); err != nil {
			return fmt.Errorf("cannot start agent %q: the hub endpoint in %s is not allowed on the %q runtime: %w: %w; %s",
				agentName, key, runtimeName, ErrPlaintextAgentHubEndpoint, err, agentHubTransportFix)
		}
	}
	return nil
}

func isAgentHubEndpointEnvKey(key string) bool {
	for _, k := range agentHubEndpointEnvKeys {
		if key == k {
			return true
		}
	}
	return false
}

// checkAgentHubEndpoint returns nil when rawURL may be given to an agent on
// runtimeName. Otherwise it returns an error wrapping
// config.ErrPlaintextNonLoopback that names the URL with its credentials
// redacted: for http/ws, its own text (the remedy is agentHubTransportFix);
// for anything else (another scheme, an unparsable URL, no host),
// config.CheckHubTransport's.
func checkAgentHubEndpoint(runtimeName string, extraHosts []string, rawURL string) error {
	transportErr := config.CheckHubTransport(rawURL)
	if transportErr == nil {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return transportErr
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
	default:
		return transportErr
	}
	if isHostLocalAgentHubHost(runtimeName, u.Hostname(), extraHosts) {
		return nil
	}
	return fmt.Errorf("%w: %s uses %s to host %q, which is neither a loopback address nor a host alias of this runtime, "+
		"so the agent's hub credential would cross the network unencrypted",
		config.ErrPlaintextNonLoopback, config.RedactURL(rawURL), strings.ToLower(u.Scheme), u.Hostname())
}

// isHostLocalAgentHubHost reports whether host, in a container on
// runtimeName, names the container's own host, so plaintext to it never
// leaves that host. Names are matched literally (case-insensitive, no
// trailing dot) and never resolved.
//   - cloudrun-sandbox: an IPv4 link-local literal (169.254.0.0/16), the
//     launcher address the broker gives sandboxes on its own instance.
//   - docker, podman, container: a host alias from hostLocalAliasesByRuntime.
//     An IP literal is never host-local here: with a remote Docker daemon the
//     broker cannot tell whose interface an address is, while an alias always
//     resolves on the container's own host. An alias that requires a
//     mapping is accepted only with a name:host-gateway entry in extraHosts;
//     for any alias, an entry with another target refuses it. A trailing dot
//     is not accepted: an absolute name may skip /etc/hosts and go to DNS.
//   - every other runtime, including an unknown one: never.
func isHostLocalAgentHubHost(runtimeName, host string, extraHosts []string) bool {
	host = strings.ToLower(host)
	if host == "" {
		return false
	}
	if runtimeName == "cloudrun-sandbox" {
		ip := net.ParseIP(host)
		return ip != nil && ip.To4() != nil && ip.IsLinkLocalUnicast()
	}
	if net.ParseIP(host) != nil {
		return false
	}
	for _, alias := range hostLocalAliasesByRuntime[runtimeName] {
		if host != alias.name {
			continue
		}
		if runtimeName == "container" {
			// Apple drops --add-host, so a mapping neither helps nor hurts;
			// its aliases never require one (TestAppleAliasesNeedNoMapping).
			return !alias.requiresMapping
		}
		target, mapped := extraHostTarget(extraHosts, host)
		if mapped {
			return target == hostGatewayTarget
		}
		return !alias.requiresMapping
	}
	return false
}

// extraHostTarget returns the target of the first --add-host entry
// ("name:target") for host, the entry the runtime applies (mergeExtraHosts
// keeps the first entry per name). Names compare case-insensitively, as
// /etc/hosts lookups do; host is already lower case.
func extraHostTarget(extraHosts []string, host string) (string, bool) {
	for _, h := range extraHosts {
		name, target, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		if strings.ToLower(name) == host {
			return strings.ToLower(target), true
		}
	}
	return "", false
}
