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

package artifacts

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestReadGrantFilterMatchesGrantsForRead pins that the read set the list
// query pushes down to SQL is grantsForRead (ptone/scion#5145, sec condition
// (ii)(b)): the helper's arguments are exactly grantsForRead, and both
// pushdowns of the candidate query (the grant arms and the "shared with the
// project" subquery) carry them, so neither can drift from canReadWith.
func TestReadGrantFilterMatchesGrantsForRead(t *testing.T) {
	cond, args := readGrantFilter("g")
	want := make([]any, len(grantsForRead))
	for i, p := range grantsForRead {
		want[i] = p
	}
	if !slices.Equal(args, want) {
		t.Fatalf("readGrantFilter args = %v, want grantsForRead %v", args, want)
	}
	if wantCond := "g.permission IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(want)), ", ") + ")"; cond != wantCond {
		t.Fatalf("readGrantFilter cond = %q, want %q", cond, wantCond)
	}

	s := &sqlStore{dialect: dialectSQLite}
	q := CandidateQuery{
		PrincipalKind: PrincipalKindUser, PrincipalRef: "u1",
		ScopeRefs: []string{"p1"}, HomeScope: "p1", ScopeShares: true,
		Now: time.Unix(0, 0).UTC(), Limit: 10,
	}
	sqlText, qargs := s.candidateQuery(q)
	// The principal and scope grant arms each carry the read set once, and
	// the "shared with the project" subquery is part of the filters every
	// arm shares (owner, principal and scope arms): 2 + 3 = 5.
	const wantFilters = 5
	if got := strings.Count(sqlText, ".permission IN ("); got != wantFilters {
		t.Fatalf("candidate query has %d permission filters, want %d:\n%s", got, wantFilters, sqlText)
	}
	if got := countRuns(qargs, want); got != wantFilters {
		t.Fatalf("candidate query args carry grantsForRead %d times, want %d: %v", got, wantFilters, qargs)
	}
}

// countRuns counts the non-overlapping occurrences of run in args.
func countRuns(args, run []any) int {
	n := 0
	for i := 0; i+len(run) <= len(args); {
		if slices.Equal(args[i:i+len(run)], run) {
			n++
			i += len(run)
			continue
		}
		i++
	}
	return n
}
