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
	Status       string            `json:"status"`
	Version      string            `json:"version"`
	ScionVersion string            `json:"scionVersion"`
	HubID        string            `json:"hub_id,omitempty"`
	HubName      string            `json:"hub_name,omitempty"`
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

	// Check NFS workspace storage when configured
	s.checkWorkspaceStorageHealth(checks)

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
			// and would 503 the pod on any suffix.
			//
			// This is not free, and the cost is not local. GetHealthInfo marks
			// the whole response "degraded" on any non-healthy check value, and
			// six comparators downstream test for "healthy" exactly. Four
			// consequences, most reachable first; this key is set only under a
			// gke-shared-volume config, so exposure depends on the deployment:
			//   - the diagnostics UI styles it unhealthy and labels it
			//     "degraded": renderStatusBanner in diagnostics.ts has no
			//     degraded class, so degraded falls through its statusClass
			//     ternary to the red unhealthy style, and through its
			//     statusLabel ternary to printing the raw status. Not "anything
			//     but healthy is red" — unknown has its own neutral class, and
			//     it is what the banner shows before the health fetch resolves.
			//     This is the one that actually happens on a GKE hub with this
			//     backend;
			//   - on a workstation configured with this backend, and only
			//     there: waitForServerReady (cmd/server_daemon.go) never
			//     returns true, so `scion server start` stalls for its full 20s
			//     wait and then skips the browser open with "server not yet
			//     ready" — in an interactive non-headless terminal with web
			//     enabled — and `scion server status` leaves WebRunning and
			//     HubRunning both false, reporting the web frontend and the hub
			//     API as not detected. Both, and the hub half is the one worth
			//     spelling out: a workstation enables web by default
			//     (cmd/server_config.go), so the hub is mounted on the web port
			//     rather than binding :9810 (cmd/server_foreground.go), and the
			//     status command's :9810 fallback — which would otherwise leave
			//     HubRunning true, since it parses the body without comparing
			//     the status — has nothing to connect to here;
			//   - scripts/starter-hub/gce-start-hub.sh greps for
			//     '"status":"healthy"' and exits 1 on both its health checks.
			//     The settings.yaml that script writes declares no
			//     workspace_storage, and the script health-checks only the hub
			//     it just deployed, so this clause bites only where an operator
			//     supplies that config out of band — via the hub.env
			//     EnvironmentFile, say, whose SCION_ overrides were not traced.
			// The other two only forward degraded: WebServer.handleHealthz into
			// the web composite at /healthz, handleHealthSummary into the health
			// dashboard, which does have a degraded class. Counted, not damage.
			//
			// That coupling is pre-existing and tracked in ptone/scion#1094.
			// Tolerated here because this branch is unreachable on the
			// platforms we ship — os.Stat always yields a *syscall.Stat_t on
			// linux and darwin, and a container whose root cannot be stat'ed
			// has larger problems. Note that hedge is a PLATFORM one: if it
			// stops holding, the consequences above go live on their own
			// reachability, not on this one. If it ever does become reachable,
			// prefer logging over a check-map entry.
			checks["workspace_storage_mount_verification"] = "unavailable: could not compare filesystem device IDs"
		}
		checks["workspace_storage"] = "healthy"
	case <-time.After(2 * time.Second):
		checks["workspace_storage"] = "unhealthy: mount check timed out"
	}
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
