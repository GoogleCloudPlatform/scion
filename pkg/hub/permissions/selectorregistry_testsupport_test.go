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

import "testing"

// TestOverrideSelectorInputsForTest_OverlapPanicsThenRestoreAllowsReuse pins
// that a second override while one is still active panics rather than
// silently leaking the first override's mutated registry, and that once the
// first override is restored, installing a new one succeeds again.
func TestOverrideSelectorInputsForTest_OverlapPanicsThenRestoreAllowsReuse(t *testing.T) {
	restore := OverrideSelectorInputsForTest(Registry, UATManageAliases)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected a second, overlapping OverrideSelectorInputsForTest call to panic")
			}
		}()
		OverrideSelectorInputsForTest(Registry, UATManageAliases)
	}()

	restore()

	// Restore is idempotent.
	restore()

	// A new override succeeds now that the first was restored.
	restore2 := OverrideSelectorInputsForTest(Registry, UATManageAliases)
	restore2()
}
