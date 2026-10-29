// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// operationParamAllowlist lists, per routine operation, the only parameters
// it accepts. Operations not listed here accept free-form parameters.
var operationParamAllowlist = map[string][]string{
	"update-binary":  maintenance.UpdateBinaryParams,
	"rebuild-server": {"branch"},
	// The star reconciliation takes no parameters.
	entadapter.ChatStarReconcileKey: {},
}

// rawRequestParams returns the keys of a run request's "params" object,
// whatever the value types (parseMigrationParams drops non-strings, which
// must not let an unknown parameter through).
func rawRequestParams(body map[string]interface{}) map[string]string {
	keys := map[string]string{}
	if m, ok := body["params"].(map[string]interface{}); ok {
		for k := range m {
			keys[k] = ""
		}
	}
	return keys
}

// writeMaintenanceRequestError writes err as an API error, using the code and
// status of a *maintenance.RequestError (400 invalid_request otherwise).
func writeMaintenanceRequestError(w http.ResponseWriter, err error) {
	if re, ok := maintenance.IsRequestError(err); ok {
		writeError(w, re.HTTPStatus(), re.Code, re.Message, nil)
		return
	}
	writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
}

// maintenanceExclusiveGroups maps an operation key to every key whose runs
// it must not overlap. update-binary (sudo-install) and rebuild-server both
// stage <RepoPath>/scion.rebuild and install the hub binary, so they are one
// group. Keys not listed only exclude themselves.
var maintenanceExclusiveGroups = map[string][]string{
	"update-binary":  {"update-binary", "rebuild-server"},
	"rebuild-server": {"update-binary", "rebuild-server"},
}

func maintenanceExclusiveGroup(key string) []string {
	if g, ok := maintenanceExclusiveGroups[key]; ok {
		return g
	}
	return []string{key}
}

// maintenanceRunCreateMu serialises the in-flight check and run creation in
// createExclusiveMaintenanceRun, closing the check-then-create window within
// this hub process (a single-node hub is one process).
var maintenanceRunCreateMu sync.Mutex

// maintenanceGroupInFlight returns the in-flight run (running or restarting)
// of any operation in key's exclusive group, or nil.
func (s *Server) maintenanceGroupInFlight(ctx context.Context, key string) (*store.MaintenanceOperationRun, error) {
	for _, k := range maintenanceExclusiveGroup(key) {
		runs, err := s.store.ListMaintenanceRuns(ctx, k, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 && maintenanceRunInFlight(runs[0].Status) {
			r := runs[0]
			return &r, nil
		}
	}
	return nil, nil
}

// createExclusiveMaintenanceRun creates run unless an operation in its
// exclusive group is in flight, in which case that run is returned and
// nothing is created. A run of the hub-binary group (update-binary,
// rebuild-server) is also refused with errHubRestartPending while a Restart
// Hub request is under way.
func (s *Server) createExclusiveMaintenanceRun(ctx context.Context, run *store.MaintenanceOperationRun) (*store.MaintenanceOperationRun, error) {
	maintenanceRunCreateMu.Lock()
	defer maintenanceRunCreateMu.Unlock()
	if hubBinaryOperation(run.OperationKey) && s.hubRestartPending() {
		return nil, errHubRestartPending
	}
	blocking, err := s.maintenanceGroupInFlight(ctx, run.OperationKey)
	if err != nil || blocking != nil {
		return blocking, err
	}
	return nil, s.store.CreateMaintenanceRun(ctx, run)
}

// errHubRestartPending refuses a hub-binary run while Restart Hub is under
// way.
var errHubRestartPending = errors.New("a hub restart is in progress; retry once the hub is back")

// hubRestartHold is how long a Restart Hub reservation blocks hub-binary
// runs. The restart normally ends this process well within it; the bound
// only matters when a requested restart never happens.
const hubRestartHold = 2 * time.Minute

// hubBinaryOperation reports whether key installs the hub binary (the
// update-binary/rebuild-server exclusive group), which Restart Hub must not
// interleave with.
func hubBinaryOperation(key string) bool {
	_, ok := maintenanceExclusiveGroups[key]
	return ok
}

// hubRestartPending reports whether a Restart Hub reservation is active.
func (s *Server) hubRestartPending() bool {
	at := s.hubRestartReservedAt.Load()
	return at != 0 && time.Since(time.Unix(0, at)) < hubRestartHold
}

// maintenanceGroupRunning returns the latest run of key's exclusive group
// that is "running" (not "restarting"), or nil.
func (s *Server) maintenanceGroupRunning(ctx context.Context, key string) (*store.MaintenanceOperationRun, error) {
	for _, k := range maintenanceExclusiveGroup(key) {
		runs, err := s.store.ListMaintenanceRuns(ctx, k, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 && runs[0].Status == store.MaintenanceStatusRunning {
			r := runs[0]
			return &r, nil
		}
	}
	return nil, nil
}

// reserveHubRestart puts Restart Hub in the hub-binary exclusive group.
//
// It refuses (returning the run) while an update-binary or rebuild-server
// run is "running": that covers the window between an update's Activate and
// its before-restart hook, where a restart would boot the new binary with no
// pending update. A "restarting" run does not block: its pending update is
// already durable, so a restart boots into the state boot reconciliation
// expects, and Restart Hub stays the in-UI way out when an update's own
// restart never ends the process.
//
// Otherwise it records a reservation, returning its stamp for
// releaseHubRestart, that refuses new hub-binary runs until the process
// exits (or hubRestartHold passes). The check and the reservation happen
// under the run-creation lock.
func (s *Server) reserveHubRestart(ctx context.Context) (blocking *store.MaintenanceOperationRun, stamp int64, err error) {
	maintenanceRunCreateMu.Lock()
	defer maintenanceRunCreateMu.Unlock()
	blocking, err = s.maintenanceGroupRunning(ctx, "update-binary")
	if err != nil || blocking != nil {
		return blocking, 0, err
	}
	stamp = time.Now().UnixNano()
	s.hubRestartReservedAt.Store(stamp)
	return nil, stamp, nil
}

// releaseHubRestart drops the Restart Hub reservation stamp (its restart
// could not be requested). A newer reservation by a concurrent restart is
// left in place.
func (s *Server) releaseHubRestart(stamp int64) {
	s.hubRestartReservedAt.CompareAndSwap(stamp, 0)
}

// writeHubRestartPending writes the 409 for a hub-binary run refused while
// Restart Hub is under way. The code is the exclusive group's (conflict).
func writeHubRestartPending(w http.ResponseWriter, key string) {
	writeError(w, http.StatusConflict, ErrCodeConflict,
		fmt.Sprintf("A hub restart is in progress; %s cannot start until the hub is back", key),
		map[string]interface{}{"operationKey": "restart"})
}

// writeMaintenanceInFlight writes the 409 for a request blocked by an
// in-flight run of key's exclusive group.
func writeMaintenanceInFlight(w http.ResponseWriter, key string, blocking *store.MaintenanceOperationRun) {
	msg := "Operation is already running"
	if blocking.OperationKey != key {
		msg = fmt.Sprintf("Operation %s is running; %s cannot run at the same time", blocking.OperationKey, key)
	}
	writeError(w, http.StatusConflict, ErrCodeConflict, msg, map[string]interface{}{
		"runId":        blocking.ID,
		"operationKey": blocking.OperationKey,
	})
}

// maintenanceRunInFlight reports whether a run in this status blocks a new
// run of the same operation: it is running, or it installed a binary and is
// waiting for the hub restart to be confirmed.
func maintenanceRunInFlight(status string) bool {
	return status == store.MaintenanceStatusRunning || status == store.MaintenanceStatusRestarting
}

// maintenanceRunTimeouts bounds whole runs of the listed operations.
// update-binary's own steps are bounded (10 min download phase plus short
// install/hook/restart deadlines); this is the backstop. Other operations
// (image builds, rebuilds) keep running until they finish, as before.
var maintenanceRunTimeouts = map[string]time.Duration{
	"update-binary": 20 * time.Minute,
	// Each batch of the star reconciliation is bounded too; this bounds
	// the sweeps together.
	entadapter.ChatStarReconcileKey: messageStarReconcileTimeout,
}

// maintenanceRunContext returns the context a run of key executes under.
func maintenanceRunContext(key string) (context.Context, context.CancelFunc) {
	if d, ok := maintenanceRunTimeouts[key]; ok {
		return context.WithTimeout(context.Background(), d)
	}
	return context.WithCancel(context.Background())
}

// maintenanceCleanupTimeout bounds a cleanup write made after a failure.
const maintenanceCleanupTimeout = 10 * time.Second

// maintenanceCleanupContext returns the context for a cleanup that must run
// after a failure, whatever happened to parent (which may be the context
// that failed: cancelled, or past its deadline). It is detached from
// parent's cancellation, keeps parent's values (logging, trace) and has its
// own deadline.
func maintenanceCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), maintenanceCleanupTimeout)
}

// launchMaintenanceRun runs executor in the background for the already
// created run and records the outcome on it.
//
// An executor that restarts the hub (maintenance.RestartingExecutor) is given
// a before-restart hook that records the run as "restarting", with its log
// and result, and the system.pending_update row, before the restart is
// requested; the run then stays "restarting" for boot reconciliation
// (reconcilePendingUpdate) instead of being marked completed by a goroutine
// racing the shutdown. If the run fails after the hook (the restart could
// not be requested), the pending update is cleared again.
func (s *Server) launchMaintenanceRun(run *store.MaintenanceOperationRun, executor MaintenanceExecutor, params map[string]string) {
	log := s.maintenanceLog
	key, runID := run.OperationKey, run.ID

	go func() {
		buf := &runLogBuffer{}
		restarting := false

		runCtx, cancel := maintenanceRunContext(key)
		defer cancel()

		// Writes after the run (its outcome, a pending update to clear) use
		// a detached, bounded context: runCtx may be past its deadline.
		cleanup := func(f func(ctx context.Context)) {
			ctx, cancel := maintenanceCleanupContext(runCtx)
			defer cancel()
			f(ctx)
		}

		defer func() {
			if r := recover(); r != nil {
				log.Error("Operation executor panicked", maintenanceLogAttrs(key, "run_id", runID, "panic", fmt.Sprint(r))...)
				finishedAt := time.Now()
				run.CompletedAt = &finishedAt
				run.Status = store.MaintenanceStatusFailed
				run.Log = buf.String()
				resultJSON, _ := json.Marshal(map[string]interface{}{
					"error": fmt.Sprintf("executor panic: %v", r),
				})
				run.Result = string(resultJSON)
				cleanup(func(ctx context.Context) {
					if err := s.store.UpdateMaintenanceRun(ctx, run); err != nil {
						log.Error("Failed to update run record after panic", maintenanceLogAttrs(key, "run_id", runID, "error", err)...)
					}
					// A panic in or after the before-restart hook may leave
					// a pending update naming this failed run; nothing
					// restarts, so remove it (a no-op when it names another
					// run or none).
					s.clearPendingUpdateFor(ctx, runID)
				})
			}
		}()

		var execErr error
		if re, ok := executor.(maintenance.RestartingExecutor); ok {
			execErr = re.RunWithBeforeRestart(runCtx, buf, params,
				func(ctx context.Context, p maintenance.PendingRestart) error {
					if err := s.markRunRestarting(ctx, run, buf, p); err != nil {
						return err
					}
					restarting = true
					return nil
				})
		} else {
			execErr = executor.Run(runCtx, buf, params)
		}

		finishedAt := time.Now()
		run.Log = buf.String()

		switch {
		case execErr != nil:
			run.CompletedAt = &finishedAt
			run.Status = store.MaintenanceStatusFailed
			result := map[string]interface{}{"error": execErr.Error()}
			if re, ok := maintenance.IsRequestError(execErr); ok {
				result["code"] = re.Code
			}
			resultJSON, _ := json.Marshal(result)
			run.Result = string(resultJSON)
			if restarting {
				cleanup(func(ctx context.Context) { s.clearPendingUpdateFor(ctx, runID) })
			}
			log.Error("Operation failed", maintenanceLogAttrs(key, "run_id", runID, "error", execErr, "duration", finishedAt.Sub(run.StartedAt))...)
			log.Debug("Operation failure log output", maintenanceLogAttrs(key, "run_id", runID, "log", run.Log)...)
		case restarting:
			// Status, result and log were written before the restart, and
			// boot reconciliation completes the run. Nothing more is
			// written: a late write from this (exiting) process must never
			// overwrite the outcome the restarted hub records.
			log.Info("Operation installed an update, hub restart requested", maintenanceLogAttrs(key, "run_id", runID)...)
			return
		default:
			run.CompletedAt = &finishedAt
			run.Status = store.MaintenanceStatusCompleted
			log.Info("Operation completed successfully", maintenanceLogAttrs(key, "run_id", runID, "duration", finishedAt.Sub(run.StartedAt))...)
		}

		cleanup(func(ctx context.Context) {
			if err := s.store.UpdateMaintenanceRun(ctx, run); err != nil {
				log.Error("Failed to update run record", maintenanceLogAttrs(key, "run_id", runID, "error", err)...)
			}
		})
	}()
}

// markRunRestarting writes system.pending_update for boot reconciliation,
// then records run as "restarting" with the log so far and the update's
// from/to/strategy. It runs as the before-restart hook, so a failure of
// either write aborts the restart (the runner then fails the run).
func (s *Server) markRunRestarting(ctx context.Context, run *store.MaintenanceOperationRun, buf *runLogBuffer, p maintenance.PendingRestart) error {
	result := map[string]interface{}{
		"from":     p.From,
		"to":       p.To,
		"strategy": p.Strategy,
	}
	if p.Snapshot != "" {
		result["snapshot"] = p.Snapshot
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode run result: %w", err)
	}

	// pending_update is written first: a page that finds the run
	// "restarting" can then always find the pending update naming it.
	pending := PendingUpdate{
		RunID:    run.ID,
		From:     p.From,
		To:       p.To,
		Snapshot: p.Snapshot,
		Strategy: p.Strategy,
		At:       time.Now().UTC().Format(time.RFC3339),
		Channel:  p.Channel,
		Pin:      p.Pin,
		SetBy:    run.StartedBy,
	}
	if err := s.writeHubSettingJSON(ctx, HubSettingSectionPendingUpdate, pending); err != nil {
		return fmt.Errorf("record pending update: %w", err)
	}

	prevStatus, prevResult, prevLog := run.Status, run.Result, run.Log
	run.Status = store.MaintenanceStatusRestarting
	run.Result = string(resultJSON)
	run.Log = buf.String()
	if err := s.store.UpdateMaintenanceRun(ctx, run); err != nil {
		run.Status, run.Result, run.Log = prevStatus, prevResult, prevLog
		// No restart follows, so the pending update must not survive, even
		// when ctx (the hook's) is what failed the write.
		s.clearPendingUpdateAfterFailure(ctx, run.ID)
		return fmt.Errorf("record run as restarting: %w", err)
	}
	// system.update_available is deliberately left alone: the update is not
	// confirmed until the new binary boots (boot reconciliation clears it on
	// "verified"), and a restart that fails and rolls back must keep the
	// banner an admin retries from.
	return nil
}

// clearPendingUpdateAfterFailure clears the pending update naming runID
// after a failure, on a cleanup context (maintenanceCleanupContext), so a
// cancelled or expired ctx cannot leave a stale pending_update behind.
func (s *Server) clearPendingUpdateAfterFailure(ctx context.Context, runID string) {
	cctx, cancel := maintenanceCleanupContext(ctx)
	defer cancel()
	s.clearPendingUpdateFor(cctx, runID)
}

// runLogBuffer is a mutex-guarded log buffer. It implements only Write (not
// io.ReaderFrom), so a child process started by an executor can keep writing
// to it while the runner reads it.
type runLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *runLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *runLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
