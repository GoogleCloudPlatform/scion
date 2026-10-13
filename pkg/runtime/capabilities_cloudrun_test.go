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

// The Cloud Run runtime cannot serve interactive attach, so it must opt out;
// the broker gates, /info, heartbeat and registration all read this answer.
func TestHasAttachSupport_CloudRunRuntimeOptsOut(t *testing.T) {
	if HasAttachSupport(&CloudRunRuntime{}) {
		t.Error("HasAttachSupport(&CloudRunRuntime{}) = true, want false: the Cloud Run runtime does not support attach")
	}
}

// The cloudrun-sandbox runtime is a separate runtime that does support
// attach; opting the Cloud Run runtime out must not change it.
func TestHasAttachSupport_CloudRunSandboxRuntimeSupported(t *testing.T) {
	if !HasAttachSupport(&CloudRunSandboxRuntime{}) {
		t.Error("HasAttachSupport(&CloudRunSandboxRuntime{}) = false, want true: the cloudrun-sandbox runtime supports attach")
	}
}
