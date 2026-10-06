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

// Real-SQLite coverage for ptone/scion#1091 option B: on a SQLite hub the
// hub_settings DB is the source of truth for Layer-1 settings, exactly as on
// postgres. Since #1432 OperationalSettings is wired on every driver, and each
// ops.Update re-applies the full DB snapshot, so an admin write that touched
// only settings.yaml (or only in-memory state) was silently reverted by the
// next unrelated DB-backed admin write. These tests drive the admin handlers
// against a real migrated SQLite store and then make such an unrelated write.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/knadh/koanf/v2"
)

// newSQLiteOpsServer builds a SQLite-driver Server backed by a real migrated
// SQLite store with OperationalSettings wired the way initOperationalSettings
// does it: seed rows are written first by the caller (via seed), then Refresh,
// ApplySnapshot, SetOperationalSettings. ops.server is set so ops.Update
// self-applies, as StartPropagation arranges in production.
func newSQLiteOpsServer(t *testing.T, bootstrap *koanf.Koanf, seed map[string]string) (*Server, store.Store, *OperationalSettings) {
	t.Helper()
	st, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	for section, doc := range seed {
		if _, err := st.UpsertHubSetting(ctx, section, json.RawMessage(doc), "system", -1, "seeded"); err != nil {
			t.Fatalf("seed %s: %v", section, err)
		}
	}

	if bootstrap == nil {
		bootstrap = emptyKoanf()
	}
	ops := NewOperationalSettings(st, bootstrap, emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	srv := &Server{
		dbDriver:       "sqlite",
		store:          st,
		maintenance:    NewMaintenanceState(false, ""),
		maintenanceLog: logging.Subsystem("hub.maintenance"),
	}
	snap := ops.Snapshot()
	ApplySnapshot(srv, snap)
	ApplyMaintenanceFromSnapshot(srv, snap)
	srv.SetOperationalSettings(ops)
	ops.server = srv
	return srv, st, ops
}

// unrelatedLifecycleUpdate is the trigger from the ptone/scion#1091 repro: another admin
// write to a different section, which re-applies the whole DB snapshot.
func unrelatedLifecycleUpdate(t *testing.T, ops *OperationalSettings) {
	t.Helper()
	if _, err := ops.Update(context.Background(), "lifecycle",
		json.RawMessage(`{"auto_suspend_stalled":true}`), "other@example.com", -1, "managed"); err != nil {
		t.Fatalf("unrelated lifecycle update: %v", err)
	}
}

func hubSettingDocMap(t *testing.T, st store.Store, section string) (*store.HubSetting, map[string]interface{}) {
	t.Helper()
	rec, err := st.GetHubSetting(context.Background(), section)
	if err != nil {
		t.Fatalf("get hub setting %s: %v", section, err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Value, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", section, err)
	}
	return rec, m
}

// hubSettingRevisions maps each hub_settings section to its revision.
func hubSettingRevisions(t *testing.T, st store.Store) map[string]int64 {
	t.Helper()
	rows, err := st.ListHubSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Section] = r.Revision
	}
	return out
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// W1 on SQLite: PUT /api/v1/github-app persists to the github_app DB section
// and survives an unrelated ops.Update. Before the fix the handler gated on
// IsPostgres(), wrote only settings.yaml, and the lifecycle write reverted the
// in-memory app_id to the seeded DB value (111).
func TestSQLite_UpdateGitHubApp_PersistsToDBAndSurvivesUnrelatedUpdate(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	srv, st, ops := newSQLiteOpsServer(t, nil, map[string]string{
		"github_app": `{"app_id":111,"webhooks_enabled":false}`,
	})
	if got := srv.config.GitHubAppConfig.AppID; got != 111 {
		t.Fatalf("precondition: want in-memory app_id 111 from DB, got %d", got)
	}

	rr := httptest.NewRecorder()
	srv.handleUpdateGitHubApp(rr, adminRequest(http.MethodPut, "/api/v1/github-app", githubAppUpdateBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", rr.Code, rr.Body.String())
	}

	unrelatedLifecycleUpdate(t, ops)

	if got := srv.config.GitHubAppConfig.AppID; got != 999 {
		t.Errorf("in-memory app_id = %d after unrelated ops.Update, want 999", got)
	}
	rec, doc := hubSettingDocMap(t, st, "github_app")
	if got, _ := doc["app_id"].(float64); got != 999 {
		t.Errorf("DB github_app.app_id = %v, want 999", doc["app_id"])
	}
	if rec.Origin != "managed" {
		t.Errorf("DB github_app origin = %q, want managed", rec.Origin)
	}
	if data := readFileString(t, settingsPath); strings.Contains(data, "app_id") {
		t.Errorf("settings.yaml must not be written on a DB-backed SQLite hub:\n%s", data)
	}
}

// Server-config PUT on SQLite routes through the DB handler: a Layer-1 key
// lands in the DB, survives an unrelated ops.Update, is reflected by GET, and
// shows up in superseded_keys against the settings.yaml (bootstrap) value.
func TestSQLite_PutServerConfig_Layer1PersistsToDB(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	bootstrap := newFileKoanf(t, map[string]interface{}{
		"quotas.enforce_broker_quotas": true,
	})
	srv, st, ops := newSQLiteOpsServer(t, bootstrap, map[string]string{
		"quotas": `{"enforce_broker_quotas":true}`,
	})
	if !srv.brokerQuotasEnforced() {
		t.Fatal("precondition: want broker quotas enforced from the seeded row")
	}
	before := readFileString(t, settingsPath)

	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"quotas":{"enforce_broker_quotas":false}}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	unrelatedLifecycleUpdate(t, ops)

	if srv.brokerQuotasEnforced() {
		t.Error("broker quotas re-enabled after unrelated ops.Update; the PUT was reverted")
	}
	rec, doc := hubSettingDocMap(t, st, "quotas")
	if got, ok := doc["enforce_broker_quotas"].(bool); !ok || got {
		t.Errorf("DB quotas.enforce_broker_quotas = %v, want false", doc["enforce_broker_quotas"])
	}
	if rec.Origin != "managed" {
		t.Errorf("DB quotas origin = %q, want managed", rec.Origin)
	}
	if after := readFileString(t, settingsPath); after != before {
		t.Errorf("settings.yaml must not change on a DB-backed SQLite hub:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	getRR := httptest.NewRecorder()
	srv.handleAdminServerConfig(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	var resp ServerConfigDBResponse
	if err := json.Unmarshal(getRR.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal GET: %v", err)
	}
	if resp.SectionMeta == nil {
		t.Error("GET on a DB-backed SQLite hub should carry section_metadata (DB handler)")
	}
	if resp.Quotas == nil || resp.Quotas.EnforceBrokerQuotas == nil || *resp.Quotas.EnforceBrokerQuotas {
		t.Errorf("GET quotas: want enforce_broker_quotas=false, got %+v", resp.Quotas)
	}
	found := false
	for _, sk := range resp.SupersededKeys["quotas"] {
		if sk.Key == "quotas.enforce_broker_quotas" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected quotas.enforce_broker_quotas in superseded_keys, got %+v", resp.SupersededKeys)
	}
}

// Layer-0 keys on SQLite get the same treatment as on postgres: 422
// layer0_rejected, nothing written to the DB or to settings.yaml.
func TestSQLite_PutServerConfig_Layer0Rejected(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	srv, st, _ := newSQLiteOpsServer(t, nil, nil)
	before := readFileString(t, settingsPath)
	// Migration seeds some rows of its own; compare against them.
	rowsBefore := hubSettingRevisions(t, st)

	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"database":{"driver":"postgres"},"hub":{"admin_emails":["a@example.com"]}}}`))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "layer0_rejected" {
		t.Errorf("expected error=layer0_rejected, got %v", resp["error"])
	}

	if rowsAfter := hubSettingRevisions(t, st); !reflect.DeepEqual(rowsAfter, rowsBefore) {
		t.Errorf("a rejected Layer-0 PUT must write nothing to the DB:\nbefore: %v\nafter:  %v", rowsBefore, rowsAfter)
	}
	if after := readFileString(t, settingsPath); after != before {
		t.Errorf("a rejected Layer-0 PUT must not touch settings.yaml:\n%s", after)
	}
}

// Maintenance PUT on SQLite persists to the maintenance DB section. Before the
// fix it was in-memory only, so with a maintenance row present the next
// unrelated ops.Update re-applied admin_mode=false.
func TestSQLite_PutMaintenance_PersistsToDBAndSurvivesUnrelatedUpdate(t *testing.T) {
	tempSettingsHome(t)
	srv, st, ops := newSQLiteOpsServer(t, nil, map[string]string{
		"maintenance": `{"admin_mode":false}`,
	})

	rr := httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, adminRequest(http.MethodPut, "/api/v1/admin/maintenance",
		`{"enabled":true,"message":"sqlite maint"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	unrelatedLifecycleUpdate(t, ops)

	if !srv.maintenance.IsEnabled() {
		t.Error("maintenance disabled after unrelated ops.Update; the PUT was reverted")
	}
	if got := srv.maintenance.Message(); got != "sqlite maint" {
		t.Errorf("maintenance message = %q, want %q", got, "sqlite maint")
	}
	rec, doc := hubSettingDocMap(t, st, "maintenance")
	if got, _ := doc["admin_mode"].(bool); !got {
		t.Errorf("DB maintenance.admin_mode = %v, want true", doc["admin_mode"])
	}
	if rec.Origin != "managed" {
		t.Errorf("DB maintenance origin = %q, want managed", rec.Origin)
	}
}

// DELETE /api/v1/admin/server-config/sections/{name} works on SQLite: the row
// is removed and the section falls back to bootstrap material immediately.
func TestSQLite_ServerConfigSectionReset(t *testing.T) {
	tempSettingsHome(t)
	bootstrap := newFileKoanf(t, map[string]interface{}{
		"quotas.enforce_broker_quotas": true,
	})
	srv, st, ops := newSQLiteOpsServer(t, bootstrap, nil)
	if _, err := ops.Update(context.Background(), "quotas",
		json.RawMessage(`{"enforce_broker_quotas":false}`), "admin@example.com", -1, "managed"); err != nil {
		t.Fatal(err)
	}
	if srv.brokerQuotasEnforced() {
		t.Fatal("precondition: managed row should disable broker quotas")
	}

	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr, adminRequest(http.MethodDelete,
		"/api/v1/admin/server-config/sections/quotas", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if _, err := st.GetHubSetting(context.Background(), "quotas"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("quotas row should be gone after reset, got err=%v", err)
	}
	if !srv.brokerQuotasEnforced() {
		t.Error("after reset, broker quotas should fall back to the bootstrap value (true)")
	}
	if snap := ops.Snapshot(); snap.EnforceBrokerQuotas == nil || !*snap.EnforceBrokerQuotas {
		t.Errorf("snapshot EnforceBrokerQuotas = %v, want bootstrap true", snap.EnforceBrokerQuotas)
	}
}

// On SQLite the maintenance section is never seeded, so
// with no row the live state (set at startup from SCION_SERVER_ADMIN_MODE or
// settings.yaml) is authoritative. GET must report it, and a message-only PUT
// must build on it rather than on the empty snapshot, which would write
// admin_mode=false and take the hub out of maintenance.
func TestSQLite_Maintenance_NoRowUsesLiveState(t *testing.T) {
	tempSettingsHome(t)
	srv, st, _ := newSQLiteOpsServer(t, nil, nil)
	if _, err := st.GetHubSetting(context.Background(), "maintenance"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("precondition: want no maintenance row, got err=%v", err)
	}
	// As set at startup from SCION_SERVER_ADMIN_MODE=true.
	srv.maintenance = NewMaintenanceState(true, "env maint")

	rr := httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, adminRequest(http.MethodGet, "/api/v1/admin/maintenance", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Enabled bool   `json:"enabled"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Message != "env maint" {
		t.Errorf("GET with no row = %+v, want the live state {enabled:true message:env maint}", got)
	}

	rr = httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, adminRequest(http.MethodPut, "/api/v1/admin/maintenance", `{"message":"new msg"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !srv.maintenance.IsEnabled() {
		t.Error("a message-only PUT took the hub out of maintenance")
	}
	if got := srv.maintenance.Message(); got != "new msg" {
		t.Errorf("live message = %q, want %q", got, "new msg")
	}
	_, doc := hubSettingDocMap(t, st, "maintenance")
	if v, _ := doc["admin_mode"].(bool); !v {
		t.Errorf("DB maintenance.admin_mode = %v, want true (kept from live state)", doc["admin_mode"])
	}
	if doc["maintenance_message"] != "new msg" {
		t.Errorf("DB maintenance doc = %v, want maintenance_message=new msg", doc)
	}
}

// putServerConfig issues PUT /api/v1/admin/server-config through the
// dispatcher and returns the recorder.
func putServerConfig(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr
}

func rejectedKeys(t *testing.T, rr *httptest.ResponseRecorder) (string, []string) {
	t.Helper()
	var resp struct {
		Error string   `json:"error"`
		Keys  []string `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
	}
	return resp.Error, resp.Keys
}

// A key the DB-backed PUT neither persists nor rejects
// by classification must not get 200 "saved". Each case writes nothing.
func TestSQLite_PutServerConfig_UnpersistedKeysRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"file-only auto_inject_gcloud_adc", `{"auto_inject_gcloud_adc":true}`, []string{"auto_inject_gcloud_adc"}},
		{"flat dotted key", `{"server.hub.auto_suspend_stalled":true}`, []string{"server.hub.auto_suspend_stalled"}},
		{"unknown nested key", `{"server":{"hub":{"bogus":1}}}`, []string{"server.hub.bogus"}},
		{"mixed with a Layer-1 key", `{"quotas":{"enforce_broker_quotas":false},"auto_inject_gcloud_adc":true}`, []string{"auto_inject_gcloud_adc"}},
		{"unmapped server field, nested key reported once", `{"server":{"scheduler":{"enabled":true}}}`, []string{"server.scheduler"}},
		{"agent role the handler does not write", `{"default_agent_role":"agent-role-readonly"}`, []string{"default_agent_role"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settingsPath := tempSettingsHome(t)
			srv, st, _ := newSQLiteOpsServer(t, nil, nil)
			before := readFileString(t, settingsPath)
			rowsBefore := hubSettingRevisions(t, st)

			rr := putServerConfig(t, srv, tc.body)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
			}
			code, keys := rejectedKeys(t, rr)
			if code != "unpersisted_keys_rejected" || !reflect.DeepEqual(keys, tc.want) {
				t.Errorf("got error=%q keys=%v, want unpersisted_keys_rejected %v", code, keys, tc.want)
			}
			if after := hubSettingRevisions(t, st); !reflect.DeepEqual(after, rowsBefore) {
				t.Errorf("a rejected PUT must write nothing to the DB:\nbefore: %v\nafter:  %v", rowsBefore, after)
			}
			if after := readFileString(t, settingsPath); after != before {
				t.Errorf("a rejected PUT must not touch settings.yaml:\n%s", after)
			}
		})
	}
}

func TestSQLite_PutServerConfig_EmptyBodyRejected(t *testing.T) {
	tempSettingsHome(t)
	srv, _, _ := newSQLiteOpsServer(t, nil, nil)
	for _, body := range []string{`{}`, `{"expected_revisions":{"quotas":1}}`} {
		if rr := putServerConfig(t, srv, body); rr.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: expected 400, got %d: %s", body, rr.Code, rr.Body.String())
		}
	}
}

// No-op echoes keep getting 200: GET-only fields sent back unchanged
// (section_metadata, superseded_keys, version info, ...) and unpersisted
// request keys at the GET view's value (here auto_inject_gcloud_adc false,
// which GET omits). A key neither the request nor the GET view knows is
// never an echo, even at a zero value (ptone/scion#3463). The full GET body
// is not a 200 echo on any driver: it carries schema_version, rejected as
// unclassified (ptone/scion#938).
func TestSQLite_PutServerConfig_EchoAccepted(t *testing.T) {
	tempSettingsHome(t)
	srv, st, _ := newSQLiteOpsServer(t, nil, map[string]string{
		"quotas": `{"enforce_broker_quotas":true}`,
	})

	getRR := httptest.NewRecorder()
	srv.handleAdminServerConfig(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", getRR.Code, getRR.Body.String())
	}
	var view map[string]json.RawMessage
	if err := json.Unmarshal(getRR.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	echo := map[string]json.RawMessage{}
	for _, k := range []string{"scion_version", "settings_tier", "section_metadata", "superseded_keys", "quotas"} {
		if v, ok := view[k]; ok {
			echo[k] = v
		}
	}
	if _, ok := echo["section_metadata"]; !ok {
		t.Fatalf("precondition: GET view should carry section_metadata, got %s", getRR.Body.String())
	}
	body, _ := json.Marshal(echo)
	if rr := putServerConfig(t, srv, string(body)); rr.Code != http.StatusOK {
		t.Errorf("echo of GET-only fields: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if rr := putServerConfig(t, srv, `{"default_harness_auth":"","quotas":{"enforce_broker_quotas":false}}`); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("zero-valued unknown key: expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, doc := hubSettingDocMap(t, st, "quotas"); doc["enforce_broker_quotas"] != true {
		t.Fatalf("a rejected PUT must write nothing, got quotas %v", doc)
	}

	rr := putServerConfig(t, srv, `{"auto_inject_gcloud_adc":false,"quotas":{"enforce_broker_quotas":false}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("zero-valued unpersisted keys with a Layer-1 change: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, doc := hubSettingDocMap(t, st, "quotas"); doc["enforce_broker_quotas"] != false {
		t.Errorf("the Layer-1 change must still be written, got %v", doc)
	}
}

// With the real SQLite propagation wiring (in-process
// ChannelEventPublisher + StartPropagation, as startSettingsPropagation does
// it), the event echo and a poll-backstop Refresh re-apply leave a PUT intact.
func TestSQLite_Propagation_PutSurvivesEchoAndRefresh(t *testing.T) {
	tempSettingsHome(t)
	srv, _, ops := newSQLiteOpsServer(t, nil, map[string]string{
		"quotas": `{"enforce_broker_quotas":true}`,
	})
	ops.server = nil // let StartPropagation wire it, as in production
	ops.PollInterval = 20 * time.Millisecond
	pub := NewChannelEventPublisher()
	ops.SetEventPublisher(pub)
	// Observe the echo the Update publishes.
	echo, unsub := pub.Subscribe(settingsUpdatedSubject)
	defer unsub()
	ctx, cancel := context.WithCancel(context.Background())
	ops.StartPropagation(ctx, srv)
	defer func() { cancel(); ops.StopPropagation() }()

	if rr := putServerConfig(t, srv, `{"quotas":{"enforce_broker_quotas":false}}`); rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	select {
	case <-echo:
	case <-time.After(5 * time.Second):
		t.Fatal("no admin.settings.updated event published on SQLite")
	}

	// The subscription loop handles the same event; let the poll backstop
	// (20ms, with jitter) run a few Refresh re-applies too, then force one.
	time.Sleep(200 * time.Millisecond)
	ops.refreshAndApply(context.Background(), srv)

	if srv.brokerQuotasEnforced() {
		t.Error("PUT reverted by the propagation echo / Refresh re-apply")
	}
	if snap := ops.Snapshot(); snap.EnforceBrokerQuotas == nil || *snap.EnforceBrokerQuotas {
		t.Errorf("snapshot EnforceBrokerQuotas = %v, want false", snap.EnforceBrokerQuotas)
	}
}
