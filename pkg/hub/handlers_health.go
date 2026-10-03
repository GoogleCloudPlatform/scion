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
// Consequences of returning a non-"healthy" key here: the exact-"healthy"
// comparators downstream that make this consequential are the same coupling
// checkWorkspaceStorageHealth's workspace_storage_mount_verification note
// documents (that coupling is pre-existing and tracked in
// ptone/scion#1094). This is reachable on the default single-node
// workstation setup (hub + co-located broker, colocatedBrokerRegisters), not
// just an edge case, so a failed or still-pending registration visibly
// degrades:
//   - the web diagnostics page (renderStatusBanner in
//     web/src/components/pages/diagnostics.ts) fetches /healthz and, having
//     no degraded class, renders a composite "degraded" status in the red
//     unhealthy style with the raw label — intended here, since agent
//     dispatch is in fact broken.
//   - `scion server status` (cmd/server_daemon.go) used to report the Hub
//     API/Web Frontend as "not detected" even though the process is up and
//     serving. It now names the colocated_broker reason instead, on both the
//     standalone Hub port (checks at the top level of the response) and the
//     combined workstation setup, where the web server answers /healthz and
//     nests the Hub's checks under "hub" (WebServer.handleHealthz in web.go,
//     CompositeHealthResponse) — cmd/server_daemon.go's colocatedBrokerReason
//     looks in both places.
//   - `scion server start` names the same reason, but only through
//     printWorkstationQuickstart's wait-for-ready path, which runs solely
//     when it is about to open a browser: web enabled, an interactive
//     non-headless terminal, and SCION_NO_BROWSER unset. Any other `start`
//     invocation prints nothing about this at all (see the workstation
//     bullet in checkWorkspaceStorageHealth's
//     workspace_storage_mount_verification note above, on why this path
//     polls the web port specifically).
//   - scripts/starter-hub/gce-start-hub.sh greps for
//     `"status":"healthy"` and exits 1.
//   - web/e2e/harness/hub.ts throws waiting for a healthy response.
//     These last two are not patched to name the check; they still see a
//     hard "not healthy" stop.
//
// The registration-pending window (as opposed to a hard failure) is bounded
// — registration runs immediately after the listener starts — so callers
// polling during that brief window still see healthy shortly after. A
// terminal registration failure does not self-heal, so the consumers above
// degrade for the rest of the process's life, which is judged the right
// tradeoff here: the Hub genuinely cannot dispatch agents without its
// only broker, so a check-map entry — rather than logging-only, which the
// workspace_storage_mount_verification note's closing guidance would
// otherwise prefer for an unreachable branch like that one — is deliberate
// here, not an oversight.
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
