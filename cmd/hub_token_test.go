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
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
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
		// Whole scope names only: agent:stop_all is a valid scope.
		if regexp.MustCompile(`(^|[^a-z_:])` + regexp.QuoteMeta(stale) + `($|[^a-z_])`).MatchString(help) {
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

	expectedMin := before.Add(365 * 24 * time.Hour)
	expectedMax := after.Add(365 * 24 * time.Hour)
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
		// The day and year units parse their number just as strictly
		// (ptone/scion#3811).
		"1.5d",
		"3xd",
		"xd",
		"1e2d",
		"0d",
		"-1d",
		"d",
		"1.5y",
		"2.9y",
		"0y",
		"-1y",
		"y",
		// A number too large to parse at all gets the generic error.
		"99999999999999999999h",
		"99999999999999999999d",
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

// TestParseExpiry_OverLimit checks that values in any unit above the
// hub's 1-year maximum are rejected before multiplying, so huge values cannot
// overflow into an expiry in the past, and that the error names the limit.
func TestParseExpiry_OverLimit(t *testing.T) {
	tests := []string{
		"8761h",
		"525601m",
		"99999999999999h",
		"99999999999999m",
		"9223372036854775807h",
		"366d",
		"99999999999999d",
		"9223372036854775807d",
		"2y",
		"9223372036854775807y",
	}

	for _, input := range tests {
		_, err := parseExpiry(input)
		if err == nil {
			t.Errorf("expected error for input %q, got nil", input)
			continue
		}
		want := fmt.Sprintf("%q exceeds the maximum expiry of 1 year (1y, 365d, 8760h or 525600m)", input)
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
		{"1y", now.Add(365 * 24 * time.Hour)},
		{"2026-12-31T00:00:00Z", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)},
		{"2026-12-31T10:00:00+02:00", time.Date(2026, 12, 31, 8, 0, 0, 0, time.UTC)},
		// The largest day value accepted: the hub's 1-year maximum.
		{"365d", now.Add(365 * 24 * time.Hour)},
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

// TestParseExpiryAt_YearIs365Days pins that 1y means exactly 365 days, the
// hub's maximum token lifetime, so it is never over that maximum: neither
// when the following year contains 29 February (where one calendar year
// would be 366 days) nor in a span without a leap day (ptone/scion#3930).
func TestParseExpiryAt_YearIs365Days(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
	}{
		{"span containing 29 February", time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)},
		{"span starting on 29 February", time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC)},
		{"span without a leap day", time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseExpiryAt("1y", tt.now)
			if err != nil {
				t.Fatalf("parseExpiryAt(1y): unexpected error: %v", err)
			}
			want := tt.now.Add(365 * 24 * time.Hour)
			if !got.Equal(want) {
				t.Errorf("parseExpiryAt(1y) = %v, want %v", got, want)
			}
			if lifetime := got.Sub(tt.now); lifetime > store.UATMaxExpiry {
				t.Errorf("1y lifetime %v exceeds the hub maximum %v", lifetime, store.UATMaxExpiry)
			}
			// 2y stays over the limit at every reference time.
			if _, err := parseExpiryAt("2y", tt.now); err == nil {
				t.Error("parseExpiryAt(2y): expected an over-limit error, got nil")
			}
		})
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

// TestHubTokenHelp_BoundaryAndLiveAccess pins the boundary-related help:
// the live-access paragraph, the hub-token project-name note, and the
// list and scopes flags and their text.
func TestHubTokenHelp_BoundaryAndLiveAccess(t *testing.T) {
	for _, want := range []string{
		"checked against your current access",
		"Hub tokens and project names",
		"project:list (only a hub-wide role",
		"run inside a linked checkout",
		"scion hub token scopes --boundary hub",
		"restore",
		"service-accounts list",
	} {
		if !strings.Contains(hubTokenCmd.Long, want) {
			t.Errorf("hub token help missing %q", want)
		}
	}
	if !strings.Contains(hubTokenCreateCmd.Long, `"--boundary hub"`) {
		t.Error("hub token create help must point to the hub eligibility listing")
	}
	for _, want := range []string{"BOUNDARY", "--wide", "PURPOSE", "--boundary project|hub", "matches hub tokens"} {
		if !strings.Contains(hubTokenListCmd.Long, want) {
			t.Errorf("hub token list help missing %q", want)
		}
	}
	for _, want := range []string{"--boundary hub", "Boundary (--boundary):"} {
		if !strings.Contains(hubTokenScopesCmd.Long, want) {
			t.Errorf("hub token scopes help missing %q", want)
		}
	}
	for _, f := range []string{"boundary", "wide", "project", "json"} {
		if hubTokenListCmd.Flags().Lookup(f) == nil {
			t.Errorf("hub token list must register --%s", f)
		}
	}
	for _, f := range []string{"boundary", "project", "json"} {
		if hubTokenScopesCmd.Flags().Lookup(f) == nil {
			t.Errorf("hub token scopes must register --%s", f)
		}
	}
}

// TestHubTokenHelp_HubSettingsRows pins the hub settings table in the hub
// token help: it is rendered from hubSettingsScopeRows (the constants the
// commands' refusal hints name), every subcommand of the three hub settings
// commands is named in a row, every label fits the table's command column,
// and every scope is a hub-boundary selector.
func TestHubTokenHelp_HubSettingsRows(t *testing.T) {
	if !strings.Contains(hubTokenCmd.Long, hubSettingsScopeTable()) {
		t.Fatal("hub token help does not contain the hub settings table")
	}
	for _, sc := range []string{srvcfgReadScope, srvcfgWriteScope, projDefaultsReadScope, projDefaultsWriteScope, experimentsScope} {
		if !strings.Contains(hubSettingsScopeTable(), sc) {
			t.Errorf("hub settings table does not name %s", sc)
		}
	}
	want := map[string][]string{ // the row's command lines, joined -> its scopes
		"scion hub settings get, schema":                     {srvcfgReadScope},
		"scion hub settings set, unset":                      {srvcfgReadScope, srvcfgWriteScope},
		"scion hub settings unset --section NAME --reset":    {srvcfgWriteScope},
		"scion hub project-defaults get":                     {projDefaultsReadScope},
		"scion hub project-defaults set, unset":              {projDefaultsReadScope, projDefaultsWriteScope},
		"scion hub experiments list, enable, disable, reset": {experimentsScope},
	}
	if len(hubSettingsScopeRows()) != len(want) {
		t.Errorf("hub settings table has %d rows, want %d", len(hubSettingsScopeRows()), len(want))
	}
	for _, line := range strings.Split(hubSettingsScopeTable(), "\n") {
		if len(line) > hubTokenHelpWidth {
			t.Errorf("hub settings table line is %d characters, wider than %d: %q", len(line), hubTokenHelpWidth, line)
		}
	}
	text := map[string]string{} // parent command name -> its rows' command text
	for _, row := range hubSettingsScopeRows() {
		label := strings.Join(strings.Fields(strings.Join(row.commands, " ")), " ")
		if w, ok := want[label]; !ok {
			t.Errorf("unexpected row %q", label)
		} else if strings.Join(row.scopes, ",") != strings.Join(w, ",") {
			t.Errorf("row %q has scopes %v, want %v", label, row.scopes, w)
		}
		for _, line := range row.commands {
			if len(line) > 29 {
				t.Errorf("label %q is longer than 29 characters, leaving under two spaces before the scopes", line)
			}
		}
		f := strings.Fields(row.commands[0])
		if len(f) < 3 || f[0] != "scion" || f[1] != "hub" {
			t.Errorf("row %q does not start with scion hub <command>", row.commands[0])
			continue
		}
		parent := strings.TrimSuffix(f[2], ",")
		text[parent] += " " + strings.Join(row.commands, " ")
		for _, sc := range row.scopes {
			m, ok := permissions.ResolveSelector(sc)
			if !ok {
				t.Errorf("row %q: scope %s is not a selector", row.commands[0], sc)
				continue
			}
			hub := false
			for _, k := range m.AllowedBoundaries {
				if k == permissions.BoundaryKindHub {
					hub = true
				}
			}
			if !hub {
				t.Errorf("row %q: scope %s cannot be issued on a hub boundary", row.commands[0], sc)
			}
		}
	}
	word := regexp.MustCompile(`[a-z-]+`)
	for _, parent := range []*cobra.Command{hubSettingsCmd, hubProjectDefaultsCmd, hubExperimentsCmd} {
		named := map[string]bool{}
		for _, w := range word.FindAllString(text[parent.Name()], -1) {
			named[w] = true
		}
		for _, sub := range parent.Commands() {
			if !named[sub.Name()] {
				t.Errorf("scion hub %s %s has no row in the hub token help", parent.Name(), sub.Name())
			}
		}
	}
}

// TestHubTokenHelp_NamedScopesAreSelectors pins that every scope the hub
// token help names (scope:action words) is a selector a token can carry, so
// a renamed or misspelt scope fails here.
func TestHubTokenHelp_NamedScopesAreSelectors(t *testing.T) {
	words := helpScopeWords(hubTokenCmd.Long)
	if len(words) < 30 {
		t.Fatalf("found only %d scope words in the hub token help; the extraction no longer matches it", len(words))
	}
	// Every scope:action candidate in the help is a scope word: one the
	// extraction skips (joined to a longer token) fails here, not silently.
	if raw := regexp.MustCompile(`[a-z][a-z_]*:[a-z][a-z_]*`).FindAllString(hubTokenCmd.Long, -1); len(raw) != len(words) {
		t.Fatalf("extracted %d scope words from %d candidates in the hub token help: %v vs %v", len(words), len(raw), words, raw)
	}
	for _, w := range words {
		if _, ok := permissions.ResolveSelector(w); !ok {
			t.Errorf("hub token help names %s, which is not a token selector", w)
		}
	}
}

// TestHelpScopeWords_ExtractsEveryListedScope checks the extraction itself:
// every scope of a comma-joined list without spaces is found, words that
// are part of a longer token are not, and a misspelt scope found this way
// does not resolve (the control for the test above).
func TestHelpScopeWords_ExtractsEveryListedScope(t *testing.T) {
	got := helpScopeWords("--scopes project:read,hub_config:raed,agent:list\n  scion message   project:read, agent:message (x:y) a/b:c d:e-f SCION:X")
	want := []string{"project:read", "hub_config:raed", "agent:list", "project:read", "agent:message", "x:y"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("helpScopeWords = %v, want %v", got, want)
	}
	if _, ok := permissions.ResolveSelector(got[1]); ok {
		t.Fatal("control: the misspelt hub_config:raed must not resolve")
	}
	got = helpScopeWords("x --scopes project:read,hub_config:raed y")
	if strings.Join(got, " ") != "project:read hub_config:raed" {
		t.Fatalf("helpScopeWords = %v, want both words of the comma-joined list", got)
	}
	got = helpScopeWords("needs hub_config:raed.")
	if strings.Join(got, " ") != "hub_config:raed" {
		t.Fatalf("helpScopeWords = %v, want the scope that ends the sentence", got)
	}
	got = helpScopeWords("needs hub_config:raed. Also agent:read: here, and skill:list.")
	if strings.Join(got, " ") != "hub_config:raed agent:read skill:list" {
		t.Fatalf("helpScopeWords = %v, want the scopes that end a sentence or a label", got)
	}
	if _, ok := permissions.ResolveSelector(got[0]); ok {
		t.Fatal("control: a misspelt scope ending a sentence must not resolve")
	}
	if got := helpScopeWords("v1.2:b x:y.z a.b:c.d"); len(got) != 0 {
		t.Fatalf("helpScopeWords = %v, want none: a '.' or ':' inside a token still joins", got)
	}
}

// helpScopeWords returns the scope:action words of help text in order: a
// lowercase word, a colon and a lowercase word, not joined to a longer
// token (letters, digits, '_', '-', '/', '.' or ':' on either side). A '.'
// or ':' followed by whitespace or the end of the text ends a sentence or
// a label and does not join.
func helpScopeWords(text string) []string {
	re := regexp.MustCompile(`[a-z][a-z_]*:[a-z][a-z_]*`)
	joined := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-' || c == '/' || c == '.' || c == ':'
	}
	var out []string
	for _, loc := range re.FindAllStringIndex(text, -1) {
		if loc[0] > 0 && joined(text[loc[0]-1]) {
			continue
		}
		if loc[1] < len(text) && joined(text[loc[1]]) && !sentenceEnd(text, loc[1]) {
			continue
		}
		out = append(out, text[loc[0]:loc[1]])
	}
	return out
}

// sentenceEnd reports whether text[i] is a '.' or ':' followed by
// whitespace or the end of the text.
func sentenceEnd(text string, i int) bool {
	if text[i] != '.' && text[i] != ':' {
		return false
	}
	return i+1 == len(text) || text[i+1] == ' ' || text[i+1] == '\n' || text[i+1] == '\t'
}

// TestHubTokenHelp_ProjectSettingsRows pins the scion project settings rows
// of the hub token help: rendered from the scopes the commands' refusal
// hints name, every subcommand named, within the table width.
func TestHubTokenHelp_ProjectSettingsRows(t *testing.T) {
	lines := projectSettingsScopeLines()
	if !strings.Contains(hubTokenCmd.Long, lines) {
		t.Fatal("hub token help does not contain the project settings rows")
	}
	if !strings.Contains(lines, "scion project settings get     "+projSettingsReadScope+"\n") {
		t.Errorf("project settings get row does not name %s:\n%s", projSettingsReadScope, lines)
	}
	if !strings.Contains(lines, projSettingsReadScope+", "+projSettingsWriteScope) {
		t.Errorf("project settings set/unset row does not name %s and %s:\n%s", projSettingsReadScope, projSettingsWriteScope, lines)
	}
	for _, line := range strings.Split(strings.TrimRight(lines, "\n"), "\n") {
		if len(line) > hubTokenHelpWidth {
			t.Errorf("line is %d characters, wider than %d: %q", len(line), hubTokenHelpWidth, line)
		}
	}
	for _, sub := range projectSettingsCmd.Commands() {
		if !regexp.MustCompile(`\b` + sub.Name() + `\b`).MatchString(lines) {
			t.Errorf("scion project settings %s has no row in the hub token help", sub.Name())
		}
	}
}
