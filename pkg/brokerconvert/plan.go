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

// Package brokerconvert builds the read-only plan for moving a legacy,
// profile-resolving Runtime Broker host to flat Runtime Broker instances.
// It wraps config.ConvertProfilesToInstances (the proposed instances, the
// profile map and the findings) and capacityplan.PlanInstanceCapacity (the
// per-instance agent limits), and adds what the Hub reports about the
// legacy Runtime Broker. Nothing here writes a file, registers anything or
// sends a request other than a GET.
package brokerconvert

import (
	"errors"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// SchemaVersion is the version of the JSON plan document.
const SchemaVersion = 1

// TipNote heads every plan: the plan proposes, it never applies, and it
// prints no harness env value unless asked.
const TipNote = "Proposed only: nothing was written, registered or changed. Instance keys and names are proposals you may edit. Harness env values are redacted: shown as " + RedactedValue + " in the human output and omitted from JSON (keys only); --show-env-values prints them in the human output, which then contains secrets and must not be shared."

// RedactedValue replaces a harness env value in the plan.
const RedactedValue = "<redacted>"

// Summary results, in order of precedence.
const (
	ResultNothingToConvert  = "nothing_to_convert"
	ResultBlockingFindings  = "blocking_findings"
	ResultDecisionsRequired = "operator_decisions_required"
	ResultNoBlocking        = "no_blocking_findings"
)

// Capacity statuses.
const (
	// CapacityPlanned: the planner ran with the Hub's values.
	CapacityPlanned = "planned"
	// CapacityNeedsHub: the Hub sections were skipped or no Hub is
	// configured, so there is no agent limit to plan from.
	CapacityNeedsHub = "needs_hub"
	// CapacityUnavailable: the Hub did not return the Runtime Broker's
	// settings to this user.
	CapacityUnavailable = "unavailable"
)

// Mapping statuses of a profile reference.
const (
	MappingMapped         = "mapped"
	MappingNoMapping      = "no_mapping"
	MappingProfileUnknown = "profile_unknown"
)

// ErrInstancesConfigured refuses a host that already declares flat
// Runtime Broker instances.
var ErrInstancesConfigured = errors.New("this host already runs flat Runtime Broker instances (server.broker.instances is set); there is nothing to convert")

// Input is the host-local input of BuildLocal.
type Input struct {
	// Settings are the effective legacy settings: the global settings plus
	// the installed Hub overlay.
	Settings *config.VersionedSettings
	// RawSettings are the raw bytes of the global settings file, for the
	// converter's unknown and removed key checks. May be empty.
	RawSettings []byte
	// Profiles limits the conversion to these profiles; empty means all.
	Profiles []string
	// Choices are the operator's reviewed choices.
	Choices []config.ConversionChoice
	// ConfiguredRuntimes is passed to the converter unchanged.
	ConfiguredRuntimes func(string) bool
}

// Plan is the plan document. Its JSON field names are part of the CLI
// contract (schemaVersion 1).
type Plan struct {
	SchemaVersion  int           `json:"schemaVersion"`
	TipNote        string        `json:"tipNote"`
	LegacyBrokerID string        `json:"legacyBrokerId,omitempty"`
	LegacyBroker   *LegacyBroker `json:"legacyBroker,omitempty"`
	// Instances are the proposed instances in their publishable form
	// (config.PresentInstance), the shape a Runtime Broker publishes to the
	// Hub: no kubeconfig, context, service-account policy, env value or
	// secret target. The paste-ready settings block is human output only.
	Instances  []api.RuntimeTargetPresentation `json:"instances"`
	ProfileMap []ProfileMapping                `json:"profileMap"`
	Findings   []Finding                       `json:"findings"`
	Capacity   Capacity                        `json:"capacity"`
	// Agents and Projects are nil when the Hub sections were skipped.
	Agents   *AgentsSection   `json:"agents"`
	Projects *ProjectsSection `json:"projects"`
	Summary  Summary          `json:"summary"`

	// settingsInstances are the proposed instances in settings form, for
	// the human output's paste-ready block only.
	settingsInstances []config.V1RuntimeBrokerInstanceConfig

	// hubIncomplete records a Hub call that failed for a reason other
	// than visibility; the plan is still printed, and the command exits 1.
	hubIncomplete bool
}

// LegacyBroker is the Hub's row for this host's legacy Runtime Broker.
type LegacyBroker struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Profiles are the profile names the Runtime Broker last reported.
	Profiles []string `json:"profiles"`
	// LocalOnly and HubOnly list the profiles in scope that only one side
	// has. A difference is a notice, never a stop.
	LocalOnly []string `json:"localOnly,omitempty"`
	HubOnly   []string `json:"hubOnly,omitempty"`
}

// ProfileMapping is one profile in scope and the instance it maps to.
type ProfileMapping struct {
	Profile string `json:"profile"`
	// Instance is the proposed instance key; empty when the profile has a
	// blocking finding.
	Instance string `json:"instance,omitempty"`
	// BlockedBy is the code of the blocking finding that leaves the
	// profile unmapped.
	BlockedBy string `json:"blockedBy,omitempty"`
}

// Finding is one converter finding, rendered from its code and settings
// key path only. The converter's own message is never carried: it can
// quote settings values.
type Finding struct {
	Code     string   `json:"code"`
	Severity string   `json:"severity"`
	Path     string   `json:"path"`
	Profiles []string `json:"profiles"`
	// Text is the fixed text for Code (FindingText).
	Text string `json:"text"`
}

// Capacity is the proposed per-instance agent limit.
type Capacity struct {
	Status string `json:"status"`
	// Outcome is the planner's outcome when Status is planned.
	Outcome string `json:"outcome,omitempty"`
	// MaxAgents is the legacy Runtime Broker's own setting (nil: inherit).
	MaxAgents *int64 `json:"maxAgents,omitempty"`
	// Allocations are the proposed limits, keyed by proposed instance key.
	Allocations []Allocation `json:"allocations,omitempty"`
	// Reasons explain a required decision; each starts with a reason code.
	Reasons []string `json:"reasons,omitempty"`
	// Flags are notes that do not change the outcome.
	Flags []string `json:"flags,omitempty"`
	Note  string   `json:"note,omitempty"`
}

// Allocation is one proposed instance limit.
type Allocation struct {
	Instance  string `json:"instance"`
	MaxAgents int64  `json:"maxAgents"`
}

// AgentsSection lists the non-terminal agents still bound to the legacy
// Runtime Broker.
type AgentsSection struct {
	Items []AgentEntry `json:"items"`
	// TerminalCount is the number of bound agents in a terminal phase
	// (stopped, error, halted), which are not listed.
	TerminalCount int `json:"terminalCount"`
	// Note is set when the list is incomplete or could not be read.
	Note string `json:"note,omitempty"`
}

// AgentEntry is one bound agent.
type AgentEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"projectId,omitempty"`
	Project   string `json:"project,omitempty"`
	Phase     string `json:"phase,omitempty"`
	ProfileRef
}

// ProfileRef is a profile name and what it would map to.
type ProfileRef struct {
	Profile  string `json:"profile"`
	Mapping  string `json:"mapping"`
	Instance string `json:"instance,omitempty"`
}

// ProjectsSection lists the projects the legacy Runtime Broker provides.
type ProjectsSection struct {
	Items []ProjectEntry `json:"items"`
	Note  string         `json:"note,omitempty"`
}

// ProjectEntry is one providing project and its profile-valued settings.
type ProjectEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ActiveProfile is the project's activeProfile setting, when set.
	ActiveProfile *ProfileRef `json:"activeProfile,omitempty"`
	// ServiceAccountProfiles are the profile keys of the project's
	// per-profile default GCP service accounts (keys only).
	ServiceAccountProfiles []ProfileRef `json:"serviceAccountProfiles,omitempty"`
	// Note is set when the project's settings could not be read.
	Note string `json:"note,omitempty"`
}

// Summary closes the plan.
type Summary struct {
	Result           string `json:"result"`
	ProfilesInScope  int    `json:"profilesInScope"`
	MappedProfiles   int    `json:"mappedProfiles"`
	Instances        int    `json:"instances"`
	BlockingFindings int    `json:"blockingFindings"`
	NoticeFindings   int    `json:"noticeFindings"`
	DecisionRequired bool   `json:"decisionRequired"`
	Agents           int    `json:"agents"`
	Projects         int    `json:"projects"`
}

// Exit statuses of the plan command.
const (
	ExitClean   = 0
	ExitError   = 1
	ExitBlocked = 3
)

// ExitCode is the command's exit status for a produced plan: 1 when a Hub
// call failed for a reason other than visibility, 3 when the plan has a
// blocking finding or needs an operator decision, otherwise 0.
func (p *Plan) ExitCode() int {
	switch {
	case p.hubIncomplete:
		return ExitError
	case p.Summary.BlockingFindings > 0 || p.Summary.DecisionRequired:
		return ExitBlocked
	}
	return ExitClean
}

// BuildLocal builds the host-local part of the plan: the proposed
// instances, the profile map and the findings. Capacity is marked as
// needing the Hub; CollectHub replaces it. It refuses a host whose
// settings already declare flat instances.
func BuildLocal(in Input) (*Plan, error) {
	vs := in.Settings
	if vs == nil {
		vs = &config.VersionedSettings{}
	}
	if vs.Server != nil && vs.Server.Broker != nil && len(vs.Server.Broker.Instances) > 0 {
		return nil, ErrInstancesConfigured
	}

	scope := profilesInScope(vs, in.Profiles)
	p := &Plan{
		SchemaVersion: SchemaVersion,
		TipNote:       TipNote,
		Instances:     []api.RuntimeTargetPresentation{},
		ProfileMap:    []ProfileMapping{},
		Findings:      []Finding{},
		Capacity: Capacity{
			Status: CapacityNeedsHub,
			Note:   "the per-instance agent limits need the Hub (the legacy Runtime Broker's maxAgents setting)",
		},
	}
	if len(scope) == 0 {
		p.summarize()
		return p, nil
	}

	res := config.ConvertProfilesToInstances(config.ProfileConversionInput{
		Settings:           vs,
		RawSettings:        in.RawSettings,
		Profiles:           in.Profiles,
		Choices:            in.Choices,
		ConfiguredRuntimes: in.ConfiguredRuntimes,
	})
	p.settingsInstances = res.Instances
	for _, inst := range res.Instances {
		p.Instances = append(p.Instances, config.PresentInstance(inst).API())
	}
	blockedBy := map[string]string{}
	for _, f := range res.Findings {
		p.Findings = append(p.Findings, Finding{
			Code:     f.Code,
			Severity: f.Severity,
			Path:     f.Path,
			Profiles: nonNil(f.Profiles),
			Text:     FindingText(f.Code),
		})
		if f.Severity == config.ConversionSeverityBlocking {
			for _, name := range f.Profiles {
				if _, ok := blockedBy[name]; !ok {
					blockedBy[name] = f.Code
				}
			}
		}
	}
	for _, name := range scope {
		m := ProfileMapping{Profile: name}
		if key, ok := res.ProfileMap[name]; ok {
			m.Instance = key
		} else {
			m.BlockedBy = blockedBy[name]
		}
		p.ProfileMap = append(p.ProfileMap, m)
	}
	p.summarize()
	return p, nil
}

// profilesInScope is the sorted, de-duplicated list of profiles the
// converter considers: the named ones, or every profile in the settings.
func profilesInScope(vs *config.VersionedSettings, named []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(named) > 0 {
		for _, name := range named {
			add(name)
		}
	} else {
		for name := range vs.Profiles {
			add(name)
		}
	}
	sort.Strings(out)
	return out
}

// refFor maps a profile name through the plan's profile map.
func (p *Plan) refFor(profile string) ProfileRef {
	if profile == "" {
		return ProfileRef{Mapping: MappingProfileUnknown}
	}
	for _, m := range p.ProfileMap {
		if m.Profile == profile {
			if m.Instance != "" {
				return ProfileRef{Profile: profile, Mapping: MappingMapped, Instance: m.Instance}
			}
			break
		}
	}
	return ProfileRef{Profile: profile, Mapping: MappingNoMapping}
}

// summarize recomputes the summary from the plan.
func (p *Plan) summarize() {
	s := Summary{
		ProfilesInScope:  len(p.ProfileMap),
		Instances:        len(p.Instances),
		DecisionRequired: p.Capacity.Status == CapacityPlanned && p.Capacity.Outcome == capacityDecision,
	}
	for _, m := range p.ProfileMap {
		if m.Instance != "" {
			s.MappedProfiles++
		}
	}
	for _, f := range p.Findings {
		if f.Severity == config.ConversionSeverityBlocking {
			s.BlockingFindings++
		} else {
			s.NoticeFindings++
		}
	}
	if p.Agents != nil {
		s.Agents = len(p.Agents.Items)
	}
	if p.Projects != nil {
		s.Projects = len(p.Projects.Items)
	}
	switch {
	case s.ProfilesInScope == 0:
		s.Result = ResultNothingToConvert
	case s.BlockingFindings > 0:
		s.Result = ResultBlockingFindings
	case s.DecisionRequired:
		s.Result = ResultDecisionsRequired
	default:
		s.Result = ResultNoBlocking
	}
	p.Summary = s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return append([]string(nil), s...)
}

// findingTexts are the fixed texts of the converter's finding codes. They
// quote no settings value; the finding's key path and profiles say where.
var findingTexts = map[string]string{
	config.ConversionUnsupportedFlatTargetType:  "this runtime type has no flat Runtime Broker target; keep these profiles on the legacy Runtime Broker",
	config.ConversionFlatTargetScopeViolation:   "this setting reaches beyond one target scope, which a flat Runtime Broker instance cannot hold",
	config.ConversionProfileConflict:            "these profiles share a target but differ in one or more settings, so they keep separate instances; compare the proposed instances, then give the profiles the same values or add a field choice to merge them",
	config.ConversionProfileConflictResolved:    "an operator choice consolidated this field on one profile's value; the other profiles' values are not carried over",
	config.ConversionLegacyFieldNoEffect:        "this legacy setting has no effect on a flat Runtime Broker instance",
	config.ConversionLegacyFieldRemoved:         "this setting was removed and is not converted",
	config.ConversionUnknownSettingsKey:         "this key, profile or operator choice is not recognised or cannot be applied, so it is not converted",
	config.ConversionFieldNotValidForTargetType: "this field is not valid for the instance's target type and is not converted",
}

// FindingText is the fixed text for a finding code.
func FindingText(code string) string {
	if t, ok := findingTexts[code]; ok {
		return t
	}
	return "see the conversion rules for this finding code"
}
