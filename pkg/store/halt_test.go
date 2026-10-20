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

package store

import "testing"

// The agent-reported "signaled" reason is never halt evidence: it only lifts
// the clean-exit block of a halt-evidence reason the agent runtime reports
// (ptone/scion#5278).
func TestIsHaltEvidence_SignaledIsNotEvidence(t *testing.T) {
	if IsHaltEvidence("signaled") {
		t.Error(`IsHaltEvidence("signaled") = true, want false`)
	}
}
