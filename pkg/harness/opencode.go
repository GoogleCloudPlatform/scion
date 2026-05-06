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
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	opencodeEmbeds "github.com/GoogleCloudPlatform/scion/pkg/harness/opencode"
)

type OpenCode struct{}

func (o *OpenCode) Name() string {
	return "opencode"
}

func (o *OpenCode) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{
		Harness: "opencode",
		Limits: api.HarnessLimitCapabilities{
			MaxTurns:      api.CapabilityField{Support: api.SupportNo, Reason: "This harness has no hook dialect for turn events"},
			MaxModelCalls: api.CapabilityField{Support: api.SupportNo, Reason: "This harness has no hook dialect for model events"},
			MaxDuration:   api.CapabilityField{Support: api.SupportYes},
		},
		Telemetry: api.HarnessTelemetryCapabilities{
			EnabledConfig: api.CapabilityField{Support: api.SupportYes},
			NativeEmitter: api.CapabilityField{Support: api.SupportNo, Reason: "Native telemetry forwarding is not wired for this harness"},
		},
		Prompts: api.HarnessPromptCapabilities{
			SystemPrompt:      api.CapabilityField{Support: api.SupportPartial, Reason: "System prompt is downgraded into AGENTS.md"},
			AgentInstructions: api.CapabilityField{Support: api.SupportYes},
		},
		Auth: api.HarnessAuthCapabilities{
			APIKey:   api.CapabilityField{Support: api.SupportYes},
			AuthFile: api.CapabilityField{Support: api.SupportYes},
			VertexAI: api.CapabilityField{Support: api.SupportNo, Reason: "Vertex AI auth is not supported for this harness"},
		},
		Resume: api.CapabilityField{Support: api.SupportYes},
	}
}

func (o *OpenCode) GetEnv(agentName string, agentHome string, unixUsername string) map[string]string {
	return map[string]string{}
}

func (o *OpenCode) GetCommand(task string, resume bool, baseArgs []string) []string {
	args := []string{"opencode"}
	if resume {
		args = append(args, "--continue")
	} else {
		args = append(args, "--prompt")
		if task != "" {
			args = append(args, task)
		}
	}

	args = append(args, baseArgs...)
	return args
}
func (o *OpenCode) DefaultConfigDir() string {
	return ".config/opencode"
}

func (o *OpenCode) SkillsDir() string {
	return ".config/opencode/skills"
}

func (o *OpenCode) HasSystemPrompt(agentHome string) bool {
	return false
}

func (o *OpenCode) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	return nil
}

func (o *OpenCode) GetEmbedDir() string {
	return "opencode"
}

func (o *OpenCode) GetInterruptKey() string {
	return "C-c"
}

func (o *OpenCode) GetHarnessEmbedsFS() (embed.FS, string) {
	return opencodeEmbeds.EmbedsFS, "embeds"
}

func (o *OpenCode) GetTelemetryEnv() map[string]string {
	// OpenCode telemetry env var injection is deferred.
	return nil
}

func (o *OpenCode) InjectAgentInstructions(agentHome string, content []byte) error {
	target := filepath.Join(agentHome, "AGENTS.md")
	return os.WriteFile(target, content, 0644)
}

func (o *OpenCode) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	// Explicit selection support
	if auth.SelectedType != "" {
		switch auth.SelectedType {
		case "api-key":
			key := auth.AnthropicAPIKey
			if key == "" {
				key = auth.OpenAIAPIKey
			}
			if key == "" {
				return nil, fmt.Errorf("opencode: auth type %q selected but no API key found; set ANTHROPIC_API_KEY or OPENAI_API_KEY", auth.SelectedType)
			}
			envKey := "ANTHROPIC_API_KEY"
			if auth.AnthropicAPIKey == "" {
				envKey = "OPENAI_API_KEY"
			}
			return &api.ResolvedAuth{
				Method:  "api-key",
				EnvVars: map[string]string{envKey: key},
			}, nil
		case "auth-file":
			if auth.OpenCodeAuthFile == "" {
				return nil, fmt.Errorf("opencode: auth type %q selected but no auth file found; expected ~/.local/share/opencode/auth.json", auth.SelectedType)
			}
			return &api.ResolvedAuth{
				Method: "auth-file",
				Files: []api.FileMapping{
					{SourcePath: auth.OpenCodeAuthFile, ContainerPath: "~/.local/share/opencode/auth.json"},
				},
			}, nil
		default:
			return nil, fmt.Errorf("opencode: unknown auth type %q; valid types are: api-key, auth-file", auth.SelectedType)
		}
	}

	// Auto-detect preference order: AnthropicAPIKey → OpenAIAPIKey → OpenCodeAuthFile → error

	if auth.AnthropicAPIKey != "" {
		return &api.ResolvedAuth{
			Method: "api-key",
			EnvVars: map[string]string{
				"ANTHROPIC_API_KEY": auth.AnthropicAPIKey,
			},
		}, nil
	}

	if auth.OpenAIAPIKey != "" {
		return &api.ResolvedAuth{
			Method: "api-key",
			EnvVars: map[string]string{
				"OPENAI_API_KEY": auth.OpenAIAPIKey,
			},
		}, nil
	}

	if auth.OpenCodeAuthFile != "" {
		return &api.ResolvedAuth{
			Method: "auth-file",
			Files: []api.FileMapping{
				{
					SourcePath:    auth.OpenCodeAuthFile,
					ContainerPath: "~/.local/share/opencode/auth.json",
				},
			},
		}, nil
	}

	return nil, fmt.Errorf("opencode: no valid auth method found; set ANTHROPIC_API_KEY or OPENAI_API_KEY, or provide auth credentials at ~/.local/share/opencode/auth.json")
}

func (o *OpenCode) InjectSystemPrompt(agentHome string, content []byte) error {
	// OpenCode has no native system prompt support — downgrade by prepending to AGENTS.md
	agentsPath := filepath.Join(agentHome, "AGENTS.md")
	header := fmt.Sprintf("# System Prompt\n\n%s\n\n---\n\n", string(content))

	existing, err := os.ReadFile(agentsPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read existing agent instructions: %w", err)
	}

	merged := []byte(header)
	if len(existing) > 0 {
		merged = append(merged, existing...)
	}
	return os.WriteFile(agentsPath, merged, 0644)
}
