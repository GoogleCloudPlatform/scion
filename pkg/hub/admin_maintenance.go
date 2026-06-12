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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// maintenanceLogAttrs returns common slog attributes for maintenance operation logging.
func maintenanceLogAttrs(key string, extra ...any) []any {
	attrs := []any{"operation_key", key}
	return append(attrs, extra...)
}

// handleAdminMaintenanceOps handles routes under /api/v1/admin/maintenance/operations.
func (s *Server) handleAdminMaintenanceOps(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	// Extract sub-path: /api/v1/admin/maintenance/operations/{key}[/run|/runs|/runs/{runId}]
	subPath := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/maintenance/operations")
	subPath = strings.TrimPrefix(subPath, "/")

	if subPath == "" {
		if r.Method != http.MethodGet {
			MethodNotAllowed(w)
			return
		}
		s.listMaintenanceOperations(w, r)
		return
	}

	// Parse: {key}, {key}/run, {key}/runs, {key}/runs/{runId}
	parts := strings.SplitN(subPath, "/", 3)
	key := parts[0]

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			MethodNotAllowed(w)
			return
		}
		s.getMaintenanceOperation(w, r, key)
		return
	}

	action := parts[1]

	switch action {
	case "run":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.executeOperation(w, r, key, user)
	case "runs":
		if r.Method != http.MethodGet {
			MethodNotAllowed(w)
			return
		}
		if len(parts) == 3 && parts[2] != "" {
			s.getOperationRun(w, r, parts[2])
		} else {
			s.listOperationRuns(w, r, key)
		}
	default:
		writeError(w, http.StatusNotFound, ErrCodeNotFound, "Not found", nil)
	}
}

// handleAdminMaintenanceMigrations handles routes under /api/v1/admin/maintenance/migrations/.
// Supports POST /api/v1/admin/maintenance/migrations/{key}/run to execute a migration.
func (s *Server) handleAdminMaintenanceMigrations(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	// Extract sub-path: /api/v1/admin/maintenance/migrations/{key}/run
	subPath := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/maintenance/migrations/")
	parts := strings.SplitN(subPath, "/", 2)
	if len(parts) < 2 || parts[1] != "run" || parts[0] == "" {
		writeError(w, http.StatusNotFound, ErrCodeNotFound, "Not found", nil)
		return
	}

	key := parts[0]

	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	s.executeMigration(w, r, key, user)
}

// executeMigration starts execution of a migration by key.
func (s *Server) executeMigration(w http.ResponseWriter, r *http.Request, key string, user UserIdentity) {
	ctx := r.Context()
	log := s.maintenanceLog

	log.Debug("Execute migration requested", maintenanceLogAttrs(key, "user", user.Email())...)

	// Look up the migration.
	op, err := s.store.GetMaintenanceOperation(ctx, key)
	if err != nil {
		if err == store.ErrNotFound {
			log.Debug("Migration not found", maintenanceLogAttrs(key)...)
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "Migration not found", nil)
			return
		}
		log.Error("Failed to get migration from store", maintenanceLogAttrs(key, "error", err)...)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to get migration", nil)
		return
	}

	// Verify this is a migration, not a routine operation.
	if op.Category != store.MaintenanceCategoryMigration {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "This operation is not a migration; use the operations endpoint", nil)
		return
	}

	// Prevent re-running completed migrations (use CLI --force for that).
	if op.Status == store.MaintenanceStatusCompleted {
		writeError(w, http.StatusConflict, ErrCodeConflict, "Migration already completed; use CLI --force to re-run", nil)
		return
	}

	// Prevent running if already in progress.
	if op.Status == store.MaintenanceStatusRunning {
		writeError(w, http.StatusConflict, ErrCodeConflict, "Migration is already running", nil)
		return
	}

	// Parse request body for params.
	var body map[string]interface{}
	if r.Body != nil {
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	params := parseMigrationParams(body)

	// Resolve the executor for this migration key.
	executor, err := s.resolveMaintenanceExecutor(key)
	if err != nil {
		log.Error("Failed to resolve migration executor", maintenanceLogAttrs(key, "error", err)...)
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
		return
	}

	// Mark the migration as running.
	now := time.Now()
	op.Status = store.MaintenanceStatusRunning
	op.StartedAt = &now
	op.StartedBy = user.Email()
	if err := s.store.UpdateMaintenanceOperation(ctx, op); err != nil {
		log.Error("Failed to update migration status", maintenanceLogAttrs(key, "error", err)...)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to update migration status", nil)
		return
	}

	log.Info("Migration started", maintenanceLogAttrs(key, "user", user.Email())...)

	// Execute asynchronously.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Migration executor panicked", maintenanceLogAttrs(key, "panic", fmt.Sprint(r))...)
				finishedAt := time.Now()
				op.CompletedAt = &finishedAt
				op.Status = store.MaintenanceStatusFailed
				result := map[string]interface{}{
					"error": fmt.Sprintf("executor panic: %v", r),
				}
				resultJSON, _ := json.Marshal(result)
				op.Result = string(resultJSON)
				if err := s.store.UpdateMaintenanceOperation(context.Background(), op); err != nil {
					log.Error("Failed to update migration after panic", maintenanceLogAttrs(key, "error", err)...)
				}
			}
		}()

		var buf bytes.Buffer
		execErr := executor.Run(context.Background(), &buf, params)

		finishedAt := time.Now()
		op.CompletedAt = &finishedAt

		if execErr != nil {
			op.Status = store.MaintenanceStatusFailed
			result := map[string]interface{}{
				"error": execErr.Error(),
				"log":   buf.String(),
			}
			resultJSON, _ := json.Marshal(result)
			op.Result = string(resultJSON)
			log.Error("Migration failed", maintenanceLogAttrs(key, "error", execErr)...)
			log.Debug("Migration failure log output", maintenanceLogAttrs(key, "log", buf.String())...)
		} else {
			op.Status = store.MaintenanceStatusCompleted
			result := map[string]interface{}{
				"log": buf.String(),
			}
			if params["dryRun"] == "true" {
				// Dry runs don't actually complete the migration — reset to pending.
				op.Status = store.MaintenanceStatusPending
				op.CompletedAt = nil
				result["dryRun"] = true
			}
			resultJSON, _ := json.Marshal(result)
			op.Result = string(resultJSON)
			log.Info("Migration completed", maintenanceLogAttrs(key, "status", op.Status)...)
		}

		if err := s.store.UpdateMaintenanceOperation(context.Background(), op); err != nil {
			log.Error("Failed to update migration record", maintenanceLogAttrs(key, "error", err)...)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "running",
	})
}

// resolveMaintenanceExecutor returns the executor for a given operation key.
func (s *Server) resolveMaintenanceExecutor(key string) (MaintenanceExecutor, error) {
	mc := s.config.MaintenanceConfig
	log := s.maintenanceLog

	switch key {
	case "secret-hub-id-migration":
		backend := s.GetSecretBackend()
		if backend == nil {
			return nil, fmt.Errorf("no secret backend configured; cannot run secret migration")
		}
		return &SecretMigrationExecutor{
			store:         s.store,
			secretBackend: backend,
		}, nil
	case "pull-images":
		log.Debug("Resolved pull-images executor",
			"runtime_bin", mc.RuntimeBin, "registry", mc.ImageRegistry,
			"tag", mc.ImageTag, "harnesses", fmt.Sprint(mc.Harnesses))
		return &PullImagesExecutor{
			runtimeBin: mc.RuntimeBin,
			registry:   mc.ImageRegistry,
			tag:        mc.ImageTag,
			harnesses:  mc.Harnesses,
		}, nil
	case "rebuild-server":
		log.Debug("Resolved rebuild-server executor",
			"repo_path", mc.RepoPath, "branch", mc.RepoBranch,
			"binary_dest", mc.BinaryDest, "service_name", mc.ServiceName)
		return &RebuildServerExecutor{
			repoPath:    mc.RepoPath,
			repoBranch:  mc.RepoBranch,
			binaryDest:  mc.BinaryDest,
			serviceName: mc.ServiceName,
		}, nil
	case "rebuild-web":
		log.Debug("Resolved rebuild-web executor", "repo_path", mc.RepoPath, "branch", mc.RepoBranch)
		return &RebuildWebExecutor{
			repoPath:   mc.RepoPath,
			repoBranch: mc.RepoBranch,
		}, nil
	case "rebuild-container-binaries":
		log.Debug("Resolved rebuild-container-binaries executor", "repo_path", mc.RepoPath)
		return &RebuildContainerBinariesExecutor{
			repoPath: mc.RepoPath,
		}, nil
	default:
		return nil, fmt.Errorf("no executor registered for operation %q", key)
	}
}

// executeOperation starts execution of a routine operation by key.
func (s *Server) executeOperation(w http.ResponseWriter, r *http.Request, key string, user UserIdentity) {
	ctx := r.Context()
	log := s.maintenanceLog

	log.Debug("Execute operation requested", maintenanceLogAttrs(key, "user", user.Email())...)

	// Look up the operation.
	op, err := s.store.GetMaintenanceOperation(ctx, key)
	if err != nil {
		if err == store.ErrNotFound {
			log.Debug("Operation not found", maintenanceLogAttrs(key)...)
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "Operation not found", nil)
			return
		}
		log.Error("Failed to get operation from store", maintenanceLogAttrs(key, "error", err)...)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to get operation", nil)
		return
	}

	// Verify this is a routine operation, not a migration.
	if op.Category != store.MaintenanceCategoryOperation {
		log.Debug("Rejected: operation is a migration", maintenanceLogAttrs(key, "category", op.Category)...)
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "This is a migration; use the migrations endpoint", nil)
		return
	}

	// Parse request body for params.
	var body map[string]interface{}
	if r.Body != nil {
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	params := parseMigrationParams(body) // reuse same param parser

	// Resolve the executor.
	executor, err := s.resolveMaintenanceExecutor(key)
	if err != nil {
		log.Error("Failed to resolve executor", maintenanceLogAttrs(key, "error", err)...)
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
		return
	}

	// Create a run record.
	runID := api.NewUUID()
	now := time.Now()
	run := &store.MaintenanceOperationRun{
		ID:           runID,
		OperationKey: key,
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    now,
		StartedBy:    user.Email(),
	}
	if err := s.store.CreateMaintenanceRun(ctx, run); err != nil {
		log.Error("Failed to create run record", maintenanceLogAttrs(key, "error", err)...)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to create run record", nil)
		return
	}

	log.Info("Operation started", maintenanceLogAttrs(key, "run_id", runID, "user", user.Email())...)

	// Execute asynchronously.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Operation executor panicked", maintenanceLogAttrs(key, "run_id", runID, "panic", fmt.Sprint(r))...)
				finishedAt := time.Now()
				run.CompletedAt = &finishedAt
				run.Status = store.MaintenanceStatusFailed
				result := map[string]interface{}{
					"error": fmt.Sprintf("executor panic: %v", r),
				}
				resultJSON, _ := json.Marshal(result)
				run.Result = string(resultJSON)
				if err := s.store.UpdateMaintenanceRun(context.Background(), run); err != nil {
					log.Error("Failed to update run record after panic", maintenanceLogAttrs(key, "run_id", runID, "error", err)...)
				}
			}
		}()

		var buf bytes.Buffer
		execErr := executor.Run(context.Background(), &buf, params)

		finishedAt := time.Now()
		run.CompletedAt = &finishedAt
		run.Log = buf.String()

		if execErr != nil {
			run.Status = store.MaintenanceStatusFailed
			result := map[string]interface{}{
				"error": execErr.Error(),
			}
			resultJSON, _ := json.Marshal(result)
			run.Result = string(resultJSON)
			log.Error("Operation failed", maintenanceLogAttrs(key, "run_id", runID, "error", execErr, "duration", finishedAt.Sub(run.StartedAt))...)
			log.Debug("Operation failure log output", maintenanceLogAttrs(key, "run_id", runID, "log", buf.String())...)
		} else {
			run.Status = store.MaintenanceStatusCompleted
			log.Info("Operation completed successfully", maintenanceLogAttrs(key, "run_id", runID, "duration", finishedAt.Sub(run.StartedAt))...)
		}

		if err := s.store.UpdateMaintenanceRun(context.Background(), run); err != nil {
			log.Error("Failed to update run record", maintenanceLogAttrs(key, "run_id", runID, "error", err)...)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"runId":  runID,
		"status": "running",
	})
}

// listOperationRuns returns the run history for a given operation key.
func (s *Server) listOperationRuns(w http.ResponseWriter, r *http.Request, key string) {
	// Verify operation exists.
	if _, err := s.store.GetMaintenanceOperation(r.Context(), key); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "Operation not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to get operation", nil)
		return
	}

	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &limit); n != 1 || err != nil || limit < 1 {
			limit = 20
		}
		if limit > 100 {
			limit = 100
		}
	}

	runs, err := s.store.ListMaintenanceRuns(r.Context(), key, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to list runs", nil)
		return
	}

	var resp []maintenanceRunResponse
	for _, run := range runs {
		resp = append(resp, toMaintenanceRunResponse(run))
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"runs": resp,
	})
}

// getOperationRun returns a single run by ID.
func (s *Server) getOperationRun(w http.ResponseWriter, r *http.Request, runID string) {
	run, err := s.store.GetMaintenanceRun(r.Context(), runID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "Run not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to get run", nil)
		return
	}

	resp := toMaintenanceRunResponse(*run)
	writeJSON(w, http.StatusOK, resp)
}

// listMaintenanceOperations returns all operations grouped by category.
func (s *Server) listMaintenanceOperations(w http.ResponseWriter, r *http.Request) {
	ops, err := s.store.ListMaintenanceOperations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to list maintenance operations", nil)
		return
	}

	var migrations []maintenanceOperationResponse
	var operations []maintenanceOperationWithLastRunResponse

	devBinaries := os.Getenv("SCION_DEV_BINARIES") != ""
	for _, op := range ops {
		if op.Key == "rebuild-container-binaries" && !devBinaries {
			continue
		}
		if op.Category == store.MaintenanceCategoryMigration {
			migrations = append(migrations, toMaintenanceOperationResponse(op))
		} else {
			resp := maintenanceOperationWithLastRunResponse{
				maintenanceOperationResponse: toMaintenanceOperationResponse(op),
			}

			// Fetch the most recent run for this operation.
			runs, err := s.store.ListMaintenanceRuns(r.Context(), op.Key, 1)
			if err == nil && len(runs) > 0 {
				lastRun := toMaintenanceRunResponse(runs[0])
				resp.LastRun = &lastRun
			}

			operations = append(operations, resp)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"migrations": migrations,
		"operations": operations,
	})
}

// getMaintenanceOperation returns a single operation by key.
func (s *Server) getMaintenanceOperation(w http.ResponseWriter, r *http.Request, key string) {
	op, err := s.store.GetMaintenanceOperation(r.Context(), key)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "Operation not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to get operation", nil)
		return
	}

	writeJSON(w, http.StatusOK, toMaintenanceOperationResponse(*op))
}

// Response types for maintenance operations API.

type maintenanceOperationResponse struct {
	ID          string      `json:"id"`
	Key         string      `json:"key"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Category    string      `json:"category"`
	Status      string      `json:"status"`
	CreatedAt   interface{} `json:"createdAt"`
	StartedAt   interface{} `json:"startedAt"`
	CompletedAt interface{} `json:"completedAt"`
	StartedBy   interface{} `json:"startedBy"`
	Result      interface{} `json:"result"`
}

type maintenanceOperationWithLastRunResponse struct {
	maintenanceOperationResponse
	LastRun *maintenanceRunResponse `json:"lastRun"`
}

type maintenanceRunResponse struct {
	ID           string      `json:"id"`
	OperationKey string      `json:"operationKey,omitempty"`
	Status       string      `json:"status"`
	StartedAt    interface{} `json:"startedAt"`
	CompletedAt  interface{} `json:"completedAt"`
	StartedBy    interface{} `json:"startedBy"`
	Result       interface{} `json:"result"`
	Log          interface{} `json:"log,omitempty"`
}

func toMaintenanceOperationResponse(op store.MaintenanceOperation) maintenanceOperationResponse {
	resp := maintenanceOperationResponse{
		ID:          op.ID,
		Key:         op.Key,
		Title:       op.Title,
		Description: op.Description,
		Category:    op.Category,
		Status:      op.Status,
		CreatedAt:   op.CreatedAt,
	}
	if op.StartedAt != nil {
		resp.StartedAt = op.StartedAt
	}
	if op.CompletedAt != nil {
		resp.CompletedAt = op.CompletedAt
	}
	if op.StartedBy != "" {
		resp.StartedBy = op.StartedBy
	}
	if op.Result != "" {
		resp.Result = op.Result
	}
	return resp
}

func toMaintenanceRunResponse(run store.MaintenanceOperationRun) maintenanceRunResponse {
	resp := maintenanceRunResponse{
		ID:           run.ID,
		OperationKey: run.OperationKey,
		Status:       run.Status,
		StartedAt:    run.StartedAt,
	}
	if run.CompletedAt != nil {
		resp.CompletedAt = run.CompletedAt
	}
	if run.StartedBy != "" {
		resp.StartedBy = run.StartedBy
	}
	if run.Result != "" {
		resp.Result = run.Result
	}
	if run.Log != "" {
		resp.Log = run.Log
	}
	return resp
}

// handleCheckForUpdates handles POST /api/v1/admin/maintenance/check-updates.
// It fetches from origin and compares the local HEAD against origin/main.
func (s *Server) handleCheckForUpdates(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	mc := s.config.MaintenanceConfig
	if mc.RepoPath == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"No repository path configured; set maintenance.repo_path in settings", nil)
		return
	}

	result, err := CheckForUpdates(r.Context(), mc.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to check for updates: "+err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, result)
}
