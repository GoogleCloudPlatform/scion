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
	"strings"
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

// Every Section.NestedDurations entry is a koanf path of its section below
// the section name, at least two levels deep in the section document, and
// the nested families of ptone/scion#4660 are registered.
func TestSectionNestedDurationsAreSectionKeys(t *testing.T) {
	for _, sec := range Registry {
		for _, kp := range sec.NestedDurations {
			rest, ok := strings.CutPrefix(kp, sec.Name+".")
			if !ok || !strings.Contains(rest, ".") {
				t.Errorf("%s: nested duration key %q is not a nested path below the section name", sec.Name, kp)
				continue
			}
			if got := OwningSection(strings.ReplaceAll(kp, "*", "x")); got != sec.Name {
				t.Errorf("%s: nested duration key %q is owned by section %q", sec.Name, kp, got)
			}
		}
	}
	for _, want := range []struct{ section, key string }{
		{"telemetry", "telemetry.cloud.batch.timeout"},
		{"telemetry", "telemetry.hub.report_interval"},
		{"runtimes", "runtimes.*.substrate.template_ready_timeout"},
		{"harness_configs", "harness_configs.*.provisioner.timeout"},
		{"harness_configs", "harness_configs.*.ready.settle"},
		{"harness_configs", "harness_configs.*.ready.timeout"},
		{"harness_configs", "harness_configs.*.delivery.extra_submit_delay"},
	} {
		if s := SectionByName(want.section); s == nil || !slices.Contains(s.NestedDurations, want.key) {
			t.Errorf("%s is not marked as a nested duration key", want.key)
		}
	}
}
