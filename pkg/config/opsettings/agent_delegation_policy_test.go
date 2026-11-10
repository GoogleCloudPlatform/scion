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

package opsettings

import (
	"encoding/json"
	"testing"
)

func TestAgentDelegationPolicyDoc_ParsesRemovalsPerBoundaryKind(t *testing.T) {
	doc, err := ParseAgentDelegationPolicyDoc(json.RawMessage(`{"remove":{"hub":["project.read"],"project":["agent.read"]}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := []struct {
		kind, permission string
		want             bool
	}{
		{"hub", "project.read", true},
		{"hub", "agent.read", false},
		{"project", "agent.read", true},
		{"project", "project.read", false},
	}
	for _, tc := range cases {
		if got := doc.Removes(tc.kind, tc.permission); got != tc.want {
			t.Errorf("Removes(%q, %q) = %v, want %v", tc.kind, tc.permission, got, tc.want)
		}
	}
	empty, err := ParseAgentDelegationPolicyDoc(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if empty.Removes("hub", "agent.read") {
		t.Error("an empty document removes nothing")
	}
}

func TestAgentDelegationPolicyDoc_RefusesWhatItCannotRead(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":          `{`,
		"unknown field":     `{"add":{"hub":["agent.create"]}}`,
		"unknown kind":      `{"remove":{"global":["agent.read"]}}`,
		"empty permission":  `{"remove":{"hub":[""]}}`,
		"wrong value type":  `{"remove":{"hub":"agent.read"}}`,
		"trailing document": `{} {}`,
	} {
		if _, err := ParseAgentDelegationPolicyDoc(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: parse accepted %s", name, raw)
		}
	}
}

func TestAgentDelegationPolicySchema_ValidatesWrites(t *testing.T) {
	if errs := Validate(AgentDelegationPolicySectionName, json.RawMessage(`{"remove":{"project":["agent.read"]}}`)); len(errs) != 0 {
		t.Fatalf("valid document refused: %v", errs)
	}
	for _, raw := range []string{
		`{"add":{"hub":["agent.create"]}}`,
		`{"remove":{"global":["agent.read"]}}`,
		`{"remove":{"hub":[""]}}`,
		`{"remove":{"hub":"agent.read"}}`,
	} {
		if errs := Validate(AgentDelegationPolicySectionName, json.RawMessage(raw)); len(errs) == 0 {
			t.Errorf("schema accepted %s", raw)
		}
	}
}
