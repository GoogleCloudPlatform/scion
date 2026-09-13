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
	// GET with no DB row returns compiled default: switch ON (Phase 9a).
	srv := newAdminMessagingServer(t, newFakeHubSettingStore())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body messagingResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ConversationEnvelopeSwitch == nil || *body.ConversationEnvelopeSwitch != true {
		t.Errorf("expected conversation_envelope_switch=true (compiled default ON), got %v", body.ConversationEnvelopeSwitch)
	}
}

func TestHandleAdminMessaging_GetEmptyRow(t *testing.T) {
	// An empty JSON doc `{}` in the DB row → switch ON (key omitted → default).
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

	var body messagingResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ConversationEnvelopeSwitch == nil || *body.ConversationEnvelopeSwitch != true {
		t.Errorf("expected conversation_envelope_switch=true (empty doc → omitted → default ON), got %v", body.ConversationEnvelopeSwitch)
	}
}

func TestHandleAdminMessaging_GetMalformedRow(t *testing.T) {
	// Malformed JSON in the DB row → switch OFF (fail-closed, DEF-92).
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

	var body messagingResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ConversationEnvelopeSwitch == nil || *body.ConversationEnvelopeSwitch != false {
		t.Errorf("expected conversation_envelope_switch=false (malformed → fail-closed), got %v", body.ConversationEnvelopeSwitch)
	}
}

func TestHandleAdminMessaging_GetExplicitlyFalse(t *testing.T) {
	// Explicitly false value → switch OFF.
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":false}`))
	srv := newAdminMessagingServer(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body messagingResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ConversationEnvelopeSwitch == nil || *body.ConversationEnvelopeSwitch != false {
		t.Errorf("expected conversation_envelope_switch=false, got %v", body.ConversationEnvelopeSwitch)
	}
}

func TestHandleAdminMessaging_GetExplicitlyTrue(t *testing.T) {
	// Explicitly true value → switch ON.
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":true}`))
	srv := newAdminMessagingServer(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body messagingResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ConversationEnvelopeSwitch == nil || *body.ConversationEnvelopeSwitch != true {
		t.Errorf("expected conversation_envelope_switch=true, got %v", body.ConversationEnvelopeSwitch)
	}
}

func TestHandleAdminMessaging_PutSwitch(t *testing.T) {
	// PUT the switch to false and verify.
	srv := newAdminMessagingServer(t, newFakeHubSettingStore())

	putBody := `{"conversation_envelope_switch": false}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = adminContext(putReq)
	putRR := httptest.NewRecorder()
	srv.handleAdminMessaging(putRR, putReq)

	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}

	var putResp messagingResponse
	if err := json.NewDecoder(putRR.Body).Decode(&putResp); err != nil {
		t.Fatalf("failed to decode PUT response: %v", err)
	}
	if putResp.ConversationEnvelopeSwitch == nil || *putResp.ConversationEnvelopeSwitch != false {
		t.Errorf("PUT response: expected conversation_envelope_switch=false, got %v", putResp.ConversationEnvelopeSwitch)
	}

	// GET to verify persistence.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	getReq = adminContext(getReq)
	getRR := httptest.NewRecorder()
	srv.handleAdminMessaging(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}

	var getResp messagingResponse
	if err := json.NewDecoder(getRR.Body).Decode(&getResp); err != nil {
		t.Fatalf("failed to decode GET response: %v", err)
	}
	if getResp.ConversationEnvelopeSwitch == nil || *getResp.ConversationEnvelopeSwitch != false {
		t.Errorf("GET after PUT: expected conversation_envelope_switch=false, got %v", getResp.ConversationEnvelopeSwitch)
	}
}

func TestHandleAdminMessaging_PutEmptyDocPreserves(t *testing.T) {
	// PUT {} should preserve existing value (presence-aware: no fields sent).
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":false}`))
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

	var resp messagingResponse
	if err := json.NewDecoder(putRR.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ConversationEnvelopeSwitch == nil || *resp.ConversationEnvelopeSwitch != false {
		t.Errorf("expected conversation_envelope_switch=false (preserved), got %v", resp.ConversationEnvelopeSwitch)
	}
}

func TestHandleAdminMessaging_PutInvalidPayload(t *testing.T) {
	// PUT with a non-boolean value should return 400.
	srv := newAdminMessagingServer(t, newFakeHubSettingStore())

	payload := `{"conversation_envelope_switch": "yes"}`
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
		bytes.NewBufferString(`{"conversation_envelope_switch": true}`))
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

	putBody := `{"conversation_envelope_switch": false}`
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

func TestHandleAdminMessaging_GetNilOperationalSettings(t *testing.T) {
	// GET with nil OperationalSettings (init failed) → switch OFF.
	// Enforcement sites read `ops != nil && ops.ConversationEnvelopeSwitch()`,
	// which yields false when ops is nil. The GET must report the same value
	// enforcement would actually use — not the compiled default, which is
	// unreachable when there is no OperationalSettings to evaluate it.
	srv := newAdminMessagingServer(t, nil) // nil store = no OperationalSettings

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messaging", nil)
	req = adminContext(req)
	rr := httptest.NewRecorder()
	srv.handleAdminMessaging(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body messagingResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	// nil ops → enforcement yields false → GET reports false.
	if body.ConversationEnvelopeSwitch == nil || *body.ConversationEnvelopeSwitch != false {
		t.Errorf("expected conversation_envelope_switch=false (nil ops → enforcement OFF), got %v", body.ConversationEnvelopeSwitch)
	}
}

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

// --- AC-9-7c: after one PUT, stored document contains new key and no stale keys ---

func TestHandleAdminMessaging_AC97c_PutCleansStaleKeys(t *testing.T) {
	// Start with a row containing ONLY stale keys (pre-upgrade state).
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`{"conversation_read_switch":false,"conversation_write_deny_switch":false}`))
	srv := newAdminMessagingServer(t, store)

	// PUT the new switch to false (any explicit value triggers a write).
	putBody := `{"conversation_envelope_switch": false}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = adminContext(putReq)
	putRR := httptest.NewRecorder()
	srv.handleAdminMessaging(putRR, putReq)

	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}

	// Inspect the raw stored document.
	store.mu.Lock()
	defer store.mu.Unlock()
	hs, ok := store.settings["messaging"]
	if !ok {
		t.Fatal("messaging setting not found in store after PUT")
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(hs.Value, &raw); err != nil {
		t.Fatalf("failed to unmarshal stored doc: %v", err)
	}

	// New key must be present.
	if _, exists := raw["conversation_envelope_switch"]; !exists {
		t.Error("stored document missing conversation_envelope_switch after PUT")
	}

	// Stale keys must be absent (self-cleaning).
	if _, exists := raw["conversation_read_switch"]; exists {
		t.Error("stored document still contains stale key conversation_read_switch after PUT")
	}
	if _, exists := raw["conversation_write_deny_switch"]; exists {
		t.Error("stored document still contains stale key conversation_write_deny_switch after PUT")
	}
}

// --- AC-9-7d: explicit-null reset returns switch to compiled default (ON) ---

func TestHandleAdminMessaging_AC97d_NullResetReturnsDefault(t *testing.T) {
	// Start with switch explicitly false.
	store := newFakeHubSettingStore()
	store.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":false}`))
	srv := newAdminMessagingServer(t, store)

	// PUT with explicit null for the switch.
	putBody := `{"conversation_envelope_switch": null}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/messaging",
		bytes.NewBufferString(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = adminContext(putReq)
	putRR := httptest.NewRecorder()
	srv.handleAdminMessaging(putRR, putReq)

	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}

	var resp messagingResponse
	if err := json.NewDecoder(putRR.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	// The null reset must return the compiled default: ON.
	if resp.ConversationEnvelopeSwitch == nil || *resp.ConversationEnvelopeSwitch != true {
		t.Errorf("expected conversation_envelope_switch=true (null reset → compiled default ON), got %v", resp.ConversationEnvelopeSwitch)
	}

	// Verify the section was deleted from the store (absent → default path).
	store.mu.Lock()
	_, exists := store.settings["messaging"]
	store.mu.Unlock()
	if exists {
		t.Error("expected messaging section to be deleted after null reset, but it still exists")
	}
}
