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

// TestDecisionActionableReason covers every branch of the actionable-set
// rule (design choices v5, 4.1).
func TestDecisionActionableReason(t *testing.T) {
	direct := &DecisionAnswer{Provenance: DecisionProvenanceDirect, AnsweredByUserID: "u1"}
	mine := &DecisionAnswer{Provenance: DecisionProvenanceAgentInterpreted, AnsweredByUserID: "u1"}
	theirs := &DecisionAnswer{Provenance: DecisionProvenanceAgentInterpreted, AnsweredByUserID: "u2"}
	gone := &DecisionAnswer{Provenance: DecisionProvenanceAgentInterpreted}
	cases := map[string]struct {
		d    *Decision
		want string
	}{
		"pending":                      {&Decision{Status: DecisionStatusPending}, ""},
		"replaced":                     {&Decision{Status: DecisionStatusReplaced}, AnswerSetReasonReplaced},
		"direct answer":                {&Decision{Status: DecisionStatusDecided, Answers: []*DecisionAnswer{direct}}, AnswerSetReasonDecided},
		"own interpreted answer":       {&Decision{Status: DecisionStatusDecided, Answers: []*DecisionAnswer{mine}}, ""},
		"another user's interpreted":   {&Decision{Status: DecisionStatusDecided, Answers: []*DecisionAnswer{theirs}}, AnswerSetReasonNotRecordedAnswerer},
		"interpreted, answerer gone":   {&Decision{Status: DecisionStatusDecided, Answers: []*DecisionAnswer{gone}}, AnswerSetReasonNotRecordedAnswerer},
		"interpreted then direct":      {&Decision{Status: DecisionStatusDecided, Answers: []*DecisionAnswer{mine, direct}}, AnswerSetReasonDecided},
		"decided with no answer (odd)": {&Decision{Status: DecisionStatusDecided}, AnswerSetReasonNotActionable},
	}
	for name, tc := range cases {
		if got := DecisionActionableReason(tc.d, "u1"); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}
