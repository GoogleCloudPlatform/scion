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

package preflight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// HubSettingRow is the part of a hub_settings row the profiles check reads.
type HubSettingRow struct {
	Section string
	Origin  string
	// UpdatedBy is nil when the column is NULL.
	UpdatedBy *string
	Value     json.RawMessage
}

// ReadOnlyQueries is the database reader the preflight command implements
// with column-restricted reads in one read-only snapshot. Rows carry only
// the columns the checks need.
type ReadOnlyQueries interface {
	// HubSettings returns the hub_settings rows of sections that exist.
	HubSettings(ctx context.Context, sections []string) ([]HubSettingRow, error)
	// Agents returns the non-deleted agents. An applied_config that does
	// not decode (store.AppliedConfigProfile) gives AppliedProfile "" and is
	// counted in unparseable.
	Agents(ctx context.Context) (rows []agent.ProfileRefAgentRow, unparseable int, err error)
	// Projects returns every project, SharedWorkspace evaluated through
	// store.Project.IsSharedWorkspace.
	Projects(ctx context.Context) ([]agent.ProfileRefProject, error)
	// Providers returns the provider rows of broker brokerID.
	Providers(ctx context.Context, brokerID string) ([]agent.ProfileRefProvider, error)
	// Broker reports whether broker id has a row, and whether that row is a
	// flat instance (a non-empty runtime_target_id). The command takes the
	// ID only from the hub's legacy-only co-located broker accessor; a flat
	// row here means it was mis-wired, and the check does not apply.
	Broker(ctx context.Context, id string) (found, flat bool, err error)
}

// profilesCheckTimeout bounds the settings.profiles check.
const profilesCheckTimeout = 30 * time.Second

// ProfilesCheck returns the settings.profiles check: the profile references
// on this hub's co-located Runtime Broker that the settings the restarted
// hub will hold no longer resolve (config.CheckProfileCatalog).
func ProfilesCheck() Check {
	return Check{Name: ProfilesCheckName, Timeout: profilesCheckTimeout, Run: runProfilesCheck}
}

// catalogSections are the hub_settings sections the check evaluates.
var catalogSections = []string{"runtimes", "profiles"}

// The settings loaders the check calls; test seams for their error paths.
var (
	extractSection = opsettings.ExtractSectionFromKoanf
	catalogView    = config.ProfileCatalogView
	profileOrigins = config.ProfileOrigins
)

func runProfilesCheck(ctx context.Context, env Env) ([]Finding, error) {
	code := func(suffix string) string { return ProfilesCheckName + "." + suffix }
	// Without a legacy co-located Runtime Broker the hub's stored profiles
	// affect no broker in this process: the check does not apply. A flat
	// instance co-located with the hub is a legitimate deployment, so it is
	// information, never a block (blocking would make force routine there).
	notApplicable := func(why string) []Finding {
		return []Finding{{Code: code("not_applicable"), Severity: SeverityInfo,
			Message: "no legacy co-located broker; profile check skipped: " + why}}
	}
	if env.ColocatedBrokerID == "" {
		return notApplicable("this hub has no legacy co-located Runtime Broker"), nil
	}
	if env.DB == nil {
		return nil, errors.New("no database reader")
	}
	found, flat, err := env.DB.Broker(ctx, env.ColocatedBrokerID)
	if err != nil {
		return nil, fmt.Errorf("read the co-located broker row: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("co-located broker %s has no broker row", env.ColocatedBrokerID)
	}
	if flat {
		return notApplicable(fmt.Sprintf("the co-located broker %s is a flat instance", env.ColocatedBrokerID)), nil
	}
	rows, err := env.DB.HubSettings(ctx, catalogSections)
	if err != nil {
		return nil, fmt.Errorf("read hub_settings: %w", err)
	}
	bySection := map[string]*HubSettingRow{}
	for i := range rows {
		r := &rows[i]
		if r.Origin == "" {
			return nil, fmt.Errorf("hub_settings row %s has no origin", r.Section)
		}
		bySection[r.Section] = r
	}

	var out []Finding
	scan, scanErr := config.ScanSettingsFileProfileTimezones(env.GlobalDir)
	if scanErr != nil {
		out = append(out, Finding{Code: code("timezone_scan_failed"), Severity: SeverityWarn,
			Message: "the settings file could not be scanned for removed profile timezone keys; profile names are not affected"})
	}
	bootstrap := config.LoadServerBootstrapKoanf(env.ServerConfigPath, scan.ProfileTimezones)

	// before: the rows as they stand (the running hub's); after: the rows
	// the restarted hub's boot-time re-sync leaves.
	beforeDocs := map[string]json.RawMessage{}
	afterDocs := map[string]json.RawMessage{}
	for _, section := range catalogSections {
		bootDoc, err := extractSection(bootstrap, section)
		if err != nil {
			slog.Warn("preflight: the bootstrap section does not extract", "section", section, "error", err)
			return append(out, Finding{Code: code("could_not_evaluate"), Severity: SeverityBlock,
				Message:     fmt.Sprintf("the preflight could not evaluate the %s bootstrap material: %s", section, loaderErrorClass("the server config or SCION_SEED settings for it do not decode", err)),
				Remediation: "fix the server config or SCION_SEED material for this section, then run the rebuild again"}), nil
		}
		var row *opsettings.SectionRow
		if r := bySection[section]; r != nil {
			row = &opsettings.SectionRow{Origin: r.Origin, UpdatedBy: r.UpdatedBy, Value: r.Value}
			beforeDocs[section] = r.Value
		}
		if doc, present := opsettings.SyncedSection(section, row, bootDoc); present {
			afterDocs[section] = doc
		}
	}
	beforeSec, beforeErrs := config.OverlayMapSections(beforeDocs, bootstrap)
	afterSec, afterErrs := config.OverlayMapSections(afterDocs, bootstrap)
	for _, e := range append(beforeErrs, afterErrs...) {
		if e.Section == "runtimes" || e.Section == "profiles" {
			return nil, fmt.Errorf("the stored %s section does not decode", e.Section)
		}
	}

	agents, unparseable, err := env.DB.Agents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read agents: %w", err)
	}
	projects, err := env.DB.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("read projects: %w", err)
	}
	providers, err := env.DB.Providers(ctx, env.ColocatedBrokerID)
	if err != nil {
		return nil, fmt.Errorf("read the co-located broker's providers: %w", err)
	}
	refs, counted, _ := agent.BuildProfileReferencesDetailed(env.GlobalDir, env.ColocatedBrokerID,
		agent.ProfileRefInputs{Providers: providers, Projects: projects, Agents: agents})

	after, err := catalogView(env.GlobalDir, afterSec.Runtimes, afterSec.Profiles, &refs)
	if err != nil {
		slog.Warn("preflight: the effective settings do not load", "error", err)
		return nil, errors.New(loaderErrorClass("the hub's effective settings do not load", err))
	}
	before, err := catalogView(env.GlobalDir, beforeSec.Runtimes, beforeSec.Profiles, nil)
	if err != nil {
		slog.Warn("preflight: the effective settings do not load", "error", err)
		return nil, errors.New(loaderErrorClass("the hub's effective settings do not load", err))
	}
	origins, err := profileOrigins(env.GlobalDir)
	if err != nil {
		slog.Warn("preflight: the profile origins do not load", "error", err)
		return append(out, Finding{Code: code("could_not_evaluate"), Severity: SeverityBlock,
			Message:     "the preflight could not label where profiles come from: " + loaderErrorClass("the settings files under the hub's global directory do not load", err),
			Remediation: "check that the settings files under the hub's global directory load"}), nil
	}

	provided := map[string]bool{}
	for _, pv := range providers {
		provided[pv.ProjectID] = true
	}
	projectActive := map[string]string{}
	for _, p := range projects {
		if provided[p.ID] && p.ActiveProfile != "" {
			projectActive[p.ID] = p.ActiveProfile
		}
	}

	subj := newSubjectIndex(counted, projectActive)
	for _, f := range config.CheckProfileCatalog(after, before, origins, projectActive, refs.Agents) {
		out = append(out, profileFinding(f, refs, subj))
	}
	if unparseable > 0 {
		out = append(out, Finding{Code: code("unparseable_applied_config"), Severity: SeverityWarn,
			Message: fmt.Sprintf("%d agent(s) have an applied config that does not decode; they are counted as having no saved profile, as the hub counts them", unparseable),
			Data:    map[string]any{"agents": unparseable}})
	}
	if refs.AgentInfoMissing > 0 || refs.AgentInfoUnreadable > 0 {
		out = append(out, Finding{Code: code("agent_info_missing"), Severity: SeverityInfo,
			Message: fmt.Sprintf("%d agent(s) on the co-located broker have no agent-info.json and %d have one that cannot be read; their profile is taken from the hub. A high missing count can mean the preflight looked in the wrong directory (it uses the hub's HOME)",
				refs.AgentInfoMissing, refs.AgentInfoUnreadable),
			Data: map[string]any{"missing": refs.AgentInfoMissing, "unreadable": refs.AgentInfoUnreadable}})
	}
	return out, nil
}

// keyPathRE finds the key paths a decode error names: mapstructure's
// "* 'profiles.a.runtime' expected type ..." and koanf's "'profiles.a'
// ..." forms. Only a quoted dotted name of key characters counts.
var keyPathRE = regexp.MustCompile(`(?m)(?:^|\* )'([A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*)' `)

// loaderErrorClass is how a settings loader's error is reported in a
// finding: the fixed class, plus the key paths the error names. The error
// text itself never goes into a finding, because a decode error quotes the
// value it could not convert; the caller logs it locally.
func loaderErrorClass(class string, err error) string {
	seen := map[string]bool{}
	var paths []string
	for _, m := range keyPathRE.FindAllStringSubmatch(err.Error(), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			paths = append(paths, m[1])
		}
	}
	if len(paths) == 0 {
		return class
	}
	sort.Strings(paths)
	return fmt.Sprintf("%s (at %s)", class, joinPaths(paths))
}

func joinPaths(paths []string) string {
	out := paths[0]
	for _, p := range paths[1:] {
		out += ", " + p
	}
	return out
}

// subjectIndex lists the counted agents by profile, and the projects whose
// activeProfile references a profile.
type subjectIndex struct {
	agents   map[string][]string
	projects map[string][]string
}

func newSubjectIndex(counted []agent.CountedAgent, projectActive map[string]string) subjectIndex {
	s := subjectIndex{agents: map[string][]string{}, projects: map[string][]string{}}
	for _, c := range counted {
		project := c.ProjectSlug
		if project == "" {
			project = c.ProjectID
		}
		s.agents[c.Profile] = append(s.agents[c.Profile], project+"/"+c.AgentSlug)
	}
	for _, list := range s.agents {
		sort.Strings(list)
	}
	for id, profile := range projectActive {
		s.projects[profile] = append(s.projects[profile], id)
	}
	for _, list := range s.projects {
		sort.Strings(list)
	}
	return s
}

// forProfile is the agents counted under profile, plus those without a
// saved profile when the finding's count includes them (the profile is the
// default); with no agent, the projects referencing profile.
func (s subjectIndex) forProfile(profile string, agents int) []string {
	out := append([]string(nil), s.agents[profile]...)
	if agents > len(out) {
		out = append(out, s.agents[""]...)
	}
	if len(out) == 0 {
		out = append(out, s.projects[profile]...)
	}
	return out
}

// sourcePhrase says where a profile resolved before the change.
func sourcePhrase(s config.ProfileSource) string {
	switch s {
	case config.ProfileSourceStored:
		return "one of the hub's stored profiles"
	case config.ProfileSourceFile:
		return "defined in a settings file"
	case config.ProfileSourceBuiltin:
		return "a built-in default profile"
	case config.ProfileSourceSettings:
		return "a settings-file or built-in profile"
	default:
		return "not defined"
	}
}

// agentScope is the scope sentence of an agent count (D2).
const agentScope = "agents on this hub's co-located broker, or not yet placed, are counted; agents on remote brokers resolve profiles against their own settings and are not counted"

// profileFinding maps a catalog finding to a preflight finding: severity
// and code from ProfileFindingSeverity, a message and remediation built from
// names and counts only, subjects, and Data.
func profileFinding(f config.ProfileCatalogFinding, refs config.ProfileReferences, subj subjectIndex) Finding {
	sev, code := ProfileFindingSeverity(f)
	out := Finding{Code: code, Severity: sev, Data: map[string]any{"profile": f.Profile, "agents": f.Agents}}
	if f.Previous != "" {
		out.Data["previous"] = f.Previous
	}
	src := sourcePhrase(f.Source)
	switch f.Kind {
	case config.ProfileFindingAgentSavedProfile:
		out.Subjects = subj.forProfile(f.Profile, f.Agents)
		if f.PreExisting {
			out.Message = fmt.Sprintf("%d agent(s) use saved profile %q, which is already not available, so their dispatches already fail; before, it was %s (%s)", f.Agents, f.Profile, src, agentScope)
		} else {
			out.Message = fmt.Sprintf("%d agent(s) use saved profile %q, which is not available after the update; before, it was %s (%s)", f.Agents, f.Profile, src, agentScope)
		}
		out.Remediation = "add the profile to the hub's stored profiles, or move the agents to a stored profile"
	case config.ProfileFindingActiveProfile:
		out.Subjects = subj.forProfile("", f.Agents)
		name := fmt.Sprintf("active_profile %q", f.Profile)
		if f.Profile == "" {
			name = "active_profile (unset)"
		}
		if f.PreExisting {
			out.Message = fmt.Sprintf("dispatches of agents without a profile ALREADY fail: %s is not available (%d agents without a saved profile; %s)", name, f.Agents, agentScope)
		} else {
			out.Message = fmt.Sprintf("%s is not available after the update; before, it was %s. %d agent(s) without a saved profile dispatch on it (%s)", name, src, f.Agents, agentScope)
		}
		if f.Previous != "" {
			out.Message += fmt.Sprintf("; before, the default resolved to %q", f.Previous)
		}
		out.Remediation = "set active_profile to one of the hub's stored profiles, or add the profile to them"
	case config.ProfileFindingProjectActiveProfile:
		out.Subjects = []string{f.Project}
		out.Data["project"] = f.Project
		if f.PreExisting {
			out.Message = fmt.Sprintf("project %s's activeProfile %q is already not available; before, it was %s", f.Project, f.Profile, src)
		} else {
			out.Message = fmt.Sprintf("project %s's activeProfile %q is not available after the update; before, it was %s", f.Project, f.Profile, src)
		}
		out.Remediation = "set the project's activeProfile to one of the hub's stored profiles, or add the profile to them"
	case config.ProfileFindingProfileRuntimeMissing:
		out.Subjects = subj.forProfile(f.Profile, f.Agents)
		if f.PreExisting {
			out.Message = fmt.Sprintf("profile %q resolves, but the runtime it names already does not (%d agent(s) use it; %s)", f.Profile, f.Agents, agentScope)
		} else {
			out.Message = fmt.Sprintf("profile %q resolves after the update, but the runtime it names does not (%d agent(s) use it; %s)", f.Profile, f.Agents, agentScope)
		}
		out.Remediation = "add the runtime the profile names to the hub's stored runtimes, or point the profile at a stored runtime"
	case config.ProfileFindingDroppedUnused:
		out.Message = fmt.Sprintf("profile %q is not available after the update, and nothing uses it; before, it was %s", f.Profile, src)
		out.Remediation = "add the profile to the hub's stored profiles if it should stay available"
	case config.ProfileFindingActiveProfileFallback:
		out.Subjects = subj.forProfile("", f.Agents)
		if f.Profile == "" {
			out.Message = fmt.Sprintf("active_profile is unset; %d agent(s) without a profile will use the single stored profile", f.Agents)
		} else {
			out.Message = fmt.Sprintf("active_profile %q is not one of the hub's stored profiles; %d agent(s) without a profile will use the single stored profile instead; before, it was %s", f.Profile, f.Agents, src)
		}
		if f.Previous != "" {
			out.Message += fmt.Sprintf("; before, the default resolved to %q", f.Previous)
		}
	case config.ProfileFindingActiveProfileChanged:
		out.Subjects = subj.forProfile("", f.Agents)
		out.Message = fmt.Sprintf("the default profile changes from %q to %q; %d agent(s) without a saved profile will dispatch on %q", f.Previous, f.Profile, f.Agents, f.Profile)
	case config.ProfileFindingRetainedBuiltinProfile:
		out.Subjects = subj.forProfile(f.Profile, f.Agents)
		projects := refs.Projects[f.Profile]
		if refs.Agents[f.Profile] == 0 && projects == 0 {
			out.Message = fmt.Sprintf("built-in profile %q is kept beside the hub's stored profiles because it is the hub's active_profile", f.Profile)
			if f.Agents > 0 {
				out.Message += fmt.Sprintf(" (%d agent(s) without a saved profile use it)", f.Agents)
			}
		} else {
			why := fmt.Sprintf("%d agent(s)", f.Agents)
			if projects > 0 {
				why += fmt.Sprintf(" and %d project(s)", projects)
			}
			out.Message = fmt.Sprintf("built-in profile %q is kept beside the hub's stored profiles because %s use it (ptone/scion#5113)", f.Profile, why)
		}
		out.Remediation = "add it to the hub's stored settings to manage it explicitly"
	case config.ProfileFindingRetainedBuiltinRuntime:
		if f.Agents > 0 {
			out.Message = fmt.Sprintf("built-in runtime %q is kept beside the hub's stored runtimes because profiles in use name it (%d agent(s))", f.Profile, f.Agents)
		} else {
			out.Message = fmt.Sprintf("built-in runtime %q is kept beside the hub's stored runtimes because a kept profile names it (no agent uses it yet)", f.Profile)
		}
		out.Remediation = "add it to the hub's stored settings to manage it explicitly"
	default:
		out.Message = fmt.Sprintf("profile catalog finding %q for %q", f.Kind, f.Profile)
	}
	return out
}
