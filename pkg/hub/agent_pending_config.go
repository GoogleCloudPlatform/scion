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
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Held config edits (ptone/scion#3976), behind hub.agent_edit_held.
//
// A config edit the hub cannot apply now is kept as the agent's pending
// config (store.AgentPendingConfig), a stored PATCH body:
//
//   - Container-creation (T1) keys saved while the agent has a live or
//     transitional container (provisioning, cloning, starting, running,
//     stopping) are held and applied at the next container creation: the
//     start fold in startAgentCore covers start, restart, resume (wake) and
//     the starts of create-on-existing.
//   - Provision-rendered (T2) keys are held in every phase: a start does not
//     re-render them, so they wait for a reincarnation (which applies them;
//     not this file).
//
// Applying a held edit runs applyAgentConfigPatch, the code the live PATCH
// runs, so held and immediate edits have one meaning (clears, present-key
// overlay, explicit-edit recording, cleared-field hints).
//
// The pending config is written with the agent's state_version
// compare-and-set: by the PATCH's whole-row UpdateAgent, by the start fold
// and by the discard (UpdateAgentConfig, which leaves status columns alone).

// Audit mutation types for pending config edits.
const (
	mutationTypeAgentPendingConfigSaved     = "agent_pending_config_saved"
	mutationTypeAgentPendingConfigApplied   = "agent_pending_config_applied"
	mutationTypeAgentPendingConfigDiscarded = "agent_pending_config_discarded"
)

// Triggers recorded by agent_pending_config_applied.
const (
	pendingFoldTriggerUpdate  = "update"
	pendingFoldTriggerStart   = "start"
	pendingFoldTriggerResume  = "resume"
	pendingFoldTriggerRestart = "restart"
	pendingFoldTriggerWake    = "wake"
)

// agentViewEdit is the single-agent GET view (?view=edit) that adds
// effectiveNextRun.
const agentViewEdit = "edit"

// pendingFoldAttempts bounds the start fold's compare-and-set retries.
const pendingFoldAttempts = 3

// errPendingConfigFold marks a start refused before its dispatch because its
// pending config could not be applied and persisted (the agent's row kept
// changing, or the write failed). Nothing was sent to the broker.
var errPendingConfigFold = errors.New("start not sent: the agent's pending config edits could not be applied")

// heldReincarnateWarningPrefix starts the start warning (and status message)
// naming the keys that stay pending until a reincarnation.
const heldReincarnateWarningPrefix = "Held for reincarnation: "

// heldEditsEnabled reports whether hub.agent_edit_held is on.
func (s *Server) heldEditsEnabled() bool {
	return s.experimentEnabled(experiments.AgentEditHeld)
}

// isContainerConfigField reports whether f is a container-creation (T1)
// config key: the keys a start applies.
func isContainerConfigField(f agentEditField) bool {
	return f.Tier == EditTierContainer && strings.HasPrefix(f.Key, agentConfigKeyPrefix)
}

// heldConfigUpdate is what applyAgentConfigUpdateHeld did with a PATCH's
// config object.
type heldConfigUpdate struct {
	warnings []string
	// applied, held and heldForReincarnate are the request's changed keys,
	// by when each takes effect (AgentUpdateDisposition).
	applied            []string
	held               []string
	heldForReincarnate []string
	// saved lists the keys this request stored in the pending config, and
	// folded the pending keys applied before the request's own (a PATCH in a
	// phase with no container).
	saved  []string
	folded []string
}

// applyAgentConfigUpdateHeld is the agent PATCH's config step with held
// edits on. In a phase with no container (created, stopped, error, halted,
// suspended) it first applies any pending T1 keys, so the request lands on
// top of them, then writes the request's T1 keys now; with a live or
// transitional container every config key is held. T2 keys are held in
// every phase. Validation runs first, on the whole request, exactly as the
// live PATCH validates. The writes are in memory; the caller persists the
// agent. On a refusal it writes the response and returns false.
func (s *Server) applyAgentConfigUpdateHeld(ctx context.Context, w http.ResponseWriter, agent *store.Agent, p configPatch) (heldConfigUpdate, bool) {
	var out heldConfigUpdate
	live := phaseHasLiveOrTransitionalContainer(agent.Phase)
	if !live && !configPatchPhase(agent.Phase) {
		Conflict(w, "Config cannot be updated in the agent's current phase")
		return out, false
	}
	if !s.validateAgentConfigPatch(ctx, w, agent, p.cfg) {
		return out, false
	}
	canAttachEnv := canViewAgentEnv(ctx, s, agent)

	isHeld := func(f agentEditField) bool {
		return f.Tier == EditTierProvision || (live && isContainerConfigField(f))
	}
	heldPart := p.subset(isHeld)
	nowPart := p
	if !heldPart.empty() {
		nowPart = p.subset(func(f agentEditField) bool { return !isHeld(f) })
	}

	if !live {
		out.folded = s.foldPendingConfig(ctx, agent, isContainerConfigField)
		if !nowPart.empty() {
			res := s.applyAgentConfigPatch(ctx, agent, nowPart, canAttachEnv)
			out.warnings = append(out.warnings, res.warnings...)
			out.applied = res.applied
			out.heldForReincarnate = res.heldForReincarnate
		}
	}
	if !heldPart.empty() {
		h := s.holdAgentConfigPatch(ctx, agent, heldPart, canAttachEnv)
		out.warnings = append(out.warnings, h.warnings...)
		out.held = h.held
		out.heldForReincarnate = sortedUnion(out.heldForReincarnate, h.heldForReincarnate)
		out.saved = h.saved
	}
	return out, true
}

// heldPatchResult is what holdAgentConfigPatch stored.
type heldPatchResult struct {
	warnings           []string
	held               []string
	heldForReincarnate []string
	saved              []string
}

// holdAgentConfigPatch merges p into the agent's pending config, in memory,
// with present-key overlay: each key p changes replaces the pending value
// (a stored null clears the key when applied). A key is changed when it
// differs from the agent's next-run value, its applied config with the
// pending config laid over it; an unchanged key is not stored and is in no
// list. Changed keys are split as the live PATCH splits them, by effect:
// held (applied at the next container creation) or heldForReincarnate (T2
// keys, and T1 clears and removals a start does not apply).
func (s *Server) holdAgentConfigPatch(ctx context.Context, agent *store.Agent, p configPatch, canAttachEnv bool) heldPatchResult {
	var out heldPatchResult
	if stripConfigEnvTZ(p.cfg) {
		out.warnings = append(out.warnings, configEnvTZIgnoredWarning)
	}
	base := effectiveNextRunInline(agent)
	// cmp is the request as it would be applied: the model alias resolved
	// as applyAgentConfigPatch resolves it. The pending config keeps the
	// value as sent; it is resolved again when applied.
	cmp := *p.cfg
	if cmp.Model != "" {
		cmp.Model = s.resolveModelAliasForAgent(ctx, agent, cmp.Model)
	}
	provision, cleared := reincarnateOnlyConfigEdits(p.raw, &cmp, base)
	removed := removedConfigEntries(base, &cmp, p.present, canAttachEnv)
	out.held, out.heldForReincarnate = configEditDisposition(p.raw, &cmp, base, provision, cleared, removed)
	out.warnings = append(out.warnings, heldEditWarnings(provision, cleared, removed)...)
	out.saved = sortedUnion(out.held, out.heldForReincarnate)
	if len(out.saved) == 0 {
		return out
	}

	raw := canonicalConfigRaw(p)
	pending := agent.PendingConfig.Clone()
	if pending == nil {
		pending = &store.AgentPendingConfig{}
	}
	if pending.Config == nil {
		pending.Config = map[string]json.RawMessage{}
	}
	for _, key := range out.saved {
		k := strings.TrimPrefix(key, agentConfigKeyPrefix)
		v, ok := raw[k]
		if !ok {
			continue
		}
		pending.Config[k] = v
		if k == "env" {
			pending.EnvSaverCanViewEnv = canAttachEnv
		}
	}
	pending.SavedBy, pending.SavedByName = pendingSaver(ctx)
	pending.SavedAt = time.Now().UTC()
	pending.BaseGeneration = agent.Generation
	pending.BaseStateVersion = agent.StateVersion
	agent.PendingConfig = pending
	return out
}

// heldEditWarnings returns the PATCH warnings for held keys that take
// effect only at the next reincarnation: provision-rendered keys, and T1
// clears and removals that a start does not apply.
func heldEditWarnings(provision, cleared []string, removed map[string][]string) []string {
	var out []string
	if len(provision) > 0 {
		out = append(out, strings.Join(provision, ", ")+": held until the next reincarnation (a start does not re-render prompts, skills or services)")
	}
	if len(cleared) > 0 {
		out = append(out, strings.Join(cleared, ", ")+": the clear is held; the agent keeps its previous value until the next reincarnation (a start does not apply a cleared value)")
	}
	for _, w := range []struct{ key, what string }{
		{"env", "variables"}, {"mcp_servers", "MCP servers"}, {"volumes", "volumes"},
	} {
		if r := removed[w.key]; len(r) > 0 {
			out = append(out, "config."+w.key+": the removal of "+strings.Join(r, ", ")+" is held; the agent keeps those "+w.what+" until the next reincarnation (a start does not remove them)")
		}
	}
	return out
}

// canonicalConfigRaw returns p's raw config object keyed by each key's json
// key (the request may spell a key in another case; encoding/json matches
// it case-insensitively). A key spelled exactly as its json key wins over
// another spelling. env is re-encoded from the decoded config, so the TZ
// entry stripped from it is not stored; a JSON null is kept as null.
func canonicalConfigRaw(p configPatch) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(p.raw))
	exact := map[string]bool{}
	for rk, rv := range p.raw {
		f, ok := agentConfigEditFieldByLowerKey[strings.ToLower(rk)]
		if !ok {
			continue
		}
		k := strings.TrimPrefix(f.Key, agentConfigKeyPrefix)
		if exact[k] {
			continue
		}
		out[k] = append(json.RawMessage(nil), rv...)
		exact[k] = rk == k
	}
	if v, ok := out["env"]; ok && string(v) != "null" && p.cfg != nil {
		if b, err := json.Marshal(p.cfg.Env); err == nil {
			out["env"] = b
		}
	}
	return out
}

// pendingSaver returns the saving caller as a principal reference and a
// display name, for AgentPendingConfig.SavedBy and SavedByName.
func pendingSaver(ctx context.Context) (string, string) {
	if u := GetUserIdentityFromContext(ctx); u != nil {
		name := u.DisplayName()
		if name == "" {
			name = u.Email()
		}
		return "user:" + u.ID(), name
	}
	if a := GetAgentIdentityFromContext(ctx); a != nil {
		return "agent:" + a.ID(), ""
	}
	if id := GetIdentityFromContext(ctx); id != nil {
		return id.Type() + ":" + id.ID(), ""
	}
	return "", ""
}

// pendingConfigPatch decodes a pending config into the configPatch it
// stores: the raw object, its present keys and its decoded form.
func pendingConfigPatch(pc *store.AgentPendingConfig) configPatch {
	p := configPatch{cfg: &api.ScionConfig{}, raw: map[string]json.RawMessage{}, present: map[string]bool{}}
	if pc.IsEmpty() {
		return p
	}
	for k, v := range pc.Config {
		p.raw[k] = append(json.RawMessage(nil), v...)
		p.present[strings.ToLower(k)] = true
	}
	// Keys are decoded one at a time, so one value that no longer decodes
	// (a stored value of an older shape) drops only that key.
	for k, v := range p.raw {
		one, err := json.Marshal(map[string]json.RawMessage{k: v})
		if err == nil {
			err = json.Unmarshal(one, p.cfg)
		}
		if err != nil {
			slog.Warn("pending config: a stored key could not be decoded; ignoring it", "key", k, "error", err)
			delete(p.raw, k)
			delete(p.present, strings.ToLower(k))
		}
	}
	return p
}

// foldPendingConfig applies the pending config's keys that keep accepts to
// agent's applied config, through applyAgentConfigPatch, and removes them
// from the pending config, in memory: the caller persists the agent. It
// returns the applied keys as sorted wire keys, nil when there were none.
func (s *Server) foldPendingConfig(ctx context.Context, agent *store.Agent, keep func(agentEditField) bool) []string {
	if agent.PendingConfig.IsEmpty() {
		return nil
	}
	p := pendingConfigPatch(agent.PendingConfig).subset(keep)
	if p.empty() {
		return nil
	}
	s.applyAgentConfigPatch(ctx, agent, p, agent.PendingConfig.EnvSaverCanViewEnv)
	rest := agent.PendingConfig.Clone()
	var folded []string
	for _, f := range configPatchKeys(p.raw) {
		folded = append(folded, f.Key)
		delete(rest.Config, strings.TrimPrefix(f.Key, agentConfigKeyPrefix))
	}
	if _, ok := rest.Config["env"]; !ok {
		rest.EnvSaverCanViewEnv = false
	}
	if rest.IsEmpty() {
		rest = nil
	}
	agent.PendingConfig = rest
	return folded
}

// pendingConfigKeys returns the pending config's keys as sorted wire keys.
func pendingConfigKeys(pc *store.AgentPendingConfig) []string {
	if pc.IsEmpty() {
		return nil
	}
	out := make([]string, 0, len(pc.Config))
	for k := range pc.Config {
		out = append(out, agentConfigKeyPrefix+k)
	}
	sort.Strings(out)
	return out
}

// startFoldTrigger names the start kind for agent_pending_config_applied.
func startFoldTrigger(opts StartOpts) string {
	switch {
	case opts.Kind == store.StartClaimRestart:
		return pendingFoldTriggerRestart
	case opts.Kind == store.StartClaimWake:
		return pendingFoldTriggerWake
	case opts.Resume:
		return pendingFoldTriggerResume
	}
	return pendingFoldTriggerStart
}

// foldPendingConfigAtStart applies the agent's pending T1 keys before a
// start is dispatched: startAgentCore calls it under the start claim, after
// the fence check, so every start path (start, restart, resume and wake,
// and the starts of create-on-existing) dispatches them. It returns the
// keys that stay pending (T2), for the start's warning. With
// hub.agent_edit_held off it does nothing: stored pending edits stay inert.
//
// The fold runs twice, with the same pending keys. On the stored row, read
// here: its result is persisted with the state_version compare-and-set
// (UpdateAgentConfig, which leaves the row's status columns, already moved
// on by the start, as they are), and a conflict re-reads the row and runs
// again. And on the caller's copy of the agent, whose applied config the
// dispatch sends and may hold start inputs not yet written (a task), so
// they are neither lost nor written early. The caller's copy takes the new
// state version only if it was current; a stale copy keeps its version, so
// its later whole-row write still fails its compare-and-set as before.
func (s *Server) foldPendingConfigAtStart(ctx context.Context, agent *store.Agent, trigger string) ([]string, error) {
	if !s.heldEditsEnabled() || agent.PendingConfig.IsEmpty() {
		return nil, nil
	}
	for attempt := 0; attempt < pendingFoldAttempts; attempt++ {
		stored, err := s.store.GetAgent(ctx, agent.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errPendingConfigFold, err)
		}
		st := *stored
		st.AppliedConfig = cloneAppliedConfig(stored.AppliedConfig)
		st.PendingConfig = stored.PendingConfig.Clone()
		folded := s.foldPendingConfig(ctx, &st, isContainerConfigField)
		if len(folded) == 0 {
			// Nothing to apply now (another start applied it, or it was
			// discarded): the caller's copy follows the stored row.
			agent.PendingConfig = stored.PendingConfig
			return pendingConfigKeys(stored.PendingConfig), nil
		}
		version, err := s.store.UpdateAgentConfig(ctx, agent.ID, stored.StateVersion, st.AppliedConfig, st.PendingConfig)
		if errors.Is(err, store.ErrVersionConflict) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errPendingConfigFold, err)
		}
		mem := *agent
		mem.AppliedConfig = cloneAppliedConfig(agent.AppliedConfig)
		mem.PendingConfig = stored.PendingConfig.Clone()
		s.foldPendingConfig(ctx, &mem, isContainerConfigField)
		agent.AppliedConfig = mem.AppliedConfig
		agent.PendingConfig = st.PendingConfig
		if agent.StateVersion == stored.StateVersion {
			agent.StateVersion = version
		}
		s.auditPendingConfig(ctx, agent.ID, mutationTypeAgentPendingConfigApplied, folded, trigger)
		return pendingConfigKeys(st.PendingConfig), nil
	}
	return nil, fmt.Errorf("%w: %w", errPendingConfigFold, store.ErrVersionConflict)
}

// heldForReincarnateWarning is the start warning (and status message) for
// pending keys a start leaves pending, e.g. "Held for reincarnation:
// system prompt, skills"; "" when there are none.
func heldForReincarnateWarning(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	labels := make([]string, 0, len(keys))
	for _, k := range keys {
		labels = append(labels, strings.ReplaceAll(strings.TrimPrefix(k, agentConfigKeyPrefix), "_", " "))
	}
	return heldReincarnateWarningPrefix + strings.Join(labels, ", ")
}

// noteHeldForReincarnate reports the keys a start left pending: a start
// warning, and the agent's status message. Called after the start's own
// status write, so the message is not cleared by it; a later status report
// replaces it as usual.
func (s *Server) noteHeldForReincarnate(ctx context.Context, agent *store.Agent, keys []string) {
	msg := heldForReincarnateWarning(keys)
	if msg == "" {
		return
	}
	addDispatchWarnings(ctx, msg)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.store.UpdateAgentStatus(wctx, agent.ID, store.AgentStatusUpdate{Message: msg}); err != nil {
		slog.Warn("start: recording the held-for-reincarnation message failed", "agent_id", agent.ID, "error", err)
		return
	}
	agent.Message = msg
}

// auditPendingConfig writes a pending config audit record: keys are the
// wire keys concerned and trigger, for an application, what applied them.
func (s *Server) auditPendingConfig(ctx context.Context, agentID, mutationType string, keys []string, trigger string) {
	summary := struct {
		Keys    []string `json:"keys"`
		Trigger string   `json:"trigger,omitempty"`
	}{Keys: keys, Trigger: trigger}
	if summary.Keys == nil {
		summary.Keys = []string{}
	}
	after, err := json.Marshal(summary)
	if err != nil {
		after = nil
	}
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType: mutationType,
		TargetType:   "agent",
		TargetID:     agentID,
		AfterSummary: string(after),
	})
}

// cloneAppliedConfig returns a deep copy of ac (nil for nil). It round-trips
// through JSON as persistence does, bypassing the response redaction of
// AgentAppliedConfig.MarshalJSON, so Env is copied too.
func cloneAppliedConfig(ac *store.AgentAppliedConfig) *store.AgentAppliedConfig {
	if ac == nil {
		return nil
	}
	type raw store.AgentAppliedConfig
	data, err := json.Marshal((*raw)(ac))
	if err != nil {
		cp := *ac
		return &cp
	}
	var out raw
	if err := json.Unmarshal(data, &out); err != nil {
		cp := *ac
		return &cp
	}
	cfg := store.AgentAppliedConfig(out)
	return &cfg
}

// effectiveNextRunInline returns the agent's inline config as its next run
// would see it: the applied inline config with every pending config key
// laid over it (present-key overlay). It is a fresh copy.
func effectiveNextRunInline(agent *store.Agent) *api.ScionConfig {
	var base *api.ScionConfig
	if agent.AppliedConfig != nil {
		base = agent.AppliedConfig.InlineConfig
	}
	if agent.PendingConfig.IsEmpty() {
		return deepCopyScionConfig(base)
	}
	p := pendingConfigPatch(agent.PendingConfig)
	return mergePresentInlineFields(base, p.cfg, p.present)
}

// effectiveNextRun returns the agent's applied config with every pending
// key applied, as applying them would write it (AppliedConfig ⊕
// pendingConfig), without persisting anything. CreateInputs is left out.
// It returns the applied config itself when nothing is pending.
func (s *Server) effectiveNextRun(ctx context.Context, agent *store.Agent) *store.AgentAppliedConfig {
	if agent.PendingConfig.IsEmpty() {
		return agent.AppliedConfig
	}
	scratch := *agent
	scratch.AppliedConfig = cloneAppliedConfig(agent.AppliedConfig)
	scratch.PendingConfig = agent.PendingConfig.Clone()
	s.foldPendingConfig(ctx, &scratch, func(agentEditField) bool { return true })
	if scratch.AppliedConfig != nil {
		scratch.AppliedConfig.CreateInputs = nil
	}
	return scratch.AppliedConfig
}

// agentPendingConfigDiscardResponse is the DELETE pending-config response:
// the agent as written and the keys discarded.
type agentPendingConfigDiscardResponse struct {
	*store.Agent
	Discarded []string `json:"discarded"`
}

// handleAgentPendingConfig serves /api/v1/agents/{id}/pending-config.
func (s *Server) handleAgentPendingConfig(w http.ResponseWriter, r *http.Request, id string) {
	if !s.heldEditsEnabled() {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodDelete {
		MethodNotAllowed(w, http.MethodDelete)
		return
	}
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	s.discardAgentPendingConfig(w, r, agent)
}

// discardAgentPendingConfig clears the agent's pending config (agent.update
// on the agent). An optional stateVersion query parameter must match the
// agent's. Discarding nothing answers 200 with no keys and writes nothing.
func (s *Server) discardAgentPendingConfig(w http.ResponseWriter, r *http.Request, agent *store.Agent) {
	ctx := r.Context()
	if !s.authorize(w, r, agentResource(agent), ActionUpdate) {
		return
	}
	if !agent.DeletedAt.IsZero() {
		Conflict(w, "The agent is deleted and cannot be edited")
		return
	}
	if v := r.URL.Query().Get("stateVersion"); v != "" {
		want, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			BadRequest(w, "stateVersion must be an integer")
			return
		}
		if want != agent.StateVersion {
			Conflict(w, "Version conflict - resource was modified")
			return
		}
	}
	discarded := pendingConfigKeys(agent.PendingConfig)
	if len(discarded) > 0 {
		version, err := s.store.UpdateAgentConfig(ctx, agent.ID, agent.StateVersion, agent.AppliedConfig, nil)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		agent.PendingConfig = nil
		agent.StateVersion = version
		s.auditPendingConfig(ctx, agent.ID, mutationTypeAgentPendingConfigDiscarded, discarded, "")
	} else {
		discarded = []string{}
	}
	writeJSON(w, http.StatusOK, agentPendingConfigDiscardResponse{
		Agent:     redactedAgentCopy(ctx, s, agent),
		Discarded: discarded,
	})
}

// sortedUnion returns the sorted union of a and b, nil when both are empty.
func sortedUnion(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	set := make(map[string]bool, len(a)+len(b))
	for _, k := range a {
		set[k] = true
	}
	for _, k := range b {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
