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
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type durationErrorBody struct {
	Error struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Details map[string]interface{} `json:"details"`
	} `json:"error"`
}

// storedSectionKey returns the stored value of key in the section row, and
// whether the row holds the key.
func storedSectionKey(t *testing.T, fakeStore *fakeHubSettingStore, section, key string) (string, bool) {
	t.Helper()
	fakeStore.mu.Lock()
	row, ok := fakeStore.settings[section]
	fakeStore.mu.Unlock()
	if !ok || row == nil {
		return "", false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(row.Value, &m); err != nil {
		t.Fatalf("unmarshal %s row: %v", section, err)
	}
	raw, ok := m[key]
	if !ok {
		return "", false
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s.%s: %v", section, key, err)
	}
	return v, true
}

// A duration-valued key (opsettings Section.Durations) saves a valid
// duration, refuses a value that is not one with a 422 naming the field and
// writes nothing, and clears on "" and null (ptone/scion#4452).
func TestPutServerConfigDB_DurationSettingsValidated(t *testing.T) {
	cases := []struct {
		name    string
		section string
		key     string
		field   string
		body    func(v string) string
		clears  []string // values that clear the key
	}{
		{
			name: "stalled_threshold", section: "lifecycle", key: "stalled_threshold",
			field:  "server.hub.stalled_threshold",
			body:   func(v string) string { return `{"server": {"hub": {"stalled_threshold": ` + v + `}}}` },
			clears: []string{`""`, `null`},
		},
		{
			name: "soft_delete_retention", section: "lifecycle", key: "soft_delete_retention",
			field:  "server.hub.soft_delete_retention",
			body:   func(v string) string { return `{"server": {"hub": {"soft_delete_retention": ` + v + `}}}` },
			clears: []string{`""`, `null`},
		},
		{
			name: "default_max_duration", section: "agent_defaults", key: "default_max_duration",
			field:  "default_max_duration",
			body:   func(v string) string { return `{"default_max_duration": ` + v + `}` },
			clears: []string{`""`, `null`},
		},
		{
			name: "federation refresh_interval", section: "federation", key: "refresh_interval",
			field: "server.federation.refresh_interval",
			// federation is a top-level key of the request body.
			body:   func(v string) string { return `{"federation": {"refresh_interval": ` + v + `}}` },
			clears: []string{`""`},
		},
		{
			// Regression test for the debounce_interval request body
			// (ptone/scion#4451).
			name: "federation debounce_interval", section: "federation", key: "debounce_interval",
			field:  "server.federation.debounce_interval",
			body:   func(v string) string { return `{"federation": {"debounce_interval": ` + v + `}}` },
			clears: []string{`""`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fakeStore, ops := newTestDBServer(t)

			if rr := putStartClaimConfigDB(t, srv, ops, tc.body(`"45m"`)); rr.Code != http.StatusOK {
				t.Fatalf("valid value: %d %s", rr.Code, rr.Body.String())
			}
			if got, _ := storedSectionKey(t, fakeStore, tc.section, tc.key); got != "45m" {
				t.Fatalf("stored %s = %q, want 45m", tc.key, got)
			}

			for _, bad := range []string{`"abc"`, `"30"`, `"1hour"`} {
				rr := putStartClaimConfigDB(t, srv, ops, tc.body(bad))
				if rr.Code != http.StatusUnprocessableEntity {
					t.Fatalf("%s: status %d, want 422: %s", bad, rr.Code, rr.Body.String())
				}
				var b durationErrorBody
				if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
					t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
				}
				if b.Error.Code != ErrCodeValidationError || b.Error.Details["field"] != tc.field {
					t.Errorf("%s: code %q field %v, want %q %q", bad, b.Error.Code, b.Error.Details["field"], ErrCodeValidationError, tc.field)
				}
				if !strings.Contains(b.Error.Message, tc.key) {
					t.Errorf("%s: message %q does not name %s", bad, b.Error.Message, tc.key)
				}
				if got, _ := storedSectionKey(t, fakeStore, tc.section, tc.key); got != "45m" {
					t.Errorf("%s: stored %s = %q after a refused save, want 45m", bad, tc.key, got)
				}
			}

			for _, clear := range tc.clears {
				if rr := putStartClaimConfigDB(t, srv, ops, tc.body(`"45m"`)); rr.Code != http.StatusOK {
					t.Fatalf("re-set before clear: %d %s", rr.Code, rr.Body.String())
				}
				if rr := putStartClaimConfigDB(t, srv, ops, tc.body(clear)); rr.Code != http.StatusOK {
					t.Fatalf("clear %s: %d %s", clear, rr.Code, rr.Body.String())
				}
				if got, ok := storedSectionKey(t, fakeStore, tc.section, tc.key); ok && got != "" {
					t.Errorf("clear %s: stored %s = %q, want cleared", clear, tc.key, got)
				}
			}
		})
	}
}

// storedNestedKey returns the string at path in the section row, or "" if
// the row or any step of the path is missing.
func storedNestedKey(t *testing.T, fakeStore *fakeHubSettingStore, section string, path ...string) string {
	t.Helper()
	fakeStore.mu.Lock()
	row, ok := fakeStore.settings[section]
	fakeStore.mu.Unlock()
	if !ok || row == nil {
		return ""
	}
	var cur interface{}
	if err := json.Unmarshal(row.Value, &cur); err != nil {
		t.Fatalf("unmarshal %s row: %v", section, err)
	}
	for _, p := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = m[p]
	}
	v, _ := cur.(string)
	return v
}

// A duration-valued key below the top level of its section document
// (opsettings Section.NestedDurations) saves a valid duration, refuses a
// value that is not one with a 422 naming the full key path and writes
// nothing, and accepts "" and null (ptone/scion#4660). The map sections
// are checked in every entry, not only the first.
func TestPutServerConfigDB_NestedDurationSettingsValidated(t *testing.T) {
	cases := []struct {
		name    string
		section string
		path    []string // path of the key in the section row
		field   string
		body    func(v string) string
	}{
		{
			name: "telemetry batch timeout", section: "telemetry",
			path:  []string{"cloud", "batch", "timeout"},
			field: "telemetry.cloud.batch.timeout",
			body:  func(v string) string { return `{"telemetry": {"cloud": {"batch": {"timeout": ` + v + `}}}}` },
		},
		{
			name: "telemetry report_interval", section: "telemetry",
			path:  []string{"hub", "report_interval"},
			field: "telemetry.hub.report_interval",
			body:  func(v string) string { return `{"telemetry": {"hub": {"report_interval": ` + v + `}}}` },
		},
		{
			name: "runtime template_ready_timeout", section: "runtimes",
			path:  []string{"rt-b", "substrate", "template_ready_timeout"},
			field: "runtimes.rt-b.substrate.template_ready_timeout",
			body: func(v string) string {
				return `{"runtimes": {"rt-a": {"type": "substrate", "substrate": {"template_ready_timeout": "10m"}},` +
					` "rt-b": {"type": "substrate", "substrate": {"template_ready_timeout": ` + v + `}}}}`
			},
		},
		{
			name: "harness provisioner timeout", section: "harness_configs",
			path:  []string{"hc-b", "provisioner", "timeout"},
			field: "harness_configs.hc-b.provisioner.timeout",
			body: func(v string) string {
				return `{"harness_configs": {"hc-a": {"harness": "claude", "provisioner": {"timeout": "10m"}},` +
					` "hc-b": {"harness": "claude", "provisioner": {"timeout": ` + v + `}}}}`
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fakeStore, ops := newTestDBServer(t)

			if rr := putStartClaimConfigDB(t, srv, ops, tc.body(`"45m"`)); rr.Code != http.StatusOK {
				t.Fatalf("valid value: %d %s", rr.Code, rr.Body.String())
			}
			if got := storedNestedKey(t, fakeStore, tc.section, tc.path...); got != "45m" {
				t.Fatalf("stored %s = %q, want 45m", tc.field, got)
			}

			for _, bad := range []string{`"abc"`, `"30"`, `"1hour"`} {
				rr := putStartClaimConfigDB(t, srv, ops, tc.body(bad))
				if rr.Code != http.StatusUnprocessableEntity {
					t.Fatalf("%s: status %d, want 422: %s", bad, rr.Code, rr.Body.String())
				}
				var b durationErrorBody
				if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
					t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
				}
				if b.Error.Code != ErrCodeValidationError || b.Error.Details["field"] != tc.field {
					t.Errorf("%s: code %q field %v, want %q %q", bad, b.Error.Code, b.Error.Details["field"], ErrCodeValidationError, tc.field)
				}
				if !strings.Contains(b.Error.Message, tc.field) {
					t.Errorf("%s: message %q does not name %s", bad, b.Error.Message, tc.field)
				}
				if got := storedNestedKey(t, fakeStore, tc.section, tc.path...); got != "45m" {
					t.Errorf("%s: stored %s = %q after a refused save, want 45m", bad, tc.field, got)
				}
			}

			for _, clear := range []string{`""`, `null`} {
				if rr := putStartClaimConfigDB(t, srv, ops, tc.body(`"45m"`)); rr.Code != http.StatusOK {
					t.Fatalf("re-set before clear: %d %s", rr.Code, rr.Body.String())
				}
				if rr := putStartClaimConfigDB(t, srv, ops, tc.body(clear)); rr.Code != http.StatusOK {
					t.Fatalf("clear %s: %d %s", clear, rr.Code, rr.Body.String())
				}
				if got := storedNestedKey(t, fakeStore, tc.section, tc.path...); got != "" {
					t.Errorf("clear %s: stored %s = %q, want cleared", clear, tc.field, got)
				}
			}
		})
	}
}

// With invalid values in more than one map entry, the error names the
// entry that sorts first, whatever the order in the body.
func TestPutServerConfigDB_NestedDurationErrorNamesFirstMapEntry(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := putStartClaimConfigDB(t, srv, ops, `{"runtimes": {`+
		`"rt-z": {"type": "substrate", "substrate": {"template_ready_timeout": "abc"}},`+
		` "rt-m": {"type": "substrate", "substrate": {"template_ready_timeout": "10m"}},`+
		` "rt-a": {"type": "substrate", "substrate": {"template_ready_timeout": "1hour"}}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", rr.Code, rr.Body.String())
	}
	var b durationErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
	}
	if want := "runtimes.rt-a.substrate.template_ready_timeout"; b.Error.Details["field"] != want {
		t.Errorf("field %v, want %q", b.Error.Details["field"], want)
	}
}

// An invalid federation duration carried over from the stored row by the
// merge (the body sends another federation key) is refused with a 422
// whose details.field names the key, in the same shape as the body-path
// duration errors (ptone/scion#4451). The stored value overflows a
// duration: it passes the section schema pattern, so the merge carries it,
// but does not parse.
func TestPutServerConfigDB_FederationStoredDurationErrorNamesField(t *testing.T) {
	cases := []struct {
		name, stored, field, key string
	}{
		{"refresh_interval", `{"enabled":false,"refresh_interval":"3000000h"}`, "server.federation.refresh_interval", "refresh_interval"},
		{"debounce_interval", `{"enabled":false,"debounce_interval":"3000000h"}`, "server.federation.debounce_interval", "debounce_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fakeStore, ops := newTestDBServer(t)
			fakeStore.seedWithOrigin("federation", json.RawMessage(tc.stored), "managed")

			rr := putStartClaimConfigDB(t, srv, ops, `{"federation": {"algorithms": ["RS256"]}}`)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", rr.Code, rr.Body.String())
			}
			var b durationErrorBody
			if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
				t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
			}
			if b.Error.Code != ErrCodeValidationError || b.Error.Details["field"] != tc.field {
				t.Errorf("code %q field %v, want %q %q", b.Error.Code, b.Error.Details["field"], ErrCodeValidationError, tc.field)
			}
			if !strings.Contains(b.Error.Message, tc.key) {
				t.Errorf("message %q does not name %s", b.Error.Message, tc.key)
			}
		})
	}
}
