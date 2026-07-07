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

import (
	"context"
	"embed"
)

// Harness interface defines the methods a harness must implement
type Harness interface {
	Name() string
	AdvancedCapabilities() HarnessAdvancedCapabilities
	GetEnv(agentName string, agentHome string, unixUsername string) map[string]string
	GetCommand(task string, resume bool, baseArgs []string) []string
	DefaultConfigDir() string
	SkillsDir() string
	HasSystemPrompt(agentHome string) bool

	// Provision performs harness-specific setup during agent creation.
	// This is called after templates are copied and scion-agent.json is written.
	// agentDir is the directory containing scion-agent.json (which may differ
	// from filepath.Dir(agentHome) when split storage is active).
	Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error

	// GetInterruptKey returns the single key used to interrupt the harness process (e.g., "C-c" or "Escape").
	GetInterruptKey() string

	// GetInterruptSequence returns the sequence of keys to send to interrupt the
	// harness, each as a separate tmux send-keys call. When interrupt_signal is
	// "sequence" (or interrupt_sequence is populated), this takes priority over
	// GetInterruptKey. Returns nil to fall back to GetInterruptKey.
	GetInterruptSequence() []string

	// GetHarnessEmbedsFS returns the embedded filesystem containing default harness-config files
	// and the base path within it (e.g., "embeds").
	GetHarnessEmbedsFS() (embed.FS, string)

	// InjectAgentInstructions places agent instructions content into the harness's
	// expected location within the agent home directory.
	InjectAgentInstructions(agentHome string, content []byte) error

	// InjectSystemPrompt delivers system prompt content. Harnesses with native system
	// prompt support write to their expected location. Harnesses without it merge the
	// content into agent instructions (downgrade).
	InjectSystemPrompt(agentHome string, content []byte) error

	// GetTelemetryEnv returns harness-specific environment variables that direct
	// the harness's native telemetry output to the local OTLP collector
	// (localhost:4317). These are injected only when telemetry is enabled.
	GetTelemetryEnv() map[string]string

	// ResolveAuth selects the single best authentication method from a populated
	// AuthConfig and returns the env vars and file mappings needed to inject
	// credentials into the agent container. Returns an error with an actionable
	// message if no valid auth method is available.
	ResolveAuth(auth AuthConfig) (*ResolvedAuth, error)
}

// AuthSettingsApplier is an optional interface that harnesses can implement
// to update their native settings files after auth resolution. This is called
// at agent start time, after ResolveAuth, to ensure harness-specific config
// (e.g. Gemini's settings.json selectedType) reflects the resolved auth method.
type AuthSettingsApplier interface {
	ApplyAuthSettings(agentHome string, resolved *ResolvedAuth) error
}

// TelemetrySettingsApplier is an optional interface that harnesses can
// implement to reconcile native telemetry settings files after effective
// telemetry config (settings/template/CLI overrides) is resolved at start time.
type TelemetrySettingsApplier interface {
	ApplyTelemetrySettings(agentHome string, telemetry *TelemetryConfig, env map[string]string) error
}

// MCPSettingsApplier is an optional interface that harnesses can implement to
// stage or apply universal MCP server config (defined once in scion-agent.yaml's
// mcp_servers block) into the harness's native MCP configuration. Container-script
// harnesses stage inputs/mcp-servers.json for the in-container provision.py to
// translate; built-in harnesses without script provisioners do not implement
// this interface and continue using inline MCP config in their home/ files.
type MCPSettingsApplier interface {
	ApplyMCPSettings(agentHome string, mcpServers map[string]MCPServerConfig) error
}
