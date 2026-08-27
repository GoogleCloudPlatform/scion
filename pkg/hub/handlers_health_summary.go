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
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// HealthSummaryResponse is the composite health summary returned by
// GET /api/v1/admin/health/summary. It aggregates all subsystem health
// into a single response for the health dashboard.
type HealthSummaryResponse struct {
	Status   string                 `json:"status"`
	Hub      HealthSummaryHub       `json:"hub"`
	Database HealthSummaryDB        `json:"database"`
	Brokers  []HealthSummaryBrkr    `json:"brokers"`
	Agents   HealthSummaryAgents    `json:"agents"`
	Dispatch *HealthSummaryDispatch `json:"dispatch"` // nil when dispatch metrics are unavailable
	Stall    HealthSummaryStall     `json:"stall_config"`
}

// HealthSummaryHub contains hub-level health information.
type HealthSummaryHub struct {
	Status           string `json:"status"`
	Version          string `json:"version"`
	Uptime           string `json:"uptime"`
	ConnectedBrokers int    `json:"connected_brokers"`
	ActiveAgents     int    `json:"active_agents"`
	Projects         int    `json:"projects"`
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

// HealthSummaryBrkr contains per-broker health information.
type HealthSummaryBrkr struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Status           string    `json:"status"`
	Runtime          string    `json:"runtime"`
	RuntimeAvailable bool      `json:"runtime_available"`
	AgentCount       int       `json:"agent_count"`
	AgentHealthy     int       `json:"agent_healthy"`
	LastHeartbeat    time.Time `json:"last_heartbeat"`
	// NFS health fields are not yet populated — the broker heartbeat protocol
	// does not currently report NFS mount status. Tracked as a follow-up to
	// the health monitoring design (§4.1 D5). TODO: wire NFS health once the
	// broker API exposes it.
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

// HealthSummaryStall contains stall detection configuration.
type HealthSummaryStall struct {
	ThresholdSeconds int  `json:"threshold_seconds"`
	AutoSuspend      bool `json:"auto_suspend"`
}

// handleHealthSummary handles GET /api/v1/admin/health/summary.
// Returns a composite health summary aggregating all subsystems.
// Authorization: enforced by routeGuard via hub.health.read permission.
func (s *Server) handleHealthSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	// Get base health info
	healthInfo := s.GetHealthInfo(ctx)

	// Determine overall status early so DB-error branches can degrade it.
	overallStatus := "healthy"
	if healthInfo.Status != "" && healthInfo.Status != "healthy" {
		overallStatus = healthInfo.Status
	}

	// Build hub section
	hubSummary := HealthSummaryHub{
		Status:  healthInfo.Status,
		Version: healthInfo.ScionVersion,
		Uptime:  healthInfo.Uptime,
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
		overallStatus = "degraded"
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
	var brokerSummaries []HealthSummaryBrkr
	brokerResult, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{Limit: 100})
	if err != nil {
		slog.Error("health summary: failed to list runtime brokers", "error", err)
		overallStatus = "degraded"
	} else {
		for _, b := range brokerResult.Items {
			agentCount := 0
			agentHealthy := 0
			if agentAgg != nil {
				if bucket, ok := agentAgg.ByBroker[b.ID]; ok {
					agentCount = bucket.Count
					agentHealthy = bucket.Healthy
				}
			}

			// Determine runtime type from profiles
			runtime := "unknown"
			runtimeAvailable := false
			if len(b.Profiles) > 0 {
				runtime = b.Profiles[0].Type
				runtimeAvailable = b.Profiles[0].Available
			}

			brokerSummaries = append(brokerSummaries, HealthSummaryBrkr{
				ID:               b.ID,
				Name:             b.Name,
				Status:           b.Status,
				Runtime:          runtime,
				RuntimeAvailable: runtimeAvailable,
				AgentCount:       agentCount,
				AgentHealthy:     agentHealthy,
				LastHeartbeat:    b.LastHeartbeat,
			})
		}
	}
	if brokerSummaries == nil {
		brokerSummaries = []HealthSummaryBrkr{}
	}

	// Dispatch section: the dispatchmetrics.Recorder does not currently expose a
	// Stats() method for reading in-process counters. Rather than returning
	// hardcoded zeros (which would mislead operators), we omit the dispatch data
	// and let the dashboard render "data not available". TODO: add a Stats()
	// method to dispatchmetrics.Recorder to populate this section.
	var dispatchSummary *HealthSummaryDispatch // nil = unavailable

	// Build stall config section
	stallConfig := HealthSummaryStall{
		ThresholdSeconds: int(s.config.StalledThreshold.Seconds()),
		AutoSuspend:      s.config.AutoSuspendStalled,
	}

	// Propagate unhealthy agent/broker signals into overall status.
	if len(agentsSummary.Stalled) > 0 || len(agentsSummary.Crashed) > 0 || len(agentsSummary.Errored) > 0 {
		overallStatus = "degraded"
	}
	for _, b := range brokerSummaries {
		if b.Status != "online" && b.Status != "" {
			overallStatus = "degraded"
			break
		}
	}

	resp := HealthSummaryResponse{
		Status:   overallStatus,
		Hub:      hubSummary,
		Database: dbSummary,
		Brokers:  brokerSummaries,
		Agents:   agentsSummary,
		Dispatch: dispatchSummary,
		Stall:    stallConfig,
	}

	writeJSON(w, http.StatusOK, resp)
}
