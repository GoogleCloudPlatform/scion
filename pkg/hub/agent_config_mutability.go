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
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Agent config mutability: which agent fields can be edited after creation,
// and when an edit takes effect (ptone/scion#3952, ptone/scion#3972).
//
// An edit's fate depends on what the agent's next transition re-runs on the
// broker, so every field is placed in an edit tier:
//
//   - T0, immediate metadata: not part of the launch spec; takes effect at
//     once in any phase (name, labels, annotations, task summary, message
//     mode).
//   - T1, container creation: re-applied by any start, restart, resume or
//     reincarnation. The broker merges the hub's inline config into the
//     agent's persisted config and recreates the container (model, limits,
//     image, env, ...). Clearing or zeroing a T1 value does not reach the
//     broker's persisted config through a start (its merge keeps non-empty
//     values), so a clear only takes effect at the next reincarnation.
//   - T2, provision-rendered: rendered into files when the agent is
//     provisioned, so only a reincarnation (which reprovisions) applies a
//     change (system prompt, agent instructions, skills, services).
//   - T3, principal: changes the agent's authority or identity; applied only
//     by an explicit reincarnation, authorized against the requester
//     (role, GCP identity).
//   - TX, immutable: no transition applies a change (project, template,
//     harness config, broker, profile, branch, workspace).
//
// The agent's phase then decides each field's disposition: written now,
// held until the next container creation, held until a reincarnation,
// immediate, or locked. editDisposition is that rule and agentEditFields is
// the table; TestAgentEditTable_EveryKeyEveryPhase enumerates both, and
// fails when a ScionConfig key is added without a table entry.

// EditTier classifies an agent field by what re-applies an edit to it.
type EditTier string

const (
	EditTierMetadata  EditTier = "T0"
	EditTierContainer EditTier = "T1"
	EditTierProvision EditTier = "T2"
	EditTierPrincipal EditTier = "T3"
	EditTierImmutable EditTier = "TX"
)

// agentConfigKeyPrefix prefixes the wire key of a key of the PATCH body's
// config object.
const agentConfigKeyPrefix = "config."

// EditDisposition is what happens to an edit of a field in the agent's
// current phase.
type EditDisposition string

const (
	// EditImmediate takes effect at once.
	EditImmediate EditDisposition = "immediate"
	// EditNow is written to the agent's applied config and takes effect at
	// the next container creation (start, restart, resume, reincarnation).
	EditNow EditDisposition = "now"
	// EditHeld is kept aside while a container is live and applied at the
	// next container creation.
	EditHeld EditDisposition = "held"
	// EditReincarnate takes effect only at the next reincarnation.
	EditReincarnate EditDisposition = "reincarnate"
	// EditLocked cannot be edited.
	EditLocked EditDisposition = "locked"
)

// agentEditField is one row of the mutability table.
type agentEditField struct {
	// Key is the field's wire key: "config.<json key>" for a key of the
	// PATCH body's config object, else the top-level request key.
	Key  string
	Tier EditTier
	// SessionSensitive marks a T1 field that a resume passes to the harness
	// unchanged, but whose effect on a continued conversation depends on
	// the harness.
	SessionSensitive bool
	// NotOnResume marks a T1 field a resume does not send (the task).
	NotOnResume bool
	// AnyPhase marks a T1 field the hub already stores in every phase and
	// applies at the next start (the timezone pin).
	AnyPhase bool
	// CreatedDisposition, when set, is the field's disposition in the
	// created phase instead of the tier's.
	CreatedDisposition EditDisposition
	// LockedReason explains a TX field, or a CreatedDisposition of locked.
	LockedReason string
}

// Locked reasons shared by several rows or phases.
const (
	editReasonImmutable      = "Fixed when the agent was created; no transition applies a change."
	editReasonWorkspace      = "The workspace is provisioned when the agent is created; no transition applies a change."
	editReasonInternal       = "Managed by Scion; not user-editable."
	editReasonDeleted        = "The agent is deleted."
	editReasonNotOnResume    = "The task is not sent again when a suspended agent resumes."
	editReasonRoleCreated    = "The role cannot change before the first start. Delete and recreate the agent, or reincarnate it after it has started."
	editReasonRunning        = "Editing a running agent is not available yet."
	editReasonNoUpdate       = "You do not have permission to edit this agent."
	editReasonCannotDelegate = "You cannot delegate this agent's role, so you cannot change it."
	editReasonUnknownPhase   = "The agent's phase does not allow edits."
	editReasonFixedPatch     = "Set when the agent was created; recreate the agent, or change the template and reincarnate it."
)

// agentEditFields is the mutability table: every field the agent form shows
// and every key of the agent's inline config (api.ScionConfig).
var agentEditFields = []agentEditField{
	// T0: immediate metadata.
	{Key: "name", Tier: EditTierMetadata},
	{Key: "labels", Tier: EditTierMetadata},
	{Key: "annotations", Tier: EditTierMetadata},
	{Key: "taskSummary", Tier: EditTierMetadata},
	{Key: "messageMode", Tier: EditTierMetadata},

	// T1: applied at any container creation.
	{Key: "config.model", Tier: EditTierContainer, SessionSensitive: true},
	{Key: "config.thinking_level", Tier: EditTierContainer, SessionSensitive: true},
	{Key: "config.auth_selectedType", Tier: EditTierContainer, SessionSensitive: true},
	{Key: "config.image", Tier: EditTierContainer},
	{Key: "config.user", Tier: EditTierContainer},
	{Key: "config.max_turns", Tier: EditTierContainer},
	{Key: "config.max_model_calls", Tier: EditTierContainer},
	{Key: "config.max_duration", Tier: EditTierContainer},
	{Key: "config.resources", Tier: EditTierContainer},
	{Key: "config.env", Tier: EditTierContainer},
	{Key: "config.telemetry", Tier: EditTierContainer},
	{Key: "config.mcp_servers", Tier: EditTierContainer},
	{Key: "config.volumes", Tier: EditTierContainer},
	{Key: "config.kubernetes", Tier: EditTierContainer},
	{Key: "config.command_args", Tier: EditTierContainer},
	{Key: "config.task_flag", Tier: EditTierContainer},
	{Key: "config.task", Tier: EditTierContainer, NotOnResume: true},
	{Key: "explicitTimezone", Tier: EditTierContainer, AnyPhase: true},

	// T2: rendered at provision; applied only by a reincarnation.
	{Key: "config.system_prompt", Tier: EditTierProvision},
	{Key: "config.agent_instructions", Tier: EditTierProvision},
	{Key: "config.skills", Tier: EditTierProvision},
	{Key: "config.services", Tier: EditTierProvision},
	{Key: "config.secrets", Tier: EditTierProvision},

	// T3: principal; applied only by an explicit reincarnation.
	{Key: "agentRole", Tier: EditTierPrincipal, CreatedDisposition: EditLocked, LockedReason: editReasonRoleCreated},
	{Key: "gcp_identity", Tier: EditTierPrincipal, CreatedDisposition: EditNow},

	// TX: immutable.
	{Key: "projectId", Tier: EditTierImmutable, LockedReason: editReasonImmutable},
	{Key: "template", Tier: EditTierImmutable, LockedReason: editReasonImmutable},
	{Key: "harnessConfig", Tier: EditTierImmutable, LockedReason: editReasonImmutable},
	{Key: "runtimeBrokerId", Tier: EditTierImmutable, LockedReason: "Moving an agent to another broker is done by reincarnating it with a target broker."},
	{Key: "profile", Tier: EditTierImmutable, LockedReason: editReasonImmutable},
	{Key: "branch", Tier: EditTierImmutable, LockedReason: editReasonWorkspace},
	{Key: "config.branch", Tier: EditTierImmutable, LockedReason: editReasonWorkspace},
	{Key: "config.clone_depth", Tier: EditTierImmutable, LockedReason: editReasonWorkspace},
	{Key: "config.explicit_workspace", Tier: EditTierImmutable, LockedReason: editReasonWorkspace},
	{Key: "config.empty_per_agent_workspace", Tier: EditTierImmutable, LockedReason: editReasonWorkspace},
	{Key: "config.harness", Tier: EditTierImmutable, LockedReason: editReasonImmutable},
	{Key: "config.harness_config", Tier: EditTierImmutable, LockedReason: editReasonImmutable},
	{Key: "config.default_harness_config", Tier: EditTierImmutable, LockedReason: editReasonImmutable},
	{Key: "config.config_dir", Tier: EditTierImmutable, LockedReason: editReasonInternal},
	{Key: "config.detached", Tier: EditTierImmutable, LockedReason: editReasonInternal},
	{Key: "config.hub", Tier: EditTierImmutable, LockedReason: editReasonInternal},
}

// agentEditFieldByKey indexes agentEditFields by wire key.
var agentEditFieldByKey = func() map[string]agentEditField {
	m := make(map[string]agentEditField, len(agentEditFields))
	for _, f := range agentEditFields {
		m[f.Key] = f
	}
	return m
}()

// agentConfigEditFieldByLowerKey indexes the config rows by their json key,
// lower-cased: the agent PATCH lower-cases the keys of the request's config
// object (encoding/json matches field names case-insensitively).
var agentConfigEditFieldByLowerKey = func() map[string]agentEditField {
	m := make(map[string]agentEditField)
	for _, f := range agentEditFields {
		if k, ok := strings.CutPrefix(f.Key, agentConfigKeyPrefix); ok {
			m[strings.ToLower(k)] = f
		}
	}
	return m
}()

// editDisposition is the phase rule: the disposition of an edit to f for an
// agent in phase. deleted is true for a soft-deleted agent.
func editDisposition(f agentEditField, phase string, deleted bool) (EditDisposition, string) {
	if deleted {
		return EditLocked, editReasonDeleted
	}
	p := state.Phase(phase)
	if p == state.PhaseCreated && f.CreatedDisposition != "" {
		if f.CreatedDisposition == EditLocked {
			return EditLocked, f.LockedReason
		}
		return f.CreatedDisposition, ""
	}
	var noContainer, liveOrTransitional bool
	switch p {
	case state.PhaseCreated, state.PhaseStopped, state.PhaseError, state.PhaseSuspended:
		noContainer = true
	case state.PhaseProvisioning, state.PhaseCloning, state.PhaseStarting, state.PhaseRunning, state.PhaseStopping:
		liveOrTransitional = true
	default:
		return EditLocked, editReasonUnknownPhase
	}
	switch f.Tier {
	case EditTierMetadata:
		return EditImmediate, ""
	case EditTierContainer:
		if f.AnyPhase {
			return EditNow, ""
		}
		if p == state.PhaseSuspended && f.NotOnResume {
			return EditLocked, editReasonNotOnResume
		}
		if noContainer {
			return EditNow, ""
		}
		if liveOrTransitional {
			return EditHeld, ""
		}
	case EditTierProvision, EditTierPrincipal:
		return EditReincarnate, ""
	case EditTierImmutable:
		return EditLocked, f.LockedReason
	}
	return EditLocked, editReasonUnknownPhase
}

// phaseHasLiveOrTransitionalContainer reports whether an agent in phase has
// a live container or one being created or removed: the phases whose config
// edits would be held rather than written.
func phaseHasLiveOrTransitionalContainer(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseProvisioning, state.PhaseCloning, state.PhaseStarting, state.PhaseRunning, state.PhaseStopping:
		return true
	}
	return false
}

// AgentEditability is the per-agent, per-caller result of the mutability
// table, returned as "editability" on GET /api/v1/agents/{id}.
type AgentEditability struct {
	Phase  string                    `json:"phase"`
	Fields map[string]FieldEditState `json:"fields"`
}

// FieldEditState is one field's entry in AgentEditability.
type FieldEditState struct {
	Tier        EditTier        `json:"tier"`
	Disposition EditDisposition `json:"disposition"`
	// SessionSensitive is set on a field a resume applies but whose effect
	// on the continued conversation depends on the harness.
	SessionSensitive bool `json:"sessionSensitive,omitempty"`
	// Note is "session" for a session-sensitive field of a suspended agent:
	// the edit applies at resume and the conversation continues.
	Note string `json:"note,omitempty"`
	// ClearNeedsReincarnate is set on a T1 field: clearing or zeroing it
	// takes effect only at the next reincarnation.
	ClearNeedsReincarnate bool `json:"clearNeedsReincarnate,omitempty"`
	// Reason explains a locked field.
	Reason string `json:"reason,omitempty"`
}

// agentEditAccess is what the caller may do to the agent, for
// buildAgentEditability.
type agentEditAccess struct {
	// CanUpdate is agent.update on the agent; without it nothing is
	// editable.
	CanUpdate bool
	// CanChangeRole is whether the caller could change the agent's role by
	// reincarnating it: agent.lifecycle on the agent, and authority to
	// delegate the agent's role.
	CanChangeRole bool
}

// buildAgentEditability applies the table to agent for a caller with
// access. Held edits of a running agent are not available yet, so a held
// disposition is reported locked.
func buildAgentEditability(agent *store.Agent, access agentEditAccess) *AgentEditability {
	if agent == nil {
		return nil
	}
	deleted := !agent.DeletedAt.IsZero()
	out := &AgentEditability{
		Phase:  agent.Phase,
		Fields: make(map[string]FieldEditState, len(agentEditFields)),
	}
	live := phaseHasLiveOrTransitionalContainer(agent.Phase)
	for _, f := range agentEditFields {
		d, reason := editDisposition(f, agent.Phase, deleted)
		// Held edits do not exist yet, and a config PATCH of an agent
		// with a live or transitional container is refused, so every
		// config key of such an agent is locked, provision-rendered keys
		// included: the reincarnate endpoint takes no config patch for
		// them either. The role and GCP identity keep "reincarnate"; they
		// change through the reincarnate endpoint.
		if d == EditHeld || (live && d != EditLocked && strings.HasPrefix(f.Key, agentConfigKeyPrefix)) {
			d, reason = EditLocked, editReasonRunning
		}
		if d != EditLocked {
			switch {
			case !access.CanUpdate:
				d, reason = EditLocked, editReasonNoUpdate
			case f.Key == "agentRole" && !access.CanChangeRole:
				d, reason = EditLocked, editReasonCannotDelegate
			}
		}
		st := FieldEditState{
			Tier:                  f.Tier,
			Disposition:           d,
			SessionSensitive:      f.SessionSensitive,
			ClearNeedsReincarnate: f.Tier == EditTierContainer && strings.HasPrefix(f.Key, agentConfigKeyPrefix),
		}
		if d == EditLocked {
			st.Reason = reason
		}
		if f.SessionSensitive && d == EditNow && state.Phase(agent.Phase) == state.PhaseSuspended {
			st.Note = "session"
		}
		out.Fields[f.Key] = st
	}
	return out
}

// patchRefusal is the answer to an agent PATCH that names keys the table
// locks for the agent: the request is refused whole and nothing is written.
type patchRefusal struct {
	// Conflict is true when a lock comes from the agent's phase or its
	// deletion (409); false when every refused key is fixed (400).
	Conflict bool
	// Fields maps each refused wire key to the reason.
	Fields map[string]string
}

// lockedPatchKeys checks the keys an agent PATCH names against the
// mutability table, with the same rule GET editability reports: top are the
// present top-level wire keys, rawConfig the request's raw config object and
// cfg its decoded form. applied carries the agent's applied values of the
// fixed keys that have one (branch, harness_config, and harness as the hub
// resolves it). It returns nil when nothing is locked.
//
// Precedence, highest first (writePatchRefusal answers by it):
//  1. A deleted agent: every present key is locked (409).
//  2. A phase that takes no config (running, transitional): every present
//     config key is locked, a fixed key's echo included (409).
//  3. A fixed (TX) config key that would change, in a phase that takes
//     config (400). Unchanged means equal to the agent's stored inline value
//     or, for a non-empty request value, its applied value; an empty value
//     where no inline value is stored is ignored.
//
// Config keys the table does not know are ignored, as encoding/json ignores
// them.
func lockedPatchKeys(agent *store.Agent, top []string, rawConfig map[string]json.RawMessage, cfg, applied *api.ScionConfig) *patchRefusal {
	ed := buildAgentEditability(agent, agentEditAccess{CanUpdate: true, CanChangeRole: true})
	var stored *api.ScionConfig
	if agent.AppliedConfig != nil {
		stored = agent.AppliedConfig.InlineConfig
	}
	deleted := !agent.DeletedAt.IsZero()
	configPhase := configPatchPhase(agent.Phase)
	ref := &patchRefusal{Fields: map[string]string{}}
	check := func(f agentEditField) {
		k, isConfig := strings.CutPrefix(f.Key, agentConfigKeyPrefix)
		switch {
		case deleted:
			ref.Conflict = true
			ref.Fields[f.Key] = editReasonDeleted
			return
		case isConfig && !configPhase:
			ref.Conflict = true
			ref.Fields[f.Key] = editReasonNoConfigPhase(agent.Phase)
			return
		}
		st, ok := ed.Fields[f.Key]
		if !ok || st.Disposition != EditLocked {
			return
		}
		if f.Tier == EditTierImmutable {
			// A fixed top-level key (template, profile, ...) has no PATCH
			// surface, so nothing decodes or writes it.
			if !isConfig || configFieldUnchanged(k, cfg, stored) ||
				(!configFieldZero(k, cfg) && configFieldUnchanged(k, cfg, applied)) {
				return
			}
			ref.Fields[f.Key] = editReasonFixedPatch
			return
		}
		ref.Conflict = true
		ref.Fields[f.Key] = st.Reason
	}
	for _, k := range top {
		if f, ok := agentEditFieldByKey[k]; ok {
			check(f)
		}
	}
	for _, f := range configPatchKeys(rawConfig) {
		check(f)
	}
	if len(ref.Fields) == 0 {
		return nil
	}
	return ref
}

// editReasonNoConfigPhase is the lock reason of a config key in a phase that
// takes no config.
func editReasonNoConfigPhase(phase string) string {
	if phaseHasLiveOrTransitionalContainer(phase) {
		return editReasonRunning
	}
	return editReasonUnknownPhase
}

// appliedFixedValues returns, as a config, the agent's applied values of the
// fixed config keys that have one: branch, harness_config, and harness, the
// harness the hub resolves for the agent. lockedPatchKeys treats a request
// value equal to one of them as unchanged.
func appliedFixedValues(agent *store.Agent, resolvedHarness string) *api.ScionConfig {
	out := &api.ScionConfig{Harness: resolvedHarness}
	if agent != nil && agent.AppliedConfig != nil {
		out.Branch = agent.AppliedConfig.Branch
		out.HarnessConfig = agent.AppliedConfig.HarnessConfig
	}
	return out
}

// scionConfigFieldByJSONKey indexes api.ScionConfig's fields by json key.
var scionConfigFieldByJSONKey = func() map[string]int {
	m := map[string]int{}
	typ := reflect.TypeOf(api.ScionConfig{})
	for i := 0; i < typ.NumField(); i++ {
		if k := scionConfigJSONKey(typ.Field(i)); k != "" {
			m[k] = i
		}
	}
	return m
}()

// scionConfigJSONKey is the json key encoding/json uses for an
// api.ScionConfig field, or "" for a field it does not encode.
func scionConfigJSONKey(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	tag := strings.Split(f.Tag.Get("json"), ",")[0]
	if tag == "-" {
		return ""
	}
	if tag == "" {
		return f.Name
	}
	return tag
}

// configFieldZero reports whether the config key key is empty or unset in
// cfg.
func configFieldZero(key string, cfg *api.ScionConfig) bool {
	return configFieldUnchanged(key, cfg, nil)
}

// configFieldUnchanged reports whether the config key key has the same
// value in req as in stored, counting an empty value and an unset one as
// the same.
func configFieldUnchanged(key string, req, stored *api.ScionConfig) bool {
	i, ok := scionConfigFieldByJSONKey[key]
	if !ok {
		return true
	}
	var a, b reflect.Value
	if req != nil {
		a = reflect.ValueOf(*req).Field(i)
	}
	if stored != nil {
		b = reflect.ValueOf(*stored).Field(i)
	}
	aZero := !a.IsValid() || a.IsZero() || (isNilable(a) && a.Len() == 0)
	bZero := !b.IsValid() || b.IsZero() || (isNilable(b) && b.Len() == 0)
	if aZero || bZero {
		return aZero == bZero
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

func isNilable(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Map, reflect.Slice:
		return true
	}
	return false
}

// AgentUpdateDisposition reports, in the agent PATCH response, what the hub
// did with each key the request named.
type AgentUpdateDisposition struct {
	// Applied lists the wire keys written to the agent, sorted.
	Applied []string `json:"applied"`
}

// configPatchKeys returns the table rows of the request's config keys, in
// wire-key order, given its raw config object. Keys that are not
// api.ScionConfig keys (and so were not decoded) are skipped.
func configPatchKeys(rawConfig map[string]json.RawMessage) []agentEditField {
	var out []agentEditField
	seen := make(map[string]bool, len(rawConfig))
	for k := range rawConfig {
		if f, ok := agentConfigEditFieldByLowerKey[strings.ToLower(k)]; ok && !seen[f.Key] {
			seen[f.Key] = true
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// reincarnateOnlyConfigEdits splits the request's config keys whose edit
// takes effect only at the next reincarnation into provision, the T2 keys
// (rendered into files at provision; a plain start does not re-render
// them), and cleared, the T1 keys the request clears or zeroes (the
// broker's start-time merge keeps the previous non-empty value). A key
// whose value does not change from stored (req is the decoded request) is
// not counted. Both are sorted wire keys.
func reincarnateOnlyConfigEdits(rawConfig map[string]json.RawMessage, req, stored *api.ScionConfig) (provision, cleared []string) {
	for _, f := range configPatchKeys(rawConfig) {
		k := strings.TrimPrefix(f.Key, agentConfigKeyPrefix)
		if configFieldUnchanged(k, req, stored) {
			continue
		}
		switch f.Tier {
		case EditTierProvision:
			provision = append(provision, f.Key)
		case EditTierContainer:
			if rawJSONIsEmpty(rawValueCaseInsensitive(rawConfig, k)) {
				cleared = append(cleared, f.Key)
			}
		}
	}
	return provision, cleared
}

// reincarnateOnlyEditWarnings returns the PATCH warnings for the config
// keys reincarnateOnlyConfigEdits reports.
func reincarnateOnlyEditWarnings(rawConfig map[string]json.RawMessage, req, stored *api.ScionConfig) []string {
	provision, cleared := reincarnateOnlyConfigEdits(rawConfig, req, stored)
	var out []string
	if len(provision) > 0 {
		out = append(out, strings.Join(provision, ", ")+": stored now; rendered at the next reincarnation (a plain start does not re-render prompts, skills or services)")
	}
	if len(cleared) > 0 {
		out = append(out, strings.Join(cleared, ", ")+": cleared now; the agent keeps its previous value until the next reincarnation (a plain start does not apply a cleared or zero value)")
	}
	return out
}

// rawValueCaseInsensitive returns the value of key in raw, matching the key
// case-insensitively as encoding/json does.
func rawValueCaseInsensitive(raw map[string]json.RawMessage, key string) json.RawMessage {
	if v, ok := raw[key]; ok {
		return v
	}
	for k, v := range raw {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return nil
}

// rawJSONIsEmpty reports whether v is null, "", 0, false, [] or {}.
func rawJSONIsEmpty(v json.RawMessage) bool {
	t := bytes.TrimSpace(v)
	switch string(t) {
	case "", "null", `""`, "false", "[]", "{}":
		return true
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(t))
	dec.UseNumber()
	if err := dec.Decode(&n); err == nil {
		f, err := n.Float64()
		return err == nil && f == 0
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(t, &m); err == nil {
		return len(m) == 0
	}
	var a []json.RawMessage
	if err := json.Unmarshal(t, &a); err == nil {
		return len(a) == 0
	}
	return false
}
