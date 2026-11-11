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

package substratecaps

import (
	"slices"
	"testing"
)

func TestNames_MatchesRequiredInOrder(t *testing.T) {
	want := make([]string, len(Required))
	for i, c := range Required {
		want[i] = c.Name
	}
	if got := Names(); !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

// TestRequired_EveryEntryHasNameBitAndReason guards against a capability
// being added with a zero-value field by mistake (e.g. an EffBit of 0 that
// was meant to be set, or a copy-pasted entry missing its own Why) — bit 0
// is legitimately CAP_CHOWN, so this checks Name/Why are non-empty and
// EffBit is within the 64-bit range /proc/self/status's CapEff uses,
// rather than rejecting bit 0 outright.
func TestRequired_EveryEntryHasNameBitAndReason(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Required {
		if c.Name == "" {
			t.Errorf("Capability with empty Name (EffBit=%d)", c.EffBit)
		}
		if c.Why == "" {
			t.Errorf("Capability %q has no Why", c.Name)
		}
		if c.EffBit > 63 {
			t.Errorf("Capability %q EffBit = %d, want 0-63", c.Name, c.EffBit)
		}
		if seen[c.Name] {
			t.Errorf("Capability %q listed more than once", c.Name)
		}
		seen[c.Name] = true
	}
	if len(Required) == 0 {
		t.Error("Required is empty")
	}
}

// TestRequired_IncludesDACOverride pins DAC_OVERRIDE (CAP_DAC_OVERRIDE, bit
// 1) in Required: removing it would leave every other test in this package
// passing (they all derive their expectations from Required itself), so
// this is the one guard that actually fails if it's dropped.
func TestRequired_IncludesDACOverride(t *testing.T) {
	for _, c := range Required {
		if c.Name == "DAC_OVERRIDE" {
			if c.EffBit != 1 {
				t.Errorf("DAC_OVERRIDE EffBit = %d, want 1 (CAP_DAC_OVERRIDE)", c.EffBit)
			}
			return
		}
	}
	t.Error("Required does not include DAC_OVERRIDE")
}
