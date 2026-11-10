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

import "fmt"

const hostNetworkMode = "host"

// hostNetworkMetadataFix is the remedy named in the refusal and the warning
// below: with a hub URL the container can reach over the bridge, and without
// the host-networking escape hatch, the broker does not put the agent on
// host networking.
const hostNetworkMetadataFix = "configure a public or bridge-reachable hub URL (SCION_SERVER_BASE_URL) so agents run on bridge networking, and leave SCION_FORCE_HOST_NETWORK unset on the broker"

// hostNetworkMetadataWarning is shown to the user when a block-mode agent
// starts on host networking. It names no values.
const hostNetworkMetadataWarning = "Warning: GCP metadata mode \"block\" is not enforced at the network level under host networking; " + hostNetworkMetadataFix

// appliesContainerNetworkMode reports whether the runtime places the agent in
// the network mode chosen by runtime.ResolveHostNetworking and grants the
// capabilities in RunConfig: the Docker and Podman runtimes. The Apple
// runtime strips --network and --cap-add, and the Kubernetes and Cloud Run
// runtimes build their own workloads, so none of them share the host's
// network namespace through this path.
func appliesContainerNetworkMode(runtimeName string) bool {
	return runtimeName == "docker" || runtimeName == "podman"
}

// metadataModeIn reports whether env holds SCION_METADATA_MODE=<mode>. Like
// hasMetadataInterception it matches any entry, so a contradictory duplicate
// resolves to the restrictive answer.
func metadataModeIn(env []string, mode string) bool {
	want := "SCION_METADATA_MODE=" + mode
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// checkHostNetworkMetadata decides whether an agent may start given its
// network mode and GCP metadata mode. Under host networking the agent shares
// the host's network namespace, so the in-container metadata interception
// cannot be installed:
//
//   - assign is refused: the assigned identity would be served on a host
//     loopback port that other agents and host processes can reach.
//   - block starts, with a warning returned for the user, because block is
//     the default and host networking is the default for a loopback hub,
//     unless the broker's host network block policy refuses it (see
//     hostNetworkBlockPolicy: by default on a detected cloud host).
//
// It returns the user-facing warning (empty when there is none) or the
// refusal error.
func checkHostNetworkMetadata(agentName, runtimeName, networkMode string, env []string) (string, error) {
	if networkMode != hostNetworkMode || !appliesContainerNetworkMode(runtimeName) {
		return "", nil
	}
	if metadataModeIn(env, "assign") {
		return "", &hostNetworkRefusal{reason: hostNetworkRefusalAssignUnsupported,
			msg: fmt.Sprintf("cannot start agent %q: GCP metadata mode \"assign\" is not supported under host networking; %s", agentName, hostNetworkMetadataFix)}
	}
	if metadataModeIn(env, "block") {
		return hostNetworkBlockPolicy(agentName, hostNetworkMetadataWarning)
	}
	return "", nil
}

// metadataInterceptionGranted reports whether the container gets the
// in-container metadata interception (NET_ADMIN, and the marker that tells
// sciontool the interception is required): block or assign mode, on a runtime
// that applies container capabilities, and never under host networking,
// where the capability would apply to the host's network namespace.
func metadataInterceptionGranted(runtimeName, networkMode string, env []string) bool {
	if networkMode == hostNetworkMode || !appliesContainerNetworkMode(runtimeName) {
		return false
	}
	return hasMetadataInterception(env)
}
