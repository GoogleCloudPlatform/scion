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

// ResetSelectorRegistryForTest clears the process-wide cached selector
// registry built from Registry and UATManageAliases, so a test that mutates
// either one is reflected on the next ResolveSelector or
// ValidateSelectorRegistry call instead of being masked by the cache.
//
// Test-only. Production code never mutates Registry or UATManageAliases
// after process startup, so it never needs to invalidate this cache; the
// live registry callers depend on is expected to be immutable for the life
// of the process. A test that calls this must restore Registry/
// UATManageAliases and call it again afterward (t.Cleanup), so later tests
// observe the real registry.
//
// Exported (rather than confined to a _test.go file in this package)
// because callers outside this package — e.g. pkg/hub tests proving that a
// Registry change cannot widen an already-issued credential — need to force
// the same cache invalidation the mutation would otherwise require.
func ResetSelectorRegistryForTest() {
	selectorRegistryOnce = sync.Once{}
	selectorRegistry = nil
}
