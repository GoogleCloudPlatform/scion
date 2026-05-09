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
	AgentActionFinalizeEnv       = "finalize-env"
)

// RuntimeBrokerAgentActionMethod returns the HTTP method for actions routed
// through runtimebroker handleAgentAction. It intentionally does not cover
// every agent action defined in this package.
func RuntimeBrokerAgentActionMethod(action string) (string, bool) {
	switch action {
	case AgentActionLogs, AgentActionStats, AgentActionHasPrompt:
		return http.MethodGet, true
	case AgentActionStart, AgentActionStop, AgentActionSuspend, AgentActionRestart, AgentActionMessage, AgentActionExec, AgentActionFinalizeEnv:
		return http.MethodPost, true
	default:
		return "", false
	}
}
