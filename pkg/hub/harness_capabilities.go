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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func canonicalHarnessName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude":
		return "claude"
	case "gemini", "gemini-cli":
		return "gemini-cli"
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
	return s.resolveHarnessTypeFromConfigRefMemo(ctx, projectID, configRef, nil)
}

// harnessTypeMemo memoizes harness-config lookups for one request. A
// project-scope result depends on the project and the reference, a
// global-scope result on the reference alone, so a global harness-config
// shared by agents in many projects is read once, not once per project.
type harnessTypeMemo struct {
	project map[harnessRefKey]string
	global  map[string]string
}

// harnessRefKey identifies one project-scope harness-config lookup.
type harnessRefKey struct{ projectID, ref string }

func newHarnessTypeMemo() *harnessTypeMemo {
	return &harnessTypeMemo{project: map[harnessRefKey]string{}, global: map[string]string{}}
}

// projectTable and globalTable return the memo's tables, nil for a nil memo.
func (m *harnessTypeMemo) projectTable() map[harnessRefKey]string {
	if m == nil {
		return nil
	}
	return m.project
}

func (m *harnessTypeMemo) globalTable() map[string]string {
	if m == nil {
		return nil
	}
	return m.global
}

// memoized returns table[key], calling load and storing its result on a
// miss. A nil table (no memo) always calls load and stores nothing.
func memoized[K comparable](table map[K]string, key K, load func() string) string {
	if table == nil {
		return load()
	}
	if h, ok := table[key]; ok {
		return h
	}
	h := load()
	table[key] = h
	return h
}

// harnessTypeOfConfig returns the harness type a stored harness-config
// names, or "" for none (also for a lookup error or a missing record).
func harnessTypeOfConfig(hc *store.HarnessConfig, err error) string {
	if err != nil || hc == nil {
		return ""
	}
	if h := canonicalHarnessName(hc.Harness); h != "" {
		return h
	}
	if inferred := inferHarnessFromName(hc.Harness); inferred != "" {
		return inferred
	}
	return hc.Harness
}

// resolveHarnessTypeFromConfigRefMemo is resolveHarnessTypeFromConfigRef
// with an optional per-request memo (nil = no memo).
func (s *Server) resolveHarnessTypeFromConfigRefMemo(ctx context.Context, projectID, configRef string, memo *harnessTypeMemo) string {
	configRef = strings.TrimSpace(configRef)
	if configRef == "" {
		return ""
	}
	if h := canonicalHarnessName(configRef); h != "" {
		return h
	}

	if projectID != "" {
		h := memoized(memo.projectTable(), harnessRefKey{projectID, configRef}, func() string {
			return harnessTypeOfConfig(s.store.GetHarnessConfigBySlug(ctx, configRef, store.HarnessConfigScopeProject, projectID))
		})
		if h != "" {
			return h
		}
	}

	h := memoized(memo.globalTable(), configRef, func() string {
		return harnessTypeOfConfig(s.store.GetHarnessConfigBySlug(ctx, configRef, store.HarnessConfigScopeGlobal, ""))
	})
	if h != "" {
		return h
	}

	return inferHarnessFromName(configRef)
}

// resolveAgentHarnessType resolves the agent's harness type live: inline
// harness, then its harness-config references. It does not read the
// harness stamped at create (AppliedConfig.Harness), which every
// harnessVendor (lists, events and the single-agent view, through
// agentBadgeHarness) prefers; the two agree unless a harness-config's
// harness type is changed in the catalog after the agent was created.
func (s *Server) resolveAgentHarnessType(ctx context.Context, agent *store.Agent) string {
	return s.resolveAgentHarnessTypeMemo(ctx, agent, nil)
}

// inlineHarnessType returns the harness named by ac's inline config,
// canonicalized, or "".
func inlineHarnessType(ac *store.AgentAppliedConfig) string {
	if ac == nil || ac.InlineConfig == nil || ac.InlineConfig.Harness == "" {
		return ""
	}
	if h := canonicalHarnessName(ac.InlineConfig.Harness); h != "" {
		return h
	}
	return ac.InlineConfig.Harness
}

// resolveAgentHarnessTypeMemo is resolveAgentHarnessType with an optional
// per-request memo of harness-config lookups (nil = no memo). With a memo,
// agents that share a harness-config reference cost one lookup chain
// between them, not one each.
func (s *Server) resolveAgentHarnessTypeMemo(ctx context.Context, agent *store.Agent, memo *harnessTypeMemo) string {
	if agent == nil {
		return "generic"
	}

	if h := inlineHarnessType(agent.AppliedConfig); h != "" {
		return h
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
		if h := s.resolveHarnessTypeFromConfigRefMemo(ctx, agent.ProjectID, ref, memo); h != "" {
			return h
		}
	}

	return "generic"
}

// storedAgentHarness returns the agent's harness type from its own record,
// without a store lookup: the harness stamped at create
// (AppliedConfig.Harness), else an inline harness, canonicalized as
// resolveAgentHarnessType does. It returns "" when the record alone does
// not name it, which is the case for an agent created before the stamp
// that names its harness only through a harness-config reference.
func storedAgentHarness(agent *store.Agent) string {
	if agent == nil || agent.AppliedConfig == nil {
		return ""
	}
	if agent.AppliedConfig.Harness != "" {
		return agent.AppliedConfig.Harness
	}
	return inlineHarnessType(agent.AppliedConfig)
}

// stampAgentHarness records the agent's harness type on
// AppliedConfig.Harness at create, unless already set: an inline harness
// first (as resolveAgentHarnessType), then the harness of resolvedHC, the
// hub harness-config create resolved and stamped by ID (the one the broker
// hydrates, possibly from the template default), then
// resolveAgentHarnessType. A harness that does not resolve (the "generic"
// fallback) is not stamped, so a later read resolves it again.
func (s *Server) stampAgentHarness(ctx context.Context, agent *store.Agent, resolvedHC *store.HarnessConfig) {
	if agent == nil || agent.AppliedConfig == nil || agent.AppliedConfig.Harness != "" {
		return
	}
	h := inlineHarnessType(agent.AppliedConfig)
	if h == "" {
		h = harnessTypeOfConfig(resolvedHC, nil)
	}
	if h == "" {
		h = s.resolveAgentHarnessType(ctx, agent)
	}
	if h != "generic" {
		agent.AppliedConfig.Harness = h
	}
}

// agentBadgeHarness returns the harness the single-agent view derives its
// vendor from: the record's harness (storedAgentHarness) when it names one,
// as lists and events do, else the live resolved harness. So all three
// agree for every agent.
func agentBadgeHarness(agent *store.Agent, resolvedHarness string) string {
	if h := storedAgentHarness(agent); h != "" {
		return h
	}
	return resolvedHarness
}

// agentModelSize returns the model size alias recorded on the agent at
// create (AppliedConfig.ModelSize), or "".
func agentModelSize(agent *store.Agent) string {
	if agent == nil || agent.AppliedConfig == nil {
		return ""
	}
	return agent.AppliedConfig.ModelSize
}

// setAgentListBadges sets HarnessVendor and ModelSize on every item of an
// agent list. The harness comes from the agent record when it names it
// (storedAgentHarness, no lookup); otherwise from resolveAgentHarnessType
// with a memo shared by the whole list, so the store is read at most once
// per distinct project-scope (project, reference) and once per distinct
// global-scope reference, never once per agent.
func (s *Server) setAgentListBadges(ctx context.Context, agents []AgentWithCapabilities) {
	var memo *harnessTypeMemo
	for i := range agents {
		a := &agents[i].Agent
		var resolved string
		if storedAgentHarness(a) == "" {
			if memo == nil {
				memo = newHarnessTypeMemo()
			}
			resolved = s.resolveAgentHarnessTypeMemo(ctx, a, memo)
		}
		agents[i].HarnessVendor = harness.Vendor(agentBadgeHarness(a, resolved))
		agents[i].ModelSize = agentModelSize(a)
	}
}

func (s *Server) resolveAgentHarnessCapabilities(ctx context.Context, agent *store.Agent) (string, api.HarnessAdvancedCapabilities) {
	resolvedHarness := s.resolveAgentHarnessType(ctx, agent)
	return resolvedHarness, harness.New(resolvedHarness).AdvancedCapabilities()
}

// resolveModelAliasForAgent resolves a model size alias (e.g. "extra-large")
// to a concrete model name (e.g. "fable") using the agent's harness config's
// model_aliases map. When the stored harness config has no model_aliases (or
// can't be found at all — e.g. it was deleted, or the agent only has an
// inline/harness-name reference), this falls back to the harness's built-in
// alias table (harnesses/<name>/config.yaml) so aliases still resolve rather
// than passing through unchanged to a resumed agent.
func (s *Server) resolveModelAliasForAgent(ctx context.Context, agent *store.Agent, model string) string {
	if agent == nil || agent.AppliedConfig == nil || model == "" {
		return model
	}

	// Try by ID first (fast path — stamped during create)
	if hcID := agent.AppliedConfig.HarnessConfigID; hcID != "" {
		hc, err := s.store.GetHarnessConfig(ctx, hcID)
		if err == nil && hc != nil {
			if aliases := s.modelAliasesForHarnessConfig(ctx, hc); len(aliases) > 0 {
				return config.ResolveModelAlias(model, aliases)
			}
		}
	}

	// Fallback: by slug (project scope, then global)
	hcSlug := agent.AppliedConfig.HarnessConfig
	if hcSlug != "" {
		var hc *store.HarnessConfig
		if agent.ProjectID != "" {
			hc, _ = s.store.GetHarnessConfigBySlug(ctx, hcSlug, store.HarnessConfigScopeProject, agent.ProjectID)
		}
		if hc == nil {
			hc, _ = s.store.GetHarnessConfigBySlug(ctx, hcSlug, store.HarnessConfigScopeGlobal, "")
		}
		if hc != nil {
			if aliases := s.modelAliasesForHarnessConfig(ctx, hc); len(aliases) > 0 {
				return config.ResolveModelAlias(model, aliases)
			}
		}
	}

	// Built-in fallback: no stored harness config carries a model_aliases
	// map for this agent. Resolve against the harness's own bundled
	// defaults instead of passing the alias through unresolved.
	if harnessType := s.resolveAgentHarnessType(ctx, agent); harnessType != "" {
		if aliases := harness.DefaultModelAliases(harnessType); len(aliases) > 0 {
			return config.ResolveModelAlias(model, aliases)
		}
	}
	return model
}

// modelAliasesForHarnessConfig returns hc's model_aliases map, backfilling
// it from the record's own stored config.yaml when the DB record predates
// resource_store.go stamping ModelAliases onto HarnessConfigData (or was
// otherwise never synced since). This is a read-time-only backfill — it does
// not persist the result — so a record missing aliases self-heals on every
// resolution rather than only on its next sync/create/update/finalize.
func (s *Server) modelAliasesForHarnessConfig(ctx context.Context, hc *store.HarnessConfig) map[string]string {
	if hc == nil {
		return nil
	}
	if hc.Config != nil && len(hc.Config.ModelAliases) > 0 {
		return hc.Config.ModelAliases
	}
	stor := s.GetStorage()
	if stor == nil || hc.StoragePath == "" {
		return nil
	}
	entry, ok := s.extractHarnessConfigEntryFromStorage(ctx, stor, hc)
	if !ok {
		return nil
	}
	return entry.ModelAliases
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
