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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	// server.broker.enabled was already true: unchanged, so neither written
	// nor reported.
	wantRestart := []string{"active_profile", "server.broker.port", "server.hub.port"}
	if !reflect.DeepEqual(resp.Reload.RequiresRestart, wantRestart) {
		t.Errorf("reload.requires_restart = %v, want %v", resp.Reload.RequiresRestart, wantRestart)
	}
	wantFile := []string{"active_profile", "auto_inject_gcloud_adc", "server.broker.port", "server.hub.port", "server.log_level"}
	if !reflect.DeepEqual(resp.FileKeys, wantFile) {
		t.Errorf("file_keys = %v, want %v", resp.FileKeys, wantFile)
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
	if want := []string{"server.database.driver", "server.database.url"}; !reflect.DeepEqual(resp.Reload.RequiresRestart, want) {
		t.Errorf("requires_restart = %v, want %v", resp.Reload.RequiresRestart, want)
	}
	if after := hubSettingRevisions(t, st); !reflect.DeepEqual(after, rowsBefore) {
		t.Errorf("a Layer-0-only PUT must not write the DB:\nbefore: %v\nafter:  %v", rowsBefore, after)
	}
}

const workstationClearFixture = `# workstation settings
schema_version: "1"
server:
  log_format: json
  auth:
    dev_mode: true
  broker:
    enabled: true # co-located broker
    port: 9810
    broker_id: b-123
    broker_token: tok-secret
  message_broker:
    enabled: true
    type: inprocess
  storage:
    provider: gcs
    bucket: my-bucket
`

// Review r2 finding 1: on a workstation hub an explicit false, "" or zero
// in the body clears the value (omitempty fields lose the key); leaves the
// body does not carry are kept. Each row also checks the response names the
// changed leaves.
func TestWorkstation_PutServerConfig_ClearsFromPresence(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		absent   [][]string
		want     map[string]interface{}
		fileKeys []string
	}{
		{
			name:     "dev_mode false",
			body:     `{"server":{"auth":{"dev_mode":false}}}`,
			absent:   [][]string{{"server", "auth", "dev_mode"}},
			fileKeys: []string{"server.auth.dev_mode"},
		},
		{
			name:     "message_broker disabled",
			body:     `{"server":{"message_broker":{"enabled":false}}}`,
			absent:   [][]string{{"server", "message_broker", "enabled"}},
			want:     map[string]interface{}{"server.message_broker.type": "inprocess"},
			fileKeys: []string{"server.message_broker.enabled"},
		},
		{
			// What the admin page sends when the switch is turned off.
			name:     "UI message_broker switch-off",
			body:     `{"server":{"message_broker":{"enabled":false,"type":"inprocess"}}}`,
			absent:   [][]string{{"server", "message_broker", "enabled"}},
			fileKeys: []string{"server.message_broker.enabled"},
		},
		{
			name:     "log_format cleared",
			body:     `{"server":{"log_format":""}}`,
			absent:   [][]string{{"server", "log_format"}},
			fileKeys: []string{"server.log_format"},
		},
		{
			name:   "broker disabled with a new port; hub-owned identity survives",
			body:   `{"server":{"broker":{"enabled":false,"port":9800}}}`,
			absent: [][]string{{"server", "broker", "enabled"}},
			want: map[string]interface{}{
				"server.broker.port":         9800,
				"server.broker.broker_id":    "b-123",
				"server.broker.broker_token": "tok-secret",
			},
			fileKeys: []string{"server.broker.enabled", "server.broker.port"},
		},
		{
			name:     "storage provider changed, bucket kept when not sent",
			body:     `{"server":{"storage":{"provider":"local"}}}`,
			want:     map[string]interface{}{"server.storage.provider": "local", "server.storage.bucket": "my-bucket"},
			fileKeys: []string{"server.storage.provider"},
		},
		{
			name:     "storage bucket cleared explicitly",
			body:     `{"server":{"storage":{"provider":"local","bucket":""}}}`,
			absent:   [][]string{{"server", "storage", "bucket"}},
			want:     map[string]interface{}{"server.storage.provider": "local"},
			fileKeys: []string{"server.storage.bucket", "server.storage.provider"},
		},
		{
			name:     "null removes a block",
			body:     `{"server":{"storage":null}}`,
			absent:   [][]string{{"server", "storage"}},
			fileKeys: []string{"server.storage"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settingsPath := tempSettingsHome(t)
			if err := os.WriteFile(settingsPath, []byte(workstationClearFixture), 0o644); err != nil {
				t.Fatal(err)
			}
			srv, _, _ := newSQLiteHubInMode(t, true, nil)
			rr := putServerConfig(t, srv, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			resp := decodePut(t, rr)
			if !reflect.DeepEqual(resp.FileKeys, tc.fileKeys) {
				t.Errorf("file_keys = %v, want %v", resp.FileKeys, tc.fileKeys)
			}
			m := readYAMLMap(t, settingsPath)
			for _, p := range tc.absent {
				if v := yamlAt(m, p...); v != nil {
					t.Errorf("%s = %v, want it removed", strings.Join(p, "."), v)
				}
			}
			for k, want := range tc.want {
				if got := yamlAt(m, strings.Split(k, ".")...); !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %v, want %v", k, got, want)
				}
			}
			if data := readFileString(t, settingsPath); !strings.Contains(data, "# workstation settings") {
				t.Errorf("comments must survive the edit:\n%s", data)
			}
		})
	}
}

// Review r2 N2: a pure echo of the GET body writes nothing and reports
// nothing as requires_restart.
func TestWorkstation_PutServerConfig_EchoWritesNothing(t *testing.T) {
	settingsPath := workstationHome(t)
	srv, _, _ := newSQLiteHubInMode(t, true, nil)
	getRR := httptest.NewRecorder()
	srv.handleAdminServerConfig(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	before := readFileString(t, settingsPath)
	fi, _ := os.Stat(settingsPath)

	rr := putServerConfig(t, srv, getRR.Body.String())
	if rr.Code != http.StatusOK {
		t.Fatalf("echo: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodePut(t, rr)
	if len(resp.Reload.RequiresRestart) != 0 || len(resp.FileKeys) != 0 {
		t.Errorf("echo reported requires_restart=%v file_keys=%v, want none", resp.Reload.RequiresRestart, resp.FileKeys)
	}
	fi2, _ := os.Stat(settingsPath)
	if after := readFileString(t, settingsPath); after != before || !fi2.ModTime().Equal(fi.ModTime()) {
		t.Errorf("echo rewrote settings.yaml:\n%s", after)
	}
}

// Review r2 N1: a broker-token write (config.UpdateSetting, as broker
// registration does) racing a workstation PUT must not lose either update,
// and the lock order must not deadlock against the DB writes or the
// workstation-settings PATCH.
func TestWorkstation_PutServerConfig_ConcurrentSettingsWriters(t *testing.T) {
	settingsPath := workstationHome(t)
	srv, _, _ := newSQLiteHubInMode(t, true, nil)
	globalDir := filepath.Dir(settingsPath)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			var wg sync.WaitGroup
			wg.Add(3)
			go func(i int) {
				defer wg.Done()
				rr := putServerConfig(t, srv, fmt.Sprintf(`{"quotas":{"enforce_broker_quotas":%v},"server":{"hub":{"port":%d}}}`, i%2 == 0, 9000+i))
				if rr.Code != http.StatusOK {
					t.Errorf("PUT %d: %d %s", i, rr.Code, rr.Body.String())
				}
			}(i)
			go func(i int) {
				defer wg.Done()
				if err := config.UpdateSetting(globalDir, "hub.brokerToken", fmt.Sprintf("tok-%d", i), true); err != nil {
					t.Errorf("UpdateSetting %d: %v", i, err)
				}
			}(i)
			go func(i int) {
				defer wg.Done()
				rr := httptest.NewRecorder()
				srv.handleWorkstationSettings(rr, adminRequest(http.MethodPatch, "/api/v1/system/workstation-settings",
					fmt.Sprintf(`{"auto_inject_gcloud_adc":%v}`, i%2 == 0)))
				if rr.Code != http.StatusOK {
					t.Errorf("PATCH %d: %d %s", i, rr.Code, rr.Body.String())
				}
			}(i)
			wg.Wait()
			m := readYAMLMap(t, settingsPath)
			if got := yamlAt(m, "server", "hub", "port"); got != 9000+i {
				t.Errorf("round %d: server.hub.port = %v, want %d (PUT lost)", i, got, 9000+i)
			}
			if got := yamlAt(m, "server", "broker", "broker_token"); got != fmt.Sprintf("tok-%d", i) {
				t.Errorf("round %d: broker_token = %v, want tok-%d (token write lost)", i, got, i)
			}
			if got := yamlAt(m, "auto_inject_gcloud_adc"); (got == true) != (i%2 == 0) {
				t.Errorf("round %d: auto_inject_gcloud_adc = %v (PATCH lost)", i, got)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent settings writers deadlocked")
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

// Review r2 finding 2: the workstation UI now sends buildLayer1Payload's
// explicit empties for Layer-1 fields; the DB must clear them, and none of
// them may leak into settings.yaml.
func TestWorkstation_PutServerConfig_ClearsLayer1Values(t *testing.T) {
	settingsPath := workstationHome(t)
	srv, st, _ := newSQLiteHubInMode(t, true, map[string]string{
		"access":    `{"admin_emails":["seed-admin@example.com"],"authorized_domains":["example.com"]}`,
		"endpoints": `{"public_url":"https://hub.example.com"}`,
	})
	before := readFileString(t, settingsPath)

	rr := putServerConfig(t, srv, `{"server":{"hub":{"public_url":""},"auth":{"authorized_domains":[]}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, doc := hubSettingDocMap(t, st, "access"); doc["authorized_domains"] != nil && len(doc["authorized_domains"].([]interface{})) != 0 {
		t.Errorf("access.authorized_domains = %v, want cleared", doc["authorized_domains"])
	}
	if _, doc := hubSettingDocMap(t, st, "endpoints"); doc["public_url"] != nil && doc["public_url"] != "" {
		t.Errorf("endpoints.public_url = %v, want cleared", doc["public_url"])
	}
	if after := readFileString(t, settingsPath); after != before {
		t.Errorf("Layer-1 clears must not touch settings.yaml:\n%s", after)
	}
}

// Review r2 N5(b): during a workstation break-glass with an existing row, a
// message-only PUT keeps the row's admin_mode (false), so the hub leaves
// maintenance once restarted without the break-glass; live state stays on.
func TestMaintenanceBreakGlass_MessagePutKeepsRowAdminMode(t *testing.T) {
	tempSettingsHome(t)
	srv, st, ops := newSQLiteHubInMode(t, true, map[string]string{
		"maintenance": `{"admin_mode":false}`,
	})
	srv.config.AdminMode = true
	srv.maintenance = NewMaintenanceState(true, "")
	ApplyMaintenanceFromSnapshot(srv, ops.Snapshot())

	rr := httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, adminRequest(http.MethodPut, "/api/v1/admin/maintenance", `{"message":"upgrading"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	_, doc := hubSettingDocMap(t, st, "maintenance")
	if v, _ := doc["admin_mode"].(bool); v {
		t.Errorf("row admin_mode = true; a message-only PUT must not copy the break-glass into the row")
	}
	if doc["maintenance_message"] != "upgrading" {
		t.Errorf("row message = %v, want upgrading", doc["maintenance_message"])
	}
	if !srv.maintenance.IsEnabled() || srv.maintenance.Message() != "upgrading" {
		t.Errorf("live state = %v %q, want still in maintenance with the new message", srv.maintenance.IsEnabled(), srv.maintenance.Message())
	}
}

const hostedLayer0Fixture = `schema_version: "1"
server:
  hub:
    port: 9810
  auth:
    dev_mode: true
`

// On a hosted hub a Layer-0 leaf present in the body is checked against the
// GET view: a change, including an explicit zero over a stored true, is
// rejected with 422 layer0_rejected naming the leaf.
func TestHosted_PutServerConfig_Layer0ExplicitZeroRejected(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	if err := os.WriteFile(settingsPath, []byte(hostedLayer0Fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, st, _ := newSQLiteHubInMode(t, false, nil)
	rowsBefore := hubSettingRevisions(t, st)

	rr := putServerConfig(t, srv, `{"server":{"auth":{"dev_mode":false}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	code, keys := rejectedKeys(t, rr)
	if code != "layer0_rejected" || !reflect.DeepEqual(keys, []string{"server.auth.dev_mode"}) {
		t.Errorf("got %q %v, want layer0_rejected [server.auth.dev_mode]", code, keys)
	}
	if got := yamlAt(readYAMLMap(t, settingsPath), "server", "auth", "dev_mode"); got != true {
		t.Errorf("settings.yaml dev_mode = %v, want unchanged true", got)
	}
	if after := hubSettingRevisions(t, st); !reflect.DeepEqual(after, rowsBefore) {
		t.Errorf("DB changed on a rejected PUT")
	}
}

// Echoes are ignored on a hosted hub: zero-valued Layer-0 blocks (what a
// client sends for unset fields) and Layer-0 values equal to the GET view
// return 200 and write nothing.
func TestHosted_PutServerConfig_Layer0EchoIgnored(t *testing.T) {
	for _, body := range []string{
		`{"server":{"database":{"driver":"","url":""},"broker":{"enabled":false,"port":0},"storage":{},"log_format":""}}`,
		`{"server":{"hub":{"port":9810},"auth":{"dev_mode":true}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			settingsPath := tempSettingsHome(t)
			if err := os.WriteFile(settingsPath, []byte(hostedLayer0Fixture), 0o644); err != nil {
				t.Fatal(err)
			}
			srv, st, _ := newSQLiteHubInMode(t, false, nil)
			before := readFileString(t, settingsPath)
			rowsBefore := hubSettingRevisions(t, st)

			rr := putServerConfig(t, srv, body)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			if resp := decodePut(t, rr); len(resp.Reload.Applied) != 0 || len(resp.FileKeys) != 0 {
				t.Errorf("echo applied %v / file_keys %v, want nothing", resp.Reload.Applied, resp.FileKeys)
			}
			if after := readFileString(t, settingsPath); after != before {
				t.Errorf("settings.yaml changed:\n%s", after)
			}
			if after := hubSettingRevisions(t, st); !reflect.DeepEqual(after, rowsBefore) {
				t.Errorf("DB changed on an echo")
			}
		})
	}
}
