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
	"encoding/json"
	"os"
	"testing"
	"time"
)

// goldenFixture mirrors testdata/order_golden.json, which asserts the
// server order and lists the sub-second cases explicitly.
type goldenFixture struct {
	Description         string             `json:"description"`
	Rows                []goldenFixtureRow `json:"rows"`
	ExpectedUpdatedDesc []string           `json:"expectedUpdatedDesc"`
	ExpectedUpdatedAsc  []string           `json:"expectedUpdatedAsc"`
	ExpectedCreatedDesc []string           `json:"expectedCreatedDesc"`
	ExpectedCreatedAsc  []string           `json:"expectedCreatedAsc"`
}

type goldenFixtureRow struct {
	ID                string `json:"id"`
	Created           string `json:"created"`
	Updated           string `json:"updated"`
	LastActivityEvent string `json:"lastActivityEvent"`
}

func loadGoldenFixture(t *testing.T) goldenFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/order_golden.json")
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	var fx goldenFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse golden fixture: %v", err)
	}
	return fx
}

func parseGoldenTime(t *testing.T, s string) time.Time {
	t.Helper()
	if s == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse golden timestamp %q: %v", s, err)
	}
	return ts
}

func orderIDs(sortKey, dir string, t *testing.T, fx goldenFixture) []string {
	t.Helper()
	type idRow struct {
		id  string
		row Row
	}
	irows := make([]idRow, len(fx.Rows))
	for i, r := range fx.Rows {
		created := parseGoldenTime(t, r.Created)
		updated := parseGoldenTime(t, r.Updated)
		lae := parseGoldenTime(t, r.LastActivityEvent)
		irows[i] = idRow{id: r.ID, row: KeyFor(sortKey, r.ID, created, updated, lae)}
	}
	rows := make([]Row, len(irows))
	for i, ir := range irows {
		rows[i] = ir.row
	}
	SortRows(dir, rows)
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}

func assertIDOrder(t *testing.T, label string, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: length mismatch: want %v, got %v", label, want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("%s: order mismatch at index %d: want %v, got %v", label, i, want, got)
		}
	}
}

// TestGoldenOrder pins the sorted-mode total order against the
// committed golden fixture, including the sub-second trailing-zero-fraction
// pair (t1/t2) where true-time order differs from what a naive string
// (localeCompare) comparison would produce.
func TestGoldenOrder(t *testing.T) {
	fx := loadGoldenFixture(t)

	assertIDOrder(t, "updated desc", fx.ExpectedUpdatedDesc, orderIDs(Updated, Desc, t, fx))
	assertIDOrder(t, "updated asc", fx.ExpectedUpdatedAsc, orderIDs(Updated, Asc, t, fx))
	assertIDOrder(t, "created desc", fx.ExpectedCreatedDesc, orderIDs(Created, Desc, t, fx))
	assertIDOrder(t, "created asc", fx.ExpectedCreatedAsc, orderIDs(Created, Asc, t, fx))
}
