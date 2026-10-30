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
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// configPatch is the config object of an agent PATCH body: cfg is its
// decoded form, raw the raw object (keys as the request spelled them) and
// present its keys, lower-cased (encoding/json matches field names
// case-insensitively, and the presence checks must agree with it).
//
// A pending edit (store.AgentPendingConfig) is a stored configPatch, so the
// live PATCH and the application of a held edit run the same code:
// applyAgentConfigPatch.
type configPatch struct {
	cfg     *api.ScionConfig
	raw     map[string]json.RawMessage
	present map[string]bool
}

// empty reports whether p names no key.
func (p configPatch) empty() bool {
	return p.cfg == nil || len(p.present) == 0
}

// subset returns the part of p that names only the keys whose table row
// keep accepts. Config keys the table does not know are dropped (they were
// not decoded either). The decoded config is a copy with every other field
// zeroed, so p is left as it was.
func (p configPatch) subset(keep func(agentEditField) bool) configPatch {
	out := configPatch{
		raw:     map[string]json.RawMessage{},
		present: map[string]bool{},
	}
	for rk, rv := range p.raw {
		f, ok := agentConfigEditFieldByLowerKey[strings.ToLower(rk)]
		if !ok || !keep(f) {
			continue
		}
		out.raw[rk] = rv
		out.present[strings.ToLower(rk)] = true
	}
	if p.cfg == nil {
		return out
	}
	// A struct copy, not a JSON round trip: the omitempty tags would turn
	// an explicit empty value (an emptied env map) into an absent one. Env
	// is copied, since the TZ strip edits it in place.
	cp := *p.cfg
	cp.Env = maps.Clone(p.cfg.Env)
	out.cfg = &cp
	v := reflect.ValueOf(out.cfg).Elem()
	for i := 0; i < v.NumField(); i++ {
		k := scionConfigJSONKey(v.Type().Field(i))
		if k == "" || out.present[strings.ToLower(k)] {
			continue
		}
		v.Field(i).Set(reflect.Zero(v.Field(i).Type()))
	}
	return out
}

// validateAgentConfigPatch runs the agent PATCH's config validation, which
// writes nothing: the harness capability check and the thinking-level
// range. On a failure it writes the error response and returns false. A
// held edit is validated by the same call when it is saved; the harness is
// fixed after create, so the check still holds when the edit is applied.
func (s *Server) validateAgentConfigPatch(ctx context.Context, w http.ResponseWriter, agent *store.Agent, cfg *api.ScionConfig) bool {
	resolvedHarness, harnessCaps := s.resolveAgentHarnessCapabilities(ctx, agent)
	if issues := validateConfigAgainstHarnessCapabilities(cfg, harnessCaps); len(issues) > 0 {
		ValidationError(w, "Config contains unsupported fields for harness "+resolvedHarness, map[string]interface{}{
			"harness": resolvedHarness,
			"fields":  issues,
		})
		return false
	}
	if cfg.ThinkingLevel != nil {
		if tl := *cfg.ThinkingLevel; tl < 0 || tl > 100 {
			BadRequest(w, "thinking_level must be between 0 and 100")
			return false
		}
	}
	return s.validateUnlimitedCounts(ctx, w, agent, cfg)
}

// validateUnlimitedCounts refuses a change of a count limit to Unlimited (0)
// that the agent's broker cannot apply (ptone/scion#5605): 422 with the
// per-field reasons, or 503 when the broker record cannot be read. Only a
// change counts, against the value the agent's next run would otherwise
// use: with held edits on, the applied inline config overlaid by the
// pending config, except that a count limit pending as 0 counts at its
// applied value (unlimitedCheckBase); with them off, pending rows are
// inert, so the applied inline config alone. The broker is looked up only when there is such a
// change. It runs for the live PATCH and for the held save alike.
func (s *Server) validateUnlimitedCounts(ctx context.Context, w http.ResponseWriter, agent *store.Agent, cfg *api.ScionConfig) bool {
	var applied *api.ScionConfig
	if agent != nil && agent.AppliedConfig != nil {
		applied = agent.AppliedConfig.InlineConfig
	}
	base := applied
	if s.heldEditsEnabled() {
		base = unlimitedCheckBase(agent, applied)
	}
	fields, err := s.unlimitedCountRefusal(ctx, agent, cfg, base)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"The agent's runtime broker could not be read to check that it supports Unlimited; retry", nil)
		return false
	}
	if len(fields) == 0 {
		return true
	}
	details := make(map[string]interface{}, len(fields))
	for k, v := range fields {
		details[k] = v
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError,
		"Unlimited is not supported for this agent's runtime broker; reload the page, or upgrade the broker",
		map[string]interface{}{"fields": details})
	return false
}

// configPatchResult is what applyAgentConfigPatch did: the warnings for the
// response, and the changed keys split by when they take effect (see
// configEditDisposition).
type configPatchResult struct {
	warnings           []string
	applied            []string
	heldForReincarnate []string
}

// stripConfigEnvTZ removes TZ from cfg's env: config.env never sets the
// agent timezone; explicitTimezone does. It reports whether a non-empty TZ
// was dropped, which the response warns about (an empty value is a marker
// the configure page may round-trip, so it is dropped silently).
func stripConfigEnvTZ(cfg *api.ScionConfig) bool {
	if cfg == nil {
		return false
	}
	v, ok := cfg.Env[agentTZEnvKey]
	if !ok {
		return false
	}
	delete(cfg.Env, agentTZEnvKey)
	return v != ""
}

// applyAgentConfigPatch writes an agent PATCH's config object into the
// agent's applied config, in memory: the caller persists the agent. It is
// the single apply path of config edits, for a live PATCH and for a held
// edit applied later (foldPendingConfig). p must have passed
// validateAgentConfigPatch. canAttachEnv is whether the caller who sent the
// edit could read the agent's env (canViewAgentEnv): it gates the env
// removal rules.
func (s *Server) applyAgentConfigPatch(ctx context.Context, agent *store.Agent, p configPatch, canAttachEnv bool) configPatchResult {
	var res configPatchResult
	if agent.AppliedConfig == nil {
		agent.AppliedConfig = &store.AgentAppliedConfig{}
	}
	cfg := p.cfg
	var modelSize string
	if cfg.Model != "" {
		// Resolved here, ahead of both recordExplicitEdits and the live
		// write below, so both compare/store the same resolved value
		// the requester's alias (if any) maps to -- not the raw alias
		// they typed. This also ensures InlineConfig (set later)
		// carries the resolved value.
		// The size alias, if any, is taken first: it replaces the
		// recorded size with the model below, so an explicit ID
		// clears it. Declined limitation (#5502 review M2):
		// CreateInputs records the resolved model, not the alias, so
		// a reincarnation after this PATCH re-derives no size. The
		// badge is display-only, and keeping the alias would change
		// the reincarnate explicit-input contract.
		modelSize = config.ModelSizeAlias(cfg.Model)
		cfg.Model = s.resolveModelAliasForAgent(ctx, agent, cfg.Model)
	}

	// old is a snapshot of the live config exactly as it stood before
	// any of the writes below, for recordExplicitEdits' diff (Option C,
	// ptone/scion#2493, invariant E). Taken after thinking-level
	// validation and model-alias resolution (pure reads) but before the
	// first assignment into agent.AppliedConfig itself.
	old := *agent.AppliedConfig

	// The TZ strip runs here, between the `old` snapshot above and the
	// recordExplicitEdits call below -- never after it. See
	// recordExplicitEdits' doc comment for why.
	if stripConfigEnvTZ(cfg) {
		res.warnings = append(res.warnings, configEnvTZIgnoredWarning)
	}
	// canAttachEnv is the same attach-equivalent-access gate the GET
	// response's Env redaction uses (ResponseView). A caller who fails
	// it never saw the live Env to begin with, so their request's env
	// map cannot be trusted to list every key that still exists live --
	// see recordExplicitEdits' env-removal gate. It also gates the env
	// removals below (removedConfigEntries, nextClearedFields).
	if agent.AppliedConfig.CreateInputs != nil {
		recordExplicitEdits(agent.AppliedConfig.CreateInputs, &old, cfg, p.present,
			dispatchImageRegistry(s.GetDispatcher(), agent), canAttachEnv)
	}

	if cfg.Image != "" {
		agent.AppliedConfig.Image = cfg.Image
	}
	if cfg.Model != "" {
		agent.AppliedConfig.Model = cfg.Model
		agent.AppliedConfig.ModelSize = modelSize
	}
	// Thinking level is applied only when the request names it; an
	// explicit null unsets it, an absent key leaves it alone.
	if p.present["thinking_level"] {
		agent.AppliedConfig.ThinkingLevel = cfg.ThinkingLevel
	}
	if cfg.Task != "" {
		agent.AppliedConfig.Task = cfg.Task
	}
	if cfg.AuthSelectedType != "" {
		agent.AppliedConfig.HarnessAuth = cfg.AuthSelectedType
	}
	if cfg.Env != nil {
		// AppliedConfig.Env is a copy, so the auto-expose resolution
		// below never leaks a derived value into InlineConfig.Env.
		agent.AppliedConfig.Env = maps.Clone(cfg.Env)
		project, err := s.store.GetProject(ctx, agent.ProjectID)
		if err != nil {
			slog.WarnContext(ctx, "applyAgentUpdate: project lookup failed; auto-expose project tier not re-derived",
				"agent", agent.ID, "project", agent.ProjectID, "error", err)
			project = nil
		}
		applyPatchAutoExposeEnv(agent.AppliedConfig, &old, project, cfg.Env)
	}
	dropEchoedInlineImage(cfg, &old, dispatchImageRegistry(s.GetDispatcher(), agent))
	// Start from the live InlineConfig and overlay only the keys the
	// request names (ptone/scion#3901). The configure page does not
	// render volumes, skills, MCP servers, services, command args or
	// kubernetes, and sends telemetry and env only when touched, so a
	// wholesale replace would wipe them on every Save and Start.
	// The harness cannot change here: harness and harness_config are
	// fixed keys, refused when they would change. So the harness check of
	// validateAgentConfigPatch covers every key the request sends, and
	// kept keys are left as they were.
	merged := mergePresentInlineFields(old.InlineConfig, cfg, p.present)
	agent.AppliedConfig.InlineConfig = merged
	// provision, cleared and removed are computed once and drive both
	// the reincarnation warnings and the disposition, so a key warned
	// as taking effect at the next reincarnation is the key reported
	// held for it.
	provision, cleared := reincarnateOnlyConfigEdits(p.raw, cfg, old.InlineConfig)
	removed := removedConfigEntries(old.InlineConfig, cfg, p.present, canAttachEnv)
	// The broker's start-time merge keeps a previous value the inline
	// config leaves empty; the cleared paths it sends with each start
	// let the broker remove them (ptone/scion#3906).
	merged.ClearedFields = nextClearedFields(old.InlineConfig, cfg, p.raw, cleared, canAttachEnv)
	// The provision-rendered keys changed since creation, which a
	// broker with changedFields re-renders at start; for such a broker
	// they take effect at the next start, not at the next
	// reincarnation (ptone/scion#3982).
	merged.ChangedFields = nextChangedFields(old.InlineConfig, provision)
	// A resources edit that drops a stored sub-field keeps that
	// sub-field at start (the merge applies only non-empty values) and
	// records no clear, so it waits for the next reincarnation
	// (ptone/scion#5676). Added after the clears are recorded:
	// recording it would revert the whole resources value.
	cleared = withDroppedResourcesSubField(cleared, old.InlineConfig, cfg, p.present)
	// A broker with clearedFields applies the clears recorded above at
	// the next start (ptone/scion#4890), so only the rest are held.
	// What is reported applied follows what was recorded above.
	sup := s.agentBrokerStartSupport(ctx, agent)
	provision = sup.heldProvisionEdits(provision, old.InlineConfig, cfg, merged.ClearedFields)
	cleared = sup.heldClearEdits(cleared, merged.ClearedFields, droppedEnvKeys(old.InlineConfig, cfg, p.present))
	removed = sup.heldRemovals(removed, merged.ClearedFields)
	res.warnings = append(res.warnings, sup.reincarnateOnlyEditWarnings(provision, cleared)...)
	res.warnings = append(res.warnings, removedEntriesWarnings(removed)...)
	res.applied, res.heldForReincarnate = configEditDisposition(p.raw, cfg, old.InlineConfig, provision, cleared, removed)
	return res
}

// unlimitedCheckBase is the base validateUnlimitedCounts compares a request
// with when held edits are on: the agent's next-run inline config
// (effectiveNextRunInline), but with each count limit pending as 0 taken
// from applied instead. A pending 0 may be waiting for a broker that can
// apply it (foldKeep), so it is not known to be the next run's value, and a
// request that sends 0 again must still be checked against the broker:
// otherwise a PATCH of an agent with no container would write the 0 to the
// applied config and report it applied on a broker that keeps the inherited
// limit. On a capable broker the extra check refuses nothing.
func unlimitedCheckBase(agent *store.Agent, applied *api.ScionConfig) *api.ScionConfig {
	base := effectiveNextRunInline(agent)
	if agent.PendingConfig.IsEmpty() {
		return base
	}
	p := pendingConfigPatch(agent.PendingConfig)
	if base == nil {
		base = &api.ScionConfig{}
	}
	var appliedTurns, appliedCalls *int
	if applied != nil {
		appliedTurns, appliedCalls = applied.MaxTurns, applied.MaxModelCalls
	}
	if p.present["max_turns"] && api.IsUnlimitedCount(p.cfg.MaxTurns) {
		base.MaxTurns = api.CloneIntPtr(appliedTurns)
	}
	if p.present["max_model_calls"] && api.IsUnlimitedCount(p.cfg.MaxModelCalls) {
		base.MaxModelCalls = api.CloneIntPtr(appliedCalls)
	}
	return base
}
