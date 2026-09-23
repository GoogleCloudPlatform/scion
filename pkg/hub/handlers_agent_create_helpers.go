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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// resolveTemplate looks up a template by ID or name/slug.
// It tries: 1) by ID, 2) by slug in project scope, 3) by slug in global scope.
// Returns nil if not found, or an error for actual failures.
func (s *Server) resolveTemplate(ctx context.Context, templateRef, projectID string) (*store.Template, error) {
	// Try looking up by ID first (the CLI typically resolves names to IDs)
	template, err := s.store.GetTemplate(ctx, templateRef)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	if template != nil {
		return template, nil
	}

	// Try by slug/name within project scope
	template, err = s.store.GetTemplateBySlug(ctx, templateRef, "project", projectID)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	if template != nil {
		return template, nil
	}

	// Try global scope
	template, err = s.store.GetTemplateBySlug(ctx, templateRef, "global", "")
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	return template, nil
}

// authorizeResolvedTemplate reports whether identity may read the resolved
// template candidate before it is used to populate a new agent's applied
// config. resolveTemplate's first lookup arm resolves by ID across every
// scope, so a candidate it returns is not yet known to be one the caller may
// see — this establishes that, mirroring the read-authorization gate every
// other template read surface applies via templateResource (ptone/scion#1916).
//
// A nil template needs no check. A nil identity is fail-closed, not a
// pass: both of this gate's callers already require an identity before they
// can reach it (createAgentInProject runs behind authorizeAgentCreate, which
// rejects a nil identity outright, and the scheduler path's creatorIdentity
// is resolved from a scheduled event's CreatedBy before dispatch ever calls
// this — an empty/unresolvable creator fails the dispatch first). A
// background/system context that has no principal to check against, such as
// ValidateStartupDefaults, must not call this gate at all — it calls
// resolveTemplate directly and never surfaces the candidate to a caller, so
// there is nothing here for it to pass through. If a genuine internal path
// ever needs to bypass this check, it must do so explicitly (e.g. a
// documented system-principal Identity), not by leaving identity nil.
// A global-scope template is the hub-wide catalog — no confidentiality
// boundary applies, the same rule filterHubWideTemplateGrants encodes for
// the curated hub-member/hub-viewer grant (ptone/scion#1901/#1916). It is
// checked directly here, rather than relying on that grant, because it must
// also cover agent and broker principals, which never hold a
// hub-member-equivalent grant of their own but must still be able to resolve
// the hub-wide default template (e.g. a delegate agent's scheduled dispatch
// applying the hub's DefaultTemplate setting). A broker identity is scoped
// by brokerMayReadCatalogResource for the same reason getTemplateV2 and
// handleTemplateDownload use it: brokers read templates during agent
// creation (hydration) over HMAC auth, not as user principals, but that is
// authority to hydrate the projects they serve plus the hub-wide catalog —
// already covered by the global-scope check above — not every project's or
// user's private template.
func (s *Server) authorizeResolvedTemplate(ctx context.Context, identity Identity, tmpl *store.Template) bool {
	if tmpl == nil {
		return true
	}
	if identity == nil {
		return false
	}
	if tmpl.Scope == store.TemplateScopeGlobal {
		return true
	}
	if broker := GetBrokerIdentityFromContext(ctx); broker != nil {
		return s.brokerMayReadCatalogResource(ctx, broker, tmpl.Scope, tmpl.ScopeID)
	}
	if s.authzService == nil {
		return false
	}
	return s.authzService.CheckAccess(ctx, identity, templateResource(tmpl), ActionRead).Allowed
}

// getHarnessConfigFromTemplate returns the harness config name from a resolved template,
// or the fallback value if no template was resolved. Prefers the template's
// DefaultHarnessConfig (e.g. "claude-web") over the generic Harness type (e.g. "claude").
func (s *Server) getHarnessConfigFromTemplate(template *store.Template, fallback string) string {
	if template != nil {
		if template.DefaultHarnessConfig != "" {
			return template.DefaultHarnessConfig
		}
		if template.Harness != "" {
			return template.Harness
		}
	}
	return fallback
}

// buildAppliedConfig constructs an AgentAppliedConfig from a CreateAgentRequest.
// When req.Config is a ScionConfig, its fields are extracted into the applied config
// and the full ScionConfig is preserved as InlineConfig for threading to the broker.
func (s *Server) buildAppliedConfig(req CreateAgentRequest, harnessConfig string, creatorName string, effectiveRole AgentRole) *store.AgentAppliedConfig {
	ac := &store.AgentAppliedConfig{
		Profile:       req.Profile,
		HarnessConfig: harnessConfig,
		HarnessAuth:   req.HarnessAuth,
		Task:          req.Task,
		Attach:        req.Attach,
		Branch:        req.Branch,
		Workspace:     req.Workspace,
		CreatorName:   creatorName,
		AgentRole:     string(effectiveRole),
	}

	ac.NoAuth = req.NoAuth

	if req.Config != nil {
		ac.Image = req.Config.Image
		ac.Env = req.Config.Env
		ac.Model = req.Config.Model
		ac.ThinkingLevel = req.Config.ThinkingLevel

		// Extract ScionConfig-specific fields
		if req.Config.HarnessConfig != "" {
			ac.HarnessConfig = req.Config.HarnessConfig
		}
		if req.Config.AuthSelectedType != "" {
			ac.HarnessAuth = req.Config.AuthSelectedType
		}
		if req.Config.Task != "" && ac.Task == "" {
			ac.Task = req.Config.Task
		}

		// Preserve the full inline config for the broker
		ac.InlineConfig = req.Config
	}

	if ac.HarnessAuth == "none" {
		ac.NoAuth = true
	}

	// Snapshot the explicit inputs now, before resolveDerivedConfig (called
	// later, from populateAgentConfig) has a chance to fill in template/
	// harness-config/hub-default values on top of them. This is the only
	// point at which "explicit" and "derived" are still distinguishable —
	// several of these fields (Image, Model, Env, HarnessAuth, Workspace,
	// Branch) are dual-purpose: resolveDerivedConfig/populateAgentConfig only
	// fill them when empty, so a later read of agent.AppliedConfig cannot
	// tell "the user set this" from "the template/hub defaulted it".
	// `scion reincarnate` (design §3.3 Amendment A1) replays CreateInputs,
	// not the live AppliedConfig, so a migrated agent's derived fields are
	// recomputed fresh instead of inheriting a stale generation's values.
	//
	// InlineConfig is deep-copied rather than aliased: resolveDerivedConfig
	// mutates agent.AppliedConfig.InlineConfig in place (e.g. stamping hub
	// telemetry defaults), and CreateInputs must not observe that mutation
	// through a shared pointer.
	ac.CreateInputs = &store.AgentCreateInputs{
		InlineConfig:  deepCopyScionConfig(req.Config),
		HarnessConfig: ac.HarnessConfig,
		HarnessAuth:   ac.HarnessAuth,
		Profile:       ac.Profile,
		ThinkingLevel: ac.ThinkingLevel,
		Branch:        ac.Branch,
		Workspace:     ac.Workspace,
	}

	return ac
}

// deepCopyScionConfig returns an independent copy of cfg via a JSON
// marshal/unmarshal round trip, so the caller can hold onto a snapshot that
// later in-place mutation of the original cannot reach. Returns nil for a nil
// input, and nil (with the error swallowed) if marshaling ever fails — that
// can only happen for a pathological ScionConfig (e.g. a channel or func
// field, none of which the type has today), and a snapshot miss here is not
// worth failing agent creation over.
func deepCopyScionConfig(cfg *api.ScionConfig) *api.ScionConfig {
	if cfg == nil {
		return nil
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	var out api.ScionConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}

// populateAgentConfig enriches an agent's AppliedConfig with project-derived and
// template-derived fields after the initial config block has been set up.
// It populates GitClone config from project labels for git-anchored projects, and
// sets template ID, hash, and hub access scopes from the resolved template.
func (s *Server) populateAgentConfig(ctx context.Context, agent *store.Agent, project *store.Project, resolvedTemplate *store.Template) {
	if agent.AppliedConfig == nil {
		return
	}

	// Populate GitClone config for git-anchored projects (per-agent clone mode).
	// Shared-workspace git projects skip clone — agents mount the shared workspace instead.
	if project != nil && project.GitRemote != "" && !project.IsSharedWorkspace() {
		cloneURL := resolveCloneURL(project.Labels["scion.dev/clone-url"], project.GitRemote)
		defaultBranch := project.Labels["scion.dev/default-branch"]
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		defaultDepth := 1
		agent.AppliedConfig.GitClone = &api.GitCloneConfig{
			URL:    cloneURL,
			Branch: defaultBranch,
			Depth:  &defaultDepth,
		}
	}

	// Populate workspace path for hub-managed projects and shared-workspace git projects.
	// When the user provided a relative workspace (project subdirectory), preserve it
	// verbatim -- the broker will resolve it against its own project root.
	if project != nil && (project.GitRemote == "" || project.IsSharedWorkspace()) {
		existingWorkspace := agent.AppliedConfig.Workspace
		if existingWorkspace == "" {
			workspacePath, err := s.hubManagedProjectPath(project.Slug)
			if err == nil {
				agent.AppliedConfig.Workspace = workspacePath
			}
		}
	}

	// For shared-workspace git projects, default the branch to the project's
	// default branch (the workspace's current branch) instead of the agent slug.
	if project != nil && project.IsSharedWorkspace() && agent.AppliedConfig.Branch == "" {
		defaultBranch := project.Labels["scion.dev/default-branch"]
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		agent.AppliedConfig.Branch = defaultBranch
	}

	s.resolveDerivedConfig(ctx, agent, project, resolvedTemplate)
}

// resolveDerivedConfig is fill-if-empty, not recompute-against-the-catalog:
// for most fields (Image, Model, Env entries, HarnessConfigID/Hash,
// ProjectPreStartHookID/Script, template-derived InlineConfig.Telemetry) it
// only writes a slot on agent.AppliedConfig that is still empty, and leaves
// an already-populated slot alone. Calling it on an agent's EXISTING
// (already-derived) AppliedConfig therefore keeps every stale value from
// that derivation — it cannot tell "the caller set this explicitly" from
// "a previous call to this function derived it".
//
// This function alone does not reproduce what create does to an agent's
// config. On both create paths (handlers_agents_core.go and the scheduled-
// dispatch path in server.go), applyProjectDefaults and then
// applyHubAgentDefaults run on AppliedConfig BEFORE this function, and
// applyHubAgentDefaults may set the withHubDefaultHarnessConfig context flag
// that this function reads. Those two steps fill-if-empty HarnessConfig,
// HarnessAuth, Model, ThinkingLevel and Profile, plus InlineConfig's
// limits/resources, from project annotations and hub-wide agent_defaults.
// Because this function's own template-derived fills are themselves
// fill-if-empty, a project or hub default installed by those steps outranks
// the template here — skip the two steps and the template wins instead,
// inverting create's actual precedence (request > project/hub > template).
// To recompute against the current template/harness-config catalog (what
// `scion reincarnate` needs), call this on a freshly built AppliedConfig
// that holds only kept fields and explicit inputs, *after* replaying
// applyProjectDefaults then applyHubAgentDefaults (propagating its ctx flag)
// on that same fresh config, in that order — never on an existing agent's
// config, and never on this function alone.
//
// Exceptions — these ignore whether the slot is already set:
//   - TemplateID and TemplateHash are replaced whenever resolvedTemplate is
//     non-nil; HubAccessScopes too, but only when the template declares
//     hubAccess (otherwise an existing value is left as-is).
//   - Model-alias resolution: when AppliedConfig.Model is an alias, rewrites
//     it to the resolved concrete name, and overwrites a non-empty
//     InlineConfig.Model with that same resolved value (InlineConfig.Model
//     is untouched if it was already empty, and unaffected if
//     AppliedConfig.Model was not an alias).
//   - The auto-no-auth fallback can flip NoAuth to true and HarnessAuth from
//     "" to "none" based on a live credential check, and, once written, that
//     result is indistinguishable from an explicit --harness-auth none.
//   - The project's TelemetryEnabled annotation, when set to a valid bool,
//     overwrites InlineConfig.Telemetry.Enabled, even when the requester set
//     it inline.
//   - InlineConfig.Skills is always rewritten by mergeInjectedSkills, and it
//     is neither fill-if-empty nor additive: whatever is already in Skills
//     on entry is relabeled Scope="template" (highest precedence), merged
//     with the *current* hub/user/project injections, and the result
//     overwrites Skills. The precondition this assumes is that incoming
//     Skills holds only the requester's explicit inline-config skills
//     (req.Config.Skills on create) — which this function labels
//     Scope="template" — and nothing a previous merge produced. (The Hub
//     template's own skills are not read here; dispatch adds them
//     separately.) Calling this on a config whose Skills was already merged
//     (by a prior call to this function) promotes every hub/user/project
//     skill in it to template scope, permanently outranking the live
//     injections — deleting the injection can no longer remove it. A caller
//     reconstructing explicit inputs for reincarnate must capture Skills
//     before this ever runs, not read it back out afterward, and a legacy
//     caller with no such capture must drop Skills entirely rather than pass
//     through whatever InlineConfig currently has.
//
// InlineConfig is not a record of the requester's explicit inputs after this
// runs: this function creates it when nil (mergeInjectedSkills always does,
// which is why a bare create's InlineConfig is never nil) and writes into it
// — template/hub/project telemetry defaults, the project- or hub-level
// SCION_AUTO_EXPOSE_PORTS default (in InlineConfig.Env), the resolved Model
// alias, and InlineConfig.Skills (see above). It also receives the template
// env merge below by aliasing, not by a write in this function: the create
// path's config builder, buildAppliedConfig, sets AppliedConfig.InlineConfig
// to req.Config itself and AppliedConfig.Env to req.Config.Env, so
// InlineConfig *is* the request object and AppliedConfig.Env is its Env
// field — every InlineConfig write listed above also mutates the request,
// and, in the same direction, when the requester supplied any env, the
// template-env-merge writes below land in InlineConfig.Env too (and the
// auto-expose key also appears in AppliedConfig.Env). InlineConfig.Telemetry
// can also be aliased, by this function's own template-telemetry fill, to
// resolvedTemplate.Config.Telemetry, so the project TelemetryEnabled write
// above can mutate the template object through that shared pointer.
// InlineConfig therefore cannot be stripped back to explicit inputs by
// removing known hub/project keys — the aliased template-env keys are
// indistinguishable from explicit ones by inspecting InlineConfig. A caller
// that needs the original explicit request inputs (reincarnate does) must
// capture them before this runs, not read them back out of InlineConfig
// afterward.
//
// Precondition: agent.AppliedConfig must be non-nil (populateAgentConfig's
// caller-facing guard covers today's only call site; a direct caller must
// check first).
//
// This is the reusable half of populateAgentConfig: create calls it today,
// and reincarnate is intended to call it too (against a freshly built
// config that has already been through applyProjectDefaults and
// applyHubAgentDefaults, per above). It is not, by itself, "create's
// resolution" — see the precedence paragraph above — but combined with
// those two steps it derives a migrated agent's config the same way create
// derives a new agent's, given the same explicit inputs. What it
// deliberately does NOT touch: GitClone, Workspace, and Branch (kept
// verbatim across a reincarnation per design §3.3) — those are populated by
// populateAgentConfig above this call, before AppliedConfig is handed here.
func (s *Server) resolveDerivedConfig(ctx context.Context, agent *store.Agent, project *store.Project, resolvedTemplate *store.Template) {
	// Populate template ID, hash, and hub access scopes if template was resolved.
	if resolvedTemplate != nil {
		agent.AppliedConfig.TemplateID = resolvedTemplate.ID
		agent.AppliedConfig.TemplateHash = resolvedTemplate.ContentHash
		if resolvedTemplate.Config != nil && resolvedTemplate.Config.HubAccess != nil {
			// Still store the scopes on AppliedConfig for backward-compat visibility,
			// but they are no longer used for token generation (replaced by AgentRole).
			agent.AppliedConfig.HubAccessScopes = resolvedTemplate.Config.HubAccess.Scopes
			if len(resolvedTemplate.Config.HubAccess.Scopes) > 0 {
				slog.Warn("Template uses deprecated hubAccess.scopes; agent role determines scopes instead",
					"template", resolvedTemplate.Slug,
					"scopes", resolvedTemplate.Config.HubAccess.Scopes,
					"agent_role", agent.AppliedConfig.AgentRole,
				)
			}
		}

		// Merge template-level config values as defaults into AppliedConfig.
		// These act as pre-populated defaults for the advanced config form and
		// ensure the hub agent record reflects the effective configuration.
		// Explicit request values (already set) take precedence.
		if resolvedTemplate.Image != "" && agent.AppliedConfig.Image == "" {
			agent.AppliedConfig.Image = resolvedTemplate.Image
		}
		if resolvedTemplate.Config != nil {
			if resolvedTemplate.Config.Image != "" && agent.AppliedConfig.Image == "" {
				agent.AppliedConfig.Image = resolvedTemplate.Config.Image
			}
			if resolvedTemplate.Config.Model != "" && agent.AppliedConfig.Model == "" {
				agent.AppliedConfig.Model = resolvedTemplate.Config.Model
			}
			// Merge template env vars as defaults (don't overwrite explicit config env)
			if len(resolvedTemplate.Config.Env) > 0 {
				if agent.AppliedConfig.Env == nil {
					agent.AppliedConfig.Env = make(map[string]string)
				}
				for k, v := range resolvedTemplate.Config.Env {
					if _, exists := agent.AppliedConfig.Env[k]; !exists {
						agent.AppliedConfig.Env[k] = v
					}
				}
			}
			// Merge template telemetry config as default (don't overwrite explicit inline telemetry)
			if resolvedTemplate.Config.Telemetry != nil {
				if agent.AppliedConfig.InlineConfig == nil {
					agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
				}
				if agent.AppliedConfig.InlineConfig.Telemetry == nil {
					agent.AppliedConfig.InlineConfig.Telemetry = resolvedTemplate.Config.Telemetry
				}
			}
		}
	}

	// Populate harness config ID and hash for broker hydration.
	// Mirrors the template ID/hash stamping above: resolve the harness config
	// by slug (project scope first, then global) and stamp its ID and content
	// hash so the broker can fetch it from Hub storage.
	hcName := agent.AppliedConfig.HarnessConfig
	// Record where the name came from, for the not-found log level below. A
	// name that arrived from the project's default-harness-config annotation
	// is operator-supplied and displaced the template, so failing to resolve
	// it is worth a warning. Anything else — most often the template's bare
	// Harness type — is the pre-existing normal case.
	hcFromProjectAnnotation := hcName != "" && project != nil && project.Annotations != nil &&
		project.Annotations[projectSettingDefaultHarnessConfig] == hcName
	// A third provenance: the hub operational default_harness_config, applied
	// by applyHubAgentDefaults just above the call to this function. Carried on
	// the context rather than inferred by comparing hcName back against the
	// setting — that comparison cannot tell "the hub defaulted it" from "the
	// user named the same config the hub defaults to". Without this an operator
	// cannot tell a bad hub default from a bad project annotation in the log.
	// The !hcFromProjectAnnotation conjunct cannot currently be false when the
	// ctx flag is true — if the annotation supplied the name then the slot was
	// occupied and applyHubAgentDefaults never fired — so it is defence in
	// depth, not a live case. Kept so the two provenances stay mutually
	// exclusive by construction rather than by that reasoning holding.
	hcFromHubDefault := hcName != "" && !hcFromProjectAnnotation && hubDefaultHarnessConfigFromContext(ctx)
	if hcName == "" && resolvedTemplate != nil {
		hcName = s.getHarnessConfigFromTemplate(resolvedTemplate, "")
	}
	if hcName != "" && agent.AppliedConfig.HarnessConfigID == "" {
		var hc *store.HarnessConfig
		if project != nil {
			var err error
			hc, err = s.store.GetHarnessConfigBySlug(ctx, hcName, store.HarnessConfigScopeProject, project.ID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.agentLifecycleLog.Warn("failed to get project harness config by slug", "slug", hcName, "project_id", project.ID, "error", err)
			}
		}
		if hc == nil {
			var err error
			hc, err = s.store.GetHarnessConfigBySlug(ctx, hcName, store.HarnessConfigScopeGlobal, "")
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.agentLifecycleLog.Warn("failed to get global harness config by slug", "slug", hcName, "error", err)
			}
		}
		if hc != nil {
			agent.AppliedConfig.HarnessConfigID = hc.ID
			agent.AppliedConfig.HarnessConfigHash = hc.ContentHash

			// Auto no-auth fallback: when auth is "auto" (empty) and the harness
			// config declares no_auth.behavior=drop-to-shell, check whether the
			// required auth credentials are available. If not, enable NoAuth so
			// the broker doesn't reject the agent for missing env vars.
			if !agent.AppliedConfig.NoAuth &&
				agent.AppliedConfig.HarnessAuth == "" &&
				hc.Config != nil &&
				hc.Config.NoAuthBehavior == "drop-to-shell" {
				hasCreds, err := s.hasRequiredAuthCredentials(ctx, agent, hc.Harness, hc.Config.AuthMeta)
				if err != nil {
					s.agentLifecycleLog.Error("Failed to check auth credentials for fallback", "agent_id", agent.ID, "error", err)
				} else if !hasCreds {
					agent.AppliedConfig.NoAuth = true
					agent.AppliedConfig.HarnessAuth = "none"
					s.agentLifecycleLog.Info("Auto no-auth fallback: harness supports drop-to-shell and no credentials found",
						"agent_id", agent.ID, "harness", hc.Harness)
				}
			}
		} else {
			// Not-found is not an error here — the broker may still resolve the
			// name from its own search path — but it should not be entirely
			// silent either. The level tracks provenance:
			//
			//   WARN  — the name came from the project's
			//           scion.io/default-harness-config annotation. That
			//           annotation now outranks the template, so a stale or
			//           misspelled value displaced a known-good template value
			//           and the agent dispatches with no ID or hash for the
			//           broker to hydrate from Hub storage. An operator can fix
			//           it, so say so loudly.
			//
			//   WARN  — the name came from the hub operational
			//           agent_defaults.default_harness_config. Same argument,
			//           one tier lower and deployment-wide: a stale hub default
			//           silently costs every agent in the deployment its ID and
			//           hash. The two are distinguished in the log attributes
			//           so an operator knows which knob to turn.
			//
			//   DEBUG — anything else. Most often hcName is the template's bare
			//           Harness type ("claude") rather than a stored
			//           harness-config slug, via getHarnessConfigFromTemplate's
			//           second branch. Harness configs are created through the
			//           API rather than seeded, so "no HarnessConfig row whose
			//           slug matches the harness type" is the default state of a
			//           fresh deployment. Warning there would fire on every
			//           single agent create and train operators to ignore it.
			projectID := ""
			if project != nil {
				projectID = project.ID
			}
			level := slog.LevelDebug
			if hcFromProjectAnnotation || hcFromHubDefault {
				level = slog.LevelWarn
			}
			s.agentLifecycleLog.Log(ctx, level,
				"harness config not found in project or global scope; "+
					"agent will be dispatched without a config ID/hash and the broker must resolve it locally",
				"slug", hcName, "agent_id", agent.ID, "project_id", projectID,
				"from_project_annotation", hcFromProjectAnnotation,
				"from_hub_default", hcFromHubDefault)
		}
	}

	// Stamp the pre-start hook for broker delivery.
	// Resolve the active hook and inline its script content into AppliedConfig
	// so the broker can stage it without an extra Hub round-trip. Mirrors the
	// HarnessConfigID stamping pattern above.
	//
	// Resolution is a two-step fallback (see
	// .design/project-prestart-hooks-extensions.md section 1):
	//   1. The project's active hook wins outright.
	//   2. Otherwise the hub-wide active hook applies, if any.
	//   3. Neither → no script staged.
	// Either way the broker stages a single file (30-project-custom) and
	// AppliedConfig.ProjectPreStartHookID records which hook it came from, so
	// no scope-specific fields are needed downstream.
	//
	// The hub fallback is entered only on a definitive "no project hook"
	// (ErrNotFound). Any other project-lookup failure (DB blip, duplicate rows)
	// is ambiguous: the project may well have an override we simply failed to
	// read, and silently staging the hub script in that case would run the
	// wrong code. On an ambiguous error we log and stage nothing.
	if project != nil && agent.AppliedConfig.ProjectPreStartHookID == "" {
		hook, hookErr := s.store.GetActiveProjectPreStartHook(ctx, project.ID)
		switch {
		case hookErr == nil:
			// 1. Project-scoped hook found — it takes precedence.
		case errors.Is(hookErr, store.ErrNotFound):
			// 2. No project hook — fall back to the hub-scoped hook, if any.
			var hubErr error
			hook, hubErr = s.store.GetActiveHubPreStartHook(ctx)
			if hubErr != nil {
				if !errors.Is(hubErr, store.ErrNotFound) {
					s.agentLifecycleLog.Warn("failed to resolve hub pre-start hook", "error", hubErr)
				}
				hook = nil
			}
		default:
			// 3. Ambiguous project lookup failure — stage nothing.
			s.agentLifecycleLog.Warn("failed to resolve project pre-start hook; skipping hook staging",
				"project_id", project.ID, "error", hookErr)
			hook = nil
		}

		if hook != nil {
			agent.AppliedConfig.ProjectPreStartHookID = hook.ID
			agent.AppliedConfig.ProjectPreStartHookScript = hook.Script
		}
	}

	// Resolve model size aliases (e.g. "extra-large" → "fable") so that
	// AppliedConfig.Model and InlineConfig.Model carry the concrete model
	// name. This prevents raw aliases from leaking into SCION_MODEL env var
	// (set by httpdispatcher) and --model CLI flag (set by run.go).
	if agent.AppliedConfig.Model != "" {
		resolved := s.resolveModelAliasForAgent(ctx, agent, agent.AppliedConfig.Model)
		if resolved != agent.AppliedConfig.Model {
			agent.AppliedConfig.Model = resolved
			if agent.AppliedConfig.InlineConfig != nil && agent.AppliedConfig.InlineConfig.Model != "" {
				agent.AppliedConfig.InlineConfig.Model = resolved
			}
		}
	}

	// Merge hub-level telemetry config as lowest-priority default.
	// Only applies when no per-agent or template telemetry config is set.
	s.mu.RLock()
	hubTelemetry := s.config.TelemetryConfig
	s.mu.RUnlock()
	if hubTelemetry != nil {
		if agent.AppliedConfig.InlineConfig == nil {
			agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
		}
		if agent.AppliedConfig.InlineConfig.Telemetry == nil {
			// Deep copy to avoid sharing the pointer with the server config.
			copied := *hubTelemetry
			agent.AppliedConfig.InlineConfig.Telemetry = &copied
		}
	}

	// Apply project-level TelemetryEnabled override. This takes effect regardless
	// of where the telemetry config came from (inline, template, or hub), so
	// project admins can enable/disable telemetry for all agents in the project.
	if project != nil && project.Annotations != nil {
		if val, ok := project.Annotations[projectSettingTelemetryEnabled]; ok {
			if b, err := strconv.ParseBool(val); err == nil {
				if agent.AppliedConfig.InlineConfig == nil {
					agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
				}
				if agent.AppliedConfig.InlineConfig.Telemetry == nil {
					agent.AppliedConfig.InlineConfig.Telemetry = &api.TelemetryConfig{}
				}
				agent.AppliedConfig.InlineConfig.Telemetry.Enabled = &b
			}
		}
	}

	// Apply project-level AutoExposePortsEnabled override.
	// Only set the env var if the agent's own config does not already specify it,
	// so agent-level settings take priority over project-level.
	if project != nil && project.Annotations != nil {
		if val, ok := project.Annotations[projectSettingAutoExposePortsEnabled]; ok {
			enabled, err := strconv.ParseBool(val)
			if err == nil {
				if agent.AppliedConfig.InlineConfig == nil {
					agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
				}
				if agent.AppliedConfig.InlineConfig.Env == nil {
					agent.AppliedConfig.InlineConfig.Env = make(map[string]string)
				}
				if _, exists := agent.AppliedConfig.InlineConfig.Env["SCION_AUTO_EXPOSE_PORTS"]; !exists {
					agent.AppliedConfig.InlineConfig.Env["SCION_AUTO_EXPOSE_PORTS"] = strconv.FormatBool(enabled)
				}
			}
		}
	}

	// Apply hub-level AutoExposePortsDefault as lowest-priority fallback.
	// Injects SCION_AUTO_EXPOSE_PORTS if neither the agent config nor
	// the project annotation already set it, respecting both true and false defaults.
	s.mu.RLock()
	hubAutoExposeDefault := s.config.AutoExposePortsDefault
	s.mu.RUnlock()
	if hubAutoExposeDefault != nil {
		if agent.AppliedConfig.InlineConfig == nil {
			agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
		}
		if agent.AppliedConfig.InlineConfig.Env == nil {
			agent.AppliedConfig.InlineConfig.Env = make(map[string]string)
		}
		if _, exists := agent.AppliedConfig.InlineConfig.Env["SCION_AUTO_EXPOSE_PORTS"]; !exists {
			agent.AppliedConfig.InlineConfig.Env["SCION_AUTO_EXPOSE_PORTS"] = strconv.FormatBool(*hubAutoExposeDefault)
		}
	}

	// Merge injected skills from hub/user/project scopes into InlineConfig.Skills
	// so the provisioner's existing Step 3b handles them.
	s.mergeInjectedSkills(ctx, agent, project)
}

// mergeInjectedSkills fetches injected-skills refs from hub, user, and project
// scopes and merges them with template-level skills (highest precedence) into
// agent.AppliedConfig.InlineConfig.Skills. Fetches are best-effort: errors are
// logged and that scope's skills are omitted for this provisioning.
func (s *Server) mergeInjectedSkills(ctx context.Context, agent *store.Agent, project *store.Project) {
	if agent.AppliedConfig.InlineConfig == nil {
		agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
	}

	// Fetch hub-scope injected skills (system + user_defined).
	// ErrNotFound is the normal case when no hub skills are configured; all
	// other errors are unexpected and logged as warnings (best-effort).
	var hubRefs []api.SkillReference
	if hs, err := s.store.GetHubSetting(ctx, "injected_skills"); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("mergeInjectedSkills: failed to fetch hub injected skills setting", "error", err)
		}
	} else {
		var setting api.HubSkillInjectionSetting
		if err := json.Unmarshal(hs.Value, &setting); err != nil {
			slog.Warn("mergeInjectedSkills: failed to unmarshal hub injected skills setting", "error", err)
		} else {
			hubRefs = append(setting.System, setting.UserDefined...)
		}
	}
	for i := range hubRefs {
		hubRefs[i].Scope = "hub"
	}

	// Fetch user-scope injected skills.
	var userRefs []api.SkillReference
	if agent.OwnerID != "" {
		if sis, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeUser, agent.OwnerID); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("mergeInjectedSkills: failed to fetch user injected skills", "error", err)
			}
			// continue, best-effort
		} else {
			for _, si := range sis {
				userRefs = append(userRefs, si.ToSkillReference())
			}
		}
	}

	// Progeny skill resolution: when the agent has ancestry, include
	// user-scoped skill injections marked allowProgeny whose creator is in
	// the ancestry chain. These are added at user-scope precedence,
	// following the same pattern as resolveEnvFromStorage for env vars.
	if agent != nil && len(agent.Ancestry) > 1 {
		if progenySkills, err := s.store.ListProgenySkillInjections(ctx, agent.Ancestry); err != nil {
			slog.Warn("mergeInjectedSkills: failed to fetch progeny skill injections", "error", err)
		} else {
			// Deduplicate against already-included user refs by base URI.
			existingURIs := make(map[string]bool, len(userRefs))
			for _, ref := range userRefs {
				existingURIs[skillBaseURI(ref.URI)] = true
			}
			for _, si := range progenySkills {
				ref := si.ToSkillReference()
				if !existingURIs[skillBaseURI(ref.URI)] {
					userRefs = append(userRefs, ref)
					existingURIs[skillBaseURI(ref.URI)] = true
				}
			}
		}
	}

	for i := range userRefs {
		userRefs[i].Scope = "user"
	}

	// Fetch project-scope injected skills.
	var projectRefs []api.SkillReference
	if project != nil {
		if sis, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("mergeInjectedSkills: failed to fetch project injected skills", "error", err)
			}
			// continue, best-effort
		} else {
			for _, si := range sis {
				projectRefs = append(projectRefs, si.ToSkillReference())
			}
		}
	}
	// Copy the slice to avoid mutating the original SkillReference values.
	if len(projectRefs) > 0 {
		copied := make([]api.SkillReference, len(projectRefs))
		copy(copied, projectRefs)
		projectRefs = copied
	}
	for i := range projectRefs {
		projectRefs[i].Scope = "project"
	}

	// Template refs are already in InlineConfig.Skills (highest precedence).
	// Copy the slice to avoid mutating the caller's InlineConfig.Skills in place.
	var templateRefs []api.SkillReference
	if len(agent.AppliedConfig.InlineConfig.Skills) > 0 {
		templateRefs = make([]api.SkillReference, len(agent.AppliedConfig.InlineConfig.Skills))
		copy(templateRefs, agent.AppliedConfig.InlineConfig.Skills)
		for i := range templateRefs {
			templateRefs[i].Scope = "template"
		}
	}

	// Merge: hub → user → project → template (lowest to highest precedence).
	merged := mergeSkillRefs(hubRefs, userRefs, projectRefs, templateRefs)
	agent.AppliedConfig.InlineConfig.Skills = merged
}

// mergeSkillRefs deduplicates skill references by base URI across multiple scope
// slices. Later slices have higher precedence. When the same base URI appears in
// multiple scopes with different version pins, the higher-precedence entry wins.
// A single warning is emitted per base URI where the winning version differs from
// the first-seen version (version conflict). The result is returned in ascending
// base-URI order for deterministic output.
func mergeSkillRefs(scopes ...[]api.SkillReference) []api.SkillReference {
	// seen holds the current winner (last write wins — highest precedence).
	// first holds the initial entry per base URI so we can detect version conflicts
	// without firing intermediate warnings for every overwrite in a 3+ scope chain.
	seen := map[string]api.SkillReference{}
	first := map[string]api.SkillReference{}
	for _, refs := range scopes {
		for _, ref := range refs {
			base := skillBaseURI(ref.URI)
			if _, ok := seen[base]; !ok {
				first[base] = ref
			}
			seen[base] = ref
		}
	}
	// Warn once per base URI where the final winner has a different version than
	// the first-seen entry. This avoids misleading log noise when 3+ scopes
	// conflict (e.g. labelling transient intermediates as "winner").
	for base, winner := range seen {
		orig := first[base]
		if orig.URI != winner.URI {
			slog.Warn("possible skill injection version conflict or duplicate URI",
				"base_uri", base, "winner", winner.URI, "original", orig.URI)
		}
	}
	// Build result slice using the already-computed keys from `seen` for the sort
	// to avoid re-calling skillBaseURI O(n log n) times.
	type entry struct {
		base string
		ref  api.SkillReference
	}
	entries := make([]entry, 0, len(seen))
	for base, ref := range seen {
		entries = append(entries, entry{base, ref})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].base < entries[j].base
	})
	result := make([]api.SkillReference, len(entries))
	for i, e := range entries {
		result[i] = e.ref
	}
	return result
}

// existingAgentResult describes the outcome of handleExistingAgent.
type existingAgentResult int

const (
	// existingAgentNone means no existing agent was found (or it was nil).
	existingAgentNone existingAgentResult = iota
	// existingAgentDeleted means the stale agent was cleaned up; caller should fall through to create.
	existingAgentDeleted
	// existingAgentStarted means the existing agent was (re)started; response already written.
	existingAgentStarted
	// existingAgentErrored means an error occurred; response already written.
	existingAgentErrored
	// existingAgentConflict means an active agent with the same slug exists; caller should return 409.
	existingAgentConflict
)

// createNotifySubscription creates a notification subscription for the given agent
// if notify is true and a subscriber has been identified.
func (s *Server) createNotifySubscription(ctx context.Context, agentID, projectID, notifySubscriberType, notifySubscriberID, createdBy string) {
	if notifySubscriberID == "" {
		return
	}
	sub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    notifySubscriberType,
		SubscriberID:      notifySubscriberID,
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED", "STALLED", "ERROR"},
		CreatedAt:         time.Now(),
		CreatedBy:         createdBy,
	}
	if err := s.store.CreateNotificationSubscription(ctx, sub); err != nil {
		s.agentLifecycleLog.Warn("Failed to create notification subscription",
			"agent_id", agentID, "subscriber", notifySubscriberID, "error", err)
	} else {
		s.agentLifecycleLog.Debug("Created notification subscription",
			"subscriptionID", sub.ID, "agent_id", agentID,
			"subscriberType", notifySubscriberType, "subscriberID", notifySubscriberID)
	}
}

// resumeInPlaceDecision decides whether an existing agent in a terminal-ish
// phase may be restarted in place rather than rejected as a duplicate, and
// whether the harness should be handed its resume flag when that happens.
//
// Local (non-Hub) mode applies the same contract in localResumeDecision
// (cmd/common.go); keep the two in step.
//
// A stopped agent restarts with a *fresh* harness session even when resume was
// requested, mirroring the local CLI's effectiveResume. A forced recovery is
// the opposite case: the agent died without a clean shutdown (typically a host
// crash), so the whole point is to continue the interrupted session.
//
// phase=running is deliberately not forceable. A live agent must not be
// recreated out from under itself, and an operator who truly wants that can
// stop it first.
func resumeInPlaceDecision(phase string, resume, force bool) (resumeInPlace, forcedRecovery bool) {
	if !resume {
		return false, false
	}
	if force && phase == string(state.PhaseError) {
		return true, true
	}
	return phase == string(state.PhaseStopped), false
}

// handleExistingAgent encapsulates the full decision tree for an agent that
// already exists when a create/start request arrives.
//
// Phases:
//  1. Stale cleanup (running/stopped/error + not provision-only): dispatch delete, remove from DB → deleted
//  2. Env-gather re-provisioning (provisioning + GatherEnv): dispatch delete, remove from DB → deleted
//  3. Restart (created/provisioning/pending + not provision-only): recover broker ID, update config, dispatch start → started
//  4. Otherwise: none (caller decides what to do)
func (s *Server) handleExistingAgent(
	ctx context.Context,
	w http.ResponseWriter,
	existingAgent *store.Agent,
	project *store.Project,
	runtimeBrokerID string,
	req CreateAgentRequest,
	notifySubscriberType, notifySubscriberID, createdBy string,
) existingAgentResult {
	if existingAgent == nil {
		return existingAgentNone
	}

	// Authorization: every branch below starts, resumes, or restarts
	// existingAgent -- an agent that may belong to a different owner than the
	// caller of this create request. The only check the caller
	// (createAgentInProject) has made so far is authorizeAgentCreate, which
	// confirms the caller may create SOME agent in this project; it says
	// nothing about managing this SPECIFIC pre-existing one. Without this
	// gate, any project member who could create an agent could resume or
	// restart another member's agent by name, and get its response body
	// (including, pre-redaction, its applied config) back.
	//
	// Gate with the same lifecycle authorization the /start route enforces,
	// before any branch below acts. A denial folds into the ordinary
	// name-conflict result (existingAgentConflict) rather than a 403, so a
	// caller who cannot manage the colliding agent learns only that the name
	// is taken -- not who owns it, its phase, or its configuration.
	if !s.agentLifecycleAllowed(ctx, GetIdentityFromContext(ctx), existingAgent) {
		return existingAgentConflict
	}

	s.agentLifecycleLog.Info("handleExistingAgent: found existing agent",
		"slug", existingAgent.Slug,
		"existing_agent_id", existingAgent.ID,
		"existing_owner_id", existingAgent.OwnerID,
		"existing_phase", existingAgent.Phase,
		"caller_id", createdBy,
	)
	cleanupMode := req.CleanupMode
	if cleanupMode == "" {
		cleanupMode = "strict"
	}

	// Suspended agents are restarted in-place (not deleted), preserving harness state.
	if !req.ProvisionOnly && existingAgent.Phase == string(state.PhaseSuspended) {
		if existingAgent.RuntimeBrokerID == "" && runtimeBrokerID != "" {
			existingAgent.RuntimeBrokerID = runtimeBrokerID
		}

		dispatcher := s.GetDispatcher()
		if dispatcher == nil || existingAgent.RuntimeBrokerID == "" {
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				"cannot resume agent: no runtime broker available", nil)
			return existingAgentErrored
		}

		if req.Task != "" {
			if existingAgent.AppliedConfig == nil {
				existingAgent.AppliedConfig = &store.AgentAppliedConfig{}
			}
			existingAgent.AppliedConfig.Task = req.Task
			existingAgent.AppliedConfig.Attach = req.Attach
		}

		// A suspended agent's reservation was released when it was suspended;
		// re-reserve (with the cap check) before dispatch, same as create
		// (ptone/scion#1963). Idempotent, and rejects with the same
		// quota-exceeded response create uses if the broker is at capacity.
		if !s.checkAndReserveBrokerQuotaHTTP(ctx, w, existingAgent) {
			return existingAgentErrored
		}

		// This branch only runs for suspended agents, so resume the harness
		// session (Claude --continue) rather than starting fresh.
		resume := existingAgent.Phase == string(state.PhaseSuspended)
		if err := dispatcher.DispatchAgentStart(ctx, existingAgent, req.Task, resume); err != nil {
			s.releaseBrokerQuota(ctx, existingAgent)
			if isContainerNameConflict(err) {
				Conflict(w, "Agent name is already in use by a stopped container. Please delete the existing agent or choose a different name.")
			} else {
				RuntimeError(w, "Failed to resume suspended agent: "+err.Error())
			}
			return existingAgentErrored
		}

		if existingAgent.Phase == string(state.PhaseSuspended) {
			existingAgent.Phase = string(state.PhaseRunning)
		}
		if err := s.store.UpdateAgent(ctx, existingAgent); err != nil {
			s.agentLifecycleLog.Warn("Failed to update agent status after resume", "agent_id", existingAgent.ID, "error", err)
		}

		if req.Notify {
			s.createNotifySubscription(ctx, existingAgent.ID, existingAgent.ProjectID, notifySubscriberType, notifySubscriberID, createdBy)
		}

		s.enrichAgent(ctx, existingAgent, project, nil)
		writeJSON(w, http.StatusOK, CreateAgentResponse{
			Agent: redactedAgentCopy(ctx, s, existingAgent),
		})
		return existingAgentStarted
	}

	// Phase 1: Agent is running/stopped/error.
	// Resume=true for stopped agents restarts in-place; otherwise reject as duplicate.
	if !req.ProvisionOnly &&
		(existingAgent.Phase == string(state.PhaseRunning) ||
			existingAgent.Phase == string(state.PhaseStopped) ||
			existingAgent.Phase == string(state.PhaseError)) {

		resumeInPlace, forcedRecovery := resumeInPlaceDecision(existingAgent.Phase, req.Resume, req.ForceResume)
		if resumeInPlace {
			if existingAgent.RuntimeBrokerID == "" && runtimeBrokerID != "" {
				existingAgent.RuntimeBrokerID = runtimeBrokerID
			}

			dispatcher := s.GetDispatcher()
			if dispatcher == nil || existingAgent.RuntimeBrokerID == "" {
				writeError(w, http.StatusBadRequest, ErrCodeValidationError,
					"cannot resume agent: no runtime broker available", nil)
				return existingAgentErrored
			}

			if req.Task != "" {
				if existingAgent.AppliedConfig == nil {
					existingAgent.AppliedConfig = &store.AgentAppliedConfig{}
				}
				existingAgent.AppliedConfig.Task = req.Task
				existingAgent.AppliedConfig.Attach = req.Attach
			}

			// A stopped agent restarts with a fresh harness session even when
			// resume was requested (mirrors the local CLI's effectiveResume).
			// A forced recovery is the opposite: the whole point is to continue
			// the session the crash interrupted, so the harness resume flag is
			// passed through.
			if forcedRecovery {
				s.agentLifecycleLog.Warn("Force-resuming agent from error phase",
					"agent_id", existingAgent.ID, "agent", existingAgent.Name,
					"container_status", existingAgent.ContainerStatus)
			}
			// A stopped or errored agent's reservation was released when it
			// stopped/crashed; re-reserve (with the cap check) before
			// dispatch, same as create (ptone/scion#1963).
			if !s.checkAndReserveBrokerQuotaHTTP(ctx, w, existingAgent) {
				return existingAgentErrored
			}
			if err := dispatcher.DispatchAgentStart(ctx, existingAgent, req.Task, forcedRecovery); err != nil {
				s.releaseBrokerQuota(ctx, existingAgent)
				if isContainerNameConflict(err) {
					Conflict(w, "Agent name is already in use by a stopped container. Please delete the existing agent or choose a different name.")
				} else {
					RuntimeError(w, "Failed to resume stopped agent: "+err.Error())
				}
				return existingAgentErrored
			}

			existingAgent.Phase = string(state.PhaseRunning)
			if err := s.updateAgentAfterDispatch(ctx, existingAgent); err != nil {
				s.agentLifecycleLog.Warn("Failed to update agent status after resume", "agent_id", existingAgent.ID, "error", err)
			}

			if req.Notify {
				s.createNotifySubscription(ctx, existingAgent.ID, existingAgent.ProjectID, notifySubscriberType, notifySubscriberID, createdBy)
			}

			s.enrichAgent(ctx, existingAgent, project, nil)
			writeJSON(w, http.StatusOK, CreateAgentResponse{
				Agent: redactedAgentCopy(ctx, s, existingAgent),
			})
			return existingAgentStarted
		}

		return existingAgentConflict
	}

	// Phase 2: Env-gather re-provisioning — provisioning + GatherEnv requested.
	if req.GatherEnv && existingAgent.Phase == string(state.PhaseProvisioning) {
		dispatcher := s.GetDispatcher()
		if dispatcher != nil && existingAgent.RuntimeBrokerID != "" {
			if err := dispatcher.DispatchAgentDelete(ctx, existingAgent, false, false, false, time.Time{}); err != nil {
				if cleanupMode != "force" {
					RuntimeError(w, "Failed to clean up existing provisioning agent before env-gather recreate: "+err.Error())
					return existingAgentErrored
				}
				s.agentLifecycleLog.Warn("Proceeding after env-gather cleanup failure due to cleanupMode=force",
					"agent_id", existingAgent.ID, "agentName", existingAgent.Name, "error", err)
			}
		}
		if err := s.store.DeleteAgent(ctx, existingAgent.ID); err != nil {
			writeErrorFromErr(w, err, "")
			return existingAgentErrored
		}
		// ptone/scion#1963 delete-path audit: this hard-deletes a
		// provisioning-phase agent, which counts against
		// max_agents_per_broker (isBrokerQuotaCountedPhase). Release both
		// limits explicitly, matching the main delete handler
		// (handlers_agents_core.go) — the stale-reservation reconcile would
		// eventually catch a missed release once the agent record is gone,
		// but there is no reason to wait for that here.
		if s.quotaService != nil {
			s.releaseBrokerQuota(ctx, existingAgent)
			s.quotaService.Release(ctx, "max_agents_per_project", existingAgent.ID)
		}
		return existingAgentDeleted
	}

	// Phase 3: Restart — agent was provisioned/created and needs to be started.
	if !req.ProvisionOnly &&
		(existingAgent.Phase == string(state.PhaseCreated) ||
			existingAgent.Phase == string(state.PhaseProvisioning)) {

		// Recover RuntimeBrokerID from the freshly-resolved value if the stored one is empty.
		if existingAgent.RuntimeBrokerID == "" && runtimeBrokerID != "" {
			existingAgent.RuntimeBrokerID = runtimeBrokerID
		}

		dispatcher := s.GetDispatcher()
		if dispatcher == nil || existingAgent.RuntimeBrokerID == "" {
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				"cannot start agent: no runtime broker available", nil)
			return existingAgentErrored
		}

		// Update applied config with the task/attach if provided.
		if req.Task != "" {
			if existingAgent.AppliedConfig == nil {
				existingAgent.AppliedConfig = &store.AgentAppliedConfig{}
			}
			existingAgent.AppliedConfig.Task = req.Task
			existingAgent.AppliedConfig.Attach = req.Attach
		}

		// No quota re-reserve here (ptone/scion#1963): created/provisioning is
		// a counted phase (isBrokerQuotaCountedPhase), so this agent already
		// holds the reservation createAgentInProject took at CreateAgent time
		// — it was never released. Re-reserving would be a no-op anyway
		// (CheckAndReserve is idempotent per resource) but the point is this
		// path never lost its slot to begin with.
		//
		// Dispatch start action — DispatchAgentStart applies the broker's
		// response (status, container info) onto existingAgent in-place.
		// A created/provisioning agent has no prior session to resume.
		if err := dispatcher.DispatchAgentStart(ctx, existingAgent, req.Task, false); err != nil {
			if isContainerNameConflict(err) {
				Conflict(w, "Agent name is already in use by a stopped container. Please delete the existing agent or choose a different name.")
			} else {
				RuntimeError(w, "Failed to start agent: "+err.Error())
			}
			return existingAgentErrored
		}

		// If the broker didn't set a running phase, default to running.
		if existingAgent.Phase == string(state.PhaseCreated) ||
			existingAgent.Phase == string(state.PhaseProvisioning) {
			existingAgent.Phase = string(state.PhaseRunning)
		}
		if err := s.store.UpdateAgent(ctx, existingAgent); err != nil {
			// Log but continue — agent was started.
			s.agentLifecycleLog.Warn("Failed to update agent status after start", "agent_id", existingAgent.ID, "error", err)
		}

		// Create notification subscription if requested.
		if req.Notify {
			s.createNotifySubscription(ctx, existingAgent.ID, existingAgent.ProjectID, notifySubscriberType, notifySubscriberID, createdBy)
		}

		// Enrich and return the existing agent.
		s.enrichAgent(ctx, existingAgent, project, nil)
		writeJSON(w, http.StatusOK, CreateAgentResponse{
			Agent: redactedAgentCopy(ctx, s, existingAgent),
		})
		return existingAgentStarted
	}

	return existingAgentConflict
}

// resolveRuntimeBroker determines which runtime broker should run the agent.
// Priority order:
//  1. Explicitly specified broker (requestedBrokerID) - verified to be a provider
//  2. Project's default runtime broker - verified to be available (online)
//  3. Single provider (any status) - used automatically
//  4. Multiple providers with online brokers - returns error requiring explicit selection
//  5. No providers - returns error
//
// Returns the runtime broker ID or an error (after writing the HTTP error response).
func (s *Server) resolveRuntimeBroker(ctx context.Context, w http.ResponseWriter, requestedBrokerID string, project *store.Project) (string, error) {
	// Get ALL providers for this project (regardless of status)
	allProviders, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return "", err
	}

	// Get available (online) brokers for fallback logic
	availableBrokers, err := s.getAvailableBrokersForProject(ctx, project.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return "", err
	}

	slog.Debug("Resolving runtime broker",
		"project_id", project.ID, "projectName", project.Name,
		"requestedBroker", requestedBrokerID,
		"totalProviders", len(allProviders),
		"onlineProviders", len(availableBrokers),
		"defaultBroker", project.DefaultRuntimeBrokerID,
		"isHubNative", project.GitRemote == "")

	// Convert to summary for error responses, marking and prioritizing the default broker
	brokerSummaries := make([]RuntimeBrokerSummary, 0, len(availableBrokers))
	var defaultBrokerSummary *RuntimeBrokerSummary
	for _, h := range availableBrokers {
		summary := RuntimeBrokerSummary{
			ID:        h.ID,
			Name:      h.Name,
			Status:    h.Status,
			IsDefault: h.ID == project.DefaultRuntimeBrokerID,
		}
		if summary.IsDefault {
			defaultBrokerSummary = &summary
		} else {
			brokerSummaries = append(brokerSummaries, summary)
		}
	}
	// Prepend default broker if found (so it appears first in the list)
	if defaultBrokerSummary != nil {
		brokerSummaries = append([]RuntimeBrokerSummary{*defaultBrokerSummary}, brokerSummaries...)
	}

	// Case 1: Explicit runtime broker specified
	if requestedBrokerID != "" {
		// Check if the requested broker is a provider to this project (by ID, Name, or Slug)
		for _, p := range allProviders {
			if p.BrokerID == requestedBrokerID || p.BrokerName == requestedBrokerID {
				return p.BrokerID, nil
			}
			// Fetch broker to check slug
			broker, err := s.store.GetRuntimeBroker(ctx, p.BrokerID)
			if err == nil && broker.Slug == requestedBrokerID {
				return broker.ID, nil
			}
		}

		// Broker is not yet a provider — try to auto-link it.
		// The user explicitly selected this broker, so we honor that by linking it
		// to the project as a provider. This is common for hub-managed projects where
		// providers aren't established via CLI registration.
		broker, err := s.findBrokerByIDOrSlug(ctx, requestedBrokerID)
		if err == nil && broker != nil {
			provider := &store.ProjectProvider{
				ProjectID:  project.ID,
				BrokerID:   broker.ID,
				BrokerName: broker.Name,
				Status:     broker.Status,
				LinkedBy:   "agent-create",
			}
			if addErr := s.store.AddProjectProvider(ctx, provider); addErr != nil {
				slog.Warn("Failed to auto-link broker during agent creation",
					"broker", broker.Name, "project_id", project.ID, "error", addErr)
				RuntimeBrokerUnavailable(w, requestedBrokerID, brokerSummaries)
				return "", store.ErrNotFound
			}
			slog.Info("Auto-linked broker as project provider",
				"broker", broker.Name, "brokerID", broker.ID, "project_id", project.ID)

			// Set as default if project has none
			if project.DefaultRuntimeBrokerID == "" {
				project.DefaultRuntimeBrokerID = broker.ID
				if updateErr := s.store.UpdateProject(ctx, project); updateErr != nil {
					slog.Warn("Failed to set default runtime broker",
						"broker", broker.Name, "project_id", project.ID, "error", updateErr)
				}
			}
			return broker.ID, nil
		}

		// Broker doesn't exist at all
		slog.Warn("Requested broker not found during agent creation",
			"requestedBrokerID", requestedBrokerID, "project_id", project.ID,
			"providerCount", len(allProviders))
		RuntimeBrokerUnavailable(w, requestedBrokerID, brokerSummaries)
		return "", store.ErrNotFound
	}

	// Case 2: Use project's default runtime broker (must be online and dispatchable)
	if project.DefaultRuntimeBrokerID != "" {
		// Check if the default broker is still available
		for _, h := range availableBrokers {
			if h.ID == project.DefaultRuntimeBrokerID {
				if s.canDispatchToBroker(ctx, &h) {
					return project.DefaultRuntimeBrokerID, nil
				}
				// Default broker exists but user can't dispatch to it — fall through
				break
			}
		}
		// Default broker is not available or not dispatchable
		if len(availableBrokers) > 0 {
			NoRuntimeBroker(w, "Default runtime broker is unavailable; specify an alternative", brokerSummaries)
		} else {
			NoRuntimeBroker(w, "Default runtime broker is unavailable and no alternatives found", brokerSummaries)
		}
		return "", store.ErrNotFound
	}

	// Case 2.5: Hub-level default broker (from hub operational agent_defaults).
	// Used when the project has no default broker set. The hub default must be a
	// provider for this project and must be online and dispatchable.
	if hubDefault := s.hubAgentDefaults().DefaultRuntimeBroker; hubDefault != "" {
		for _, h := range availableBrokers {
			if h.ID == hubDefault || strings.EqualFold(h.Name, hubDefault) || strings.EqualFold(h.Slug, hubDefault) {
				if s.canDispatchToBroker(ctx, &h) {
					slog.Info("Using hub-level default runtime broker",
						"broker", h.Name, "brokerID", h.ID, "project_id", project.ID)
					return h.ID, nil
				}
				break
			}
		}
		// Hub default is set but not available/dispatchable for this project — fall through.
		slog.Debug("Hub-level default broker not available for project, falling through to auto-select",
			"hubDefault", hubDefault, "project_id", project.ID)
	}

	// Case 3: No default and no explicit broker - auto-select only when there is
	// exactly one provider and its broker is online and dispatchable.
	if len(allProviders) == 1 {
		broker, brokerErr := s.store.GetRuntimeBroker(ctx, allProviders[0].BrokerID)
		if brokerErr == nil && broker.Status == store.BrokerStatusOnline && s.canDispatchToBroker(ctx, broker) {
			return allProviders[0].BrokerID, nil
		}
		NoRuntimeBroker(w, "No runtime brokers available for this project that you have permission to use", brokerSummaries)
		return "", store.ErrNotFound
	}

	// Case 4: Multiple providers - filter to dispatchable brokers, then require selection
	var dispatchable []store.RuntimeBroker
	for _, h := range availableBrokers {
		if s.canDispatchToBroker(ctx, &h) {
			dispatchable = append(dispatchable, h)
		}
	}

	switch len(dispatchable) {
	case 0:
		NoRuntimeBroker(w, "No runtime brokers available for this project; register a runtime broker first", brokerSummaries)
		return "", store.ErrNotFound
	case 1:
		return dispatchable[0].ID, nil
	default:
		// Multiple dispatchable brokers - require explicit selection
		NoRuntimeBroker(w, "Multiple runtime brokers available for this project; specify runtimeBrokerId to select one", brokerSummaries)
		return "", store.ErrNotFound
	}
}

// brokerServesProject reports whether the broker is linked to the project as one
// of its providers.
//
// This is the "same project" test for agent-initiated dispatch. A broker is not
// owned by a project — the association is the project_providers link — so an
// agent's project can only be compared against a broker via this lookup. It is a
// shared helper rather than an inlined query so that callers of
// canDispatchToBroker cannot drift on the definition.
func (s *Server) brokerServesProject(ctx context.Context, brokerID, projectID string) bool {
	if brokerID == "" || projectID == "" {
		return false
	}
	provider, err := s.store.GetProjectProvider(ctx, projectID, brokerID)
	return err == nil && provider != nil
}

// canDispatchToBroker reports whether the caller may have an agent dispatched to
// this broker. It writes no HTTP response.
//
// Dispatch here is not a standalone permission: it is the broker-selection step
// inside agent creation, reached only after authorizeAgentCreate has already
// authorized the create itself. So agents may dispatch only to brokers that serve
// their own project, and only when their template granted ScopeAgentCreate —
// re-asserting the create gate rather than widening the Part 2 read-class baseline
// to ActionDispatch.
//
// Auto-provide brokers are shared infrastructure (e.g. a combo hub-broker server's
// default broker) and stay dispatchable by any authenticated caller. That check is
// deliberately kept ahead of the identity switch: it is a property of the broker,
// not of the caller.
//
// Broker-typed callers reach default: and are denied. CheckAccess already answered
// them with "unknown identity type"; the nil branch this replaced was the only
// thing admitting them, and it admitted everyone (#591).
//
// checkBrokerDispatchAccess (handlers_runtime_brokers.go) is the response-writing
// wrapper around this function. It delegates rather than restating the rule: the two
// were previously written twice, and the copy drifted into a fail-open (#591). Do not
// reintroduce a second transcription — add response behaviour to the wrapper instead.
func (s *Server) canDispatchToBroker(ctx context.Context, broker *store.RuntimeBroker) bool {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		// Unauthenticated. Note this branch is inverted from allow to deny, not
		// deleted: GetIdentityFromContext returns a literal nil interface, which
		// panics CheckAccess on identity.Type().
		return false
	}
	if broker.AutoProvide {
		return true
	}
	switch identity.Type() {
	case "user", "dev":
		user, ok := identity.(UserIdentity)
		if !ok {
			return false
		}
		decision := s.authzService.CheckAccess(ctx, user, brokerResource(broker), ActionDispatch)
		return decision.Allowed
	case "agent":
		agentIdent, ok := identity.(AgentIdentity)
		if !ok {
			return false
		}
		return agentIdent.HasScope(ScopeAgentCreate) &&
			s.brokerServesProject(ctx, broker.ID, agentIdent.ProjectID())
	default:
		return false
	}
}

// getAvailableBrokersForProject returns online runtime brokers that are providers to the project.
func (s *Server) getAvailableBrokersForProject(ctx context.Context, projectID string) ([]store.RuntimeBroker, error) {
	// Get providers for this project
	providers, err := s.store.GetProjectProviders(ctx, projectID)
	if err != nil {
		return nil, err
	}

	// Filter to online brokers and fetch their full details
	var availableBrokers []store.RuntimeBroker
	for _, provider := range providers {
		if provider.Status == store.BrokerStatusOnline {
			broker, err := s.store.GetRuntimeBroker(ctx, provider.BrokerID)
			if err != nil {
				continue // Skip brokers we can't fetch
			}
			if broker.Status == store.BrokerStatusOnline {
				availableBrokers = append(availableBrokers, *broker)
			}
		}
	}

	return availableBrokers, nil
}

// findBrokerByIDOrSlug looks up a runtime broker by ID, slug, or name.
func (s *Server) findBrokerByIDOrSlug(ctx context.Context, identifier string) (*store.RuntimeBroker, error) {
	// Try by ID first
	broker, err := s.store.GetRuntimeBroker(ctx, identifier)
	if err == nil {
		return broker, nil
	}

	// Try by name (case-insensitive)
	broker, err = s.store.GetRuntimeBrokerByName(ctx, identifier)
	if err == nil {
		return broker, nil
	}

	return nil, store.ErrNotFound
}

// agentHasGCPIdentityAssigned returns true when the agent's own GCPIdentity
// config has MetadataMode set to assign or passthrough, mirroring the broker's
// check at pkg/runtimebroker/handlers.go:2186-2187.
func agentHasGCPIdentityAssigned(agent *store.Agent) bool {
	if agent == nil || agent.AppliedConfig == nil || agent.AppliedConfig.GCPIdentity == nil {
		return false
	}
	mode := agent.AppliedConfig.GCPIdentity.MetadataMode
	return mode == store.GCPMetadataModeAssign || mode == store.GCPMetadataModePassthrough
}

// hasRequiredAuthCredentials checks whether the required auth environment
// variables and file secrets for the given harness type are available in the
// agent's env, or in the hub's env/secret stores (user and project scopes).
//
// When authMeta is non-nil (config-driven harness), file requirements from
// required_files are also evaluated. Files marked with
// SkippedWhenGCPServiceAccountAssigned are treated as satisfied when the
// agent's project has at least one verified GCP service account.
func (s *Server) hasRequiredAuthCredentials(ctx context.Context, agent *store.Agent, harnessType string, authMeta *config.HarnessAuthMetadata) (bool, error) {
	// When authMeta defines auth types and no explicit type was selected,
	// check ALL config-defined auth types and return true if ANY is fully
	// satisfiable. This prevents the compiled default (api-key) from
	// short-circuiting before config-driven types like vertex-ai are checked.
	if authMeta != nil && agent.AppliedConfig.HarnessAuth == "" && len(authMeta.Types) > 0 {
		gcpSAAssigned, err := s.projectHasVerifiedGCPSA(ctx, agent.ProjectID)
		if err != nil {
			return false, err
		}
		if !gcpSAAssigned {
			gcpSAAssigned = agentHasGCPIdentityAssigned(agent)
		}
		for authType := range authMeta.Types {
			satisfied, err := s.isAuthTypeSatisfied(ctx, agent, authMeta, authType, gcpSAAssigned)
			if err != nil {
				return false, err
			}
			if satisfied {
				return true, nil
			}
		}
		return false, nil
	}

	// Explicit auth type selected or no authMeta — use config-driven check when available.
	keyGroups := harness.RequiredAuthEnvKeysFromConfig(authMeta, agent.AppliedConfig.HarnessAuth)
	if len(keyGroups) == 0 && authMeta == nil {
		return true, nil
	}
	for _, group := range keyGroups {
		found, err := s.hasAnyKey(ctx, agent, group)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
	}

	// Check config-driven file requirements (e.g. gcloud-adc for vertex-ai).
	if authMeta != nil {
		gcpSAAssigned, err := s.projectHasVerifiedGCPSA(ctx, agent.ProjectID)
		if err != nil {
			return false, err
		}
		if !gcpSAAssigned {
			gcpSAAssigned = agentHasGCPIdentityAssigned(agent)
		}
		fileSecrets := harness.RequiredAuthSecretsFromConfig(authMeta, agent.AppliedConfig.HarnessAuth, gcpSAAssigned)
		for _, fs := range fileSecrets {
			keys := append([]string{fs.Key}, fs.AlternativeEnvKeys...)
			found, err := s.hasAnyKey(ctx, agent, keys)
			if err != nil {
				return false, err
			}
			if !found {
				return false, nil
			}
		}
	}

	return true, nil
}

// isAuthTypeSatisfied checks whether all env-var and file-secret requirements
// for a single auth type (as declared in authMeta) are met. Unlike the broker
// preflight (which only enforces required: true files), this checks ALL listed
// files so the hub can determine whether the auth type is viable.
//
// When gcpSAAssigned is true and the auth type is GCP-backed (has files with
// SkippedWhenGCPServiceAccountAssigned), env-var requirements are also treated
// as satisfied. This is because GCP-backed auth types (e.g. vertex-ai) get
// their env vars (GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_REGION) injected at
// runtime by the broker's resolveAuthEnvOverlay from settings.yaml or GCP
// metadata — credentials the Hub cannot see at preflight time (#1165).
func (s *Server) isAuthTypeSatisfied(ctx context.Context, agent *store.Agent, authMeta *config.HarnessAuthMetadata, authType string, gcpSAAssigned bool) (bool, error) {
	if authMeta == nil {
		return false, nil
	}

	// Determine whether this is a GCP-backed auth type whose runtime
	// environment will be provided by the broker/GCP metadata server.
	gcpRuntime := false
	if gcpSAAssigned {
		if t, ok := authMeta.Types[authType]; ok {
			for _, f := range t.RequiredFiles {
				if f.SkippedWhenGCPServiceAccountAssigned {
					gcpRuntime = true
					break
				}
			}
		}
	}

	// When gcpRuntime is true, skip the env-var check: the broker's
	// resolveAuthEnvOverlay and GCP metadata will provide the required
	// env vars at runtime, but they are invisible to the Hub's storage-only
	// hasAnyKey check, causing a false NoAuth trigger.
	if !gcpRuntime {
		keyGroups := harness.RequiredAuthEnvKeysFromConfig(authMeta, authType)
		for _, group := range keyGroups {
			found, err := s.hasAnyKey(ctx, agent, group)
			if err != nil {
				return false, err
			}
			if !found {
				return false, nil
			}
		}
	}

	t, ok := authMeta.Types[authType]
	if ok {
		for _, f := range t.RequiredFiles {
			if f.SkippedWhenGCPServiceAccountAssigned && gcpSAAssigned {
				continue
			}
			keys := []string{f.Name}
			keys = append(keys, f.AlternativeEnvKeys...)
			found, err := s.hasAnyKey(ctx, agent, keys)
			if err != nil {
				return false, err
			}
			if !found {
				return false, nil
			}
		}
	}

	return true, nil
}

// projectHasVerifiedGCPSA returns true if the project has at least one
// verified GCP service account, meaning the GCE metadata server can provide
// application default credentials at runtime.
func (s *Server) projectHasVerifiedGCPSA(ctx context.Context, projectID string) (bool, error) {
	if projectID == "" {
		return false, nil
	}
	sas, err := s.store.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   "project",
		ScopeID: projectID,
	})
	if err != nil {
		return false, err
	}
	for _, sa := range sas {
		if sa.Verified {
			return true, nil
		}
	}
	return false, nil
}

// hasAnyKey returns true if at least one of the keys is present in the
// agent's env, or in the hub's env/secret stores at user, project, or hub scope.
// For progeny agents (those with ancestry len > 1), it also checks for
// allowProgeny secrets and env vars inherited through the ancestry chain.
func (s *Server) hasAnyKey(ctx context.Context, agent *store.Agent, keys []string) (bool, error) {
	for _, key := range keys {
		if agent.AppliedConfig != nil && agent.AppliedConfig.Env != nil {
			if _, ok := agent.AppliedConfig.Env[key]; ok {
				return true, nil
			}
		}
		if agent.OwnerID != "" {
			ev, err := s.store.GetEnvVar(ctx, key, "user", agent.OwnerID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return false, err
			}
			if ev != nil {
				return true, nil
			}
			sec, err := s.store.GetSecret(ctx, key, "user", agent.OwnerID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return false, err
			}
			if sec != nil {
				return true, nil
			}
		}
		if agent.ProjectID != "" {
			ev, err := s.store.GetEnvVar(ctx, key, "project", agent.ProjectID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return false, err
			}
			if ev != nil {
				return true, nil
			}
			sec, err := s.store.GetSecret(ctx, key, "project", agent.ProjectID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return false, err
			}
			if sec != nil {
				return true, nil
			}
		}
		if s.hubID != "" {
			ev, err := s.store.GetEnvVar(ctx, key, store.ScopeHub, s.hubID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return false, err
			}
			if ev != nil {
				return true, nil
			}
			sec, err := s.store.GetSecret(ctx, key, store.ScopeHub, s.hubID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return false, err
			}
			if sec != nil {
				return true, nil
			}
		}
	}

	// Progeny resolution: when the agent has ancestry (len > 1 means it is a
	// progeny agent, not a direct user agent), check for allowProgeny secrets
	// and env vars inherited through the ancestry chain. This parallels the
	// resolution in pkg/secret/localbackend.go.
	if len(agent.Ancestry) > 1 {
		keySet := make(map[string]struct{}, len(keys))
		for _, k := range keys {
			keySet[k] = struct{}{}
		}

		progenySecrets, err := s.store.ListProgenySecrets(ctx, agent.Ancestry)
		if err != nil {
			return false, err
		}
		for _, ps := range progenySecrets {
			if _, ok := keySet[ps.Key]; ok {
				return true, nil
			}
		}

		progenyEnvVars, err := s.store.ListProgenyEnvVars(ctx, agent.Ancestry)
		if err != nil {
			return false, err
		}
		for _, pev := range progenyEnvVars {
			if _, ok := keySet[pev.Key]; ok {
				return true, nil
			}
		}
	}

	return false, nil
}
