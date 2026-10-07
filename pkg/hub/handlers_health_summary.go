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
	Agents   HealthSummaryAgents    `json:"agents"`
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
	Agents           HealthBrokerAgents   `json:"agents"`
}

// HealthBrokerAgents holds per-broker agent counts.
type HealthBrokerAgents struct {
	// Total is the number of non-deleted agents placed on the broker, in
	// any phase: it includes stopped, suspended and errored agents, not
	// only running ones.
	Total int `json:"total"`
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

// HealthSummaryAgents contains agent health summary.
type HealthSummaryAgents struct {
	Total   int            `json:"total"`
	ByPhase map[string]int `json:"by_phase"`
	Stalled []string       `json:"stalled"`
	Crashed []string       `json:"crashed"`
	Errored []string       `json:"errored"`
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
	agentsSummary := HealthSummaryAgents{
		ByPhase: make(map[string]int),
		Stalled: []string{},
		Crashed: []string{},
		Errored: []string{},
	}

	agentAgg, err := s.store.AggregateAgentHealth(ctx)
	if err != nil {
		slog.Error("health summary: failed to aggregate agent health", "error", err)
		degrade()
	} else {
		agentsSummary.Total = agentAgg.Total
		agentsSummary.ByPhase = agentAgg.ByPhase
		if len(agentAgg.StalledNames) > 0 {
			agentsSummary.Stalled = agentAgg.StalledNames
		}
		if len(agentAgg.CrashedNames) > 0 {
			agentsSummary.Crashed = agentAgg.CrashedNames
		}
		if len(agentAgg.ErroredNames) > 0 {
			agentsSummary.Errored = agentAgg.ErroredNames
		}
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
	if len(agentsSummary.Stalled) > 0 || len(agentsSummary.Crashed) > 0 || len(agentsSummary.Errored) > 0 {
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
// attention: it is not online, or its NFS workspace share is unhealthy.
func healthSummaryBrokerHasProblem(b HealthSummaryBroker) bool {
	if healthSummaryBrokerStatusIsProblem(b.Status) {
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

// healthSummaryBroker builds one runtime broker row of the health summary.
func healthSummaryBroker(b *store.RuntimeBroker, agentAgg *store.AgentHealthAggregate) HealthSummaryBroker {
	row := HealthSummaryBroker{
		ID:               b.ID,
		Name:             b.Name,
		Version:          b.Version,
		Status:           b.Status,
		Runtime:          brokerRuntimeSummary(b),
		WorkspaceStorage: brokerStorageSummary(b.WorkspaceStorage),
	}
	if !b.LastHeartbeat.IsZero() {
		hb := b.LastHeartbeat
		row.LastHeartbeat = &hb
	}
	if agentAgg != nil {
		if bucket, ok := agentAgg.ByBroker[b.ID]; ok {
			row.Agents.Total = bucket.Count
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
