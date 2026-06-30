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

package managedagent

import "io"

// CreateAgentConfig contains the parameters for creating a managed agent.
type CreateAgentConfig struct {
	ID                string
	BaseAgent         string
	SystemInstruction string
	Description       string
	Tools             []ToolConfig
	Environment       *EnvironmentConfig
}

// InteractionRequest contains the parameters for creating an interaction.
// Either CloudAgentID or AgentName should be set; AgentName is the base
// agent identifier used when no cloud-side agent config exists.
type InteractionRequest struct {
	CloudAgentID          string
	AgentName             string
	Model                 string
	Input                 string
	PreviousInteractionID string
	EnvironmentID         string
	Environment           *EnvironmentConfig
	Stream                bool
	Background            bool
	Tools                 []ToolConfig
	SystemInstruction     string
}

// InteractionHandle represents a running or completed interaction.
type InteractionHandle struct {
	InteractionID string
	EnvironmentID string
	Status        InteractionStatus
	Steps         []Step
	OutputText    string
	Usage         *UsageInfo
	EventStream   io.ReadCloser
}

// InteractionState is the polled state of an interaction.
type InteractionState struct {
	InteractionID string
	Status        InteractionStatus
	Steps         []Step
	OutputText    string
	EnvironmentID string
	Usage         *UsageInfo
}

// InteractionStatus represents the status of an interaction.
type InteractionStatus string

const (
	StatusInProgress     InteractionStatus = "in_progress"
	StatusRequiresAction InteractionStatus = "requires_action"
	StatusCompleted      InteractionStatus = "completed"
	StatusFailed         InteractionStatus = "failed"
	StatusCancelled      InteractionStatus = "cancelled"
	StatusIncomplete     InteractionStatus = "incomplete"
)

// Step represents a single step in an interaction.
type Step struct {
	Type      string
	Text      string
	Arguments string
	ToolName  string
}

// ToolConfig defines a tool available to the agent.
type ToolConfig struct {
	Type       string
	Name       string
	Parameters map[string]interface{}
}

// EnvironmentConfig describes the execution environment for a managed agent.
type EnvironmentConfig struct {
	Type    string
	Sources []SourceConfig
	Network *NetworkConfig
}

// SourceConfig describes a source to mount into the agent environment.
type SourceConfig struct {
	Type   string
	URI    string
	Branch string
	Path   string
}

// NetworkConfig describes network access rules for the agent environment.
type NetworkConfig struct {
	Disabled  bool
	Allowlist []AllowlistEntry
}

// AllowlistEntry is a domain and optional header injection for egress rules.
type AllowlistEntry struct {
	Domain  string
	Headers map[string]string
}

// UsageInfo contains token usage statistics for an interaction.
type UsageInfo struct {
	TotalInputTokens  int
	TotalOutputTokens int
	TotalTokens       int
}
