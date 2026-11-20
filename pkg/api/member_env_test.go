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

package api

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestMemberSettableEnvScopes(t *testing.T) {
	autoExpose := []string{"SCION_AUTO_EXPOSE_INTERVAL", "SCION_AUTO_EXPOSE_MODE", "SCION_AUTO_EXPOSE_PORTS", "SCION_AUTO_EXPOSE_PORTS_LIST"}
	for scope, want := range map[MemberEnvScope][]string{
		MemberEnvEdit:          autoExpose,
		MemberEnvCreate:        append(slices.Clone(autoExpose), "SCION_LOG_LEVEL", "SCION_TELEMETRY_ENABLED"),
		MemberEnvHarnessConfig: append(slices.Clone(autoExpose), "SCION_LIVENESS_HARNESS_ROOT_DEPTH"),
	} {
		got := slices.Sorted(maps.Keys(MemberSettableEnvNames(scope)))
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("scope %b: %v, want %v", scope, got, want)
		}
		for _, n := range want {
			if !IsMemberSettableEnv(n, scope) {
				t.Errorf("IsMemberSettableEnv(%q, %b) = false", n, scope)
			}
		}
	}
	if IsMemberSettableEnv("SCION_LOG_LEVEL", MemberEnvEdit) || IsMemberSettableEnv("SCION_LIVENESS_HARNESS_ROOT_DEPTH", MemberEnvCreate) {
		t.Error("a name is settable outside its scope")
	}
	if IsMemberSettableEnv("SCION_HUB_ENDPOINT", MemberEnvCreate|MemberEnvEdit|MemberEnvHarnessConfig) {
		t.Error("an unlisted reserved name is settable")
	}
}

// Every entry is a reserved name (the list only exempts), listed once, with
// a reason and at least one scope.
func TestMemberSettableEnvEntries(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range MemberSettableEnv() {
		if !strings.HasPrefix(e.Name, "SCION_") && !strings.HasPrefix(e.Name, "GCE_METADATA_") {
			t.Errorf("%s is not a reserved name", e.Name)
		}
		if seen[e.Name] {
			t.Errorf("%s listed twice", e.Name)
		}
		seen[e.Name] = true
		if e.Reason == "" || e.Scope == 0 {
			t.Errorf("%s needs a reason and a scope", e.Name)
		}
	}
}
