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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	codexEmbeds "github.com/GoogleCloudPlatform/scion/pkg/harness/codex"
)

type Codex struct{}

func (c *Codex) Name() string {
	return "codex"
}

func (c *Codex) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{
		Harness: "codex",
		Limits: api.HarnessLimitCapabilities{
			MaxTurns:      api.CapabilityField{Support: api.SupportNo, Reason: "This harness has no hook dialect for turn events"},
			MaxModelCalls: api.CapabilityField{Support: api.SupportNo, Reason: "This harness has no hook dialect for model events"},
			MaxDuration:   api.CapabilityField{Support: api.SupportYes},
		},
		Telemetry: api.HarnessTelemetryCapabilities{
			EnabledConfig: api.CapabilityField{Support: api.SupportYes},
			NativeEmitter: api.CapabilityField{Support: api.SupportYes},
		},
		Prompts: api.HarnessPromptCapabilities{
			SystemPrompt:      api.CapabilityField{Support: api.SupportNo, Reason: "System prompt injection is not implemented for this harness"},
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

func (c *Codex) GetEnv(agentName string, agentHome string, unixUsername string) map[string]string {
	return map[string]string{}
}

func (c *Codex) GetCommand(task string, resume bool, baseArgs []string) []string {
	args := []string{"codex", "--sandbox", "danger-full-access", "--dangerously-bypass-approvals-and-sandbox"}
	if resume {
		args = append(args, "resume", "--last")
	} else {
		if task != "" {
			args = append(args, task)
		}
	}

	args = append(args, baseArgs...)
	return args
}

func (c *Codex) DefaultConfigDir() string {
	return ".codex"
}

func (c *Codex) SkillsDir() string {
	return ".codex/skills"
}

func (c *Codex) HasSystemPrompt(agentHome string) bool {
	return false
}

func (c *Codex) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	scionAgentPath := filepath.Join(agentDir, "scion-agent.json")

	var telemetryCfg *api.TelemetryConfig
	if data, err := os.ReadFile(scionAgentPath); err == nil {
		var cfg api.ScionConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("failed to parse scion-agent.json: %w", err)
		}
		telemetryCfg = cfg.Telemetry
	}

	return c.ApplyTelemetrySettings(agentHome, telemetryCfg, nil)
}

func (c *Codex) GetEmbedDir() string {
	return "codex"
}

func (c *Codex) GetInterruptKey() string {
	return "C-c"
}

func (c *Codex) GetHarnessEmbedsFS() (embed.FS, string) {
	return codexEmbeds.EmbedsFS, "embeds"
}

func (c *Codex) GetTelemetryEnv() map[string]string {
	// Codex uses a TOML config file for telemetry, not env vars.
	// File-based injection is handled via ResolveAuth.
	return nil
}

func (c *Codex) ApplyTelemetrySettings(agentHome string, telemetry *api.TelemetryConfig, env map[string]string) error {
	return c.reconcileConfig(agentHome, telemetry, env)
}

func (c *Codex) InjectAgentInstructions(agentHome string, content []byte) error {
	dir := filepath.Join(agentHome, ".codex")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create .codex directory: %w", err)
	}
	target := filepath.Join(dir, "AGENTS.md")
	return os.WriteFile(target, content, 0644)
}

func (c *Codex) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	// Explicit selection support
	if auth.SelectedType != "" {
		switch auth.SelectedType {
		case "api-key":
			key := auth.CodexAPIKey
			if key == "" {
				key = auth.OpenAIAPIKey
			}
			if key == "" {
				return nil, fmt.Errorf("codex: auth type %q selected but no API key found; set CODEX_API_KEY or OPENAI_API_KEY", auth.SelectedType)
			}
			envKey := "CODEX_API_KEY"
			if auth.CodexAPIKey == "" {
				envKey = "OPENAI_API_KEY"
			}
			return &api.ResolvedAuth{
				Method:  "api-key",
				EnvVars: map[string]string{envKey: key},
			}, nil
		case "auth-file":
			if auth.CodexAuthFile == "" {
				return nil, fmt.Errorf("codex: auth type %q selected but no auth file found; expected ~/.codex/auth.json", auth.SelectedType)
			}
			return &api.ResolvedAuth{
				Method: "auth-file",
				Files: []api.FileMapping{
					{SourcePath: auth.CodexAuthFile, ContainerPath: "~/.codex/auth.json"},
				},
			}, nil
		default:
			return nil, fmt.Errorf("codex: unknown auth type %q; valid types are: api-key, auth-file", auth.SelectedType)
		}
	}

	// Auto-detect preference order: CodexAPIKey → OpenAIAPIKey → CodexAuthFile → error

	if auth.CodexAPIKey != "" {
		return &api.ResolvedAuth{
			Method: "api-key",
			EnvVars: map[string]string{
				"CODEX_API_KEY": auth.CodexAPIKey,
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

	if auth.CodexAuthFile != "" {
		return &api.ResolvedAuth{
			Method: "auth-file",
			Files: []api.FileMapping{
				{
					SourcePath:    auth.CodexAuthFile,
					ContainerPath: "~/.codex/auth.json",
				},
			},
		}, nil
	}

	return nil, fmt.Errorf("codex: no valid auth method found; set CODEX_API_KEY or OPENAI_API_KEY, or provide auth credentials at ~/.codex/auth.json")
}

func (c *Codex) ApplyAuthSettings(agentHome string, resolved *api.ResolvedAuth) error {
	if resolved == nil || resolved.Method != "api-key" {
		return nil
	}

	// Extract the API key from whichever env var was resolved.
	var apiKey string
	for _, k := range []string{"CODEX_API_KEY", "OPENAI_API_KEY"} {
		if v := resolved.EnvVars[k]; v != "" {
			apiKey = v
			break
		}
	}
	if apiKey == "" {
		return nil
	}

	codexDir := filepath.Join(agentHome, ".codex")
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		return fmt.Errorf("failed to create .codex directory: %w", err)
	}

	authData := map[string]string{
		"auth_mode":      "apikey",
		"OPENAI_API_KEY": apiKey,
	}
	data, err := json.MarshalIndent(authData, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal auth.json: %w", err)
	}
	authPath := filepath.Join(codexDir, "auth.json")
	return os.WriteFile(authPath, append(data, '\n'), 0600)
}

func (c *Codex) InjectSystemPrompt(agentHome string, content []byte) error {
	// TODO: Codex has no native system prompt support. System prompt injection is
	// not yet implemented for this harness.
	return nil
}
