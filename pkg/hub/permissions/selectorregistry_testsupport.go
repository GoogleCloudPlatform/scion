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

import "sync"

// selectorTestMu guards selectorOverrideActive and serializes
// OverrideSelectorInputsForTest/restore calls against each other and
// against the cache reset they perform. It does not make this hook safe to
// call concurrently with ResolveSelector itself — see the doc comment
// below.
var selectorTestMu sync.Mutex

// selectorOverrideActive is true while an install/restore pair from
// OverrideSelectorInputsForTest has not yet been restored. Guarded by
// selectorTestMu.
var selectorOverrideActive bool

// OverrideSelectorInputsForTest replaces Registry and UATManageAliases for
// the duration of a test and resets the cached selector registry so the
// replacement takes effect on the next ResolveSelector or
// ValidateSelectorRegistry call. It returns a restore function that
// reinstates the originals and resets the cache again; callers install it
// via t.Cleanup.
//
//	restore := permissions.OverrideSelectorInputsForTest(mutatedRegistry, mutatedAliases)
//	t.Cleanup(restore)
//
// Serializes install and restore, and rejects a second override while one
// is active by panicking, so overlapping overrides fail loudly instead of
// leaking a mutated registry into later tests (an install/restore pair that
// overlaps another and restores out of order would otherwise leave the
// process on the wrong registry for the rest of the binary). It does not
// make this hook safe to call concurrently with ResolveSelector: production
// code treats Registry/UATManageAliases as immutable for the life of the
// process and never calls this. restore is idempotent; calling it more than
// once after the first call is a no-op.
//
// Exported (rather than confined to a _test.go file in this package)
// because callers outside this package — e.g. pkg/hub tests proving that a
// Registry or alias change cannot widen an already-issued credential — need
// to force the same override and cache invalidation a live mutation would
// otherwise require, and an export_test.go accessor is visible only to this
// package's own tests.
func OverrideSelectorInputsForTest(registry []Permission, aliases map[string]string) (restore func()) {
	selectorTestMu.Lock()
	defer selectorTestMu.Unlock()

	if selectorOverrideActive {
		panic("OverrideSelectorInputsForTest: an override is already active; overrides must not overlap (do not use from parallel tests)")
	}
	selectorOverrideActive = true

	originalRegistry := Registry
	originalAliases := UATManageAliases
	Registry = registry
	UATManageAliases = aliases
	resetSelectorRegistryCache()

	var restored bool
	return func() {
		selectorTestMu.Lock()
		defer selectorTestMu.Unlock()
		if restored {
			return
		}
		restored = true
		Registry = originalRegistry
		UATManageAliases = originalAliases
		resetSelectorRegistryCache()
		selectorOverrideActive = false
	}
}

// resetSelectorRegistryCache clears the process-wide cached selector
// registry so the next ResolveSelector or ValidateSelectorRegistry call
// rebuilds it from the current Registry/UATManageAliases.
func resetSelectorRegistryCache() {
	selectorRegistryOnce = sync.Once{}
	selectorRegistry = nil
}
