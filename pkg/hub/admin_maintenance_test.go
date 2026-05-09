//go:build !no_sqlite

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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

func newTestServerWithStore(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}
	srv := &Server{
		store:          s,
		maintenanceLog: logging.Subsystem("hub.maintenance"),
	}
	return srv, s
}

func TestListMaintenanceOperations(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Should have both migrations and operations keys.
	if _, ok := body["migrations"]; !ok {
		t.Error("response missing 'migrations' key")
	}
	if _, ok := body["operations"]; !ok {
		t.Error("response missing 'operations' key")
	}
}

func TestListMaintenanceOperations_NonAdmin(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestExecuteMigration_NonAdmin(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/secret-hub-id-migration/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceMigrations(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestExecuteMigration_NotFound(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/nonexistent/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceMigrations(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteMigration_AlreadyCompleted(t *testing.T) {
	srv, s := newTestServerWithStore(t)

	// Mark the migration as completed.
	op, err := s.GetMaintenanceOperation(context.Background(), "secret-hub-id-migration")
	if err != nil {
		t.Fatalf("failed to get operation: %v", err)
	}
	now := time.Now()
	op.Status = store.MaintenanceStatusCompleted
	op.CompletedAt = &now
	if err := s.UpdateMaintenanceOperation(context.Background(), op); err != nil {
		t.Fatalf("failed to update operation: %v", err)
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/secret-hub-id-migration/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceMigrations(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteMigration_AlreadyRunning(t *testing.T) {
	srv, s := newTestServerWithStore(t)

	// Mark the migration as running.
	op, err := s.GetMaintenanceOperation(context.Background(), "secret-hub-id-migration")
	if err != nil {
		t.Fatalf("failed to get operation: %v", err)
	}
	now := time.Now()
	op.Status = store.MaintenanceStatusRunning
	op.StartedAt = &now
	if err := s.UpdateMaintenanceOperation(context.Background(), op); err != nil {
		t.Fatalf("failed to update operation: %v", err)
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/secret-hub-id-migration/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceMigrations(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteMigration_NoSecretBackend(t *testing.T) {
	srv, _ := newTestServerWithStore(t)
	// No secret backend configured → should return error.

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"params":{"dryRun":true}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/secret-hub-id-migration/run",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceMigrations(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteMigration_OperationNotMigration(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	// Try to run a routine operation through the migrations endpoint.
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/pull-images/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceMigrations(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteMigration_MethodNotAllowed(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/migrations/secret-hub-id-migration/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceMigrations(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteMigration_InvalidPath(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

	// Missing /run suffix
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/secret-hub-id-migration", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceMigrations(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing /run, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Phase 3: Operation execution tests
// ────────────────────────────────────────────────────────────────────────────

func TestExecuteOperation_NonAdmin(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/operations/pull-images/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestExecuteOperation_NotFound(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/operations/nonexistent/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteOperation_MigrationNotOperation(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	// Try to run a migration through the operations endpoint.
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/operations/secret-hub-id-migration/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteOperation_MethodNotAllowed(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations/pull-images/run", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteOperation_Success(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	// No real runtime configured — the pull-images executor will fail
	// when actually trying to pull, but we can at least verify the API
	// creates a run record and returns 200 with a runId.

	// The pull-images executor will fail because no registry is configured,
	// but the API itself should succeed (async execution).
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/operations/pull-images/run",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	runID, ok := resp["runId"].(string)
	if !ok || runID == "" {
		t.Fatal("response missing runId")
	}
	if resp["status"] != "running" {
		t.Fatalf("expected status=running, got %v", resp["status"])
	}

	// Wait briefly for the async executor to complete.
	time.Sleep(200 * time.Millisecond)

	// Verify run record was created.
	run, err := s.GetMaintenanceRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("failed to get run: %v", err)
	}
	if run.OperationKey != "pull-images" {
		t.Errorf("expected operationKey=pull-images, got %s", run.OperationKey)
	}
	if run.StartedBy != "admin@example.com" {
		t.Errorf("expected startedBy=admin@example.com, got %s", run.StartedBy)
	}
}

func TestListOperationRuns(t *testing.T) {
	srv, s := newTestServerWithStore(t)

	// Create a couple of run records.
	now := time.Now()
	completed := time.Now().Add(10 * time.Second)
	for i, status := range []string{"completed", "failed"} {
		run := &store.MaintenanceOperationRun{
			ID:           fmt.Sprintf("run-%d", i),
			OperationKey: "pull-images",
			Status:       status,
			StartedAt:    now,
			CompletedAt:  &completed,
			StartedBy:    "admin@example.com",
			Log:          fmt.Sprintf("log for run %d", i),
		}
		if err := s.CreateMaintenanceRun(context.Background(), run); err != nil {
			t.Fatalf("failed to create run: %v", err)
		}
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations/pull-images/runs", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	var runs []map[string]interface{}
	if err := json.Unmarshal(body["runs"], &runs); err != nil {
		t.Fatalf("invalid runs JSON: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
}

func TestListOperationRuns_NotFound(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations/nonexistent/runs", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetOperationRun(t *testing.T) {
	srv, s := newTestServerWithStore(t)

	now := time.Now()
	completed := now.Add(10 * time.Second)
	run := &store.MaintenanceOperationRun{
		ID:           "run-detail-1",
		OperationKey: "pull-images",
		Status:       "completed",
		StartedAt:    now,
		CompletedAt:  &completed,
		StartedBy:    "admin@example.com",
		Log:          "Pulling images...\nDone.",
	}
	if err := s.CreateMaintenanceRun(context.Background(), run); err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations/pull-images/runs/run-detail-1", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["id"] != "run-detail-1" {
		t.Errorf("expected id=run-detail-1, got %v", resp["id"])
	}
	if resp["log"] != "Pulling images...\nDone." {
		t.Errorf("unexpected log: %v", resp["log"])
	}
}

func TestGetOperationRun_NotFound(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations/pull-images/runs/nonexistent", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestParseMigrationParams(t *testing.T) {
	tests := []struct {
		name    string
		body    map[string]interface{}
		wantDry string
	}{
		{
			name:    "empty",
			body:    nil,
			wantDry: "",
		},
		{
			name:    "dryRun true",
			body:    map[string]interface{}{"params": map[string]interface{}{"dryRun": true}},
			wantDry: "true",
		},
		{
			name:    "dryRun false",
			body:    map[string]interface{}{"params": map[string]interface{}{"dryRun": false}},
			wantDry: "",
		},
		{
			name:    "no params key",
			body:    map[string]interface{}{"other": "value"},
			wantDry: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := parseMigrationParams(tt.body)
			if got := params["dryRun"]; got != tt.wantDry {
				t.Errorf("parseMigrationParams() dryRun = %q, want %q", got, tt.wantDry)
			}
		})
	}
}

func TestCheckForUpdates_NoRepoPath(t *testing.T) {
	srv, _ := newTestServerWithStore(t)
	// No RepoPath configured — should return 400.
	srv.config.MaintenanceConfig = MaintenanceConfig{}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/check-updates", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleCheckForUpdates(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCheckForUpdates_WrongMethod(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/check-updates", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleCheckForUpdates(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCheckForUpdates_NonAdmin(t *testing.T) {
	srv, _ := newTestServerWithStore(t)

	viewer := NewAuthenticatedUser("u1", "viewer@example.com", "Viewer", "viewer", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/check-updates", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), viewer))
	rr := httptest.NewRecorder()
	srv.handleCheckForUpdates(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListMaintenanceOperations_HidesContainerBinariesByDefault(t *testing.T) {
	srv, _ := newTestServerWithStore(t)
	t.Setenv("SCION_DEV_BINARIES", "")

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Operations []struct {
			Key string `json:"key"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	for _, op := range body.Operations {
		if op.Key == "rebuild-container-binaries" {
			t.Error("rebuild-container-binaries should be hidden when SCION_DEV_BINARIES is not set")
		}
	}
}

func TestListMaintenanceOperations_ShowsContainerBinariesWhenEnvSet(t *testing.T) {
	srv, _ := newTestServerWithStore(t)
	t.Setenv("SCION_DEV_BINARIES", "/tmp/test-binaries")

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Operations []struct {
			Key string `json:"key"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	found := false
	for _, op := range body.Operations {
		if op.Key == "rebuild-container-binaries" {
			found = true
			break
		}
	}
	if !found {
		t.Error("rebuild-container-binaries should be visible when SCION_DEV_BINARIES is set")
	}
}
