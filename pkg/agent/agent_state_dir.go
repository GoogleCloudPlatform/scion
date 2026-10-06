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

// AgentStateDir is the exported form of agentStateDir, the single resolution
// of an agent's broker-side state directory that GetAgent and ProvisionAgent
// use. It returns the directory and the effective shared-workspace flag (true
// also when the request does not say so but the agent's state is stored in
// the external agents root). strict is broker mode or a Hub-supplied project
// ID, as for withAgentStateDir.
//
// Callers that read the agent's state outside GetAgent — the broker's runtime
// classification (ProvisionedProfile) and Start's provenance pre-check — use
// it so they read the same record GetAgent will (ptone/scion#1799).
func AgentStateDir(projectDir, agentName string, sharedWorkspace bool, hubProjectID string, strict bool) (dir string, shared bool, err error) {
	return agentStateDir(projectDir, agentName, sharedWorkspace, hubProjectID, strict)
}
