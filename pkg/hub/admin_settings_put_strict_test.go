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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// strictPutCases are PUT bodies that carry a valid change next to a key the
// server-config PUT would drop (ptone/scion#3463). The zero values matter:
// a key absent from the GET view used to pass as an "echo" when its value
// was false, "" or null.
var strictPutCases = []struct {
	name string
	body string
	keys []string
}{
	{
		name: "unknown top-level key",
		body: `{"default_model": "m1", "bogus_setting": false}`,
		keys: []string{"bogus_setting"},
	},
	{
		name: "unknown nested key",
		body: `{"default_model": "m1", "server": {"hub": {"auto_suspend_stalled": true, "bogus": ""}}}`,
		keys: []string{"server.hub.bogus"},
	},
	{
		name: "flat dotted key",
		body: `{"default_model": "m1", "server.hub.auto_suspend_stalled": false}`,
		keys: []string{"server.hub.auto_suspend_stalled"},
	},
	{
		name: "flat dotted key and unknown key",
		body: `{"server.hub.auto_suspend_stalled": true, "nope": null, "default_model": "m1"}`,
		keys: []string{"nope", "server.hub.auto_suspend_stalled"},
	},
}

func assertRejectedKeys(t *testing.T, rr *httptest.ResponseRecorder, want []string) {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error string   `json:"error"`
		Keys  []string `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 422 body: %v", err)
	}
	sort.Strings(resp.Keys)
	if !reflect.DeepEqual(resp.Keys, want) {
		t.Errorf("rejected keys = %v, want %v", resp.Keys, want)
	}
	if resp.Error != "unpersisted_keys_rejected" {
		t.Errorf("error = %q, want unpersisted_keys_rejected", resp.Error)
	}
}

// File-backed PUT: an unknown or flat dotted key is a 422 naming it, and
// settings.yaml is left exactly as it was (nothing of the valid part of the
// body is applied either).
func TestHandlePutServerConfig_RejectsUnknownAndDottedKeys(t *testing.T) {
	for _, tc := range strictPutCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
				t.Fatal(err)
			}
			before := []byte("schema_version: \"1\"\ndefault_model: m0\n")
			if err := os.WriteFile(settingsPath, before, 0644); err != nil {
				t.Fatal(err)
			}

			srv := &Server{}
			rr := httptest.NewRecorder()
			srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", tc.body))
			assertRejectedKeys(t, rr, tc.keys)

			after, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("settings.yaml changed by a rejected PUT:\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

// File-backed PUT: a valid nested update still saves, and sending the GET
// body back (read-only fields included) is still a 200.
func TestHandlePutServerConfig_ValidNestedUpdateAndGetEchoStillSave(t *testing.T) {
	srv := &Server{}
	rr, settingsPath := fileModePutServerConfig(t, srv,
		`{"default_model": "m1", "server": {"hub": {"auto_suspend_stalled": true}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid PUT: %d %s", rr.Code, rr.Body.String())
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("default_model: m1")) || !bytes.Contains(data, []byte("auto_suspend_stalled: true")) {
		t.Errorf("valid PUT not persisted: %s", data)
	}

	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", getRR.Code, getRR.Body.String())
	}
	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
	if echo.Code != http.StatusOK {
		t.Fatalf("GET body echoed back: %d %s", echo.Code, echo.Body.String())
	}

	// A changed read-only GET field is not an echo.
	rr = httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"settings_tier": "db"}`))
	assertRejectedKeys(t, rr, []string{"settings_tier"})
}

func snapshotHubSettings(f *fakeHubSettingStore) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.settings {
		out[k] = string(v.Value)
	}
	return out
}

// DB-backed PUT: an unknown or flat dotted key is a 422 naming it, whatever
// its value, and no section row is written.
func TestPutServerConfigDB_RejectsUnknownAndDottedKeys(t *testing.T) {
	for _, tc := range strictPutCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fakeStore, ops := newTestDBServer(t)
			fakeStore.seedWithOrigin("agent_defaults", json.RawMessage(`{"default_model":"m0"}`), "managed")
			fakeStore.seedWithOrigin("lifecycle", json.RawMessage(`{"auto_suspend_stalled":false}`), "managed")
			before := snapshotHubSettings(fakeStore)

			rr := httptest.NewRecorder()
			srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", tc.body), ops)
			assertRejectedKeys(t, rr, tc.keys)

			if after := snapshotHubSettings(fakeStore); !reflect.DeepEqual(before, after) {
				t.Errorf("hub settings changed by a rejected PUT:\nbefore: %v\nafter:  %v", before, after)
			}
		})
	}
}

// DB-backed PUT: a valid nested update still saves.
func TestPutServerConfigDB_ValidNestedUpdateStillSaves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"default_model": "m1", "server": {"hub": {"auto_suspend_stalled": true}}}`), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid PUT: %d %s", rr.Code, rr.Body.String())
	}
	got := snapshotHubSettings(fakeStore)
	var lc opsettings.LifecycleSettings
	if err := json.Unmarshal([]byte(got["lifecycle"]), &lc); err != nil {
		t.Fatal(err)
	}
	if lc.AutoSuspendStalled == nil || !*lc.AutoSuspendStalled {
		t.Errorf("lifecycle row = %s, want auto_suspend_stalled true", got["lifecycle"])
	}
	var ad opsettings.AgentDefaultsSettings
	if err := json.Unmarshal([]byte(got["agent_defaults"]), &ad); err != nil {
		t.Fatal(err)
	}
	if ad.DefaultModel != "m1" {
		t.Errorf("agent_defaults row = %s, want default_model m1", got["agent_defaults"])
	}
}
