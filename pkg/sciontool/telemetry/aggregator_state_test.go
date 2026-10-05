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

package telemetry

import (
	"encoding/json"
	"reflect"
	"testing"
)

// State and RestoreState carry a session across aggregators (and through
// JSON) without changing what Finalize reports.
func TestAggregator_StateRoundTrip(t *testing.T) {
	a := NewAggregator()
	a.ObserveSession("s1") // implicit open
	a.RecordToolEnd("Bash", "")
	a.RecordToolEnd("Bash", "boom")
	a.RecordModelEnd(10, 3, 2, 1)
	a.RecordTurn()

	data, err := json.Marshal(a.State())
	if err != nil {
		t.Fatal(err)
	}
	var st AggregatorState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Open || !st.Implicit || st.SessionID != "s1" {
		t.Fatalf("state = %+v", st)
	}

	b := NewAggregator()
	b.RestoreState(st)
	// A late session-start for the implicitly opened session keeps counts,
	// exactly as it would have on the original aggregator.
	b.StartSession("s1")
	b.RecordTurn()

	a.StartSession("s1")
	a.RecordTurn()

	want := a.Finalize(0, 0, 0, 0, "")
	got := b.Finalize(0, 0, 0, 0, "")
	got.EndedAt = want.EndedAt
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	got.StartedAt = want.StartedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("restored summary = %+v\nwant %+v", got, want)
	}
	if got.TurnCount != 2 || got.ToolCalls["Bash"] != (ToolCallStats{Calls: 2, Success: 1, Error: 1}) {
		t.Errorf("restored counts = %+v", got)
	}
}

// Mutating a restored aggregator does not write through to the state value.
func TestAggregator_RestoreStateCopiesToolCalls(t *testing.T) {
	st := AggregatorState{Open: true, SessionID: "s1", ToolCalls: map[string]ToolCallStats{"Bash": {Calls: 1, Success: 1}}}
	a := NewAggregator()
	a.RestoreState(st)
	a.RecordToolEnd("Bash", "")
	if st.ToolCalls["Bash"].Calls != 1 {
		t.Errorf("state mutated through restored aggregator: %+v", st.ToolCalls)
	}
}
