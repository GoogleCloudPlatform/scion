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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// monitoringDashboardURL returns the configured monitoring dashboard link
// (server.hub.monitoring_dashboard_url), or "" when it is unset. It reads
// the value ApplySnapshot last applied, so a change saved through the
// admin API, or propagated from another replica, takes effect without a
// restart.
func (s *Server) monitoringDashboardURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.MonitoringDashboardURL
}

// validateMonitoringDashboardURLRequest checks a server-config PUT's
// server.hub.monitoring_dashboard_url with config.ValidateMonitoringDashboardURL
// and writes a 422 validation_error naming the key when it fails. An
// omitted key or an explicit "" (clear) passes. It reports whether the
// request may continue.
func validateMonitoringDashboardURLRequest(w http.ResponseWriter, req *ServerConfigUpdateRequest) bool {
	if req == nil || req.Server == nil || req.Server.Hub == nil {
		return true
	}
	if err := config.ValidateMonitoringDashboardURL(req.Server.Hub.MonitoringDashboardURL); err != nil {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, err.Error(), map[string]interface{}{
			"field": config.MonitoringDashboardURLKey,
		})
		return false
	}
	return true
}

// healthSummaryLinks returns the Health page links for the health summary,
// or nil when none is configured, so the field is omitted.
func (s *Server) healthSummaryLinks() *HealthSummaryLinks {
	u := s.monitoringDashboardURL()
	if u == "" {
		return nil
	}
	return &HealthSummaryLinks{MonitoringDashboard: u}
}
