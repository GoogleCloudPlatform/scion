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

package google

// CreateAgentRequest is the request body for POST /v1beta/agents.
// BaseEnvironment is polymorphic: a string ("remote" or an environment ID)
// or an *Environment config object.
type CreateAgentRequest struct {
	ID                string      `json:"id"`
	BaseAgent         string      `json:"base_agent"`
	SystemInstruction string      `json:"system_instruction,omitempty"`
	Description       string      `json:"description,omitempty"`
	Tools             []AgentTool `json:"tools,omitempty"`
	BaseEnvironment   interface{} `json:"base_environment,omitempty"`
}

// Agent is the response resource from the Agents API.
type Agent struct {
	ID                string       `json:"id"`
	BaseAgent         string       `json:"base_agent"`
	SystemInstruction string       `json:"system_instruction,omitempty"`
	Description       string       `json:"description,omitempty"`
	Tools             []AgentTool  `json:"tools,omitempty"`
	BaseEnvironment   *Environment `json:"base_environment,omitempty"`
}

// AgentTool describes a tool available to the agent.
type AgentTool struct {
	Type       string                 `json:"type"`
	Name       string                 `json:"name,omitempty"`
	Parameters map[string]interface{} `json:"parameters,omitempty"`
}

// ListAgentsResponse is the response body for GET /v1beta/agents.
type ListAgentsResponse struct {
	Agents        []Agent `json:"agents"`
	NextPageToken string  `json:"next_page_token,omitempty"`
}

// CreateInteractionRequest is the request body for POST /v1beta/interactions.
// Environment is polymorphic: a string ("remote" or an environment ID)
// or an *Environment config object.
type CreateInteractionRequest struct {
	Agent                 string      `json:"agent,omitempty"`
	Model                 string      `json:"model,omitempty"`
	Input                 string      `json:"input"`
	SystemInstruction     string      `json:"system_instruction,omitempty"`
	Tools                 []AgentTool `json:"tools,omitempty"`
	Environment           interface{} `json:"environment,omitempty"`
	PreviousInteractionID string      `json:"previous_interaction_id,omitempty"`
	Stream                bool        `json:"stream,omitempty"`
	Background            bool        `json:"background,omitempty"`
	Store                 *bool       `json:"store,omitempty"`
}

// Interaction is the response resource from the Interactions API.
type Interaction struct {
	ID            string            `json:"id"`
	Agent         string            `json:"agent,omitempty"`
	Model         string            `json:"model,omitempty"`
	Status        string            `json:"status"`
	Steps         []InteractionStep `json:"steps,omitempty"`
	OutputText    string            `json:"output_text,omitempty"`
	EnvironmentID string            `json:"environment_id,omitempty"`
	Usage         *InteractionUsage `json:"usage,omitempty"`
}

// InteractionStep is a single step in an interaction's execution.
// Arguments appears in polled responses; ArgumentsDelta in streaming deltas.
type InteractionStep struct {
	Type           string `json:"type"`
	Text           string `json:"text,omitempty"`
	Arguments      string `json:"arguments,omitempty"`
	ArgumentsDelta string `json:"arguments_delta,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
}

// InteractionUsage contains token usage statistics.
type InteractionUsage struct {
	TotalInputTokens   int `json:"total_input_tokens"`
	TotalOutputTokens  int `json:"total_output_tokens"`
	TotalCachedTokens  int `json:"total_cached_tokens,omitempty"`
	TotalThoughtTokens int `json:"total_thought_tokens,omitempty"`
	TotalToolUseTokens int `json:"total_tool_use_tokens,omitempty"`
	TotalTokens        int `json:"total_tokens"`
}

// Environment describes the execution environment configuration.
type Environment struct {
	Type    string         `json:"type,omitempty"`
	Sources []SourceConfig `json:"sources,omitempty"`
	Network *NetworkConfig `json:"network,omitempty"`
}

// SourceConfig describes a source to mount into the environment.
type SourceConfig struct {
	Type   string `json:"type"`
	URI    string `json:"uri,omitempty"`
	Branch string `json:"branch,omitempty"`
	Path   string `json:"path,omitempty"`
}

// NetworkConfig describes network access rules.
type NetworkConfig struct {
	Disabled  bool             `json:"disabled,omitempty"`
	Allowlist []AllowlistEntry `json:"allowlist,omitempty"`
}

// AllowlistEntry is a domain and optional header injection for egress.
type AllowlistEntry struct {
	Domain  string            `json:"domain"`
	Headers map[string]string `json:"headers,omitempty"`
}

// StepDeltaEvent is the data payload for step.delta SSE events.
type StepDeltaEvent struct {
	Type           string `json:"type,omitempty"`
	Text           string `json:"text,omitempty"`
	ArgumentsDelta string `json:"arguments_delta,omitempty"`
}

// StepStartEvent is the data payload for step.start SSE events.
type StepStartEvent struct {
	Type     string `json:"type"`
	ToolName string `json:"tool_name,omitempty"`
}

// APIError is an error response from the Google API.
type APIError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}
