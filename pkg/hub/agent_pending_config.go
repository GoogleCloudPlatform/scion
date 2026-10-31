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
	"reflect"
	"slices"
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
//   - Provision-rendered (T2) keys are held in every phase, unless the
//     agent's broker re-renders them at start (changedFields,
//     ptone/scion#3982; never secrets): then they are written now in a
//     phase with no container and folded by the next start, which lists
//     them in changed_fields, like container keys. Applying the others at
//     a reincarnation is
//     ptone/scion#3978; until then a reincarnation neither applies nor
//     clears pending edits, so they stay pending, and pending T1 keys are
//     still applied at the next start after a reincarnation. The experiment
//     stays off until that lands.
//   - A start applies only what it brings into effect (planPendingFold):
//     a held key whose edit only a reincarnation brings into effect (a
//     clear or removal the broker does not apply at start, a dropped
//     resources sub-field) stays pending as a whole, the parts a start
//     could apply included, and is not written into the applied config
//     (ptone/scion#5864).
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

// pendingDiscardTriggerSuperseded is the trigger of an
// agent_pending_config_discarded record for pending keys a config edit
// written through (held edits off) replaced.
const pendingDiscardTriggerSuperseded = "superseded"

// pendingDiscardTriggerUnappliable is the trigger of an
// agent_pending_config_discarded record for stored pending keys a fold
// removed because they can never be applied.
const pendingDiscardTriggerUnappliable = "unappliable"

// agentViewEdit is the single-agent GET view (?view=edit) that adds
// effectiveNextRun.
const agentViewEdit = "edit"

// pendingConfigSystemActor is the actor of a pending config audit record
// written with no caller identity.
const pendingConfigSystemActor = "pending_config"

// pendingFoldAttempts bounds the start fold's compare-and-set retries.
const pendingFoldAttempts = 3

// errPendingConfigFold marks a start refused before its dispatch because its
// pending config could not be applied and persisted (the agent's row kept
// changing, or the write failed). Nothing was sent to the broker.
var errPendingConfigFold = errors.New("start not sent: the agent's pending config edits could not be applied")

// heldReincarnateWarningPrefix starts the start warning (and status message)
// naming the provision-rendered keys a start leaves pending.
const heldReincarnateWarningPrefix = "Held for reincarnation: "

// heldReincarnateWarningSuffix ends that warning. A reincarnation does not
// apply pending edits yet (ptone/scion#3978): they stay pending through it,
// and pending container keys are still applied at the next start after it.
const heldReincarnateWarningSuffix = ". A reincarnation does not apply pending edits yet; they stay pending."

// heldNotRerenderedNote is the PATCH warning for held provision-rendered
// keys a broker that re-renders at start still does not apply (secrets).
const heldNotRerenderedNote = "held in the pending config; a start does not apply this key, and a reincarnation does not apply pending edits yet, so it stays pending"

// heldProvisionNote is the PATCH warning for held provision-rendered keys.
const heldProvisionNote = "held in the pending config; a start does not re-render prompts, skills or services, and a reincarnation does not apply pending edits yet, so they stay pending"

// heldEditsEnabled reports whether hub.agent_edit_held is on.
func (s *Server) heldEditsEnabled() bool {
	return s.experimentEnabled(experiments.AgentEditHeld)
}

// startApplies reports whether a start, with this broker, applies a
// pending edit of f: a container-creation (T1) config key, or a
// provision-rendered key the broker re-renders at start (not secrets).
func (sup brokerStartSupport) startApplies(f agentEditField) bool {
	return isContainerConfigField(f) || (f.Tier == EditTierProvision && sup.appliesAtStart(f.Key))
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
	// unappliable lists stored pending keys the fold removed because they
	// can never be applied.
	unappliable []string
	// superseded lists pending keys the request wrote now (a phase with no
	// container) that the fold had left pending: a waiting Unlimited
	// (foldKeep). They are removed, so a stale pending value is never
	// applied later over the newer one.
	superseded []string
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
	// What the agent's broker applies at a start (ptone/scion#3982): with
	// changedFields, a provision-rendered key it re-renders is written now
	// in a phase with no container, like a container key, and applied by
	// the next start's fold when held.
	sup := s.agentBrokerStartSupport(ctx, agent)

	isHeld := func(f agentEditField) bool {
		if live {
			return isContainerConfigField(f) || f.Tier == EditTierProvision
		}
		return f.Tier == EditTierProvision && !sup.appliesAtStart(f.Key)
	}
	heldPart := p.subset(isHeld)
	nowPart := p
	if !heldPart.empty() {
		nowPart = p.subset(func(f agentEditField) bool { return !isHeld(f) })
	}

	if !live {
		keep, waiting := sup.foldKeep(agent.PendingConfig)
		out.folded, out.unappliable = s.foldPendingConfig(ctx, agent, pendingFoldPlan{keep: keep, envCanView: storedEnvCanView(agent.PendingConfig)})
		if !nowPart.empty() {
			res := s.applyAgentConfigPatch(ctx, agent, nowPart, canAttachEnv)
			out.warnings = append(out.warnings, res.warnings...)
			out.applied = res.applied
			out.heldForReincarnate = res.heldForReincarnate
			// A waiting key the request writes now is superseded: the
			// fold used to drain every pending container key before the
			// request, and must still leave none older than this write.
			out.superseded = dropPendingKeysNamed(agent, waitingKeysNamed(waiting, nowPart.present))
			waiting = stringsWithout(waiting, out.superseded)
		}
		if w := unlimitedWaitsWarning(waiting); w != "" {
			out.warnings = append(out.warnings, w)
		}
	}
	if !heldPart.empty() {
		h := s.holdAgentConfigPatch(ctx, agent, heldPart, canAttachEnv, sup)
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
// list. Changed keys are split as the fold will apply them for the agent's
// broker (sup), by effect: held (applied at the next container creation) or
// heldForReincarnate (T2 keys the broker does not re-render at start, and
// the clears and removals a start does not apply). That split is decided
// against the applied config, as the fold applies the pending keys to it.
func (s *Server) holdAgentConfigPatch(ctx context.Context, agent *store.Agent, p configPatch, canAttachEnv bool, sup brokerStartSupport) heldPatchResult {
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
	// changed: the keys this save changes from the next-run value (base);
	// only they are stored and reported.
	changed, _ := configEditDisposition(p.raw, &cmp, base, nil, nil, nil)
	// Whether a start applies a changed key is decided as the fold will
	// decide it: by applyAgentConfigPatch's rules, against the applied
	// config (the fold applies the pending keys to it, not to base), with
	// the clears that application would record and the broker's filters.
	var applied *api.ScionConfig
	if agent.AppliedConfig != nil {
		applied = agent.AppliedConfig.InlineConfig
	}
	c := classifyHeldEdits(applied, &cmp, p.raw, p.present, canAttachEnv, sup, changed)
	out.held, out.heldForReincarnate = c.held, c.heldForReincarnate
	out.warnings = append(out.warnings, heldEditWarnings(c.provision, c.cleared, c.removed, sup)...)
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

// heldEditClass is how classifyHeldEdits split a set of candidate keys.
type heldEditClass struct {
	// held and heldForReincarnate are the candidates (sorted wire keys,
	// in candidate order) a start applies, and those it does not bring
	// into effect, so they wait for a reincarnation.
	held, heldForReincarnate []string
	// provision, cleared and removed are the reasons, for heldEditWarnings.
	provision, cleared []string
	removed            map[string][]string
}

// classifyHeldEdits splits candidates, wire keys of the config patch
// (raw, present) decoded as cmp (the model alias resolved), by whether a
// start with this broker (sup) brings each into effect when applied to
// applied: by applyAgentConfigPatch's rules, with the clears that
// application would record (canAttachEnv decides whether env removals are
// recorded) and the broker's filters. It is pure: no store access and no
// authorization. The save (holdAgentConfigPatch) and the fold
// (planPendingFold) both use it, so a key is reported held for
// reincarnation at save exactly when the fold leaves it pending.
//
// The unit is the whole key: a collection key (env, skills, mcp_servers,
// volumes, resources) any part of whose edit a start does not apply is
// held for reincarnation as a whole, including the parts a start could
// apply (ptone/scion#5864).
func classifyHeldEdits(applied, cmp *api.ScionConfig, raw map[string]json.RawMessage, present map[string]bool, canAttachEnv bool, sup brokerStartSupport, candidates []string) heldEditClass {
	var out heldEditClass
	provision, cleared := reincarnateOnlyConfigEdits(raw, cmp, applied)
	removed := removedConfigEntries(applied, cmp, present, canAttachEnv)
	recorded := nextClearedFields(applied, cmp, raw, cleared, canAttachEnv)
	cleared = withDroppedResourcesSubField(cleared, applied, cmp, present)
	provision = sup.heldProvisionEdits(provision, applied, cmp, recorded)
	cleared = sup.heldClearEdits(cleared, recorded, droppedEnvKeys(applied, cmp, present))
	removed = sup.heldRemovals(removed, recorded)
	isCandidate := make(map[string]bool, len(candidates))
	for _, k := range candidates {
		isCandidate[k] = true
	}
	onlyCandidates := func(keys []string) []string {
		var out []string
		for _, k := range keys {
			if isCandidate[k] {
				out = append(out, k)
			}
		}
		return out
	}
	provision, cleared = onlyCandidates(provision), onlyCandidates(cleared)
	for k := range removed {
		if !isCandidate[agentConfigKeyPrefix+k] {
			delete(removed, k)
		}
	}
	// A candidate a start never applies on this broker (a provision key it
	// does not re-render; secrets always) stays pending whatever it is set
	// to, even back to its applied value: it waits for a reincarnation.
	for _, k := range candidates {
		if f, ok := agentEditFieldByKey[k]; ok && !sup.startApplies(f) && !slices.Contains(provision, k) {
			provision = append(provision, k)
		}
	}
	sort.Strings(provision)
	notApplied := map[string]bool{}
	for _, k := range provision {
		notApplied[k] = true
	}
	for _, k := range cleared {
		notApplied[k] = true
	}
	for k := range removed {
		notApplied[agentConfigKeyPrefix+k] = true
	}
	for _, k := range candidates {
		if notApplied[k] {
			out.heldForReincarnate = append(out.heldForReincarnate, k)
		} else {
			out.held = append(out.held, k)
		}
	}
	out.provision, out.cleared, out.removed = provision, cleared, removed
	return out
}

// heldWholeKeyNote is the PATCH warning for held provision-rendered keys
// the broker re-renders at start but whose change a start does not apply.
const heldWholeKeyNote = "the whole key's change is kept pending, including any part a start could apply, until a reincarnation applies pending edits (a start does not apply a clear or a removed skill)"

// heldClearNote is the PATCH warning for held clears a start does not apply.
const heldClearNote = "the clear is kept pending until a reincarnation applies pending edits; the agent keeps its current value (a start does not apply a cleared value)"

// heldEditWarnings returns the PATCH warnings for held keys a start does
// not bring into effect, all of which stay pending, whole, through a start
// (ptone/scion#5864): provision-rendered keys the broker (sup) does not
// re-render at start, those it re-renders but whose change a start does
// not apply (a clear, a removed skill), and T1 clears and removals. A
// collection edit that removes anything stays pending as a whole, its
// additions included.
func heldEditWarnings(provision, cleared []string, removed map[string][]string, sup brokerStartSupport) []string {
	var out []string
	var pending, written []string
	for _, k := range provision {
		if sup.appliesAtStart(k) {
			written = append(written, k)
		} else {
			pending = append(pending, k)
		}
	}
	if len(pending) > 0 {
		note := heldProvisionNote
		if sup.ChangedFields {
			// Only what the broker does not re-render stays pending: secrets.
			note = heldNotRerenderedNote
		}
		out = append(out, strings.Join(pending, ", ")+": "+note)
	}
	if len(written) > 0 {
		out = append(out, strings.Join(written, ", ")+": "+heldWholeKeyNote)
	}
	if len(cleared) > 0 {
		out = append(out, strings.Join(cleared, ", ")+": "+heldClearNote)
	}
	for _, w := range []struct{ key, what string }{
		{"env", "variables"}, {"mcp_servers", "MCP servers"}, {"volumes", "volumes"},
	} {
		if r := removed[w.key]; len(r) > 0 {
			out = append(out, "config."+w.key+": the edit removes "+strings.Join(r, ", ")+", so the whole edit, additions included, is kept pending until a reincarnation applies pending edits; the agent keeps its current "+w.what+" (a start does not remove them)")
		}
	}
	return out
}

// canonicalConfigRaw returns the pending-config form of p: each config key
// p names, under its json key, encoded from the decoded config rather than
// copied from the request bytes. So the stored object has the canonical
// shape at every depth (a key the request spelled in another case, which
// encoding/json accepts, is stored under its json key), response redaction
// applies to every key, and env no longer holds the TZ entry stripped from
// it. A value the request sent as null encodes as null (a nil pointer, map
// or slice) or as its zero value, which the apply treats the same way.
func canonicalConfigRaw(p configPatch) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(p.raw))
	if p.cfg == nil {
		return out
	}
	v := reflect.ValueOf(p.cfg).Elem()
	for rk := range p.raw {
		f, ok := agentConfigEditFieldByLowerKey[strings.ToLower(rk)]
		if !ok {
			continue
		}
		k := strings.TrimPrefix(f.Key, agentConfigKeyPrefix)
		i, ok := scionConfigFieldByJSONKey[k]
		if !ok {
			continue
		}
		b, err := json.Marshal(v.Field(i).Interface())
		if err != nil {
			continue
		}
		out[k] = b
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
			delete(p.raw, k)
			delete(p.present, strings.ToLower(k))
		}
	}
	return p
}

// pendingFoldPlan is what a fold of an agent's pending config applies,
// decided once per compare-and-set attempt on the row that attempt read
// (planPendingFold).
type pendingFoldPlan struct {
	// keep accepts the keys the fold applies now: those a start applies
	// (foldKeep), except the ones only a reincarnation brings into effect.
	keep func(agentEditField) bool
	// waiting: count limits pending as Unlimited the broker cannot apply
	// (foldKeep). reincarnate: keys only a reincarnation brings into
	// effect (classifyHeldEdits). Both stay pending, and are disjoint.
	waiting, reincarnate []string
	// envCanView decides whether the fold applies env removals.
	envCanView bool
}

// planPendingFold decides what a fold of stored's pending config applies
// with the agent's broker (sup). The keys foldKeep accepts are classified
// by classifyHeldEdits against the current applied config and broker, as
// a save classifies them, and those only a reincarnation brings into
// effect are kept pending: a start never writes them into the applied
// config or the create inputs (ptone/scion#5864). A waiting Unlimited is
// never a candidate, so it is never also held for reincarnation.
func (s *Server) planPendingFold(ctx context.Context, stored *store.Agent, sup brokerStartSupport) pendingFoldPlan {
	keep0, waiting := sup.foldKeep(stored.PendingConfig)
	plan := pendingFoldPlan{keep: keep0, waiting: waiting, envCanView: storedEnvCanView(stored.PendingConfig)}
	if stored.PendingConfig.IsEmpty() {
		return plan
	}
	p := pendingConfigPatch(stored.PendingConfig).subset(keep0)
	if p.empty() {
		return plan
	}
	var candidates []string
	for _, f := range configPatchKeys(p.raw) {
		candidates = append(candidates, f.Key)
	}
	// cmp is the pending patch as the fold applies it: the model alias
	// resolved, as at save.
	cmp := *p.cfg
	if cmp.Model != "" {
		cmp.Model = s.resolveModelAliasForAgent(ctx, stored, cmp.Model)
	}
	var applied *api.ScionConfig
	if stored.AppliedConfig != nil {
		applied = stored.AppliedConfig.InlineConfig
	}
	c := classifyHeldEdits(applied, &cmp, p.raw, p.present, plan.envCanView, sup, candidates)
	if len(c.heldForReincarnate) == 0 {
		return plan
	}
	reincarnate := make(map[string]bool, len(c.heldForReincarnate))
	for _, k := range c.heldForReincarnate {
		reincarnate[k] = true
	}
	plan.reincarnate = c.heldForReincarnate
	plan.keep = func(f agentEditField) bool { return keep0(f) && !reincarnate[f.Key] }
	return plan
}

// storedEnvCanView is the env saver's stored env-view decision.
func storedEnvCanView(pc *store.AgentPendingConfig) bool {
	return pc != nil && pc.EnvSaverCanViewEnv
}

// foldPendingConfig applies the pending config's keys that plan.keep accepts to
// agent's applied config, through applyAgentConfigPatch, and removes them
// from the pending config, in memory: the caller persists the agent. A
// stored key that can never be applied (it no longer decodes, or the edit
// table no longer knows it) is removed too. It returns the applied keys and
// the removed unappliable keys, each as sorted wire keys (nil when none).
// A caller that persists the result audits and logs the removed keys
// (auditUnappliablePendingKeys); one that reads a scratch copy ignores them.
// Env removals are applied as plan.envCanView decides.
func (s *Server) foldPendingConfig(ctx context.Context, agent *store.Agent, plan pendingFoldPlan) (folded, unappliable []string) {
	if agent.PendingConfig.IsEmpty() {
		return nil, nil
	}
	full := pendingConfigPatch(agent.PendingConfig)
	rest := agent.PendingConfig.Clone()
	for k := range rest.Config {
		_, decoded := full.raw[k]
		_, known := agentConfigEditFieldByLowerKey[strings.ToLower(k)]
		if !decoded || !known {
			unappliable = append(unappliable, agentConfigKeyPrefix+k)
			delete(rest.Config, k)
		}
	}
	sort.Strings(unappliable)
	p := full.subset(plan.keep)
	if !p.empty() {
		s.applyAgentConfigPatch(ctx, agent, p, plan.envCanView)
		for _, f := range configPatchKeys(p.raw) {
			folded = append(folded, f.Key)
			delete(rest.Config, strings.TrimPrefix(f.Key, agentConfigKeyPrefix))
		}
	}
	if len(folded) > 0 || len(unappliable) > 0 {
		agent.PendingConfig = restOrNil(rest)
	}
	return folded, unappliable
}

// auditUnappliablePendingKeys logs and audits pending keys a persisted fold
// removed because they can never be applied (agent_pending_config_discarded,
// trigger unappliable).
func (s *Server) auditUnappliablePendingKeys(ctx context.Context, agentID string, keys []string) {
	if len(keys) == 0 {
		return
	}
	slog.WarnContext(ctx, "pending config: removed stored keys that cannot be applied", "agent_id", agentID, "keys", keys)
	s.auditPendingConfig(ctx, agentID, mutationTypeAgentPendingConfigDiscarded, keys, pendingDiscardTriggerUnappliable)
}

// restOrNil returns rest, a pending config some keys were removed from, as
// the agent's new pending config: nil when no key is left, and without the
// env saver flag when env is gone.
func restOrNil(rest *store.AgentPendingConfig) *store.AgentPendingConfig {
	if _, ok := rest.Config["env"]; !ok {
		rest.EnvSaverCanViewEnv = false
	}
	if rest.IsEmpty() {
		return nil
	}
	return rest
}

// dropPendingKeysNamed removes from the agent's pending config, in memory,
// every key present names (the lower-cased config keys of a PATCH the hub
// wrote through, with held edits off), so a stale pending value cannot
// later be applied over the newer one. It returns the removed wire keys,
// sorted.
func dropPendingKeysNamed(agent *store.Agent, present map[string]bool) []string {
	if agent.PendingConfig.IsEmpty() || len(present) == 0 {
		return nil
	}
	rest := agent.PendingConfig.Clone()
	var dropped []string
	for k := range rest.Config {
		if present[strings.ToLower(k)] {
			dropped = append(dropped, agentConfigKeyPrefix+k)
			delete(rest.Config, k)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	agent.PendingConfig = restOrNil(rest)
	sort.Strings(dropped)
	return dropped
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
// and the starts of create-on-existing) dispatches them. Only the keys a
// start brings into effect are applied (planPendingFold): a key whose edit
// only a reincarnation brings into effect stays pending, whole, and is not
// written into the applied config (ptone/scion#5864). It returns the keys
// that stay pending for a reincarnation, for the start's warning. With
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
	// Container keys are applied at every start; provision-rendered keys
	// too when the agent's broker re-renders them at start (changedFields,
	// ptone/scion#3982): applyAgentConfigPatch then lists them in
	// changed_fields. Other pending keys stay pending.
	sup := s.agentBrokerStartSupport(ctx, agent)
	for attempt := 0; attempt < pendingFoldAttempts; attempt++ {
		stored, err := s.store.GetAgent(ctx, agent.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errPendingConfigFold, err)
		}
		st := *stored
		st.AppliedConfig = cloneAppliedConfig(stored.AppliedConfig)
		st.PendingConfig = stored.PendingConfig.Clone()
		// A pending Unlimited the broker cannot apply stays pending
		// (foldKeep, ptone/scion#5605), with its own start warning rather
		// than the held-for-reincarnation one; a key only a reincarnation
		// brings into effect stays pending too (planPendingFold).
		plan := s.planPendingFold(ctx, stored, sup)
		folded, unappliable := s.foldPendingConfig(ctx, &st, plan)
		if len(folded) == 0 && len(unappliable) == 0 {
			// Nothing to apply or remove now (another start applied it,
			// it was discarded, or every key stays pending): the caller's
			// copy follows the stored row.
			agent.PendingConfig = stored.PendingConfig
			addUnlimitedWaitsWarning(ctx, plan.waiting)
			return stringsWithout(pendingConfigKeys(stored.PendingConfig), plan.waiting), nil
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
		s.foldPendingConfig(ctx, &mem, plan)
		agent.AppliedConfig = mem.AppliedConfig
		agent.PendingConfig = st.PendingConfig
		if agent.StateVersion == stored.StateVersion {
			agent.StateVersion = version
		}
		if len(folded) > 0 {
			s.auditPendingConfig(ctx, agent.ID, mutationTypeAgentPendingConfigApplied, folded, trigger)
		}
		s.auditUnappliablePendingKeys(ctx, agent.ID, unappliable)
		addUnlimitedWaitsWarning(ctx, plan.waiting)
		return stringsWithout(pendingConfigKeys(st.PendingConfig), plan.waiting), nil
	}
	return nil, fmt.Errorf("%w: %w", errPendingConfigFold, store.ErrVersionConflict)
}

// heldForReincarnateWarning is the start warning (and status message) for
// pending keys a start leaves pending, e.g. "Held for reincarnation:
// system prompt, skills. A reincarnation does not apply pending edits yet;
// they stay pending."; "" when there are none.
func heldForReincarnateWarning(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	labels := make([]string, 0, len(keys))
	for _, k := range keys {
		labels = append(labels, strings.ReplaceAll(strings.TrimPrefix(k, agentConfigKeyPrefix), "_", " "))
	}
	return heldReincarnateWarningPrefix + strings.Join(labels, ", ") + heldReincarnateWarningSuffix
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
	rec := &store.MutationAuditRecord{
		MutationType: mutationType,
		TargetType:   "agent",
		TargetID:     agentID,
		AfterSummary: string(after),
	}
	// A start with no caller identity (a hub-internal wake or resume) is
	// recorded as the hub's own action: the actor columns are required.
	if GetIdentityFromContext(ctx) == nil {
		rec.ActorPrincipalKind = "system"
		rec.ActorPrincipalID = pendingConfigSystemActor
	}
	s.emitMutationAudit(ctx, rec)
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
	_, _ = s.foldPendingConfig(ctx, &scratch, pendingFoldPlan{keep: func(agentEditField) bool { return true }, envCanView: storedEnvCanView(agent.PendingConfig)})
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
