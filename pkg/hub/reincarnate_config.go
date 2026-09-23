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
//     that predates that field. HarnessConfig and HarnessAuth are
//     dual-purpose exactly like Model, NOT kept fields — left empty here
//     (rather than copied from the live config) is what lets
//     deriveAgentConfig's project/template/hub resolution below fill them
//     fresh from the CURRENT catalog when the requester never set them,
//     instead of freezing in whatever generation N happened to resolve;
//   - then deriveAgentConfig run on THAT fresh config: harness-config
//     resolution, applyProjectDefaults, applyHubAgentDefaults, then
//     populateAgentConfig/resolveDerivedConfig — so every derived slot is
//     recomputed against the current catalog exactly the way create computes
//     it, per-field precedence included (see deriveAgentConfig's doc
//     comment: Model is request > project > hub > template, HarnessConfig is
//     request > project > template > hub).
//
// Returns the fresh config, any warnings to surface on the plan (e.g. the
// legacy-fallback notice), and an error only for a genuine failure (missing
// AppliedConfig).
func (s *Server) buildFreshAppliedConfig(ctx context.Context, agent *store.Agent, project *store.Project) (*store.AgentAppliedConfig, []string, error) {
	old := agent.AppliedConfig
	if old == nil {
		return nil, nil, fmt.Errorf("agent %s has no applied config", agent.ID)
	}

	// Resolve the template FIRST: the legacy-fallback env reconstruction
	// (A1 addendum 2) needs to know the current template's env keys to strip
	// them from a legacy agent's InlineConfig.Env before they're
	// misclassified as explicit.
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
	var templateEnv map[string]string
	if resolvedTemplate != nil && resolvedTemplate.Config != nil {
		templateEnv = resolvedTemplate.Config.Env
	}

	createInputs := old.CreateInputs
	var warnings []string
	if createInputs == nil {
		createInputs, warnings = legacyCreateInputsFromAppliedConfig(old, templateEnv)
	}

	fresh := &store.AgentAppliedConfig{
		// Kept verbatim: identity-adjacent fields the pipeline never touches,
		// and (for Phase 1, which accepts no --harness/--reset-overrides/etc.
		// request overrides) never overridden by the request either.
		// WorkspaceStoragePath and AgentRoleGrandfathered were missing here
		// (p1b-r1 R4): without the storage path a remote broker gets the
		// hub-local path populateAgentConfig stamps for an empty Workspace
		// instead of the GCS path it actually needs, and grandfathered-role
		// provenance is audit data, not something to re-derive.
		Attach:                 old.Attach,
		CreatorName:            old.CreatorName,
		AgentRole:              old.AgentRole,
		AgentRoleGrandfathered: old.AgentRoleGrandfathered,
		GitClone:               old.GitClone,
		Workspace:              old.Workspace,
		WorkspaceStoragePath:   old.WorkspaceStoragePath,
		Branch:                 old.Branch,
		GCPIdentity:            old.GCPIdentity,

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

	// Design §3.5: make an empty (implicit-default) branch explicit, so a
	// template change cannot silently rename it. Today's implicit default
	// for clone-per-agent (init.go) and the value here are the same
	// slug-derived name, so this is a no-op for the resolved branch itself;
	// it just stops leaving it to be re-derived independently by two
	// generations that could in principle diverge.
	if fresh.GitClone != nil && fresh.Branch == "" {
		fresh.Branch = "scion/" + agent.Slug
	}

	// Unlike buildAppliedConfig, deliberately do NOT alias Image/Model/Env to
	// InlineConfig's backing map (design §3.3 A1 addendum 2, rule 3):
	// InlineConfig is a fresh, independent copy, and Env gets its own clone,
	// so resolveDerivedConfig's template-env merge below cannot write through
	// to CreateInputs.InlineConfig.Env or leave the new InlineConfig carrying
	// merged values it didn't have explicitly. De-aliasing the create path
	// itself is out of scope (unaudited downstream readers); CreateInputs
	// makes it unnecessary there.
	if createInputs.InlineConfig != nil {
		fresh.InlineConfig = deepCopyScionConfig(createInputs.InlineConfig)
		fresh.Image = fresh.InlineConfig.Image
		fresh.Model = fresh.InlineConfig.Model
		fresh.Env = maps.Clone(fresh.InlineConfig.Env)
	}

	// p1b-r1 C1 (SECURITY): NoAuth must be re-derived from every source that
	// can produce it at create time, not just an explicit HarnessAuth="none".
	// createInputs.NoAuth carries an explicit --no-auth AND a role=none
	// mapping (role is itself kept, so its NoAuth consequence must be too —
	// see AgentCreateInputs.NoAuth's doc comment). Missing this let a
	// role=none or --no-auth agent — including every scheduled-dispatch
	// agent — reincarnate with NoAuth reset to false and regain injected
	// secrets on its next start. This is OR'd, never overwritten: nothing
	// here can flip a true back to false.
	if createInputs.NoAuth || fresh.HarnessAuth == "none" || fresh.AgentRole == string(AgentRoleNone) {
		fresh.NoAuth = true
	}

	freshAgent := &store.Agent{
		ID:            agent.ID,
		ProjectID:     agent.ProjectID,
		OwnerID:       agent.OwnerID,
		Ancestry:      agent.Ancestry,
		AppliedConfig: fresh,
	}
	// deriveAgentConfig — not resolveDerivedConfig directly — replays the
	// FULL create pipeline (applyProjectDefaults, then applyHubAgentDefaults,
	// then populateAgentConfig/resolveDerivedConfig). Calling
	// resolveDerivedConfig alone would skip the project/hub defaulting step
	// and let the template win over a project or hub default (design §3.3
	// Amendment A1 property 1).
	s.deriveAgentConfig(ctx, freshAgent, project, resolvedTemplate)

	return fresh, warnings, nil
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
//   - InlineConfig.Env["SCION_AUTO_EXPOSE_PORTS"] is stripped: it is a
//     project- or hub-level default, never an explicit request input.
//   - Every other key InlineConfig.Env shares with templateEnv (the CURRENT
//     template's env map) is also dropped, whatever its value (A1 addendum
//     2, rule 2): buildAppliedConfig aliases AppliedConfig.Env to
//     InlineConfig.Env, so a legacy agent's InlineConfig.Env is
//     indistinguishable-by-inspection from a mix of explicit keys and
//     template defaults merged in at create time. Assuming "the template
//     still owns this key" errs toward template freshness — reincarnate's
//     whole purpose — at the cost of resetting a genuinely explicit
//     per-agent override that happens to share a name with a template key.
//     Each dropped key is surfaced in the plan with its old and new value.
//     Keys InlineConfig.Env has that templateEnv does NOT define are kept as
//     explicit, but flagged: they may equally be a stale default the
//     requester never asked for, left over from an earlier version of the
//     template that did define them.
//   - Everything else (HarnessConfig name, HarnessAuth, Profile,
//     ThinkingLevel, Branch, Workspace, and any InlineConfig.Env key not
//     covered above) is taken from the live AppliedConfig verbatim. These are
//     single-source fields that resolveDerivedConfig never overwrites once
//     set (HarnessAuth aside, whose auto-no-auth fallback Phase 1 does not
//     attempt to distinguish from an explicit "none" for a legacy agent), so
//     reading them from the live config carries no staleness risk.
func legacyCreateInputsFromAppliedConfig(old *store.AgentAppliedConfig, templateEnv map[string]string) (*store.AgentCreateInputs, []string) {
	warnings := []string{"explicit inputs reconstructed heuristically (agent predates CreateInputs)"}

	var inline *api.ScionConfig
	if old.InlineConfig != nil {
		inline = deepCopyScionConfig(old.InlineConfig)

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

			var dropped, kept []string
			for k := range inline.Env {
				if newVal, definedByTemplate := templateEnv[k]; definedByTemplate {
					// p1b-r1 N2: never surface the OLD value. It came from a
					// legacy agent's live InlineConfig.Env, which is
					// indistinguishable-by-inspection from an explicit
					// per-agent override — and that override could be a
					// secret that merely happens to share a key name with a
					// template default. The new value is the template's own
					// current default, not user data, so it is safe to show.
					dropped = append(dropped, fmt.Sprintf("%s (new=%q)", k, newVal))
					delete(inline.Env, k)
				} else {
					kept = append(kept, k)
				}
			}
			if len(dropped) > 0 {
				sort.Strings(dropped)
				warnings = append(warnings, fmt.Sprintf(
					"dropped %d env key(s) assumed template-derived (now redefined by the current template): %s",
					len(dropped), strings.Join(dropped, ", ")))
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
		// is kept (the safe direction — see p1b-r1 C1) but a false value is
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
func computeReincarnationPlan(old, fresh *store.AgentAppliedConfig, warnings []string) ReincarnationPlan {
	plan := ReincarnationPlan{
		Template:   FieldChange{Old: old.TemplateHash, New: fresh.TemplateHash},
		Image:      FieldChange{Old: old.Image, New: fresh.Image},
		HarnessCfg: FieldChange{Old: old.HarnessConfigHash, New: fresh.HarnessConfigHash},
		Model:      FieldChange{Old: old.Model, New: fresh.Model},
		EnvKeys:    diffEnvKeys(old.Env, fresh.Env),
		Branch:     fresh.Branch,
		Warnings:   warnings,
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
