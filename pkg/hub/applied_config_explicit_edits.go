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
	"encoding/json"
	"maps"
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
//     request's raw "config" object, lower-cased (applyAgentUpdate decodes
//     this separately from updates.Config: every ScionConfig field in the
//     PATCH body is `omitempty`, so the decoded struct alone cannot
//     distinguish an omitted field from one explicitly set to its Go zero
//     value, and "absent" versus "present and cleared" is exactly the
//     distinction the "other inline fields" pass below needs; lower-cased
//     because encoding/json itself matches struct field names
//     case-insensitively, so a non-canonical-case key from a non-web caller
//     must still count as present — see recordOtherInlineFieldEdits);
//   - canAttachEnv is whether the caller has attach-equivalent access to the
//     agent (canViewAgentEnv) — the same gate the GET response's Env
//     redaction uses. When false, the per-key env "removed" half below is
//     skipped (see the Env block).
//
// A nil ci (no CreateInputs -- the agent predates the field, or was created
// before explicit inputs were captured) is a no-op: the reincarnate fallback
// (legacyCreateInputsFromAppliedConfig) already reads the live config
// directly in that case, so there is nothing here for it to seed.
//
// Ordering with the TZ strip: applyAgentUpdate strips config.env["TZ"] (the
// env editor never sets the agent timezone; explicitTimezone does) between
// the `old` snapshot and this call, never after it. Otherwise an ignored TZ
// would land in CreateInputs.InlineConfig.Env here (via the per-key Env diff
// below) and be replayed by reincarnate's create-time timezone capture as a
// pin the request never asked for.
func recordExplicitEdits(ci *store.AgentCreateInputs, old *store.AgentAppliedConfig, cfg *api.ScionConfig, present map[string]bool, imageRegistry string, canAttachEnv bool) {
	if ci == nil {
		return
	}

	ensureInline := func() *api.ScionConfig {
		if ci.InlineConfig == nil {
			ci.InlineConfig = &api.ScionConfig{}
		}
		return ci.InlineConfig
	}

	// Image: both sides are canonicalised (registry-qualified) before
	// comparing, so an echo is not a diff regardless of which form old.Image
	// happens to be in -- old.Image is registry-qualified only after a
	// broker echo (httpdispatcher.go's applyBrokerResponse); before that --
	// for example a `created`-phase agent whose image came bare from a
	// template -- it is bare. Canonicalising only the request side would
	// read a bare echo of a bare old.Image as a diff and freeze the
	// template's image into CreateInputs. computeReincarnationPlan
	// (reincarnate_config.go) canonicalises both sides for exactly this
	// reason; this mirrors it. "" means "unchanged" here, exactly like
	// applyAgentUpdate's own live write: an empty Image never clears
	// anything, live or explicit.
	if cfg.Image != "" &&
		config.RewriteImageRegistry(cfg.Image, imageRegistry) != config.RewriteImageRegistry(old.Image, imageRegistry) {
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
	// key added or changed against the PAGE-VISIBLE baseline (see
	// diffExplicitEnvKeys: old.Env, falling back to old.InlineConfig.Env for
	// a key old.Env lacks -- ptone/scion#2493 R5-1) is set. A key the live
	// old.Env had that the request's Env no longer has is deleted -- UNLESS
	// canAttachEnv is false: the GET response withholds Env entirely from a
	// viewer without attach-equivalent access (canViewAgentEnv,
	// ResponseView), so that viewer's client can only ever load an empty
	// env and echo it back empty. Treating every one of its absent keys as
	// "removed" would read a response-redaction artifact as the user
	// deleting every explicit env key, turning a recoverable live-only loss
	// into a permanent one in CreateInputs. Additions are unaffected by this
	// gate: those can only come from someone typing a new key/value, which
	// is unambiguous regardless of what the viewer can see.
	//
	// The "removed" half stays old.Env-only (never falls back to
	// old.InlineConfig.Env): old.Env is the one live map every removal
	// candidate must already be absent from to count as removed at all, and
	// widening that check would change the R1-3/GITHUB_TOKEN behavior this
	// block does not otherwise touch.
	if cfg.Env != nil {
		var oldInlineEnv map[string]string
		if old.InlineConfig != nil {
			oldInlineEnv = old.InlineConfig.Env
		}
		added, removed := diffExplicitEnvKeys(old.Env, oldInlineEnv, cfg.Env)
		if !canAttachEnv {
			removed = nil
		}
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
	// differs from the live AppliedConfig.InlineConfig value (empty
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

// autoExposeEnvKeys mirrors AUTO_EXPOSE_ENV_KEYS in agent-configure.ts: the
// four env keys the dedicated auto-expose UI controls own. populateForm
// reads these per key with InlineConfig.Env taking precedence over
// AppliedConfig.Env (R4-2) -- the OPPOSITE precedence from every other env
// key, which the page reads from AppliedConfig.Env first and falls back to
// InlineConfig.Env only when AppliedConfig.Env is entirely empty. See
// pageVisibleEnvValue, which this diff-baseline precedence mirrors
// (ptone/scion#2493 R6-1).
var autoExposeEnvKeys = map[string]bool{
	"SCION_AUTO_EXPOSE_PORTS":      true,
	"SCION_AUTO_EXPOSE_MODE":       true,
	"SCION_AUTO_EXPOSE_PORTS_LIST": true,
	"SCION_AUTO_EXPOSE_INTERVAL":   true,
}

// pageVisibleEnvValue returns the value (and whether one exists at all)
// that agent-configure.ts's populateForm would have read for env key k,
// given the live AppliedConfig.Env (oldEnv) and InlineConfig.Env
// (oldInlineEnv) -- the same two maps diffExplicitEnvKeys' caller already
// has. The rule of thumb this and diffExplicitEnvKeys exist to uphold: a
// diff's baseline must always equal whatever the page actually read the
// value from (R5-1's framing, sharpened by R6-1).
//
// Two different precedence orders apply, because the page itself reads env
// two different ways:
//   - The four autoExposeEnvKeys are read per key by the dedicated
//     auto-expose controls: InlineConfig.Env (the requester's explicit
//     value) first, then AppliedConfig.Env (the project- or
//     template-derived value resolveDerivedConfig writes there).
//   - Every other key is read by the custom env-row editor as a whole map,
//     ac.env || ic.env: AppliedConfig.Env wins outright whenever it is
//     non-empty, and InlineConfig.Env is consulted per key only as a
//     fallback for a key AppliedConfig.Env doesn't have at all (R5-1) --
//     this is not a literal model of "ac.env entirely, else ic.env
//     entirely" (reproducing that exactly would need the full key sets, not
//     a per-key decision), but it gives the identical answer for every key
//     ac.env actually holds, which is the only case the page's own
//     all-or-nothing read can ever disagree with a naive oldEnv-only
//     baseline about.
func pageVisibleEnvValue(k string, oldEnv, oldInlineEnv map[string]string) (string, bool) {
	first, second := oldEnv, oldInlineEnv
	if autoExposeEnvKeys[k] {
		first, second = oldInlineEnv, oldEnv
	}
	if v, ok := first[k]; ok {
		return v, true
	}
	v, ok := second[k]
	return v, ok
}

// diffExplicitEnvKeys compares a request's env map against the live env it
// would replace, per §5 of ptone/scion#2493's options.md, as refined by
// review rounds 5 and 6 (R5-1, R6-1): added returns every key in newEnv
// that is missing from, or whose value differs from, pageVisibleEnvValue's
// answer for that key -- the value (if any) the configure page would
// actually have loaded and therefore could legitimately echo back
// unedited.
//
// removed returns every key oldEnv has that newEnv does not, EXCEPT
// GITHUB_TOKEN, which is never reported as removed: store.AgentAppliedConfig's
// MarshalJSON strips it from every API response unconditionally, even for an
// attach-capable viewer (see ResponseView's doc comment), so no caller's
// client can ever legitimately echo it back -- its absence from a request is
// therefore never evidence that the user removed it, only that the response
// never contained it to begin with. removed is deliberately NOT given the
// oldInlineEnv fallback and does not use pageVisibleEnvValue: every removal
// candidate must already be absent from oldEnv to be considered at all, and
// oldEnv is also the map the live write (`agent.AppliedConfig.Env =
// cfg.Env`) actually replaces, so it is already the correct single baseline
// for "did the user delete this" -- unaffected by either round's fix.
//
// A key present in both maps with an unchanged page-visible value appears
// in neither return.
//
// Deliberately separate from reincarnate_config.go's diffEnvKeys, which
// compares key names only (never values, since it feeds a user-facing plan
// and env values may be secrets) and returns a different shape (KeyDiff).
// This one needs values, to tell an unchanged echoed key apart from an
// edited one.
func diffExplicitEnvKeys(oldEnv, oldInlineEnv, newEnv map[string]string) (added map[string]string, removed []string) {
	for k, v := range newEnv {
		if b, ok := pageVisibleEnvValue(k, oldEnv, oldInlineEnv); ok && b == v {
			continue
		}
		if added == nil {
			added = make(map[string]string)
		}
		added[k] = v
	}
	for k := range oldEnv {
		if k == "GITHUB_TOKEN" {
			continue
		}
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
// present is keyed by lower-cased JSON field name (see recordExplicitEdits):
// encoding/json matches struct field names case-insensitively when there is
// no exact match, so a request sending `"System_Prompt"` still sets
// cfg.SystemPrompt even though present's key (built from the SAME raw
// bytes by applyAgentUpdate) would otherwise read "System_Prompt" while this
// function's canonical struct-tag name reads "system_prompt" -- two
// spellings of the same presence fact that must compare equal. The lookup
// below lower-cases the canonical name to match.
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
		if !present[strings.ToLower(name)] {
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

// carryForwardAbsentPageOwnedFields is the narrow page-owned carve-out from
// options.md §7.2 (ptone/scion#2493 R3-1, extended by R4-1): a small,
// explicit list of ScionConfig fields that the configure page used to send
// UNCONDITIONALLY at base (e572e72) -- so the wholesale InlineConfig replace
// in applyAgentUpdate was lossless for them -- but which Option C's
// present-keys-only fixes (R1-1, R2-1) correctly made conditional, to stop
// an untouched echo from freezing an unedited value into CreateInputs. That
// correctness fix had a side effect this function undoes: for exactly these
// fields, an untouched Save/Start now ALSO wipes the LIVE value via the same
// wholesale replace, which base never did.
//
// It must be called from applyAgentUpdate AFTER recordExplicitEdits (so
// CreateInputs has already correctly recorded these fields as untouched)
// and AFTER the field's own live-AppliedConfig write (e.g. `if cfg.Env !=
// nil { agent.AppliedConfig.Env = cfg.Env }`), but BEFORE the wholesale
// `agent.AppliedConfig.InlineConfig = cfg` assignment it exists to patch.
// present is the same lower-cased, raw-JSON-derived presence set
// recordExplicitEdits uses; old is the pre-PATCH snapshot.
//
// Each field's live-write guard already treats "absent" the same way this
// does (a nil cfg.Env, or a cfg.Telemetry this function has not yet filled
// in, changes nothing live), so adding an entry here only ever fills in a
// value the live write itself would otherwise have left untouched -- it
// never overrides an explicit value or a live write.
//
// Covered fields and why each needs it (the sweep review round 4 asked for,
// confirmed against base e572e72's buildConfig):
//   - Env: resolveDerivedConfig's auto-expose/hub-default env stamps, and
//     any create-time explicit env on a LEGACY agent (CreateInputs == nil),
//     live only in InlineConfig.Env. legacyCreateInputsFromAppliedConfig
//     (reincarnate_config.go) reads exactly that field to reconstruct a
//     legacy agent's explicit inputs at reincarnate -- a nil InlineConfig.Env
//     silently drops every one of its env keys, with no CreateInputs record
//     to fall back on (R4-1).
//   - Telemetry: project/hub telemetry defaults and an explicit opt-out live
//     only in InlineConfig.Telemetry; a nil value lets the broker's
//     settings/template fallback silently override it (R3-1).
//
// Fields checked and found NOT to need this (base buildConfig already sent
// them conditionally, or MORE is sent at head than at base -- see PR body's
// wipe-class audit table): model, image, auth_selectedType, task (hub-side
// "empty means unchanged" fields, excluded from recordExplicitEdits
// entirely and never wholesale-overwritten with a meaningfully different
// absent value); thinking_level (sent unconditionally at both base and
// head); branch, user, agent_instructions, system_prompt, max_turns,
// max_model_calls, max_duration (truthy-only at base, explicit-empty-always
// at head -- strictly more is sent now, never less); resources (`if
// hasResources` at both base and head, unchanged). harness/harness_config/
// default_harness_config and volumes/skills/mcp_servers/services/secrets/
// hub/kubernetes were never sent by this page at either revision, so they
// are §7.2's general wholesale-replace problem, not this narrow carve-out's.
func carryForwardAbsentPageOwnedFields(cfg *api.ScionConfig, old *store.AgentAppliedConfig, present map[string]bool) {
	if old.InlineConfig == nil {
		return
	}
	if !present["telemetry"] && old.InlineConfig.Telemetry != nil {
		cfg.Telemetry = deepCopyTelemetryConfig(old.InlineConfig.Telemetry)
	}
	if !present["env"] {
		cfg.Env = maps.Clone(old.InlineConfig.Env)
	}
}

// deepCopyTelemetryConfig returns an independent copy of cfg via a JSON
// marshal/unmarshal round trip (the same technique deepCopyScionConfig uses,
// handlers_agent_create_helpers.go). Returns nil for a nil input, and nil
// (with the error swallowed) if marshaling ever fails.
//
// Used by carryForwardAbsentPageOwnedFields to preserve the live
// InlineConfig.Telemetry across a PATCH that never mentions "telemetry" --
// a copy, not the same pointer, so the caller's subsequent wholesale
// InlineConfig replace (agent.AppliedConfig.InlineConfig = cfg) never leaves
// the new InlineConfig aliasing the old one's Telemetry.
func deepCopyTelemetryConfig(cfg *api.TelemetryConfig) *api.TelemetryConfig {
	if cfg == nil {
		return nil
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	var out api.TelemetryConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}
