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
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// onAgentHalted runs once for each halt, on the hub instance whose write
// moved the agent into phase halted (the store reports it through
// AgentStatusResult, or the container-missing CAS returns a halted row).
// haltedFrom is the phase the write would otherwise have stored, when it
// differs from the agent's previous phase, or "".
func (s *Server) onAgentHalted(ctx context.Context, agent *store.Agent, haltedFrom string) {
	s.agentLifecycleLog.Warn("agent halted by the platform",
		"agent_id", agent.ID, "agent", agent.Slug, "project_id", agent.ProjectID,
		"exit_reason", agent.ExitReason, "halted_from", haltedFrom)
}
