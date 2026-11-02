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

package opsettings

import (
	"encoding/json"
	"testing"
)

func TestEffectiveOrigin(t *testing.T) {
	str := func(v string) *string { return &v }
	cases := []struct {
		section, origin string
		updatedBy       *string
		want            string
	}{
		{"profiles", OriginSeeded, str("seed"), OriginSeeded},
		{"profiles", OriginSeeded, str("admin@example.com"), OriginManaged},
		{"profiles", OriginSeeded, str(""), OriginManaged},
		{"profiles", OriginSeeded, nil, OriginSeeded},
		{"profiles", OriginManaged, str("seed"), OriginManaged},
		{"_meta", OriginSeeded, str("admin@example.com"), OriginSeeded},
		{SettingsRevisionSection, OriginSeeded, str("admin@example.com"), OriginSeeded},
		{SettingsRevisionSection, OriginSeeded, nil, OriginSeeded},
	}
	for _, c := range cases {
		if got := EffectiveOrigin(c.section, c.origin, c.updatedBy); got != c.want {
			t.Errorf("EffectiveOrigin(%q, %q, %v) = %q, want %q", c.section, c.origin, c.updatedBy, got, c.want)
		}
	}
}

func TestSyncedSection(t *testing.T) {
	str := func(v string) *string { return &v }
	boot := json.RawMessage(`{"a":{"runtime":"docker"}}`)
	empty := json.RawMessage(`{}`)
	stored := json.RawMessage(`{"b":{"runtime":"docker"}}`)
	cases := []struct {
		name        string
		section     string
		row         *SectionRow
		boot        json.RawMessage
		wantDoc     string
		wantPresent bool
	}{
		{"absent, bootstrap set: seeded", "profiles", nil, boot, string(boot), true},
		{"absent, bootstrap empty catalog: stays absent", "profiles", nil, empty, "", false},
		{"absent, bootstrap empty non-catalog: seeded", "telemetry", nil, empty, string(empty), true},
		{"seeded row: bootstrap wins", "profiles", &SectionRow{Origin: OriginSeeded, UpdatedBy: str("seed"), Value: stored}, boot, string(boot), true},
		{"seeded row, bootstrap now empty: empty doc", "profiles", &SectionRow{Origin: OriginSeeded, UpdatedBy: str("seed"), Value: stored}, empty, string(empty), true},
		{"managed row kept", "profiles", &SectionRow{Origin: OriginManaged, Value: stored}, boot, string(stored), true},
		{"pre-origin admin row kept (effective managed)", "profiles", &SectionRow{Origin: OriginSeeded, UpdatedBy: str("admin@example.com"), Value: stored}, boot, string(stored), true},
		{"seeded row with NULL updated_by: bootstrap wins", "profiles", &SectionRow{Origin: OriginSeeded, Value: stored}, boot, string(boot), true},
	}
	for _, c := range cases {
		doc, present := SyncedSection(c.section, c.row, c.boot)
		if string(doc) != c.wantDoc || present != c.wantPresent {
			t.Errorf("%s: got (%s, %v), want (%s, %v)", c.name, doc, present, c.wantDoc, c.wantPresent)
		}
	}
}
