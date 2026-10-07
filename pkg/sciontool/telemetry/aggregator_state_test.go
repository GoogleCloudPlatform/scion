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
	"strings"
	"testing"
	"time"
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

// aggregatorFieldsNotInState are the Aggregator fields deliberately left out
// of AggregatorState: the mutex, and the values every hook process reads
// from its environment.
var aggregatorFieldsNotInState = map[string]bool{
	"mu": true, "agentID": true, "projectID": true, "model": true,
}

// Every per-session Aggregator field must have an AggregatorState field of
// the same name, so a new counter cannot be added without being persisted.
func TestAggregatorState_CoversAggregatorFields(t *testing.T) {
	stateType := reflect.TypeOf(AggregatorState{})
	stateFields := map[string]bool{}
	for i := 0; i < stateType.NumField(); i++ {
		stateFields[strings.ToLower(stateType.Field(i).Name)] = true
	}
	aggType := reflect.TypeOf(Aggregator{})
	for i := 0; i < aggType.NumField(); i++ {
		name := aggType.Field(i).Name
		if aggregatorFieldsNotInState[name] {
			continue
		}
		if !stateFields[strings.ToLower(name)] {
			t.Errorf("Aggregator.%s has no AggregatorState field; add it to State/RestoreState (or to aggregatorFieldsNotInState)", name)
		}
	}
}

// Every AggregatorState field, set to a non-zero value, must survive
// RestoreState followed by State, so a field cannot be added to the struct
// without being copied both ways.
func TestAggregatorState_RoundTripsEveryField(t *testing.T) {
	var want AggregatorState
	v := reflect.ValueOf(&want).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x" + v.Type().Field(i).Name)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int64:
			f.SetInt(int64(100 + i))
		case reflect.Map:
			// Only ToolCalls is populated here; any other map field must be
			// added explicitly so it is actually round-tripped.
			if name := v.Type().Field(i).Name; name != "ToolCalls" {
				t.Fatalf("field %s: unhandled map field; extend this test", name)
			}
			f.Set(reflect.ValueOf(map[string]ToolCallStats{"Bash": {Calls: 3, Success: 2, Error: 1}}))
		case reflect.Struct:
			if f.Type() != reflect.TypeOf(time.Time{}) {
				t.Fatalf("field %s: unhandled struct type %s", v.Type().Field(i).Name, f.Type())
			}
			f.Set(reflect.ValueOf(time.Unix(1700000000, 0).UTC()))
		default:
			t.Fatalf("field %s: unhandled kind %s; extend this test", v.Type().Field(i).Name, f.Kind())
		}
	}

	a := NewAggregator()
	a.RestoreState(want)
	if got := a.State(); !reflect.DeepEqual(got, want) {
		t.Errorf("State after RestoreState = %+v\nwant %+v", got, want)
	}
}
