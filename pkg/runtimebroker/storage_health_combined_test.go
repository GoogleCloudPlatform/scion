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
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestGetHealthInfo_StorageInvalidAndOverridesBothReported: with invalid
// project storage settings AND removed storage overrides, /healthz reports
// both checks with their fixed values and a degraded (not unhealthy)
// status, the heartbeat keeps both, and a create is refused with
// storage_invalid (that refusal runs first).
func TestGetHealthInfo_StorageInvalidAndOverridesBothReported(t *testing.T) {
	old := storageOverridesCacheTTL
	storageOverridesCacheTTL = 0
	t.Cleanup(func() { storageOverridesCacheTTL = old })

	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	srv.config.StorageError = invalidStorageErr()
	srv.config.StorageOverridesCheck = func() error { return errors.New("removed overrides") }

	info := srv.GetHealthInfo(context.Background())
	if info.Status != api.BrokerHealthDegraded {
		t.Errorf("status = %q, want %q", info.Status, api.BrokerHealthDegraded)
	}
	if got := info.Checks[healthCheckProjectStorage]; got != api.BrokerHealthUnavailable {
		t.Errorf("%s = %q, want %q", healthCheckProjectStorage, got, api.BrokerHealthUnavailable)
	}
	if got := info.Checks[storageOverridesCheck]; got != storageOverridesCheckDegraded {
		t.Errorf("%s = %q, want %q", storageOverridesCheck, got, storageOverridesCheckDegraded)
	}

	report := srv.heartbeatHealthReport(context.Background())
	if report == nil {
		t.Fatal("no heartbeat health report")
	}
	if report.Status != api.BrokerHealthDegraded {
		t.Errorf("heartbeat status = %q, want %q", report.Status, api.BrokerHealthDegraded)
	}
	if got := report.Checks[healthCheckProjectStorage]; got != api.BrokerHealthUnavailable {
		t.Errorf("heartbeat %s = %q, want %q", healthCheckProjectStorage, got, api.BrokerHealthUnavailable)
	}
	if got := report.Checks[storageOverridesCheck]; got != api.BrokerHealthDegraded {
		t.Errorf("heartbeat %s = %q, want %q", storageOverridesCheck, got, api.BrokerHealthDegraded)
	}

	w := postCreate(t, srv, map[string]any{"name": "agent-both", "config": map[string]any{"template": "claude"}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != api.BrokerErrCodeStorageInvalid {
		t.Errorf("create error code = %q, want %q", resp.Error.Code, api.BrokerErrCodeStorageInvalid)
	}
	if mgr.StartCallCount() != 0 || mgr.PreflightCallCount() != 0 {
		t.Error("the refused create reached the manager")
	}
}
