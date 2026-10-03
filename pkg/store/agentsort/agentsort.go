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

// Package agentsort is the single reference implementation of the
// server-sorted agent list total order. Both the
// project-endpoint positioning (pkg/hub) and the test suites that assert
// pages concatenate to "the agentsort reference" use it, so there is exactly
// one place that can get a tie-break wrong.
package agentsort

import (
	stdsort "sort"
	"time"
)

// Sort key names, matching the "sort" query parameter (design 4.1).
const (
	Created = "created"
	Updated = "updated"
)

// Direction names, matching the "dir" query parameter (design 4.1).
const (
	Asc  = "asc"
	Desc = "desc"
)

// Row is the (K, created, id) tuple the total order is defined over. K is
// the position key: for sort=updated it is LastActivityEvent when non-zero,
// else Updated; for sort=created it is Created. Every row that enters a
// comparison must be built with the same sort, so KeyFor is the one
// constructor.
type Row struct {
	K       time.Time
	Created time.Time
	ID      string
}

// KeyFor builds the Row for one agent's position under sort. lastActivity
// may be the zero time when the agent has none, which matches the store's
// NULL representation (design 4.2: "the store writes a zero
// LastActivityEvent as NULL... COALESCE therefore matches the client's
// '0001' rule").
func KeyFor(sort string, id string, created, updated, lastActivity time.Time) Row {
	k := created
	if sort == Updated {
		k = updated
		if !lastActivity.IsZero() {
			k = lastActivity
		}
	}
	return Row{K: k, Created: created, ID: id}
}

// Less reports whether a sorts strictly before b in the section-4.2 total
// order for dir. The sort key is already folded into Row.K by KeyFor, so
// only the direction is needed here:
//
//	updated: (K dir, created DESC, id DESC)
//	created: (created dir, id DESC)      -- K == Created, so this falls out
//	                                         of the same rule with no special
//	                                         case.
//
// Ties are always broken by created DESC then id DESC, regardless of dir —
// this is what makes both directions show ties in the same created-desc
// order, matching today's client (design 4.2).
func Less(dir string, a, b Row) bool {
	if !a.K.Equal(b.K) {
		if dir == Asc {
			return a.K.Before(b.K)
		}
		return a.K.After(b.K)
	}
	if !a.Created.Equal(b.Created) {
		return a.Created.After(b.Created)
	}
	return a.ID > b.ID
}

// Compare returns -1 if a sorts before b, 1 if after, 0 if the tuples are
// identical (which, since ID is a unique key, only happens for a == b).
func Compare(dir string, a, b Row) int {
	if Less(dir, a, b) {
		return -1
	}
	if Less(dir, b, a) {
		return 1
	}
	return 0
}

// SortRows sorts rows in place into the section-4.2 total order for dir
// (rows must come from KeyFor with a single sort key). It is used by the
// project endpoint, which reads its candidate set unordered (bounded by the
// candidate ceiling) and positions it in Go rather than in SQL.
func SortRows(dir string, rows []Row) {
	stdsort.Slice(rows, func(i, j int) bool { return Less(dir, rows[i], rows[j]) })
}
