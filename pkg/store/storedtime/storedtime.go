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

// Package storedtime parses timestamps as SQLite stores them: Go's
// time.Time.String() text (what the driver writes for a bound time.Time) and
// the RFC 3339 and legacy layouts the raw webchat tables hold. It is the one
// parser shared by the webchat store readers and the utc-timestamp-normalize
// maintenance operation.
//
package storedtime

import (
	"fmt"
	"strings"
	"time"
)

// goStringLayout is time.Time.String()'s layout without its trailing
// zone-abbreviation token.
const goStringLayout = "2006-01-02 15:04:05.999999999 -0700"

// ParseGoString parses text written by Go's time.Time.String() and returns
// the instant in UTC.
//
// It accepts any zone abbreviation, alphabetic ("JST", "UTC") or numeric
// ("-03", "+0545", or the "+0300" a nameless time.FixedZone prints), and an
// optional monotonic-clock suffix (" m=+0.071"). It strips the suffix, drops
// the abbreviation token and parses the rest; the numeric offset is
// authoritative. A "-0700 MST" layout is not used because Go's MST element
// rejects four-digit numeric abbreviations such as Asia/Kathmandu's.
//
// Example inputs:
//
//	2026-10-01 04:00:00 +0000 UTC
//	2026-10-01 14:43:43.309458928 +0900 JST m=+0.088686566
//	2026-10-01 09:45:00 +0545 +0545
func ParseGoString(s string) (time.Time, error) {
	text := strings.TrimSpace(s)
	if i := strings.Index(text, " m="); i >= 0 {
		text = text[:i]
	}
	fields := strings.Fields(text)
	switch len(fields) {
	case 3:
		// Already without an abbreviation.
	case 4:
		fields = fields[:3]
	default:
		return time.Time{}, fmt.Errorf("parse Go time string %q: want date, time, offset and zone abbreviation", s)
	}
	t, err := time.Parse(goStringLayout, strings.Join(fields, " "))
	if err != nil {
		return time.Time{}, fmt.Errorf("parse Go time string %q: %w", s, err)
	}
	return t.UTC(), nil
}

// Parse parses a stored timestamp in any layout SQLite rows can hold and
// returns it in UTC: RFC 3339 with any offset (the canonical webchat form),
// the two legacy webchat layouts ("2006-01-02 15:04:05.999999999-07:00" and
// the zoneless "2006-01-02 15:04:05.999999999", read as UTC), and
// time.Time.String() text (the ent column form, see ParseGoString).
func Parse(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("parse stored time: empty")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", s); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse("2006-01-02 15:04:05.999999999", s); err == nil {
		return parsed.UTC(), nil
	}
	return ParseGoString(s)
}
