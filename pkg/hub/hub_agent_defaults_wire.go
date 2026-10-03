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

package hub

import "github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"

// remoteHubAgentDefaults converts the hub's operational agent_defaults section
// into the wire form sent to a runtime broker, or nil when the hub has no
// limit/resource or auto-expose defaults to send.
//
// Only the four limit/resource fields cross here. default_template and
// default_harness_config are resolved hub-side (they need ID/hash stamping) and
// travel on the AppliedConfig ladder instead — see design §3.2.2.
//
// 🔴 The nil-when-empty return is load-bearing for file-mode parity.
// BuildLayer1SnapshotFromFile deliberately leaves the agent-defaults fields
// zero (design §3.2.4), because in file mode a co-located broker reads the same
// settings.yaml and applies these values itself at the BOTTOM of its own chain.
// Sending them from the hub as well would promote them from that bottom tier to
// the hub tier and silently outrank broker profile resources and template
// limits in deployments that have always behaved the other way. Because the
// snapshot is empty in file mode, the four limit/resource fields are zero
// there, so the broker-side limit rung never fires — no file-mode branch
// needed anywhere. That is rejected alternative A7. With no auto-expose
// default either, this returns nil and the wire field is omitted.
//
// autoExposePorts is the hub's auto-expose-ports default. Unlike the four
// limit/resource fields it is sent in file mode too: no broker reads it from
// its own settings.yaml, so the hub is its only source, and it lands at the
// broker's lowest env tier (buildAgentEnv's defaultEnv) in every mode.
func remoteHubAgentDefaults(d opsettings.AgentDefaultsSettings, autoExposePorts *bool) *RemoteHubAgentDefaults {
	if d.DefaultMaxTurns == 0 && d.DefaultMaxModelCalls == 0 &&
		d.DefaultMaxDuration == "" && d.DefaultResources == nil && autoExposePorts == nil {
		return nil
	}
	out := &RemoteHubAgentDefaults{
		MaxTurns:        d.DefaultMaxTurns,
		MaxModelCalls:   d.DefaultMaxModelCalls,
		MaxDuration:     d.DefaultMaxDuration,
		AutoExposePorts: copyBoolPtr(autoExposePorts),
	}
	if d.DefaultResources != nil {
		// Copy the pointee: hubAgentDefaults() already returns a deep copy, but
		// a second alias to a caller-owned spec on a struct that outlives this
		// call is the kind of thing a later edit turns into a data race.
		rs := *d.DefaultResources
		out.Resources = &rs
	}
	return out
}

// startHubAgentDefaults is the hub-defaults wire value for a start or restart
// dispatch: the auto-expose default only, or nil when the hub has none. The
// limit/resource fields stay create/provision-only, so a start never changes
// which tier supplies them.
func startHubAgentDefaults(autoExposePorts *bool) *RemoteHubAgentDefaults {
	if autoExposePorts == nil {
		return nil
	}
	return &RemoteHubAgentDefaults{AutoExposePorts: copyBoolPtr(autoExposePorts)}
}

func copyBoolPtr(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}
