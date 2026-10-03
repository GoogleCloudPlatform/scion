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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- helpers ---

func retireTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), &buf
}

func countLines(s, substr string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func mustRow(t *testing.T, st store.HubSettingStore, section string) (*store.HubSetting, map[string]any) {
	t.Helper()
	row, doc, err := hubSettingDoc(context.Background(), st, section)
	if err != nil {
		t.Fatalf("read %s: %v", section, err)
	}
	if row == nil {
		t.Fatalf("%s row missing", section)
	}
	return row, doc
}

func profileTZ(doc map[string]any, profile string) (any, bool) {
	p, ok := doc[profile].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := p["timezone"]
	return v, ok
}

const strippedMsg = "stripped from the hub profiles settings"

// --- five-case table, DB tier ---

func TestRetireProfileTimezones_DBTier_Table(t *testing.T) {
	tests := []struct {
		name     string
		def      string // agent_defaults.default_timezone; "-" = no agent_defaults row
		profiles string
		values   int    // legacy keys in the profiles row
		wantDef  string // default_timezone afterwards
		wantCopy bool
		action   string // substring of each non-empty value's action
	}{
		{
			name:     "Z empty: only an empty value",
			def:      "",
			profiles: `{"a":{"runtime":"docker","timezone":""},"b":{"runtime":"docker"}}`,
			values:   1, wantDef: "", action: "empty value removed",
		},
		{
			name:     "D empty, one zone: copied",
			def:      "",
			profiles: `{"a":{"runtime":"docker","timezone":"Asia/Kathmandu"},"b.c":{"runtime":"docker","timezone":"Asia/Kathmandu"},"e":{"timezone":""}}`,
			values:   3, wantDef: "Asia/Kathmandu", wantCopy: true, action: "copied into agent_defaults.default_timezone",
		},
		{
			name:     "D empty, one zone, no agent_defaults row: copied with create-only write",
			def:      "-",
			profiles: `{"a":{"runtime":"docker","timezone":"Asia/Tokyo"}}`,
			values:   1, wantDef: "Asia/Tokyo", wantCopy: true, action: "copied into agent_defaults.default_timezone",
		},
		{
			name:     "D set, Z = {D}: nothing to copy",
			def:      "Asia/Tokyo",
			profiles: `{"a":{"runtime":"docker","timezone":"Asia/Tokyo"}}`,
			values:   1, wantDef: "Asia/Tokyo", action: "nothing to copy",
		},
		{
			name:     "D set, Z != {D}: D kept",
			def:      "UTC",
			profiles: `{"a":{"runtime":"docker","timezone":"Asia/Tokyo"},"b":{"runtime":"docker","timezone":"Asia/Kathmandu"}}`,
			values:   2, wantDef: "UTC", action: "is kept",
		},
		{
			name:     "D empty, several zones: nothing copied",
			def:      "",
			profiles: `{"a":{"runtime":"docker","timezone":"Asia/Tokyo"},"b":{"runtime":"docker","timezone":"Asia/Kathmandu"}}`,
			values:   2, wantDef: "", action: "profiles set different zones",
		},
		{
			name:     "D empty, one invalid zone: dropped",
			def:      "",
			profiles: `{"a":{"runtime":"docker","timezone":"Not/AZone"}}`,
			values:   1, wantDef: "", action: "not a valid IANA time zone name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st := newFakeHubSettingStore()
			st.seedWithOrigin("profiles", json.RawMessage(tt.profiles), "managed")
			if tt.def != "-" {
				st.seedWithOrigin("agent_defaults", json.RawMessage(`{"default_template":"claude","default_timezone":"`+tt.def+`"}`), "seeded")
			}
			ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
			if _, err := ops.Refresh(ctx); err != nil { // as at boot
				t.Fatal(err)
			}
			log, buf := retireTestLogger()

			if err := RetireProfileTimezones(ctx, ops, ProfileTimezoneRetireInput{DBTier: true}, log); err != nil {
				t.Fatalf("RetireProfileTimezones: %v", err)
			}

			// profiles: every key stripped, origin kept, one revision bump.
			profRow, profDoc := mustRow(t, st, "profiles")
			if got := config.LegacyProfileTimezonesFromProfiles(profDoc); len(got) != 0 {
				t.Errorf("profiles still carry timezone: %+v", got)
			}
			if profRow.Revision != 2 || profRow.UpdatedBy != profileTimezoneRetireUpdatedBy || profRow.Origin != "managed" {
				t.Errorf("profiles row rev=%d by=%q origin=%q; want 2/%s/managed", profRow.Revision, profRow.UpdatedBy, profRow.Origin, profileTimezoneRetireUpdatedBy)
			}
			if _, ok := profDoc["a"].(map[string]any)["runtime"]; !ok {
				t.Errorf("other profile keys lost: %v", profDoc)
			}

			// agent_defaults.
			adRow, adDoc, err := hubSettingDoc(ctx, st, "agent_defaults")
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantCopy {
				wantRev := int64(2)
				if tt.def == "-" {
					wantRev = 1
				}
				if adRow == nil || adRow.Revision != wantRev || adRow.UpdatedBy != profileTimezoneRetireUpdatedBy || adRow.Origin != "managed" {
					t.Fatalf("agent_defaults row %+v; want rev %d, updatedBy %s, origin managed", adRow, wantRev, profileTimezoneRetireUpdatedBy)
				}
				if tt.def != "-" && adDoc["default_template"] != "claude" {
					t.Errorf("agent_defaults lost other keys: %v", adDoc)
				}
				if countLines(buf.String(), "agent_defaults is now admin-managed") != 1 {
					t.Errorf("want one admin-managed line, log:\n%s", buf)
				}
			} else {
				if tt.def == "-" {
					if adRow != nil {
						t.Errorf("agent_defaults row created without a copy: %+v", adRow)
					}
				} else if adRow.Revision != 1 || adRow.Origin != "seeded" {
					t.Errorf("agent_defaults written without a copy: rev=%d origin=%q", adRow.Revision, adRow.Origin)
				}
				if strings.Contains(buf.String(), "admin-managed") {
					t.Errorf("admin-managed line logged without a copy:\n%s", buf)
				}
			}
			if got, _ := adDoc["default_timezone"].(string); got != tt.wantDef {
				t.Errorf("default_timezone = %q, want %q", got, tt.wantDef)
			}
			if got := ops.Snapshot().DefaultTimezone; got != tt.wantDef {
				t.Errorf("snapshot default_timezone = %q, want %q (cache must follow the write)", got, tt.wantDef)
			}

			// One warning per stripped value.
			out := buf.String()
			if got := countLines(out, strippedMsg); got != tt.values {
				t.Errorf("stripped warnings = %d, want %d; log:\n%s", got, tt.values, out)
			}
			if !strings.Contains(out, tt.action) {
				t.Errorf("log lacks action %q:\n%s", tt.action, out)
			}

			// Idempotent: a second run writes nothing and logs nothing.
			log2, buf2 := retireTestLogger()
			if err := RetireProfileTimezones(ctx, ops, ProfileTimezoneRetireInput{DBTier: true}, log2); err != nil {
				t.Fatalf("second run: %v", err)
			}
			profRow2, _ := mustRow(t, st, "profiles")
			if profRow2.Revision != profRow.Revision {
				t.Errorf("second run bumped profiles revision %d -> %d", profRow.Revision, profRow2.Revision)
			}
			if adRow != nil {
				adRow2, _ := mustRow(t, st, "agent_defaults")
				if adRow2.Revision != adRow.Revision {
					t.Errorf("second run bumped agent_defaults revision %d -> %d", adRow.Revision, adRow2.Revision)
				}
			}
			if buf2.Len() != 0 {
				t.Errorf("second run logged:\n%s", buf2)
			}
		})
	}
}

// TestRetireProfileTimezones_DBTier_NoValues is a no-op on a hub that never
// set the field: no writes, no log.
func TestRetireProfileTimezones_DBTier_NoValues(t *testing.T) {
	st := newFakeHubSettingStore()
	st.seedWithOrigin("profiles", json.RawMessage(`{"a":{"runtime":"docker"}}`), "seeded")
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	log, buf := retireTestLogger()
	if err := RetireProfileTimezones(context.Background(), ops, ProfileTimezoneRetireInput{DBTier: true}, log); err != nil {
		t.Fatal(err)
	}
	if row, _ := mustRow(t, st, "profiles"); row.Revision != 1 {
		t.Errorf("profiles revision = %d, want 1", row.Revision)
	}
	if _, err := st.GetHubSetting(context.Background(), "agent_defaults"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("agent_defaults row created: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("logged:\n%s", buf)
	}
}

// --- crash between the two writes ---

// failingProfilesStore fails the next profiles write, simulating a crash
// after agent_defaults was written.
type failingProfilesStore struct {
	*fakeHubSettingStore
	failProfiles bool
}

var errInjectedCrash = errors.New("injected crash")

func (s *failingProfilesStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	if section == "profiles" && s.failProfiles {
		return nil, errInjectedCrash
	}
	return s.fakeHubSettingStore.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, origin)
}

func TestRetireProfileTimezones_CrashBetweenWritesConverges(t *testing.T) {
	ctx := context.Background()
	const profiles = `{"a":{"runtime":"docker","timezone":"Asia/Kathmandu"},"b":{"runtime":"docker"}}`

	// Reference: one uninterrupted run.
	ref := newFakeHubSettingStore()
	ref.seedWithOrigin("profiles", json.RawMessage(profiles), "managed")
	ref.seedWithOrigin("agent_defaults", json.RawMessage(`{"default_timezone":""}`), "seeded")
	refLog, _ := retireTestLogger()
	if err := RetireProfileTimezones(ctx, NewOperationalSettings(ref, emptyKoanf(), emptyKoanf()), ProfileTimezoneRetireInput{DBTier: true}, refLog); err != nil {
		t.Fatal(err)
	}

	st := &failingProfilesStore{fakeHubSettingStore: newFakeHubSettingStore(), failProfiles: true}
	st.seedWithOrigin("profiles", json.RawMessage(profiles), "managed")
	st.seedWithOrigin("agent_defaults", json.RawMessage(`{"default_timezone":""}`), "seeded")

	// Boot 1 crashes after agent_defaults is written.
	log1, _ := retireTestLogger()
	err := RetireProfileTimezones(ctx, NewOperationalSettings(st, emptyKoanf(), emptyKoanf()), ProfileTimezoneRetireInput{DBTier: true}, log1)
	if !errors.Is(err, errInjectedCrash) {
		t.Fatalf("boot 1 err = %v, want injected crash", err)
	}
	_, adDoc := mustRow(t, st, "agent_defaults")
	if adDoc["default_timezone"] != "Asia/Kathmandu" {
		t.Fatalf("agent_defaults not written before the crash: %v", adDoc)
	}
	_, profDoc := mustRow(t, st, "profiles")
	if _, ok := profileTZ(profDoc, "a"); !ok {
		t.Fatalf("profiles stripped despite the injected failure: %v", profDoc)
	}

	// Boot 2 reruns as "Z = {D}" and only strips.
	st.failProfiles = false
	log2, buf2 := retireTestLogger()
	if err := RetireProfileTimezones(ctx, NewOperationalSettings(st, emptyKoanf(), emptyKoanf()), ProfileTimezoneRetireInput{DBTier: true}, log2); err != nil {
		t.Fatalf("boot 2: %v", err)
	}
	if !strings.Contains(buf2.String(), "nothing to copy") || strings.Contains(buf2.String(), "admin-managed") {
		t.Errorf("rerun should only strip; log:\n%s", buf2)
	}

	for _, section := range []string{"profiles", "agent_defaults"} {
		gotRow, gotDoc := mustRow(t, st, section)
		refRow, refDoc := mustRow(t, ref, section)
		g, _ := json.Marshal(gotDoc)
		r, _ := json.Marshal(refDoc)
		if string(g) != string(r) || gotRow.Origin != refRow.Origin || gotRow.UpdatedBy != refRow.UpdatedBy {
			t.Errorf("%s after crash+rerun = %s (origin %q, by %q); uninterrupted run = %s (origin %q, by %q)",
				section, g, gotRow.Origin, gotRow.UpdatedBy, r, refRow.Origin, refRow.UpdatedBy)
		}
	}
}

// --- seeded vs managed rows and settings-file values ---

func legacyScan(def string, values ...config.LegacyProfileTimezone) config.SettingsFileProfileTimezoneScan {
	return config.SettingsFileProfileTimezoneScan{Path: "/etc/scion/settings.yaml", DefaultTimezone: def, ProfileTimezones: values}
}

// TestRetireProfileTimezones_YAMLOnlyKey_SeededRow covers a value that only
// exists in the settings file while the profiles row is seeded from it (the
// seed extraction drops the key). It counts towards the table, the row is not
// written, and the warning says to remove the key from the file.
func TestRetireProfileTimezones_YAMLOnlyKey_SeededRow(t *testing.T) {
	tests := []struct {
		name     string
		def      string
		values   []config.LegacyProfileTimezone
		wantDef  string
		wantCopy bool
	}{
		{name: "one zone copies", values: []config.LegacyProfileTimezone{{Profile: "team.west", Timezone: "America/Los_Angeles"}}, wantDef: "America/Los_Angeles", wantCopy: true},
		{name: "two zones copy nothing", values: []config.LegacyProfileTimezone{{Profile: "a", Timezone: "Asia/Tokyo"}, {Profile: "b", Timezone: "UTC"}}},
		{name: "default kept", def: "Europe/Paris", values: []config.LegacyProfileTimezone{{Profile: "a", Timezone: "Asia/Tokyo"}}, wantDef: "Europe/Paris"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st := newFakeHubSettingStore()
			st.seedWithOrigin("profiles", json.RawMessage(`{"a":{"runtime":"docker"}}`), "seeded")
			st.seedWithOrigin("agent_defaults", json.RawMessage(`{"default_timezone":"`+tt.def+`"}`), "seeded")
			ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
			log, buf := retireTestLogger()
			// The file's own default_timezone does not count on the DB tier.
			in := ProfileTimezoneRetireInput{DBTier: true, File: legacyScan("Asia/Seoul", tt.values...)}
			if err := RetireProfileTimezones(ctx, ops, in, log); err != nil {
				t.Fatal(err)
			}

			profRow, _ := mustRow(t, st, "profiles")
			if profRow.Revision != 1 || profRow.Origin != "seeded" {
				t.Errorf("profiles row written: rev=%d origin=%q", profRow.Revision, profRow.Origin)
			}
			adRow, adDoc := mustRow(t, st, "agent_defaults")
			if got, _ := adDoc["default_timezone"].(string); got != tt.wantDef {
				t.Errorf("default_timezone = %q, want %q", got, tt.wantDef)
			}
			wantOrigin, wantRev := "seeded", int64(1)
			if tt.wantCopy {
				wantOrigin, wantRev = "managed", 2
			}
			if adRow.Origin != wantOrigin || adRow.Revision != wantRev {
				t.Errorf("agent_defaults rev=%d origin=%q, want %d/%s", adRow.Revision, adRow.Origin, wantRev, wantOrigin)
			}
			out := buf.String()
			if got := countLines(out, "remove it from the file"); got != len(tt.values) {
				t.Errorf("remove warnings = %d, want %d:\n%s", got, len(tt.values), out)
			}
			if strings.Contains(out, "default_timezone: ") || strings.Contains(out, strippedMsg) {
				t.Errorf("DB-tier yaml-only warning must not suggest adding a line or claim a strip:\n%s", out)
			}
		})
	}
}

// TestRetireProfileTimezones_ManagedRow strips a managed profiles row and
// ignores settings-file values: the file no longer feeds that row.
func TestRetireProfileTimezones_ManagedRow(t *testing.T) {
	ctx := context.Background()
	st := newFakeHubSettingStore()
	st.seedWithOrigin("profiles", json.RawMessage(`{"team.east":{"runtime":"docker","timezone":"America/New_York"}}`), "managed")
	st.seedWithOrigin("agent_defaults", json.RawMessage(`{"default_template":"claude"}`), "seeded")
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	log, buf := retireTestLogger()
	in := ProfileTimezoneRetireInput{DBTier: true, File: legacyScan("", config.LegacyProfileTimezone{Profile: "old", Timezone: "Asia/Tokyo"})}
	if err := RetireProfileTimezones(ctx, ops, in, log); err != nil {
		t.Fatal(err)
	}
	profRow, profDoc := mustRow(t, st, "profiles")
	if _, ok := profileTZ(profDoc, "team.east"); ok || profRow.Origin != "managed" || profRow.Revision != 2 {
		t.Errorf("profiles row %+v doc %v; want stripped, managed, rev 2", profRow, profDoc)
	}
	adRow, adDoc := mustRow(t, st, "agent_defaults")
	if adDoc["default_timezone"] != "America/New_York" || adRow.Origin != "managed" {
		t.Errorf("agent_defaults %v origin %q; want the row's single zone copied, origin managed", adDoc, adRow.Origin)
	}
	out := buf.String()
	if strings.Contains(out, "Asia/Tokyo") || strings.Contains(out, "remove it from the file") {
		t.Errorf("settings-file value counted for a managed row:\n%s", out)
	}
	if countLines(out, strippedMsg) != 1 {
		t.Errorf("want one strip warning:\n%s", out)
	}
}

// --- file tier ---

func TestRetireProfileTimezones_FileTier_WarnsAndLeavesFile(t *testing.T) {
	const body = "schema_version: \"1\"\nprofiles:\n  team.west:\n    runtime: docker\n    timezone: Asia/Kathmandu\n  blank:\n    runtime: docker\n    timezone: \"\"\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	scan, err := config.ScanSettingsFileProfileTimezones(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := newFakeHubSettingStore()
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	log, buf := retireTestLogger()
	// A SQLite hub has operational settings but the file is authoritative.
	if err := RetireProfileTimezones(context.Background(), ops, ProfileTimezoneRetireInput{DBTier: false, File: scan}, log); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if got := countLines(out, "settings file not modified"); got != 2 {
		t.Errorf("want one warning per value (2), got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "profile=team.west") {
		t.Errorf("warning lacks the dotted profile name:\n%s", out)
	}
	if !strings.Contains(out, "`default_timezone: Asia/Kathmandu`") || !strings.Contains(out, "agent_defaults.default_timezone") {
		t.Errorf("warning lacks the line to add:\n%s", out)
	}
	if !strings.Contains(out, "empty value removed") {
		t.Errorf("empty value not reported:\n%s", out)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != body {
		t.Errorf("settings file changed (err %v):\n%s", err, data)
	}
	if rows, _ := st.ListHubSettings(context.Background()); len(rows) != 0 {
		t.Errorf("file tier wrote hub_settings: %+v", rows)
	}

	// With a default already in the file, no line is suggested.
	buf.Reset()
	scan.DefaultTimezone = "UTC"
	if err := RetireProfileTimezones(context.Background(), nil, ProfileTimezoneRetireInput{File: scan}, log); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "`default_timezone:") || !strings.Contains(buf.String(), "is kept") {
		t.Errorf("unexpected hint with a default set:\n%s", buf)
	}
}

// --- 422 on both PUT handlers ---

func assertRemovedProfileTimezone422(t *testing.T, rr *httptest.ResponseRecorder, profile string) {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := "profiles." + profile + ".timezone was removed; set agent_defaults.default_timezone, or a hub/broker-scope TZ environment variable"
	if resp.Error.Message != want {
		t.Errorf("message = %q, want %q", resp.Error.Message, want)
	}
	if resp.Error.Code != ErrCodeValidationError {
		t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeValidationError)
	}
}

func TestPutServerConfig_RemovedProfileTimezone422(t *testing.T) {
	for _, tz := range []string{`""`, `"Asia/Tokyo"`, `null`} {
		body := `{"profiles":{"team.west":{"runtime":"docker","timezone":` + tz + `}}}`

		t.Run("db "+tz, func(t *testing.T) {
			srv, st, ops := newTestDBServer(t)
			rr := httptest.NewRecorder()
			srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
			assertRemovedProfileTimezone422(t, rr, "team.west")
			if rows, _ := st.ListHubSettings(context.Background()); len(rows) != 0 {
				t.Errorf("rejected PUT wrote hub_settings: %+v", rows)
			}
		})

		t.Run("file "+tz, func(t *testing.T) {
			srv, _, settingsPath := fileModeGCPIdentityServer(t)
			before, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
			assertRemovedProfileTimezone422(t, rr, "team.west")
			after, _ := os.ReadFile(settingsPath)
			if string(after) != string(before) {
				t.Errorf("rejected PUT changed settings.yaml:\n%s", after)
			}
		})
	}
}

// TestPutServerConfigDB_ProfilesWithoutTimezoneAccepted keeps ordinary
// profile saves working.
func TestPutServerConfigDB_ProfilesWithoutTimezoneAccepted(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"profiles":{"pacific":{"runtime":"docker"}}}`), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
}
