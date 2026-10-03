package clitime

import (
	"strings"
	"testing"
	"time"
)

// instant is 2026-10-03 05:04:09 UTC: 14:04 in Tokyo, 01:04 in New York
// (EDT), 10:49 in Kathmandu.
var instant = time.Date(2026, 10, 3, 5, 4, 9, 0, time.UTC)

func withZone(t *testing.T, loc *time.Location) {
	t.Helper()
	SetZone(loc)
	t.Cleanup(func() { SetZone(nil) })
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestFormatStyles(t *testing.T) {
	tests := []struct {
		zone  string
		style Style
		want  string
	}{
		{"UTC", Full, "2026-10-03 05:04:09 UTC"},
		{"UTC", Minute, "2026-10-03 05:04 UTC"},
		{"UTC", Clock, "05:04:09 UTC"},
		{"UTC", Date, "2026-10-03 UTC"},
		{"Asia/Tokyo", Full, "2026-10-03 14:04:09 JST"},
		{"America/New_York", Full, "2026-10-03 01:04:09 EDT"},
		{"America/New_York", Date, "2026-10-03 EDT"},
		// A zone without a letter abbreviation prints a numeric one.
		{"Asia/Kathmandu", Minute, "2026-10-03 10:49 +0545"},
		// The date follows the display zone, not UTC.
		{"Pacific/Kiritimati", Date, "2026-10-03 +14"},
		{"Pacific/Pago_Pago", Date, "2026-10-02 SST"},
	}
	for _, tc := range tests {
		t.Run(tc.zone, func(t *testing.T) {
			withZone(t, mustLoad(t, tc.zone))
			if got := Format(instant, tc.style); got != tc.want {
				t.Errorf("Format(%v, %d) in %s = %q, want %q", instant, tc.style, tc.zone, got, tc.want)
			}
		})
	}
}

func TestFormatIgnoresInputLocation(t *testing.T) {
	withZone(t, time.UTC)
	tokyo := instant.In(mustLoad(t, "Asia/Tokyo"))
	if got, want := Format(tokyo, Full), "2026-10-03 05:04:09 UTC"; got != want {
		t.Errorf("Format = %q, want %q", got, want)
	}
}

func TestFormatHourCycle(t *testing.T) {
	withZone(t, time.UTC)
	midnight := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 1, 2, 18, 30, 0, 0, time.UTC)
	if got, want := Format(midnight, Minute), "2026-01-02 00:00 UTC"; got != want {
		t.Errorf("midnight = %q, want %q", got, want)
	}
	if got, want := Format(evening, Clock), "18:30:00 UTC"; got != want {
		t.Errorf("evening = %q, want %q", got, want)
	}
}

func TestFormatZero(t *testing.T) {
	if got := Format(time.Time{}, Full); got != "-" {
		t.Errorf("Format(zero) = %q, want %q", got, "-")
	}
}

func TestZoneDefaultsToLocal(t *testing.T) {
	SetZone(nil)
	if Zone() != time.Local {
		t.Errorf("Zone() = %v, want time.Local", Zone())
	}
}

func TestResolveZone(t *testing.T) {
	ny := mustLoad(t, "America/New_York")

	if loc, err := ResolveZone("", false); err != nil || loc != time.Local {
		t.Errorf("ResolveZone(\"\", false) = %v, %v; want Local", loc, err)
	}
	if loc, err := ResolveZone("", true); err != nil || loc != time.UTC {
		t.Errorf("ResolveZone(\"\", true) = %v, %v; want UTC", loc, err)
	}
	loc, err := ResolveZone("America/New_York", false)
	if err != nil || loc.String() != ny.String() {
		t.Errorf("ResolveZone(America/New_York) = %v, %v", loc, err)
	}
	if loc, err := ResolveZone("UTC", false); err != nil || loc.String() != "UTC" {
		t.Errorf("ResolveZone(UTC) = %v, %v", loc, err)
	}

	for _, bad := range []string{"Mars/Olympus", "Local", "not a zone", "+09:00"} {
		if _, err := ResolveZone(bad, false); err == nil || !strings.Contains(err.Error(), "invalid --tz") {
			t.Errorf("ResolveZone(%q) error = %v, want an invalid --tz error", bad, err)
		}
	}
	if _, err := ResolveZone("America/New_York", true); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("ResolveZone(tz, utc) error = %v, want mutually exclusive", err)
	}
}

func TestRelative(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	restore := SetNow(func() time.Time { return base })
	defer restore()

	tests := []struct {
		offset time.Duration
		want   string
	}{
		{0, "just now"},
		{-59 * time.Second, "just now"},
		{-time.Minute, "1m ago"},
		{-5*time.Minute - 59*time.Second, "5m ago"},
		{-time.Hour, "1h ago"},
		{-23*time.Hour - 59*time.Minute, "23h ago"},
		{-24 * time.Hour, "1d ago"},
		{-45 * 24 * time.Hour, "45d ago"},
		{30 * time.Second, "in <1m"},
		{5 * time.Minute, "in 5m"},
		{3*time.Hour + 10*time.Minute, "in 3h"},
		{50 * time.Hour, "in 2d"},
	}
	for _, tc := range tests {
		if got := Relative(base.Add(tc.offset)); got != tc.want {
			t.Errorf("Relative(now%+v) = %q, want %q", tc.offset, got, tc.want)
		}
	}
	if got := Relative(time.Time{}); got != "never" {
		t.Errorf("Relative(zero) = %q, want never", got)
	}
}

func TestRelativeIsZoneIndependent(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	restore := SetNow(func() time.Time { return base })
	defer restore()
	withZone(t, mustLoad(t, "Asia/Kathmandu"))
	past := base.Add(-90 * time.Minute).In(mustLoad(t, "Asia/Tokyo"))
	if got := Relative(past); got != "1h ago" {
		t.Errorf("Relative = %q, want 1h ago", got)
	}
}

func TestNowFollowsSetNow(t *testing.T) {
	fixed := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	restore := SetNow(func() time.Time { return fixed })
	if got := Now(); !got.Equal(fixed) {
		restore()
		t.Fatalf("Now() = %v, want %v", got, fixed)
	}
	restore()
	if got := Now(); got.Equal(fixed) {
		t.Fatalf("Now() still returns the fixed clock after restore")
	}
}
