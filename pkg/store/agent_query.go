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
	"slices"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
)

// ImpossibleFilterUUID is substituted for unresolvable or out-of-scope
// reference filters (project, broker, owner) so that nonexistent and
// unauthorized referents are indistinguishable and produce an empty 200
// rather than an error or existence oracle.
const ImpossibleFilterUUID = "00000000-0000-0000-0000-000000000000"

// AgentQueryScope is the scope classification extracted from `is:mine` /
// `is:shared` terms in an agents filter query.
type AgentQueryScope string

const (
	// AgentQueryScopeNone indicates no `is:` scope term was present.
	AgentQueryScopeNone AgentQueryScope = ""
	// AgentQueryScopeMine restricts results to the caller's owned projects.
	AgentQueryScopeMine AgentQueryScope = "mine"
	// AgentQueryScopeShared restricts results to shared projects.
	AgentQueryScopeShared AgentQueryScope = "shared"
	// AgentQueryScopeConflict indicates contradictory `is:` terms (e.g.
	// `is:mine -is:mine` or `-is:mine -is:shared`) that can match no agent.
	AgentQueryScopeConflict AgentQueryScope = "conflict"
)

// AgentQueryResolver resolves reference values in an agents filter query to
// canonical store IDs. Each hook is optional; when nil, values pass through
// unchanged (useful in store-level tests that supply raw IDs directly).
// A hook returning ("", false) marks the referent unresolvable and substitutes
// ImpossibleFilterUUID.
type AgentQueryResolver struct {
	ResolveProject func(raw string) (string, bool)
	ResolveBroker  func(raw string) (string, bool)
	ResolveOwner   func(raw string) (string, bool)
}

// TranslatedAgentQuery is the canonicalized store representation of a
// validated agents filterquery.Query.
type TranslatedAgentQuery struct {
	Terms *QueryTerms
	Scope AgentQueryScope
}

// TranslateAgentQuery translates a validated agents filterquery.Query into
// canonicalized store.QueryTerms (deterministic for cursor bindings) plus any
// extracted `is:` scope classification. Both the hub handlers and store parity
// tests call this single translator.
func TranslateAgentQuery(q filterquery.Query, resolver AgentQueryResolver) TranslatedAgentQuery {
	if q.IsEmpty() {
		return TranslatedAgentQuery{}
	}

	qt := &QueryTerms{
		Text: q.Text,
	}
	if len(q.ExcludeText) > 0 {
		excl := append([]string(nil), q.ExcludeText...)
		sort.Strings(excl)
		qt.ExcludeText = slices.Compact(excl)
	}

	var wantMine, wantShared bool

	if len(q.Terms) > 0 {
		terms := make([]QueryTerm, 0, len(q.Terms))
		for _, t := range q.Terms {
			key := strings.ToLower(t.Key)
			if key == "is" {
				for _, v := range t.Values {
					switch strings.ToLower(v) {
					case "mine":
						if t.Negate {
							wantShared = true
						} else {
							wantMine = true
						}
					case "shared":
						if t.Negate {
							wantMine = true
						} else {
							wantShared = true
						}
					}
				}
				continue
			}

			vals := make([]string, len(t.Values))
			for i, v := range t.Values {
				switch key {
				case "label":
					vals[i] = v
				case "project":
					vals[i] = resolveOrImpossible(v, resolver.ResolveProject)
				case "broker":
					vals[i] = resolveOrImpossible(v, resolver.ResolveBroker)
				case "owner":
					vals[i] = resolveOrImpossible(v, resolver.ResolveOwner)
				default:
					vals[i] = strings.ToLower(v)
				}
			}
			sort.Strings(vals)
			vals = slices.Compact(vals)

			op := QueryOpEq
			if key == "label" {
				allHasKey := true
				allEq := true
				for _, v := range vals {
					if strings.Contains(v, "=") {
						allHasKey = false
					} else {
						allEq = false
					}
				}
				if allHasKey {
					op = QueryOpHasKey
				} else if allEq {
					op = QueryOpLabelEq
				}
			}
			terms = append(terms, QueryTerm{
				Field:  key,
				Values: vals,
				Negate: t.Negate,
				Op:     op,
			})
		}
		if len(terms) > 0 {
			sort.Slice(terms, func(i, j int) bool {
				if terms[i].Field != terms[j].Field {
					return terms[i].Field < terms[j].Field
				}
				if terms[i].Negate != terms[j].Negate {
					return !terms[i].Negate
				}
				return strings.Join(terms[i].Values, ",") < strings.Join(terms[j].Values, ",")
			})
			qt.Terms = terms
		}
	}

	var scope AgentQueryScope
	switch {
	case wantMine && wantShared:
		scope = AgentQueryScopeConflict
	case wantMine:
		scope = AgentQueryScopeMine
	case wantShared:
		scope = AgentQueryScopeShared
	}

	if qt.Text == "" && len(qt.ExcludeText) == 0 && len(qt.Terms) == 0 {
		qt = nil
	}

	return TranslatedAgentQuery{
		Terms: qt,
		Scope: scope,
	}
}

func resolveOrImpossible(raw string, fn func(string) (string, bool)) string {
	if fn == nil {
		return raw
	}
	resolved, ok := fn(raw)
	if !ok || resolved == "" {
		return ImpossibleFilterUUID
	}
	return resolved
}
