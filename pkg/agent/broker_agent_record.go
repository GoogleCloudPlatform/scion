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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The helpers below read an existing agent's settings from the broker-side
// agent dir (AgentStateDir): scion-agent.json, written by provisioning next
// to the image provenance record. That dir is outside every container mount
// (the runtime mounts only its home and workspace subdirectories, and masks
// the in-project agents root wherever a wider tree is mounted), so these
// reads never depend on agent-info.json, which lives in the agent home.

// brokerAgentDir resolves agentName's broker-side agent dir as the broker's
// start path does (strict). ok is false when the project or the dir cannot
// be resolved.
func brokerAgentDir(projectPath, agentName string, sharedWorkspace bool, hubProjectID string) (string, bool) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", false
	}
	agentDir, _, err := AgentStateDir(projectDir, agentName, sharedWorkspace, hubProjectID, true)
	if err != nil {
		return "", false
	}
	return agentDir, true
}

// IsProvisioned reports whether agentName has been provisioned on this
// broker: its broker-side agent dir holds a scion-agent config.
func IsProvisioned(projectPath, agentName string, sharedWorkspace bool, hubProjectID string) bool {
	agentDir, ok := brokerAgentDir(projectPath, agentName, sharedWorkspace, hubProjectID)
	return ok && config.ScionAgentConfigExists(agentDir)
}

// PersistedHarnessConfig returns the harness-config name recorded in
// agentName's scion-agent config in the broker-side agent dir, or "" when
// there is none or it cannot be read. It is the "stored-config" tier Start
// resolves the harness-config name from (config.ResolveHarnessConfigName).
func PersistedHarnessConfig(projectPath, agentName string, sharedWorkspace bool, hubProjectID string) string {
	agentDir, ok := brokerAgentDir(projectPath, agentName, sharedWorkspace, hubProjectID)
	if !ok || !config.ScionAgentConfigExists(agentDir) {
		return ""
	}
	cfg, err := (&config.Template{Path: agentDir}).LoadConfig()
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.HarnessConfig
}
