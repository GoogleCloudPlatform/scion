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
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// operationParamAllowlist lists, per routine operation, the only parameters
// it accepts. Operations not listed here accept free-form parameters.
var operationParamAllowlist = map[string][]string{
	"update-binary":  maintenance.UpdateBinaryParams,
	"rebuild-server": {"branch"},
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
// nothing is created.
func (s *Server) createExclusiveMaintenanceRun(ctx context.Context, run *store.MaintenanceOperationRun) (*store.MaintenanceOperationRun, error) {
	maintenanceRunCreateMu.Lock()
	defer maintenanceRunCreateMu.Unlock()
	blocking, err := s.maintenanceGroupInFlight(ctx, run.OperationKey)
	if err != nil || blocking != nil {
		return blocking, err
	}
	return nil, s.store.CreateMaintenanceRun(ctx, run)
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

// detectUpdateInstaller picks the update-binary Installer for this host from
// the maintenance config, with the default install root.
func (s *Server) detectUpdateInstaller() maintenance.Installer {
	mc := s.config.MaintenanceConfig
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return maintenance.Detect(ctx, maintenance.DetectOptions{
		RepoPath:    mc.RepoPath,
		BinaryDest:  mc.BinaryDest,
		ServiceName: mc.ServiceName,
	})
}

// maintenanceRunTimeouts bounds whole runs of the listed operations.
// update-binary's own steps are bounded (10 min download phase plus short
// install/hook/restart deadlines); this is the backstop. Other operations
// (image builds, rebuilds) keep running until they finish, as before.
var maintenanceRunTimeouts = map[string]time.Duration{
	"update-binary": 20 * time.Minute,
}

// maintenanceRunContext returns the context a run of key executes under.
func maintenanceRunContext(key string) (context.Context, context.CancelFunc) {
	if d, ok := maintenanceRunTimeouts[key]; ok {
		return context.WithTimeout(context.Background(), d)
	}
	return context.WithCancel(context.Background())
}

// launchMaintenanceRun runs executor in the background for the already
// created run and records the outcome on it.
//
// An executor that restarts the hub (maintenance.RestartingExecutor) is given
// a before-restart hook that records the run as "restarting", with its log
// and result, before the restart is requested; the run then stays
// "restarting" for boot reconciliation instead of being marked completed by
// a goroutine racing the shutdown.
func (s *Server) launchMaintenanceRun(run *store.MaintenanceOperationRun, executor MaintenanceExecutor, params map[string]string) {
	log := s.maintenanceLog
	key, runID := run.OperationKey, run.ID

	go func() {
		buf := &runLogBuffer{}
		restarting := false

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
				if err := s.store.UpdateMaintenanceRun(context.Background(), run); err != nil {
					log.Error("Failed to update run record after panic", maintenanceLogAttrs(key, "run_id", runID, "error", err)...)
				}
			}
		}()

		runCtx, cancel := maintenanceRunContext(key)
		defer cancel()

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
			log.Error("Operation failed", maintenanceLogAttrs(key, "run_id", runID, "error", execErr, "duration", finishedAt.Sub(run.StartedAt))...)
			log.Debug("Operation failure log output", maintenanceLogAttrs(key, "run_id", runID, "log", run.Log)...)
		case restarting:
			// Status and result were written before the restart; only the
			// log tail is added. Boot reconciliation completes the run.
			log.Info("Operation installed an update, hub restart requested", maintenanceLogAttrs(key, "run_id", runID)...)
		default:
			run.CompletedAt = &finishedAt
			run.Status = store.MaintenanceStatusCompleted
			log.Info("Operation completed successfully", maintenanceLogAttrs(key, "run_id", runID, "duration", finishedAt.Sub(run.StartedAt))...)
		}

		if err := s.store.UpdateMaintenanceRun(context.Background(), run); err != nil {
			log.Error("Failed to update run record", maintenanceLogAttrs(key, "run_id", runID, "error", err)...)
		}
	}()
}

// markRunRestarting records run as "restarting" with the log so far and the
// update's from/to/strategy. It runs as the before-restart hook, so a failure aborts the restart.
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

	prevStatus, prevResult, prevLog := run.Status, run.Result, run.Log
	run.Status = store.MaintenanceStatusRestarting
	run.Result = string(resultJSON)
	run.Log = buf.String()
	if err := s.store.UpdateMaintenanceRun(ctx, run); err != nil {
		run.Status, run.Result, run.Log = prevStatus, prevResult, prevLog
		return fmt.Errorf("record run as restarting: %w", err)
	}
	// system.update_available is deliberately left alone: the update is not
	// confirmed until the new binary boots (boot reconciliation clears it on
	// "verified"), and a restart that fails and rolls back must keep the
	// banner an admin retries from.
	return nil
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
