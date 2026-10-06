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
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

type HealthResponse struct {
	// Status must stay the first field: shell health checks
	// (scripts/starter-hub/gce-start-hub.sh, scripts/single-node-vm/deploy.sh)
	// read the top-level status by matching the body prefix {"status":"...".
	Status       string            `json:"status"`
	Version      string            `json:"version"`
	ScionVersion string            `json:"scionVersion"`
	HubID        string            `json:"hub_id,omitempty"`
	HubName      string            `json:"hub_name,omitempty"`
	Uptime       string            `json:"uptime"`
	Checks       map[string]string `json:"checks,omitempty"`
	Stats        *HealthStats      `json:"stats,omitempty"`
}

// Composite health status values reported by /healthz (HealthResponse.Status
// and CompositeHealthResponse.Status).
//
//   - healthy:   every check reports "healthy".
//   - degraded:  only non-critical checks are non-healthy. The process is up
//     and serving; something worth an operator's attention is wrong (e.g.
//     the co-located broker failed to register). "Process is up" consumers
//     (scion server start/status, gce-start-hub.sh) treat this as up and
//     name the non-healthy checks.
//   - unhealthy: a critical check (see criticalHealthChecks) failed. The hub
//     cannot serve requests meaningfully; consumers treat this as down.
//
// Uptime monitoring (deploy/monitoring/uptime-checks.yaml) still matches
// "healthy" exactly, so degraded continues to alert there.
const (
	HealthStatusHealthy   = "healthy"
	HealthStatusDegraded  = "degraded"
	HealthStatusUnhealthy = "unhealthy"
)

// criticalHealthChecks is the single definition of which check-map keys make
// the composite status "unhealthy" rather than "degraded" when they report
// anything but "healthy". Keep it small: a key belongs here only if, when it
// fails, the hub cannot serve requests at all.
//
//   - database: nothing works without the store.
//   - workspace_storage: the configured shared workspace mount is missing,
//     unmounted, or hung. handleReadyz already takes the pod out of service
//     for this, so /healthz agrees and reports unhealthy. Its companion
//     workspace_storage_mount_verification ("mount could not be verified")
//     is deliberately NOT critical: it only degrades.
//
// Check-map contract (for anyone adding a key in GetHealthInfo or a check*
// helper): the value is "healthy" or a non-healthy string, conventionally
// "unhealthy: <short fixed reason>" (the endpoint is unauthenticated, so no
// raw error text). A non-healthy value of a key not listed here makes the
// composite status "degraded", which consumers treat as "up, with a named
// problem" — so an informational key no longer reads as "down". Add the key
// here only if it should take the hub down for those consumers.
// handleReadyz (Kubernetes readiness) is deliberately independent of this
// set and consults its own checks.
var criticalHealthChecks = map[string]bool{
	"database":          true,
	"workspace_storage": true,
}

// deriveHealthStatus computes the composite status from a check map: unhealthy
// if any critical check is non-healthy, degraded if only non-critical checks
// are, healthy otherwise.
func deriveHealthStatus(checks map[string]string) string {
	status := HealthStatusHealthy
	for k, v := range checks {
		if v == HealthStatusHealthy {
			continue
		}
		if criticalHealthChecks[k] {
			return HealthStatusUnhealthy
		}
		status = HealthStatusDegraded
	}
	return status
}

// healthStatusRank orders composite statuses by severity. Unknown non-empty
// values rank as degraded: something reported a problem we cannot classify,
// but nothing said the component is down.
func healthStatusRank(status string) int {
	switch status {
	case HealthStatusHealthy:
		return 0
	case HealthStatusUnhealthy:
		return 2
	default:
		return 1
	}
}

// worseHealthStatus returns the more severe of two composite statuses,
// normalizing unknown non-healthy values to degraded.
func worseHealthStatus(a, b string) string {
	worst := a
	if healthStatusRank(b) > healthStatusRank(a) {
		worst = b
	}
	switch healthStatusRank(worst) {
	case 0:
		return HealthStatusHealthy
	case 2:
		return HealthStatusUnhealthy
	default:
		return HealthStatusDegraded
	}
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

	// Check NFS workspace storage when configured
	s.checkWorkspaceStorageHealth(checks)

	// Check co-located broker registration when this Hub expects one
	s.checkColocatedBrokerHealth(checks)

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

	return &HealthResponse{
		Status:       deriveHealthStatus(checks),
		Version:      "0.1.0", // TODO: Get from build info
		ScionVersion: version.Short(),
		HubID:        s.HubID(),
		HubName:      s.HubName(),
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

// checkWorkspaceStorageHealth verifies that the configured workspace storage
// backend is accessible. For NFS, Cloud Run volume and GKE shared volume
// backends, it stats the mount point to confirm it is present. For local
// storage, no check is needed.
func (s *Server) checkWorkspaceStorageHealth(checks map[string]string) {
	wsCfg := s.config.WorkspaceStorageConfig
	if wsCfg == nil || wsCfg.Backend == "" || wsCfg.Backend == "local" {
		return // Local storage — no health check needed
	}

	mountPath := workspaceMountRoot(wsCfg)
	if mountPath == "" {
		checks["workspace_storage"] = "unhealthy: mount path not configured"
		return
	}

	// For the GKE shared volume, presence of the directory is not enough. The
	// pod spec has to mount the PVC at the path derived from the volume name,
	// and nothing enforces that it did; when it did not, the hub creates that
	// directory itself on the container overlay the first time a project is
	// written. Requiring the path to be a mounted volume keeps a
	// wrongly-mounted deployment permanently unready instead of letting it
	// latch healthy over ephemeral storage. See isMountedVolume.
	requireMount := wsCfg.Backend == "gke-shared-volume"

	// Wrap os.Stat in a goroutine with a timeout to prevent blocking on a
	// hung NFS mount. A stuck stat call would otherwise hang the health
	// endpoint indefinitely, taking down readiness probes.
	type statResult struct {
		err          error
		mounted      bool
		determinable bool
	}
	ch := make(chan statResult, 1)
	go func() {
		fi, err := os.Stat(mountPath)
		if err != nil {
			ch <- statResult{err: err}
			return
		}
		mounted, determinable := true, true
		if requireMount {
			mounted, determinable = isMountedVolume(fi, containerRootPath)
		}
		ch <- statResult{mounted: mounted, determinable: determinable}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			checks["workspace_storage"] = "unhealthy: mount not available"
			return
		}
		if !res.mounted {
			checks["workspace_storage"] = "unhealthy: mount path is not a mounted volume"
			return
		}
		if !res.determinable {
			// The mount could not be verified, so the storage check passed by
			// default and the silent-ephemeral-storage failure is possible
			// again. Readiness stays green deliberately — an unenforceable
			// check must not take a pod out of service — but the operator gets
			// a distinct signal instead of an indistinguishable "healthy". A
			// separate key rather than a qualified workspace_storage value,
			// because handleReadyz compares that value to "healthy" exactly
			// and would 503 the pod on any suffix, and, since
			// workspace_storage is critical, would also make /healthz
			// unhealthy.
			//
			// A non-critical key, so this makes /healthz report "degraded"
			// (up, with a named problem), never "unhealthy"; see
			// criticalHealthChecks. Previously any non-healthy key read as
			// "down" to scion server start/status and gce-start-hub.sh
			// (ptone/scion#1094).
			checks["workspace_storage_mount_verification"] = "unavailable: could not compare filesystem device IDs"
		}
		checks["workspace_storage"] = "healthy"
	case <-time.After(2 * time.Second):
		checks["workspace_storage"] = "unhealthy: mount check timed out"
	}
}

// checkColocatedBrokerHealth reports on the co-located (embedded) runtime
// broker that this Hub process expects, when it expects one. A hub that
// never calls ExpectEmbeddedBroker (a purely distributed deployment with no
// local broker) has nothing to check here, so it adds no key — same
// no-op-when-not-configured shape as checkWorkspaceStorageHealth.
//
// This is what makes ptone/scion#2154 visible: previously, a co-located
// broker that failed to register (e.g. a non-UUID broker_id rejected by the
// store) left the Hub reporting "healthy" with zero brokers, and the first
// symptom an operator saw was agent creation failing with 422 (no runtime
// broker available). Surfacing the failure here means /healthz (and the
// admin health summary, which derives its overall status from GetHealthInfo)
// goes "degraded" immediately instead. There is no retry: a registration
// failure is terminal for the process lifetime (the broker never gets a
// second attempt), so this stays degraded until the broker configuration is
// fixed and the process is restarted — not until "resolved" in place.
//
// colocated_broker is a non-critical key (see criticalHealthChecks), so a
// failed or still-pending registration makes /healthz "degraded", not
// "unhealthy": the process is up and serving, but agent dispatch is broken.
// Consumers treat degraded as up and name this check — `scion server status`
// and `scion server start` (cmd/server_daemon.go) print it,
// gce-start-hub.sh warns, the diagnostics banner shows an amber "Degraded",
// and the admin health summary lists it under hub.checks. The
// registration-pending window is bounded (registration runs immediately
// after the listener starts), so `scion server start` keeps polling for
// "healthy" until its deadline before settling for degraded.
func (s *Server) checkColocatedBrokerHealth(checks map[string]string) {
	state := s.embeddedBrokerSnapshot()
	switch {
	case state.id != "":
		checks["colocated_broker"] = "healthy"
	case state.regErr != "":
		// Fixed string: /healthz and /health are unauthenticated (see
		// isPublicRoute in web.go), and state.regErr is the verbatim error
		// chain from registerGlobalProjectAndBroker, which can carry store/
		// driver detail (e.g. Postgres table and constraint names). The full
		// error is already logged at ERROR with broker_id by
		// startRuntimeBroker; an operator diagnoses from there, not from the
		// public health response. The authenticated admin health summary
		// could carry more detail in the future if that turns out to be
		// needed, but does not today.
		checks["colocated_broker"] = "unhealthy: registration failed"
	case state.pending:
		checks["colocated_broker"] = "unhealthy: registration pending"
	default:
		// No co-located broker expected for this Hub; nothing to check.
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	resp := s.GetHealthInfo(r.Context())
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
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

	// Check workspace storage health for readiness
	wsCfg := s.config.WorkspaceStorageConfig
	if wsCfg != nil && wsCfg.Backend != "" && wsCfg.Backend != "local" {
		checks := make(map[string]string)
		s.checkWorkspaceStorageHealth(checks)
		if status, ok := checks["workspace_storage"]; ok && status != "healthy" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "not_ready",
				"reason": "workspace storage not available",
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ready",
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	// Build a combined metrics response
	type combinedMetrics struct {
		Broker         *MetricsSnapshot               `json:"broker,omitempty"`
		GCP            *GCPTokenMetricsSnapshot       `json:"gcp,omitempty"`
		ExternalBearer *ExternalBearerMetricsSnapshot `json:"externalBearer,omitempty"`
	}

	var combined combinedMetrics

	if s.metrics != nil {
		combined.Broker = s.metrics.GetSnapshot()
	}
	if s.gcpTokenMetrics != nil {
		combined.GCP = s.gcpTokenMetrics.GetSnapshot()
	}
	// Like gcpTokenMetrics above (and unlike Broker, which only exists when
	// broker auth is enabled), externalBearerSnapshot is always constructed
	// by New(): the exchange-deletion soak gate must not
	// depend on GCP export being configured, so this section is always
	// present on a Server built through New().
	if s.externalBearerSnapshot != nil {
		combined.ExternalBearer = s.externalBearerSnapshot.GetSnapshot()
	}

	if combined.Broker == nil && combined.GCP == nil && combined.ExternalBearer == nil {
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "no_metrics",
			"reason": "metrics not configured",
		})
		return
	}

	writeJSON(w, http.StatusOK, combined)
}
