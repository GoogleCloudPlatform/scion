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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

func createRunWithStatus(t *testing.T, s store.Store, key, status string) *store.MaintenanceOperationRun {
	t.Helper()
	run := &store.MaintenanceOperationRun{ID: api.NewUUID(), OperationKey: key, Status: status, StartedAt: time.Now()}
	if err := s.CreateMaintenanceRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

type restartAPIErrorBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeRestartAPIError(t *testing.T, body []byte) restartAPIErrorBody {
	t.Helper()
	var e restartAPIErrorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body %s: %v", body, err)
	}
	return e
}

// G5: Restart Hub is refused while an update or rebuild is running (the
// window between an update's Activate and its before-restart hook).
func TestHandleAdminRestart_RefusedWhileHubBinaryRunInFlight(t *testing.T) {
	for _, tt := range []struct{ key, status string }{
		{"update-binary", store.MaintenanceStatusRunning},
		{"rebuild-server", store.MaintenanceStatusRunning},
	} {
		t.Run(tt.key+"/"+tt.status, func(t *testing.T) {
			srv, s := newTestServerWithStore(t)
			inst := newTestInstaller(maintenance.StrategySudoInstall)
			srv.config.MaintenanceConfig.Installer = inst
			blocking := createRunWithStatus(t, s, tt.key, tt.status)

			rr := postRestart(t, srv)
			if rr.Code != http.StatusConflict {
				t.Fatalf("response = %d %s, want 409", rr.Code, rr.Body.String())
			}
			e := decodeRestartAPIError(t, rr.Body.Bytes())
			if e.Error.Code != ErrCodeConflict || e.Error.Details["runId"] != blocking.ID ||
				e.Error.Details["operationKey"] != tt.key || e.Error.Details["status"] != tt.status ||
				!strings.Contains(e.Error.Message, "Cannot restart the hub while "+tt.key) {
				t.Fatalf("error = %+v", e.Error)
			}
			if inst.restartCount() != 0 {
				t.Fatal("the hub was asked to restart during an update")
			}
			if srv.hubRestartPending() {
				t.Fatal("a refused restart must not reserve the group")
			}
		})
	}
}

// A "restarting" update already wrote its pending update, so a restart is
// safe; Restart Hub stays the in-UI way out when that update's own restart
// never ends the process.
func TestHandleAdminRestart_AllowedWhileUpdateRestarting(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	inst := newTestInstaller(maintenance.StrategySudoInstall)
	srv.config.MaintenanceConfig.Installer = inst
	createRunWithStatus(t, s, "update-binary", store.MaintenanceStatusRestarting)

	if rr := postRestart(t, srv); rr.Code != http.StatusOK {
		t.Fatalf("response = %d %s, want 200", rr.Code, rr.Body.String())
	}
	if inst.restartCount() != 1 || !srv.hubRestartPending() {
		t.Fatalf("restarts = %d, reserved = %v", inst.restartCount(), srv.hubRestartPending())
	}
}

func TestHandleAdminRestart_AllowedWithoutHubBinaryRunInFlight(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	inst := newTestInstaller(maintenance.StrategySudoInstall)
	srv.config.MaintenanceConfig.Installer = inst
	// Finished runs and runs of other operations do not block.
	createRunWithStatus(t, s, "update-binary", store.MaintenanceStatusCompleted)
	createRunWithStatus(t, s, "rebuild-server", store.MaintenanceStatusFailed)
	createRunWithStatus(t, s, "pull-images", store.MaintenanceStatusRunning)

	if rr := postRestart(t, srv); rr.Code != http.StatusOK {
		t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
	}
	if inst.restartCount() != 1 {
		t.Fatalf("restarts = %d, want 1", inst.restartCount())
	}
}

// Once Restart Hub is accepted, no update or rebuild can start before the
// hub is back, so nothing can be activated in the restart's shadow.
func TestHubRestartReservation_RefusesHubBinaryRuns(t *testing.T) {
	prev := version.Version
	version.Version = "nightly-20261023"
	t.Cleanup(func() { version.Version = prev })
	ctx := context.Background()

	srv, s := newTestServerWithStore(t)
	srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "auto",
		Installer: newTestInstaller(maintenance.StrategySudoInstall)}
	if rr := postRestart(t, srv); rr.Code != http.StatusOK {
		t.Fatalf("restart = %d %s", rr.Code, rr.Body.String())
	}

	rr := postOperationRun(t, srv, "update-binary", `{"params":{"target_version":"nightly-20261024"}}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("update during restart = %d %s, want 409", rr.Code, rr.Body.String())
	}
	if e := decodeRestartAPIError(t, rr.Body.Bytes()); e.Error.Code != ErrCodeConflict ||
		!strings.Contains(e.Error.Message, "hub restart is in progress") {
		t.Fatalf("error = %+v", e.Error)
	}
	rr = postOperationRun(t, srv, "rebuild-server", `{"params":{}}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("rebuild during restart = %d %s, want 409", rr.Code, rr.Body.String())
	}
	if e := decodeRestartAPIError(t, rr.Body.Bytes()); e.Error.Code != ErrCodeConflict ||
		!strings.Contains(e.Error.Message, "hub restart is in progress") {
		t.Fatalf("rebuild error = %+v", e.Error)
	}

	// The scheduled auto update is held back too.
	srv.autoApplyUpdate(ctx, "nightly", newParamRecordingExecutor())
	for _, key := range []string{"update-binary", "rebuild-server"} {
		if runs, _ := s.ListMaintenanceRuns(ctx, key, 5); len(runs) != 0 {
			t.Fatalf("%s run created during a restart: %+v", key, runs)
		}
	}

	// Other operations are not in the group.
	other := &store.MaintenanceOperationRun{ID: api.NewUUID(), OperationKey: "pull-images",
		Status: store.MaintenanceStatusRunning, StartedAt: time.Now()}
	if blocking, err := srv.createExclusiveMaintenanceRun(ctx, other); err != nil || blocking != nil {
		t.Fatalf("pull-images during restart: blocking=%v err=%v", blocking, err)
	}
}

func TestHubRestartReservation_ReleasedOnFailureAndExpires(t *testing.T) {
	ctx := context.Background()
	newUpdateRun := func() *store.MaintenanceOperationRun {
		return &store.MaintenanceOperationRun{ID: api.NewUUID(), OperationKey: "update-binary",
			Status: store.MaintenanceStatusRunning, StartedAt: time.Now()}
	}

	t.Run("failed restart releases the reservation", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		inst := newTestInstaller(maintenance.StrategySudoInstall)
		inst.restartErr = errors.New("sudo: a password is required")
		srv.config.MaintenanceConfig.Installer = inst
		if rr := postRestart(t, srv); rr.Code != http.StatusInternalServerError {
			t.Fatalf("restart = %d %s", rr.Code, rr.Body.String())
		}
		if blocking, err := srv.createExclusiveMaintenanceRun(ctx, newUpdateRun()); err != nil || blocking != nil {
			t.Fatalf("update after a failed restart: blocking=%v err=%v", blocking, err)
		}
	})

	t.Run("a reservation older than the hold expires", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.hubRestartReservedAt.Store(time.Now().Add(-hubRestartHold - time.Second).UnixNano())
		if srv.hubRestartPending() {
			t.Fatal("an expired reservation still blocks")
		}
		if blocking, err := srv.createExclusiveMaintenanceRun(ctx, newUpdateRun()); err != nil || blocking != nil {
			t.Fatalf("update after the hold: blocking=%v err=%v", blocking, err)
		}
	})

	t.Run("an active reservation refuses with errHubRestartPending", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		blocking, stamp, err := srv.reserveHubRestart(ctx)
		if err != nil || blocking != nil || stamp == 0 {
			t.Fatalf("reserve: blocking=%v stamp=%d err=%v", blocking, stamp, err)
		}
		run := newUpdateRun()
		if _, err := srv.createExclusiveMaintenanceRun(ctx, run); !errors.Is(err, errHubRestartPending) {
			t.Fatalf("err = %v, want errHubRestartPending", err)
		}
		if _, err := s.GetMaintenanceRun(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("run created during a restart: %v", err)
		}
		srv.releaseHubRestart(stamp)
		if blocking, err := srv.createExclusiveMaintenanceRun(ctx, newUpdateRun()); err != nil || blocking != nil {
			t.Fatalf("after release: blocking=%v err=%v", blocking, err)
		}
	})

	t.Run("a failed restart does not release a concurrent restart's reservation", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		_, first, err := srv.reserveHubRestart(ctx)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond) // distinct stamps
		_, second, err := srv.reserveHubRestart(ctx)
		if err != nil || second == first {
			t.Fatalf("second reserve: stamp=%d (first %d) err=%v", second, first, err)
		}
		srv.releaseHubRestart(first) // the first restart failed
		if !srv.hubRestartPending() {
			t.Fatal("the first restart's failure released the second restart's reservation")
		}
		if _, err := srv.createExclusiveMaintenanceRun(ctx, newUpdateRun()); !errors.Is(err, errHubRestartPending) {
			t.Fatalf("err = %v, want errHubRestartPending", err)
		}
		srv.releaseHubRestart(second)
		if srv.hubRestartPending() {
			t.Fatal("releasing the current stamp must clear the reservation")
		}
	})
}
