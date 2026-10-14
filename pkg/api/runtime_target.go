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
	if m.ActualRuntimeTargetID == "" {
		return fmt.Sprintf("Runtime Broker %s serves no runtime target, but the request expected %s",
			m.RuntimeBrokerID, m.ExpectedRuntimeTargetID)
	}
	return fmt.Sprintf("Runtime Broker %s serves runtime target %s, but the request expected %s",
		m.RuntimeBrokerID, m.ActualRuntimeTargetID, m.ExpectedRuntimeTargetID)
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

// PinnedRuntimeTarget is the read-only API view of an agent's pinned
// placement on a flat Runtime Broker (.design/flat-runtime-brokers-contract.md
// section 8): the runtime target the agent was pinned to and the Runtime
// Broker that serves it. It is computed from stored data for responses and is
// never written through the API.
type PinnedRuntimeTarget struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	RuntimeBrokerID string `json:"runtimeBrokerId"`
}

// RuntimeTargetPresentationSchemaVersion is the only
// RuntimeTargetPresentation.SchemaVersion this build knows. A Hub stores
// only presentations of a version it knows.
const RuntimeTargetPresentationSchemaVersion = 1

// RuntimeTargetPresentation is the publishable form of a flat Runtime
// Broker instance entry, sent by the instance on every heartbeat and shown
// read-only by the Hub (ptone/scion#3282). It is
// built only from config.PresentInstance (config.FlatInstancePresentation.API),
// and apart from SchemaVersion its field set equals
// config.FlatInstancePresentation's, which is the "published" column of the
// frozen contract table FC4-a. Fields FC4-a never publishes (context,
// kubeconfig, service-account policy, home_storage_leaf, volume source
// layout, secret targets, environment values) have no field here, so the Hub
// cannot store them even if a Runtime Broker sent them.
//
// It is the instance's reported defaults for new agents. It is not the
// effective configuration of any agent, and not evidence of current
// credential or cluster health. It is never used for placement, admission
// or dispatch: the stored runtime target descriptor and the agent pin stay
// the authority.
type RuntimeTargetPresentation struct {
	// SchemaVersion is RuntimeTargetPresentationSchemaVersion. It is not an
	// FC4-a field.
	SchemaVersion int                     `json:"schemaVersion"`
	Key           string                  `json:"key"`
	Name          string                  `json:"name"`
	Target        *PresentedTarget        `json:"target,omitempty"`
	AgentDefaults *PresentedAgentDefaults `json:"agentDefaults,omitempty"`
}

// PresentedTarget is the publishable part of an instance's runtime_target.
// SafeToEvict keeps unset (nil), false and true distinct.
type PresentedTarget struct {
	Type                     string            `json:"type"`
	DisplayName              string            `json:"displayName,omitempty"`
	Namespace                string            `json:"namespace,omitempty"`
	GKE                      bool              `json:"gke,omitempty"`
	PriorityClassName        string            `json:"priorityClassName,omitempty"`
	SharedDirStorageClass    string            `json:"sharedDirStorageClass,omitempty"`
	SharedDirSize            string            `json:"sharedDirSize,omitempty"`
	SafeToEvict              *bool             `json:"safeToEvict,omitempty"`
	SharedDirStorageBackend  string            `json:"sharedDirStorageBackend,omitempty"`
	SharedDirStorageBackends map[string]string `json:"sharedDirStorageBackends,omitempty"`
	HomeStorageBackend       string            `json:"homeStorageBackend,omitempty"`
}

// PresentedAgentDefaults is the publishable part of an instance's
// agent_defaults.
type PresentedAgentDefaults struct {
	DefaultHarnessConfig string                              `json:"defaultHarnessConfig,omitempty"`
	ImageRegistry        string                              `json:"imageRegistry,omitempty"`
	CloneDepth           CloneDepth                          `json:"cloneDepth,omitempty"`
	Resources            *ResourceSpec                       `json:"resources,omitempty"`
	Volumes              []PresentedVolume                   `json:"volumes,omitempty"`
	Secrets              []PresentedSecretDecl               `json:"secrets,omitempty"`
	HarnessOverrides     map[string]PresentedHarnessOverride `json:"harnessOverrides,omitempty"`
}

// PresentedVolume is the publishable part of a volume mount: its mount
// target, whether it is read-only, and its kind.
type PresentedVolume struct {
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	Type     string `json:"type,omitempty"`
}

// PresentedSecretDecl is the publishable part of a secret declaration. A
// declaration never carries a value.
type PresentedSecretDecl struct {
	Key         string `json:"key"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

// PresentedHarnessOverride is the publishable part of one harness override.
// Its environment is published as the sorted variable names only.
type PresentedHarnessOverride struct {
	Image            string            `json:"image,omitempty"`
	ImagePullPolicy  string            `json:"imagePullPolicy,omitempty"`
	AuthSelectedType string            `json:"authSelectedType,omitempty"`
	User             string            `json:"user,omitempty"`
	EnvKeys          []string          `json:"envKeys,omitempty"`
	Volumes          []PresentedVolume `json:"volumes,omitempty"`
	Resources        *ResourceSpec     `json:"resources,omitempty"`
}
