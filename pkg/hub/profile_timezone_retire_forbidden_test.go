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
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestRetireProfileTimezones_DBTier_StripsRowWithForbiddenKeys checks that a
// stored profiles row carrying, next to a timezone, keys the profiles
// section schema no longer allows (a retired env, an unknown key) is still
// stripped: the forbidden keys are dropped with a warning naming their
// paths, not their values, and the other keys are kept.
func TestRetireProfileTimezones_DBTier_StripsRowWithForbiddenKeys(t *testing.T) {
	const secret = "s3cr3t-value-must-not-leak"
	st := newFakeHubSettingStore()
	st.seedWithOrigin("profiles", json.RawMessage(`{
		"a": {"runtime": "docker", "timezone": "Asia/Kathmandu", "env": {"FOO": "`+secret+`"}},
		"b": {"runtime": "docker", "image_registry": "registry.example.test/team", "no_such_key": "`+secret+`"}
	}`), "managed")
	st.seedWithOrigin("agent_defaults", json.RawMessage(`{"default_timezone":""}`), "seeded")
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil { // as at boot
		t.Fatal(err)
	}
	log, buf := retireTestLogger()

	if err := RetireProfileTimezones(context.Background(), ops, ProfileTimezoneRetireInput{DBTier: true}, log); err != nil {
		t.Fatalf("RetireProfileTimezones: %v\nlog:\n%s", err, buf)
	}

	row, profDoc := mustRow(t, st, "profiles")
	if row.Revision != 2 {
		t.Errorf("profiles revision = %d, want 2 (one strip write)", row.Revision)
	}
	if row.Origin != "managed" {
		t.Errorf("profiles origin = %q, want managed", row.Origin)
	}
	if _, ok := profileTZ(profDoc, "a"); ok {
		t.Errorf("profiles.a.timezone not stripped: %v", profDoc)
	}
	a, _ := profDoc["a"].(map[string]any)
	b, _ := profDoc["b"].(map[string]any)
	if _, ok := a["env"]; ok {
		t.Errorf("profiles.a.env kept: %v", a)
	}
	if _, ok := b["no_such_key"]; ok {
		t.Errorf("profiles.b.no_such_key kept: %v", b)
	}
	if a["runtime"] != "docker" || b["runtime"] != "docker" || b["image_registry"] != "registry.example.test/team" {
		t.Errorf("allowed keys not kept: %v", profDoc)
	}

	out := buf.String()
	for _, path := range []string{"profiles.a.env", "profiles.b.no_such_key"} {
		if !strings.Contains(out, path) {
			t.Errorf("log does not name %s:\n%s", path, out)
		}
	}
	if strings.Contains(out, secret) {
		t.Errorf("log contains a dropped value:\n%s", out)
	}
	_, adDoc := mustRow(t, st, "agent_defaults")
	if adDoc["default_timezone"] != "Asia/Kathmandu" {
		t.Errorf("agent_defaults.default_timezone = %v, want Asia/Kathmandu", adDoc["default_timezone"])
	}
}
