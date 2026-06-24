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
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

type HealthResponse struct {
	Status       string            `json:"status"`
	Version      string            `json:"version"`
	ScionVersion string            `json:"scionVersion"`
	Uptime       string            `json:"uptime"`
	Checks       map[string]string `json:"checks,omitempty"`
	Stats        *HealthStats      `json:"stats,omitempty"`
}

type HealthStats struct {
	ConnectedBrokers int `json:"connectedBrokers,omitempty"`
	ActiveAgents     int `json:"activeAgents,omitempty"`
	Projects         int `json:"projects,omitempty"`
}

// GetHealthInfo returns the current health status of the Hub server.
// This can be called directly by co-located components (e.g., the WebServer)
// to build composite health responses without making an HTTP round-trip.
func (s *Server) GetHealthInfo(ctx context.Context) *HealthResponse {
	checks := make(map[string]string)

	// Check database
	if err := s.store.Ping(ctx); err != nil {
		checks["database"] = "unhealthy"
	} else {
		checks["database"] = "healthy"
	}

	// Get stats
	stats := &HealthStats{}
	if agentResult, err := s.store.ListAgents(ctx, store.AgentFilter{Phase: string(state.PhaseRunning)}, store.ListOptions{Limit: 1}); err == nil {
		stats.ActiveAgents = agentResult.TotalCount
	}
	if projectResult, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1}); err == nil {
		stats.Projects = projectResult.TotalCount
	}
	if brokerResult, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{Status: store.BrokerStatusOnline}, store.ListOptions{Limit: 1}); err == nil {
		stats.ConnectedBrokers = brokerResult.TotalCount
	}

	status := "healthy"
	for _, v := range checks {
		if v != "healthy" {
			status = "degraded"
			break
		}
	}

	return &HealthResponse{
		Status:       status,
		Version:      "0.1.0", // TODO: Get from build info
		ScionVersion: version.Short(),
		Uptime:       time.Since(s.startTime).Round(time.Second).String(),
		Checks:       checks,
		Stats:        stats,
	}
}

// HealthStatus returns the status string from the health response.
// This enables interface-based status checking from the web handler.
func (h *HealthResponse) HealthStatus() string {
	return h.Status
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	resp := s.GetHealthInfo(r.Context())
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Check if database is connected and migrated
	if err := s.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "database not available",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ready",
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Build a combined metrics response
	type combinedMetrics struct {
		Broker *MetricsSnapshot         `json:"broker,omitempty"`
		GCP    *GCPTokenMetricsSnapshot `json:"gcp,omitempty"`
	}

	var combined combinedMetrics

	if s.metrics != nil {
		combined.Broker = s.metrics.GetSnapshot()
	}
	if s.gcpTokenMetrics != nil {
		combined.GCP = s.gcpTokenMetrics.GetSnapshot()
	}

	if combined.Broker == nil && combined.GCP == nil {
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "no_metrics",
			"reason": "metrics not configured",
		})
		return
	}

	writeJSON(w, http.StatusOK, combined)
}
