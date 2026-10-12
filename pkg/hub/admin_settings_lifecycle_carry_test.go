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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
	ops.envOverrides = map[string]bool{
		"server.hub.stalled_threshold":  true,
		"server.hub.start_max_duration": true,
	}
	fakeStore.seedWithOrigin("lifecycle", json.RawMessage(`{"stalled_threshold":"7m","soft_delete_retention":"72h","start_max_duration":"15m","start_claim_lease_ttl":"2m"}`), "seeded")

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
	// The start-claim keys follow the same rule.
	if got.StartMaxDuration != "" {
		t.Errorf("env-overridden start_max_duration carried forward: %q", got.StartMaxDuration)
	}
	if got.StartClaimLeaseTTL != "2m" {
		t.Errorf("start_claim_lease_ttl = %q, want 2m carried forward", got.StartClaimLeaseTTL)
	}
}

// DB-backed PUT: an explicit null or "" sent as the only lifecycle key
// clears that key and leaves the others unchanged (ptone/scion#3464).
func TestPutServerConfigDB_LoneExplicitClearClearsLifecycleKey(t *testing.T) {
	const stored = `{"auto_suspend_stalled":true,"stalled_threshold":"7m","soft_delete_retention":"72h","soft_delete_retain_files":true}`
	tru := true
	want := opsettings.LifecycleSettings{
		AutoSuspendStalled:    &tru,
		StalledThreshold:      "7m",
		SoftDeleteRetention:   "72h",
		SoftDeleteRetainFiles: &tru,
	}
	for _, tc := range []struct {
		name  string
		hub   string
		clear func(*opsettings.LifecycleSettings)
	}{
		{"stalled_threshold", `{"stalled_threshold": ""}`, func(l *opsettings.LifecycleSettings) { l.StalledThreshold = "" }},
		{"soft_delete_retention", `{"soft_delete_retention": ""}`, func(l *opsettings.LifecycleSettings) { l.SoftDeleteRetention = "" }},
		{"auto_suspend_stalled", `{"auto_suspend_stalled": null}`, func(l *opsettings.LifecycleSettings) { l.AutoSuspendStalled = nil }},
		{"soft_delete_retain_files", `{"soft_delete_retain_files": null}`, func(l *opsettings.LifecycleSettings) { l.SoftDeleteRetainFiles = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fakeStore, ops := newTestDBServer(t)
			fakeStore.seedWithOrigin("lifecycle", json.RawMessage(stored), "managed")

			rr := putServerConfigDB(t, srv, ops, `{"server": {"hub": `+tc.hub+`}}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
			}
			exp := want
			tc.clear(&exp)
			gotJSON, _ := json.Marshal(lifecycleRow(t, fakeStore))
			expJSON, _ := json.Marshal(exp)
			if !bytes.Equal(gotJSON, expJSON) {
				t.Errorf("lifecycle row = %s, want %s", gotJSON, expJSON)
			}
		})
	}
}

// lifecycleRaceStore simulates another replica writing the lifecycle row
// between the PUT's read of it and its write. With noRow, the first read
// reports the row as missing and the other replica creates it.
type lifecycleRaceStore struct {
	*fakeHubSettingStore
	noRow bool
	once  sync.Once
}

func (c *lifecycleRaceStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	if section != "lifecycle" {
		return c.fakeHubSettingStore.GetHubSetting(ctx, section)
	}
	row, err := c.fakeHubSettingStore.GetHubSetting(ctx, section)
	var snapshot *store.HubSetting
	if err == nil {
		cp := *row
		snapshot = &cp
	}
	raced := false
	c.once.Do(func() {
		raced = true
		_, _ = c.UpsertHubSetting(ctx, "lifecycle",
			json.RawMessage(`{"stalled_threshold":"9m"}`), "other-replica", -1, "managed")
	})
	if raced && c.noRow {
		return nil, store.ErrNotFound
	}
	if snapshot != nil {
		return snapshot, nil
	}
	return row, err
}

// The lifecycle carry-forward write is CAS-guarded on the revision it read,
// so a concurrent lifecycle write yields 409 instead of a lost update.
func TestPutServerConfigDB_LifecycleCarryForward_ConcurrentWrite409(t *testing.T) {
	fake := newFakeHubSettingStore()
	fake.seedWithOrigin("lifecycle", json.RawMessage(`{"stalled_threshold":"7m"}`), "managed")
	raceStore := &lifecycleRaceStore{fakeHubSettingStore: fake}
	ops := NewOperationalSettings(raceStore, emptyKoanf(), emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"soft_delete_retention":"24h"}}}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	got := lifecycleRow(t, fake)
	if got.StalledThreshold != "9m" || got.SoftDeleteRetention != "" {
		t.Errorf("the concurrent writer's row must stand, got %+v", got)
	}
}

// With no lifecycle row, the carry-forward write is create-only (revision
// 0), so a concurrent insert yields 409 rather than being overwritten.
func TestPutServerConfigDB_LifecycleCarryForward_NoRowConcurrentCreate409(t *testing.T) {
	fake := newFakeHubSettingStore()
	raceStore := &lifecycleRaceStore{fakeHubSettingStore: fake, noRow: true}
	ops := NewOperationalSettings(raceStore, emptyKoanf(), emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"soft_delete_retention":"24h"}}}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	got := lifecycleRow(t, fake)
	if got.StalledThreshold != "9m" || got.SoftDeleteRetention != "" {
		t.Errorf("the concurrent creator's row must stand, got %+v", got)
	}
}
