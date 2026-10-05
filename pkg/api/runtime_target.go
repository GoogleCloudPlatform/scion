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

import "fmt"

// RuntimeTargetDescriptor is the non-sensitive registration descriptor of a
// flat Runtime Broker's single runtime target (.design/flat-runtime-brokers-contract.md
// section 6). ID is an opaque, stable identifier minted by the Runtime
// Broker instance; it is not an inventory target key (see GLOSSARY.md).
type RuntimeTargetDescriptor struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"displayName,omitempty"`
}

// Wire error codes shared by the Hub and the Runtime Broker for flat
// Runtime Broker requests. pkg/hub and pkg/runtimebroker alias these.
const (
	// ErrCodeRuntimeTargetMismatch (409): the request's expected runtime
	// target differs from the one the Runtime Broker serves.
	ErrCodeRuntimeTargetMismatch = "runtime_target_mismatch"
	// ErrCodeRuntimeProfileUnsupported (422): a Runtime Broker Profile was
	// sent to a flat Runtime Broker.
	ErrCodeRuntimeProfileUnsupported = "runtime_profile_unsupported"
	// ErrCodeRuntimeTargetRequired (412): a flat Runtime Broker received a
	// create without expectedRuntimeTargetId.
	ErrCodeRuntimeTargetRequired = "runtime_target_required"
)

// Instance-side (non-HTTP) error codes reported by a flat Runtime Broker
// instance at startup or activation.
const (
	// ErrCodeRuntimeTargetAckMissing: the Hub did not acknowledge the
	// runtime target binding (unsupported protocol/feature).
	ErrCodeRuntimeTargetAckMissing = "runtime_target_ack_missing"
	// ErrCodeRuntimeTargetBindingConflict: the Hub acknowledged a different
	// Runtime Broker ID or runtime target.
	ErrCodeRuntimeTargetBindingConflict = "runtime_target_binding_conflict"
	// ErrCodeFlatRuntimeBrokerRemoteUnsupported: server.broker.instances is
	// configured without the Hub in the same process.
	ErrCodeFlatRuntimeBrokerRemoteUnsupported = "flat_runtime_broker_remote_unsupported"
	// ErrCodeFlatRuntimeBrokerNotRegistered: a remote flat instance has no
	// instance-scoped credentials.
	ErrCodeFlatRuntimeBrokerNotRegistered = "flat_runtime_broker_not_registered"
)

// RuntimeTargetMismatch describes an expected-target mismatch. Its fields are
// the frozen details keys of the runtime_target_mismatch envelope.
type RuntimeTargetMismatch struct {
	RuntimeBrokerID         string `json:"runtimeBrokerId"`
	ExpectedRuntimeTargetID string `json:"expectedRuntimeTargetId"`
	ActualRuntimeTargetID   string `json:"actualRuntimeTargetId"`
}

// Message returns the frozen human-readable mismatch message.
func (m *RuntimeTargetMismatch) Message() string {
	actual := m.ActualRuntimeTargetID
	if actual == "" {
		actual = "no runtime target"
	}
	return fmt.Sprintf("Runtime Broker %s serves runtime target %s, but the request expected %s",
		m.RuntimeBrokerID, actual, m.ExpectedRuntimeTargetID)
}

// Details returns the envelope details map for the mismatch.
func (m *RuntimeTargetMismatch) Details() map[string]interface{} {
	return map[string]interface{}{
		"runtimeBrokerId":         m.RuntimeBrokerID,
		"expectedRuntimeTargetId": m.ExpectedRuntimeTargetID,
		"actualRuntimeTargetId":   m.ActualRuntimeTargetID,
	}
}

// CheckExpectedRuntimeTarget compares a request's expected runtime target with
// the one a Runtime Broker actually serves (actual is "" for a legacy
// Runtime Broker). It returns nil when expected is empty or equal to actual,
// and the mismatch otherwise. It is pure; callers build their envelopes from
// the result.
func CheckExpectedRuntimeTarget(runtimeBrokerID, actual, expected string) *RuntimeTargetMismatch {
	if expected == "" || expected == actual {
		return nil
	}
	return &RuntimeTargetMismatch{
		RuntimeBrokerID:         runtimeBrokerID,
		ExpectedRuntimeTargetID: expected,
		ActualRuntimeTargetID:   actual,
	}
}
