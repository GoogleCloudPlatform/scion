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

package hub

import (
	"context"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func canonicalHarnessName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude":
		return "claude"
	case "gemini":
		return "gemini"
	case "opencode":
		return "opencode"
	case "codex":
		return "codex"
	case "generic":
		return "generic"
	default:
		return ""
	}
}

func (s *Server) resolveHarnessTypeFromConfigRef(ctx context.Context, projectID, configRef string) string {
	configRef = strings.TrimSpace(configRef)
	if configRef == "" {
		return ""
	}
	if h := canonicalHarnessName(configRef); h != "" {
		return h
	}

	if projectID != "" {
		hc, err := s.store.GetHarnessConfigBySlug(ctx, configRef, store.HarnessConfigScopeProject, projectID)
		if err == nil && hc != nil {
			if h := canonicalHarnessName(hc.Harness); h != "" {
				return h
			}
			if inferred := inferHarnessFromName(hc.Harness); inferred != "" {
				return inferred
			}
		}
	}

	hc, err := s.store.GetHarnessConfigBySlug(ctx, configRef, store.HarnessConfigScopeGlobal, "")
	if err == nil && hc != nil {
		if h := canonicalHarnessName(hc.Harness); h != "" {
			return h
		}
		if inferred := inferHarnessFromName(hc.Harness); inferred != "" {
			return inferred
		}
	}

	if inferred := inferHarnessFromName(configRef); inferred != "" {
		return inferred
	}
	return ""
}

func (s *Server) resolveAgentHarnessType(ctx context.Context, agent *store.Agent) string {
	if agent == nil {
		return "generic"
	}

	if agent.AppliedConfig != nil && agent.AppliedConfig.InlineConfig != nil {
		if h := canonicalHarnessName(agent.AppliedConfig.InlineConfig.Harness); h != "" {
			return h
		}
	}

	var refs []string
	if agent.AppliedConfig != nil {
		refs = append(refs, agent.AppliedConfig.HarnessConfig)
		if agent.AppliedConfig.InlineConfig != nil {
			refs = append(refs, agent.AppliedConfig.InlineConfig.HarnessConfig)
		}
	}
	refs = append(refs, agent.HarnessConfig)

	for _, ref := range refs {
		if h := s.resolveHarnessTypeFromConfigRef(ctx, agent.ProjectID, ref); h != "" {
			return h
		}
	}

	return "generic"
}

func (s *Server) resolveAgentHarnessCapabilities(ctx context.Context, agent *store.Agent) (string, api.HarnessAdvancedCapabilities) {
	resolvedHarness := s.resolveAgentHarnessType(ctx, agent)
	return resolvedHarness, harness.New(resolvedHarness).AdvancedCapabilities()
}

func supportReason(field api.CapabilityField) string {
	if field.Reason != "" {
		return field.Reason
	}
	return "Unsupported for this harness"
}

func validateConfigAgainstHarnessCapabilities(cfg *api.ScionConfig, caps api.HarnessAdvancedCapabilities) map[string]interface{} {
	if cfg == nil {
		return nil
	}

	issues := map[string]interface{}{}

	if cfg.MaxTurns > 0 && caps.Limits.MaxTurns.Support == api.SupportNo {
		issues["max_turns"] = supportReason(caps.Limits.MaxTurns)
	}
	if cfg.MaxModelCalls > 0 && caps.Limits.MaxModelCalls.Support == api.SupportNo {
		issues["max_model_calls"] = supportReason(caps.Limits.MaxModelCalls)
	}
	if strings.TrimSpace(cfg.MaxDuration) != "" && caps.Limits.MaxDuration.Support == api.SupportNo {
		issues["max_duration"] = supportReason(caps.Limits.MaxDuration)
	}

	if strings.TrimSpace(cfg.SystemPrompt) != "" && caps.Prompts.SystemPrompt.Support == api.SupportNo {
		issues["system_prompt"] = supportReason(caps.Prompts.SystemPrompt)
	}
	if strings.TrimSpace(cfg.AgentInstructions) != "" && caps.Prompts.AgentInstructions.Support == api.SupportNo {
		issues["agent_instructions"] = supportReason(caps.Prompts.AgentInstructions)
	}

	if cfg.AuthSelectedType != "" {
		switch cfg.AuthSelectedType {
		case "api-key":
			if caps.Auth.APIKey.Support == api.SupportNo {
				issues["auth_selectedType"] = supportReason(caps.Auth.APIKey)
			}
		case "oauth-token":
			if caps.Auth.OAuthToken.Support == api.SupportNo {
				issues["auth_selectedType"] = supportReason(caps.Auth.OAuthToken)
			}
		case "auth-file":
			if caps.Auth.AuthFile.Support == api.SupportNo {
				issues["auth_selectedType"] = supportReason(caps.Auth.AuthFile)
			}
		case "vertex-ai":
			if caps.Auth.VertexAI.Support == api.SupportNo {
				issues["auth_selectedType"] = supportReason(caps.Auth.VertexAI)
			}
		default:
			issues["auth_selectedType"] = "Unknown auth type"
		}
	}

	if len(issues) == 0 {
		return nil
	}
	return issues
}
