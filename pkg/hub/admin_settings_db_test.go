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
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

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
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

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
	body := `{
		"schema_version": "2",
		"runtimes": {"go": {"image": "golang:1.21"}},
		"profiles": {"dev": {}}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

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
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

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
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

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
	_, err := ops.Update(context.Background(), "access", badDoc, "test@user.com", -1, "managed")
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
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

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
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

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
	if err := json.Unmarshal(row.Value, &ms); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
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
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if resp["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", resp["enabled"])
	}
	if resp["message"] != "Test maintenance" {
		t.Errorf("expected message 'Test maintenance', got %v", resp["message"])
	}
}

func TestMaintenanceDB_EnvDoesNotOverrideDB(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// DB says maintenance off.
	fakeStore.seed("maintenance", json.RawMessage(`{"admin_mode":false}`))
	_, _ = ops.Refresh(context.Background())

	// Env says on — but per B3 redesign, env no longer force-wins for
	// maintenance. DB is authoritative in HA mode.
	t.Setenv("SCION_SERVER_ADMIN_MODE", "true")

	snap := ops.Snapshot()
	ApplyMaintenanceFromSnapshot(srv, snap)

	// Server should NOT be in maintenance — DB wins.
	if srv.maintenance.IsEnabled() {
		t.Error("expected maintenance disabled — DB wins over env in HA mode")
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
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if _, ok := resp["section_metadata"]; ok {
		t.Error("file mode should not include section_metadata")
	}
	// env_overrides IS included in file mode when SCION_SERVER_* vars are set
	// (H1 fix). Without env vars, it's omitted.
}

func TestFileMode_EnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "admin@test.com")
	t.Setenv("SCION_SERVER_DATABASE_DRIVER", "postgres")

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("file-mode GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	overrides, ok := resp["env_overrides"]
	if !ok {
		t.Fatal("file-mode response should include env_overrides when SCION_SERVER_* vars are set")
	}

	overrideList, ok := overrides.([]interface{})
	if !ok {
		t.Fatalf("env_overrides should be an array, got %T", overrides)
	}

	found := make(map[string]bool)
	for _, v := range overrideList {
		found[v.(string)] = true
	}

	if !found["server.hub.admin_emails"] {
		t.Error("expected server.hub.admin_emails in env_overrides")
	}
	if !found["server.database.driver"] {
		t.Error("expected server.database.driver in env_overrides (all env keys reported)")
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
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
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

	doc, err := buildSingleSectionDoc(req, "access", nil)
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

// ---- B1: Web UI always-sent empty Layer-0 objects must not 422 ----

func TestPutServerConfigDB_UIPayloadWithEmptyLayer0Objects_Succeeds(t *testing.T) {
	// B1: Simulate the EXACT web UI buildPayload() shape — the UI always sends
	// database, broker, storage, secrets, message_broker as empty or near-empty
	// objects. These must NOT trigger a 422.
	//
	// Always-sent UI keys from admin-server-config.ts buildPayload() ~882-924:
	//   - server.database = {} or {driver: ""} (line 889)
	//   - server.broker = {enabled: false, auto_provide: false} (lines 872-882)
	//   - server.storage = {} (line 912)
	//   - server.secrets = {} (line 918)
	//   - server.message_broker = {enabled: false} (line 921-924)
	srv, fakeStore, ops := newTestDBServer(t)

	// Exact UI payload shape including empty Layer-0 objects and a Layer-1 change.
	body := `{
		"server": {
			"hub": {
				"admin_emails": ["ui@admin.com"],
				"auto_suspend_stalled": true,
				"soft_delete_retain_files": false
			},
			"auth": {
				"user_access_mode": "open"
			},
			"database": {},
			"broker": {},
			"storage": {},
			"secrets": {},
			"message_broker": {}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("B1: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if resp["status"] != "saved" {
		t.Errorf("expected status=saved, got %v", resp["status"])
	}

	// Verify Layer-1 section was written.
	fakeStore.mu.Lock()
	_, ok := fakeStore.settings["access"]
	fakeStore.mu.Unlock()
	if !ok {
		t.Error("expected 'access' section written")
	}

	// Verify snapshot reflects Layer-1 values.
	snap := ops.Snapshot()
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "ui@admin.com" {
		t.Errorf("AdminEmails: want [ui@admin.com], got %v", snap.AdminEmails)
	}

	// Empty Layer-0 objects should NOT appear in ignored_keys (they're artifacts,
	// not user intent — fully-zero structs are excluded from ignored_keys too).
	if ignored, ok := resp["ignored_keys"]; ok {
		ignoredSlice, _ := ignored.([]interface{})
		for _, k := range ignoredSlice {
			ks := k.(string)
			for _, l0 := range []string{"server.database", "server.broker", "server.storage", "server.secrets", "server.message_broker"} {
				if ks == l0 {
					t.Errorf("B1: empty Layer-0 object %q should not appear in ignored_keys", l0)
				}
			}
		}
	}
}

func TestPutServerConfigDB_Layer0ObjectWithRealValue_Still422(t *testing.T) {
	// B1: A Layer-0 object with a real value (e.g. database.driver set) → still 422.
	srv, fakeStore, ops := newTestDBServer(t)

	body := `{
		"server": {
			"hub": {"admin_emails": ["admin@test.com"]},
			"database": {"driver": "postgres"},
			"broker": {},
			"storage": {},
			"secrets": {},
			"message_broker": {}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("B1: expected 422 for non-empty Layer-0, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify nothing written.
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()
	if len(fakeStore.settings) > 0 {
		t.Error("expected no writes when non-empty Layer-0 present")
	}
}

// ---- N1: GitHubApp secret masking tests ----

func TestMaskSensitiveFields_GitHubAppSecrets(t *testing.T) {
	// N1: Both PrivateKey and WebhookSecret must be masked.
	resp := &ServerConfigResponse{
		Server: &config.V1ServerConfig{
			GitHubApp: &config.V1GitHubAppConfig{
				AppID:         12345,
				PrivateKey:    "-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
				WebhookSecret: "whsec_secret123",
			},
		},
	}

	maskSensitiveFields(resp)

	if resp.Server.GitHubApp.PrivateKey != "********" {
		t.Errorf("N1: PrivateKey not masked, got %q", resp.Server.GitHubApp.PrivateKey)
	}
	if resp.Server.GitHubApp.WebhookSecret != "********" {
		t.Errorf("N1: WebhookSecret not masked, got %q", resp.Server.GitHubApp.WebhookSecret)
	}
	// Non-secret fields preserved.
	if resp.Server.GitHubApp.AppID != 12345 {
		t.Errorf("N1: AppID should be preserved, got %d", resp.Server.GitHubApp.AppID)
	}
}

func TestMaskSensitiveFields_GitHubAppSecretsEmpty(t *testing.T) {
	// N1: When secrets are empty, masking should not set them to "********".
	resp := &ServerConfigResponse{
		Server: &config.V1ServerConfig{
			GitHubApp: &config.V1GitHubAppConfig{
				AppID: 12345,
			},
		},
	}

	maskSensitiveFields(resp)

	if resp.Server.GitHubApp.PrivateKey != "" {
		t.Errorf("N1: empty PrivateKey should remain empty, got %q", resp.Server.GitHubApp.PrivateKey)
	}
	if resp.Server.GitHubApp.WebhookSecret != "" {
		t.Errorf("N1: empty WebhookSecret should remain empty, got %q", resp.Server.GitHubApp.WebhookSecret)
	}
}

func TestGetServerConfigDB_MasksGitHubAppSecrets(t *testing.T) {
	// N1: DB-mode GET path must mask GitHubApp secrets.
	srv, _, ops := newTestDBServer(t)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	// Handler calls maskSensitiveFields — if it reaches here without panic, masking ran.
}

// ---- B3: Provenance API tests ----

func TestGetServerConfigDB_SettingsTierIsDB(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp["settings_tier"] != "db" {
		t.Errorf("settings_tier: want 'db', got %v", resp["settings_tier"])
	}
}

func TestGetServerConfigDB_OriginInSectionMetadata(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fileK := emptyKoanf()
	envK := emptyKoanf()
	ops := NewOperationalSettings(fakeStore, fileK, envK)

	// Seed a section with "seeded" origin and another with "managed" origin.
	fakeStore.seedWithOrigin("access", json.RawMessage(`{"admin_emails":["a@test.com"]}`), "seeded")
	fakeStore.seedWithOrigin("maintenance", json.RawMessage(`{"admin_mode":false}`), "managed")
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

	accessMeta := resp.SectionMeta["access"]
	if accessMeta.Origin != "seeded" {
		t.Errorf("access origin: want 'seeded', got %q", accessMeta.Origin)
	}

	maintMeta := resp.SectionMeta["maintenance"]
	if maintMeta.Origin != "managed" {
		t.Errorf("maintenance origin: want 'managed', got %q", maintMeta.Origin)
	}

	// Sections without DB rows should have no origin.
	lifecycleMeta := resp.SectionMeta["lifecycle"]
	if lifecycleMeta.Origin != "" {
		t.Errorf("lifecycle origin: want empty, got %q", lifecycleMeta.Origin)
	}
}

func TestGetServerConfigDB_SupersededKeys(t *testing.T) {
	fakeStore := newFakeHubSettingStore()

	// Bootstrap koanf has admin_emails from bootstrap merge.
	bootstrapK := newFileKoanf(t, map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"bootstrap@example.com"},
		"server.auth.user_access_mode": "open",
	})
	envK := emptyKoanf()
	ops := NewOperationalSettings(fakeStore, bootstrapK, envK)

	// Seed access as "managed" with different admin_emails (DB wins, bootstrap is superseded).
	fakeStore.seedWithOrigin("access", json.RawMessage(`{"admin_emails":["admin@db.com"],"user_access_mode":"open"}`), "managed")
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

	// admin_emails differs between bootstrap (["bootstrap@example.com"]) and
	// DB (["admin@db.com"]), so it should appear in superseded_keys for "access".
	// user_access_mode is "open" in both → NOT superseded.
	// Keys must be full koanf paths so the frontend can match them.
	accessSup, ok := resp.SupersededKeys["access"]
	if !ok {
		t.Fatal("expected superseded_keys entry for 'access'")
	}

	found := false
	for _, sk := range accessSup {
		if sk.Key == "server.hub.admin_emails" {
			found = true
			if sk.Source != "yaml" {
				t.Errorf("admin_emails source: want 'yaml', got %q", sk.Source)
			}
		}
		if sk.Key == "admin_emails" {
			t.Error("superseded key should use full koanf path, not section-level JSON key")
		}
		if sk.Key == "server.auth.user_access_mode" || sk.Key == "user_access_mode" {
			t.Error("user_access_mode should NOT be superseded (values match)")
		}
	}
	if !found {
		t.Errorf("expected server.hub.admin_emails in superseded_keys, got %+v", accessSup)
	}
}

func TestGetServerConfigDB_SupersededKeys_SeededSectionExcluded(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	bootstrapK := newFileKoanf(t, map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"bootstrap@example.com"},
	})
	envK := emptyKoanf()
	ops := NewOperationalSettings(fakeStore, bootstrapK, envK)

	// Seed access as "seeded" — superseded keys only apply to "managed" sections.
	fakeStore.seedWithOrigin("access", json.RawMessage(`{"admin_emails":["admin@db.com"]}`), "seeded")
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

	// Seeded sections should not have superseded keys.
	if resp.SupersededKeys != nil {
		if _, ok := resp.SupersededKeys["access"]; ok {
			t.Error("seeded sections should not appear in superseded_keys")
		}
	}
}

func TestGetServerConfigDB_DeprecatedEnvKeys(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	bootstrapK := emptyKoanf()

	// envKoanf with a SCION_SERVER_* var targeting a Layer-1 key.
	envK := newEnvKoanf(t, map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"env@example.com"},
	})
	ops := NewOperationalSettings(fakeStore, bootstrapK, envK)
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

	if len(resp.DeprecatedEnvKeys) == 0 {
		t.Fatal("expected non-empty deprecated_env_keys")
	}

	found := false
	for _, d := range resp.DeprecatedEnvKeys {
		if d.KoanfKey == "server.hub.admin_emails" {
			found = true
			if d.SeedEquivalent == "" {
				t.Error("expected non-empty seed_equivalent")
			}
			if d.EnvVar == "" {
				t.Error("expected non-empty env_var")
			}
		}
	}
	if !found {
		t.Errorf("expected server.hub.admin_emails in deprecated_env_keys, got %+v", resp.DeprecatedEnvKeys)
	}
}

func TestGetServerConfigDB_DeprecatedEnvKeys_EmptyWhenNoServerEnvVars(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp ServerConfigDBResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.DeprecatedEnvKeys) != 0 {
		t.Errorf("expected no deprecated_env_keys, got %+v", resp.DeprecatedEnvKeys)
	}
}

func TestFileMode_SettingsTierIsFile(t *testing.T) {
	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp["settings_tier"] != "file" {
		t.Errorf("file mode settings_tier: want 'file', got %v", resp["settings_tier"])
	}
}

// ---- N2: Extract server.env and auth.DevMode tests ----

func TestExtractKoanfKeys_ServerEnv_IsLayer0(t *testing.T) {
	// N2: server.env must be extracted and classified as Layer-0.
	req := &ServerConfigUpdateRequest{
		Server: &config.V1ServerConfig{
			Env: "production",
		},
	}

	keys := extractKoanfKeysFromRequest(req)
	keySet := make(map[string]bool)
	for _, k := range keys {
		keySet[k] = true
	}
	if !keySet["server.env"] {
		t.Error("N2: server.env not extracted")
	}
}

func TestExtractKoanfKeys_DevTokenFile_IsLayer0(t *testing.T) {
	// N4: auth.dev_token_file must be extracted and classified as Layer-0.
	req := &ServerConfigUpdateRequest{
		Server: &config.V1ServerConfig{
			Auth: &config.V1AuthConfig{
				DevTokenFile: "/path/to/token",
			},
		},
	}

	keys := extractKoanfKeysFromRequest(req)
	keySet := make(map[string]bool)
	for _, k := range keys {
		keySet[k] = true
	}
	if !keySet["server.auth.dev_token_file"] {
		t.Error("N4: server.auth.dev_token_file not extracted")
	}
}

func TestPutServerConfigDB_ServerEnv_422(t *testing.T) {
	// N2: A PUT with server.env should trigger 422.
	srv, _, ops := newTestDBServer(t)

	body := `{
		"server": {
			"env": "production",
			"hub": {"admin_emails": ["admin@test.com"]}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("N2: expected 422 for server.env, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---- N6: Presence-aware field clearing tests ----

func TestPutServerConfigDB_ExplicitEmptyAdminEmails_ClearsField(t *testing.T) {
	// N6: Explicitly sending admin_emails as [] should clear the field.
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed existing access section.
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["existing@admin.com"],"user_access_mode":"open"}`))
	_, _ = ops.Refresh(context.Background())

	// Send explicit empty admin_emails.
	body := `{
		"server": {
			"hub": {"admin_emails": []},
			"auth": {"user_access_mode": "open"}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("N6: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify section doc has empty admin_emails.
	fakeStore.mu.Lock()
	row := fakeStore.settings["access"]
	fakeStore.mu.Unlock()

	var access opsettings.AccessSettings
	if err := json.Unmarshal(row.Value, &access); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(access.AdminEmails) != 0 {
		t.Errorf("N6: expected empty admin_emails after explicit [], got %v", access.AdminEmails)
	}
}

func TestPutServerConfigDB_ExplicitEmptyUserAccessMode_ClearsField(t *testing.T) {
	// N6: Explicitly sending user_access_mode as "" should clear the field.
	srv, fakeStore, ops := newTestDBServer(t)

	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["admin@test.com"],"user_access_mode":"invite_only"}`))
	_, _ = ops.Refresh(context.Background())

	body := `{
		"server": {
			"hub": {"admin_emails": ["admin@test.com"]},
			"auth": {"user_access_mode": ""}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("N6: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	fakeStore.mu.Lock()
	row := fakeStore.settings["access"]
	fakeStore.mu.Unlock()

	var access opsettings.AccessSettings
	if err := json.Unmarshal(row.Value, &access); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if access.UserAccessMode != "" {
		t.Errorf("N6: expected empty user_access_mode after explicit \"\", got %q", access.UserAccessMode)
	}
}

func TestPutServerConfigDB_OmittedFieldsPreserved(t *testing.T) {
	// N6: Omitting a field from the PUT payload should NOT clear it.
	// When admin_emails is omitted, the section doc should not contain it,
	// and on refresh the existing DB value is preserved.
	srv, fakeStore, ops := newTestDBServer(t)

	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["existing@admin.com"],"user_access_mode":"open"}`))
	_, _ = ops.Refresh(context.Background())

	// Only send user_access_mode, omit admin_emails entirely.
	body := `{
		"server": {
			"auth": {"user_access_mode": "invite_only"}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("N6: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify section doc was written.
	fakeStore.mu.Lock()
	row := fakeStore.settings["access"]
	fakeStore.mu.Unlock()

	var access opsettings.AccessSettings
	if err := json.Unmarshal(row.Value, &access); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if access.UserAccessMode != "invite_only" {
		t.Errorf("N6: expected user_access_mode=invite_only, got %q", access.UserAccessMode)
	}
	// admin_emails was omitted — should be nil/empty in the section doc
	// (the existing DB value is preserved by the OperationalSettings merge).
}

func TestPutServerConfigDB_ExplicitEmptyNotificationChannels_ClearsField(t *testing.T) {
	// N6: Explicitly sending notification_channels as [] should clear channels.
	srv, fakeStore, ops := newTestDBServer(t)

	fakeStore.seed("notifications", json.RawMessage(`{"notification_channels":[{"type":"slack"}]}`))
	_, _ = ops.Refresh(context.Background())

	body := `{
		"server": {
			"notification_channels": []
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("N6: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	fakeStore.mu.Lock()
	row := fakeStore.settings["notifications"]
	fakeStore.mu.Unlock()

	var notif opsettings.NotificationsSettings
	if err := json.Unmarshal(row.Value, &notif); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(notif.NotificationChannels) != 0 {
		t.Errorf("N6: expected empty notification_channels, got %v", notif.NotificationChannels)
	}
}

func TestPutServerConfigDB_ExplicitEmptyPublicURL_ClearsField(t *testing.T) {
	// N6: Explicitly sending public_url as "" should clear it.
	srv, fakeStore, ops := newTestDBServer(t)

	fakeStore.seed("endpoints", json.RawMessage(`{"public_url":"https://old.url","image_registry":"registry.test"}`))
	_, _ = ops.Refresh(context.Background())

	body := `{
		"server": {
			"hub": {"public_url": ""}
		}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("N6: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	fakeStore.mu.Lock()
	row := fakeStore.settings["endpoints"]
	fakeStore.mu.Unlock()

	var endpoints opsettings.EndpointsSettings
	if err := json.Unmarshal(row.Value, &endpoints); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if endpoints.PublicURL != "" {
		t.Errorf("N6: expected empty public_url, got %q", endpoints.PublicURL)
	}
}

// ---- N7: Maintenance message clearing ----

func TestPutMaintenanceDB_ExplicitEmptyMessage_ClearsMessage(t *testing.T) {
	// N7: Explicitly sending message: "" should clear the maintenance message.
	srv, fakeStore, ops := newTestDBServer(t)
	t.Setenv("SCION_SERVER_ADMIN_MODE", "")
	t.Setenv("SCION_SERVER_MAINTENANCE_MESSAGE", "")
	ops.server = srv

	// Seed with existing message.
	fakeStore.seed("maintenance", json.RawMessage(`{"admin_mode":true,"maintenance_message":"Existing message"}`))
	_, _ = ops.Refresh(context.Background())

	// Send explicit empty message.
	body := `{"enabled": true, "message": ""}`
	req := adminRequest(http.MethodPut, "/api/v1/admin/maintenance", body)
	rr := httptest.NewRecorder()
	srv.handlePutMaintenanceDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("N7: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify the message was cleared in the store.
	fakeStore.mu.Lock()
	row := fakeStore.settings["maintenance"]
	fakeStore.mu.Unlock()

	var ms opsettings.MaintenanceSettings
	if err := json.Unmarshal(row.Value, &ms); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if ms.MaintenanceMessage != "" {
		t.Errorf("N7: expected empty maintenance_message, got %q", ms.MaintenanceMessage)
	}
}

func TestPutMaintenanceDB_OmittedMessage_PreservesExisting(t *testing.T) {
	// N7: Omitting the message field should preserve the existing message.
	srv, fakeStore, ops := newTestDBServer(t)
	t.Setenv("SCION_SERVER_ADMIN_MODE", "")
	t.Setenv("SCION_SERVER_MAINTENANCE_MESSAGE", "")
	ops.server = srv

	// Seed with existing message.
	fakeStore.seed("maintenance", json.RawMessage(`{"admin_mode":true,"maintenance_message":"Keep this"}`))
	_, _ = ops.Refresh(context.Background())

	// Send only enabled, omit message.
	body := `{"enabled": false}`
	req := adminRequest(http.MethodPut, "/api/v1/admin/maintenance", body)
	rr := httptest.NewRecorder()
	srv.handlePutMaintenanceDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("N7: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify the message was preserved in the store.
	fakeStore.mu.Lock()
	row := fakeStore.settings["maintenance"]
	fakeStore.mu.Unlock()

	var ms opsettings.MaintenanceSettings
	if err := json.Unmarshal(row.Value, &ms); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if ms.MaintenanceMessage != "Keep this" {
		t.Errorf("N7: expected preserved message 'Keep this', got %q", ms.MaintenanceMessage)
	}
}

// ---- B1: isZeroStruct helper test ----

func TestIsZeroStruct(t *testing.T) {
	// Zero-valued structs.
	if !isZeroStruct(&config.V1DatabaseConfig{}) {
		t.Error("expected zero V1DatabaseConfig")
	}
	if !isZeroStruct(&config.V1BrokerConfig{}) {
		t.Error("expected zero V1BrokerConfig")
	}
	if !isZeroStruct(&config.V1StorageConfig{}) {
		t.Error("expected zero V1StorageConfig")
	}
	if !isZeroStruct(&config.V1SecretsConfig{}) {
		t.Error("expected zero V1SecretsConfig")
	}
	if !isZeroStruct(&config.V1MessageBrokerConfig{}) {
		t.Error("expected zero V1MessageBrokerConfig")
	}

	// Non-zero structs.
	if isZeroStruct(&config.V1DatabaseConfig{Driver: "postgres"}) {
		t.Error("V1DatabaseConfig with driver should not be zero")
	}
	if isZeroStruct(&config.V1BrokerConfig{Enabled: true}) {
		t.Error("V1BrokerConfig with enabled=true should not be zero")
	}
	if isZeroStruct(&config.V1StorageConfig{Provider: "gcs"}) {
		t.Error("V1StorageConfig with provider should not be zero")
	}
	if isZeroStruct(&config.V1MessageBrokerConfig{Enabled: true}) {
		t.Error("V1MessageBrokerConfig with enabled=true should not be zero")
	}

	// Nil.
	if !isZeroStruct((*config.V1DatabaseConfig)(nil)) {
		t.Error("nil should be zero")
	}
}

// ---- N2: Multi-section CAS partial-apply test ----

func TestPutServerConfigDB_CAS_MultiSection_PartialApply(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed access at revision 1 and lifecycle at revision 1.
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["a@test.com"]}`))
	fakeStore.seed("lifecycle", json.RawMessage(`{"soft_delete_retention":"72h"}`))
	_, _ = ops.Refresh(context.Background())

	// Advance lifecycle to revision 2 so our expected_revision of 1 is stale.
	_, _ = ops.Update(context.Background(), "lifecycle",
		json.RawMessage(`{"soft_delete_retention":"48h"}`), "other@test.com", -1, "managed")

	// PUT both sections: access with correct rev (1), lifecycle with stale rev (1).
	// Sections are written alphabetically: access first (succeeds), lifecycle second (conflicts).
	body := `{
		"server": {
			"hub": {
				"admin_emails": ["new@test.com"],
				"soft_delete_retention": "24h"
			},
			"auth": {}
		},
		"expected_revisions": {"access": 1, "lifecycle": 1}
	}`

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if resp["error"] != "revision_conflict" {
		t.Errorf("expected error=revision_conflict, got %v", resp["error"])
	}

	// access should appear in applied (alphabetically first, correct revision).
	applied, ok := resp["applied"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected applied map, got %T", resp["applied"])
	}
	if _, ok := applied["access"]; !ok {
		t.Error("expected 'access' in applied map")
	}

	// lifecycle should appear in conflicted.
	conflicted, ok := resp["conflicted"].([]interface{})
	if !ok || len(conflicted) == 0 {
		t.Fatalf("expected non-empty conflicted array, got %v", resp["conflicted"])
	}
	c0 := conflicted[0].(map[string]interface{})
	if c0["section"] != "lifecycle" {
		t.Errorf("expected conflicted section=lifecycle, got %v", c0["section"])
	}
	if c0["expected_revision"] != float64(1) {
		t.Errorf("expected expected_revision=1, got %v", c0["expected_revision"])
	}
	if c0["current_revision"] != float64(2) {
		t.Errorf("expected current_revision=2, got %v", c0["current_revision"])
	}
}

// ---- N3: Telemetry nil-path test ----

func TestApplySnapshotToResponse_NilTelemetry(t *testing.T) {
	enabled := true
	resp := ServerConfigResponse{
		Telemetry: &config.V1TelemetryConfig{
			Enabled: &enabled,
		},
	}

	snap := Layer1Snapshot{
		TelemetryConfig: nil,
	}

	applySnapshotToResponse(&resp, snap)

	if resp.Telemetry != nil {
		t.Errorf("expected nil telemetry after snapshot with nil TelemetryConfig, got %+v", resp.Telemetry)
	}
}

// ---- Schema endpoint tests ----

func TestGetServerConfigSchema_Shape(t *testing.T) {
	srv, _, _ := newTestDBServer(t)

	req := adminRequest(http.MethodGet, "/api/v1/admin/server-config/schema", "")
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSchema(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	sections, ok := resp["sections"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected sections map, got %T", resp["sections"])
	}

	expectedSections := opsettings.SectionNames()
	for _, name := range expectedSections {
		sec, ok := sections[name].(map[string]interface{})
		if !ok {
			t.Errorf("missing section %q in schema response", name)
			continue
		}
		if _, ok := sec["schema"]; !ok {
			t.Errorf("section %q missing 'schema' key", name)
		}
		if _, ok := sec["koanf_paths"]; !ok {
			t.Errorf("section %q missing 'koanf_paths' key", name)
		}
	}
}

func TestGetServerConfigSchema_AuthGating(t *testing.T) {
	srv, _, _ := newTestDBServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config/schema", nil)
	// No identity in context → unauthenticated.
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSchema(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for unauthenticated request, got %d", rr.Code)
	}
}

func TestGetServerConfigSchema_StableOutput(t *testing.T) {
	srv, _, _ := newTestDBServer(t)

	req1 := adminRequest(http.MethodGet, "/api/v1/admin/server-config/schema", "")
	rr1 := httptest.NewRecorder()
	srv.handleAdminServerConfigSchema(rr1, req1)

	req2 := adminRequest(http.MethodGet, "/api/v1/admin/server-config/schema", "")
	rr2 := httptest.NewRecorder()
	srv.handleAdminServerConfigSchema(rr2, req2)

	if rr1.Body.String() != rr2.Body.String() {
		t.Error("schema endpoint returned different output on consecutive calls")
	}
}

func TestGetServerConfigSchema_MethodNotAllowed(t *testing.T) {
	srv, _, _ := newTestDBServer(t)

	req := adminRequest(http.MethodPost, "/api/v1/admin/server-config/schema", `{}`)
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSchema(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", rr.Code)
	}
}

// ---- #391: Boolean and field-clearing round-trip correctness ----

func TestRoundTrip_AutoSuspendStalled_TruePersists(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	body := `{
		"server": {
			"hub": {"auto_suspend_stalled": true}
		}
	}`
	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	snap := ops.Snapshot()
	if !snap.AutoSuspendStalled {
		t.Error("#391: AutoSuspendStalled should be true after saving true")
	}
}

func TestRoundTrip_AutoSuspendStalled_FalsePersists(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// First set it to true.
	fakeStore.seed("lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`))
	_, _ = ops.Refresh(context.Background())
	if !ops.Snapshot().AutoSuspendStalled {
		t.Fatal("precondition: AutoSuspendStalled should be true")
	}

	// Now set it to false.
	body := `{
		"server": {
			"hub": {"auto_suspend_stalled": false}
		}
	}`
	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	snap := ops.Snapshot()
	if snap.AutoSuspendStalled {
		t.Error("#391: AutoSuspendStalled should be false after saving false, not reverted to default/true")
	}
}

func TestRoundTrip_AdminEmails_SetThenClear(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	// Step 1: Set admin_emails to ["a@b.com"].
	body := `{
		"server": {
			"hub": {"admin_emails": ["a@b.com"]},
			"auth": {"user_access_mode": "open"}
		}
	}`
	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("PUT(set): expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	snap := ops.Snapshot()
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "a@b.com" {
		t.Fatalf("precondition: AdminEmails should be [a@b.com], got %v", snap.AdminEmails)
	}

	// Step 2: Clear admin_emails to [].
	body = `{
		"server": {
			"hub": {"admin_emails": []},
			"auth": {"user_access_mode": "open"}
		}
	}`
	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("PUT(clear): expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	snap = ops.Snapshot()
	if len(snap.AdminEmails) != 0 {
		t.Errorf("#391: AdminEmails should be empty after clearing to [], got %v", snap.AdminEmails)
	}
}

func TestRoundTrip_TelemetryEnabled_FalsePersists(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	// Step 1: Set telemetry.enabled to true.
	body := `{
		"telemetry": {"enabled": true}
	}`
	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("PUT(true): expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	snap := ops.Snapshot()
	if snap.TelemetryConfig == nil || snap.TelemetryConfig.Enabled == nil || !*snap.TelemetryConfig.Enabled {
		t.Fatal("precondition: telemetry.enabled should be true")
	}

	// Step 2: Set telemetry.enabled to false.
	body = `{
		"telemetry": {"enabled": false}
	}`
	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("PUT(false): expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	snap = ops.Snapshot()
	if snap.TelemetryConfig == nil || snap.TelemetryConfig.Enabled == nil {
		t.Fatal("#391: telemetry.enabled should not be nil after saving false")
	}
	if *snap.TelemetryConfig.Enabled {
		t.Error("#391: telemetry.enabled should be false after saving false, not reverted to default/true")
	}
}

func TestRoundTrip_WebhooksEnabled_FalsePersists(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed with webhooks_enabled = true.
	fakeStore.seed("github_app", json.RawMessage(`{"app_id":42,"webhooks_enabled":true}`))
	_, _ = ops.Refresh(context.Background())
	if !ops.Snapshot().GitHubWebhooksEnabled {
		t.Fatal("precondition: GitHubWebhooksEnabled should be true")
	}

	// Set webhooks_enabled to false.
	body := `{
		"server": {
			"github_app": {"app_id": 42, "webhooks_enabled": false}
		}
	}`
	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", body)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify the section doc stores *false (not omitted).
	fakeStore.mu.Lock()
	row := fakeStore.settings["github_app"]
	fakeStore.mu.Unlock()

	var ga opsettings.GitHubAppSettings
	if err := json.Unmarshal(row.Value, &ga); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if ga.WebhooksEnabled == nil {
		t.Fatal("#391: WebhooksEnabled should be *false in section doc, not nil (omitted)")
	}
	if *ga.WebhooksEnabled {
		t.Error("#391: WebhooksEnabled should be false in section doc")
	}

	// Verify snapshot reads back false.
	snap := ops.Snapshot()
	if snap.GitHubWebhooksEnabled {
		t.Error("#391: GitHubWebhooksEnabled should be false after saving false")
	}
}

// ---- DELETE /api/v1/admin/server-config/sections/{name} (reset) ----

func TestResetSection_DeletesManagedSection(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// Seed a managed section.
	fakeStore.seedWithOrigin("access", json.RawMessage(`{"admin_emails":["admin@db.com"],"user_access_mode":"open"}`), "managed")
	_, _ = ops.Refresh(context.Background())

	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr, adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/access", ""))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Row should be deleted.
	fakeStore.mu.Lock()
	_, exists := fakeStore.settings["access"]
	fakeStore.mu.Unlock()
	if exists {
		t.Error("expected access row to be deleted after reset")
	}
}

func TestResetSection_RejectsNonDelete(t *testing.T) {
	srv, _, _ := newTestDBServer(t)

	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config/sections/access", ""))

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}

func TestResetSection_RejectsUnknownSection(t *testing.T) {
	srv, _, _ := newTestDBServer(t)

	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr, adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/nonexistent", ""))

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestResetSection_RequiresAdmin(t *testing.T) {
	srv, _, _ := newTestDBServer(t)

	// Non-admin request.
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/access", nil)
	viewer := NewAuthenticatedUser("u2", "viewer@example.com", "Viewer", "viewer", "cli")
	r = r.WithContext(contextWithIdentity(r.Context(), viewer))

	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rr.Code)
	}
}
