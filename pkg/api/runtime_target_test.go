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

package api

import (
	"encoding/json"
	"testing"
)

func TestRuntimeTargetDescriptor_JSONShape(t *testing.T) {
	b, err := json.Marshal(RuntimeTargetDescriptor{ID: "t1", Type: "docker", DisplayName: "Local Docker"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"id":"t1","type":"docker","displayName":"Local Docker"}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	b, err = json.Marshal(RuntimeTargetDescriptor{ID: "t1", Type: "docker"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"id":"t1","type":"docker"}`; got != want {
		t.Fatalf("displayName must be omitted when empty: got %s, want %s", got, want)
	}
}

func TestCheckExpectedRuntimeTarget_MatchEmptyAndMismatch(t *testing.T) {
	if m := CheckExpectedRuntimeTarget("b1", "t1", ""); m != nil {
		t.Fatalf("empty expected must pass, got %+v", m)
	}
	if m := CheckExpectedRuntimeTarget("b1", "t1", "t1"); m != nil {
		t.Fatalf("equal expected must pass, got %+v", m)
	}
	m := CheckExpectedRuntimeTarget("b1", "t1", "t2")
	if m == nil {
		t.Fatal("mismatch must be reported")
	}
	if m.RuntimeBrokerID != "b1" || m.ExpectedRuntimeTargetID != "t2" || m.ActualRuntimeTargetID != "t1" {
		t.Fatalf("unexpected mismatch fields: %+v", m)
	}
	// A legacy Runtime Broker has no target: any non-empty expectation mismatches.
	if m := CheckExpectedRuntimeTarget("b1", "", "t2"); m == nil || m.ActualRuntimeTargetID != "" {
		t.Fatalf("legacy target must mismatch with empty actual, got %+v", m)
	}
}

func TestRuntimeTargetMismatch_MessageAndDetails(t *testing.T) {
	m := &RuntimeTargetMismatch{RuntimeBrokerID: "b1", ExpectedRuntimeTargetID: "t2", ActualRuntimeTargetID: "t1"}
	if got, want := m.Message(), "Runtime Broker b1 serves runtime target t1, but the request expected t2"; got != want {
		t.Fatalf("message: got %q, want %q", got, want)
	}
	d := m.Details()
	for _, k := range []string{"runtimeBrokerId", "expectedRuntimeTargetId", "actualRuntimeTargetId"} {
		if _, ok := d[k]; !ok {
			t.Fatalf("details key %q missing: %v", k, d)
		}
	}
	if len(d) != 3 {
		t.Fatalf("details must have exactly the frozen keys, got %v", d)
	}
	legacy := &RuntimeTargetMismatch{RuntimeBrokerID: "b1", ExpectedRuntimeTargetID: "t2"}
	if got, want := legacy.Message(), "Runtime Broker b1 serves runtime target no runtime target, but the request expected t2"; got != want {
		t.Fatalf("legacy message: got %q, want %q", got, want)
	}
	b, _ := json.Marshal(legacy)
	if got, want := string(b), `{"runtimeBrokerId":"b1","expectedRuntimeTargetId":"t2","actualRuntimeTargetId":""}`; got != want {
		t.Fatalf("legacy JSON: got %s, want %s", got, want)
	}
}

func TestFlatRuntimeBrokerErrorCodes_Distinct(t *testing.T) {
	codes := []string{
		ErrCodeRuntimeTargetMismatch, ErrCodeRuntimeProfileUnsupported, ErrCodeRuntimeTargetRequired,
		ErrCodeRuntimeTargetAckMissing, ErrCodeRuntimeTargetBindingConflict,
		ErrCodeFlatRuntimeBrokerRemoteUnsupported, ErrCodeFlatRuntimeBrokerNotRegistered,
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if c == "" || seen[c] {
			t.Fatalf("code %q empty or duplicated", c)
		}
		seen[c] = true
	}
}
