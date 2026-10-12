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
	"slices"
	"testing"
)

// Every Section.Durations entry is a key of its section and a top-level
// property of the section schema, so the DB-tier save finds it in the
// section document.
func TestSectionDurationsAreSectionKeys(t *testing.T) {
	n := 0
	for _, sec := range Registry {
		for _, kp := range sec.Durations {
			if !slices.Contains(sec.KoanfPaths, kp) {
				t.Errorf("%s: duration key %q is not in KoanfPaths", sec.Name, kp)
			}
		}
		props, _ := rawSchemas[sec.Name]["properties"].(map[string]interface{})
		for _, k := range DurationKeys(sec.Name) {
			n++
			if _, ok := props[k.SectionKey]; !ok {
				t.Errorf("%s: duration key %q (section key %q) is not a schema property", sec.Name, k.KoanfPath, k.SectionKey)
			}
		}
	}
	for _, want := range []string{"server.hub.stalled_threshold", "default_max_duration"} {
		if sec := SectionByName(OwningSection(want)); sec == nil || !slices.Contains(sec.Durations, want) {
			t.Errorf("%s is not marked as a duration key", want)
		}
	}
	if n == 0 {
		t.Fatal("no duration keys registered")
	}
}
