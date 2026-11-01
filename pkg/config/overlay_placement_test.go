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

package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// On load, an unknown runtime or profile key that is not a placement key
// is a warning: the section still loads with its placement, and the key is
// named (never its value).
func TestPlacementLoadUnknownNonPlacementWarns(t *testing.T) {
	docs := map[string]json.RawMessage{
		"runtimes": json.RawMessage(`{"k":{"type":"kubernetes","node_selector":{"pool":"a"},"future_field":"secret-value"}}`),
		"profiles": json.RawMessage(`{"p":{"runtime":"k","another_new_key":1}}`),
	}
	got, errs := OverlayMapSections(docs, nil)
	if len(errs) != 0 || len(got.PlacementErrors) != 0 {
		t.Fatalf("decode errors %v, placement errors %v", errs, got.PlacementErrors)
	}
	if got.Runtimes["k"].NodeSelector["pool"] != "a" {
		t.Fatalf("placement not loaded: %+v", got.Runtimes)
	}
	if len(got.Warnings) != 2 || !strings.HasPrefix(got.Warnings[0], "profiles.p.another_new_key") || !strings.HasPrefix(got.Warnings[1], "runtimes.k.future_field") {
		t.Fatalf("warnings = %v", got.Warnings)
	}
	for _, w := range got.Warnings {
		if strings.Contains(w, "secret") {
			t.Fatalf("a warning carries a value: %q", w)
		}
	}
}

// On load, malformed or misspelt placement keys, invalid placement, sibling
// runtime entries that disagree, and the rules across runtime entries and
// profiles are placement errors naming the entry: the Runtime Broker then
// refuses Kubernetes starts rather than reading them as no placement.
func TestPlacementLoadUnknownPlacementRejects(t *testing.T) {
	cases := map[string]map[string]json.RawMessage{
		"runtimes.k":                    {"runtimes": json.RawMessage(`{"k":{"type":"kubernetes","nodeSelector":{"pool":"a"}}}`)},
		"profiles.p":                    {"profiles": json.RawMessage(`{"p":{"runtime":"kubernetes","tolerations":[{"key":"a","tolerationSeconds":5}]}}`)},
		"runtimes.d":                    {"runtimes": json.RawMessage(`{"d":{"type":"docker","runtime_class_name":"gvisor"}}`)},
		"runtimes.c.runtime_class_name": {"runtimes": json.RawMessage(`{"c":{"type":"kubernetes","runtime_class_name":"Not_Valid"}}`)},
		// Sibling runtime entries (same context and namespace) that
		// disagree: the pair is named once, at its first entry.
		"runtimes.a": {"runtimes": json.RawMessage(`{"a":{"type":"kubernetes","context":"c1","namespace":"agents","node_selector":{"pool":"x"}},` +
			`"b":{"type":"kubernetes","context":"c1","namespace":"agents","node_selector":{"pool":"y"}}}`)},
		// A profile's pool selector that differs from its runtime entry's.
		"profiles.p.node_selector": {
			"runtimes": json.RawMessage(`{"k":{"type":"kubernetes","node_selector":{"scion.dev/pool":"a"}}}`),
			"profiles": json.RawMessage(`{"p":{"runtime":"k","node_selector":{"scion.dev/pool":"b"}}}`),
		},
		// A profile with placement whose runtime entry does not exist.
		"profiles.p.runtime": {"profiles": json.RawMessage(`{"p":{"runtime":"nosuch","runtime_class_name":"gvisor"}}`)},
	}
	for want, docs := range cases {
		got, _ := OverlayMapSections(docs, nil)
		if !reflect.DeepEqual(got.PlacementErrors, []string{want}) {
			t.Errorf("%s: placement errors %v", want, got.PlacementErrors)
		}
	}
	clean, _ := OverlayMapSections(map[string]json.RawMessage{
		"runtimes": json.RawMessage(`{"k":{"type":"kubernetes","tolerations":[{"key":"a","operator":"Exists"}]}}`),
	}, nil)
	if clean.PlacementErrors != nil || clean.Warnings != nil {
		t.Fatalf("clean sections: %+v", clean)
	}
}

// The overlay records the findings and reports when they change.
func TestSettingsOverlayPlacementFindings(t *testing.T) {
	var none *SettingsOverlay
	if none.PlacementErrors() != nil {
		t.Fatal("a nil overlay has placement errors")
	}
	o := NewSettingsOverlay()
	if !o.UpdateWithPlacementFindings(nil, nil, nil, []string{"runtimes.k"}, nil) || o.UpdateWithPlacementFindings(nil, nil, nil, []string{"runtimes.k"}, nil) {
		t.Fatal("change reporting is wrong")
	}
	errs := o.PlacementErrors()
	errs[0] = "changed"
	if o.PlacementErrors()[0] != "runtimes.k" {
		t.Fatal("PlacementErrors shares the overlay's slice")
	}
	if !o.UpdateWithPlacementFindings(nil, nil, nil, nil, nil) || o.PlacementErrors() != nil {
		t.Fatal("findings not cleared")
	}
}

// Apply carries the overlay's placement findings into the settings it
// fills, together with the maps from the same update: settings loaded
// before a later update keep both the earlier maps and the earlier
// findings, and settings loaded after it get both new ones.
func TestSettingsOverlayApplyCarriesPlacementFindings(t *testing.T) {
	if (*VersionedSettings)(nil).PlacementErrors() != nil {
		t.Fatal("nil settings have placement errors")
	}
	o := NewSettingsOverlay()
	invalid := map[string]V1RuntimeConfig{"k": {Type: "kubernetes", RuntimeClassName: "Not_Valid"}}
	o.UpdateWithPlacementFindings(invalid, nil, nil, []string{"runtimes.k.runtime_class_name"}, nil)
	before := &VersionedSettings{}
	o.Apply(before)

	valid := map[string]V1RuntimeConfig{"k": {Type: "kubernetes", RuntimeClassName: "gvisor"}}
	o.UpdateWithPlacementFindings(valid, nil, nil, nil, nil)
	after := &VersionedSettings{}
	o.Apply(after)

	if got := before.PlacementErrors(); !reflect.DeepEqual(got, []string{"runtimes.k.runtime_class_name"}) || before.Runtimes["k"].RuntimeClassName != "Not_Valid" {
		t.Fatalf("settings applied before the fix: findings %v, runtime class %q", got, before.Runtimes["k"].RuntimeClassName)
	}
	if got := after.PlacementErrors(); got != nil || after.Runtimes["k"].RuntimeClassName != "gvisor" {
		t.Fatalf("settings applied after the fix: findings %v, runtime class %q", got, after.Runtimes["k"].RuntimeClassName)
	}
	errs := before.PlacementErrors()
	errs[0] = "changed"
	if before.PlacementErrors()[0] != "runtimes.k.runtime_class_name" {
		t.Fatal("PlacementErrors shares the settings' slice")
	}
}
