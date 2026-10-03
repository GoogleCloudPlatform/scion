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

package agentsort

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// TestKeyFor_UpdatedUsesLastActivityWhenSet asserts the COALESCE rule:
// K = LastActivityEvent when non-zero, else Updated.
func TestKeyFor_UpdatedUsesLastActivityWhenSet(t *testing.T) {
	created := mustParse(t, "2026-01-01T00:00:00Z")
	updated := mustParse(t, "2026-01-02T00:00:00Z")
	lastActivity := mustParse(t, "2026-01-03T00:00:00Z")

	row := KeyFor(Updated, "id-1", created, updated, lastActivity)
	if !row.K.Equal(lastActivity) {
		t.Fatalf("K = %v, want lastActivity %v", row.K, lastActivity)
	}

	rowNoActivity := KeyFor(Updated, "id-1", created, updated, time.Time{})
	if !rowNoActivity.K.Equal(updated) {
		t.Fatalf("K = %v, want updated %v (zero LastActivityEvent)", rowNoActivity.K, updated)
	}
}

func TestKeyFor_CreatedUsesCreated(t *testing.T) {
	created := mustParse(t, "2026-01-01T00:00:00Z")
	updated := mustParse(t, "2026-01-02T00:00:00Z")
	row := KeyFor(Created, "id-1", created, updated, time.Time{})
	if !row.K.Equal(created) {
		t.Fatalf("K = %v, want created %v", row.K, created)
	}
}

// TestLess_TiesAlwaysCreatedDescIDDesc pins design 4.2: "Ties therefore show
// exactly as today in both directions" — the tie-break never flips with dir.
func TestLess_TiesAlwaysCreatedDescIDDesc(t *testing.T) {
	sameK := mustParse(t, "2026-01-01T00:00:00Z")
	createdA := mustParse(t, "2026-01-01T00:00:01Z")
	createdB := mustParse(t, "2026-01-01T00:00:02Z") // B created after A

	a := Row{K: sameK, Created: createdA, ID: "a"}
	b := Row{K: sameK, Created: createdB, ID: "b"}

	for _, dir := range []string{Asc, Desc} {
		// B has the later created time, so under "created DESC" B sorts
		// before A, regardless of dir.
		if !Less(dir, b, a) {
			t.Errorf("dir=%s: want b before a (created desc tiebreak)", dir)
		}
		if Less(dir, a, b) {
			t.Errorf("dir=%s: want NOT a before b", dir)
		}
	}
}

func TestLess_IDDescTiebreakWhenCreatedAlsoTies(t *testing.T) {
	sameK := mustParse(t, "2026-01-01T00:00:00Z")
	sameCreated := mustParse(t, "2026-01-01T00:00:01Z")
	a := Row{K: sameK, Created: sameCreated, ID: "aaa"}
	b := Row{K: sameK, Created: sameCreated, ID: "bbb"}
	// id desc: "bbb" > "aaa" so b sorts before a.
	if !Less(Desc, b, a) {
		t.Fatalf("want b before a on id-desc tiebreak")
	}
	if Less(Desc, a, b) {
		t.Fatalf("want NOT a before b")
	}
}

// TestLess_PrimaryKeyRespectsDir checks the primary K comparison flips with
// dir while the tiebreak does not.
func TestLess_PrimaryKeyRespectsDir(t *testing.T) {
	earlier := Row{K: mustParse(t, "2026-01-01T00:00:00Z"), Created: mustParse(t, "2026-01-01T00:00:00Z"), ID: "a"}
	later := Row{K: mustParse(t, "2026-01-02T00:00:00Z"), Created: mustParse(t, "2026-01-02T00:00:00Z"), ID: "b"}

	if !Less(Desc, later, earlier) {
		t.Errorf("desc: want later before earlier")
	}
	if !Less(Asc, earlier, later) {
		t.Errorf("asc: want earlier before later")
	}
}

// TestSortRows_TrailingZeroFractionsAtBoundary covers the sub-second
// RFC3339Nano trailing-zero case the design calls out (4.2): Go's true-time
// ordering must place these correctly even though their string
// representations would misorder under localeCompare.
func TestSortRows_TrailingZeroFractionsAtBoundary(t *testing.T) {
	// "…05.12Z" is chronologically after "…05.1Z" (0.12s > 0.10s), even
	// though lexicographically "05.12" < "05.1" is false in this case but
	// the reverse (05.5 vs 05, or 05.12 vs 05.1) demonstrates real
	// mis-ordering under string compare; agentsort must always agree with
	// true chronological order regardless of string form.
	t1 := mustParse(t, "2026-01-01T00:00:05.1Z")  // 100ms
	t2 := mustParse(t, "2026-01-01T00:00:05.12Z") // 120ms, chronologically after t1

	rows := []Row{
		{K: t2, Created: t2, ID: "b"},
		{K: t1, Created: t1, ID: "a"},
	}
	SortRows(Desc, rows)
	if rows[0].ID != "b" || rows[1].ID != "a" {
		t.Fatalf("desc order = %v, want [b, a] (true time, not string order)", rows)
	}

	SortRows(Asc, rows)
	if rows[0].ID != "a" || rows[1].ID != "b" {
		t.Fatalf("asc order = %v, want [a, b]", rows)
	}
}

// TestSortRows_NonUTC pins that a non-UTC time.Time compares correctly by
// instant, not by its zone-local clock fields (design's own test plan: "one
// non-UTC time.Time").
func TestSortRows_NonUTC(t *testing.T) {
	utc := mustParse(t, "2026-01-01T12:00:00Z")
	// Same instant, expressed in a +02:00 offset: 14:00+02:00 == 12:00Z.
	sameInstant := utc.In(time.FixedZone("test+2", 2*60*60))
	later := mustParse(t, "2026-01-01T12:00:01Z")

	rows := []Row{
		{K: later, Created: later, ID: "later"},
		{K: sameInstant, Created: sameInstant, ID: "same"},
	}
	SortRows(Desc, rows)
	if rows[0].ID != "later" || rows[1].ID != "same" {
		t.Fatalf("order = %v, want [later, same]", rows)
	}

	// And the "same instant" row must tie (not error) against a UTC row
	// with the identical instant.
	if Compare(Desc, Row{K: utc, Created: utc, ID: "x"}, Row{K: sameInstant, Created: sameInstant, ID: "x"}) != 0 {
		t.Fatalf("expected equal instants (different zones) to compare equal")
	}
}

func TestCompare_Basic(t *testing.T) {
	a := Row{K: mustParse(t, "2026-01-01T00:00:00Z"), Created: mustParse(t, "2026-01-01T00:00:00Z"), ID: "a"}
	b := Row{K: mustParse(t, "2026-01-02T00:00:00Z"), Created: mustParse(t, "2026-01-02T00:00:00Z"), ID: "b"}
	if Compare(Desc, b, a) != -1 {
		t.Errorf("want b before a (desc) => -1")
	}
	if Compare(Desc, a, b) != 1 {
		t.Errorf("want a after b (desc) => 1")
	}
	if Compare(Desc, a, a) != 0 {
		t.Errorf("want equal => 0")
	}
}
