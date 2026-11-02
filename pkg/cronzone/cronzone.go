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

// Package cronzone evaluates a recurring schedule's cron expression in an
// IANA time zone.
//
// A schedule without a zone (or with UTC) is evaluated in UTC, exactly as
// before zones existed. A schedule with a zone matches its cron fields
// against local wall-clock times in that zone, so a "30 19 * * *" schedule in
// America/Los_Angeles runs at 19:30 local time on both sides of a daylight
// saving change. Every returned instant is in UTC.
//
// Daylight saving changes create local times that do not exist (spring
// forward) or that occur twice (fall back). resolveWallTime holds the rule
// for both:
//
//   - A matching local time that is skipped runs once, at the first instant
//     after the gap (the moment the clocks change). Several matches inside one
//     gap collapse into that single run.
//   - A matching local time that occurs twice runs once, at its first
//     occurrence. A schedule that runs more often than hourly therefore does
//     not run again during the repeated hour.
//
// The next run is always strictly after the reference instant. If it is
// computed during the second pass of a repeated hour, after the first
// occurrence of a matching local time has gone by, the second occurrence is
// used.
package cronzone

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxZoneNameLength is the longest zone name ValidateZone accepts. IANA zone
// names are far shorter; the limit only bounds stored values.
const MaxZoneNameLength = 64

// ErrInvalidZone is wrapped by every error ValidateZone returns.
var ErrInvalidZone = errors.New("invalid time zone")

// zoneError carries a user-facing message and matches ErrInvalidZone.
type zoneError struct{ msg string }

func (e *zoneError) Error() string        { return e.msg }
func (e *zoneError) Is(target error) bool { return target == ErrInvalidZone }

// ValidateZone resolves a schedule time zone name. An empty name and "UTC"
// both mean UTC and return time.UTC. "Local" is refused because it depends
// on the zone of the hub process, as are names with surrounding whitespace,
// names longer than MaxZoneNameLength and names that are not in the IANA
// time zone database. Every error matches ErrInvalidZone.
func ValidateZone(name string) (*time.Location, error) {
	switch {
	case name == "" || name == "UTC":
		return time.UTC, nil
	case len(name) > MaxZoneNameLength:
		return nil, &zoneError{fmt.Sprintf("time zone name is longer than %d characters", MaxZoneNameLength)}
	case strings.TrimSpace(name) != name:
		return nil, &zoneError{fmt.Sprintf("time zone %q has leading or trailing whitespace", name)}
	case name == "Local":
		return nil, &zoneError{`time zone "Local" is not supported: use an IANA name such as America/Los_Angeles`}
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, &zoneError{fmt.Sprintf("unknown time zone %q: use an IANA name such as America/Los_Angeles", name)}
	}
	return loc, nil
}

// Schedule is the part of a parsed cron schedule this package needs. The
// schedule must evaluate in UTC (robfig/cron's SpecSchedule with Location set
// to time.UTC): Next treats UTC as a naive wall clock.
type Schedule interface {
	Next(time.Time) time.Time
}

// maxSkips bounds how many matching wall times Next may pass over because
// they map to instants that are not after the reference instant. In practice
// at most one is skipped (the first occurrence of a repeated time); the
// bound only guarantees termination.
const maxSkips = 8

// Next returns the first run of sched strictly after `after`, evaluated in
// loc, as a UTC instant. A nil loc or time.UTC evaluates in UTC exactly as
// sched.Next does. It returns the zero time when sched has no further run
// (robfig/cron gives up after five years).
func Next(sched Schedule, loc *time.Location, after time.Time) time.Time {
	if loc == nil || loc == time.UTC {
		return sched.Next(after.UTC())
	}
	wall := wallClock(after.In(loc))
	for range maxSkips {
		wall = sched.Next(wall)
		if wall.IsZero() {
			return time.Time{}
		}
		if at, ok := resolveWallTime(wall, loc, after); ok {
			return at.UTC()
		}
	}
	return time.Time{}
}

// resolveWallTime maps a matching local wall time (carried in a UTC
// time.Time) to the instant it runs at in loc, and reports false when that
// wall time yields no run after `after`. It is the single place that decides
// the daylight saving rules described in the package doc.
func resolveWallTime(wall time.Time, loc *time.Location, after time.Time) (time.Time, bool) {
	instants := wallInstants(wall, loc)
	if len(instants) == 0 {
		// Skipped (spring forward): run at the end of the gap.
		at := gapEnd(wall, loc)
		return at, at.After(after)
	}
	// Normal (one instant) or repeated (two, ascending): run at the first
	// occurrence that is still ahead.
	for _, at := range instants {
		if at.After(after) {
			return at, true
		}
	}
	return time.Time{}, false
}

// wallClock returns t's local wall-clock reading as a UTC time.
func wallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// candidateOffsets returns the UTC offsets (in seconds) loc uses around the
// wall time: a day before, at, and a day after it. Any transition that can
// affect this wall time lies inside that window, so the offsets in effect on
// both sides of it are included.
func candidateOffsets(wall time.Time, loc *time.Location) []int {
	var offsets []int
	for _, probe := range []time.Time{wall.Add(-24 * time.Hour), wall, wall.Add(24 * time.Hour)} {
		_, off := probe.In(loc).Zone()
		seen := false
		for _, o := range offsets {
			if o == off {
				seen = true
				break
			}
		}
		if !seen {
			offsets = append(offsets, off)
		}
	}
	return offsets
}

// wallInstants returns every instant whose wall-clock reading in loc equals
// wall, in ascending order: none for a skipped time, one normally, and two
// for a repeated time.
func wallInstants(wall time.Time, loc *time.Location) []time.Time {
	var out []time.Time
	for _, off := range candidateOffsets(wall, loc) {
		at := wall.Add(-time.Duration(off) * time.Second)
		if !wallClock(at.In(loc)).Equal(wall) {
			continue
		}
		dup := false
		for _, o := range out {
			if o.Equal(at) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, at)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Before(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// gapEnd returns the instant a skipped wall time's gap ends: the moment the
// clocks changed. Reading the wall time with the offset in effect before the
// gap gives an instant after the change; the start of the zone period that
// instant falls in is the change itself.
func gapEnd(wall time.Time, loc *time.Location) time.Time {
	var latest time.Time
	for _, off := range candidateOffsets(wall, loc) {
		at := wall.Add(-time.Duration(off) * time.Second)
		if at.After(latest) {
			latest = at
		}
	}
	start, _ := latest.In(loc).ZoneBounds()
	if start.IsZero() {
		return latest
	}
	return start
}
