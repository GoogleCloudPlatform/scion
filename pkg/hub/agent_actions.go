package hub

import (
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var errNoRuntimeBrokerAssigned = errors.New("agent has no runtime broker assigned")

func requireRuntimeBrokerAssigned(agent *store.Agent) error {
	if agent.RuntimeBrokerID == "" {
		return errNoRuntimeBrokerAssigned
	}
	return nil
}
