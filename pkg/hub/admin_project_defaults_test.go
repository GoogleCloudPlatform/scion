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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// newAdminProjectDefaultsServer creates a minimal Server with an
// OperationalSettings backed by the given fakeHubSettingStore. If store is nil,
// no OperationalSettings is set (simulating file/SQLite mode).
func newAdminProjectDefaultsServer(t *testing.T, store *fakeHubSettingStore) *Server {
	t.Helper()
	srv := &Server{}
	if store != nil {
		ops := NewOperationalSettings(store, emptyKoanf(), emptyKoanf())
		if _, err := ops.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		srv.operationalSettings.Store(ops)
	}
	return srv
}

func adminContext(r *http.Request) *http.Request {
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	return r.WithContext(contextWithIdentity(r.Context(), admin))
}

func nonAdminContext(r *http.Request) *http.Request {
	user := NewAuthenticatedUser("u2", "user@example.com", "User", "member", "cli")
	return r.WithContext(contextWithIdentity(r.Context(), user))
}

// --- HTTP-level tests for handleAdminProjectDefaults ---

func TestHandleAdminProjectDefaults_GetCompiledDefault(t *testing.T) {
	// GET with no DB row returns the compiled default: default_scratchpad=true.
	srv := newAdminProjectDefaultsServer(t, newFakeHubSettingStore())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/project-defaults", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminProjectDefaults(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body opsettings.ProjectDefaultsSettings
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.DefaultScratchpad == nil {
		t.Fatal("expected default_scratchpad to be present")
	}
	if *body.DefaultScratchpad != true {
		t.Errorf("expected default_scratchpad=true (compiled default), got %v", *body.DefaultScratchpad)
	}
}

func TestHandleAdminProjectDefaults_PutAndGet(t *testing.T) {
	// PUT {"default_scratchpad": false} then GET verifies the change persisted.
	srv := newAdminProjectDefaultsServer(t, newFakeHubSettingStore())

	// PUT to disable scratchpad.
	putBody := `{"default_scratchpad": false}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/project-defaults",
		bytes.NewBufferString(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = adminContext(putReq)
	putRR := httptest.NewRecorder()
	srv.handleAdminProjectDefaults(putRR, putReq)

	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}

	// Verify PUT response contains the resolved value.
	var putResp opsettings.ProjectDefaultsSettings
	if err := json.NewDecoder(putRR.Body).Decode(&putResp); err != nil {
		t.Fatalf("failed to decode PUT response: %v", err)
	}
	if putResp.DefaultScratchpad == nil || *putResp.DefaultScratchpad != false {
		t.Errorf("PUT response: expected default_scratchpad=false, got %v", putResp.DefaultScratchpad)
	}

	// GET to verify persistence.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/project-defaults", nil)
	getReq = adminContext(getReq)
	getRR := httptest.NewRecorder()
	srv.handleAdminProjectDefaults(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}

	var getResp opsettings.ProjectDefaultsSettings
	if err := json.NewDecoder(getRR.Body).Decode(&getResp); err != nil {
		t.Fatalf("failed to decode GET response: %v", err)
	}
	if getResp.DefaultScratchpad == nil || *getResp.DefaultScratchpad != false {
		t.Errorf("GET after PUT: expected default_scratchpad=false, got %v", getResp.DefaultScratchpad)
	}
}

func TestHandleAdminProjectDefaults_PutInvalidPayload(t *testing.T) {
	// PUT with a non-boolean value should return 400.
	srv := newAdminProjectDefaultsServer(t, newFakeHubSettingStore())

	payload := `{"default_scratchpad": "yes"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/project-defaults",
		bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminProjectDefaults(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid payload, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAdminProjectDefaults_Forbidden(t *testing.T) {
	// Non-admin user should get 403.
	srv := newAdminProjectDefaultsServer(t, newFakeHubSettingStore())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/project-defaults", nil)
	req = nonAdminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminProjectDefaults(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin should get 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAdminProjectDefaults_Unauthenticated(t *testing.T) {
	// No identity in context should get 403.
	srv := newAdminProjectDefaultsServer(t, newFakeHubSettingStore())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/project-defaults", nil)
	rr := httptest.NewRecorder()
	srv.handleAdminProjectDefaults(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated should get 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAdminProjectDefaults_MethodNotAllowed(t *testing.T) {
	// POST and DELETE should return 405.
	srv := newAdminProjectDefaultsServer(t, newFakeHubSettingStore())

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/admin/project-defaults", nil)
		req = adminContext(req)
		rr := httptest.NewRecorder()
		srv.handleAdminProjectDefaults(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d: %s", method, rr.Code, rr.Body.String())
		}
	}
}

func TestHandleAdminProjectDefaults_PutEmptyDocResolvesDefault(t *testing.T) {
	// PUT {} should return the resolved compiled default (default_scratchpad=true),
	// not an empty object.
	srv := newAdminProjectDefaultsServer(t, newFakeHubSettingStore())

	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/project-defaults",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminProjectDefaults(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp opsettings.ProjectDefaultsSettings
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.DefaultScratchpad == nil {
		t.Fatal("expected default_scratchpad to be present in response")
	}
	if *resp.DefaultScratchpad != true {
		t.Errorf("expected default_scratchpad=true (compiled default), got %v", *resp.DefaultScratchpad)
	}
}

func TestHandleAdminProjectDefaults_FileSQLiteMode_PutNotImplemented(t *testing.T) {
	// In file/SQLite mode (no OperationalSettings), PUT should return 501
	// because there is no persistent storage for this section.
	srv := newAdminProjectDefaultsServer(t, nil) // nil store = file/SQLite mode

	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/project-defaults",
		bytes.NewBufferString(`{"default_scratchpad": false}`))
	req.Header.Set("Content-Type", "application/json")
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminProjectDefaults(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", rr.Code, rr.Body.String())
	}
}
