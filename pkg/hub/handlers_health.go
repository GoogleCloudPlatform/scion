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
	"errors"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
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
	checks := s.healthChecks(ctx)
	return &HealthResponse{
		Status:       deriveHealthStatus(checks),
		Version:      "0.1.0", // TODO: Get from build info
		ScionVersion: version.Short(),
		HubID:        s.HubID(),
		HubName:      s.HubName(),
		Uptime:       time.Since(s.startTime).Round(time.Second).String(),
		Checks:       checks,
		Stats:        s.healthStats(ctx),
	}
}

// healthChecks runs this process's health checks and returns the check map
// (see the check-map contract on criticalHealthChecks): one store Ping,
// workspace storage, the co-located broker, the audit log writer and, when
// configured, the Cloud Logging path.
// It runs no count queries, so the hub-instance registry tick can call it
// every tick (hub_instance_registry.go).
func (s *Server) healthChecks(ctx context.Context) map[string]string {
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

	// Project configuration identity on shared backends (non-critical).
	s.checkProjectConfigIdentityHealth(checks)

	// Audit log writer (non-critical: degraded, never unhealthy).
	s.checkAuditWriterHealth(checks)

	// Direct Cloud Logging path, when configured (non-critical).
	s.checkCloudLoggingHealth(checks)

	// Legacy storage blocks still in use (non-critical).
	s.checkProjectStorageConfig(checks)

	// The project tree on a shared backend's mount (non-critical).
	s.checkProjectStorageTree(checks)

	// Removed per-runtime/per-profile storage overrides (non-critical).
	s.checkRemovedStorageOverrides(checks)

	return checks
}

// projectStorageConfigCheck is the health check key of a hub whose storage
// comes from the deprecated blocks. It is not in criticalHealthChecks, so
// it makes the hub degraded (the diagnostics banner shows it), never
// unhealthy.
const projectStorageConfigCheck = "project_storage_config"

// checkProjectStorageConfig reports a hub whose server.project_storage was
// synthesized from the deprecated workspace_storage / shared_dir_storage /
// home_storage blocks, until the operator writes the block (design M1;
// the startup log prints the exact block to write).
func (s *Server) checkProjectStorageConfig(checks map[string]string) {
	if len(s.config.ProjectStorageWarnings) == 0 {
		return
	}
	checks[projectStorageConfigCheck] = "degraded: legacy storage blocks in use; write server.project_storage (the hub startup log prints the block)"
}

// projectStorageTreeCheck is the health check key of a shared backend other
// than nfs (cloudrun-volume, gke-shared-volume) whose mount point has no
// project tree: the share is not mounted (only the mount point is), so the
// hub, the one deleter of project trees on the share, cannot remove them.
// Not critical: degraded, never unhealthy. On nfs the same fact is part of
// the critical workspace_storage check instead (nfsProjectTreeStatus).
const projectStorageTreeCheck = "project_storage_tree"

// projectStorageTreeProbe runs the tree check of the delete path's
// storageKept probe (project_delete_storage_kept.go); a variable so tests
// can make it hang.
var projectStorageTreeProbe = func(l projectfs.Layout) error { return l.CheckTreesMounted() }

// projectTreeHealthProbe runs the health checks' tree check for one shared
// Layout; a variable so tests can make it hang or count its calls.
var projectTreeHealthProbe = func(l projectfs.Layout) error { return l.CheckTreeMounted() }

// projectTreeProbesInFlight maps a Layout key to its in-flight tree probe
// (*inFlightCall[error]), as workspaceHealthProbesInFlight does for the
// mount stat: at most one stuck probe per mount, and later callers wait on
// it with their own deadline.
var projectTreeProbesInFlight sync.Map

// startProjectTreeProbe returns the in-flight tree probe for l, starting one
// if none is running.
func startProjectTreeProbe(l projectfs.Layout) *inFlightCall[error] {
	return joinOrStartInFlight(&projectTreeProbesInFlight, l.Key(), func() (func() error, func()) {
		probe := projectTreeHealthProbe
		return func() error { return probe(l) }, nil
	})
}

// The fixed, path-free workspace_storage values of the nfs project tree check
// (/healthz is unauthenticated: they name no host path and no configured value).
const (
	workspaceStorageShareEmpty   = "unhealthy: shared project storage is empty or not mounted"
	workspaceStorageNoTree       = "unhealthy: shared project storage has no project tree"
	workspaceStorageTreeTimedOut = "unhealthy: shared project storage check timed out"
	workspaceStorageTreeError    = "unhealthy: shared project storage could not be checked"
)

// projectTreeOutcome is one tree check's result for the health checks.
type projectTreeOutcome int

const (
	projectTreeOK projectTreeOutcome = iota
	projectTreeShareEmpty
	projectTreeMissing
	projectTreeTimedOut
	projectTreeError
)

// probeProjectTree runs (or joins) the tree probe of l and waits for it until
// deadline. ErrNoProjectTree on a hub with no projects is OK: the tree
// appears with the first project. The store read gets the time that remains;
// a store error or timeout counts as having projects (hubHasProjects: fail
// closed).
func (s *Server) probeProjectTree(l projectfs.Layout, deadline time.Time) projectTreeOutcome {
	call := startProjectTreeProbe(l)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-call.done:
	case <-timer.C:
		return projectTreeTimedOut
	}
	err := call.res
	if err == nil {
		return projectTreeOK
	}
	if !errors.Is(err, projectfs.ErrNoProjectTree) {
		return projectTreeError
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if !s.hubHasProjects(ctx) {
		return projectTreeOK
	}
	if errors.Is(err, projectfs.ErrShareEmpty) {
		return projectTreeShareEmpty
	}
	return projectTreeMissing
}

// nfsLayouts returns the distinct nfs Layouts of the hub's project storage
// (the layout and its transitional shared-dir split), or none.
func (s *Server) nfsLayouts() []projectfs.Layout {
	layout, err := s.projectStorageLayout()
	if err != nil {
		return nil
	}
	var out []projectfs.Layout
	seen := map[string]bool{}
	for _, l := range []projectfs.Layout{layout, layout.SharedDirLayout()} {
		l.SharedDirs = nil
		if l.Backend != projectfs.BackendNFS || seen[l.Key()] {
			continue
		}
		seen[l.Key()] = true
		out = append(out, l)
	}
	return out
}

// nfsProjectTreeStatus returns "" when every nfs Layout of the hub has its
// project tree (or the hub has no projects yet), else the fixed unhealthy
// workspace_storage value. It is part of the critical workspace_storage
// check, so /readyz answers 503 and /healthz reports unhealthy.
func (s *Server) nfsProjectTreeStatus(deadline time.Time) string {
	for _, l := range s.nfsLayouts() {
		switch s.probeProjectTree(l, deadline) {
		case projectTreeOK:
			continue
		case projectTreeShareEmpty:
			return workspaceStorageShareEmpty
		case projectTreeMissing:
			return workspaceStorageNoTree
		case projectTreeTimedOut:
			return workspaceStorageTreeTimedOut
		default:
			return workspaceStorageTreeError
		}
	}
	return ""
}

// checkProjectStorageTree reports, on a shared backend other than nfs, a
// mount point without the project tree below it, before any delete needs
// it (non-critical: degraded). nfs Layouts are skipped: their tree is part
// of workspace_storage (nfsProjectTreeStatus). The probe is deduplicated
// per Layout and bounded by workspaceHealthTimeout, so a hung mount cannot
// block the health endpoint.
func (s *Server) checkProjectStorageTree(checks map[string]string) {
	layout, err := s.projectStorageLayout()
	if err != nil {
		return
	}
	var others []projectfs.Layout
	seen := map[string]bool{}
	for _, l := range []projectfs.Layout{layout, layout.SharedDirLayout()} {
		l.SharedDirs = nil
		if !l.Backend.Shared() || l.Backend == projectfs.BackendNFS || seen[l.Key()] {
			continue
		}
		seen[l.Key()] = true
		others = append(others, l)
	}
	if len(others) == 0 {
		return
	}
	deadline := time.Now().Add(workspaceHealthTimeout)
	for _, l := range others {
		switch s.probeProjectTree(l, deadline) {
		case projectTreeOK:
			continue
		case projectTreeTimedOut:
			checks[projectStorageTreeCheck] = "degraded: the project tree check timed out"
			return
		default:
			checks[projectStorageTreeCheck] = "degraded: the shared project storage has no project tree; the share may not be mounted"
			return
		}
	}
	checks[projectStorageTreeCheck] = HealthStatusHealthy
}

// healthStats counts running agents, projects and online runtime brokers
// for GetHealthInfo. A failed count is left at zero.
func (s *Server) healthStats(ctx context.Context) *HealthStats {
	stats := &HealthStats{}
	if agentResult, err := s.store.ListAgents(ctx, store.AgentFilter{Phase: string(state.PhaseRunning)}, store.ListOptions{Limit: 1}); err == nil {
		stats.ActiveAgents = agentResult.TotalCount
	}
	if projectResult, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1}); err == nil {
		stats.Projects = projectResult.TotalCount
	}
	if count, err := s.countOnlineRuntimeBrokers(ctx); err == nil {
		stats.ConnectedBrokers = count
	}
	return stats
}

// connectedBrokerPageSize is the page size countOnlineRuntimeBrokers uses
// when listing brokers. It is a variable so tests can force multiple pages.
var connectedBrokerPageSize = 200

// countOnlineRuntimeBrokers counts online runtime brokers. Message broker
// plugin records (Discord, Telegram, ...) carry the plugin label (see
// isPluginBroker) and are always marked online, so they are not counted.
func (s *Server) countOnlineRuntimeBrokers(ctx context.Context) (int, error) {
	filter := store.RuntimeBrokerFilter{Status: store.BrokerStatusOnline}
	opts := store.ListOptions{Limit: connectedBrokerPageSize}
	count := 0
	for {
		result, err := s.store.ListRuntimeBrokers(ctx, filter, opts)
		if err != nil {
			return 0, err
		}
		for i := range result.Items {
			if !isPluginBroker(&result.Items[i]) {
				count++
			}
		}
		if result.NextCursor == "" || result.NextCursor == opts.Cursor {
			return count, nil
		}
		opts.Cursor = result.NextCursor
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
	wsCfg := s.config.ProjectStorageConfig.WorkspaceStorage()
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

	// Stat the mount in a goroutine with a timeout so a hung NFS mount cannot
	// block the health endpoint (and with it the readiness probes). Probes of
	// the same mount share one in-flight stat; see workspaceHealthProbesInFlight.
	call := startWorkspaceHealthProbe(mountPath, requireMount)
	// One deadline for the whole check: the mount stat, and on nfs the
	// project tree probe and its store read, share workspaceHealthTimeout.
	deadline := time.Now().Add(workspaceHealthTimeout)
	timer := time.NewTimer(workspaceHealthTimeout)
	defer timer.Stop()

	select {
	case <-call.done:
		res := call.res
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
		// On nfs the Hub must also see the share itself, not only the mount
		// point: a state-only Hub on a shared layout is unsupported.
		if wsCfg.Backend == string(projectfs.BackendNFS) {
			if msg := s.nfsProjectTreeStatus(deadline); msg != "" {
				checks["workspace_storage"] = msg
				return
			}
		}
		checks["workspace_storage"] = "healthy"
	case <-timer.C:
		checks["workspace_storage"] = "unhealthy: mount check timed out"
	}
}

// workspaceHealthTimeout bounds the mount stat in checkWorkspaceStorageHealth.
// It is a package-level var so tests can shorten it.
var workspaceHealthTimeout = 2 * time.Second

// workspaceHealthStat is the stat used by checkWorkspaceStorageHealth. It is
// a package-level var so tests can inject a stat that hangs.
var workspaceHealthStat = os.Stat

// workspaceHealthStatResult is the outcome of one mount stat.
type workspaceHealthStatResult struct {
	err          error
	mounted      bool
	determinable bool
}

// workspaceHealthProbeCall is one in-flight mount stat shared by every
// checkWorkspaceStorageHealth call for the same key.
type workspaceHealthProbeCall = inFlightCall[workspaceHealthStatResult]

// workspaceHealthProbeKey identifies an in-flight mount stat. requireMount is
// part of the key because it changes what the stat goroutine computes.
type workspaceHealthProbeKey struct {
	path         string
	requireMount bool
}

// workspaceHealthProbesInFlight maps a workspaceHealthProbeKey to its
// in-flight *workspaceHealthProbeCall. On a hung mount a stat never returns
// and its goroutine holds an OS thread in the syscall. Without deduplication
// every health probe would add one more stuck thread. With it, there is at
// most one stuck stat per mount: later probes wait on the existing stat, with
// their own timeout, instead of starting a new one.
var workspaceHealthProbesInFlight sync.Map

// workspaceHealthProbeBeforeDone, when non-nil, is called by the stat
// goroutine just before it closes done. It is a test seam for checking that
// the in-flight entry is already gone by then; it is nil in production.
var workspaceHealthProbeBeforeDone func(key workspaceHealthProbeKey)

// startWorkspaceHealthProbe returns the in-flight stat for mountPath, starting
// one if none is running. The stat goroutine removes its entry before closing
// done, so the first probe after a stat returns starts a fresh one. A probe
// that joins an in-flight stat can return a result up to one stat old.
func startWorkspaceHealthProbe(mountPath string, requireMount bool) *workspaceHealthProbeCall {
	key := workspaceHealthProbeKey{path: mountPath, requireMount: requireMount}
	return joinOrStartInFlight(&workspaceHealthProbesInFlight, key, func() (func() workspaceHealthStatResult, func()) {
		stat, rootPath, hook := workspaceHealthStat, containerRootPath, workspaceHealthProbeBeforeDone
		run := func() workspaceHealthStatResult {
			fi, err := stat(mountPath)
			if err != nil {
				return workspaceHealthStatResult{err: err}
			}
			mounted, determinable := true, true
			if requireMount {
				mounted, determinable = isMountedVolume(fi, rootPath)
			}
			return workspaceHealthStatResult{mounted: mounted, determinable: determinable}
		}
		var beforeDone func()
		if hook != nil {
			beforeDone = func() { hook(key) }
		}
		return run, beforeDone
	})
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
// and the admin health summary lists it under hub.unhealthy_checks (tagged
// with the instance) and hub_instances[].checks, from the instance's
// registry row. The
// registration-pending window is bounded (registration runs immediately
// after the listener starts), so `scion server start` keeps polling for
// "healthy" until its deadline before settling for degraded.
func (s *Server) checkColocatedBrokerHealth(checks map[string]string) {
	state := s.embeddedBrokerSnapshot()
	switch {
	case len(state.ids) > 0 && state.regErr == "":
		checks["colocated_broker"] = "healthy"
	case state.regErr != "":
		// With several co-located flat instances, one refused instance
		// degrades the check even when its siblings activated.
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

	// A server that has begun shutting down is not ready, so a front
	// proxy or load balancer stops routing to it (ptone/scion#5054).
	if s.ShuttingDown() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "shutting down",
		})
		return
	}

	// An instance whose keyring signing ring cannot issue (its active
	// generation reached sign_until, or it has none) is not ready
	// (ptone/scion#1530 §2); verification of eligible generations continues
	// until verify_until.
	if reason := s.signingNotReadyReason(); reason != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": reason,
		})
		return
	}

	// A keyring-file instance whose startup encryption audit found an
	// unreadable infrastructure backup is not ready (ptone/scion#1530 §5);
	// the record is preserved, never replaced. Legacy mode reports it as an
	// unhealthy diagnostic instead.
	if reason := s.backupNotReadyReason(); reason != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": reason,
		})
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
	wsCfg := s.config.ProjectStorageConfig.WorkspaceStorage()
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
