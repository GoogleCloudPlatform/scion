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

package runtimebroker

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const (
	// durationBackstopSweepInterval is how often the broker sweeps all
	// scion-managed runtimes for expired run deadlines (design §3.5).
	durationBackstopSweepInterval = 60 * time.Second
)

// durationBackstopCallTimeout bounds each runtime List/Stop call and Hub
// status report made by the duration backstop sweeper, so one slow runtime or
// Hub connection cannot stall the sweep for other runtimes. A variable so
// tests can shorten it.
var durationBackstopCallTimeout = 60 * time.Second

// durationBackstopSweeper periodically lists all scion-managed runtimes
// (primary + auxiliary, with no MountInspector filter and no Hub-connection
// dependency) and stops any running agent whose immutable scion.io/run-deadline
// label or annotation is in the past. Because the deadline lives in runtime
// metadata rather than broker memory, enforcement survives broker restarts.
type durationBackstopSweeper struct {
	runtimes func() []scionrt.Runtime
	report   func(ctx context.Context, a api.AgentInfo)
	now      func() time.Time
	interval time.Duration
	log      *slog.Logger

	mu       sync.Mutex
	reported map[string]struct{}
}

func newDurationBackstopSweeper(
	runtimes func() []scionrt.Runtime,
	report func(ctx context.Context, a api.AgentInfo),
	log *slog.Logger,
) *durationBackstopSweeper {
	if log == nil {
		log = slog.Default()
	}
	return &durationBackstopSweeper{
		runtimes: runtimes,
		report:   report,
		now:      time.Now,
		interval: durationBackstopSweepInterval,
		log:      log,
		reported: make(map[string]struct{}),
	}
}

// run sweeps once at startup (catching any runs whose deadline elapsed while
// the broker was down) and then on every interval tick until ctx is cancelled.
func (sw *durationBackstopSweeper) run(ctx context.Context) {
	sw.sweep(ctx)
	interval := sw.interval
	if interval <= 0 {
		interval = durationBackstopSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sw.sweep(ctx)
		}
	}
}

// distinctRuntimes returns the non-nil, deduplicated runtimes to sweep.
func (sw *durationBackstopSweeper) distinctRuntimes() []scionrt.Runtime {
	if sw.runtimes == nil {
		return nil
	}
	var out []scionrt.Runtime
	seen := map[any]bool{}
	for _, rt := range sw.runtimes() {
		if rt == nil {
			continue
		}
		if reflect.TypeOf(rt).Comparable() {
			if seen[rt] {
				continue
			}
			seen[rt] = true
		}
		out = append(out, rt)
	}
	return out
}

// sweep checks all runtimes concurrently so a slow or failing runtime List
// does not block other runtimes on the same tick.
func (sw *durationBackstopSweeper) sweep(ctx context.Context) {
	rts := sw.distinctRuntimes()
	if len(rts) == 0 {
		return
	}
	now := sw.now().UTC()
	var (
		wg          sync.WaitGroup
		seenMu      sync.Mutex
		activeKeys  = make(map[string]bool)
		unlistedRts = make(map[string]bool)
	)
	for _, rt := range rts {
		wg.Add(1)
		go func(rt scionrt.Runtime) {
			defer wg.Done()
			rtKeys, ok := sw.sweepRuntime(ctx, rt, now)
			seenMu.Lock()
			defer seenMu.Unlock()
			if !ok {
				unlistedRts[rt.Name()] = true
				return
			}
			for k := range rtKeys {
				activeKeys[k] = true
			}
		}(rt)
	}
	wg.Wait()

	sw.mu.Lock()
	for k := range sw.reported {
		if activeKeys[k] || unlistedRts[runtimeOfKey(k)] {
			continue
		}
		delete(sw.reported, k)
	}
	sw.mu.Unlock()
}

func backstopReportKey(runtimeName, containerID, runID string) string {
	return runtimeName + "\x00" + containerID + "\x00" + runID
}

// sweepRuntime lists one runtime and stops any running agent whose
// scion.io/run-deadline is at or before now. It returns the set of active
// container keys observed and false if List failed.
func (sw *durationBackstopSweeper) sweepRuntime(ctx context.Context, rt scionrt.Runtime, now time.Time) (map[string]bool, bool) {
	listCtx, cancel := context.WithTimeout(ctx, durationBackstopCallTimeout)
	agents, err := rt.List(listCtx, nil)
	cancel()
	if err != nil {
		sw.log.Warn("duration backstop sweep: could not list agents; retrying on next sweep",
			"runtime", rt.Name(), "error", err)
		return nil, false
	}

	active := make(map[string]bool)
	for _, a := range agents {
		if a.ContainerID == "" || a.Name == "" {
			continue
		}
		if !isBackstopStoppableAgent(a) {
			continue
		}
		deadline, ok := runDeadlineOf(a)
		if !ok {
			continue
		}
		opID := scionrt.AgentOperationID(a)
		key := backstopReportKey(rt.Name(), opID, a.RunID)
		active[key] = true
		if now.Before(deadline) {
			continue
		}

		sw.mu.Lock()
		_, alreadyReported := sw.reported[key]
		sw.mu.Unlock()
		if alreadyReported {
			continue
		}

		sw.stopExpired(ctx, rt, a, opID, deadline, key)
	}
	return active, true
}

func (sw *durationBackstopSweeper) stopExpired(ctx context.Context, rt scionrt.Runtime, a api.AgentInfo, opID string, deadline time.Time, key string) {
	sw.log.Warn("duration backstop sweep: stopping agent whose max_duration deadline elapsed",
		"agent", a.Name, "runtime", rt.Name(), "container", opID, "run_id", a.RunID,
		"deadline", deadline.UTC().Format(time.RFC3339))

	stopCtx, cancel := context.WithTimeout(ctx, durationBackstopCallTimeout)
	err := rt.Stop(stopCtx, scionrt.RunRef{ID: opID, RunID: a.RunID})
	cancel()
	if err != nil {
		if errors.Is(err, scionrt.ErrRunConflict) || errors.Is(err, scionrt.ErrRunMismatch) {
			// A newer run replaced this container/pod between List and Stop: do not
			// report limits_exceeded against the replacement run.
			sw.log.Debug("duration backstop sweep: run changed before stop; skipping report",
				"agent", a.Name, "runtime", rt.Name(), "container", opID, "run_id", a.RunID)
			return
		}
		if isAlreadyStoppedRuntimeErr(err) {
			// sciontool or the platform already exited/removed the container
			// between List and Stop: avoid duplicate stop/report.
			sw.log.Debug("duration backstop sweep: container already stopped/absent; skipping report",
				"agent", a.Name, "runtime", rt.Name(), "container", opID, "error", err)
			return
		}
		sw.log.Error("duration backstop sweep: failed to stop expired agent; retrying on next sweep",
			"agent", a.Name, "runtime", rt.Name(), "container", opID, "error", err)
		return
	}

	sw.mu.Lock()
	sw.reported[key] = struct{}{}
	sw.mu.Unlock()

	if sw.report != nil {
		sw.report(ctx, a)
	}
}

// runDeadlineOf extracts the UTC run deadline from a's annotations or labels.
// Annotations are checked first (Kubernetes, Cloud Run Instances), then labels
// (Docker, Podman, Apple Container, Cloud Run Sandbox).
func runDeadlineOf(a api.AgentInfo) (time.Time, bool) {
	raw := strings.TrimSpace(a.Annotations[api.LabelRunDeadline])
	if raw == "" {
		raw = strings.TrimSpace(a.Labels[api.LabelRunDeadline])
	}
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// isBackstopStoppableAgent reports whether a represents a live (non-terminal)
// container/pod/instance that the backstop may stop. Already-stopped agents
// (for example when sciontool exited cleanly first) return false so the
// backstop never double-stops or emits duplicate events.
func isBackstopStoppableAgent(a api.AgentInfo) bool {
	switch state.Phase(a.Phase) {
	case state.PhaseStopped, state.PhaseError, state.PhaseHalted, state.PhaseSuspended:
		return false
	}
	if a.Phase == "" && a.ContainerStatus != "" {
		cs := strings.ToLower(strings.TrimSpace(a.ContainerStatus))
		if strings.HasPrefix(cs, "exited") || strings.HasPrefix(cs, "stopped") ||
			cs == "succeeded" || cs == "failed" {
			return false
		}
	}
	return true
}

// isAlreadyStoppedRuntimeErr reports whether a runtime Stop error indicates
// the container/pod/instance was already stopped or removed before Stop ran.
func isAlreadyStoppedRuntimeErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "is not running") ||
		strings.Contains(msg, "already stopped")
}

// reportDurationBackstopStop reports a broker-initiated max_duration backstop
// stop to connected Hub(s) via the existing broker heartbeat endpoint, with
// Inventory omitted so missing-agent reconciliation is not triggered. When no
// Hub connection is active, the report is dropped cleanly.
func (s *Server) reportDurationBackstopStop(ctx context.Context, a api.AgentInfo) {
	conns := s.hubConnectionList()
	if len(conns) == 0 {
		slog.Debug("duration backstop sweep: no hub connection; dropping limits_exceeded report",
			"agent", a.Name, "run_id", a.RunID)
		return
	}

	projectID := a.ProjectID
	if projectID == "" {
		projectID = a.Project
	}
	if projectID == "" {
		projectID = "default"
	}

	hb := &hubclient.BrokerHeartbeat{
		Status: "online",
		Projects: []hubclient.ProjectHeartbeat{
			{
				ProjectID:  projectID,
				AgentCount: 1,
				Agents: []hubclient.AgentHeartbeat{
					{
						Slug:       a.Name,
						AgentID:    heartbeatAgentID(a),
						RunID:      a.RunID,
						Status:     string(state.ActivityLimitsExceeded),
						Phase:      string(state.PhaseStopped),
						Activity:   string(state.ActivityLimitsExceeded),
						ExitReason: string(state.ExitReasonLimitsExceeded),
						Message:    api.ReasonMaxDurationBackstop,
					},
				},
			},
		},
	}

	for _, conn := range conns {
		if conn == nil || conn.HubClient == nil || conn.BrokerID == "" {
			continue
		}
		if conn.Heartbeat != nil && conn.Heartbeat.projectFilter != nil && !conn.Heartbeat.projectFilter(projectID) {
			continue
		}
		reportCtx, cancel := context.WithTimeout(ctx, durationBackstopCallTimeout)
		_, err := conn.HubClient.RuntimeBrokers().Heartbeat(reportCtx, conn.BrokerID, hb)
		cancel()
		if err != nil {
			slog.Warn("duration backstop sweep: failed to report limits_exceeded to hub",
				"hub", conn.Name, "agent", a.Name, "run_id", a.RunID, "error", err)
		}
	}
}

// startDurationBackstopSweep starts the background durationBackstopSweeper
// once per server. Cancelled by Shutdown alongside controlPathSweeper.
func (s *Server) startDurationBackstopSweep(ctx context.Context) {
	s.durationBackstopSweepOnce.Do(func() {
		sweepCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.durationBackstopSweepCancel = cancel
		s.mu.Unlock()
		sw := newDurationBackstopSweeper(s.sweptRuntimes, s.reportDurationBackstopStop, s.agentLifecycleLog)
		go sw.run(sweepCtx)
	})
}
