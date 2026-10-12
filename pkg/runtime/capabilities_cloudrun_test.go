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

package runtime

import "testing"

// Neither Cloud Run runtime calls RunConfig.Checkpoint or
// RunConfig.OnResourceCreated, so both must opt out of async launch until
// they do; otherwise an aborted launch would clean up nothing.
func TestHasAsyncLaunchSupport_CloudRunRuntimesOptOut(t *testing.T) {
	for _, rt := range []Runtime{&CloudRunRuntime{}, &CloudRunSandboxRuntime{}} {
		if HasAsyncLaunchSupport(rt) {
			t.Errorf("HasAsyncLaunchSupport(%T) = true, want false: Run calls neither Checkpoint nor OnResourceCreated", rt)
		}
	}
}
