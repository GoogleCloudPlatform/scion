package api

import "net/http"

const (
	AgentActionStatus            = "status"
	AgentActionStart             = "start"
	AgentActionStop              = "stop"
	AgentActionRestart           = "restart"
	AgentActionMessage           = "message"
	AgentActionMessages          = "messages"
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
	case AgentActionStart, AgentActionStop, AgentActionRestart, AgentActionMessage, AgentActionExec, AgentActionFinalizeEnv:
		return http.MethodPost, true
	default:
		return "", false
	}
}
