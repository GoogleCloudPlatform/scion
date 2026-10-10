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

// TestFlatPlacementView_ClientTypesDecode: the client types carry the Hub's
// read-only agent pinnedRuntimeTarget view and a flat Runtime Broker's
// runtimeTarget descriptor, and both are absent for legacy objects.
func TestFlatPlacementView_ClientTypesDecode(t *testing.T) {
	var a Agent
	if err := json.Unmarshal([]byte(`{"id":"a","runtimeBrokerId":"b","pinnedRuntimeTarget":{"id":"t","type":"docker","runtimeBrokerId":"b"}}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.PinnedRuntimeTarget == nil || a.PinnedRuntimeTarget.ID != "t" || a.PinnedRuntimeTarget.Type != "docker" || a.PinnedRuntimeTarget.RuntimeBrokerID != "b" {
		t.Fatalf("pinnedRuntimeTarget = %+v", a.PinnedRuntimeTarget)
	}
	var legacy Agent
	if err := json.Unmarshal([]byte(`{"id":"a","runtimeBrokerId":"b"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.PinnedRuntimeTarget != nil {
		t.Fatal("an unpinned agent has no pinnedRuntimeTarget")
	}

	var b RuntimeBroker
	if err := json.Unmarshal([]byte(`{"id":"b","name":"flat","runtimeTarget":{"id":"t","type":"docker","displayName":"Local Docker"}}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.RuntimeTarget == nil || b.RuntimeTarget.ID != "t" || b.RuntimeTarget.DisplayName != "Local Docker" {
		t.Fatalf("runtimeTarget = %+v", b.RuntimeTarget)
	}
	out, err := json.Marshal(RuntimeBroker{ID: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["runtimeTarget"]; ok {
		t.Fatal("a legacy Runtime Broker omits runtimeTarget")
	}
}
