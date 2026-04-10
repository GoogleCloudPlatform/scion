package hub

import (
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	agentActionStatus            = "status"
	agentActionStart             = "start"
	agentActionStop              = "stop"
	agentActionRestart           = "restart"
	agentActionMessage           = "message"
	agentActionMessages          = "messages"
	agentActionExec              = "exec"
	agentActionRestore           = "restore"
	agentActionEnv               = "env"
	agentActionTokenRefresh      = "token/refresh"
	agentActionRefreshToken      = "refresh-token"
	agentActionOutboundMessage   = "outbound-message"
	agentActionMessageLogs       = "message-logs"
	agentActionMessageLogsStream = "message-logs/stream"
)

var errNoRuntimeBrokerAssigned = errors.New("agent has no runtime broker assigned")

func requireRuntimeBrokerAssigned(agent *store.Agent) error {
	if agent.RuntimeBrokerID == "" {
		return errNoRuntimeBrokerAssigned
	}
	return nil
}
