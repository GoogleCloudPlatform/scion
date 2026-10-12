//go:build !hubshard || hubshard_3

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
