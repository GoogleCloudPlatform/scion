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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// invalidStorageErr is a startup storage error as the server command
// builds it from config.ResolveProjectStorage.
func invalidStorageErr() error {
	return errors.Join(errors.New("invalid project storage settings"), config.FieldErrorList{{
		Path:  "server.project_storage.nfs.shares[0].id",
		Rule:  "must be a single path element (no path separator)",
		Value: "../x",
	}})
}

// TestStorageInvalid_RefusesStartsKeepsStopDelete: a broker started with
// invalid project storage settings refuses create, start and restart with
// 422 storage_invalid naming the field (not its value), before the
// manager is reached; stop, delete and logs are still served.
func TestStorageInvalid_RefusesStartsKeepsStopDelete(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	srv.config.StorageError = invalidStorageErr()

	serve := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	assertRefused := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
		}
		var resp ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Error.Code != api.BrokerErrCodeStorageInvalid {
			t.Fatalf("error code = %q, want %q", resp.Error.Code, api.BrokerErrCodeStorageInvalid)
		}
		if !strings.Contains(resp.Error.Message, "server.project_storage.nfs.shares[0].id") {
			t.Errorf("message %q does not name the field", resp.Error.Message)
		}
		if strings.Contains(resp.Error.Message, "../x") {
			t.Errorf("message %q echoes the configured value", resp.Error.Message)
		}
	}

	t.Run("create sync and async", func(t *testing.T) {
		assertRefused(t, postCreate(t, srv, map[string]any{"name": "agent-s1", "config": map[string]any{"template": "claude"}}))
		assertRefused(t, postCreate(t, srv, map[string]any{
			"name": "agent-s2", "requestId": "req-s2", "asyncLaunch": true, "launchId": "L-s2",
			"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
		}))
		assertRefused(t, postCreate(t, srv, map[string]any{"name": "agent-s5", "provisionOnly": true, "config": map[string]any{"template": "claude"}}))
	})
	t.Run("start", func(t *testing.T) {
		assertRefused(t, serve(http.MethodPost, "/api/v1/agents/agent-s3/start", `{}`))
	})
	t.Run("restart", func(t *testing.T) {
		assertRefused(t, serve(http.MethodPost, "/api/v1/agents/agent-s4/restart", `{}`))
	})
	if mgr.StartCallCount() != 0 || mgr.PreflightCallCount() != 0 {
		t.Fatal("a refused request reached the manager")
	}
	if len(srv.startsInFlight.keys()) != 0 {
		t.Fatalf("a refused request left a tracker entry: %v", srv.startsInFlight.keys())
	}
	if !registryEmpty(srv) {
		t.Fatal("a refused request left a registry record")
	}

	for _, tc := range []struct{ name, method, path string }{
		{"stop", http.MethodPost, "/api/v1/agents/agent-s3/stop"},
		{"delete", http.MethodDelete, "/api/v1/agents/agent-s3"},
		{"logs", http.MethodGet, "/api/v1/agents/agent-s3/logs"},
	} {
		t.Run(tc.name+" still served", func(t *testing.T) {
			w := serve(tc.method, tc.path, `{}`)
			if w.Code == http.StatusUnprocessableEntity || strings.Contains(w.Body.String(), api.BrokerErrCodeStorageInvalid) {
				t.Fatalf("%s refused for invalid storage: %d %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestStorageInvalid_HealthAndDescriptor: the broker reports degraded
// health with a fixed project_storage check value, and a workspace storage
// descriptor marked invalid with no placement (never the local layout).
func TestStorageInvalid_HealthAndDescriptor(t *testing.T) {
	srv := newTestServerWithManager(t, newAsyncManager())
	srv.config.WorkspaceStorageBackend = "nfs"

	healthy := srv.GetHealthInfo(context.Background())
	if _, ok := healthy.Checks[healthCheckProjectStorage]; ok {
		t.Fatalf("valid storage reports a %s check: %v", healthCheckProjectStorage, healthy.Checks)
	}

	srv.config.StorageError = invalidStorageErr()
	info := srv.GetHealthInfo(context.Background())
	if info.Status == "healthy" {
		t.Errorf("status = %q, want degraded", info.Status)
	}
	if got := info.Checks[healthCheckProjectStorage]; got != api.BrokerHealthUnavailable {
		t.Errorf("%s check = %q, want %q", healthCheckProjectStorage, got, api.BrokerHealthUnavailable)
	}
	if report := srv.heartbeatHealthReport(context.Background()); report == nil || report.Checks[healthCheckProjectStorage] != api.BrokerHealthUnavailable {
		t.Errorf("heartbeat report = %+v, want %s unavailable", report, healthCheckProjectStorage)
	}

	desc := srv.workspaceStorageDescriptor()
	want := api.BrokerWorkspaceStorage{Backend: "nfs", Invalid: true}
	if desc == nil || *desc != want {
		t.Errorf("descriptor = %+v, want %+v", desc, want)
	}
}

func TestStorageInvalidMessage(t *testing.T) {
	if got := storageInvalidMessage(errors.New(`plain "10.0.0.2"`)); got != "server.project_storage: invalid; ask a hub admin; the hub log has the details" {
		t.Errorf("plain error: %q", got)
	}
	got := storageInvalidMessage(config.FieldErrorList{
		{Path: "a.b", Rule: "must be set"},
		{Path: "a.c", Rule: "must be a clean path (use \"/x\")", Value: "/x/"},
	})
	if got != "a.b: invalid; a.c: invalid; ask a hub admin; the hub log has the details" {
		t.Errorf("field errors: %q", got)
	}
}
