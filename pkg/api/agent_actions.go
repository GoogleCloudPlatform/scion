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
	// (pkg/runtimebroker/handlers.go). The Hub-side action-dispatch switches
	// (handlers_agents_core.go, handlers_projects_core.go) do not yet have a
	// case for this constant — that is task 2.1/2.2's job — so a live public
	// POST /api/v1/agents/{id}/keys request still 404s at the Hub today; only
	// the internal Hub-to-broker route this constant also names is reachable
	// once 1.2 implements a Dispatcher/BrokerClient that calls it.
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
