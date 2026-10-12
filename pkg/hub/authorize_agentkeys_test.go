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

package hub

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestAgentActionPermission_KeysMapsToAttach pins the agent-keys contract's
// decision 2 (.design/agent-keys-contract.md, ptone/scion#2191): the "keys"
// route action must map to the existing ActionAttach permission.
//
// This test pins that mapped value, not the presence of the explicit case in
// agentActionPermission's switch: today's case returns exactly what the
// default branch also returns, so deleting the explicit case (leaving
// api.AgentActionKeys to fall through to default) would not fail this test —
// confirmed by a mutation check before this comment was written. Detecting
// the explicit case's removal would require asserting against the function's
// source (e.g. an AST check), which this test does not attempt. What this
// test does guard is the more consequential regression: if either the
// explicit case's target or the default branch's return value ever changed
// so that api.AgentActionKeys stopped mapping to ActionAttach, this fails.
func TestAgentActionPermission_KeysMapsToAttach(t *testing.T) {
	if got := agentActionPermission(api.AgentActionKeys); got != ActionAttach {
		t.Fatalf("agentActionPermission(%q) = %v, want %v", api.AgentActionKeys, got, ActionAttach)
	}
}
