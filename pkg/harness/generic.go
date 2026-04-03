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
)

type Generic struct{}

func (g *Generic) Name() string {
	return "generic"
}

func (g *Generic) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{
		Harness: "generic",
		Limits: api.HarnessLimitCapabilities{
			MaxTurns:      api.CapabilityField{Support: api.SupportNo, Reason: "This harness has no hook dialect for turn events"},
			MaxModelCalls: api.CapabilityField{Support: api.SupportNo, Reason: "This harness has no hook dialect for model events"},
			MaxDuration:   api.CapabilityField{Support: api.SupportYes},
		},
		Telemetry: api.HarnessTelemetryCapabilities{
			EnabledConfig: api.CapabilityField{Support: api.SupportYes},
			NativeEmitter: api.CapabilityField{Support: api.SupportNo, Reason: "Native telemetry forwarding is not defined for generic harnesses"},
		},
		Prompts: api.HarnessPromptCapabilities{
			SystemPrompt:      api.CapabilityField{Support: api.SupportPartial, Reason: "Saved under .scion/system_prompt.md, without harness-native behavior"},
			AgentInstructions: api.CapabilityField{Support: api.SupportYes},
		},
		Auth: api.HarnessAuthCapabilities{
			APIKey:   api.CapabilityField{Support: api.SupportYes},
			AuthFile: api.CapabilityField{Support: api.SupportYes},
			VertexAI: api.CapabilityField{Support: api.SupportYes},
		},
	}
}

func (g *Generic) GetEnv(agentName string, agentHome string, unixUsername string) map[string]string {
	env := make(map[string]string)
	env["SCION_AGENT_NAME"] = agentName
	return env
}

func (g *Generic) GetCommand(task string, resume bool, baseArgs []string) []string {
	args := append([]string{}, baseArgs...)
	if task != "" {
		args = append(args, task)
	}
	return args
}

func (g *Generic) DefaultConfigDir() string {
	return ".scion"
}

func (g *Generic) SkillsDir() string {
	return ".scion/skills"
}

func (g *Generic) HasSystemPrompt(agentHome string) bool {
	return false
}

func (g *Generic) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	return nil
}

func (g *Generic) GetEmbedDir() string {
	return ""
}

func (g *Generic) GetInterruptKey() string {
	return "C-c"
}

func (g *Generic) GetHarnessEmbedsFS() (embed.FS, string) {
	return embed.FS{}, ""
}

func (g *Generic) GetTelemetryEnv() map[string]string {
	return nil
}

func (g *Generic) InjectAgentInstructions(agentHome string, content []byte) error {
	target := filepath.Join(agentHome, "agents.md")
	return os.WriteFile(target, content, 0644)
}

func (g *Generic) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	// Generic harness is a passthrough — map all available creds, never error.
	result := &api.ResolvedAuth{
		Method:  "passthrough",
		EnvVars: make(map[string]string),
	}

	if auth.AnthropicAPIKey != "" {
		result.EnvVars["ANTHROPIC_API_KEY"] = auth.AnthropicAPIKey
	}
	if auth.GeminiAPIKey != "" {
		result.EnvVars["GEMINI_API_KEY"] = auth.GeminiAPIKey
	}
	if auth.GoogleAPIKey != "" {
		result.EnvVars["GOOGLE_API_KEY"] = auth.GoogleAPIKey
	}
	if auth.OpenAIAPIKey != "" {
		result.EnvVars["OPENAI_API_KEY"] = auth.OpenAIAPIKey
	}
	if auth.CodexAPIKey != "" {
		result.EnvVars["CODEX_API_KEY"] = auth.CodexAPIKey
	}
	if auth.GoogleCloudProject != "" {
		result.EnvVars["GOOGLE_CLOUD_PROJECT"] = auth.GoogleCloudProject
	}
	if auth.GoogleCloudRegion != "" {
		result.EnvVars["GOOGLE_CLOUD_REGION"] = auth.GoogleCloudRegion
	}

	if auth.GoogleAppCredentials != "" {
		adcContainerPath := "~/.config/gcloud/application_default_credentials.json"
		result.Files = append(result.Files, api.FileMapping{
			SourcePath:    auth.GoogleAppCredentials,
			ContainerPath: adcContainerPath,
		})
	}
	if auth.OAuthCreds != "" {
		result.Files = append(result.Files, api.FileMapping{
			SourcePath:    auth.OAuthCreds,
			ContainerPath: "~/.scion/oauth_creds.json",
		})
	}
	if auth.CodexAuthFile != "" {
		result.Files = append(result.Files, api.FileMapping{
			SourcePath:    auth.CodexAuthFile,
			ContainerPath: "~/.codex/auth.json",
		})
	}
	if auth.OpenCodeAuthFile != "" {
		result.Files = append(result.Files, api.FileMapping{
			SourcePath:    auth.OpenCodeAuthFile,
			ContainerPath: "~/.local/share/opencode/auth.json",
		})
	}

	return result, nil
}

func (g *Generic) InjectSystemPrompt(agentHome string, content []byte) error {
	target := filepath.Join(agentHome, ".scion", "system_prompt.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("failed to create directory for system prompt: %w", err)
	}
	return os.WriteFile(target, content, 0644)
}
