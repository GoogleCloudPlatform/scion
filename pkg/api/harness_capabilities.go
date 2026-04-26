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

// SupportLevel captures whether a harness supports a specific advanced field.
type SupportLevel string

const (
	SupportNo      SupportLevel = "no"
	SupportPartial SupportLevel = "partial"
	SupportYes     SupportLevel = "yes"
)

// CapabilityField describes support status and optional context.
type CapabilityField struct {
	Support SupportLevel `json:"support" yaml:"support"`
	Reason  string       `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// HarnessLimitCapabilities describes support for run limits.
type HarnessLimitCapabilities struct {
	MaxTurns      CapabilityField `json:"max_turns" yaml:"max_turns"`
	MaxModelCalls CapabilityField `json:"max_model_calls" yaml:"max_model_calls"`
	MaxDuration   CapabilityField `json:"max_duration" yaml:"max_duration"`
}

// HarnessTelemetryCapabilities describes support for telemetry controls.
type HarnessTelemetryCapabilities struct {
	EnabledConfig CapabilityField `json:"enabled" yaml:"enabled"`
	NativeEmitter CapabilityField `json:"native_emitter" yaml:"native_emitter"`
}

// HarnessPromptCapabilities describes support for prompt-related fields.
type HarnessPromptCapabilities struct {
	SystemPrompt      CapabilityField `json:"system_prompt" yaml:"system_prompt"`
	AgentInstructions CapabilityField `json:"agent_instructions" yaml:"agent_instructions"`
}

// HarnessAuthCapabilities describes support for auth mode selections.
type HarnessAuthCapabilities struct {
	APIKey     CapabilityField `json:"api_key" yaml:"api_key"`
	AuthFile   CapabilityField `json:"auth_file" yaml:"auth_file"`
	OAuthToken CapabilityField `json:"oauth_token" yaml:"oauth_token"`
	VertexAI   CapabilityField `json:"vertex_ai" yaml:"vertex_ai"`
}

// HarnessAdvancedCapabilities describes advanced field support for a harness.
type HarnessAdvancedCapabilities struct {
	Harness   string                       `json:"harness" yaml:"harness,omitempty"`
	Limits    HarnessLimitCapabilities     `json:"limits" yaml:"limits"`
	Telemetry HarnessTelemetryCapabilities `json:"telemetry" yaml:"telemetry"`
	Prompts   HarnessPromptCapabilities    `json:"prompts" yaml:"prompts"`
	Auth      HarnessAuthCapabilities      `json:"auth" yaml:"auth"`
}
