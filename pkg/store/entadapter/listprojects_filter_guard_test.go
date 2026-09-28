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

// NO BUILD TAG — this file must run under -tags no_sqlite (CI mode).
//
// Source-scan guard for DEF-112: verifies that ListProjects contains no
// unconditional query.Where() call. An unconditional filter silently
// breaks the reachable/unreachable message split in the residual
// attribution report (design §4.6):
//
//   - CountUnreachableUnbackfilledMessages uses NOT EXISTS (... FROM
//     projects ...) to identify messages whose project row is absent.
//   - The backfill iterates ListProjects(ctx, ProjectFilter{}, ...) to
//     determine which messages it can reach.
//   - These agree only because ListProjects with an empty filter returns
//     every project row — NOT EXISTS is its exact complement.
//
// An unconditional filter (e.g. excluding archived projects) would cause
// ListProjects to return fewer projects than the table contains. Messages
// in filtered-out projects become unreachable in fact but classified as
// reachable by the counter. The WARN fires on every boot with a number
// no action can reduce — exactly the alarm-fatigue bug M6 existed to fix.
//
// This test catches unconditional filters that no data-driven test can
// detect: a filter keyed on archived/deleted/visibility/template status
// would exclude no row in a test database where all rows are at their
// defaults, so the consistency test (TestReachableCountConsistency_DEF112
// in cmd/) would pass while the drift ships to production.
//
// The two guards together are stronger than either alone:
//   - This test catches the structural violation regardless of seed data.
//   - The consistency test catches semantic drift from any cause,
//     including helper functions that hide the filter from source scanning.

package entadapter

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestListProjects_NoUnconditionalFilter_DEF112 reads project_store.go
// from disk, locates the ListProjects function body, and asserts that no
// query.Where() call appears at the function body's base indentation
// (one tab). gofmt guarantees that all conditional query.Where() calls
// sit inside if/switch/for blocks at two or more tabs.
//
// The discriminator:
//   - One tab:   unconditional (top level of function body)
//   - Two+ tabs: conditional (inside if, switch, case, for, etc.)
//
// Handles both statement forms:
//   - query.Where(...)          — mutating call
//   - query = query.Where(...)  — assignment form
//
// Known limitation: a multi-line call split as query.\n\t\tWhere(...)
// would not be detected. This is unlikely under gofmt for a simple
// single-predicate call, and the consistency test guards against it.
func TestListProjects_NoUnconditionalFilter_DEF112(t *testing.T) {
	src, err := os.ReadFile("project_store.go")
	if err != nil {
		t.Fatalf("reading project_store.go: %v", err)
	}

	lines := strings.Split(string(src), "\n")

	// Locate the ListProjects function body by matching the signature
	// line and tracking brace depth.
	inFunction := false
	braceDepth := 0
	funcStartLine := 0
	var violations []string

	// Matches query.Where( or query = query.Where( at exactly one tab.
	// Two+ tabs do not match, so conditional calls inside if/switch/for
	// are not flagged. gofmt guarantees the indentation.
	unconditionalWhere := regexp.MustCompile(
		`^\tquery(\.Where\(|\s*=\s*query\.Where\()`)

	for i, line := range lines {
		if !inFunction {
			if strings.Contains(line, "func (s *ProjectStore) ListProjects(") {
				inFunction = true
				funcStartLine = i + 1
				braceDepth = strings.Count(line, "{") - strings.Count(line, "}")
			}
			continue
		}

		braceDepth += strings.Count(line, "{") - strings.Count(line, "}")

		// Skip comment-only lines.
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		if unconditionalWhere.MatchString(line) {
			violations = append(violations, fmt.Sprintf(
				"  line %d: %s", i+1, trimmed))
		}

		if braceDepth <= 0 {
			break // end of function body
		}
	}

	if !inFunction {
		t.Fatal("ListProjects function not found in project_store.go; " +
			"has the method been renamed or moved?")
	}

	if len(violations) > 0 {
		t.Errorf("DEF-112: ListProjects (starting at line %d) contains "+
			"unconditional query.Where() call(s):\n\n%s\n\n"+
			"An unconditional filter causes ListProjects to return fewer "+
			"projects than the table contains, which breaks the reachable/"+
			"unreachable message split in the residual attribution report.\n\n"+
			"CountUnreachableUnbackfilledMessages uses NOT EXISTS (... FROM "+
			"projects ...) to count messages whose project row is absent. "+
			"The backfill iterates ListProjects to determine which messages "+
			"it can reach. These agree only because ListProjects with an "+
			"empty filter returns every project row. An unconditional filter "+
			"makes some messages unreachable in fact but classified as "+
			"reachable by the counter, and the WARN fires on every boot "+
			"with a number no action can reduce.\n\n"+
			"If you need to add a filter that applies regardless of the "+
			"caller's ProjectFilter, you must also update "+
			"CountUnreachableUnbackfilledMessages to exclude the same "+
			"projects, and verify that TestReachableCountConsistency_DEF112 "+
			"(in cmd/) still passes.",
			funcStartLine, strings.Join(violations, "\n"))
	}
}
