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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GET /api/v1/projects/{projectId}/settings/resolved
//
// This endpoint reports per-setting information about project annotations and
// hub-level defaults. It does NOT resolve precedence — it does not report which
// value wins, and a consumer that needs the effective value must resolve it
// itself against whatever precedence ladder exists at the time it asks.
//
// The response carries:
//   - projectSet / projectValue: what the project annotation says.
//   - hubDefault: whether a hub-level default exists (tri-state).
//   - hubValue: the raw hub-configured value when hubDefault is "present",
//     null otherwise. This is the value the hub operator configured, exposed
//     so the settings UI can show it as a placeholder hint (e.g. "Hub default:
//     200"). It is NOT an effective value: template and harness-config layers
//     may override it before an agent receives it.
//
// The response deliberately does NOT carry:
//   - An "effective" or "resolved" value that claims to know which layer wins.
//   - A "source" field that names the winning layer.
//
// These are omitted because computing an effective value would be a second
// implementation of the precedence order. Two implementations of one ordering
// do not stay equal, and the copy fails silently because a stale answer is
// still a well-formed answer.
//
// TestResolvedSettingsResponseShape_NoEffectiveValue enforces the field set as
// an exact-set assertion, so any new field fails the build rather than review.

// ResolvedHubDefault reports whether a hub-level default exists for a project
// setting. It is deliberately TRI-STATE rather than a bool.
//
// A bool has no room for "I did not look here", which forces "not evaluated" to
// be reported as "no" — and a consumer cannot tell the difference between "the
// hub has no default for this" and "this build could not determine it". The
// distinction is real in this codebase and is not hypothetical:
//
//   - In file/SQLite mode there is no OperationalSettings at all, so no
//     agent_defaults document can be read. Every key backed by that section is
//     genuinely UNKNOWN, not absent.
//   - Six of the eight opsettings.AgentDefaultsSettings fields are non-pointer
//     scalars marshalled with `omitempty`, so a value explicitly configured to
//     the zero value is dropped at write time and is indistinguishable from
//     never-configured. Where that ambiguity is reachable, absence is UNKNOWN.
//
// See resolvedSettingDescriptor.absentWhenMissing for exactly which keys can
// honestly report ABSENT and why.
type ResolvedHubDefault string

const (
	// ResolvedHubDefaultPresent means a hub-level default for this key was
	// observed. This is always a measurement, never an inference.
	ResolvedHubDefaultPresent ResolvedHubDefault = "present"

	// ResolvedHubDefaultAbsent means the hub source for this key was consulted
	// and positively does not carry a value. Only used where absence is
	// faithfully representable — see absentWhenMissing.
	ResolvedHubDefaultAbsent ResolvedHubDefault = "absent"

	// ResolvedHubDefaultUnknown means presence could not be honestly
	// determined: either the source was unreachable, or the source cannot
	// distinguish an explicitly-zero value from an unset one. It does NOT mean
	// "no".
	ResolvedHubDefaultUnknown ResolvedHubDefault = "unknown"
)

// ResolvedProjectSetting is the per-key entry of the resolved-settings
// response.
//
// There is deliberately no `value`, no `effective` and no `source` field here.
// This endpoint does not resolve precedence — resolution is the resolver's job.
//
// HubValue is the raw hub-configured value for display purposes only. It does
// NOT represent the effective value: template and harness-config layers may sit
// between the hub default and what an agent actually receives. Clients must
// treat it as an informational hint ("the hub has this configured"), never as a
// precedence statement.
//
// On the absent `source` field specifically: its absence is deliberate, not an
// omission. Reporting a winning source is the same precedence claim as
// reporting a winning value. (Had one been needed, it would also have had to
// avoid the "harness"/"template"/"settings" vocabulary of api.SecretKeyInfo,
// which is secret provenance — a different domain that happens to reuse these
// words with different meanings.)
type ResolvedProjectSetting struct {
	// ProjectSet reports whether the project annotation is present, regardless
	// of its content. An annotation explicitly set to the empty string is set.
	ProjectSet bool `json:"projectSet"`

	// ProjectValue is the raw annotation string, exactly as stored, or null
	// when the annotation is absent. It is intentionally not typed per key: the
	// annotation store holds strings, and parsing here would mean re-stating
	// per-key type knowledge that projectSettingsFromAnnotations already owns.
	ProjectValue *string `json:"projectValue"`

	// HubDefault reports the EXISTENCE of a hub-level default. Not its rank.
	HubDefault ResolvedHubDefault `json:"hubDefault"`

	// HubValue is the raw hub-configured value when HubDefault is "present",
	// null otherwise. This is the value the hub operator configured, exposed so
	// the settings UI can display it as a placeholder hint. It is NOT an
	// effective value and does not account for template or harness-config layers.
	HubValue any `json:"hubValue"`
}

// ResolvedProjectSettings is the GET .../settings/resolved response body.
//
// Settings is keyed by raw annotation key ("scion.io/default-model"), not by
// camelCase field name. Two reasons: it makes the correspondence with
// projectSettingKeys exact and eyeball-checkable, and the registry's five flat
// resource keys have no 1:1 camelCase counterpart — hubclient.ProjectSettings
// nests them under a single DefaultResources object, so a camelCase keying
// would have required inventing a mapping that does not exist in the data.
type ResolvedProjectSettings struct {
	// Project is the existing GET /settings payload, unchanged. It carries no
	// precedence information and exists so a client does not need two round
	// trips to render a settings form.
	Project *hubclient.ProjectSettings `json:"project"`

	// Settings is keyed by annotation key. Its key set is exactly
	// projectSettingKeys — enforced by TestResolvedSettings_RegistryNoDrift.
	Settings map[string]ResolvedProjectSetting `json:"settings"`
}

// hubDefaultSource identifies which hub-side configuration source is consulted
// to answer "does a hub default exist" for a given project setting.
//
// These identifiers are local to the resolved-settings domain. The collision
// with api.SecretKeyInfo.Source ("harness" | "template" | "settings") is
// deliberate and unrelated: that vocabulary describes where a *secret* came
// from and implies no correspondence with these values.
type hubDefaultSource int

const (
	// hubSourceNone: no hub-level counterpart exists for this setting.
	// opsettings.AgentDefaultsSettings has exactly eight fields, enumerated in
	// full, so this is a measured structural fact rather than an unsearched
	// one — which is why these keys may report ABSENT rather than UNKNOWN.
	hubSourceNone hubDefaultSource = iota

	// hubSourceAgentDefaults: the opsettings "agent_defaults" section document.
	hubSourceAgentDefaults

	// hubSourceTelemetryDefault: Server.config.TelemetryDefault, which is a
	// *bool and therefore presence-faithful. The hub telemetry default lives
	// here and NOT in agent_defaults; reporting "absent" for it merely because
	// agent_defaults has no telemetry field would be a false statement about a
	// question asked of the wrong source.
	hubSourceTelemetryDefault

	// hubSourceAutoExposePortsDefault: Server.config.AutoExposePortsDefault,
	// which is a *bool and therefore presence-faithful.
	hubSourceAutoExposePortsDefault
)

// resolvedSettingDescriptor carries the per-key knowledge that the annotation
// key string cannot supply by itself: which hub source answers for this key,
// where to look inside it, and whether a missing entry can honestly be reported
// as absent.
//
// WHY THIS TABLE EXISTS AS A SECOND STRUCTURE, given that projectSettingKeys is
// meant to be the single source of truth: if the response were built by ranging
// over projectSettingKeys alone, coverage of the registry would be true by
// construction and the drift guard could never fail. A check that cannot fail
// is worse than no check, because it looks like protection. The registry
// remains the authoritative key list; this table adds per-key wiring, and
// TestResolvedSettings_RegistryNoDrift asserts the two cover each other exactly
// in BOTH directions. The drift that matters is unenforced duplication — this
// duplication is enforced.
type resolvedSettingDescriptor struct {
	source hubDefaultSource

	// path is the JSON path within the source document, e.g.
	// {"default_resources", "requests", "cpu"}. Empty for hubSourceNone and
	// hubSourceTelemetryDefault.
	path []string

	// absentWhenMissing records whether a missing leaf can be reported as
	// ABSENT (true) or must be reported as UNKNOWN (false).
	//
	// This is per-field and it is NOT guessable from the Go types; it falls out
	// of the JSON schema. The write path builds AgentDefaultsSettings
	// field-by-field and marshals it with `omitempty`, so any explicitly-zero
	// value is discarded before it is persisted. Whether that matters depends
	// on whether the zero value can be persisted at all:
	//
	//   - default_max_turns, default_max_model_calls carry "minimum": 1 in
	//     settings-v1.schema.json, so 0 is rejected by validation and can never
	//     reach the document. Absence is unambiguous. -> true
	//   - default_template, default_harness_config, default_max_duration and
	//     the nested resource quantities are strings with no minLength, so ""
	//     validates and is then silently dropped by omitempty. Absence cannot
	//     be distinguished from an explicit empty value. -> false
	//
	// Treating "" as equivalent to absent would tidy this up, but that is a
	// claim about how the ladder consumes an empty hub value, and this package
	// does not own the ladder.
	absentWhenMissing bool
}

// resolvedSettingDescriptors maps every registered project setting to its hub
// source. Its key set must equal projectSettingKeys exactly; both directions
// are enforced by TestResolvedSettings_RegistryNoDrift.
var resolvedSettingDescriptors = map[string]resolvedSettingDescriptor{
	projectSettingDefaultTemplate: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_template"},
		absentWhenMissing: false, // string, "" dropped by omitempty
	},
	projectSettingDefaultHarnessConfig: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_harness_config"},
		absentWhenMissing: false, // string, "" dropped by omitempty
	},
	projectSettingDefaultModel: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_model"},
		absentWhenMissing: true, // schema has minLength:1, so "" is excluded
	},
	projectSettingDefaultThinkingLevel: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_thinking_level"},
		absentWhenMissing: true, // *int with omitempty; 0 is the clear sentinel and deletes the key, so absence is unambiguous
	},
	projectSettingTelemetryEnabled: {
		source: hubSourceTelemetryDefault,
	},
	projectSettingAutoExposePortsEnabled: {
		source: hubSourceAutoExposePortsDefault,
	},
	projectSettingActiveProfile: {
		source: hubSourceNone,
	},

	// Default agent limits
	projectSettingDefaultMaxTurns: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_max_turns"},
		absentWhenMissing: true, // int, schema "minimum": 1 makes 0 unpersistable
	},
	projectSettingDefaultMaxModelCalls: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_max_model_calls"},
		absentWhenMissing: true, // int, schema "minimum": 1 makes 0 unpersistable
	},
	projectSettingDefaultMaxDuration: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_max_duration"},
		absentWhenMissing: false, // string, "" dropped by omitempty
	},

	// Default GCP identity
	projectSettingDefaultGCPIdentityMode: {
		source: hubSourceNone,
	},
	projectSettingDefaultGCPIdentitySAID: {
		source: hubSourceNone,
	},

	// Default resource spec. The registry's five flat annotation keys face a
	// single agent_defaults "default_resources" object, so the mapping is 5:1
	// and each key resolves to a distinct nested path.
	projectSettingDefaultResourcesCPUReq: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_resources", "requests", "cpu"},
		absentWhenMissing: false,
	},
	projectSettingDefaultResourcesMemReq: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_resources", "requests", "memory"},
		absentWhenMissing: false,
	},
	projectSettingDefaultResourcesCPULim: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_resources", "limits", "cpu"},
		absentWhenMissing: false,
	},
	projectSettingDefaultResourcesMemLim: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_resources", "limits", "memory"},
		absentWhenMissing: false,
	},
	projectSettingDefaultResourcesDisk: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_resources", "disk"},
		absentWhenMissing: false,
	},

	// Agent authorization
	projectSettingMaxAgentRole: {
		source:            hubSourceAgentDefaults,
		path:              []string{"default_max_agent_role"},
		absentWhenMissing: false, // string, "" dropped by omitempty
	},
}

// handleProjectSettingsResolved serves GET
// /api/v1/projects/{projectId}/settings/resolved.
//
// Read-only by construction: there is no PUT. The response is a separate
// sub-resource rather than an extension of GET /settings precisely because
// hubclient.ProjectSettings is used as both the GET response and the PUT body —
// adding hub-derived fields there would make a naive GET-modify-PUT round trip
// promote every hub default into an explicit project annotation.
//
// See the file header for why this endpoint does not return an effective value.
func (s *Server) handleProjectSettingsResolved(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	// Same authorization as GET /settings: ActionRead on the project.
	// Deliberately not admin-gated — the point is to let a non-admin project
	// owner see that hub defaults exist. Only the existence of an
	// agent_defaults entry is exposed, never a hub value and never any other
	// part of the server configuration.
	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	}

	writeJSON(w, http.StatusOK, s.resolvedProjectSettings(project))
}

// resolvedProjectSettings builds the response body.
//
// It iterates projectSettingKeys — the authoritative registry — and looks each
// key up in resolvedSettingDescriptors. A registry key with no descriptor is a
// wiring error; it reports UNKNOWN here rather than silently vanishing from the
// response, and TestResolvedSettings_RegistryNoDrift fails the build for it.
// Reporting rather than omitting matters: an omitted key would make the
// response quietly incomplete, which is the failure the registry exists to
// prevent.
func (s *Server) resolvedProjectSettings(project *store.Project) *ResolvedProjectSettings {
	agentDefaults, agentDefaultsReadable := s.hubAgentDefaultsDoc()

	settings := make(map[string]ResolvedProjectSetting, len(projectSettingKeys))
	for _, key := range projectSettingKeys {
		entry := ResolvedProjectSetting{
			HubDefault: ResolvedHubDefaultUnknown,
		}

		if project.Annotations != nil {
			if val, ok := project.Annotations[key]; ok {
				v := val
				entry.ProjectSet = true
				entry.ProjectValue = &v
			}
		}

		desc, ok := resolvedSettingDescriptors[key]
		if !ok {
			// Unwired registry key. UNKNOWN is the honest report: we did not
			// look anywhere for it.
			settings[key] = entry
			continue
		}

		entry.HubDefault, entry.HubValue = s.hubDefaultFor(desc, agentDefaults, agentDefaultsReadable)
		settings[key] = entry
	}

	return &ResolvedProjectSettings{
		Project:  projectSettingsFromAnnotations(project),
		Settings: settings,
	}
}

// hubDefaultFor answers the existence question for one descriptor and, when
// a hub default is present, also extracts its raw value for display.
func (s *Server) hubDefaultFor(
	desc resolvedSettingDescriptor,
	agentDefaults map[string]json.RawMessage,
	agentDefaultsReadable bool,
) (ResolvedHubDefault, any) {
	switch desc.source {
	case hubSourceNone:
		// Measured structural absence: AgentDefaultsSettings has eight fields and
		// none of them corresponds to this setting.
		return ResolvedHubDefaultAbsent, nil

	case hubSourceTelemetryDefault:
		// *bool — presence-faithful, so this is a real answer rather than a
		// zero-value guess. s.config is a value, not a pointer, so there is no
		// "config absent" case to distinguish here: an unset TelemetryDefault
		// is a nil pointer either way.
		if s.config.TelemetryDefault != nil {
			return ResolvedHubDefaultPresent, *s.config.TelemetryDefault
		}
		return ResolvedHubDefaultAbsent, nil

	case hubSourceAutoExposePortsDefault:
		// *bool — same presence-faithful pattern as TelemetryDefault.
		if s.config.AutoExposePortsDefault != nil {
			return ResolvedHubDefaultPresent, *s.config.AutoExposePortsDefault
		}
		return ResolvedHubDefaultAbsent, nil

	case hubSourceAgentDefaults:
		if !agentDefaultsReadable {
			// No OperationalSettings (file mode) or the section could not be
			// read. Nowhere searched — not "no".
			//
			// This is the case where reporting ABSENT would be a measurable
			// lie, and the obvious implementation tells it. In file mode the
			// operator's config file may well carry default_template et al —
			// extractAgentDefaults reads exactly those koanf paths — but
			// BuildLayer1SnapshotFromFile never populates AgentDefaults, and
			// SetOperationalSettings is called from the DB path only. So the
			// hub genuinely did not look, and what it holds is a zero value
			// that means nothing about operator intent.
			//
			// Upstream says so itself, twice: hubAgentDefaults() is documented
			// "In file mode this always returns the zero value ... so callers
			// that gate on non-empty never fire in file mode", and
			// handlers_agents_core.go repeats it at the template rung.
			//
			// WHY NOT ABSENT, given that in file mode no hub default will in
			// fact fire? Two reasons, and the second is the load-bearing one.
			//
			// First, "no hub default will fire" is a claim about what the
			// ladder DOES, and predicting ladder behaviour is the precise act
			// this endpoint refuses to perform. Resolution would come back in
			// through the one field small enough to look harmless.
			//
			// Second, and load-bearing: ABSENT would be a claim about WHY the
			// data is missing, and this endpoint is not entitled to that claim
			// either way it turns out.
			//
			// extractAgentDefaults proves the file format can already express
			// these keys; BuildLayer1SnapshotFromFile does not read them. Two
			// readings of that were live, and UNKNOWN is the correct answer
			// under BOTH — which is the entire reason to prefer it:
			//
			//   - If it is a gap someone later closes, file mode begins holding
			//     real agent-defaults data, and every ABSENT previously emitted
			//     becomes silently wrong: no test fails, no error is raised, the
			//     field simply starts lying. UNKNOWN survives untouched and
			//     becomes answerable when the plumbing lands.
			//   - If the emptiness is permanent, ABSENT is not merely
			//     wrong-until-fixed but permanently wrong, because it would
			//     report "the operator configured nothing" about data the hub is
			//     structurally incapable of reading.
			//
			// Upstream has since settled which reading applies, and it is the
			// second: hub_agent_defaults.go records that
			// BuildLayer1SnapshotFromFile "deliberately leaves the agent-defaults
			// fields empty (design §3.2.4)" in order to keep file-mode dispatch
			// byte-identical. So do not expect this to start returning
			// present/absent in file mode — UNKNOWN is the permanent, honest
			// answer there, not a placeholder awaiting better plumbing.
			//
			// Note that the argument above did not depend on knowing which
			// reading was true. That is the property worth preserving if this
			// comment is ever revised.
			//
			// WHY THE RAW SECTION DOCUMENT, when a typed accessor is sitting
			// right there: s.hubAgentDefaults() returns AgentDefaultsSettings,
			// whose fields are non-pointer scalars. Its zero value cannot
			// distinguish "unset" from "configured to zero", so it cannot
			// express UNKNOWN at all — an implementation built on it would
			// report ABSENT for all seven agent_defaults-backed keys in file
			// mode, confidently, on the strength of a zero value upstream
			// documents as meaningless. Do not simplify this back to the typed
			// accessor; that is the bug, not the cleanup.
			//
			// Honest history, so the next reader knows which parts are
			// load-bearing: these two reasons were independent. The raw-section
			// read was adopted to avoid a name collision with the upstream
			// hubAgentDefaults() accessor, not for correctness. The correctness
			// property fell out of it. We got here partly by luck.
			return ResolvedHubDefaultUnknown, nil
		}
		return hubDefaultFromDoc(agentDefaults, desc)

	default:
		return ResolvedHubDefaultUnknown, nil
	}
}

// hubDefaultFromDoc walks desc.path through a raw section document.
//
// The walk is done over raw JSON rather than an unmarshalled struct on purpose:
// unmarshalling into AgentDefaultsSettings is exactly where presence
// information is destroyed, because its fields are non-pointer scalars whose
// zero values are indistinguishable from unset.
//
// When a hub default is present, the raw value is also returned (unmarshalled
// to a native Go type via json.Unmarshal into any) for display by the settings
// UI. The value is informational only and does not represent effective
// resolution.
func hubDefaultFromDoc(doc map[string]json.RawMessage, desc resolvedSettingDescriptor) (ResolvedHubDefault, any) {
	if len(desc.path) == 0 {
		return ResolvedHubDefaultUnknown, nil
	}

	root, ok := doc[desc.path[0]]
	if !ok {
		// For a nested path the root is "default_resources", a pointer field
		// (*api.ResourceSpec). A pointer CAN represent unset, so its absence is
		// faithful and reports as absent regardless of the leaf's own rule.
		if len(desc.path) > 1 {
			return ResolvedHubDefaultAbsent, nil
		}
		return missingLeafState(desc), nil
	}

	if len(desc.path) == 1 {
		if isJSONNull(root) {
			return missingLeafState(desc), nil
		}
		return ResolvedHubDefaultPresent, unmarshalRawValue(root)
	}

	current := root
	for _, segment := range desc.path[1:] {
		var next map[string]json.RawMessage
		if err := json.Unmarshal(current, &next); err != nil {
			// Shape is not what the schema says it should be. We cannot answer.
			return ResolvedHubDefaultUnknown, nil
		}
		child, ok := next[segment]
		if !ok {
			return missingLeafState(desc), nil
		}
		current = child
	}

	if isJSONNull(current) {
		return missingLeafState(desc), nil
	}
	return ResolvedHubDefaultPresent, unmarshalRawValue(current)
}

// unmarshalRawValue converts a json.RawMessage to a native Go value (string,
// float64, bool, etc.) for inclusion in the response. Returns nil on error.
func unmarshalRawValue(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// missingLeafState maps a missing entry to absent or unknown per the
// descriptor's measured ambiguity.
func missingLeafState(desc resolvedSettingDescriptor) ResolvedHubDefault {
	if desc.absentWhenMissing {
		return ResolvedHubDefaultAbsent
	}
	return ResolvedHubDefaultUnknown
}

func isJSONNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}

// hubAgentDefaultsDoc returns the raw agent_defaults section document, and
// whether it could be read at all.
//
// The second return value is load-bearing: false means "could not look", which
// must surface as UNKNOWN rather than as an empty document (which would read as
// "looked, found nothing" and report absent).
func (s *Server) hubAgentDefaultsDoc() (map[string]json.RawMessage, bool) {
	ops := s.GetOperationalSettings()
	if ops == nil {
		// File/SQLite mode: the agent_defaults section does not exist as a
		// document here. A missing hint must never fail the request.
		return nil, false
	}

	raw, ok := ops.rawSection("agent_defaults")
	if !ok {
		return nil, false
	}

	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false
	}
	return doc, true
}

// rawSection returns a section's raw persisted JSON document, pre-unmarshal.
//
// This exists because presence cannot be recovered after unmarshalling into the
// section struct: opsettings.AgentDefaultsSettings stores six of its eight
// fields as non-pointer scalars, so "configured to zero" and "never configured"
// become the same value. The raw document still distinguishes them, to the
// extent the write path preserved the distinction at all.
//
// Precedence mirrors Snapshot(): a DB row fully owns its section, and only a
// section absent from the DB falls back to the bootstrap merge.
func (o *OperationalSettings) rawSection(name string) (json.RawMessage, bool) {
	o.mu.RLock()
	state, inDB := o.cache[name]
	o.mu.RUnlock()

	if inDB {
		// Copy. The caller would otherwise receive bytes the cache still owns:
		// the lock is released above, but state.Value shares its backing array
		// with the live cache entry, so a caller writing through the returned
		// slice corrupts the process-wide settings cache for every reader —
		// Snapshot() included, not just this endpoint. Downstream the effect is
		// quiet rather than loud: hubAgentDefaultsDoc's unmarshal starts
		// failing, which flips every agent_defaults-backed key to "unknown"
		// hub-wide.
		//
		// Today's only consumer passes these bytes to json.Unmarshal, which
		// does not write to its input. That is a property of today's consumer,
		// not of this function's contract, and this accessor is the first thing
		// in the package to hand raw cache bytes across a file boundary.
		out := make(json.RawMessage, len(state.Value))
		copy(out, state.Value)
		return out, true
	}

	sec := opsettings.SectionByName(name)
	if sec == nil || len(sec.KoanfPaths) == 0 || o.bootstrapKoanf == nil {
		return nil, false
	}

	// ExtractSectionFromKoanf is presence-faithful for agent_defaults: its
	// extractor adds a key only when koanf reports it Exists.
	doc, err := opsettings.ExtractSectionFromKoanf(o.bootstrapKoanf, name)
	if err != nil {
		return nil, false
	}
	return doc, true
}
