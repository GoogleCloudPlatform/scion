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

package hubclient

import (
	"encoding/json"
	"testing"
)

// TestAgentLaunch_ResumePhaseJSON pins the wire name of the resume memory
// (ptone/scion#4669): the Hub sends launch.resumePhase, and a response
// without it decodes to "".
func TestAgentLaunch_ResumePhaseJSON(t *testing.T) {
	var a Agent
	if err := json.Unmarshal([]byte(`{"id":"a1","phase":"error","launch":{"id":"l1","state":"ended","active":false,"kind":"start","error":"launch_timeout","resumePhase":"suspended"}}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.Launch == nil || a.Launch.ResumePhase != "suspended" {
		t.Fatalf("launch.resumePhase not decoded: %+v", a.Launch)
	}

	var b Agent
	if err := json.Unmarshal([]byte(`{"id":"a1","phase":"error","launch":{"id":"l1","state":"ended","active":false,"kind":"start"}}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.Launch == nil || b.Launch.ResumePhase != "" {
		t.Fatalf("absent resumePhase must decode to empty: %+v", b.Launch)
	}

	raw, err := json.Marshal(AgentLaunch{ID: "l1", State: "ended", Kind: "start"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["resumePhase"]; ok {
		t.Fatalf("empty resumePhase must be omitted: %s", raw)
	}
}
