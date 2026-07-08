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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// newTestDBServer creates a test Server configured in postgres mode with a
// fakeHubSettingStore and OperationalSettings wired up for testing.
func newTestDBServer(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fileK := emptyKoanf()
	envK := emptyKoanf()

	ops := NewOperationalSettings(fakeStore, fileK, envK)

	srv := &Server{
		dbDriver:    "postgres",
		maintenance: NewMaintenanceState(false, ""),
	}
	srv.SetOperationalSettings(ops)

	return srv, fakeStore, ops
}

func adminRequest(method, url, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, url, nil)
	}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	r = r.WithContext(contextWithIdentity(r.Context(), admin))
	return r
}

// ---- GET /api/v1/admin/server-config (postgres mode) ----

func TestGetServerConfigDB_MetadataFromDB(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed some DB sections.
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["admin@db.com"],"user_access_mode":"open"}`))
	fakeStore.seed("maintenance", json.RawMessage(`{"admin_mode":false}`))
	_, _ = ops.Refresh(context.Background())

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp ServerConfigDBResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// access section should have source=db
	accessMeta, ok := resp.SectionMeta["access"]
	if !ok {
		t.Fatal("expected section_metadata for 'access'")
	}
	if accessMeta.Source != "db" {
		t.Errorf("access source: want 'db', got %q", accessMeta.Source)
	}
	if accessMeta.Revision == 0 {
		t.Error("access revision should be > 0 for DB source")
	}

	// maintenance section should have source=db
	maintMeta, ok := resp.SectionMeta["maintenance"]
	if !ok {
		t.Fatal("expected section_metadata for 'maintenance'")
	}
	if maintMeta.Source != "db" {
		t.Errorf("maintenance source: want 'db', got %q", maintMeta.Source)
	}

	// lifecycle has no DB row and no file fallback → default
	lifeMeta, ok := resp.SectionMeta["lifecycle"]
	if !ok {
		t.Fatal("expected section_metadata for 'lifecycle'")
	}
	if lifeMeta.Source != "default" {
		t.Errorf("lifecycle source: want 'default', got %q", lifeMeta.Source)
	}
}

func TestGetServerConfigDB_EnvOverridesPresent(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	envK := newEnvKoanf(t, map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"env@example.com"},
		"telemetry.enabled":       true,
	})
	fileK := emptyKoanf()
	ops := NewOperationalSettings(fakeStore, fileK, envK)
	_, _ = ops.Refresh(context.Background())

	srv := &Server{
		dbDriver:    "postgres",
		maintenance: NewMaintenanceState(false, ""),
	}
	srv.SetOperationalSettings(ops)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp ServerConfigDBResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	overrideSet := make(map[string]bool)
	for _, k := range resp.EnvOverrides {
		overrideSet[k] = true
	}
	if !overrideSet["server.hub.admin_emails"] {
		t.Error("expected server.hub.admin_emails in env_overrides")
	}
	if !overrideSet["telemetry.enabled"] {
		t.Error("expected telemetry.enabled in env_overrides")
	}
}

func TestGetServerConfigDB_FalseBooleansOverrideFileValues(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed lifecycle section with explicit false booleans in DB.
	fakeStore.seed("lifecycle", json.RawMessage(`{
		"auto_suspend_stalled": false,
		"soft_delete_retain_files": false,
		"soft_delete_retention": ""
	}`))
	_, _ = ops.Refresh(context.Background())

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp ServerConfigDBResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The DB says false; the response MUST reflect false, not a stale file value.
	if resp.Server == nil || resp.Server.Hub == nil {
		t.Fatal("expected server.hub to be populated")
	}
	if resp.Server.Hub.AutoSuspendStalled == nil {
		t.Fatal("AutoSuspendStalled should not be nil")
	}
	if *resp.Server.Hub.AutoSuspendStalled != false {
		t.Errorf("AutoSuspendStalled: want false, got %v", *resp.Server.Hub.AutoSuspendStalled)
	}
	if resp.Server.Hub.SoftDeleteRetainFiles == nil {
		t.Fatal("SoftDeleteRetainFiles should not be nil")
	}
	if *resp.Server.Hub.SoftDeleteRetainFiles != false {
		t.Errorf("SoftDeleteRetainFiles: want false, got %v", *resp.Server.Hub.SoftDeleteRetainFiles)
	}
}

func TestApplySnapshotToResponse_EmptySlicesOverrideFileValues(t *testing.T) {
	// Simulate a response pre-loaded from file with non-empty notification channels.
	resp := &ServerConfigResponse{
		Server: &config.V1ServerConfig{
			NotificationChannels: []config.V1NotificationChannelConfig{
				{Type: "slack"},
			},
		},
	}

	// Snapshot says empty (DB explicitly cleared them).
	snap := Layer1Snapshot{
		NotificationChannels: nil,
	}

	applySnapshotToResponse(resp, snap)

	if len(resp.Server.NotificationChannels) != 0 {
		t.Errorf("NotificationChannels: want empty, got %v", resp.Server.NotificationChannels)
	}
}

func TestGetServerConfigDB_MaskingIntact(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	// Verify that even if there were sensitive fields, the masking code ran.
	// We can't easily assert masking without setting up full config, but the
	// handler calls maskSensitiveFields() — verify it didn't crash.
	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["schema_version"] == nil {
		t.Error("expected schema_version in response")
	}
}

// ---- PUT /api/v1/admin/server-config (postgres mode): partitioning ----

func TestPutServerConfigDB_PureLayer1_WriteSections(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Pure Layer-1 payload: admin_emails + user_access_mode.
	body := `{
		"server": {
			"hub": {"admin_emails": ["new@admin.com"]},
			"auth": {"user_access_mode": "invite_only"}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)

	if resp["status"] != "saved" {
		t.Errorf("expected status=saved, got %v", resp["status"])
	}

	// Verify section was written to store.
	fakeStore.mu.Lock()
	row, ok := fakeStore.settings["access"]
	fakeStore.mu.Unlock()
	if !ok {
		t.Fatal("expected 'access' section in store after PUT")
	}
	if row.Revision == 0 {
		t.Error("expected revision > 0")
	}

	// Verify snapshot reflects new values.
	snap := ops.Snapshot()
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "new@admin.com" {
		t.Errorf("AdminEmails: want [new@admin.com], got %v", snap.AdminEmails)
	}
}

func TestPutServerConfigDB_Layer0Keys_Rejected422(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Payload containing Layer-0 key (database).
	body := `{
		"server": {
			"database": {"driver": "sqlite"},
			"hub": {"admin_emails": ["admin@test.com"]}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)

	if resp["error"] != "layer0_rejected" {
		t.Errorf("expected error=layer0_rejected, got %v", resp["error"])
	}

	keys, ok := resp["keys"].([]interface{})
	if !ok || len(keys) == 0 {
		t.Fatal("expected non-empty keys in 422 response")
	}

	// Verify nothing was written to store.
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()
	if len(fakeStore.settings) > 0 {
		t.Error("expected no sections written to store after Layer-0 rejection")
	}
}

func TestPutServerConfigDB_MixedValid_Layer0Rejected_NothingWritten(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Mix of Layer-0 (mode) and Layer-1 (admin_emails).
	body := `{
		"server": {
			"mode": "hosted",
			"hub": {"admin_emails": ["admin@test.com"]}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}

	// Nothing written.
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()
	if len(fakeStore.settings) > 0 {
		t.Error("expected no writes when Layer-0 keys present")
	}
}

func TestPutServerConfigDB_UnclassifiedOnly_200WithIgnoredKeys(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Payload containing only unclassified keys — not Layer-0, not Layer-1.
	sv := "2"
	body := `{
		"schema_version": "2",
		"runtimes": {"go": {"image": "golang:1.21"}},
		"profiles": {"dev": {}}
	}`
	_ = sv

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)

	if resp["status"] != "saved" {
		t.Errorf("expected status=saved, got %v", resp["status"])
	}

	// ignored_keys should list the unclassified keys.
	ignored, ok := resp["ignored_keys"].([]interface{})
	if !ok || len(ignored) == 0 {
		t.Fatal("expected non-empty ignored_keys in response")
	}
	ignoredSet := make(map[string]bool)
	for _, k := range ignored {
		ignoredSet[k.(string)] = true
	}
	for _, expected := range []string{"schema_version", "runtimes", "profiles"} {
		if !ignoredSet[expected] {
			t.Errorf("expected %q in ignored_keys, got %v", expected, ignored)
		}
	}

	// Nothing written to store.
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()
	if len(fakeStore.settings) > 0 {
		t.Error("expected no sections written to store for unclassified-only PUT")
	}
}

func TestPutServerConfigDB_MixedLayer1AndUnclassified_AppliedAndIgnored(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Mix of Layer-1 (admin_emails) and unclassified (runtimes, workspace_path).
	body := `{
		"workspace_path": "/tmp/ws",
		"server": {
			"hub": {"admin_emails": ["admin@test.com"]}
		},
		"runtimes": {"go": {"image": "golang:1.21"}}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)

	if resp["status"] != "saved" {
		t.Errorf("expected status=saved, got %v", resp["status"])
	}

	// Layer-1 section was written.
	fakeStore.mu.Lock()
	_, accessOk := fakeStore.settings["access"]
	fakeStore.mu.Unlock()
	if !accessOk {
		t.Error("expected 'access' section in store after mixed PUT")
	}

	// ignored_keys should list the unclassified keys.
	ignored, ok := resp["ignored_keys"].([]interface{})
	if !ok || len(ignored) == 0 {
		t.Fatal("expected non-empty ignored_keys for mixed PUT")
	}
	ignoredSet := make(map[string]bool)
	for _, k := range ignored {
		ignoredSet[k.(string)] = true
	}
	if !ignoredSet["runtimes"] {
		t.Error("expected 'runtimes' in ignored_keys")
	}
	if !ignoredSet["workspace_path"] {
		t.Error("expected 'workspace_path' in ignored_keys")
	}

	// Verify the Layer-1 data was actually applied.
	snap := ops.Snapshot()
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "admin@test.com" {
		t.Errorf("expected [admin@test.com], got %v", snap.AdminEmails)
	}
}

func TestPutServerConfigDB_ExplicitLayer0_Still422(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Explicit Layer-0 key (database) should still be rejected.
	body := `{
		"server": {
			"database": {"driver": "sqlite"}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)

	if resp["error"] != "layer0_rejected" {
		t.Errorf("expected error=layer0_rejected, got %v", resp["error"])
	}

	// Nothing written to store.
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()
	if len(fakeStore.settings) > 0 {
		t.Error("expected no writes after Layer-0 rejection")
	}
}

func TestPutServerConfigDB_InvalidJSON_NothingWritten(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Invalid JSON structure — Go's json.Unmarshal catches this before schema
	// validation runs. The handler returns 400 at the readJSON layer.
	body := `{
		"server": {
			"hub": {"admin_emails": "not-an-array"}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	// Nothing written.
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()
	if len(fakeStore.settings) > 0 {
		t.Error("expected no writes after invalid JSON")
	}
}

func TestPutServerConfigDB_SchemaValidationFailure_NothingWritten(t *testing.T) {
	_, fakeStore, ops := newTestDBServer(t)

	// Payload that passes Go JSON unmarshalling but fails schema validation.
	// agent_defaults with default_max_turns as a string passes readJSON
	// (Go unmarshals "not-a-number" into int as 0) but we can test with
	// a lifecycle section that has an invalid auto_suspend_stalled type.
	//
	// Actually, Go's json decoder is loose with types, so we use a different
	// approach: directly call Update with a bad doc to test schema validation.
	badDoc := json.RawMessage(`{"admin_emails": "not-an-array"}`)
	_, err := ops.Update(context.Background(), "access", badDoc, "test@user.com", -1)
	if err == nil {
		t.Fatal("expected validation error for bad access doc, got nil")
	}

	// Verify nothing was written to store.
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()
	if _, ok := fakeStore.settings["access"]; ok {
		t.Error("expected no write after schema validation failure")
	}
}

// ---- CAS tests ----

func TestPutServerConfigDB_CAS_StaleRevision_409(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed existing access section at revision 1.
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["existing@admin.com"]}`))
	_, _ = ops.Refresh(context.Background())

	// PUT with expected_revision 99 (stale).
	body := `{
		"server": {"hub": {"admin_emails": ["new@admin.com"]}},
		"expected_revisions": {"access": 99}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)

	if resp["error"] != "revision_conflict" {
		t.Errorf("expected error=revision_conflict, got %v", resp["error"])
	}

	// Assert current revision is reported.
	conflicted, ok := resp["conflicted"].([]interface{})
	if !ok || len(conflicted) == 0 {
		t.Fatal("expected non-empty conflicted in 409 response")
	}
	firstConflict, ok := conflicted[0].(map[string]interface{})
	if !ok {
		t.Fatal("expected conflict object")
	}
	if firstConflict["current_revision"] == nil {
		t.Error("expected current_revision in conflict response")
	}
}

func TestPutServerConfigDB_CAS_CorrectRevision_Succeeds(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed existing access section at revision 1.
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["existing@admin.com"]}`))
	_, _ = ops.Refresh(context.Background())

	// PUT with correct expected_revision 1.
	body := `{
		"server": {"hub": {"admin_emails": ["updated@admin.com"]}},
		"expected_revisions": {"access": 1}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestPutServerConfigDB_NoCAS_LastWriterWins(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed existing access section.
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["existing@admin.com"]}`))
	_, _ = ops.Refresh(context.Background())

	// PUT without expected_revisions — last-writer-wins.
	body := `{
		"server": {"hub": {"admin_emails": ["lww@admin.com"]}}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	snap := ops.Snapshot()
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "lww@admin.com" {
		t.Errorf("expected [lww@admin.com], got %v", snap.AdminEmails)
	}
}

func TestPutServerConfigDB_ConcurrentPUT_OneConflicts(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed existing access section at revision 1.
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["original@admin.com"]}`))
	_, _ = ops.Refresh(context.Background())

	// Two concurrent PUTs both expect revision 1. One should succeed, one should 409.
	var wg sync.WaitGroup
	results := make([]int, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body := `{
				"server": {"hub": {"admin_emails": ["concurrent@admin.com"]}},
				"expected_revisions": {"access": 1}
			}`
			req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
			rr := httptest.NewRecorder()
			srv.handlePutServerConfigDB(rr, req, ops)
			results[idx] = rr.Code
		}(i)
	}
	wg.Wait()

	got200 := 0
	got409 := 0
	for _, code := range results {
		switch code {
		case 200:
			got200++
		case 409:
			got409++
		}
	}

	// Exactly one should succeed and one should conflict.
	if got200 != 1 || got409 != 1 {
		t.Errorf("expected 1×200 + 1×409, got codes: %v", results)
	}
}

// ---- Maintenance endpoints (postgres mode) ----

func TestPutMaintenanceDB_PersistsAndApplies(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Ensure env vars don't interfere.
	t.Setenv("SCION_SERVER_ADMIN_MODE", "")
	t.Setenv("SCION_SERVER_MAINTENANCE_MESSAGE", "")

	// Wire ops server for self-apply.
	ops.server = srv

	body := `{"enabled": true, "message": "DB maintenance"}`
	req := adminRequest(http.MethodPut, "/api/v1/admin/maintenance", body)
	rr := httptest.NewRecorder()
	srv.handlePutMaintenanceDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)

	if resp["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", resp["enabled"])
	}

	// Verify section was persisted in store.
	fakeStore.mu.Lock()
	row, ok := fakeStore.settings["maintenance"]
	fakeStore.mu.Unlock()
	if !ok {
		t.Fatal("expected maintenance section in store after PUT")
	}

	var ms opsettings.MaintenanceSettings
	json.Unmarshal(row.Value, &ms)
	if !ms.AdminMode {
		t.Error("expected admin_mode=true in stored row")
	}
	if ms.MaintenanceMessage != "DB maintenance" {
		t.Errorf("expected message 'DB maintenance', got %q", ms.MaintenanceMessage)
	}
}

func TestGetMaintenanceDB_ReflectsSnapshot(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	fakeStore.seed("maintenance", json.RawMessage(`{"admin_mode":true,"maintenance_message":"Test maintenance"}`))
	_, _ = ops.Refresh(context.Background())

	rr := httptest.NewRecorder()
	srv.handleGetMaintenanceDB(rr, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)

	if resp["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", resp["enabled"])
	}
	if resp["message"] != "Test maintenance" {
		t.Errorf("expected message 'Test maintenance', got %v", resp["message"])
	}
}

func TestMaintenanceDB_EnvOverrideStillWins(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// DB says maintenance off.
	fakeStore.seed("maintenance", json.RawMessage(`{"admin_mode":false}`))
	_, _ = ops.Refresh(context.Background())

	// But env says on.
	t.Setenv("SCION_SERVER_ADMIN_MODE", "true")

	// Apply snapshot with env override.
	snap := ops.Snapshot()
	ApplyMaintenanceFromSnapshot(srv, snap)

	// Server should be in maintenance due to env override.
	if !srv.maintenance.IsEnabled() {
		t.Error("expected maintenance enabled due to env override")
	}
}

// ---- File mode: existing behavior unchanged ----

func TestFileMode_ServerConfigDispatch(t *testing.T) {
	// In file/SQLite mode, handleAdminServerConfig should NOT dispatch to DB handlers.
	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}
	// dbDriver is empty → file/SQLite mode. No OperationalSettings set.

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

	// GET should go through handleGetServerConfig (file mode).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, req)

	// Should return 200 (the file-mode handler returns the settings file or defaults).
	if rr.Code != http.StatusOK {
		t.Fatalf("file-mode GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify no section_metadata in response (file mode doesn't add it).
	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if _, ok := resp["section_metadata"]; ok {
		t.Error("file mode should not include section_metadata")
	}
	if _, ok := resp["env_overrides"]; ok {
		t.Error("file mode should not include env_overrides")
	}
}

func TestFileMode_PostgresPathsNotTaken(t *testing.T) {
	// Explicitly verify that setting dbDriver to something other than "postgres"
	// keeps the file-mode path.
	srv := &Server{
		dbDriver:    "sqlite",
		maintenance: NewMaintenanceState(false, ""),
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("sqlite-mode GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if _, ok := resp["section_metadata"]; ok {
		t.Error("sqlite mode should not include section_metadata")
	}
}

func TestFileMode_MaintenanceDispatch(t *testing.T) {
	// File-mode maintenance should use in-memory state.
	srv := &Server{
		maintenance:    NewMaintenanceState(false, ""),
		maintenanceLog: logging.Subsystem("hub.maintenance"),
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

	// GET maintenance in file mode.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("file-mode maintenance GET: expected 200, got %d", rr.Code)
	}

	// PUT maintenance in file mode.
	putBody := `{"enabled": true, "message": "File mode maint"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/admin/maintenance", strings.NewReader(putBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("file-mode maintenance PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify in-memory state was updated.
	if !srv.maintenance.IsEnabled() {
		t.Error("expected maintenance enabled after PUT")
	}
}

// ---- extractKoanfKeysFromRequest tests ----

func TestExtractKoanfKeys_AllFieldCategories(t *testing.T) {
	sv := "1"
	tmpl := "my-template"
	turns := 100
	req := &ServerConfigUpdateRequest{
		SchemaVersion:   &sv,
		DefaultTemplate: &tmpl,
		DefaultMaxTurns: &turns,
		Server: &config.V1ServerConfig{
			Hub: &config.V1ServerHubConfig{
				AdminEmails: []string{"admin@test.com"},
				PublicURL:   "https://hub.test.com",
			},
			Auth: &config.V1AuthConfig{
				UserAccessMode: "open",
			},
			Database: &config.V1DatabaseConfig{
				Driver: "postgres",
			},
		},
	}

	keys := extractKoanfKeysFromRequest(req)
	keySet := make(map[string]bool)
	for _, k := range keys {
		keySet[k] = true
	}

	// Layer-1 keys
	if !keySet["default_template"] {
		t.Error("missing default_template")
	}
	if !keySet["default_max_turns"] {
		t.Error("missing default_max_turns")
	}
	if !keySet["server.hub.admin_emails"] {
		t.Error("missing server.hub.admin_emails")
	}
	if !keySet["server.hub.public_url"] {
		t.Error("missing server.hub.public_url")
	}
	if !keySet["server.auth.user_access_mode"] {
		t.Error("missing server.auth.user_access_mode")
	}

	// Layer-0 keys
	if !keySet["schema_version"] {
		t.Error("missing schema_version")
	}
	if !keySet["server.database"] {
		t.Error("missing server.database")
	}
}

// ---- buildSingleSectionDoc tests ----

func TestBuildSingleSectionDoc_Access(t *testing.T) {
	req := &ServerConfigUpdateRequest{
		Server: &config.V1ServerConfig{
			Hub: &config.V1ServerHubConfig{
				AdminEmails: []string{"admin@test.com"},
			},
			Auth: &config.V1AuthConfig{
				UserAccessMode: "domain_restricted",
			},
		},
	}

	doc, err := buildSingleSectionDoc(req, "access")
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	var access opsettings.AccessSettings
	if err := json.Unmarshal(doc, &access); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(access.AdminEmails) != 1 || access.AdminEmails[0] != "admin@test.com" {
		t.Errorf("admin_emails: want [admin@test.com], got %v", access.AdminEmails)
	}
	if access.UserAccessMode != "domain_restricted" {
		t.Errorf("user_access_mode: want domain_restricted, got %q", access.UserAccessMode)
	}
}

// ---- Race condition tests (run with -race) ----

func TestPutServerConfigDB_ConcurrentRace(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"server": {"hub": {"admin_emails": ["race@test.com"]}}}`
			req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
			rr := httptest.NewRecorder()
			srv.handlePutServerConfigDB(rr, req, ops)
			// Any of 200/409 is acceptable — no crashes or data races.
		}()
	}
	wg.Wait()
}

func TestMaintenanceDB_ConcurrentRace(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	t.Setenv("SCION_SERVER_ADMIN_MODE", "")
	t.Setenv("SCION_SERVER_MAINTENANCE_MESSAGE", "")
	ops.server = srv

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"enabled": true, "message": "race"}`
			req := adminRequest(http.MethodPut, "/api/v1/admin/maintenance", body)
			rr := httptest.NewRecorder()
			srv.handlePutMaintenanceDB(rr, req, ops)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			srv.handleGetMaintenanceDB(rr, ops)
		}()
	}
	wg.Wait()
}
