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
	"maps"
	"reflect"
	"sort"
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
// IF it changed that field's live value. A client may reload the live,
// derived config and PATCH it back (the retired Configure page did so on
// every Save and Start; the shared agent form,
// web/src/components/shared/agent-config-form.ts, sends only the fields the
// user touched), so a PATCH body can be largely an echo, not an edit; an
// echoed value is therefore never recorded, which is what lets a
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
func recordExplicitEdits(ci *store.AgentCreateInputs, old *store.AgentAppliedConfig, cfg *api.ScionConfig, present map[string]bool, ep envPatch, imageRegistry string, canAttachEnv bool) {
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

	// ThinkingLevel, present key only: applyAgentUpdate's live write applies
	// cfg.ThinkingLevel only when "thinking_level" is in the request, so an
	// absent key changes nothing here either. When present, nil IS a value
	// (an explicit null unsets it), so a nil is compared, not skipped.
	if present["thinking_level"] && !thinkingLevelEqual(cfg.ThinkingLevel, old.ThinkingLevel) {
		ci.ThinkingLevel = cfg.ThinkingLevel
		ensureInline().ThinkingLevel = cfg.ThinkingLevel
	}

	// HarnessAuth: "" means "unchanged", same as the live write.
	if cfg.AuthSelectedType != "" && cfg.AuthSelectedType != old.HarnessAuth {
		ci.HarnessAuth = cfg.AuthSelectedType
		ensureInline().AuthSelectedType = cfg.AuthSelectedType
	}

	// Env, per key (ptone/scion#5599): !ep.present means the request didn't
	// touch env at all. Keys in ep.set are compared against their baseline,
	// keys in ep.remove (explicit null) are deleted when canAttachEnv, and
	// absent keys are untouched.
	if ep.present {
		added, removed := diffExplicitEnvKeys(old.Env, explicitEnvOf(old), ep)
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
	// agent form (or any other caller) doesn't send is never present
	// in the request, so it is left alone here regardless of its live
	// value -- absent is never treated as cleared. A present field that
	// differs from the live AppliedConfig.InlineConfig value (empty
	// included) is recorded; this is what lets a field be cleared back to
	// "not explicit" (re-derived from the template at reincarnate) by
	// sending it as an explicit empty value -- see the shared agent form
	// (web/src/components/shared/agent-config-form.ts), whose Clear sends an
	// explicit null for a value the agent stores.
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

// autoExposeEnvKeys mirrors AUTO_EXPOSE_ENV_KEYS in
// web/src/components/shared/agent-config-form.ts: the four env keys the
// dedicated auto-expose control owns. The form sends them only when the
// user changed that control, so in a PATCH env map
// an absent auto-expose key means "untouched", never "removed"
// (diffExplicitEnvKeys, applyPatchAutoExposeEnv).
var autoExposeEnvKeys = map[string]bool{
	"SCION_AUTO_EXPOSE_PORTS":      true,
	"SCION_AUTO_EXPOSE_MODE":       true,
	"SCION_AUTO_EXPOSE_PORTS_LIST": true,
	"SCION_AUTO_EXPOSE_INTERVAL":   true,
}

// diffExplicitEnvKeys compares a PATCH request's key-level envPatch (ep)
// against the agent's stored env:
//
//   - added holds every key in ep.set whose value is missing from, or
//     differs from, its baseline. For the autoExposeEnvKeys the baseline is
//     explicitEnv (the CreateInputs record, explicitEnvOf), not
//     InlineConfig.Env, which can still hold a hub-stamped value on older
//     agents. Sending the auto-expose control records it as explicit unless
//     it already was, with that value, even when it equals a
//     project-derived or hub-stamped value. For every other key the baseline
//     is oldEnv (AppliedConfig.Env), so an unedited row echoed back is never
//     recorded.
//   - removed holds every key in ep.remove (explicit JSON null) that exists
//     in oldEnv or explicitEnv (sorted). Keys absent from ep are preserved
//     and never appear in removed.
//
// A key present in ep.set with an unchanged baseline value appears in
// neither return.
func diffExplicitEnvKeys(oldEnv, explicitEnv map[string]string, ep envPatch) (added map[string]string, removed []string) {
	for k, v := range ep.set {
		baseline := oldEnv
		if autoExposeEnvKeys[k] {
			baseline = explicitEnv
		}
		if b, ok := baseline[k]; ok && b == v {
			continue
		}
		if added == nil {
			added = make(map[string]string)
		}
		added[k] = v
	}
	for k := range ep.remove {
		_, inOld := oldEnv[k]
		_, inExplicit := explicitEnv[k]
		if inOld || inExplicit {
			removed = append(removed, k)
		}
	}
	sort.Strings(removed)
	return added, removed
}

// applyPatchAutoExposeEnv resolves the autoExposeEnvKeys for a PATCH that
// carries an envPatch. ac is the live config whose Env has already been
// merged via applyEnvPatch(old.Env, ep); old is the pre-PATCH snapshot (a
// shallow copy, so its CreateInputs is the record recordExplicitEdits has
// already updated); project may be nil.
//
// Keys absent from ep were already kept in ac.Env by applyEnvPatch.
// SCION_AUTO_EXPOSE_PORTS is then resolved by resolveAutoExposeEnv. Its
// explicit tier is ep.set plus, when the request neither set nor explicitly
// removed the key, the previous explicit value from explicitEnvOf(old). The
// project tier therefore overwrites a kept non-explicit value, and never an
// explicit one. The hub default is never written; the broker applies it.
func applyPatchAutoExposeEnv(ac, old *store.AgentAppliedConfig, project *store.Project, ep envPatch) {
	if ac == nil || old == nil || !ep.present {
		return
	}
	if ac.Env == nil {
		ac.Env = make(map[string]string)
	}
	explicit := maps.Clone(ep.set)
	if explicit == nil {
		explicit = make(map[string]string)
	}
	_, sentPorts := ep.set[api.EnvAutoExposePorts]
	if !sentPorts && !ep.remove[api.EnvAutoExposePorts] {
		if v, ok := explicitEnvOf(old)[api.EnvAutoExposePorts]; ok {
			explicit[api.EnvAutoExposePorts] = v
		}
	}
	resolveAutoExposeEnv(ac, project, explicit)
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

// mergePresentInlineFields returns the new live AppliedConfig.InlineConfig
// for a PATCH /api/v1/agents/{id} config edit (ptone/scion#3901): a deep copy
// of oldInline (the pre-PATCH InlineConfig, possibly nil) with only the
// fields whose JSON key is present in the request's raw "config" object
// overwritten from cfg. A field the request does not mention -- volumes,
// skills, mcp_servers, services, command_args, kubernetes, telemetry, env
// and so on -- keeps its live value; a present field, including one sent as
// an explicit empty value or null, replaces it.
//
// present is the same lower-cased, raw-JSON-derived presence set
// recordExplicitEdits uses (keys are lower-cased because encoding/json
// matches struct field names case-insensitively). A nil or empty present
// leaves every field as it was.
//
// cfg must already carry every adjustment applyAgentUpdate makes to the
// request before it becomes live (TZ strip, model alias resolution, the
// auto-expose env carry in applyPatchAutoExposeEnv, dropEchoedInlineImage).
//
// Like recordOtherInlineFieldEdits, this walks the JSON struct tags by
// reflection so a new ScionConfig field is covered without a hand-written
// list.
func mergePresentInlineFields(oldInline, cfg *api.ScionConfig, present map[string]bool) *api.ScionConfig {
	// deepCopyScionConfig returns nil only for a nil input: a JSON round
	// trip of api.ScionConfig cannot fail.
	merged := deepCopyScionConfig(oldInline)
	if merged == nil {
		merged = &api.ScionConfig{}
	}
	if cfg == nil || len(present) == 0 {
		return merged
	}
	dst := reflect.ValueOf(merged).Elem()
	src := reflect.ValueOf(cfg).Elem()
	t := src.Type()
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" || !present[strings.ToLower(name)] {
			continue
		}
		dst.Field(i).Set(src.Field(i))
	}
	return merged
}

// recordReincarnatePatchEdits records a `scion reincarnate` patch
// (ptone/scion#3302) into CreateInputs: the same fields, written the same
// way, as recordExplicitEdits writes for a PATCH, so a later reincarnation
// without the flag replays the value. Unlike a PATCH body, a reincarnate
// flag is never an echo of the live config, so each set field is recorded
// unconditionally, even when it equals the live value: the requester asked
// for it explicitly, and it must not drift with a later template change.
// The model is recorded as given (not alias-resolved), like create's own
// CreateInputs; deriveAgentConfig resolves it against the current harness
// config each generation.
//
// A harness switch (p.HarnessConfig, ptone/scion#7757 §3.3.3) is recorded
// first: the new harness-config name and harness type replace the old ones,
// and the old harness's explicit image, model and harness auth are dropped
// (images, models and auth modes are harness-specific) unless the same
// request sets them again. The drops are recorded too, so a later plain
// reincarnation cannot bring an old-harness value back. The dropped
// values are returned for the plan's warnings.
func recordReincarnatePatchEdits(ci *store.AgentCreateInputs, p *reincarnatePatch) (dropped harnessSwitchDrops) {
	if ci == nil || p == nil {
		return dropped
	}
	ensureInline := func() *api.ScionConfig {
		if ci.InlineConfig == nil {
			ci.InlineConfig = &api.ScionConfig{}
		}
		return ci.InlineConfig
	}
	if p.HarnessConfig != nil {
		ci.HarnessConfig = p.HarnessConfigName
		inline := ensureInline()
		inline.HarnessConfig = p.HarnessConfigName
		inline.Harness = p.HarnessConfig.Harness
		if p.Image == "" && inline.Image != "" {
			dropped.Image = inline.Image
			inline.Image = ""
		}
		if p.Model == "" && inline.Model != "" {
			dropped.Model = inline.Model
			inline.Model = ""
		}
		ci.HarnessAuth = ""
		inline.AuthSelectedType = ""
		// Inline settings that belong to the old harness or its image.
		if inline.ConfigDir != "" {
			dropped.Fields = append(dropped.Fields, "config_dir")
			inline.ConfigDir = ""
		}
		if len(inline.CommandArgs) > 0 {
			dropped.Fields = append(dropped.Fields, "command_args")
			inline.CommandArgs = nil
		}
		if inline.TaskFlag != "" {
			dropped.Fields = append(dropped.Fields, "task_flag")
			inline.TaskFlag = ""
		}
		if inline.User != "" {
			dropped.Fields = append(dropped.Fields, "user")
			inline.User = ""
		}
	}
	if p.Image != "" {
		ensureInline().Image = p.Image
	}
	if p.Model != "" {
		ensureInline().Model = p.Model
	}
	if p.ThinkingLevel != nil {
		tl := *p.ThinkingLevel
		ci.ThinkingLevel = &tl
		inlineTL := tl
		ensureInline().ThinkingLevel = &inlineTL
	}
	if p.HarnessAuth != "" {
		ci.HarnessAuth = p.HarnessAuth
		ensureInline().AuthSelectedType = p.HarnessAuth
	}
	return dropped
}

// harnessSwitchDrops are the explicit old-harness values a harness switch
// dropped from the agent's recorded inputs.
type harnessSwitchDrops struct {
	Image string
	Model string
	// Fields names the other harness-specific inline settings dropped
	// (config_dir, command_args, task_flag, user), by their config key.
	Fields []string
}
