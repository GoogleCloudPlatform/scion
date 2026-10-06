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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

func lifecycleRow(t *testing.T, f *fakeHubSettingStore) opsettings.LifecycleSettings {
	t.Helper()
	f.mu.Lock()
	row := f.settings["lifecycle"]
	f.mu.Unlock()
	var lc opsettings.LifecycleSettings
	if err := json.Unmarshal(row.Value, &lc); err != nil {
		t.Fatal(err)
	}
	return lc
}

// DB-backed PUT: a partial server.hub update leaves every lifecycle key it
// omits unchanged (ptone/scion#3464). Each case changes one key and omits
// the others.
func TestPutServerConfigDB_PartialUpdateKeepsOmittedLifecycleSettings(t *testing.T) {
	const stored = `{"auto_suspend_stalled":true,"stalled_threshold":"7m","soft_delete_retention":"72h","soft_delete_retain_files":true,"start_max_duration":"15m"}`
	tru, fls := true, false
	want := opsettings.LifecycleSettings{
		AutoSuspendStalled:    &tru,
		StalledThreshold:      "7m",
		SoftDeleteRetention:   "72h",
		SoftDeleteRetainFiles: &tru,
		StartMaxDuration:      "15m",
	}
	for _, tc := range []struct {
		name string
		hub  string
		set  func(*opsettings.LifecycleSettings)
	}{
		{"auto_suspend_stalled", `{"auto_suspend_stalled": false}`, func(l *opsettings.LifecycleSettings) { l.AutoSuspendStalled = &fls }},
		{"stalled_threshold", `{"stalled_threshold": "9m"}`, func(l *opsettings.LifecycleSettings) { l.StalledThreshold = "9m" }},
		{"soft_delete_retention", `{"soft_delete_retention": "24h"}`, func(l *opsettings.LifecycleSettings) { l.SoftDeleteRetention = "24h" }},
		{"soft_delete_retain_files", `{"soft_delete_retain_files": false}`, func(l *opsettings.LifecycleSettings) { l.SoftDeleteRetainFiles = &fls }},
		// An explicit empty value still clears the key it names.
		{"explicit clear", `{"auto_suspend_stalled": true, "stalled_threshold": ""}`, func(l *opsettings.LifecycleSettings) { l.StalledThreshold = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fakeStore, ops := newTestDBServer(t)
			fakeStore.seedWithOrigin("lifecycle", json.RawMessage(stored), "managed")

			rr := httptest.NewRecorder()
			srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
				`{"server": {"hub": `+tc.hub+`}}`), ops)
			if rr.Code != http.StatusOK {
				t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
			}

			exp := want
			tc.set(&exp)
			got := lifecycleRow(t, fakeStore)
			gotJSON, _ := json.Marshal(got)
			expJSON, _ := json.Marshal(exp)
			if !bytes.Equal(gotJSON, expJSON) {
				t.Errorf("lifecycle row = %s, want %s", gotJSON, expJSON)
			}
		})
	}
}

// A seeded (non-managed) lifecycle row does not carry an env-overridden key
// into the managed row, like the access carry-forward.
func TestPutServerConfigDB_LifecycleCarryForwardSkipsEnvOverriddenSeededKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	ops.envOverrides = map[string]bool{"server.hub.stalled_threshold": true}
	fakeStore.seedWithOrigin("lifecycle", json.RawMessage(`{"stalled_threshold":"7m","soft_delete_retention":"72h"}`), "seeded")

	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server": {"hub": {"auto_suspend_stalled": true}}}`), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	got := lifecycleRow(t, fakeStore)
	if got.StalledThreshold != "" {
		t.Errorf("env-overridden stalled_threshold carried forward: %q", got.StalledThreshold)
	}
	if got.SoftDeleteRetention != "72h" {
		t.Errorf("soft_delete_retention = %q, want 72h carried forward", got.SoftDeleteRetention)
	}
}
