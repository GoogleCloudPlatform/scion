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
		ResolveBroker: func(raw string) (string, bool) {
			if raw == "east" {
				return "broker-east-id", true
			}
			return "", false
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
			name:      "broker resolution and unresolvable fallback",
			input:     "broker:east,missing",
			wantScope: AgentQueryScopeNone,
			wantTerms: 1,
			wantFirst: []string{ImpossibleFilterUUID, "broker-east-id"},
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
