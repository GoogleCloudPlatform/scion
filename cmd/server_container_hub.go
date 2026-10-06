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
	"fmt"
	"strings"
)

// colocatedHubHostAlias is the hostname colocated Docker agents use to reach
// the hub on the host when the hub's public URL is not served by this host.
// It is not an IP: the broker's colocatedExtraHosts maps it to
// host-gateway, so it follows the real docker0 gateway, and because it is
// neither localhost nor host.docker.internal the agent stays on bridge
// networking (runtime.ResolveHostNetworking).
const colocatedHubHostAlias = "scion-hub.internal"

// containerHubEndpointInputs are the facts computeContainerHubEndpoint
// decides from. Side-effecting probes (env lookups, the Docker host-gateway
// check) are done by the caller.
type containerHubEndpointInputs struct {
	// Configured is runtime_broker.container_hub_endpoint; when set it wins.
	Configured string
	// HubEnabled reports whether the hub runs in this process (colocated).
	HubEnabled bool
	// BrokerHubEndpoint is the broker's own hub URL (localhost when colocated).
	BrokerHubEndpoint string
	// RuntimeName is the broker's default runtime; empty when there is none.
	RuntimeName string
	// PublicHubEndpoint and PublicHubEndpointSource come from
	// resolveHubEndpointWithSource.
	PublicHubEndpoint       string
	PublicHubEndpointSource hubEndpointSource
	// HubListenPort is resolveHubListenPort.
	HubListenPort int
	// ForceHostNetwork is true when SCION_FORCE_HOST_NETWORK is set or the
	// Docker daemon lacks host-gateway support.
	ForceHostNetwork bool
}

// containerHubEndpointResult is what the runtime broker is configured with.
type containerHubEndpointResult struct {
	// Endpoint becomes runtimebroker.ServerConfig.ContainerHubEndpoint.
	Endpoint string
	// ColocatedPublicHubEndpoint becomes
	// runtimebroker.ServerConfig.ColocatedPublicHubEndpoint: the public URL
	// the broker rewrites to Endpoint for non-Kubernetes agents.
	ColocatedPublicHubEndpoint string
}

// computeContainerHubEndpoint decides the hub URL colocated agents are
// given in place of the broker's own (localhost) hub URL.
//
// For colocated Docker agents we prefer bridge networking, so each agent
// runs in its own network namespace. This avoids the host-global
// metadata-server (:18380) and telemetry (:4317) port collisions that
// --network=host causes for concurrent agents.
//   - When the public URL is served by this host (an explicit base URL or
//     public_url, fronted by Caddy), agents are routed at it;
//     colocatedExtraHosts maps its host to host-gateway.
//   - When the public URL was only derived from the IAP audience, this host
//     does not serve it (the single-node VM serves the hub on its listen
//     port, and the URL is a Cloud Run IAP front end agents cannot pass).
//     Agents get http://<colocatedHubHostAlias>:<listen port> and the broker
//     rewrites the public URL to it.
//   - Otherwise agents fall back to the legacy host.docker.internal path
//     (host networking).
//
// Kubernetes runtime profiles are never rewritten by the broker, so the
// IAP URL still reaches GKE-dispatched agents in hybrid deployments.
//
// logf receives informational and warning messages.
func computeContainerHubEndpoint(in containerHubEndpointInputs, logf func(format string, args ...any)) containerHubEndpointResult {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if in.Configured != "" || !in.HubEnabled || in.BrokerHubEndpoint == "" || in.RuntimeName == "" {
		return containerHubEndpointResult{Endpoint: in.Configured}
	}

	isDocker := in.RuntimeName == "docker"
	publicDomain := ""
	if in.PublicHubEndpoint != "" && !isLocalhostURL(in.PublicHubEndpoint) {
		publicDomain = strings.TrimRight(in.PublicHubEndpoint, "/")
	}
	iapDerived := publicDomain != "" && in.PublicHubEndpointSource == hubEndpointSourceIAPAudience

	var res containerHubEndpointResult
	switch {
	case isDocker && !in.ForceHostNetwork && iapDerived && in.HubListenPort > 0:
		res.Endpoint = fmt.Sprintf("http://%s:%d", colocatedHubHostAlias, in.HubListenPort)
		logf("Colocated %s agents routed via %s (bridge networking); public hub URL %s is derived from the IAP audience and not served by this host", in.RuntimeName, res.Endpoint, publicDomain)
	case isDocker && !in.ForceHostNetwork && publicDomain != "" && !iapDerived:
		// applyContainerBridgeOverride returns a non-bridge-hostname target
		// wholesale, preserving the domain's scheme and implicit port.
		res.Endpoint = publicDomain
		logf("Colocated %s agents routed via public domain %s (bridge networking)", in.RuntimeName, res.Endpoint)
	default:
		if computed := containerBridgeEndpoint(in.BrokerHubEndpoint, in.RuntimeName); computed != "" {
			res.Endpoint = computed
			if isDocker && !in.ForceHostNetwork {
				logf("WARNING: no public domain configured for colocated Docker agents; falling back to host networking. Set SCION_SERVER_BASE_URL=https://<domain> to enable per-agent bridge networking.")
			}
			logf("Auto-computed ContainerHubEndpoint for %s runtime: %s", in.RuntimeName, res.Endpoint)
		}
	}

	// An IAP-derived public URL is unreachable from colocated Docker agents
	// whichever route they get, so the broker must rewrite it too.
	if isDocker && iapDerived && res.Endpoint != "" {
		res.ColocatedPublicHubEndpoint = publicDomain
	}
	return res
}
