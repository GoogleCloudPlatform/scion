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

package harnesses

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

// claudeNotificationTypes is the set of Notification matcher values that
// Claude Code documents (https://code.claude.com/docs/en/hooks#notification).
var claudeNotificationTypes = map[string]bool{
	"permission_prompt":          true,
	"idle_prompt":                true,
	"auth_success":               true,
	"elicitation_dialog":         true,
	"elicitation_url_dialog":     true,
	"elicitation_complete":       true,
	"elicitation_response":       true,
	"agent_needs_input":          true,
	"agent_completed":            true,
	"quota_auto_resume_fired":    true,
	"quota_auto_resume_stale":    true,
	"quota_auto_resume_disabled": true,
}

// claudeNotificationExcluded lists notification types that must not reach
// the scion status hook. Every Notification becomes a sticky
// waiting_for_input that overwrites the current activity, so:
//   - idle_prompt fires ~60s after every finished turn and would clobber
//     completed/blocked;
//   - auth_success is not a wait for the user.
var claudeNotificationExcluded = []string{"idle_prompt", "auth_success"}

type claudeHookGroup struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

// splitClaudeMatcher splits a matcher into its alternatives. Claude Code
// treats "|" as the alternative separator; "," is accepted too, and
// surrounding whitespace is ignored, so a list like
// "permission_prompt, elicitation_dialog" is still checked per type.
func splitClaudeMatcher(matcher string) []string {
	var alts []string
	for _, alt := range strings.FieldsFunc(matcher, func(r rune) bool { return r == '|' || r == ',' }) {
		if alt = strings.TrimSpace(alt); alt != "" {
			alts = append(alts, alt)
		}
	}
	return alts
}

func TestClaudeSettings_SplitMatcher(t *testing.T) {
	cases := map[string][]string{
		"permission_prompt|elicitation_dialog":    {"permission_prompt", "elicitation_dialog"},
		"permission_prompt, elicitation_dialog":   {"permission_prompt", "elicitation_dialog"},
		" permission_prompt | elicitation_dialog": {"permission_prompt", "elicitation_dialog"},
		"permission_prompt":                       {"permission_prompt"},
	}
	for in, want := range cases {
		got := splitClaudeMatcher(in)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("splitClaudeMatcher(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClaudeSettings_NotificationMatcherUsesClaudeTypes(t *testing.T) {
	data, err := fs.ReadFile(FS, "claude/home/.claude/settings.json")
	if err != nil {
		t.Fatalf("read embedded claude settings: %v", err)
	}
	var settings struct {
		Hooks map[string][]claudeHookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse claude settings: %v", err)
	}

	groups := settings.Hooks["Notification"]
	if len(groups) == 0 {
		t.Fatal("claude settings register no Notification hook")
	}

	sawPermission := false
	for _, g := range groups {
		if g.Matcher == "" || g.Matcher == "*" {
			t.Errorf("Notification matcher %q is a catch-all; it would report non-wait notifications as waiting_for_input", g.Matcher)
			continue
		}
		hasScionHook := false
		for _, h := range g.Hooks {
			if strings.Contains(h.Command, "sciontool hook --dialect=claude") {
				hasScionHook = true
			}
		}
		if !hasScionHook {
			t.Errorf("Notification group %q does not run sciontool hook --dialect=claude", g.Matcher)
		}
		for _, alt := range splitClaudeMatcher(g.Matcher) {
			if !claudeNotificationTypes[alt] {
				t.Errorf("Notification matcher alternative %q is not a documented Claude Code notification type", alt)
			}
			for _, excluded := range claudeNotificationExcluded {
				if alt == excluded {
					t.Errorf("Notification matcher must not include %q", alt)
				}
			}
			if alt == "permission_prompt" {
				sawPermission = true
			}
		}
	}
	if !sawPermission {
		t.Error("Notification matcher must include permission_prompt")
	}
}
