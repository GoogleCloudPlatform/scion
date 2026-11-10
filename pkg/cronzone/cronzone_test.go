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

package cronzone

import (
	"errors"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zones below must not depend on the host's zoneinfo

	"github.com/robfig/cron/v3"
)

var testParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// utcSpec parses expr the way the hub does: 5 fields, pinned to UTC.
func utcSpec(t *testing.T, expr string) cron.Schedule {
	t.Helper()
	s, err := testParser.Parse(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	s.(*cron.SpecSchedule).Location = time.UTC
	return s
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := ValidateZone(name)
	if err != nil {
		t.Fatalf("ValidateZone(%q): %v", name, err)
	}
	return loc
}

func mustUTC(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v.UTC()
}

func TestValidateZone(t *testing.T) {
	for _, name := range []string{"", "UTC"} {
		loc, err := ValidateZone(name)
		if err != nil || loc != time.UTC {
			t.Errorf("ValidateZone(%q) = %v, %v; want time.UTC, nil", name, loc, err)
		}
	}
	for _, name := range []string{"America/Los_Angeles", "Europe/London", "Australia/Lord_Howe", "Etc/UTC", "Asia/Kolkata"} {
		loc, err := ValidateZone(name)
		if err != nil {
			t.Errorf("ValidateZone(%q): unexpected error %v", name, err)
			continue
		}
		if loc.String() != name {
			t.Errorf("ValidateZone(%q) = %q", name, loc.String())
		}
	}
	refused := map[string]string{
		"Mars/Olympus_Mons":     `unknown time zone "Mars/Olympus_Mons"`,
		"Local":                 `"Local" is not supported`,
		" America/New_York":     "leading or trailing whitespace",
		"America/New_York\n":    "leading or trailing whitespace",
		strings.Repeat("A", 65): "longer than 64",
		"../etc/passwd":         "unknown time zone",
	}
	for name, want := range refused {
		loc, err := ValidateZone(name)
		if err == nil {
			t.Errorf("ValidateZone(%q) = %v, want an error", name, loc)
			continue
		}
		if !errors.Is(err, ErrInvalidZone) {
			t.Errorf("ValidateZone(%q) error %v does not match ErrInvalidZone", name, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateZone(%q) error %q, want it to contain %q", name, err, want)
		}
	}
}

// TestNextUTCUnchanged checks that a nil zone and UTC give exactly the
// schedule's own UTC result.
func TestNextUTCUnchanged(t *testing.T) {
	sched := utcSpec(t, "30 2 * * *")
	after := mustUTC(t, "2026-03-08T01:00:00Z")
	want := sched.Next(after)
	for _, loc := range []*time.Location{nil, time.UTC} {
		if got := Next(sched, loc, after); !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("Next(loc=%v) = %v, want %v in UTC", loc, got, want)
		}
	}
	// A non-UTC reference instant must not change the result.
	la := mustZone(t, "America/Los_Angeles")
	if got := Next(sched, time.UTC, after.In(la)); !got.Equal(want) {
		t.Errorf("Next with LA-zoned reference = %v, want %v", got, want)
	}
}

// runs returns the first n runs of expr in zone, starting after start.
func runs(t *testing.T, expr, zone, start string, n int) []string {
	t.Helper()
	sched := utcSpec(t, expr)
	loc := mustZone(t, zone)
	at := mustUTC(t, start)
	var out []string
	for range n {
		at = Next(sched, loc, at)
		if at.IsZero() {
			t.Fatalf("%s in %s: no next run", expr, zone)
		}
		if at.Location() != time.UTC {
			t.Fatalf("Next returned %v, not UTC", at.Location())
		}
		out = append(out, at.Format(time.RFC3339))
	}
	return out
}

func TestNextDST(t *testing.T) {
	// America/Los_Angeles 2026: clocks go 02:00 PST -> 03:00 PDT on
	// 2026-03-08 (10:00Z) and 02:00 PDT -> 01:00 PST on 2026-11-01 (09:00Z).
	// Australia/Lord_Howe 2026: 02:00 +11 -> 01:30 +10:30 on 2026-04-05
	// (15:00Z the day before) and 02:00 +10:30 -> 02:30 +11 on 2026-10-04
	// (15:30Z the day before).
	cases := []struct {
		name, expr, zone, start string
		want                    []string
	}{
		{
			name: "daily local time keeps the wall clock across spring forward",
			expr: "30 19 * * *", zone: "America/Los_Angeles", start: "2026-03-06T12:00:00Z",
			want: []string{"2026-03-07T03:30:00Z", "2026-03-08T03:30:00Z", "2026-03-09T02:30:00Z", "2026-03-10T02:30:00Z"},
		},
		{
			name: "daily local time keeps the wall clock across fall back",
			expr: "30 19 * * *", zone: "America/Los_Angeles", start: "2026-10-30T12:00:00Z",
			want: []string{"2026-10-31T02:30:00Z", "2026-11-01T02:30:00Z", "2026-11-02T03:30:00Z"},
		},
		{
			name: "skipped 02:30 runs once at the end of the gap and the day is not lost",
			expr: "30 2 * * *", zone: "America/Los_Angeles", start: "2026-03-07T00:00:00Z",
			want: []string{"2026-03-07T10:30:00Z", "2026-03-08T10:00:00Z", "2026-03-09T09:30:00Z"},
		},
		{
			name: "sub-hourly matches inside the gap collapse into one run",
			expr: "*/20 * * * *", zone: "America/Los_Angeles", start: "2026-03-08T09:30:00Z",
			// 01:40 PST, 02:00/02:20/02:40 skipped -> 03:00 PDT (also a
			// match itself), then 03:20 PDT.
			want: []string{"2026-03-08T09:40:00Z", "2026-03-08T10:00:00Z", "2026-03-08T10:20:00Z"},
		},
		{
			name: "repeated 01:30 runs once at the first occurrence",
			expr: "30 1 * * *", zone: "America/Los_Angeles", start: "2026-10-31T00:00:00Z",
			want: []string{"2026-10-31T08:30:00Z", "2026-11-01T08:30:00Z", "2026-11-02T09:30:00Z"},
		},
		{
			name: "sub-hourly schedule does not repeat the second pass of the hour",
			expr: "*/30 * * * *", zone: "America/Los_Angeles", start: "2026-11-01T07:45:00Z",
			// 00:45 PDT start: 01:00 and 01:30 PDT, then 02:00 and 02:30
			// PST; 01:00 and 01:30 PST are not repeated.
			want: []string{"2026-11-01T08:00:00Z", "2026-11-01T08:30:00Z", "2026-11-01T10:00:00Z", "2026-11-01T10:30:00Z"},
		},
		{
			name: "computed during the second pass, the second occurrence is used",
			expr: "30 1 * * *", zone: "America/Los_Angeles", start: "2026-11-01T09:10:00Z",
			// 01:10 PST: 01:30 PDT has gone by, 01:30 PST is still ahead.
			want: []string{"2026-11-01T09:30:00Z", "2026-11-02T09:30:00Z"},
		},
		{
			name: "half-hour shift: repeated 01:45 runs once at the first occurrence",
			expr: "45 1 * * *", zone: "Australia/Lord_Howe", start: "2026-04-04T12:00:00Z",
			want: []string{"2026-04-04T14:45:00Z", "2026-04-05T15:15:00Z"},
		},
		{
			name: "half-hour shift: skipped 02:15 runs at the end of the gap",
			expr: "15 2 * * *", zone: "Australia/Lord_Howe", start: "2026-10-03T12:00:00Z",
			want: []string{"2026-10-03T15:30:00Z", "2026-10-04T15:15:00Z"},
		},
		{
			name: "sub-hourly schedule computed during the second pass uses the second occurrence once",
			expr: "*/30 * * * *", zone: "America/Los_Angeles", start: "2026-11-01T09:10:00Z",
			// 01:10 PST: 01:30 PDT has gone by, 01:30 PST is still ahead;
			// then 02:00 PST.
			want: []string{"2026-11-01T09:30:00Z", "2026-11-01T10:00:00Z"},
		},
		{
			name: "day of week is matched on the local date, not the UTC date",
			expr: "0 20 * * 5", zone: "America/Los_Angeles", start: "2026-03-04T00:00:00Z",
			// Friday 20:00 local is Saturday in UTC, on both sides of the
			// spring change (2026-03-08).
			want: []string{"2026-03-07T04:00:00Z", "2026-03-14T03:00:00Z"},
		},
		{
			name: "day of month is matched on the local date, not the UTC date",
			expr: "0 0 1 * *", zone: "Asia/Tokyo", start: "2026-03-15T00:00:00Z",
			// 00:00 on the 1st in Tokyo is 15:00 on the previous day in UTC.
			want: []string{"2026-03-31T15:00:00Z", "2026-04-30T15:00:00Z"},
		},
		{
			name: "zone without daylight saving is a fixed shift",
			expr: "0 9 * * 1", zone: "Asia/Kolkata", start: "2026-03-01T00:00:00Z",
			want: []string{"2026-03-02T03:30:00Z", "2026-03-09T03:30:00Z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runs(t, tc.expr, tc.zone, tc.start, len(tc.want))
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("runs of %q in %s after %s:\n got  %v\n want %v", tc.expr, tc.zone, tc.start, got, tc.want)
			}
		})
	}
}

// TestNextLocalDateFields checks the local weekday and day of month of the
// runs whose UTC date differs from the local one.
func TestNextLocalDateFields(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	for _, s := range runs(t, "0 20 * * 5", "America/Los_Angeles", "2026-03-04T00:00:00Z", 2) {
		local := mustUTC(t, s).In(la)
		if local.Weekday() != time.Friday || local.Hour() != 20 {
			t.Errorf("run %s is %v local, want Friday 20:00", s, local)
		}
	}
	tokyo := mustZone(t, "Asia/Tokyo")
	for _, s := range runs(t, "0 0 1 * *", "Asia/Tokyo", "2026-03-15T00:00:00Z", 2) {
		local := mustUTC(t, s).In(tokyo)
		if local.Day() != 1 || local.Hour() != 0 {
			t.Errorf("run %s is %v local, want the 1st at 00:00", s, local)
		}
	}
}

// TestNextStrictlyAfter checks that the result is after the reference instant
// even when the reference is exactly a run time.
func TestNextStrictlyAfter(t *testing.T) {
	sched := utcSpec(t, "30 1 * * *")
	loc := mustZone(t, "America/Los_Angeles")
	for _, ref := range []string{"2026-11-01T08:30:00Z", "2026-11-01T09:30:00Z", "2026-03-08T10:00:00Z"} {
		after := mustUTC(t, ref)
		if got := Next(sched, loc, after); !got.After(after) {
			t.Errorf("Next after %s = %v, not strictly after", ref, got)
		}
	}
}

// TestNextNoRun checks that a schedule that never fires returns zero.
func TestNextNoRun(t *testing.T) {
	sched := utcSpec(t, "0 0 30 2 *")
	if got := Next(sched, mustZone(t, "Europe/London"), mustUTC(t, "2026-01-01T00:00:00Z")); !got.IsZero() {
		t.Errorf("Next for 30 February = %v, want zero", got)
	}
}
