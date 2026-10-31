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

package secret

import (
	"slices"
	"testing"
)

// TestDeduplicateByTarget_ScopePrecedenceAcrossNames pins that the winner
// for a shared target is decided by scope precedence (runtime_broker < hub
// < project < user), not by name or by input order: secrets with distinct
// names and one target, across every scope, in ascending and descending
// order. Names put each expected winner strictly in the middle by name, in
// the full set and in the subset, so neither the first nor the last name
// in sorted order would win.
func TestDeduplicateByTarget_ScopePrecedenceAcrossNames(t *testing.T) {
	ascending := []SecretWithValue{
		{SecretMeta: SecretMeta{Name: "d-broker", SecretType: TypeEnvironment, Target: "SHARED_TARGET", Scope: ScopeRuntimeBroker}, Value: "broker"},
		{SecretMeta: SecretMeta{Name: "a-hub", SecretType: TypeEnvironment, Target: "SHARED_TARGET", Scope: ScopeHub}, Value: "hub"},
		{SecretMeta: SecretMeta{Name: "c-project", SecretType: TypeEnvironment, Target: "SHARED_TARGET", Scope: ScopeProject}, Value: "project"},
		{SecretMeta: SecretMeta{Name: "b-user", SecretType: TypeEnvironment, Target: "SHARED_TARGET", Scope: ScopeUser}, Value: "user"},
	}
	descending := slices.Clone(ascending)
	slices.Reverse(descending)

	for _, tc := range []struct {
		name  string
		input []SecretWithValue
	}{
		{"lowest scope first", ascending},
		{"highest scope first", descending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DeduplicateByTarget(tc.input)
			if len(got) != 1 {
				t.Fatalf("expected one secret for the shared target, got %d: %+v", len(got), got)
			}
			if got[0].Scope != ScopeUser || got[0].Name != "b-user" || got[0].Value != "user" {
				t.Fatalf("expected the user-scope secret to win, got %s (%s)", got[0].Name, got[0].Scope)
			}
		})
	}

	// Without the user secret, project outranks hub and broker in either
	// order.
	withoutUser := ascending[:3]
	for _, input := range [][]SecretWithValue{slices.Clone(withoutUser), reversed(withoutUser)} {
		got := DeduplicateByTarget(input)
		if len(got) != 1 || got[0].Scope != ScopeProject {
			t.Fatalf("expected the project-scope secret to win, got %+v", got)
		}
	}
}

func reversed(in []SecretWithValue) []SecretWithValue {
	out := slices.Clone(in)
	slices.Reverse(out)
	return out
}
