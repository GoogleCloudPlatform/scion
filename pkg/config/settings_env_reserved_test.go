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
	"strings"
	"testing"
)

func TestExpandEnvMap_ReservedHostEnvExpandsEmpty(t *testing.T) {
	const sentinel = "reserved-host-value"
	t.Setenv("SCION_SERVER_SESSION_SECRET", sentinel)
	t.Setenv("SESSION_SECRET", sentinel)
	t.Setenv("ORDINARY_VAR", "ordinary")

	got := expandEnvMap(map[string]string{
		"A":                 "${SCION_SERVER_SESSION_SECRET}",
		"B":                 "$SESSION_SECRET",
		"C":                 "${ORDINARY_VAR}",
		"${SESSION_SECRET}": "key-expansion",
		"SESSION_SECRET":    "",
		"D":                 "x-${SESSION_SECRET}-${ORDINARY_VAR}",
	})
	for k, v := range got {
		if strings.Contains(k, sentinel) || strings.Contains(v, sentinel) {
			t.Errorf("expanded map carries a reserved host value under %q", k)
		}
	}
	if got["A"] != "" || got["B"] != "" {
		t.Errorf("A = %q, B = %q; want both empty", got["A"], got["B"])
	}
	if got["C"] != "ordinary" {
		t.Errorf("C = %q, want %q", got["C"], "ordinary")
	}
	if got["D"] != "x--ordinary" {
		t.Errorf("D = %q, want %q", got["D"], "x--ordinary")
	}
	if v, ok := got["SESSION_SECRET"]; !ok || v != "" {
		t.Errorf("SESSION_SECRET = %q, %v; want an empty marker kept as-is", v, ok)
	}
	if len(got) != 5 {
		t.Errorf("got %d keys, want 5 (the key-expansion entry dropped): %v", len(got), got)
	}
}
