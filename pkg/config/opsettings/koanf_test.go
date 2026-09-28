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

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// TestAccessSection_RegistryPathsInBothMaps guards the explicit koanf<->doc
// maps for the access section: every registry KoanfPaths entry must appear in
// both koanfPathToJSONField (seeding: koanf → doc) and jsonFieldToKoanfPaths
// (Refresh/Snapshot: doc → koanf). A path missing from either map is silently
// dropped on seed or ignored when read back, which is how default_user_role
// was lost (design §5.A item 7).
//
// Scoped to access for this change; other sections (e.g. endpoints'
// server.hub.hub_name) have the same exposure and are tracked separately.
func TestAccessSection_RegistryPathsInBothMaps(t *testing.T) {
	sec := SectionByName("access")
	if sec == nil {
		t.Fatal("access section not in registry")
	}
	fwd := koanfPathToJSONField["access"]
	rev := jsonFieldToKoanfPaths["access"]
	for _, kp := range sec.KoanfPaths {
		field, ok := fwd[kp]
		if !ok {
			t.Errorf("access: registry path %q missing from koanfPathToJSONField", kp)
			continue
		}
		if got, ok := rev[field]; !ok || got != kp {
			t.Errorf("access: jsonFieldToKoanfPaths[%q] = %q, want %q", field, got, kp)
		}
	}
}

// TestAccessSection_DefaultUserRoleRoundTrip pins the access map entry for
// default_user_role in both directions.
func TestAccessSection_DefaultUserRoleRoundTrip(t *testing.T) {
	if got := koanfPathToJSONField["access"]["server.auth.default_user_role"]; got != "default_user_role" {
		t.Errorf("koanfPathToJSONField[access][server.auth.default_user_role] = %q, want default_user_role", got)
	}
	if got := jsonFieldToKoanfPaths["access"]["default_user_role"]; got != "server.auth.default_user_role" {
		t.Errorf("jsonFieldToKoanfPaths[access][default_user_role] = %q, want server.auth.default_user_role", got)
	}

	// koanf → doc (seeding).
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(map[string]interface{}{
		"server.auth.default_user_role": "viewer",
	}, "."), nil); err != nil {
		t.Fatal(err)
	}
	doc, err := ExtractSectionFromKoanf(k, "access")
	if err != nil {
		t.Fatalf("ExtractSectionFromKoanf: %v", err)
	}
	var access AccessSettings
	if err := json.Unmarshal(doc, &access); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if access.DefaultUserRole != "viewer" {
		t.Errorf("seeded access doc default_user_role = %q, want viewer (doc: %s)", access.DefaultUserRole, doc)
	}

	// doc → koanf (Refresh/Snapshot).
	back, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{"access": doc})
	if err != nil {
		t.Fatalf("LoadSectionsIntoKoanf: %v", err)
	}
	if got := back.String("server.auth.default_user_role"); got != "viewer" {
		t.Errorf("koanf server.auth.default_user_role = %q after load, want viewer", got)
	}
}
