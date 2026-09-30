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

package api

import "net/http"

const (
	AgentActionStatus            = "status"
	AgentActionStart             = "start"
	AgentActionStop              = "stop"
	AgentActionSuspend           = "suspend"
	AgentActionRestart           = "restart"
	AgentActionMessage           = "message"
	AgentActionMessages          = "messages"
	AgentActionMessagesStream    = "messages/stream"
	AgentActionExec              = "exec"
	AgentActionRestore           = "restore"
	AgentActionEnv               = "env"
	AgentActionTokenRefresh      = "token/refresh"
	AgentActionRefreshToken      = "refresh-token"
	AgentActionOutboundMessage   = "outbound-message"
	AgentActionMessageLogs       = "message-logs"
	AgentActionMessageLogsStream = "message-logs/stream"
	AgentActionLogs              = "logs"
	AgentActionStats             = "stats"
	AgentActionHasPrompt         = "has-prompt"
	AgentActionResetAuth         = "reset-auth"
	AgentActionMetrics           = "metrics"
	AgentActionSetMessageMode    = "set_message_mode"
	AgentActionReincarnate       = "reincarnate"

	// AgentActionKeys names the dedicated terminal-keystroke-injection route
	// (POST /api/v1/agents/{id}/keys and the project-scoped equivalent),
	// frozen by the agent-keys contract (.design/agent-keys-contract.md,
	// ptone/scion#2191). It is a route/action name, not an independently
	// grantable authorization permission — see agentActionPermission in
	// pkg/hub/authorize.go, which maps it explicitly to ActionAttach.
	//
	// Added to RuntimeBrokerAgentActionMethod below by task 1.1, which wires
	// the runtime broker's own dedicated keys handler
	// (pkg/runtimebroker/handlers.go), with the Hub-to-broker route reachable
	// via the Dispatcher/BrokerClient task 1.2 added. On the Hub side, both
	// action-dispatch functions (handlers_agents_core.go's
	// handleAgentAction, handlers_projects_core.go's
	// handleProjectAgentAction) route this action early, straight to
	// authorizeAgentKeys, the same way they already do for message/
	// reincarnate/set_message_mode — a live POST /api/v1/agents/{id}/keys
	// request no longer falls through to the generic authz block that
	// resolves agentActionPermission for other actions. Neither
	// action-dispatch switch has a case that calls a real keys handler yet
	// (task 2.2 adds one), so an authorized call still 404s, not because it
	// was denied. This constant exists so every task that touches the keys
	// route (1.1, 1.2, 2.1, 2.2) references a single frozen action name
	// rather than each inventing their own string.
	AgentActionKeys = "keys"
)

// RuntimeBrokerAgentActionMethod returns the HTTP method for actions routed
// through runtimebroker handleAgentAction. It intentionally does not cover
// every agent action defined in this package.
func RuntimeBrokerAgentActionMethod(action string) (string, bool) {
	switch action {
	case AgentActionLogs, AgentActionStats, AgentActionHasPrompt:
		return http.MethodGet, true
	case AgentActionStart, AgentActionStop, AgentActionSuspend, AgentActionRestart, AgentActionMessage, AgentActionExec, AgentActionResetAuth, AgentActionKeys:
		return http.MethodPost, true
	default:
		return "", false
	}
}
