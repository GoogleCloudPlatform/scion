// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package hub

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// TestReadinessMarks_Defaults: the profiling readiness_marks setting is off
// unless the stored document sets it to true.
func TestReadinessMarks_Defaults(t *testing.T) {
	if (&Server{}).ReadinessMarksEnabled() {
		t.Error("no operational settings: want off")
	}
	for _, tc := range []struct {
		name string
		doc  string // "" = no row
		want bool
	}{
		{name: "absent", want: false},
		{name: "empty document", doc: `{}`, want: false},
		{name: "explicit false", doc: `{"readiness_marks":false}`, want: false},
		{name: "malformed", doc: `{"readiness_marks":`, want: false},
		{name: "wrong type", doc: `{"readiness_marks":"yes"}`, want: false},
		{name: "on", doc: `{"readiness_marks":true}`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeHubSettingStore()
			if tc.doc != "" {
				st.seed("profiling", json.RawMessage(tc.doc))
			}
			ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
			if _, err := ops.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if got := ops.ReadinessMarks(); got != tc.want {
				t.Errorf("ReadinessMarks() = %v, want %v", got, tc.want)
			}
			srv := &Server{}
			srv.operationalSettings.Store(ops)
			if got := srv.ReadinessMarksEnabled(); got != tc.want {
				t.Errorf("ReadinessMarksEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProfilingSection_Registered: the profiling section is DB-only, takes
// only a boolean readiness_marks, and is refused to tokens on the
// server-config path.
func TestProfilingSection_Registered(t *testing.T) {
	sec := opsettings.SectionByName("profiling")
	if sec == nil {
		t.Fatal("profiling section not registered")
	}
	if len(sec.KoanfPaths) != 0 {
		t.Errorf("profiling KoanfPaths = %v, want none (DB-only)", sec.KoanfPaths)
	}
	if errs := opsettings.Validate("profiling", json.RawMessage(`{"readiness_marks":true}`)); len(errs) != 0 {
		t.Errorf("valid document refused: %v", errs)
	}
	for _, doc := range []string{`{"readiness_marks":"true"}`, `{"other":true}`} {
		if errs := opsettings.Validate("profiling", json.RawMessage(doc)); len(errs) == 0 {
			t.Errorf("document %s accepted, want refused", doc)
		}
	}
	if got := serverConfigTokenSections["profiling"]; got != settingsTokenRefused {
		t.Errorf("serverConfigTokenSections[profiling] = %v, want settingsTokenRefused", got)
	}
}
