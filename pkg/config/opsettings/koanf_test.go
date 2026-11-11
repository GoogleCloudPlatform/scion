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
	"strings"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// leafKoanfPaths returns the section's registry KoanfPaths that hold a
// value: it drops a path that is only a parent of other paths in the same
// section (e.g. github_app's "server.github_app"), and a path equal to the
// section name, which marks a map-of-objects section (runtimes, profiles,
// harness_configs) whose whole subtree is the document.
func leafKoanfPaths(sec Section) []string {
	var out []string
	for _, kp := range sec.KoanfPaths {
		if kp == sec.Name {
			continue
		}
		parent := false
		for _, other := range sec.KoanfPaths {
			if strings.HasPrefix(other, kp+".") {
				parent = true
				break
			}
		}
		if !parent {
			out = append(out, kp)
		}
	}
	return out
}

// TestMappedSections_RegistryPathsInBothMaps guards the explicit koanf<->doc
// maps: for every registry section that uses koanfPathToJSONField /
// jsonFieldToKoanfPaths, every registry KoanfPaths entry must appear in
// both maps (seeding: koanf → doc; Refresh/Snapshot: doc → koanf), and the
// two maps must be inverses. A path missing from either map is silently
// dropped on seed or ignored when read back, which is how default_user_role
// (design §5.A item 7) and endpoints' server.hub.hub_name
// (ptone/scion#2073) were lost.
//
// The check is derived from the registry and the maps, so a new section or
// path is covered as soon as it is registered.
func TestMappedSections_RegistryPathsInBothMaps(t *testing.T) {
	for name := range koanfPathToJSONField {
		if _, ok := jsonFieldToKoanfPaths[name]; !ok {
			t.Errorf("section %q is in koanfPathToJSONField but not jsonFieldToKoanfPaths", name)
		}
	}
	for name := range jsonFieldToKoanfPaths {
		if _, ok := koanfPathToJSONField[name]; !ok {
			t.Errorf("section %q is in jsonFieldToKoanfPaths but not koanfPathToJSONField", name)
		}
	}

	for _, sec := range Registry {
		fwd, inFwd := koanfPathToJSONField[sec.Name]
		rev, inRev := jsonFieldToKoanfPaths[sec.Name]
		if !inFwd && !inRev {
			continue
		}
		registered := map[string]bool{}
		for _, kp := range leafKoanfPaths(sec) {
			registered[kp] = true
			field, ok := fwd[kp]
			if !ok {
				t.Errorf("%s: registry path %q missing from koanfPathToJSONField", sec.Name, kp)
				continue
			}
			if got, ok := rev[field]; !ok || got != kp {
				t.Errorf("%s: jsonFieldToKoanfPaths[%q] = %q, want %q", sec.Name, field, got, kp)
			}
		}
		for kp := range fwd {
			if !registered[kp] {
				t.Errorf("%s: koanfPathToJSONField has %q, which is not a registry KoanfPaths entry", sec.Name, kp)
			}
		}
		for field, kp := range rev {
			if !registered[kp] {
				t.Errorf("%s: jsonFieldToKoanfPaths[%q] = %q, which is not a registry KoanfPaths entry", sec.Name, field, kp)
			}
		}
	}
}

// TestRegistrySections_EveryPathRoundTrips seeds each registry KoanfPaths
// leaf into a koanf instance, extracts its section document
// (ExtractSectionFromKoanf, the seeding path), loads the document back
// (LoadSectionsIntoKoanf, the Refresh/Snapshot path), and checks the value
// survives. Unlike the map guard above, this also covers sections with
// hand-written extract/load code (agent_defaults, notifications, ...).
func TestRegistrySections_EveryPathRoundTrips(t *testing.T) {
	for _, sec := range Registry {
		for _, kp := range leafKoanfPaths(sec) {
			const sentinel = "round-trip-sentinel"
			k := koanf.New(".")
			if err := k.Load(confmap.Provider(map[string]interface{}{kp: sentinel}, "."), nil); err != nil {
				t.Fatal(err)
			}
			doc, err := ExtractSectionFromKoanf(k, sec.Name)
			if err != nil {
				t.Errorf("%s: ExtractSectionFromKoanf: %v", sec.Name, err)
				continue
			}
			back, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{sec.Name: doc})
			if err != nil {
				t.Errorf("%s: LoadSectionsIntoKoanf: %v", sec.Name, err)
				continue
			}
			if got := back.Get(kp); got != sentinel {
				t.Errorf("%s: %q = %v after extract+load, want %q (doc: %s)", sec.Name, kp, got, sentinel, doc)
			}
		}
	}
}

// TestEndpointsSection_HubNameRoundTrip pins server.hub.hub_name through
// seeding and Refresh/Snapshot (ptone/scion#2073).
func TestEndpointsSection_HubNameRoundTrip(t *testing.T) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.hub_name": "prod-hub",
	}, "."), nil); err != nil {
		t.Fatal(err)
	}
	doc, err := ExtractSectionFromKoanf(k, "endpoints")
	if err != nil {
		t.Fatalf("ExtractSectionFromKoanf: %v", err)
	}
	var endpoints EndpointsSettings
	if err := json.Unmarshal(doc, &endpoints); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if endpoints.HubName != "prod-hub" {
		t.Errorf("seeded endpoints doc hub_name = %q, want prod-hub (doc: %s)", endpoints.HubName, doc)
	}
	if errs := Validate("endpoints", doc); len(errs) != 0 {
		t.Errorf("seeded endpoints doc should validate: %v", errs)
	}

	back, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{"endpoints": doc})
	if err != nil {
		t.Fatalf("LoadSectionsIntoKoanf: %v", err)
	}
	if got := back.String("server.hub.hub_name"); got != "prod-hub" {
		t.Errorf("koanf server.hub.hub_name = %q after load, want prod-hub", got)
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
