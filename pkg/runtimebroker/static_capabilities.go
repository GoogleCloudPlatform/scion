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

package runtimebroker

import (
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// StaticCapabilities are the capabilities this broker binary reports for a
// default runtime rt. They are fixed by the binary and the runtime, so a
// broker sends them at registration (StaticCapabilityNames, on the
// registration and join) as well as on every heartbeat: the Hub then knows
// them before the first heartbeat and does not refuse a create with a
// spurious 412 in between (ptone/scion#2918).
//
// StartsInFlight is set only when the heartbeat actually carries the
// starts-in-flight list, so it is not part of this static set.
func StaticCapabilities(rt scionrt.Runtime) *hubclient.BrokerCapabilities {
	return &hubclient.BrokerCapabilities{
		WebPTY:                 false,
		Sync:                   true,
		Attach:                 scionrt.HasAttachSupport(rt),
		Reprovision:            true,
		AsyncLaunch:            true,
		EmptyPerAgentWorkspace: scionrt.HasEmptyPerAgentSupport(rt),
		// This broker honours localOnly deletes and confirms a moved
		// agent's NFS workspace before provisioning it (agent move).
		AgentMove: true,
		// This broker's reprovision reuses an empty-per-agent workspace in
		// place (miller79/scion#167).
		ReprovisionEmptyPerAgent: true,
	}
}

// StaticCapabilityNames is StaticCapabilities as the capability names the
// registration and join requests carry (the names the Hub's
// capabilitiesFromStrings accepts).
func StaticCapabilityNames(rt scionrt.Runtime) []string {
	c := StaticCapabilities(rt)
	var names []string
	add := func(on bool, name string) {
		if on {
			names = append(names, name)
		}
	}
	add(c.WebPTY, "webPty")
	add(c.Sync, "sync")
	add(c.Attach, "attach")
	add(c.Reprovision, "reprovision")
	add(c.AsyncLaunch, "asyncLaunch")
	add(c.EmptyPerAgentWorkspace, "emptyPerAgentWorkspace")
	add(c.AgentMove, "agentMove")
	add(c.ReprovisionEmptyPerAgent, "reprovisionEmptyPerAgent")
	return names
}
