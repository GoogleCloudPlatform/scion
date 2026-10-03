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

package storedtime

import (
	"testing"
	"time"
	_ "time/tzdata" // the zone table below must not depend on the host's zoneinfo

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

// TestParseGoString_Zones covers every zone shape the SQLite driver can
// have written through time.Time.String(): alphabetic, two-digit and
// four-digit numeric abbreviations, nameless and named fixed zones, with and
// without a monotonic-clock suffix.
func TestParseGoString_Zones(t *testing.T) {
	// 2026-01-15 is southern-hemisphere summer, so Lord_Howe is on DST (+11).
	instant := time.Date(2026, 1, 15, 3, 4, 5, 123456789, time.UTC)

	zones := []struct {
		name     string
		loc      *time.Location
		wantAbbr string // the abbreviation String() prints, pinned so the case stays meaningful
	}{
		{"Asia/Kathmandu", mustLoadLocation(t, "Asia/Kathmandu"), "+0545"},
		{"nameless FixedZone +3h", time.FixedZone("", 3*60*60), "+0300"},
		{"named FixedZone XYZ -5h", time.FixedZone("XYZ", -5*60*60), "XYZ"},
		{"Asia/Tokyo", mustLoadLocation(t, "Asia/Tokyo"), "JST"},
		{"UTC", time.UTC, "UTC"},
		{"America/Sao_Paulo", mustLoadLocation(t, "America/Sao_Paulo"), "-03"},
		{"Australia/Lord_Howe (DST)", mustLoadLocation(t, "Australia/Lord_Howe"), "+11"},
		{"Asia/Kolkata", mustLoadLocation(t, "Asia/Kolkata"), "IST"},
		{"Asia/Dubai", mustLoadLocation(t, "Asia/Dubai"), "+04"},
	}
	for _, z := range zones {
		t.Run(z.name, func(t *testing.T) {
			text := instant.In(z.loc).String()
			require.Equal(t, z.wantAbbr, text[len(text)-len(z.wantAbbr):], "unexpected String() text %q", text)

			for _, in := range []string{text, text + " m=+0.088686566", text + " m=-12.5"} {
				got, err := ParseGoString(in)
				require.NoError(t, err, in)
				assert.True(t, got.Equal(instant), "%q parsed to %v, want %v", in, got, instant)
				assert.Equal(t, time.UTC, got.Location(), in)
			}
		})
	}
}

func TestParseGoString_Literals(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
	}{
		{"2026-10-01 04:00:00 +0000 UTC", time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)},
		{"2026-10-01 04:00:00.12 +0000 UTC", time.Date(2026, 10, 1, 4, 0, 0, 120000000, time.UTC)},
		{"2026-10-01 14:43:43.309458928 +0900 JST m=+0.088686566", time.Date(2026, 10, 1, 5, 43, 43, 309458928, time.UTC)},
		{"2026-10-01 09:45:00 +0545 +0545", time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)},
		{"2026-10-01 09:45:00.5 +0545 +0545 m=+1.000000001", time.Date(2026, 10, 1, 4, 0, 0, 500000000, time.UTC)},
		{"2026-10-01 07:00:00 +0300 +0300", time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)},
		{"2026-10-01 04:00:00 +0000", time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		got, err := ParseGoString(tt.in)
		require.NoError(t, err, tt.in)
		assert.True(t, got.Equal(tt.want), "%q parsed to %v, want %v", tt.in, got, tt.want)
		assert.Equal(t, time.UTC, got.Location(), tt.in)
	}
}

func TestParseGoString_Rejects(t *testing.T) {
	for _, in := range []string{
		"",
		"garbage",
		"2026-10-01T04:00:00Z",
		"2026-10-01 04:00:00",
		"2026-10-01 04:00:00 UTC",
		"2026-10-01 04:00:00 +0000 UTC extra",
	} {
		_, err := ParseGoString(in)
		assert.Error(t, err, "%q should not parse", in)
	}
}

func TestParse_Layouts(t *testing.T) {
	want := time.Date(2026, 10, 1, 4, 0, 0, 500000000, time.UTC)
	for _, in := range []string{
		"2026-10-01T04:00:00.5Z",
		"2026-10-01T13:00:00.5+09:00",
		"2026-10-01 13:00:00.5+09:00",
		"2026-10-01 04:00:00.5",
		"2026-10-01 04:00:00.5 +0000 UTC",
		"2026-10-01 09:45:00.5 +0545 +0545 m=+3.25",
	} {
		got, err := Parse(in)
		require.NoError(t, err, in)
		assert.True(t, got.Equal(want), "%q parsed to %v, want %v", in, got, want)
		assert.Equal(t, time.UTC, got.Location(), in)
	}
}

// TestParse_ErrorOmitsValue pins that a parse error never carries the input,
// so callers may log it without leaking a stored value.
func TestParse_ErrorOmitsValue(t *testing.T) {
	const secretish = "not-a-time-4f2a9c"
	for _, fn := range []func(string) (time.Time, error){Parse, ParseGoString} {
		_, err := fn(secretish)
		require.ErrorIs(t, err, ErrUnparseable)
		assert.NotContains(t, err.Error(), secretish)
	}
	_, err := Parse("")
	assert.ErrorIs(t, err, ErrUnparseable)
}
