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

package brokeridentity

import (
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Activation acknowledgement phases (.design/flat-runtime-brokers-contract.md
// section 6 R9/R10).
const (
	PhaseRegister = "register"
	PhaseJoin     = "join"
	PhaseEmbedded = "embedded"
	PhaseActivate = "activate"
)

// AckBinding is a Runtime Broker ID plus runtime target binding, as expected
// from the identity or as acknowledged by the Hub.
type AckBinding struct {
	RuntimeBrokerID string                       `json:"runtimeBrokerId"`
	RuntimeTarget   *api.RuntimeTargetDescriptor `json:"runtimeTarget,omitempty"`
}

// AckError is an activation acknowledgement refusal. Code is
// api.ErrCodeRuntimeTargetAckMissing or api.ErrCodeRuntimeTargetBindingConflict.
type AckError struct {
	Code            string     `json:"code"`
	Phase           string     `json:"phase"`
	RuntimeBrokerID string     `json:"runtimeBrokerId"`
	Expected        AckBinding `json:"expected"`
	Got             AckBinding `json:"got"`
}

func (e *AckError) Error() string {
	switch e.Code {
	case api.ErrCodeRuntimeTargetAckMissing:
		return fmt.Sprintf("%s (%s): the Hub did not acknowledge the runtime target binding for Runtime Broker %s; "+
			"it does not support flat Runtime Brokers. Finish the Hub rollout (every replica, with hub.flat_runtime_brokers on) "+
			"before enabling this instance", e.Code, e.Phase, e.RuntimeBrokerID)
	default:
		return fmt.Sprintf("%s (%s): the Hub acknowledged a different binding for Runtime Broker %s (expected %s, got %s); refusing to activate",
			e.Code, e.Phase, e.RuntimeBrokerID, bindingString(e.Expected), bindingString(e.Got))
	}
}

func bindingString(b AckBinding) string {
	if b.RuntimeTarget == nil {
		return b.RuntimeBrokerID + "/<none>"
	}
	return b.RuntimeBrokerID + "/" + b.RuntimeTarget.ID + "(" + b.RuntimeTarget.Type + ")"
}

// CheckActivationAck validates a Hub acknowledgement (or, on the embedded
// path, the stored row) against the persisted identity. Evaluation order is
// frozen: (1) a different Runtime Broker ID is a binding conflict; (2) else a
// missing runtime target is a missing acknowledgement; (3) else a different
// target ID or type is a binding conflict. It returns nil only for a bound
// result.
func CheckActivationAck(id *Identity, phase, brokerID string, target *api.RuntimeTargetDescriptor) error {
	expectedTarget := id.RuntimeTarget
	e := &AckError{
		Phase:           phase,
		RuntimeBrokerID: id.RuntimeBrokerID,
		Expected:        AckBinding{RuntimeBrokerID: id.RuntimeBrokerID, RuntimeTarget: &expectedTarget},
		Got:             AckBinding{RuntimeBrokerID: brokerID, RuntimeTarget: target},
	}
	switch {
	case brokerID != id.RuntimeBrokerID:
		e.Code = api.ErrCodeRuntimeTargetBindingConflict
	case target == nil:
		e.Code = api.ErrCodeRuntimeTargetAckMissing
	case target.ID != id.RuntimeTarget.ID || target.Type != id.RuntimeTarget.Type:
		e.Code = api.ErrCodeRuntimeTargetBindingConflict
	default:
		return nil
	}
	return e
}
