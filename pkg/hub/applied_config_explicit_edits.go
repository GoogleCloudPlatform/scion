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
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// explicitEditExcludedFields lists the ScionConfig JSON field names that
// recordExplicitEdits' generic "other inline fields" pass must never write
// into CreateInputs, even when present in the request and different from the
// live value:
//
//   - "task": CreateInputs deliberately excludes Task by design (see
//     AgentCreateInputs' doc comment, pkg/store/models.go) -- reincarnate's
//     hub-built preamble plus handoff always replaces it, so a PATCH-time
//     task is never an "explicit input" to replay.
//   - "harness", "harness_config", "default_harness_config": a harness
//     switch is not a supported PATCH operation today (applyAgentUpdate
//     validates config against the CURRENT harness's capabilities via
//     validateConfigAgainstHarnessCapabilities), so letting one reach
//     CreateInputs would let it take effect later, at reincarnate,
//     unvalidated against whatever harness is current then (design §3.6a's
//     future --harness override is the supported path for this).
var explicitEditExcludedFields = map[string]bool{
	"task":                   true,
	"harness":                true,
	"harness_config":         true,
	"default_harness_config": true,
}

// recordExplicitEdits is Option C (ptone/scion#2493) for the PATCH
// /api/v1/agents/{id} config path: it keeps AgentAppliedConfig.CreateInputs
// -- "the create request's explicit inputs, plus later explicit edits" (see
// its doc comment) -- in sync with a PATCH that changes a field's live
// value, and leaves it alone otherwise.
//
// Invariant E: a PATCH changes a CreateInputs field (or env key) IF AND ONLY
// IF it changed that field's live value. The configure page reloads the
// live, derived config and PATCHes the whole thing back on every Save and
// every Start (web/src/components/pages/agent-configure.ts's populateForm
// and buildConfig), so most of any given PATCH body is an echo, not an
// edit; an echoed value is therefore never recorded, which is what lets a
// template, hub or catalog value nobody actually touched keep refreshing at
// `scion reincarnate` instead of being frozen in as if the requester had
// typed it.
//
// Must be called from applyAgentUpdate BEFORE any of the live
// agent.AppliedConfig.* writes it diffs against:
//   - old is a snapshot of agent.AppliedConfig taken before those writes;
//   - cfg is the request's config, with Model already alias-resolved (the
//     same resolution applyAgentUpdate's own live write uses), since the
//     comparison must be against the value that is about to be written, not
//     the raw alias the requester typed;
//   - present is the set of JSON key names actually present in the
//     request's raw "config" object (applyAgentUpdate decodes this
//     separately from updates.Config: every ScionConfig field in the PATCH
//     body is `omitempty`, so the decoded struct alone cannot distinguish an
//     omitted field from one explicitly set to its Go zero value, and
//     "absent" versus "present and cleared" is exactly the distinction the
//     "other inline fields" pass below needs).
//
// A nil ci (no CreateInputs -- the agent predates the field, or was created
// before explicit inputs were captured) is a no-op: the reincarnate fallback
// (legacyCreateInputsFromAppliedConfig) already reads the live config
// directly in that case, so there is nothing here for it to seed.
//
// SEAM for ptone/scion#2457 task #16 (I2): that task adds a PATCH-time strip
// of config.env["TZ"] (so an ignored TZ key is never treated as a request
// value). That strip MUST run between the `old` snapshot and this call in
// applyAgentUpdate, never after it -- otherwise an ignored TZ would still
// land in CreateInputs.InlineConfig.Env here (via the per-key Env diff
// below) and be replayed by task #16's I1(a) as a pin the request never
// asked for. See the call site in applyAgentUpdate for the marked seam.
func recordExplicitEdits(ci *store.AgentCreateInputs, old *store.AgentAppliedConfig, cfg *api.ScionConfig, present map[string]bool, imageRegistry string) {
	if ci == nil {
		return
	}

	ensureInline := func() *api.ScionConfig {
		if ci.InlineConfig == nil {
			ci.InlineConfig = &api.ScionConfig{}
		}
		return ci.InlineConfig
	}

	// Image: compared in canonical (registry-qualified) form, so an echo of
	// the already-qualified live value is not a diff -- old.Image already
	// holds the registry-qualified form the broker actually ran (see
	// buildFreshAppliedConfig's own comment on this). "" means "unchanged"
	// here, exactly like applyAgentUpdate's own live write: an empty Image
	// never clears anything, live or explicit.
	if cfg.Image != "" && config.RewriteImageRegistry(cfg.Image, imageRegistry) != old.Image {
		ensureInline().Image = cfg.Image
	}

	// Model: cfg.Model is the caller's already-alias-resolved value (the
	// same one about to be written to agent.AppliedConfig.Model), so this
	// compares like for like. "" means "unchanged", same as the live write.
	if cfg.Model != "" && cfg.Model != old.Model {
		ensureInline().Model = cfg.Model
	}

	// ThinkingLevel: nil IS a value here (explicit-unset), not "absent" --
	// applyAgentUpdate's own live write applies cfg.ThinkingLevel
	// unconditionally ("Always apply thinking level from config"), so the
	// diff must compare it unconditionally too, not skip a nil.
	if !thinkingLevelEqual(cfg.ThinkingLevel, old.ThinkingLevel) {
		ci.ThinkingLevel = cfg.ThinkingLevel
		ensureInline().ThinkingLevel = cfg.ThinkingLevel
	}

	// HarnessAuth: "" means "unchanged", same as the live write.
	if cfg.AuthSelectedType != "" && cfg.AuthSelectedType != old.HarnessAuth {
		ci.HarnessAuth = cfg.AuthSelectedType
		ensureInline().AuthSelectedType = cfg.AuthSelectedType
	}

	// Env, per key: nil means the request didn't touch env at all (same
	// guard as the live write, which skips the whole map in that case). A
	// key added or changed against the live old.Env is set; a key the live
	// old.Env had that the request's Env no longer has is deleted. An
	// unchanged echoed key is left alone.
	if cfg.Env != nil {
		added, removed := diffExplicitEnvKeys(old.Env, cfg.Env)
		if len(added) > 0 || len(removed) > 0 {
			inline := ensureInline()
			if inline.Env == nil && len(added) > 0 {
				inline.Env = make(map[string]string, len(added))
			}
			for k, v := range added {
				inline.Env[k] = v
			}
			for _, k := range removed {
				delete(inline.Env, k)
			}
		}
	}

	// Every other ScionConfig field, present-keys-only: a field the
	// configure page (or any other caller) doesn't render is never present
	// in the request, so it is left alone here regardless of its live
	// value -- absent is never treated as cleared. A present field that
	// differs from the current CreateInputs.InlineConfig value (empty
	// included) is recorded; this is what lets a field be cleared back to
	// "not explicit" (re-derived from the template at reincarnate) by
	// sending it as an explicit empty value -- see agent-configure.ts's
	// buildConfig, which sends an explicit empty value for the fields it
	// owns when the user clears them.
	recordOtherInlineFieldEdits(ensureInline, old.InlineConfig, cfg, present)
}

// thinkingLevelEqual compares two possibly-nil thinking-level pointers by
// value, not by address: nil equals nil, and a non-nil pointer equals
// another non-nil pointer with the same underlying value.
func thinkingLevelEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// diffExplicitEnvKeys compares a request's env map against the live env it
// would replace, per §5 of ptone/scion#2493's options.md: added returns
// every key in newEnv that is missing from oldEnv or whose value differs;
// removed returns every key oldEnv has that newEnv does not. A key present
// in both with an unchanged value appears in neither.
//
// Deliberately separate from reincarnate_config.go's diffEnvKeys, which
// compares key names only (never values, since it feeds a user-facing plan
// and env values may be secrets) and returns a different shape (KeyDiff).
// This one needs values, to tell an unchanged echoed key apart from an
// edited one.
func diffExplicitEnvKeys(oldEnv, newEnv map[string]string) (added map[string]string, removed []string) {
	for k, v := range newEnv {
		if oldV, ok := oldEnv[k]; !ok || oldV != v {
			if added == nil {
				added = make(map[string]string)
			}
			added[k] = v
		}
	}
	for k := range oldEnv {
		if _, ok := newEnv[k]; !ok {
			removed = append(removed, k)
		}
	}
	return added, removed
}

// recordOtherInlineFieldEdits implements the generic "other inline fields"
// rule of recordExplicitEdits: every api.ScionConfig field other than the
// ones recordExplicitEdits handles itself (Image, Model, ThinkingLevel,
// AuthSelectedType, Env -- each compared against a different baseline or
// with different unchanged-value semantics) and the ones
// explicitEditExcludedFields names, is compared against oldInline (the live
// AppliedConfig.InlineConfig, or its zero value when nil) field by field,
// for present keys only. A present field whose value differs is copied into
// the CreateInputs InlineConfig that ensureInline returns (allocated lazily,
// and only once at least one field actually changed, so a no-op PATCH never
// turns a nil CreateInputs.InlineConfig into a non-nil empty one).
//
// Implemented via reflection over the JSON struct tags, rather than a
// hand-written field list, so a new ScionConfig field is covered by this
// rule automatically (as "other", i.e. present-keys-only, not frozen) unless
// someone deliberately special-cases or excludes it -- matching the design's
// stated default (design §5: explicit-only unless proven otherwise).
func recordOtherInlineFieldEdits(ensureInline func() *api.ScionConfig, oldInline *api.ScionConfig, cfg *api.ScionConfig, present map[string]bool) {
	if len(present) == 0 {
		return
	}

	var oldCfg api.ScionConfig
	if oldInline != nil {
		oldCfg = *oldInline
	}
	oldVal := reflect.ValueOf(oldCfg)
	cfgVal := reflect.ValueOf(*cfg)
	t := cfgVal.Type()

	var changed []int
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" || explicitEditExcludedFields[name] {
			continue
		}
		switch name {
		case "image", "model", "thinking_level", "auth_selectedType", "env":
			// Handled above against a different baseline (the live
			// AgentAppliedConfig, not InlineConfig) and/or different
			// unchanged-value semantics; skip to avoid double-recording
			// against the wrong baseline.
			continue
		}
		if !present[name] {
			continue
		}
		if !reflect.DeepEqual(cfgVal.Field(i).Interface(), oldVal.Field(i).Interface()) {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return
	}

	dst := reflect.ValueOf(ensureInline()).Elem()
	for _, i := range changed {
		dst.Field(i).Set(cfgVal.Field(i))
	}
}
