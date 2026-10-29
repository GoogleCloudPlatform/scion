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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// placementPreflightKey is the GET field of the report. A PUT body that
// sends it back has it dropped: it is read-only, never a setting.
const placementPreflightKey = "placement_preflight"

// placementPreflightNoColocated is the fixed phrase for the agents of a Hub
// with no co-located legacy Runtime Broker.
const placementPreflightNoColocated = "agents of other Runtime Brokers (this Hub has no co-located legacy Runtime Broker)"

// PlacementPreflightView is the read-only Kubernetes placement pre-upgrade
// report of GET /api/v1/admin/server-config: the stored runtimes and
// profiles (the settings the co-located legacy Runtime Broker uses) by
// category, and the stored configurations of that broker's agents by
// category. Counts and section names only, never names or values. There is
// no request field for it.
type PlacementPreflightView struct {
	// SettingsCounts maps each settings category to the number of entries
	// in it (entry pairs for sibling disagreement).
	SettingsCounts map[string]int `json:"settings_counts"`
	// SettingsRepairSections names the sections holding a finding.
	SettingsRepairSections []string `json:"settings_repair_sections,omitempty"`
	// SettingsIncomplete is true when a stored section could not be read.
	SettingsIncomplete bool `json:"settings_incomplete,omitempty"`
	// AgentsScanned counts the agents of the co-located legacy Runtime
	// Broker that were categorized; AgentsIncomplete those that could not
	// be (an unreadable record or configuration, or a profile or runtime
	// that cannot be resolved offline); AgentsNotCovered the agents of
	// other Runtime Brokers or not yet placed, which this scan does not
	// reach. Incomplete and not covered agents are never counted as clean.
	AgentsScanned    int `json:"agents_scanned"`
	AgentsIncomplete int `json:"agents_incomplete"`
	AgentsNotCovered int `json:"agents_not_covered"`
	// AgentRecordsUnreadable is true when the agent rows could not be
	// listed.
	AgentRecordsUnreadable bool `json:"agent_records_unreadable,omitempty"`
	// AgentsNotEnumerated names, by a fixed phrase, the agents this scan
	// cannot list at all (incomplete coverage).
	AgentsNotEnumerated []string `json:"agents_not_enumerated,omitempty"`
	// AgentCounts maps each agent category to the number of agents in it.
	// An agent can be in several categories.
	AgentCounts map[string]int `json:"agent_counts"`
}

// placementPreflightTimeout bounds the whole agent half of one report:
// the store listing and every agent's file reads. The report runs inside
// the server-config GET, so it is shorter than the background reference
// count's bound. A variable so tests can shorten it.
var placementPreflightTimeout = 10 * time.Second

// placementPreflight builds the placement pre-upgrade report from the
// current stored runtimes and profiles and the co-located legacy Runtime
// Broker's agents, within placementPreflightTimeout. It writes nothing and
// never fails the GET: a failure anywhere in it gives a report whose
// settings and agent coverage are both incomplete.
func (s *Server) placementPreflight(ctx context.Context, ops *OperationalSettings) (view *PlacementPreflightView) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("GET server-config: placement pre-upgrade report failed", "panic", r)
			view = placementPreflightView(config.SettingsPlacementReport{Unreadable: true},
				config.PlacementPreflightReport{RecordsUnreadable: true})
		}
	}()
	snap := ops.Snapshot()
	settings := config.ScanSettingsPlacement(snap.Runtimes, snap.Profiles)
	for _, section := range []string{"runtimes", "profiles"} {
		raw, ok := ops.rawSection(section)
		if !ok {
			continue
		}
		var entries map[string]interface{}
		if err := json.Unmarshal(raw, &entries); err != nil {
			settings.Unreadable = true
			continue
		}
		settings.AddRawSection(section, entries)
	}

	configured := placementConfiguredRuntimes(ops)
	policy := func(profile string) (*api.ResolvedPlacement, bool) {
		return config.ResolveLegacyPolicyOffline(snap.Runtimes, snap.Profiles, configured, profile)
	}
	_, brokerID, globalDir := s.profileRefs.target()
	if brokerID == "" {
		brokerID = s.LegacyColocatedBrokerID()
	}
	if globalDir == "" {
		globalDir, _ = config.GetGlobalDir()
	}
	var agents config.PlacementPreflightReport
	if brokerID == "" {
		// No co-located legacy Runtime Broker: this Hub has no agent
		// configurations of its own to read. Agents are checked by the
		// Runtime Broker that starts them.
		agents.NotEnumerated = []string{placementPreflightNoColocated}
		return placementPreflightView(settings, agents)
	}
	if s.store == nil {
		agents.RecordsUnreadable = true
		return placementPreflightView(settings, agents)
	}
	return placementPreflightView(settings, s.scanColocatedPlacement(ctx, brokerID, globalDir, policy))
}

// placementConfiguredRuntimes reports which runtime entries are the
// operator's, as the co-located broker decides (config.ConfiguredRuntimeNames):
// the entries of a stored runtimes row when it holds any (the overlay then
// replaces the section), otherwise the settings file's configured entries.
// Built-in default entries are not the operator's. nil (every entry) when
// that cannot be told, which fails closed.
func placementConfiguredRuntimes(ops *OperationalSettings) func(string) bool {
	ops.mu.RLock()
	state, stored := ops.cache["runtimes"]
	ops.mu.RUnlock()
	if stored {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(state.Value, &row); err != nil {
			return nil
		}
		if len(row) > 0 {
			return func(name string) bool { _, ok := row[name]; return ok }
		}
	}
	names, ok := config.ConfiguredRuntimeNames()
	if !ok {
		return nil
	}
	return func(name string) bool { return names[name] }
}

// scanColocatedPlacement runs the co-located broker's agent scan within
// placementPreflightTimeout: the store listing and the per-agent reads
// (agents not read by the deadline are incomplete). The report is advisory
// and never fails the settings GET: a listing error, or a failure inside
// the scan, marks the agent records unreadable (coverage incomplete) and
// is logged.
func (s *Server) scanColocatedPlacement(ctx context.Context, brokerID, globalDir string, policy agent.PlacementPolicyFunc) (agents config.PlacementPreflightReport) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("GET server-config: placement pre-upgrade scan failed", "panic", r)
			agents = config.PlacementPreflightReport{RecordsUnreadable: true}
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, placementPreflightTimeout)
	defer cancel()
	in, other, err := s.colocatedAgentRows(runCtx, brokerID)
	if err != nil {
		slog.Warn("GET server-config: placement pre-upgrade scan could not list agents", "error", err)
		return config.PlacementPreflightReport{RecordsUnreadable: true}
	}
	agents = agent.ScanColocatedLegacyPlacement(runCtx, globalDir, brokerID, in, policy)
	agents.NotCovered += other
	return agents
}

// placementPreflightView is the GET view of the two scans.
func placementPreflightView(settings config.SettingsPlacementReport, agents config.PlacementPreflightReport) *PlacementPreflightView {
	view := &PlacementPreflightView{
		SettingsCounts:         map[string]int{},
		SettingsRepairSections: settings.Sections(),
		SettingsIncomplete:     settings.Unreadable,
		AgentsScanned:          agents.Scanned,
		AgentsIncomplete:       agents.Incomplete,
		AgentsNotCovered:       agents.NotCovered,
		AgentRecordsUnreadable: agents.RecordsUnreadable,
		AgentsNotEnumerated:    agents.NotEnumerated,
		AgentCounts:            map[string]int{},
	}
	for _, c := range config.SettingsPlacementCategories {
		view.SettingsCounts[string(c)] = settings.Counts[c]
	}
	for _, c := range config.PlacementPreflightCategories {
		view.AgentCounts[string(c)] = agents.Counts[c]
	}
	return view
}
