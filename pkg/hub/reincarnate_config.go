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
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// buildFreshAppliedConfig replays create's config-resolution pipeline against
// the current template/harness-config catalog, for a `scion reincarnate`
// request (design §3.3, Amendment A1). It builds a
// brand-new AgentAppliedConfig — never mutating agent.AppliedConfig —
// containing:
//   - fields kept verbatim from the agent's CURRENT AppliedConfig: only the
//     ones deriveAgentConfig's pipeline never touches at all — identity-
//     adjacent fields, GitClone, Workspace, Branch, and GCPIdentity (an
//     input the auto-no-auth check reads, not something the pipeline
//     derives; access checks for it stay in the create handler and are not
//     re-run here, matching "kept");
//   - the requester's original explicit inputs (Image, Model, Env,
//     InlineConfig, HarnessConfig, HarnessAuth, Profile, ThinkingLevel), from
//     AppliedConfig.CreateInputs, or a heuristic reconstruction for an agent
//     that predates that field. Since Option C (ptone/scion#2493),
//     CreateInputs also picks up any later PATCH /api/v1/agents/{id} edit
//     that changed one of these fields' (or an Env key's) live value — see
//     recordExplicitEdits — so "explicit inputs" here means the create
//     request plus any explicit edit made since, not just what create itself
//     saw. HarnessConfig and HarnessAuth are dual-purpose exactly like Model,
//     NOT kept fields — left empty here (rather than copied from the live
//     config) is what lets deriveAgentConfig's project/template/hub
//     resolution below fill them fresh from the CURRENT catalog when the
//     requester never set them, instead of freezing in whatever generation N
//     happened to resolve;
//   - then deriveAgentConfig run on THAT fresh config: harness-config
//     resolution, applyProjectDefaults, applyHubAgentDefaults, then
//     populateAgentConfig/resolveDerivedConfig — so every derived slot is
//     recomputed against the current catalog exactly the way create computes
//     it, per-field precedence included (see deriveAgentConfig's doc
//     comment: Model is request > project > template > hub, HarnessConfig is
//     request > project > template > hub).
//
// Returns the fresh config, any warnings to surface on the plan (e.g. the
// legacy-fallback notice), and an error only for a genuine failure (missing
// AppliedConfig, or a workspace storage timeout from deriveAgentConfig,
// which wraps errWorkspaceStorageUnavailable).
func (s *Server) buildFreshAppliedConfig(ctx context.Context, agent *store.Agent, project *store.Project, imageRegistry string) (*store.AgentAppliedConfig, []string, error) {
	return s.buildPatchedAppliedConfig(ctx, agent, project, fixedImageRegistry(imageRegistry), nil)
}

// imageRegistryResolver returns the registry the hub rewrites an agent's
// explicit image to for the new generation's profile ("" = no rewrite). It
// is bound to the broker the new generation runs on (ptone/scion#3916).
type imageRegistryResolver func(profile string) string

// fixedImageRegistry is an imageRegistryResolver that ignores the profile.
func fixedImageRegistry(registry string) imageRegistryResolver {
	return func(string) string { return registry }
}

// buildPatchedAppliedConfig is buildFreshAppliedConfig with an optional,
// already-authorized reincarnate patch (ptone/scion#3302). The patch's
// Image/Model/ThinkingLevel/HarnessAuth are recorded into a copy of
// CreateInputs (recordReincarnatePatchEdits) before the pipeline runs, so
// they take the explicit-input slot and the new generation keeps them; its
// Role and GCPIdentity replace the kept values. A nil patch is exactly
// buildFreshAppliedConfig. agent.AppliedConfig is never mutated.
//
// resolveRegistry is called once the new generation's profile is settled,
// so the stored image is rewritten for that profile and the target broker.
func (s *Server) buildPatchedAppliedConfig(ctx context.Context, agent *store.Agent, project *store.Project, resolveRegistry imageRegistryResolver, patch *reincarnatePatch) (*store.AgentAppliedConfig, []string, error) {
	old := agent.AppliedConfig
	if old == nil {
		return nil, nil, fmt.Errorf("agent %s has no applied config", agent.ID)
	}

	// Resolve the template (no template row field feeds the legacy-fallback
	// env reconstruction below; ptone/scion#4224).
	var resolvedTemplate *store.Template
	if agent.Template != "" {
		tmpl, err := s.resolveTemplate(ctx, agent.Template, agent.ProjectID)
		if err != nil {
			s.agentLifecycleLog.Warn("reincarnate: failed to re-resolve template; continuing without one",
				"agent_id", agent.ID, "template", agent.Template, "error", err)
		} else {
			resolvedTemplate = tmpl
		}
	}

	// Adopt a legacy env TZ into ExplicitTimezone before anything reads
	// old's env: legacyCreateInputsFromAppliedConfig below would otherwise
	// replay it as a create-time env value instead of a carried-forward pin.
	adoptLegacyTZ(old)

	createInputs := old.CreateInputs
	var warnings []string
	if createInputs == nil {
		createInputs, warnings = legacyCreateInputsFromAppliedConfig(old)
	}
	if patch != nil {
		// A copy: createInputs may be the outgoing generation's own record
		// (old.CreateInputs, also saved as PreviousAppliedConfig).
		createInputs = cloneCreateInputs(createInputs)
		recordReincarnatePatchEdits(createInputs, patch)
	}

	fresh := &store.AgentAppliedConfig{
		// Kept verbatim: identity-adjacent fields the pipeline never touches,
		// and (for Phase 1, which accepts no --harness/--reset-overrides/etc.
		// request overrides) never overridden by the request either.
		// WorkspaceStoragePath (with its bucket) and AgentRoleGrandfathered
		// are kept too: without the storage path a remote broker gets the
		// hub-local path populateAgentConfig stamps for an empty Workspace
		// instead of the GCS path it actually needs, without the bucket a
		// broker with no bucket setting cannot download it
		// (ptone/scion#3422), and grandfathered-role provenance is
		// audit data, not something to re-derive.
		Attach:                 old.Attach,
		CreatorName:            old.CreatorName,
		AgentRole:              old.AgentRole,
		AgentRoleGrandfathered: old.AgentRoleGrandfathered,
		GitClone:               old.GitClone,
		Workspace:              old.Workspace,
		WorkspaceStoragePath:   old.WorkspaceStoragePath,
		WorkspaceStorageBucket: old.WorkspaceStorageBucket,
		Branch:                 old.Branch,
		GCPIdentity:            old.GCPIdentity,

		// Writer (d) of ExplicitTimezone: the pin, its legacy label and the
		// unpin record are carried forward. deriveAgentConfig's create-time
		// capture never overwrites a non-empty pin and moves nothing when
		// the agent was unpinned, so only an agent with neither re-derives
		// its timezone from CreateInputs and the current template.
		ExplicitTimezone:         old.ExplicitTimezone,
		ExplicitTimezoneLegacy:   old.ExplicitTimezoneLegacy,
		ExplicitTimezoneUnpinned: old.ExplicitTimezoneUnpinned,

		// Explicit-only: empty here (rather than copied from `old`) is what
		// lets deriveAgentConfig's pipeline fill these fresh from the CURRENT
		// project/template/hub configuration when the requester never set
		// them, instead of freezing in whatever generation N resolved.
		// HarnessConfig and HarnessAuth are NOT kept fields (design §3.3
		// Amendment A1) — they are explicit-only, exactly like Model.
		HarnessConfig: createInputs.HarnessConfig,
		HarnessAuth:   createInputs.HarnessAuth,
		Profile:       createInputs.Profile,
		ThinkingLevel: createInputs.ThinkingLevel,

		// Explicit inputs, replayed. CreateInputs (Amendment A1) captured
		// this InlineConfig before resolveDerivedConfig or mergeInjectedSkills
		// ever touched it, so — unlike the live InlineConfig — its Skills
		// field holds only what the requester actually asked for, and is
		// safe to feed back into mergeInjectedSkills as "template-scope"
		// input (see resolveDerivedConfig's doc comment).
		CreateInputs: createInputs,
	}

	// Kept fields a reincarnate patch replaces. Set before NoAuth is
	// derived below (role=none implies NoAuth) and before deriveAgentConfig
	// (GCPIdentity feeds its auto-no-auth check).
	if patch != nil {
		if patch.Role != "" {
			fresh.AgentRole = string(patch.Role)
			// An explicitly patched role is not a backfilled one.
			fresh.AgentRoleGrandfathered = false
		}
		if patch.GCPIdentity != nil {
			g := *patch.GCPIdentity
			fresh.GCPIdentity = &g
		}
	}

	// Design §3.5: make an empty (implicit-default) branch explicit, so a
	// template change cannot silently rename it. Today's implicit default
	// for clone-per-agent (init.go) and the value here are the same
	// slug-derived name, so this is a no-op for the resolved branch itself;
	// it just stops leaving it to be re-derived independently by two
	// generations that could in principle diverge.
	if fresh.GitClone != nil && fresh.Branch == "" {
		fresh.Branch = "scion/" + agent.Slug
	}

	// Do NOT alias Image/Model/Env to InlineConfig's backing map (design
	// §3.3 A1 addendum 2, rule 3): InlineConfig is a fresh, independent copy,
	// and Env gets its own clone (as buildAppliedConfig does on create), so
	// the auto-expose project tier below cannot write through to
	// CreateInputs.InlineConfig.Env or leave the new InlineConfig carrying
	// derived values it didn't have explicitly.
	if createInputs.InlineConfig != nil {
		fresh.InlineConfig = deepCopyScionConfig(createInputs.InlineConfig)
		fresh.Image = fresh.InlineConfig.Image
		fresh.Model = fresh.InlineConfig.Model
		fresh.Env = maps.Clone(fresh.InlineConfig.Env)
	}

	// SECURITY (design §3.4 Amendment A3.1): NoAuth must be re-derived from
	// every source that can produce it at create time, not just an explicit
	// HarnessAuth="none".
	// createInputs.NoAuth carries an explicit --no-auth AND a role=none
	// mapping (role is itself kept, so its NoAuth consequence must be too —
	// see AgentCreateInputs.NoAuth's doc comment). Missing this let a
	// role=none or --no-auth agent — including every scheduled-dispatch
	// agent — reincarnate with NoAuth reset to false and regain injected
	// secrets on its next start. This is OR'd, never overwritten: nothing
	// here can flip a true back to false.
	if createInputs.NoAuth || harness.IsNoAuthType(fresh.HarnessAuth) || fresh.AgentRole == string(AgentRoleNone) {
		fresh.NoAuth = true
	}

	freshAgent := &store.Agent{
		ID:            agent.ID,
		ProjectID:     agent.ProjectID,
		OwnerID:       agent.OwnerID,
		Ancestry:      agent.Ancestry,
		AppliedConfig: fresh,
		// The pin is kept across generations; a pinned (flat) agent does
		// not re-derive a default Runtime Broker Profile.
		PinnedRuntimeBrokerID:   agent.PinnedRuntimeBrokerID,
		PinnedRuntimeTargetID:   agent.PinnedRuntimeTargetID,
		PinnedRuntimeTargetType: agent.PinnedRuntimeTargetType,
	}
	// deriveAgentConfig — not resolveDerivedConfig directly — replays the
	// FULL create pipeline (applyProjectDefaults, then applyHubAgentDefaults,
	// then populateAgentConfig/resolveDerivedConfig). Calling
	// resolveDerivedConfig alone would skip the project/hub defaulting step
	// and let the template win over a project or hub default (design §3.3
	// Amendment A1 property 1).
	if err := s.deriveAgentConfig(ctx, freshAgent, project, resolvedTemplate); err != nil {
		return nil, nil, err
	}

	// Design §3.4 Amendment A11.1(a): fill Image from Hub settings, then the
	// resolved harness config, when deriveAgentConfig still left it empty.
	// resolveDerivedConfig only fills Image from an explicit inline config or
	// the template (see its own Image-fill code) — it never reads a Hub
	// settings harness_configs entry or a harness config's own image at all.
	// On create, that gap is invisible because the broker resolves and
	// echoes the actual image back (applyBrokerResponse), and the create
	// response handler persists it. The reincarnate plan has no broker round
	// trip to see that value before deciding what to show, so it is resolved
	// here instead.
	//
	// This preserves the broker's own image-resolution precedence (fixed by
	// ptone/scion#2156, amended by ptone/scion#1799 — an explicit profile
	// harness_overrides image is applied just below, above the template):
	// explicit inline, then template, then Hub settings
	// harness_configs.<name> (profiles.<p>.harness_overrides.<name>
	// outranking the base entry), then the harness config's own stored
	// image. fresh.Image already reflects "explicit inline, then template"
	// by this point (set from CreateInputs.InlineConfig.Image above, then
	// possibly filled from the template by resolveDerivedConfig inside
	// deriveAgentConfig) — see pkg/agent/provision.go's merge order (inline
	// is merged over the template first; that combined result is then
	// merged, as the higher-precedence side, over a harness-config-or-
	// settings base: `finalScionCfg = config.MergeScionConfig(hcCfg,
	// finalScionCfg)`) and pkg/config/templates.go's MergeScionConfig, whose
	// override side (`if override.Image != "" { result.Image =
	// override.Image }`) only wins when non-empty — so filling only when
	// fresh.Image is still empty here reproduces that exact order.
	//
	// Deliberately NOT added to resolveDerivedConfig itself: that would
	// change create's own behavior, which the Phase 0 golden test pins.
	var hc *store.HarnessConfig
	if fresh.HarnessConfigID != "" {
		var err error
		hc, err = s.store.GetHarnessConfig(ctx, fresh.HarnessConfigID)
		if err != nil {
			s.agentLifecycleLog.Warn("reincarnate: failed to resolve harness config for the image fallback",
				"agent_id", agent.ID, "harness_config_id", fresh.HarnessConfigID, "error", err)
			hc = nil
		}
	}
	// ptone/scion#1799: an EXPLICIT profiles.<p>.harness_overrides.<hc>.image
	// outranks the template (and inline) image the broker would otherwise
	// pick; only the user's explicit request image ranks above it. Apply
	// that here so the plan approximates what the broker will run. The plan
	// is an approximation from the hub's view: it starts from an image the
	// hub template record may carry (which the broker no longer applies), and
	// with no harness-config key (the broker resolves it) the profile
	// override cannot be looked up. The broker resolves the final image and
	// reports it. The plain harness_configs.<hc>.image default does not gain
	// this priority and stays in the fallback below.
	//
	// The hub's effective settings are loaded once for both this step and
	// the settings fallback below.
	vs := s.reincarnateImageSettings()
	// A pinned (flat) agent never takes Runtime Broker Profile-tier settings:
	// no profile harness_overrides image and no active_profile fallback, in
	// either image step below (flat Runtime Brokers contract, section 10).
	if vs != nil && agent.IsPinned() {
		vs = vs.ForProfileResolution(config.ProfileResolutionFlatInstance)
	}
	if explicitDispatchImage(fresh) == "" {
		overrideKey := fresh.HarnessConfig
		if overrideKey == "" && hc != nil {
			overrideKey = hc.Slug
		}
		if img := settingsProfileOverrideImage(vs, overrideKey, fresh.Profile); img != "" {
			fresh.Image = img
		}
	}
	if fresh.Image == "" && hc != nil {
		// Look up settings by the same key the broker dispatches with —
		// fresh.HarnessConfig is the harness-config slug/name as resolved
		// for this dispatch (GetHarnessConfigBySlug's input, echoed through
		// opts.HarnessConfig), not hc.Name (the harness config's own
		// display name, which can differ from its slug). Fall back to
		// hc.Slug only if fresh.HarnessConfig is somehow unset.
		settingsKey := fresh.HarnessConfig
		if settingsKey == "" {
			settingsKey = hc.Slug
		}
		fresh.Image = settingsHarnessConfigImage(vs, settingsKey, fresh.Profile)
	}
	if fresh.Image == "" && hc != nil && hc.Config != nil && hc.Config.Image != "" {
		fresh.Image = hc.Config.Image
	}

	// Store the image in the form the broker actually runs: the dispatcher
	// rewrites every image to its registry at send time and the broker echoes
	// that form back. RewriteImageRegistry is a no-op for an empty registry
	// and for an already-qualified name. The registry is the one for the
	// new generation's profile on the broker it runs on.
	fresh.Image = config.RewriteImageRegistry(fresh.Image, resolveRegistry(fresh.Profile))

	// deriveAgentConfig's timezone capture already stripped TZ from the
	// fresh env copies; strip again so no step after it can leave one.
	stripAgentEnvTZ(fresh)
	return fresh, warnings, nil
}

// reincarnateImageSettings loads the hub's effective settings for the
// reincarnate plan's image steps, or returns nil (logged) if they cannot be
// loaded, in which case those steps contribute nothing.
//
// The settings view comes from the hub's own config.LoadEffectiveSettings
// of its global settings directory (never a project found from the process
// working directory, ptone/scion#5363), which — in postgres mode — already
// reflects DB-backed harness_configs and
// profiles through the process-global settings overlay that
// OperationalSettings.Refresh populates (pkg/config/settings_overlay.go), the
// same overlay a co-located broker's own LoadEffectiveSettings call sees. In
// file/SQLite mode it reads the hub's settings.yaml directly.
func (s *Server) reincarnateImageSettings() *config.VersionedSettings {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		s.agentLifecycleLog.Warn("reincarnate: failed to resolve the global settings directory for the image plan", "error", err)
		return nil
	}
	vs, _, err := config.LoadEffectiveSettings(globalDir)
	if err != nil {
		s.agentLifecycleLog.Warn("reincarnate: failed to load settings for the image plan", "error", err)
		return nil
	}
	return vs
}

// settingsHarnessConfigImage resolves the Hub settings image for a named
// harness-config (harness_configs.<name>.image, with
// profiles.<profileName>.harness_overrides.<name>.image outranking the base
// entry — same precedence as pkg/config.VersionedSettings.ResolveHarnessConfig,
// which this reuses directly). Returns "" when settings has no image for
// this harness-config, in which case the caller falls back to the harness
// config's own stored default.
func settingsHarnessConfigImage(vs *config.VersionedSettings, harnessConfigName, profileName string) string {
	if vs == nil || harnessConfigName == "" {
		return ""
	}
	resolved, err := vs.ResolveHarnessConfig(profileName, harnessConfigName)
	if err != nil {
		return ""
	}
	return resolved.Image
}

// settingsProfileOverrideImage returns the image that
// profiles.<profileName>.harness_overrides.<harnessConfigName>.image sets
// explicitly in vs, or "" when unset.
func settingsProfileOverrideImage(vs *config.VersionedSettings, harnessConfigName, profileName string) string {
	if vs == nil {
		return ""
	}
	return vs.ProfileHarnessOverrideImage(profileName, harnessConfigName)
}

// imageRegistryProvider is implemented by dispatchers that rewrite image
// names to a registry before sending them to a broker.
type imageRegistryProvider interface {
	ImageRegistryFor(brokerID, profile string) string
}

// agentImageRegistryTarget returns the broker and profile an agent's image
// registry is resolved for: its runtime broker and its saved profile.
func agentImageRegistryTarget(agent *store.Agent) (brokerID, profile string) {
	if agent == nil {
		return "", ""
	}
	if agent.AppliedConfig != nil {
		profile = agent.AppliedConfig.Profile
	}
	return agent.RuntimeBrokerID, profile
}

// dispatchImageRegistry returns the registry the dispatcher rewrites the
// agent's images to (for its broker and profile), or "" when it does not
// rewrite them.
func dispatchImageRegistry(dispatcher AgentDispatcher, agent *store.Agent) string {
	brokerID, profile := agentImageRegistryTarget(agent)
	return dispatchImageRegistryForTarget(dispatcher, brokerID, profile)
}

// dispatchImageRegistryForTarget is dispatchImageRegistry for an explicit
// broker and profile.
func dispatchImageRegistryForTarget(dispatcher AgentDispatcher, brokerID, profile string) string {
	if p, ok := dispatcher.(imageRegistryProvider); ok {
		return p.ImageRegistryFor(brokerID, profile)
	}
	return ""
}

// reincarnateImageRegistry returns the resolver for a reincarnation's new
// generation on brokerID: the target broker of a move, or the agent's own
// broker in place (ptone/scion#3916). The source broker's or the old
// profile's registry is never used.
func reincarnateImageRegistry(dispatcher AgentDispatcher, brokerID string) imageRegistryResolver {
	return func(profile string) string {
		return dispatchImageRegistryForTarget(dispatcher, brokerID, profile)
	}
}

// legacyCreateInputsFromAppliedConfig reconstructs a best-effort
// AgentCreateInputs for an agent created before AppliedConfig.CreateInputs
// existed (design §3.3 Amendment A1's skills and env addenda). It cannot
// reliably tell "the requester set this" from "the template/hub/project
// defaulted it", so it is deliberately conservative:
//
//   - InlineConfig.Skills is dropped entirely, not filtered. mergeInjectedSkills
//     requires its input to hold only the requester's explicit inline-config
//     skills; a legacy agent's live Skills is already the merged
//     hub/user/project/template-labeled result, and there is no way to tell
//     which entries were originally explicit. Keeping any of them risks
//     freezing stale injected skills in as template scope (see
//     resolveDerivedConfig's doc comment). The caller must surface the
//     dropped refs as a plan warning.
//   - InlineConfig.Telemetry is dropped: it is always a hub/project/template
//     default once populated (see resolveDerivedConfig), never something the
//     requester provided directly in a way this reconstruction could trust.
//   - InlineConfig.Env["SCION_AUTO_EXPOSE_PORTS"] is stripped: on a legacy
//     agent it is usually the project or hub default an older hub stamped
//     there, and cannot be told apart from an explicit value. The caller then
//     re-derives the project and template tiers through deriveAgentConfig
//     (resolveAutoExposeEnv), exactly as for an agent with CreateInputs.
//   - Every other InlineConfig.Env key is kept as explicit, but flagged: it
//     may equally be a stale default the requester never asked for. The hub
//     that created a legacy agent aliased AppliedConfig.Env to
//     InlineConfig.Env, so residue an older hub copied from the template
//     row's stored env is replayed as explicit env too. The drop-only
//     filter that once removed keys the template row's env defined is
//     retired with that column (ptone/scion#4224): rekeying it on the
//     template's file env would drop a requester's own keys that share a
//     name with it, which is worse.
//   - Everything else (HarnessConfig name, HarnessAuth, Profile,
//     ThinkingLevel, Branch, Workspace, and any InlineConfig.Env key not
//     covered above) is taken from the live AppliedConfig verbatim. These are
//     single-source fields that resolveDerivedConfig never overwrites once
//     set (HarnessAuth aside, whose auto-no-auth fallback Phase 1 does not
//     attempt to distinguish from an explicit "none" for a legacy agent), so
//     reading them from the live config carries no staleness risk.
func legacyCreateInputsFromAppliedConfig(old *store.AgentAppliedConfig) (*store.AgentCreateInputs, []string) {
	warnings := []string{"explicit inputs reconstructed heuristically (agent predates CreateInputs)"}

	var inline *api.ScionConfig
	if old.InlineConfig != nil {
		inline = deepCopyScionConfig(old.InlineConfig)
		// cleared_fields belongs to the live generation: a reincarnation
		// provisions afresh, so the new generation starts with no clears
		// (ptone/scion#3906).
		inline.ClearedFields = nil
		// So does changed_fields: the reprovision renders every
		// provision-rendered key afresh (ptone/scion#3982).
		inline.ChangedFields = nil

		if len(inline.Skills) > 0 {
			refs := make([]string, 0, len(inline.Skills))
			for _, sk := range inline.Skills {
				refs = append(refs, sk.URI)
			}
			warnings = append(warnings, fmt.Sprintf(
				"dropped %d injected skill reference(s) that cannot be told apart from explicit ones on a legacy agent: %s",
				len(refs), strings.Join(refs, ", ")))
			inline.Skills = nil
		}

		inline.Telemetry = nil

		if inline.Env != nil {
			if _, ok := inline.Env["SCION_AUTO_EXPOSE_PORTS"]; ok {
				delete(inline.Env, "SCION_AUTO_EXPOSE_PORTS")
				warnings = append(warnings, "stripped SCION_AUTO_EXPOSE_PORTS (a project/hub default, not an explicit input) from the reconstructed config")
			}

			// Every other key is kept as explicit, but flagged. The
			// drop-only filter keyed on the template row's stored env is
			// retired with that column (ptone/scion#4224).
			var kept []string
			for k := range inline.Env {
				kept = append(kept, k)
			}
			if len(kept) > 0 {
				sort.Strings(kept)
				warnings = append(warnings, fmt.Sprintf(
					"kept %d env key(s) as explicit; may be a stale default from an earlier template that no longer defines them: %s",
					len(kept), strings.Join(kept, ", ")))
			}
		}
	}

	return &store.AgentCreateInputs{
		InlineConfig: inline,
		// NoAuth: carried forward only when already true. A legacy agent's
		// live NoAuth cannot be told apart from an explicit request versus
		// the auto-no-auth fallback (resolveDerivedConfig), so a true value
		// is kept (the safe direction) but a false value is
		// NOT trusted as "definitely never wanted no-auth"; fresh.AgentRole
		// and buildFreshAppliedConfig's own HarnessAuth=="none" check still
		// apply independently below.
		NoAuth:        old.NoAuth,
		HarnessConfig: old.HarnessConfig,
		HarnessAuth:   old.HarnessAuth,
		Profile:       old.Profile,
		ThinkingLevel: old.ThinkingLevel,
		Branch:        old.Branch,
		Workspace:     old.Workspace,
	}, warnings
}

// computeReincarnationPlan diffs the outgoing generation's AppliedConfig
// against the freshly resolved one, for the ReincarnateAgentResponse (design
// §3.2). Env key names only are compared, never values.
//
// The image is compared in canonical (registry-qualified) form, so a row that
// still holds the bare form of the same image shows as unchanged. An empty
// old image is always shown as a change.
func computeReincarnationPlan(old, fresh *store.AgentAppliedConfig, warnings []string, imageRegistry string) ReincarnationPlan {
	plan := ReincarnationPlan{
		Template:   FieldChange{Old: old.TemplateHash, New: fresh.TemplateHash},
		Image:      FieldChange{Old: old.Image, New: fresh.Image},
		HarnessCfg: FieldChange{Old: old.HarnessConfigHash, New: fresh.HarnessConfigHash},
		Model:      FieldChange{Old: old.Model, New: fresh.Model},
		EnvKeys:    diffEnvKeys(old.Env, fresh.Env),
		Branch:     fresh.Branch,
		Warnings:   warnings,
	}
	if old.Image != "" && config.RewriteImageRegistry(old.Image, imageRegistry) == fresh.Image {
		plan.Image.Old = fresh.Image
	}
	return plan
}

// diffEnvKeys compares two env maps by key name only (never values, per
// design §3.2's "names only, not values" — env values may be secrets).
func diffEnvKeys(oldEnv, newEnv map[string]string) KeyDiff {
	var diff KeyDiff
	for k := range newEnv {
		if _, ok := oldEnv[k]; !ok {
			diff.Added = append(diff.Added, k)
		} else if oldEnv[k] != newEnv[k] {
			diff.Changed = append(diff.Changed, k)
		}
	}
	for k := range oldEnv {
		if _, ok := newEnv[k]; !ok {
			diff.Removed = append(diff.Removed, k)
		}
	}
	sort.Strings(diff.Added)
	sort.Strings(diff.Removed)
	sort.Strings(diff.Changed)
	return diff
}
