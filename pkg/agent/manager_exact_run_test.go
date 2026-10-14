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

package agent

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestSelectExactRunTarget covers the run-scoped selection a restarted
// broker deletes by (ptone/scion#4668, design wave 3 §1 C-d): only an entry
// labelled with the run exactly is selected, never an unlabelled legacy
// entry (which filterAgentsByRun would admit) or another run's entry.
func TestSelectExactRunTarget(t *testing.T) {
	legacy := api.AgentInfo{Name: "a", ContainerID: "c-legacy"}
	other := api.AgentInfo{Name: "a", ContainerID: "c-other", RunID: "run-other"}
	exact := api.AgentInfo{Name: "a", ContainerID: "c-exact", RunID: "run-1"}

	if _, found, err := SelectExactRunTarget([]api.AgentInfo{legacy, other}, "a", "", "run-1"); err != nil || found {
		t.Fatalf("legacy/other-run entries must not be selected: found=%v err=%v", found, err)
	}
	got, found, err := SelectExactRunTarget([]api.AgentInfo{legacy, other, exact}, "a", "", "run-1")
	if err != nil || !found || got.ContainerID != "c-exact" {
		t.Fatalf("expected the exact-run entry, got %+v found=%v err=%v", got, found, err)
	}
	if _, found, _ := SelectExactRunTarget([]api.AgentInfo{legacy, exact}, "a", "", ""); found {
		t.Fatal("an empty run ID must select nothing")
	}
}
