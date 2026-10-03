// Package clitime formats times for human-readable CLI output.
//
// Every absolute time the CLI prints goes through Format, which converts the
// time to the display zone and always includes a zone token. Relative times
// ("5m ago", "in 3h") go through Relative, which handles both past and future
// times.
//
// The display zone is the process local zone unless the root command sets
// another one from the global --tz or --utc flag (see ResolveZone and
// SetZone). There is deliberately no environment override.
//
// Machine-readable output (--format json) does not use this package: it
// passes the API's UTC strings through unchanged.
package clitime

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Style selects the layout Format uses. Every style has a 24-hour clock (if
// it has a clock at all) and a zone token.
type Style int

const (
	// Full is date, time with seconds, and zone: "2026-10-03 14:05:09 JST".
	Full Style = iota
	// Minute is date, time to the minute, and zone: "2026-10-03 14:05 JST".
	Minute
	// Clock is time with seconds and zone: "14:05:09 JST". Use it only
	// where the date is clear from context, such as a live message tail.
	Clock
	// Date is the calendar date in the display zone, with the zone:
	// "2026-10-03 JST".
	Date
)

// Layouts for each Style. Each one carries the MST zone token, which prints
// the zone abbreviation (or a numeric offset such as "+0545" for zones that
// have no letter abbreviation).
const (
	layoutFull   = "2006-01-02 15:04:05 MST"
	layoutMinute = "2006-01-02 15:04 MST"
	layoutClock  = "15:04:05 MST"
	layoutDate   = "2006-01-02 MST"
)

var (
	mu   sync.RWMutex
	zone *time.Location // nil means time.Local

	// now is the clock Relative measures against; tests replace it.
	now = time.Now
)

// ResolveZone returns the display zone for the global --tz and --utc flags.
// The flags are mutually exclusive. An empty tz with utc false selects the
// process local zone. An unknown zone name is an error.
func ResolveZone(tz string, utc bool) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if utc && tz != "" {
		return nil, fmt.Errorf("--tz and --utc are mutually exclusive")
	}
	if utc {
		return time.UTC, nil
	}
	if tz == "" {
		return time.Local, nil
	}
	// time.LoadLocation treats "" and "UTC" specially and "Local" as the
	// process zone; "Local" is not an IANA name, so reject it to keep the
	// flag's meaning explicit.
	if tz == "Local" {
		return nil, fmt.Errorf("invalid --tz %q: expected an IANA time zone name such as America/New_York, or use --utc", tz)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid --tz %q: expected an IANA time zone name such as America/New_York, or use --utc", tz)
	}
	return loc, nil
}

// SetZone sets the display zone. A nil location restores the process local
// zone.
func SetZone(loc *time.Location) {
	mu.Lock()
	defer mu.Unlock()
	zone = loc
}

// Zone returns the current display zone.
func Zone() *time.Location {
	mu.RLock()
	defer mu.RUnlock()
	if zone == nil {
		return time.Local
	}
	return zone
}

// Format renders t in the display zone with the given style. The zero time
// renders as "-".
func Format(t time.Time, style Style) string {
	if t.IsZero() {
		return "-"
	}
	return t.In(Zone()).Format(layoutFor(style))
}

func layoutFor(style Style) string {
	switch style {
	case Minute:
		return layoutMinute
	case Clock:
		return layoutClock
	case Date:
		return layoutDate
	default:
		return layoutFull
	}
}

// Relative renders t relative to now in a compact form: "just now",
// "5m ago", "3h ago", "2d ago" for past times and "in <1m", "in 5m",
// "in 3h", "in 2d" for future times. Units are truncated, not rounded. The
// zero time renders as "never".
func Relative(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := Now().Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	if d < time.Minute {
		if future {
			return "in <1m"
		}
		return "just now"
	}
	var amount string
	switch {
	case d < time.Hour:
		amount = fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		amount = fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		amount = fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if future {
		return "in " + amount
	}
	return amount + " ago"
}

// Ago is Relative for instants that are always in the past, such as a last
// heartbeat or a creation time. Clock skew between the hub and this machine
// can put such an instant slightly ahead of the local clock; Ago renders any
// future t as "just now" instead of "in 5m". The zero time renders as
// "never".
func Ago(t time.Time) string {
	if !t.IsZero() && t.After(Now()) {
		return "just now"
	}
	return Relative(t)
}

// Now returns the current time from the clock Relative measures against, so
// callers that compare against "now" agree with Relative under SetNow.
func Now() time.Time {
	mu.RLock()
	clock := now
	mu.RUnlock()
	return clock()
}

// SetNow replaces the clock Relative measures against and returns a function
// that restores the previous clock. It exists for tests.
func SetNow(f func() time.Time) (restore func()) {
	mu.Lock()
	defer mu.Unlock()
	prev := now
	now = f
	return func() {
		mu.Lock()
		defer mu.Unlock()
		now = prev
	}
}
