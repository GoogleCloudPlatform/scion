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

// selectorTestMu serializes OverrideSelectorInputsForTest calls against each
// other and against the cache reset they perform, so overlapping install/
// restore pairs cannot interleave. It does not make this hook safe to call
// concurrently with ResolveSelector itself — see the doc comment below.
var selectorTestMu sync.Mutex

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
// Must only be called from tests, never concurrently with ResolveSelector:
// it reassigns package-level state ResolveSelector reads without a lock on
// the read side, by design, since production code treats that state as
// immutable for the life of the process. Production code never calls this.
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

	originalRegistry := Registry
	originalAliases := UATManageAliases
	Registry = registry
	UATManageAliases = aliases
	resetSelectorRegistryCache()

	return func() {
		selectorTestMu.Lock()
		defer selectorTestMu.Unlock()
		Registry = originalRegistry
		UATManageAliases = originalAliases
		resetSelectorRegistryCache()
	}
}

// resetSelectorRegistryCache clears the process-wide cached selector
// registry so the next ResolveSelector or ValidateSelectorRegistry call
// rebuilds it from the current Registry/UATManageAliases.
func resetSelectorRegistryCache() {
	selectorRegistryOnce = sync.Once{}
	selectorRegistry = nil
}
