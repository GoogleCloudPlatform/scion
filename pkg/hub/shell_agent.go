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

import (
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// isShellAgent reports whether agent uses the reserved built-in "shell"
// harness config (config.ShellHarnessConfigName).
//
// AppliedConfig.HarnessConfig is the authoritative persisted field on
// store.Agent (written on create/update and mirrored into the indexed
// harness_config column). When AppliedConfig is nil or its HarnessConfig is
// empty, isShellAgent falls back to the enriched Agent.HarnessConfig field.
func isShellAgent(agent *store.Agent) bool {
	if agent == nil {
		return false
	}
	if agent.AppliedConfig != nil && agent.AppliedConfig.HarnessConfig != "" {
		return agent.AppliedConfig.HarnessConfig == config.ShellHarnessConfigName
	}
	return agent.HarnessConfig == config.ShellHarnessConfigName
}
