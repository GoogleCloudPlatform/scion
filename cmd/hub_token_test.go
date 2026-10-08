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

package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestHubTokenCreateHelpUsesRegistryScopes(t *testing.T) {
	help := hubTokenCreateCmd.Long
	if !strings.Contains(help, permissions.UATScopeHelp()) {
		t.Fatal("hub token create help must include registry-derived UAT scope help")
	}
	for _, required := range []string{store.UATScopeProjectUpdate, store.UATScopeAgentPortAccess} {
		if !strings.Contains(help, required) {
			t.Fatalf("hub token create help missing valid scope %q", required)
		}
	}
	for _, stale := range []string{
		store.UATScopeAgentStart,
		store.UATScopeAgentStop,
		store.UATScopeAgentDispatch,
	} {
		if strings.Contains(help, stale) {
			t.Fatalf("hub token create help exposes stale scope %q", stale)
		}
	}
}

func TestParseExpiry_Days(t *testing.T) {
	before := time.Now().UTC()
	result, err := parseExpiry("30d")
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedMin := before.Add(30 * 24 * time.Hour)
	expectedMax := after.Add(30 * 24 * time.Hour)
	if result.Before(expectedMin) || result.After(expectedMax) {
		t.Errorf("expected time around %v, got %v", expectedMin, result)
	}
}

func TestParseExpiry_Years(t *testing.T) {
	before := time.Now().UTC()
	result, err := parseExpiry("1y")
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedMin := before.AddDate(1, 0, 0)
	expectedMax := after.AddDate(1, 0, 0)
	if result.Before(expectedMin) || result.After(expectedMax) {
		t.Errorf("expected time around %v, got %v", expectedMin, result)
	}
}

func TestParseExpiry_RFC3339(t *testing.T) {
	result, err := parseExpiry("2026-12-31T00:00:00Z")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	if !result.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, result)
	}
}

func TestParseExpiry_Invalid(t *testing.T) {
	tests := []string{
		"",
		"x",
		"30",
		"abc",
		"-5d",
		"0d",
		"0h",
		"0m",
		"-2h",
		"-90m",
		"h",
		"m",
		"2s",
		"2w",
		"1.5x",
		"2026-13-01T00:00:00Z",
		// The minute and hour units parse their number strictly
		// (ptone/scion#3771): Go-style and fractional inputs must not be
		// silently read as a shorter duration.
		"1h30m",
		"2h30m",
		"1.5h",
		"1.5m",
		"90mm",
		// A number too large to parse at all gets the generic error.
		"99999999999999999999h",
	}

	for _, input := range tests {
		_, err := parseExpiry(input)
		if err == nil {
			t.Errorf("expected error for input %q, got nil", input)
			continue
		}
		// The error must list every accepted form.
		for _, form := range []string{"90m", "2h", "30d", "1y", "RFC 3339"} {
			if !strings.Contains(err.Error(), form) {
				t.Errorf("error for %q does not list accepted form %q: %v", input, form, err)
			}
		}
	}
}

// TestParseExpiry_OverLimit checks that hour and minute values above the
// hub's 1-year maximum are rejected before multiplying, so huge values cannot
// overflow into an expiry in the past, and that the error names the limit.
func TestParseExpiry_OverLimit(t *testing.T) {
	tests := []string{
		"8761h",
		"525601m",
		"99999999999999h",
		"99999999999999m",
		"9223372036854775807h",
	}

	for _, input := range tests {
		_, err := parseExpiry(input)
		if err == nil {
			t.Errorf("expected error for input %q, got nil", input)
			continue
		}
		want := fmt.Sprintf("%q exceeds the maximum expiry of 1 year (8760h or 525600m)", input)
		if err.Error() != want {
			t.Errorf("error for %q = %q, want %q", input, err.Error(), want)
		}
	}
}

// TestParseExpiryAt_Table pins every accepted --expires form against a fixed
// reference time: the pre-existing day, year and RFC 3339 forms must parse
// exactly as before, and the minute and hour forms (ptone/scion#3771) must
// resolve relative to now.
func TestParseExpiryAt_Table(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Time
	}{
		// Existing forms, unchanged.
		{"30d", now.Add(30 * 24 * time.Hour)},
		{"90d", now.Add(90 * 24 * time.Hour)},
		{"1d", now.Add(24 * time.Hour)},
		{" 7d ", now.Add(7 * 24 * time.Hour)},
		{"1y", now.AddDate(1, 0, 0)},
		{"2026-12-31T00:00:00Z", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)},
		{"2026-12-31T10:00:00+02:00", time.Date(2026, 12, 31, 8, 0, 0, 0, time.UTC)},
		// The day and year units keep their original lax number parse.
		{"1.5d", now.Add(24 * time.Hour)},
		{"3xd", now.Add(3 * 24 * time.Hour)},
		{"2.9y", now.AddDate(2, 0, 0)},
		// New forms.
		{"2h", now.Add(2 * time.Hour)},
		{"1h", now.Add(time.Hour)},
		{"30h", now.Add(30 * time.Hour)},
		{"90m", now.Add(90 * time.Minute)},
		{"1m", now.Add(time.Minute)},
		{" 45m ", now.Add(45 * time.Minute)},
		// The largest hour and minute values accepted: the hub's 1-year
		// maximum token lifetime.
		{"8760h", now.Add(8760 * time.Hour)},
		{"525600m", now.Add(525600 * time.Minute)},
	}
	for _, tt := range tests {
		got, err := parseExpiryAt(tt.in, now)
		if err != nil {
			t.Errorf("parseExpiryAt(%q): unexpected error: %v", tt.in, err)
			continue
		}
		if !got.Equal(tt.want) {
			t.Errorf("parseExpiryAt(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestHubTokenCreateHelpListsExpiryForms checks that both the flag usage and
// the long help list the minute and hour forms alongside days, years and
// RFC 3339.
func TestHubTokenCreateHelpListsExpiryForms(t *testing.T) {
	usage := hubTokenCreateCmd.Flags().Lookup("expires").Usage
	for _, form := range []string{"90m", "2h", "30d", "1y", "RFC 3339"} {
		if !strings.Contains(usage, form) {
			t.Errorf("--expires usage missing %q: %s", form, usage)
		}
		if !strings.Contains(hubTokenCreateCmd.Long, form) {
			t.Errorf("hub token create help missing expiry form %q", form)
		}
	}
	if !strings.Contains(usage, expiryAcceptedForms) {
		t.Errorf("--expires usage is not built from expiryAcceptedForms: %s", usage)
	}
	if !strings.Contains(hubTokenCreateCmd.Long, expiryAcceptedForms) {
		t.Error("hub token create help is not built from expiryAcceptedForms")
	}
	if !strings.Contains(hubTokenCreateCmd.Long, "no month unit") {
		t.Error("hub token create help must say that m means minutes, not months")
	}
}

// TestParseLabelFlags is the F12 regression test (review-2 finding 2(b)):
// parseLabelFlags rejects a repeated --label key instead of silently
// keeping the last value.
func TestParseLabelFlags(t *testing.T) {
	cases := []struct {
		name    string
		input   []string
		want    map[string]string
		wantErr bool
	}{
		{name: "nil input yields nil", input: nil, want: nil},
		{name: "empty slice yields nil", input: []string{}, want: nil},
		{name: "single key=value", input: []string{"k=v"}, want: map[string]string{"k": "v"}},
		{name: "empty value allowed", input: []string{"k="}, want: map[string]string{"k": ""}},
		{name: "missing = is an error", input: []string{"k"}, wantErr: true},
		{name: "repeated key is an error", input: []string{"k=a", "k=b"}, wantErr: true},
		{name: "distinct keys both kept", input: []string{"k=a", "j=b"}, want: map[string]string{"k": "a", "j": "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLabelFlags(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for input %v, got labels %v", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %v: %v", tc.input, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestParseExpiry_90Days(t *testing.T) {
	result, err := parseExpiry("90d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Now().UTC().Add(90 * 24 * time.Hour)
	diff := result.Sub(expected)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected time close to %v, got %v (diff: %v)", expected, result, diff)
	}
}
