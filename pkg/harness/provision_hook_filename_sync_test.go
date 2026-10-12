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

package harness

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// TestHarnessProvisionHookFilenameMatchesWriter proves pkg/sciontool/hooks'
// own HarnessProvisionHookFilename constant — duplicated rather than
// imported; see its doc comment for why — never drifts from the name
// ContainerScriptHarness actually stages the wrapper under. A silent
// mismatch here would reopen the exact hole the hooks carve-out exists to
// close: DecideExecAsRoot would still classify the real, staged wrapper
// asRoot, but buildEnforcedCmd's name check would no longer match it, so it
// would fall straight through to running fully as root again.
//
// It lives here, not in pkg/sciontool/hooks, because this package's tests
// already import hooks; checking it from the hooks tests would make that
// test build link pkg/harness and everything under it.
func TestHarnessProvisionHookFilenameMatchesWriter(t *testing.T) {
	if hooks.HarnessProvisionHookFilename != HarnessProvisionHookFilename {
		t.Fatalf("hooks.HarnessProvisionHookFilename = %q, HarnessProvisionHookFilename = %q; these must stay equal",
			hooks.HarnessProvisionHookFilename, HarnessProvisionHookFilename)
	}
}
