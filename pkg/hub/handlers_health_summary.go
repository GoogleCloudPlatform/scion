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
	"database/sql"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// healthSummaryBrokerLimit caps the runtime broker rows returned by the
// health summary. The total is still reported so the dashboard can show
// that the list was truncated. A variable so tests can lower it.
var healthSummaryBrokerLimit = 100

// healthSummaryBrokerPageSize is the store page size used while counting
// runtime broker records for the health summary.
const healthSummaryBrokerPageSize = 500

// HealthSummaryResponse is the composite health summary returned by
// GET /api/v1/admin/health/summary. It aggregates all subsystem health
// into a single response for the health dashboard.
type HealthSummaryResponse struct {
	Status   string                 `json:"status"`
	Hub      HealthSummaryHub       `json:"hub"`
	Database HealthSummaryDB        `json:"database"`
	Brokers  HealthSummaryBrokers   `json:"runtime_brokers"`
	Agents   *HealthSummaryAgents   `json:"agents"`   // nil when the agent aggregate is unavailable
	Dispatch *HealthSummaryDispatch `json:"dispatch"` // nil when dispatch metrics are unavailable
}

// HealthSummaryHub contains hub-level health information.
type HealthSummaryHub struct {
	Status           string `json:"status"`
	Version          string `json:"version"`
	Uptime           string `json:"uptime"`
	ConnectedBrokers int    `json:"connected_brokers"`
	ActiveAgents     int    `json:"active_agents"`
	Projects         int    `json:"projects"`
	// Checks is the hub's /healthz check map, so a degraded or unhealthy
	// hub status carries its cause (e.g. colocated_broker) rather than
	// only the database check surfacing below.
	Checks map[string]string `json:"checks,omitempty"`
	// UnhealthyChecks lists the non-healthy checks as "key: value", sorted,
	// so a dashboard can show the cause without interpreting the map.
	UnhealthyChecks []string `json:"unhealthy_checks,omitempty"`
}

// HealthSummaryDB contains database health information.
// Fields are sourced from Go's sql.DBStats (runtime pool counters) rather than
// the OTel-based metrics described in the design doc. sql.DBStats provides
// accurate, zero-latency pool stats without depending on the metrics pipeline,
// which may itself be the thing that is unhealthy.
type HealthSummaryDB struct {
	Status     string `json:"status"`
	PoolActive int64  `json:"pool_active"`
	PoolMax    int64  `json:"pool_max"`
	PoolIdle   int64  `json:"pool_idle"`
	// PoolWaitCountTotal is the cumulative number of times a caller had to wait
	// for a DB connection (monotonically increasing counter from sql.DBStats.WaitCount).
	PoolWaitCountTotal int64 `json:"pool_wait_count_total"`
}

// HealthSummaryBrokers is the runtime broker section of the health
// summary. Plugin (message-broker) records are never included.
type HealthSummaryBrokers struct {
	Items []HealthSummaryBroker `json:"items"`
	// Total is the number of runtime brokers, excluding plugin records.
	Total int `json:"total"`
	// Truncated is true when Total exceeds len(Items).
	Truncated bool `json:"truncated"`
}

// HealthSummaryBroker contains per-broker health information.
type HealthSummaryBroker struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Status is broker liveness: online, offline or degraded.
	Status string `json:"status"`
	// LastHeartbeat is null when the broker has never sent a heartbeat.
	LastHeartbeat *time.Time `json:"last_heartbeat"`
	// Runtime is null when the broker reported no usable profile.
	Runtime *HealthBrokerRuntime `json:"runtime"`
	// WorkspaceStorage is null when the broker never reported it.
	WorkspaceStorage *HealthBrokerStorage `json:"workspace_storage"`
	// Health is the broker's last self-reported health, or null when the
	// broker never reported it (an older broker). It is as fresh as
	// LastHeartbeat and never changes Status. For an offline broker it is
	// the last report before the broker went offline.
	Health *HealthBrokerSelf  `json:"health"`
	Agents HealthBrokerAgents `json:"agents"`
}

// HealthBrokerSelf is a runtime broker's self-reported health.
type HealthBrokerSelf struct {
	// Status is healthy, degraded or unhealthy.
	Status string `json:"status"`
	// Checks maps each check the broker ran to its result, for example
	// {"runtime": "unavailable"}. Values are fixed words only (see
	// api.NormalizeBrokerHealthReport), never free text.
	Checks map[string]string `json:"checks"`
}

// HealthBrokerAgents holds per-broker agent counts.
type HealthBrokerAgents struct {
	// Running is the number of agents on the broker in phase running.
	Running int `json:"running"`
	// Attention is the number of agents on the broker needing attention:
	// phase error, or activity crashed or offline outside phase stopped.
	// Stalled agents never count.
	Attention int `json:"attention"`
}

// HealthBrokerRuntime is the runtime a broker places agents on by default.
type HealthBrokerRuntime struct {
	Type    string `json:"type"`
	Profile string `json:"profile"`
}

// HealthBrokerStorage is the workspace storage a broker reports on each
// heartbeat.
type HealthBrokerStorage struct {
	// Backend is "local" or "nfs".
	Backend string `json:"backend"`
	// NFSHealthy is set only when Backend is "nfs".
	NFSHealthy *bool `json:"nfs_healthy,omitempty"`
}

// HealthSummaryAgents is the agents section of the health summary. It
// carries no stall data: stalls are routine and are not a health signal.
type HealthSummaryAgents struct {
	// Total is the number of non-deleted agents.
	Total int `json:"total"`
	// Active is Total minus agents in phase stopped or error.
	Active int `json:"active"`
	// Errored is the number of agents in phase error or with activity
	// crashed, each agent once, stopped agents excluded.
	Errored int `json:"errored"`
	// Considered is the number of non-deleted agents not in phase stopped.
	// Errored never exceeds it.
	Considered int `json:"considered"`
	// ByPhase lists the phases that have agents, in lifecycle order
	// (state.Phases()); phases outside that list follow, sorted by name.
	ByPhase []HealthPhaseCount `json:"by_phase"`
	// Problems has one group per problem kind (errored, crashed, offline),
	// in that order. Empty groups are omitted.
	Problems []HealthAgentGroup `json:"problems"`
}

// HealthPhaseCount is the number of agents in one phase.
type HealthPhaseCount struct {
	Phase string `json:"phase"`
	Count int    `json:"count"`
}

// Agent problem group kinds.
const (
	HealthAgentGroupErrored = "errored" // phase error
	HealthAgentGroupCrashed = "crashed" // activity crashed, not stopped
	HealthAgentGroupOffline = "offline" // activity offline, not stopped
)

// HealthAgentGroup is one kind of agent problem.
type HealthAgentGroup struct {
	Kind string `json:"kind"`
	// Count is the true number of agents of this kind; Items is capped at
	// store.AgentHealthRefCap.
	Count int              `json:"count"`
	Items []HealthAgentRef `json:"items"`
}

// HealthAgentRef identifies one agent in a problem group.
type HealthAgentRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`
	// ProjectSlug is empty when the project could not be resolved.
	ProjectSlug string `json:"project_slug"`
	// BrokerID is empty when the agent is not placed on a broker.
	BrokerID string `json:"broker_id"`
}

// HealthSummaryDispatch contains dispatch pipeline health.
// When nil in HealthSummaryResponse, dispatch metrics are not available.
type HealthSummaryDispatch struct {
	StuckMessages int `json:"stuck_messages"`
	Failed1h      int `json:"failed_1h"`
}

// handleHealthSummary handles GET /api/v1/admin/health/summary.
// Returns a composite health summary aggregating all subsystems.
// Authorization: enforced by routeGuard via hub.health.read permission.
func (s *Server) handleHealthSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()

	// Get base health info
	healthInfo := s.GetHealthInfo(ctx)

	// Determine overall status early so DB-error branches can degrade it.
	// Later signals only ever raise severity (worseHealthStatus): an
	// unhealthy hub must not be reported as merely degraded.
	overallStatus := HealthStatusHealthy
	if healthInfo.Status != "" {
		overallStatus = worseHealthStatus(overallStatus, healthInfo.Status)
	}
	degrade := func() { overallStatus = worseHealthStatus(overallStatus, HealthStatusDegraded) }

	// Build hub section
	hubSummary := HealthSummaryHub{
		Status:          healthInfo.Status,
		Version:         healthInfo.ScionVersion,
		Uptime:          healthInfo.Uptime,
		Checks:          healthInfo.Checks,
		UnhealthyChecks: unhealthyChecks(healthInfo.Checks),
	}
	if healthInfo.Stats != nil {
		hubSummary.ConnectedBrokers = healthInfo.Stats.ConnectedBrokers
		hubSummary.ActiveAgents = healthInfo.Stats.ActiveAgents
		hubSummary.Projects = healthInfo.Stats.Projects
	}

	// Build database section
	dbSummary := HealthSummaryDB{
		Status: "healthy",
	}
	if healthInfo.Checks != nil {
		if dbStatus, ok := healthInfo.Checks["database"]; ok {
			dbSummary.Status = dbStatus
		}
	}
	// Get pool stats from sql.DB if available
	if dbp, ok := s.store.(interface{ DB() *sql.DB }); ok {
		db := dbp.DB()
		if db != nil {
			stats := db.Stats()
			dbSummary.PoolActive = int64(stats.InUse)
			dbSummary.PoolIdle = int64(stats.Idle)
			dbSummary.PoolWaitCountTotal = stats.WaitCount
			dbSummary.PoolMax = int64(stats.MaxOpenConnections)
		}
	}

	// Use aggregate queries instead of fetching full agent records.
	// This avoids deserialising up to 10 000 structs on every 30 s poll.
	// A nil section means "not reported": the dashboard must not read a
	// failed aggregate as zero agents with nothing needing attention.
	var agentsSummary *HealthSummaryAgents
	agentAgg, err := s.store.AggregateAgentHealth(ctx)
	if err != nil {
		slog.Error("health summary: failed to aggregate agent health", "error", err)
		degrade()
	} else {
		summary := s.healthSummaryAgents(ctx, agentAgg)
		agentsSummary = &summary
	}

	// Build brokers section using pre-computed agent buckets from the aggregate.
	brokerList, err := s.healthSummaryBrokers(ctx, agentAgg)
	if err != nil {
		slog.Error("health summary: failed to list runtime brokers", "error", err)
		degrade()
	}

	// Dispatch section: the dispatchmetrics.Recorder does not currently expose a
	// Stats() method for reading in-process counters. Rather than returning
	// hardcoded zeros (which would mislead operators), we omit the dispatch data
	// and let the dashboard render "data not available". TODO: add a Stats()
	// method to dispatchmetrics.Recorder to populate this section.
	var dispatchSummary *HealthSummaryDispatch // nil = unavailable

	// Propagate unhealthy agent/broker signals into overall status.
	// Stalled agents never count.
	if agentsSummary != nil && agentsSummary.Errored > 0 {
		degrade()
	}
	for _, b := range brokerList.Items {
		if healthSummaryBrokerStatusIsProblem(b.Status) {
			degrade()
			break
		}
	}

	resp := HealthSummaryResponse{
		Status:   overallStatus,
		Hub:      hubSummary,
		Database: dbSummary,
		Brokers:  brokerList,
		Agents:   agentsSummary,
		Dispatch: dispatchSummary,
	}

	writeJSON(w, http.StatusOK, resp)
}

// healthSummaryAgents builds the agents section from the store aggregate.
// Project slugs for the capped references are resolved with one batched
// project lookup; if it fails the slugs stay empty and the rest is kept.
func (s *Server) healthSummaryAgents(ctx context.Context, agg *store.AgentHealthAggregate) HealthSummaryAgents {
	out := HealthSummaryAgents{
		Total:      agg.Total,
		Active:     agg.Total - agg.ByPhase[string(state.PhaseStopped)] - agg.ByPhase[string(state.PhaseError)],
		Errored:    agg.Errored,
		Considered: agg.Considered,
		ByPhase:    orderedPhaseCounts(agg.ByPhase),
		Problems:   []HealthAgentGroup{},
	}
	groups := []struct {
		kind  string
		group store.AgentProblemGroup
	}{
		{HealthAgentGroupErrored, agg.ErrorPhase},
		{HealthAgentGroupCrashed, agg.Crashed},
		{HealthAgentGroupOffline, agg.Offline},
	}
	var projectIDs []string
	seen := map[string]bool{}
	for _, g := range groups {
		for _, r := range g.group.Refs {
			if r.ProjectID != "" && !seen[r.ProjectID] {
				seen[r.ProjectID] = true
				projectIDs = append(projectIDs, r.ProjectID)
			}
		}
	}
	slugs := s.healthSummaryProjectSlugs(ctx, projectIDs)
	for _, g := range groups {
		if g.group.Count == 0 {
			continue
		}
		items := make([]HealthAgentRef, 0, len(g.group.Refs))
		for _, r := range g.group.Refs {
			items = append(items, HealthAgentRef{
				ID:          r.ID,
				Name:        r.Name,
				ProjectID:   r.ProjectID,
				ProjectSlug: slugs[r.ProjectID],
				BrokerID:    r.BrokerID,
			})
		}
		out.Problems = append(out.Problems, HealthAgentGroup{Kind: g.kind, Count: g.group.Count, Items: items})
	}
	return out
}

// healthSummaryProjectSlugs maps project IDs to slugs with one batched
// lookup. It returns an empty map when ids is empty or the lookup fails.
func (s *Server) healthSummaryProjectSlugs(ctx context.Context, ids []string) map[string]string {
	slugs := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return slugs
	}
	res, err := s.store.ListProjectSummaries(ctx, store.ProjectFilter{MemberProjectIDs: ids}, store.ListOptions{Limit: len(ids), SkipTotalCount: true})
	if err != nil {
		slog.Warn("health summary: failed to resolve project slugs", "error", err)
		return slugs
	}
	for _, p := range res.Items {
		slugs[p.ID] = p.Slug
	}
	return slugs
}

// orderedPhaseCounts returns the non-zero phase counts in lifecycle order
// (state.Phases()), followed by any phase outside that list sorted by name,
// so the order is stable across polls.
func orderedPhaseCounts(byPhase map[string]int) []HealthPhaseCount {
	out := make([]HealthPhaseCount, 0, len(byPhase))
	known := map[string]bool{}
	for _, p := range state.Phases() {
		known[string(p)] = true
		if n := byPhase[string(p)]; n > 0 {
			out = append(out, HealthPhaseCount{Phase: string(p), Count: n})
		}
	}
	var other []string
	for p, n := range byPhase {
		if !known[p] && n > 0 {
			other = append(other, p)
		}
	}
	sort.Strings(other)
	for _, p := range other {
		out = append(out, HealthPhaseCount{Phase: p, Count: byPhase[p]})
	}
	return out
}

// healthSummaryBrokers lists runtime brokers for the health summary,
// excluding plugin records. Problem rows (see healthSummaryBrokerHasProblem)
// are stably sorted ahead of the rest, so capping at
// healthSummaryBrokerLimit never hides a problem broker behind healthy ones;
// within each group store order is kept. Total is the full runtime broker
// count. On a store error it returns an empty, non-nil list with the error.
func (s *Server) healthSummaryBrokers(ctx context.Context, agentAgg *store.AgentHealthAggregate) (HealthSummaryBrokers, error) {
	list := HealthSummaryBrokers{Items: []HealthSummaryBroker{}}
	opts := store.ListOptions{Limit: healthSummaryBrokerPageSize}
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, opts)
		if err != nil {
			return HealthSummaryBrokers{Items: []HealthSummaryBroker{}}, err
		}
		for i := range page.Items {
			b := &page.Items[i]
			if isPluginBroker(b) {
				continue
			}
			list.Items = append(list.Items, healthSummaryBroker(b, agentAgg))
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}
	sort.SliceStable(list.Items, func(i, j int) bool {
		return healthSummaryBrokerHasProblem(list.Items[i]) && !healthSummaryBrokerHasProblem(list.Items[j])
	})
	list.Total = len(list.Items)
	if list.Total > healthSummaryBrokerLimit {
		list.Items = list.Items[:healthSummaryBrokerLimit]
		list.Truncated = true
	}
	return list, nil
}

// healthSummaryBrokerHasProblem reports whether a runtime broker row needs
// attention: it is not online, it reports itself degraded or unhealthy, or
// its NFS workspace share is unhealthy.
func healthSummaryBrokerHasProblem(b HealthSummaryBroker) bool {
	if healthSummaryBrokerStatusIsProblem(b.Status) || healthSummaryBrokerSelfIsProblem(b.Health) {
		return true
	}
	ws := b.WorkspaceStorage
	return ws != nil && ws.NFSHealthy != nil && !*ws.NFSHealthy
}

// healthSummaryBrokerStatusIsProblem reports whether a broker status needs
// attention. Anything but online counts, including an empty status: a new
// broker is stored as offline, so an empty status only comes from a
// heartbeat that did not state one, and an unknown status is not online.
// Both the problem-first ordering and the overall status use this check.
func healthSummaryBrokerStatusIsProblem(status string) bool {
	return status != store.BrokerStatusOnline
}

// healthSummaryBrokerSelfIsProblem reports whether a broker's self-reported
// health needs attention: degraded or unhealthy. A broker that never
// reported its health (nil) is not a problem; it is shown as not reported.
func healthSummaryBrokerSelfIsProblem(h *HealthBrokerSelf) bool {
	return h != nil && (h.Status == HealthStatusDegraded || h.Status == HealthStatusUnhealthy)
}

// healthSummaryBroker builds one runtime broker row of the health summary.
func healthSummaryBroker(b *store.RuntimeBroker, agentAgg *store.AgentHealthAggregate) HealthSummaryBroker {
	row := HealthSummaryBroker{
		ID:               b.ID,
		Name:             b.Name,
		Version:          b.Version,
		Status:           b.Status,
		Runtime:          brokerRuntimeSummary(b),
		WorkspaceStorage: brokerStorageSummary(b.WorkspaceStorage),
		Health:           brokerSelfHealthSummary(b.Health),
	}
	if !b.LastHeartbeat.IsZero() {
		hb := b.LastHeartbeat
		row.LastHeartbeat = &hb
	}
	if agentAgg != nil {
		if bucket, ok := agentAgg.ByBroker[b.ID]; ok {
			row.Agents = HealthBrokerAgents{Running: bucket.Running, Attention: bucket.Attention}
		}
	}
	return row
}

// brokerRuntimeSummary returns the runtime a broker places agents on by
// default: the profile named DefaultProfile; else the single profile when
// there is exactly one; else nil (nothing to report).
func brokerRuntimeSummary(b *store.RuntimeBroker) *HealthBrokerRuntime {
	if b.DefaultProfile != "" {
		for _, p := range b.Profiles {
			if p.Name == b.DefaultProfile {
				return &HealthBrokerRuntime{Type: p.Type, Profile: p.Name}
			}
		}
	}
	if len(b.Profiles) == 1 {
		p := b.Profiles[0]
		return &HealthBrokerRuntime{Type: p.Type, Profile: p.Name}
	}
	return nil
}

// brokerStorageSummary converts the workspace storage a broker reported on
// its last heartbeat. NFS health is reported only for the NFS backend; an
// NFS backend without a described share is reported as unhealthy.
func brokerStorageSummary(ws *api.BrokerWorkspaceStorage) *HealthBrokerStorage {
	if ws == nil || ws.Backend == "" {
		return nil
	}
	out := &HealthBrokerStorage{Backend: ws.Backend}
	if ws.Backend == api.WorkspaceStorageBackendNFS {
		healthy := ws.NFS != nil && ws.NFS.Healthy
		out.NFSHealthy = &healthy
	}
	return out
}

// brokerSelfHealthSummary converts the health a broker reported on its
// heartbeat, or returns nil when it never reported one. The stored report
// is normalised again (api.NormalizeBrokerHealthReport), so the response
// only ever holds fixed values, whatever the row contains. Checks is never
// null in the response.
func brokerSelfHealthSummary(stored *api.BrokerHealthReport) *HealthBrokerSelf {
	h := api.NormalizeBrokerHealthReport(stored)
	if h == nil {
		return nil
	}
	out := &HealthBrokerSelf{Status: h.Status, Checks: make(map[string]string, len(h.Checks))}
	for k, v := range h.Checks {
		out.Checks[k] = v
	}
	return out
}

// unhealthyChecks returns the non-healthy entries of a check map as sorted
// "key: value" strings, or nil when every check is healthy.
func unhealthyChecks(checks map[string]string) []string {
	var out []string
	for k, v := range checks {
		if v != HealthStatusHealthy {
			out = append(out, k+": "+v)
		}
	}
	sort.Strings(out)
	return out
}
