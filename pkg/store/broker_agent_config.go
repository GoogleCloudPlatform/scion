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

package store

// BrokerAgentConfig is the part of a Runtime Broker's agent answer that
// records what the broker resolved for the agent: template, harness config
// and auth, image, profile and runtime. A synchronous dispatch reads it from
// the broker's response; an async launch reads it from the succeeded launch
// report (ptone/scion#4818). Both apply it with ApplyBrokerAgentConfig, so
// the two paths record the same values.
type BrokerAgentConfig struct {
	Template      string
	HarnessConfig string
	HarnessAuth   string
	// HarnessConfigSource is empty only from an older broker (a current
	// broker reports "unresolved" when no harness-config dir resolved).
	HarnessConfigSource string
	Image               string
	Profile             string
	Runtime             string
}

// ApplyBrokerAgentConfig copies the broker-resolved values in c onto a. An
// empty value keeps the recorded one. The AppliedConfig fields are written
// only when a has an AppliedConfig, and no profile is written onto a pinned
// (flat) agent.
func ApplyBrokerAgentConfig(a *Agent, c BrokerAgentConfig) {
	if c.Template != "" {
		a.Template = c.Template
	}
	if a.AppliedConfig != nil {
		if c.HarnessConfig != "" {
			a.AppliedConfig.HarnessConfig = c.HarnessConfig
		}
		if c.HarnessAuth != "" {
			a.AppliedConfig.HarnessAuth = c.HarnessAuth
		}
		if c.HarnessConfigSource != "" {
			a.AppliedConfig.HarnessConfigSource = c.HarnessConfigSource
		}
		// AppliedConfig.Image records what the broker actually resolved,
		// for display. It never feeds a later dispatch's top-tier image
		// (the hub's explicitDispatchImage reads the explicit inputs
		// instead), so recording it does not freeze the image across
		// restarts (ptone/scion#1799).
		if c.Image != "" {
			a.AppliedConfig.Image = c.Image
		}
		if c.Profile != "" && !a.IsPinned() {
			a.AppliedConfig.Profile = c.Profile
		}
	}
	if c.Runtime != "" {
		a.Runtime = c.Runtime
	}
}
