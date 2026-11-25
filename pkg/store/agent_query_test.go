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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
)

func TestTranslateAgentQuery_ScopeAndResolution(t *testing.T) {
	resolver := AgentQueryResolver{
		ResolveProject: func(raw string) (string, bool) {
			if raw == "alpha" {
				return "11111111-1111-1111-1111-111111111111", true
			}
			return "", false
		},
		ResolveBroker: func(raw string) ([]string, bool) {
			if raw == "east" {
				return []string{"broker-east-2", "broker-east-1"}, true
			}
			return nil, false
		},
		ResolveOwner: func(raw string) (string, bool) {
			if raw == "me" {
				return "22222222-2222-2222-2222-222222222222", true
			}
			return "", false
		},
	}

	tests := []struct {
		name      string
		input     string
		wantScope AgentQueryScope
		wantTerms int
		wantFirst []string
	}{
		{
			name:      "is:mine extracts scope with nil Terms",
			input:     "is:mine",
			wantScope: AgentQueryScopeMine,
			wantTerms: 0,
		},
		{
			name:      "is:shared extracts scope",
			input:     "is:shared",
			wantScope: AgentQueryScopeShared,
			wantTerms: 0,
		},
		{
			name:      "negated -is:mine maps to shared",
			input:     "-is:mine",
			wantScope: AgentQueryScopeShared,
			wantTerms: 0,
		},
		{
			name:      "contradictory is:mine -is:mine produces conflict",
			input:     "is:mine -is:mine",
			wantScope: AgentQueryScopeConflict,
			wantTerms: 0,
		},
		{
			name:      "unresolvable project maps to ImpossibleFilterUUID",
			input:     "project:missing",
			wantScope: AgentQueryScopeNone,
			wantTerms: 1,
			wantFirst: []string{ImpossibleFilterUUID},
		},
		{
			name:      "resolved project and owner:me",
			input:     "project:alpha owner:me",
			wantScope: AgentQueryScopeNone,
			wantTerms: 2,
			wantFirst: []string{"22222222-2222-2222-2222-222222222222"}, // owner sorts before project
		},
		{
			name:      "broker resolution expands all same-named brokers and unresolvable fallback",
			input:     "broker:east,missing",
			wantScope: AgentQueryScopeNone,
			wantTerms: 1,
			wantFirst: []string{ImpossibleFilterUUID, "broker-east-1", "broker-east-2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, fqErr := filterquery.Parse(tc.input, schemas.Agents.HasKey)
			if fqErr != nil {
				t.Fatalf("Parse(%q): %v", tc.input, fqErr)
			}
			got := TranslateAgentQuery(q, resolver)
			if got.Scope != tc.wantScope {
				t.Errorf("Scope = %q, want %q", got.Scope, tc.wantScope)
			}
			gotCount := 0
			if got.Terms != nil {
				gotCount = len(got.Terms.Terms)
			}
			if gotCount != tc.wantTerms {
				t.Fatalf("len(Terms) = %d, want %d (got=%+v)", gotCount, tc.wantTerms, got.Terms)
			}
			if tc.wantFirst != nil {
				gotVals := got.Terms.Terms[0].Values
				if len(gotVals) != len(tc.wantFirst) {
					t.Fatalf("first term values = %v, want %v", gotVals, tc.wantFirst)
				}
				for i := range gotVals {
					if gotVals[i] != tc.wantFirst[i] {
						t.Errorf("first term values[%d] = %q, want %q", i, gotVals[i], tc.wantFirst[i])
					}
				}
			}
		})
	}
}

func TestTranslateAgentQuery_MemoizesRepeatedReferents(t *testing.T) {
	calls := 0
	resolver := AgentQueryResolver{
		ResolveProject: func(raw string) (string, bool) {
			calls++
			return "11111111-1111-1111-1111-111111111111", true
		},
	}
	parts := make([]string, 32)
	for i := range parts {
		parts[i] = "project:x"
	}
	q, fqErr := filterquery.Parse(strings.Join(parts, " "), schemas.Agents.HasKey)
	if fqErr != nil {
		t.Fatalf("Parse: %v", fqErr)
	}
	got := TranslateAgentQuery(q, resolver)
	if calls != 1 {
		t.Fatalf("ResolveProject called %d times for 32 copies of project:x, want 1", calls)
	}
	if got.Terms == nil || len(got.Terms.Terms) != 32 {
		t.Fatalf("expected 32 terms, got %+v", got.Terms)
	}
}

func TestTranslateAgentQuery_ProjectCaseInsensitiveOrderIndependent(t *testing.T) {
	const alphaID = "11111111-1111-1111-1111-111111111111"
	resolver := AgentQueryResolver{
		// Exact-match lowercase map, mirroring slugs stored via api.Slugify.
		ResolveProject: func(raw string) (string, bool) {
			if raw == "foo" {
				return alphaID, true
			}
			return "", false
		},
	}

	for _, input := range []string{"project:foo", "project:Foo", "project:Foo,foo", "project:foo,Foo"} {
		q, fqErr := filterquery.Parse(input, schemas.Agents.HasKey)
		if fqErr != nil {
			t.Fatalf("Parse(%q): %v", input, fqErr)
		}
		got := TranslateAgentQuery(q, resolver)
		if got.Terms == nil || len(got.Terms.Terms) != 1 {
			t.Fatalf("%q: expected 1 term, got %+v", input, got.Terms)
		}
		vals := got.Terms.Terms[0].Values
		if len(vals) != 1 || vals[0] != alphaID {
			t.Errorf("%q: resolved values = %v, want [%s]", input, vals, alphaID)
		}
	}
}

func TestQueryTerms_WithoutPhaseAndMatchesPhase(t *testing.T) {
	var nilQT *QueryTerms
	if nilQT.HasPhaseTerms() || nilQT.WithoutPhase() != nil || !nilQT.MatchesPhase("running") {
		t.Fatalf("nil QueryTerms behaved unexpectedly")
	}

	// Phase-only query strips to nil.
	qPhaseOnly, fqErr := filterquery.Parse("phase:running", schemas.Agents.HasKey)
	if fqErr != nil {
		t.Fatalf("Parse: %v", fqErr)
	}
	qtPhaseOnly := TranslateAgentQuery(qPhaseOnly, AgentQueryResolver{}).Terms
	if !qtPhaseOnly.HasPhaseTerms() {
		t.Fatalf("expected HasPhaseTerms=true")
	}
	if qtPhaseOnly.WithoutPhase() != nil {
		t.Fatalf("expected WithoutPhase() == nil for phase-only query, got %+v", qtPhaseOnly.WithoutPhase())
	}
	if !qtPhaseOnly.MatchesPhase("running") || qtPhaseOnly.MatchesPhase("stopped") || qtPhaseOnly.MatchesPhase("") {
		t.Fatalf("MatchesPhase mismatch for phase:running")
	}

	// Mixed query keeps non-phase terms and evaluates multi-value + negated phase terms.
	qMixed, fqErr := filterquery.Parse("phase:running,error -phase:error label:env=prod", schemas.Agents.HasKey)
	if fqErr != nil {
		t.Fatalf("Parse: %v", fqErr)
	}
	qtMixed := TranslateAgentQuery(qMixed, AgentQueryResolver{}).Terms
	stripped := qtMixed.WithoutPhase()
	if stripped == nil || stripped.HasPhaseTerms() || len(stripped.Terms) != 1 || stripped.Terms[0].Field != "label" {
		t.Fatalf("unexpected WithoutPhase result: %+v", stripped)
	}
	if !qtMixed.MatchesPhase("running") || qtMixed.MatchesPhase("error") || qtMixed.MatchesPhase("stopped") {
		t.Fatalf("MatchesPhase mismatch for phase:running,error -phase:error")
	}
}
