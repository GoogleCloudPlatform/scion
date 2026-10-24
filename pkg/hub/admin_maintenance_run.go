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

		var execErr error
		if re, ok := executor.(maintenance.RestartingExecutor); ok {
			execErr = re.RunWithBeforeRestart(context.Background(), buf, params,
				func(ctx context.Context, p maintenance.PendingRestart) error {
					if err := s.markRunRestarting(ctx, run, buf, p); err != nil {
						return err
					}
					restarting = true
					return nil
				})
		} else {
			execErr = executor.Run(context.Background(), buf, params)
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
// update's from/to/strategy, and clears the update-available notification.
// It runs as the before-restart hook, so a failure aborts the restart.
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

	if err := s.store.DeleteHubSetting(ctx, HubSettingSectionUpdateAvailable); err != nil && err != store.ErrNotFound {
		s.maintenanceLog.Warn("Failed to clear update_available setting", "error", err)
	}
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
