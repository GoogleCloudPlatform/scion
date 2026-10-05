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

// ptone/scion#1091 option C on real SQLite: Layer-0 is editable through the
// server-config API on workstation hubs (written to settings.yaml) and
// rejected on hosted hubs, whatever the DB driver.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	yamlv3 "gopkg.in/yaml.v3"
)

const workstationSettingsYAML = `schema_version: "1"
server:
  hub:
    admin_emails:
      - seed-admin@example.com
    port: 9810
  broker:
    enabled: true
    broker_id: b-123
    broker_token: tok-secret
`

// workstationHome writes settings.yaml with Layer-1 seed values and
// hub-owned broker identity, and returns its path.
func workstationHome(t *testing.T) string {
	t.Helper()
	path := tempSettingsHome(t)
	if err := os.WriteFile(path, []byte(workstationSettingsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newSQLiteHubInMode(t *testing.T, workstation bool, seed map[string]string) (*Server, store.Store, *OperationalSettings) {
	t.Helper()
	srv, st, ops := newSQLiteOpsServer(t, nil, seed)
	srv.workstation = workstation
	return srv, st, ops
}

func readYAMLMap(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func yamlAt(m map[string]interface{}, path ...string) interface{} {
	var cur interface{} = m
	for _, p := range path {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

type putResp struct {
	Status string `json:"status"`
	Reload struct {
		Applied         []string `json:"applied"`
		RequiresRestart []string `json:"requires_restart"`
	} `json:"reload"`
	FileKeys []string `json:"file_keys"`
}

func decodePut(t *testing.T, rr *httptest.ResponseRecorder) putResp {
	t.Helper()
	var r putResp
	if err := json.Unmarshal(rr.Body.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
	}
	return r
}

func noTempSettingsFiles(t *testing.T, settingsPath string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(settingsPath), ".settings.yaml.tmp-*"))
	if len(matches) > 0 {
		t.Errorf("staged settings files left behind: %v", matches)
	}
}

// A mixed workstation PUT is split: Layer-1 to the DB, Layer-0 /
// unclassified / file-only keys to settings.yaml. Untouched file keys
// (Layer-1 seeds, hub-owned broker identity) survive, log_level is applied
// live, the broker runtime reload runs, and the rest is requires_restart.
func TestWorkstation_PutServerConfig_MixedSplit(t *testing.T) {
	settingsPath := workstationHome(t)
	srv, st, _ := newSQLiteHubInMode(t, true, nil)
	reloaded := false
	srv.runtimeReloadFunc = func() bool { reloaded = true; return true }

	rr := putServerConfig(t, srv, `{
		"quotas": {"enforce_broker_quotas": false},
		"server": {"log_level": "info", "hub": {"port": 9999}, "broker": {"enabled": true, "port": 9800}},
		"active_profile": "local",
		"auto_inject_gcloud_adc": true
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePut(t, rr)

	if _, doc := hubSettingDocMap(t, st, "quotas"); doc["enforce_broker_quotas"] != false {
		t.Errorf("Layer-1 quotas not written to the DB: %v", doc)
	}
	m := readYAMLMap(t, settingsPath)
	for _, c := range []struct {
		path []string
		want interface{}
	}{
		{[]string{"server", "log_level"}, "info"},
		{[]string{"server", "hub", "port"}, 9999},
		{[]string{"server", "broker", "port"}, 9800},
		{[]string{"active_profile"}, "local"},
		{[]string{"auto_inject_gcloud_adc"}, true},
		// Kept: siblings of the edited paths.
		{[]string{"server", "broker", "broker_id"}, "b-123"},
		{[]string{"server", "broker", "broker_token"}, "tok-secret"},
	} {
		if got := yamlAt(m, c.path...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("settings.yaml %s = %v, want %v", strings.Join(c.path, "."), got, c.want)
		}
	}
	if got := yamlAt(m, "server", "hub", "admin_emails"); !reflect.DeepEqual(got, []interface{}{"seed-admin@example.com"}) {
		t.Errorf("Layer-1 seed admin_emails in settings.yaml must survive a Layer-0 edit, got %v", got)
	}
	if yamlAt(m, "quotas") != nil {
		t.Error("Layer-1 quotas must not be written to settings.yaml")
	}

	if !reloaded {
		t.Error("the co-located broker runtime reload did not run")
	}
	for _, want := range []string{"quotas", "log_level", "broker_runtime"} {
		if !containsString(resp.Reload.Applied, want) {
			t.Errorf("reload.applied = %v, want %q", resp.Reload.Applied, want)
		}
	}
	wantRestart := []string{"active_profile", "server.broker", "server.hub.port"}
	if !reflect.DeepEqual(resp.Reload.RequiresRestart, wantRestart) {
		t.Errorf("reload.requires_restart = %v, want %v", resp.Reload.RequiresRestart, wantRestart)
	}
	if !containsString(resp.FileKeys, "auto_inject_gcloud_adc") || !containsString(resp.FileKeys, "server.log_level") {
		t.Errorf("file_keys = %v, want the file-routed keys", resp.FileKeys)
	}
	noTempSettingsFiles(t, settingsPath)
}

// A Layer-0-only workstation PUT writes settings.yaml, touches no DB row and
// reports the key as requires_restart.
func TestWorkstation_PutServerConfig_Layer0Only(t *testing.T) {
	settingsPath := workstationHome(t)
	srv, st, _ := newSQLiteHubInMode(t, true, nil)
	rowsBefore := hubSettingRevisions(t, st)

	rr := putServerConfig(t, srv, `{"server":{"database":{"driver":"postgres","url":"postgres://x"}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePut(t, rr)
	if got := yamlAt(readYAMLMap(t, settingsPath), "server", "database", "driver"); got != "postgres" {
		t.Errorf("settings.yaml server.database.driver = %v, want postgres", got)
	}
	if !reflect.DeepEqual(resp.Reload.RequiresRestart, []string{"server.database"}) {
		t.Errorf("requires_restart = %v, want [server.database]", resp.Reload.RequiresRestart)
	}
	if after := hubSettingRevisions(t, st); !reflect.DeepEqual(after, rowsBefore) {
		t.Errorf("a Layer-0-only PUT must not write the DB:\nbefore: %v\nafter:  %v", rowsBefore, after)
	}
}

// The same Layer-0 PUT on a hosted hub is rejected and writes nothing.
func TestHosted_PutServerConfig_Layer0Rejected(t *testing.T) {
	settingsPath := workstationHome(t)
	srv, _, _ := newSQLiteHubInMode(t, false, nil)
	before := readFileString(t, settingsPath)

	rr := putServerConfig(t, srv, `{"server":{"database":{"driver":"postgres"}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	if code, _ := rejectedKeys(t, rr); code != "layer0_rejected" {
		t.Errorf("error = %q, want layer0_rejected", code)
	}
	for _, body := range []string{`{"active_profile":"local"}`, `{"auto_inject_gcloud_adc":true}`} {
		if rr := putServerConfig(t, srv, body); rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("hosted PUT %s: expected 422, got %d: %s", body, rr.Code, rr.Body.String())
		}
	}
	if after := readFileString(t, settingsPath); after != before {
		t.Errorf("hosted rejections must not touch settings.yaml:\n%s", after)
	}
}

// Failure semantics: everything is validated before anything is written,
// and a DB failure discards the staged settings.yaml.
func TestWorkstation_PutServerConfig_NoPartialWrites(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"invalid Layer-1 value", `{"default_timezone":"Not/AZone","server":{"hub":{"port":9999}}}`, http.StatusUnprocessableEntity},
		{"invalid file-routed value", `{"quotas":{"enforce_broker_quotas":false},"server":{"hub":{"agent_endpoint":"not a url"}}}`, http.StatusBadRequest},
		{"unknown key", `{"quotas":{"enforce_broker_quotas":false},"server":{"hub":{"port":9999,"bogus":1}}}`, http.StatusUnprocessableEntity},
		{"revision conflict", `{"expected_revisions":{"quotas":999},"quotas":{"enforce_broker_quotas":false},"server":{"hub":{"port":9999}}}`, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settingsPath := workstationHome(t)
			srv, st, _ := newSQLiteHubInMode(t, true, map[string]string{
				"quotas": `{"enforce_broker_quotas":true}`,
			})
			before := readFileString(t, settingsPath)
			rowsBefore := hubSettingRevisions(t, st)

			rr := putServerConfig(t, srv, tc.body)
			if rr.Code != tc.status {
				t.Fatalf("expected %d, got %d: %s", tc.status, rr.Code, rr.Body.String())
			}
			if after := readFileString(t, settingsPath); after != before {
				t.Errorf("settings.yaml changed on a failed PUT:\n%s", after)
			}
			if after := hubSettingRevisions(t, st); !reflect.DeepEqual(after, rowsBefore) {
				t.Errorf("DB changed on a failed PUT:\nbefore: %v\nafter:  %v", rowsBefore, after)
			}
			noTempSettingsFiles(t, settingsPath)
		})
	}
}

// GET tells the UI whether Layer-0 is editable; on a workstation the full GET
// body echoed back is a 200 (hub_name echo included).
func TestWorkstation_GetLayer0EditableAndEcho(t *testing.T) {
	for _, workstation := range []bool{true, false} {
		workstationHome(t)
		srv, _, _ := newSQLiteHubInMode(t, workstation, nil)
		rr := httptest.NewRecorder()
		srv.handleAdminServerConfig(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
		var resp ServerConfigDBResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Layer0Editable != workstation {
			t.Errorf("workstation=%v: layer0_editable = %v", workstation, resp.Layer0Editable)
		}
		if workstation {
			if put := putServerConfig(t, srv, rr.Body.String()); put.Code != http.StatusOK {
				t.Errorf("workstation echo of the GET body: expected 200, got %d: %s", put.Code, put.Body.String())
			}
		}
	}
}

// Onboarding saves the gcloud ADC choice through the workstation-settings
// PATCH, which writes settings.yaml on a DB-backed SQLite hub too, and the
// onboarding status reads it back.
func TestWorkstation_OnboardingGcloudADC(t *testing.T) {
	settingsPath := workstationHome(t)
	srv, _, _ := newSQLiteHubInMode(t, true, nil)

	rr := httptest.NewRecorder()
	srv.handleWorkstationSettings(rr, adminRequest(http.MethodPatch, "/api/v1/system/workstation-settings",
		`{"auto_inject_gcloud_adc":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := yamlAt(readYAMLMap(t, settingsPath), "auto_inject_gcloud_adc"); got != true {
		t.Errorf("settings.yaml auto_inject_gcloud_adc = %v, want true", got)
	}
	if st := srv.computeOnboardingStatus(context.Background()); !st.AutoInjectGcloudADC {
		t.Error("onboarding status does not report the saved gcloud ADC choice")
	}
}

// Break-glass: a workstation hub started in admin mode stays in maintenance
// over a DB row that says otherwise, across later ops.Update re-applies; on
// a hosted hub the row wins.
func TestMaintenanceBreakGlass_ByMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		workstation bool
		wantEnabled bool
	}{
		{"workstation: startup admin mode wins", true, true},
		{"hosted: DB row wins", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempSettingsHome(t)
			srv, _, ops := newSQLiteHubInMode(t, tc.workstation, map[string]string{
				"maintenance": `{"admin_mode":false,"maintenance_message":"from db"}`,
			})
			// As at startup with SCION_SERVER_ADMIN_MODE=true.
			srv.config.AdminMode = true
			srv.maintenance = NewMaintenanceState(true, "")
			ApplyMaintenanceFromSnapshot(srv, ops.Snapshot()) // as initOperationalSettings does

			if got := srv.maintenance.IsEnabled(); got != tc.wantEnabled {
				t.Fatalf("after startup apply: enabled = %v, want %v", got, tc.wantEnabled)
			}
			unrelatedLifecycleUpdate(t, ops)
			if got := srv.maintenance.IsEnabled(); got != tc.wantEnabled {
				t.Errorf("after an unrelated ops.Update: enabled = %v, want %v", got, tc.wantEnabled)
			}
			if got := srv.maintenance.Message(); got != "from db" {
				t.Errorf("message = %q, want the row's message", got)
			}

			rr := httptest.NewRecorder()
			srv.handleAdminMaintenance(rr, adminRequest(http.MethodGet, "/api/v1/admin/maintenance", ""))
			var got struct {
				Enabled    bool `json:"enabled"`
				BreakGlass bool `json:"break_glass"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Enabled != tc.wantEnabled || got.BreakGlass != tc.workstation {
				t.Errorf("GET = %+v, want enabled=%v break_glass=%v", got, tc.wantEnabled, tc.workstation)
			}
		})
	}
}
