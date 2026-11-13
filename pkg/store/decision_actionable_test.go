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

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type actionableGoldenAnswer struct {
	Provenance       string `json:"provenance"`
	Outcome          string `json:"outcome"`
	AnsweredByUserID string `json:"answeredByUserId"`
}

type actionableGoldenCase struct {
	Name     string                   `json:"name"`
	UserID   string                   `json:"userId"`
	Status   string                   `json:"status"`
	Answers  []actionableGoldenAnswer `json:"answers"`
	Decision *actionableGoldenAnswer  `json:"decision"`
	Want     string                   `json:"want"`
}

// TestDecisionActionableReason covers every branch of the actionable-set
// rule (design choices v5, 4.1) from the shared golden table also consumed
// by the web Vitest suite.
func TestDecisionActionableReason(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "decision_actionable_cases.json"))
	if err != nil {
		t.Fatalf("read golden actionable cases: %v", err)
	}
	var cases []actionableGoldenCase
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cases); err != nil {
		t.Fatalf("unmarshal golden actionable cases: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("unexpected trailing data in golden actionable cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("golden actionable cases table is empty")
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			d := &Decision{Status: tc.Status}
			for _, a := range tc.Answers {
				d.Answers = append(d.Answers, &DecisionAnswer{
					Provenance:       a.Provenance,
					Outcome:          a.Outcome,
					AnsweredByUserID: a.AnsweredByUserID,
				})
			}
			eff := d.EffectiveAnswer()
			switch {
			case tc.Decision == nil && eff != nil:
				t.Fatalf("EffectiveAnswer() = %+v, want nil", eff)
			case tc.Decision != nil && eff == nil:
				t.Fatalf("EffectiveAnswer() = nil, want %+v", *tc.Decision)
			case tc.Decision != nil:
				if eff.Provenance != tc.Decision.Provenance || eff.Outcome != tc.Decision.Outcome || eff.AnsweredByUserID != tc.Decision.AnsweredByUserID {
					t.Fatalf("EffectiveAnswer() = (%q,%q,%q), want (%q,%q,%q)",
						eff.Provenance, eff.Outcome, eff.AnsweredByUserID,
						tc.Decision.Provenance, tc.Decision.Outcome, tc.Decision.AnsweredByUserID)
				}
			}
			if got := DecisionActionableReason(d, tc.UserID); got != tc.Want {
				t.Errorf("DecisionActionableReason(%s, %q) = %q, want %q", tc.Status, tc.UserID, got, tc.Want)
			}
		})
	}
}
