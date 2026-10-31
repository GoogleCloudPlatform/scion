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

package config

import "strings"

// agentHostAliasHosts are the host names that, inside an agent container, name
// the container's own host and never a network host: the Docker and Podman
// host-gateway aliases (Podman's is also the Apple container DNS rule), and
// the alias a co-located broker maps to the Docker host gateway for
// bridge-networked agents (colocatedHubHostAlias in cmd).
var agentHostAliasHosts = map[string]struct{}{
	"host.docker.internal":     {},
	"host.containers.internal": {},
	"scion-hub.internal":       {},
}

// IsAgentHostAliasHost reports whether host is one of the fixed host-alias
// names a local container runtime or the broker maps to the agent
// container's own host. The match is on the literal name, ignoring case and
// a trailing dot; the name is never resolved, and an IP literal is never an
// alias. It is the host-local name list shared by the broker's check of the
// hub endpoint given to an agent and the agent-side CLI's exemption for that
// endpoint.
func IsAgentHostAliasHost(host string) bool {
	_, ok := agentHostAliasHosts[strings.TrimSuffix(strings.ToLower(host), ".")]
	return ok
}
