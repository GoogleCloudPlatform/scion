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

// newAdminMessagingServer creates a minimal Server with an OperationalSettings
// backed by the given fakeHubSettingStore. If store is nil, no
// OperationalSettings is set (simulating file/SQLite mode).
func newAdminMessagingServer(t *testing.T, store *fakeHubSettingStore) *Server {
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

// --- HTTP-level tests for handleAdminMessaging ---

func TestHandleAdminMessaging_GetAbsentRow(t *testing.T) {
	// GET with no DB row returns compiled defaults: both switches OFF.
	srv := newAdminMessagingServer(t, newFakeHubSettingStore())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body opsettings.MessagingSettings
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ConversationReadSwitch == nil || *body.ConversationReadSwitch != false {
		t.Errorf("expected conversation_read_switch=false (compiled default), got %v", body.ConversationReadSwitch)
	}
	if body.ConversationWriteDenySwitch == nil || *body.ConversationWriteDenySwitch != false {
		t.Errorf("expected conversation_write_deny_switch=false (compiled default), got %v", body.ConversationWriteDenySwitch)
	}
}

func TestHandleAdminMessaging_GetEmptyRow(t *testing.T) {
	// An empty JSON doc `{}` in the DB row → both switches read false.
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`{}`))
	srv := newAdminMessagingServer(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body opsettings.MessagingSettings
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ConversationReadSwitch == nil || *body.ConversationReadSwitch != false {
		t.Errorf("expected conversation_read_switch=false (empty doc default), got %v", body.ConversationReadSwitch)
	}
	if body.ConversationWriteDenySwitch == nil || *body.ConversationWriteDenySwitch != false {
		t.Errorf("expected conversation_write_deny_switch=false (empty doc default), got %v", body.ConversationWriteDenySwitch)
	}
}

func TestHandleAdminMessaging_GetMalformedRow(t *testing.T) {
	// Malformed JSON in the DB row → both switches read false, no panic.
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`not valid json`))
	srv := newAdminMessagingServer(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body opsettings.MessagingSettings
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ConversationReadSwitch == nil || *body.ConversationReadSwitch != false {
		t.Errorf("expected conversation_read_switch=false (malformed fallback), got %v", body.ConversationReadSwitch)
	}
	if body.ConversationWriteDenySwitch == nil || *body.ConversationWriteDenySwitch != false {
		t.Errorf("expected conversation_write_deny_switch=false (malformed fallback), got %v", body.ConversationWriteDenySwitch)
	}
}

func TestHandleAdminMessaging_GetExplicitlyFalse(t *testing.T) {
	// Explicitly false values in the DB row → both switches read false.
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`{"conversation_read_switch":false,"conversation_write_deny_switch":false}`))
	srv := newAdminMessagingServer(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body opsettings.MessagingSettings
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ConversationReadSwitch == nil || *body.ConversationReadSwitch != false {
		t.Errorf("expected conversation_read_switch=false, got %v", body.ConversationReadSwitch)
	}
	if body.ConversationWriteDenySwitch == nil || *body.ConversationWriteDenySwitch != false {
		t.Errorf("expected conversation_write_deny_switch=false, got %v", body.ConversationWriteDenySwitch)
	}
}

func TestHandleAdminMessaging_PutOneSwitchUnchangesOther(t *testing.T) {
	// PUT one switch, the other is unchanged.
	srv := newAdminMessagingServer(t, newFakeHubSettingStore())

	// PUT only conversation_read_switch=true.
	putBody := `{"conversation_read_switch": true}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = adminContext(putReq)
	putRR := httptest.NewRecorder()
	srv.handleAdminMessaging(putRR, putReq)

	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}

	var putResp opsettings.MessagingSettings
	if err := json.NewDecoder(putRR.Body).Decode(&putResp); err != nil {
		t.Fatalf("failed to decode PUT response: %v", err)
	}
	if putResp.ConversationReadSwitch == nil || *putResp.ConversationReadSwitch != true {
		t.Errorf("PUT response: expected conversation_read_switch=true, got %v", putResp.ConversationReadSwitch)
	}
	if putResp.ConversationWriteDenySwitch == nil || *putResp.ConversationWriteDenySwitch != false {
		t.Errorf("PUT response: expected conversation_write_deny_switch=false (unchanged), got %v", putResp.ConversationWriteDenySwitch)
	}

	// GET to verify persistence.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	getReq = adminContext(getReq)
	getRR := httptest.NewRecorder()
	srv.handleAdminMessaging(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}

	var getResp opsettings.MessagingSettings
	if err := json.NewDecoder(getRR.Body).Decode(&getResp); err != nil {
		t.Fatalf("failed to decode GET response: %v", err)
	}
	if getResp.ConversationReadSwitch == nil || *getResp.ConversationReadSwitch != true {
		t.Errorf("GET after PUT: expected conversation_read_switch=true, got %v", getResp.ConversationReadSwitch)
	}
	if getResp.ConversationWriteDenySwitch == nil || *getResp.ConversationWriteDenySwitch != false {
		t.Errorf("GET after PUT: expected conversation_write_deny_switch=false (unchanged), got %v", getResp.ConversationWriteDenySwitch)
	}
}

func TestHandleAdminMessaging_PutWriteDenySwitchPreservesReadSwitch(t *testing.T) {
	// Start with read_switch=true, then PUT only write_deny_switch=true.
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`{"conversation_read_switch":true}`))
	srv := newAdminMessagingServer(t, store)

	putBody := `{"conversation_write_deny_switch": true}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = adminContext(putReq)
	putRR := httptest.NewRecorder()
	srv.handleAdminMessaging(putRR, putReq)

	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}

	var resp opsettings.MessagingSettings
	if err := json.NewDecoder(putRR.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ConversationReadSwitch == nil || *resp.ConversationReadSwitch != true {
		t.Errorf("expected conversation_read_switch=true (preserved), got %v", resp.ConversationReadSwitch)
	}
	if resp.ConversationWriteDenySwitch == nil || *resp.ConversationWriteDenySwitch != true {
		t.Errorf("expected conversation_write_deny_switch=true, got %v", resp.ConversationWriteDenySwitch)
	}
}

func TestHandleAdminMessaging_PutEmptyDocPreserves(t *testing.T) {
	// PUT {} should preserve existing values (presence-aware: no fields sent).
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`{"conversation_read_switch":true,"conversation_write_deny_switch":true}`))
	srv := newAdminMessagingServer(t, store)

	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(`{}`))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = adminContext(putReq)
	putRR := httptest.NewRecorder()
	srv.handleAdminMessaging(putRR, putReq)

	if putRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}

	var resp opsettings.MessagingSettings
	if err := json.NewDecoder(putRR.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ConversationReadSwitch == nil || *resp.ConversationReadSwitch != true {
		t.Errorf("expected conversation_read_switch=true (preserved), got %v", resp.ConversationReadSwitch)
	}
	if resp.ConversationWriteDenySwitch == nil || *resp.ConversationWriteDenySwitch != true {
		t.Errorf("expected conversation_write_deny_switch=true (preserved), got %v", resp.ConversationWriteDenySwitch)
	}
}

func TestHandleAdminMessaging_PutBothSwitches(t *testing.T) {
	// PUT both switches and verify.
	srv := newAdminMessagingServer(t, newFakeHubSettingStore())

	putBody := `{"conversation_read_switch": true, "conversation_write_deny_switch": true}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = adminContext(putReq)
	putRR := httptest.NewRecorder()
	srv.handleAdminMessaging(putRR, putReq)

	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}

	var resp opsettings.MessagingSettings
	if err := json.NewDecoder(putRR.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ConversationReadSwitch == nil || *resp.ConversationReadSwitch != true {
		t.Errorf("expected conversation_read_switch=true, got %v", resp.ConversationReadSwitch)
	}
	if resp.ConversationWriteDenySwitch == nil || *resp.ConversationWriteDenySwitch != true {
		t.Errorf("expected conversation_write_deny_switch=true, got %v", resp.ConversationWriteDenySwitch)
	}
}

func TestHandleAdminMessaging_PutInvalidPayload(t *testing.T) {
	// PUT with a non-boolean value should return 400.
	srv := newAdminMessagingServer(t, newFakeHubSettingStore())

	payload := `{"conversation_read_switch": "yes"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid payload, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAdminMessaging_MethodNotAllowed(t *testing.T) {
	// DELETE should return 405.
	srv := newAdminMessagingServer(t, newFakeHubSettingStore())

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/messaging", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAdminMessaging_FileSQLiteMode_PutNotImplemented(t *testing.T) {
	// In file/SQLite mode (no OperationalSettings), PUT should return 501.
	srv := newAdminMessagingServer(t, nil) // nil store = file/SQLite mode

	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(`{"conversation_read_switch": true}`))
	req.Header.Set("Content-Type", "application/json")
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAdminMessaging_PutRecordsUpdatedBy(t *testing.T) {
	// PUT should record the caller's email in updated_by.
	store := newFakeHubSettingStore()
	srv := newAdminMessagingServer(t, store)

	putBody := `{"conversation_read_switch": true}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = adminContext(putReq)
	putRR := httptest.NewRecorder()
	srv.handleAdminMessaging(putRR, putReq)

	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}

	// Verify the store received the correct updated_by.
	store.mu.Lock()
	defer store.mu.Unlock()
	hs, ok := store.settings["messaging"]
	if !ok {
		t.Fatal("messaging setting not found in store after PUT")
	}
	if hs.UpdatedBy != "admin@example.com" {
		t.Errorf("expected updated_by='admin@example.com', got %q", hs.UpdatedBy)
	}
	if hs.Origin != "managed" {
		t.Errorf("expected origin='managed', got %q", hs.Origin)
	}
	if hs.Revision < 1 {
		t.Errorf("expected revision >= 1, got %d", hs.Revision)
	}
}

// NOTE: Auth gating for handleAdminMessaging (non-admin and unauthenticated
// rejection) is enforced by routeGuard via the hub.messaging.update Permission
// metadata. The handler no longer performs inline admin checks. Authorization
// for admin endpoints is tested in TestRouteGuardOpsPermissions. We verify the
// route metadata entry exists below.

func TestAdminMessagingRouteMetadataExists(t *testing.T) {
	// Verify that the route metadata entry exists for admin messaging.
	meta, ok := routeMetadataTable["/api/v1/admin/messaging"]
	if !ok {
		t.Fatal("route metadata entry for /api/v1/admin/messaging not found")
	}
	if meta.Classification != RouteHubAdmin {
		t.Errorf("expected RouteHubAdmin classification, got %v", meta.Classification)
	}
}
