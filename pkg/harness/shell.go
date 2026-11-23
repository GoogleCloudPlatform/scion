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

package harness

import (
	"context"
	"embed"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Shell is the built-in harness for user-specified execution containers
// (ptone/scion#8812). It runs a respawning login shell with no LLM harness,
// no LLM credentials, no message delivery, and transitions directly to idle.
type Shell struct{}

func (s *Shell) Name() string {
	return config.ShellHarnessConfigName
}

func (s *Shell) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	noLLM := "Execution container runs a login shell with no LLM harness"
	return api.HarnessAdvancedCapabilities{
		Harness: config.ShellHarnessConfigName,
		Limits: api.HarnessLimitCapabilities{
			MaxTurns:      api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			MaxModelCalls: api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			MaxDuration:   api.CapabilityField{Support: api.SupportYes},
		},
		Telemetry: api.HarnessTelemetryCapabilities{
			EnabledConfig: api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			NativeEmitter: api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
		},
		Prompts: api.HarnessPromptCapabilities{
			SystemPrompt:      api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			AgentInstructions: api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
		},
		Auth: api.HarnessAuthCapabilities{
			APIKey:     api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			AuthFile:   api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			OAuthToken: api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			VertexAI:   api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
		},
		MCP: api.HarnessMCPCapabilities{
			Stdio:          api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			SSE:            api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			StreamableHTTP: api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
			ProjectScope:   api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
		},
		Resume:       api.CapabilityField{Support: api.SupportYes},
		AgentPlugins: api.CapabilityField{Support: api.SupportNo, Reason: noLLM},
	}
}

func (s *Shell) GetEnv(agentName string, _ string, _ string) map[string]string {
	return map[string]string{
		"SCION_AGENT_NAME": agentName,
	}
}

// GetCommand runs a respawning login shell so typing "exit" gives a fresh shell
// instead of terminating tmux window 0. Task and args are ignored.
func (s *Shell) GetCommand(_ string, _ bool, _ []string) []string {
	return []string{"sh", "-c", "while :; do bash -l; done"}
}

func (s *Shell) DefaultConfigDir() string {
	return ".scion"
}

func (s *Shell) SkillsDir() string {
	return ""
}

func (s *Shell) HasSystemPrompt(_ string) bool {
	return false
}

func (s *Shell) Provision(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (s *Shell) GetInterruptKey() string {
	return "C-c"
}

func (s *Shell) GetInterruptSequence() []string {
	return nil
}

// DeliverySpec disables message delivery (defence in depth for S5).
func (s *Shell) DeliverySpec() api.DeliverySpec {
	return api.DeliverySpec{Disabled: true}
}

// ReadySpec transitions the container from initializing directly to idle.
func (s *Shell) ReadySpec() api.ReadySpec {
	return config.DefaultReadySpec(config.ReadyStrategyIdle)
}

func (s *Shell) GetHarnessEmbedsFS() (embed.FS, string) {
	return embed.FS{}, ""
}

func (s *Shell) GetTelemetryEnv() map[string]string {
	return nil
}

func (s *Shell) InjectAgentInstructions(_ string, _ []byte) error {
	return nil
}

func (s *Shell) InjectSystemPrompt(_ string, _ []byte) error {
	return nil
}

// ResolveAuth uses NoAuth semantics: no LLM credentials are gathered or injected.
func (s *Shell) ResolveAuth(_ api.AuthConfig) (*api.ResolvedAuth, error) {
	return &api.ResolvedAuth{
		Method:  "none",
		EnvVars: map[string]string{},
	}, nil
}
