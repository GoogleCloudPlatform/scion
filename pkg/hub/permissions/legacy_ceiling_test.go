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

package permissions

import (
	"sort"
	"testing"
)

// TestLegacyUATScopeToPermissionID_MatchesUATScopeField pins the invariant
// the frozen snapshot's freshness argument depends on: every current
// Permission.UATScope value equals Resource+":"+Action, so a raw stored
// scope string and its canonical permission ID are in 1:1 correspondence.
// If a future change breaks this invariant, this test — not production
// behavior — is what should fail; the frozen table itself stays untouched.
func TestLegacyUATScopeToPermissionID_MatchesUATScopeField(t *testing.T) {
	for _, p := range Registry {
		if p.UATScope == "" {
			continue
		}
		want := p.Resource + ":" + p.Action
		if p.UATScope != want {
			t.Errorf("permission %s: UATScope %q does not equal Resource:Action %q", p.ID, p.UATScope, want)
		}
	}
}

// TestLegacyUATScopeToPermissionID_CoversAllCurrentUATScopes documents that
// the frozen snapshot was accurate as of its authoring date: every
// UATScope-bearing Registry permission today has an entry. This is a
// point-in-time documentation check, not a live derivation — adding a new
// UATScope later must NOT require (or cause) a change to the frozen table;
// this test will simply start reporting the new scope as "not yet legacy",
// which is correct, since no pre-A.2 row could ever contain it.
func TestLegacyUATScopeToPermissionID_CoversAllCurrentUATScopes(t *testing.T) {
	var missing []string
	for _, p := range Registry {
		if p.UATScope == "" {
			continue
		}
		if _, ok := legacyUATScopeToPermissionID[p.UATScope]; !ok {
			missing = append(missing, p.UATScope)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Logf("UATScope(s) not present in the frozen legacy snapshot (expected only for scopes added after 2026-09-29): %v", missing)
	}
}

// TestNormalizeLegacyUATScopes_AttachOnlyStaysAttachOnly is the
// characterization pin for the A.2 ruling: a legacy row holding only
// agent:attach normalizes to exactly agent.attach — no lifecycle
// implication.
func TestNormalizeLegacyUATScopes_AttachOnlyStaysAttachOnly(t *testing.T) {
	ids := NormalizeLegacyUATScopes([]string{"agent:attach"})
	if len(ids) != 1 || ids[0] != "agent.attach" {
		t.Fatalf("expected exactly [agent.attach], got %v", ids)
	}
}

// TestNormalizeLegacyUATScopes_ManageAliasExpansion characterizes legacy
// manage-alias expansion separately, per the A.2 brief: a legacy row's
// stored scopes always already reflect the mint-time expandScopes output
// (manage aliases were never stored raw), so normalizing them recovers
// exactly the resource's manage-alias scopes, still excluding attach/
// port_access.
func TestNormalizeLegacyUATScopes_ManageAliasExpansion(t *testing.T) {
	storedAfterMintTimeExpansion := UATManageScopesFor(ResourceAgent) // what expandScopes("agent:manage") would have persisted
	ids := NormalizeLegacyUATScopes(storedAfterMintTimeExpansion)
	sort.Strings(ids)

	want := make([]string, 0, len(storedAfterMintTimeExpansion))
	for _, scope := range storedAfterMintTimeExpansion {
		want = append(want, legacyUATScopeToPermissionID[scope])
	}
	sort.Strings(want)

	if len(ids) != len(want) {
		t.Fatalf("expected %v, got %v", want, ids)
	}
	for i := range ids {
		if ids[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, ids)
		}
	}
	for _, excluded := range []string{"agent.attach", "agent.port_access"} {
		for _, id := range ids {
			if id == excluded {
				t.Errorf("legacy agent:manage expansion must not include %s", excluded)
			}
		}
	}
}

// TestNormalizeLegacyUATScopes_UnrecognizedScopeDropped pins that an
// unrecognized scope narrows (is dropped) rather than denying or erroring
// the whole ceiling.
func TestNormalizeLegacyUATScopes_UnrecognizedScopeDropped(t *testing.T) {
	ids := NormalizeLegacyUATScopes([]string{"agent:read", "not:a-real-scope"})
	if len(ids) != 1 || ids[0] != "agent.read" {
		t.Fatalf("expected exactly [agent.read], got %v", ids)
	}
}

// TestNormalizeLegacyUATScopes_ImmuneToRegistryChanges pins F-8 requirement
// 1 directly: normalizing a legacy row must never consult the live,
// mutable Registry, so a later Registry/alias change cannot silently
// reinterpret what an existing legacy token means.
func TestNormalizeLegacyUATScopes_ImmuneToRegistryChanges(t *testing.T) {
	before := NormalizeLegacyUATScopes([]string{"agent:attach", "agent:read"})

	originalRegistry := Registry
	originalAliases := UATManageAliases
	t.Cleanup(func() {
		Registry = originalRegistry
		UATManageAliases = originalAliases
	})

	// Simulate a later change that would (if this function consulted live
	// state) alter the interpretation of "agent:attach": retarget its
	// UATScope-bearing entry's ID and add a brand new implication-like
	// alias.
	mutated := append([]Permission(nil), originalRegistry...)
	for i := range mutated {
		if mutated[i].UATScope == "agent:attach" {
			mutated[i].ID = "agent.attach.renamed"
		}
	}
	Registry = mutated
	UATManageAliases = map[string]string{"agent:attach": "agent"}

	after := NormalizeLegacyUATScopes([]string{"agent:attach", "agent:read"})
	if len(before) != len(after) {
		t.Fatalf("registry mutation changed normalization result: before=%v after=%v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("registry mutation changed normalization result: before=%v after=%v", before, after)
		}
	}
	for _, id := range after {
		if id == "agent.attach.renamed" {
			t.Error("legacy normalization must not pick up a live Registry ID rename")
		}
	}
}
