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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// webDevClaudeSettingsPath is the claude settings file of the repository's
// web-dev template harness-config, relative to this package directory.
var webDevClaudeSettingsPath = filepath.Join("..", ".scion", "templates", "web-dev",
	"harness-configs", "claude-web", "home", ".claude", "settings.json")

func loadClaudeHooks(t *testing.T, name string, data []byte) map[string][]claudeHookGroup {
	t.Helper()
	var settings struct {
		Hooks map[string][]claudeHookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return settings.Hooks
}

func runsScionClaudeHook(groups []claudeHookGroup) bool {
	for _, g := range groups {
		for _, h := range g.Hooks {
			if strings.Contains(h.Command, "sciontool hook --dialect=claude") {
				return true
			}
		}
	}
	return false
}

// TestClaudeTemplateSettings_WebDevRegistersHarnessHookEvents pins that the
// web-dev template's claude settings register every hook event that the
// bundled claude harness settings register. A template-bundled
// harness-config takes precedence over the bundled one, so web-dev agents get
// this file instead, and a missing event (for example PostToolUseFailure)
// never reaches sciontool for them.
func TestClaudeTemplateSettings_WebDevRegistersHarnessHookEvents(t *testing.T) {
	harnessData, err := fs.ReadFile(FS, "claude/home/.claude/settings.json")
	if err != nil {
		t.Fatalf("read embedded claude settings: %v", err)
	}
	harnessHooks := loadClaudeHooks(t, "embedded claude settings", harnessData)

	templateData, err := os.ReadFile(webDevClaudeSettingsPath)
	if err != nil {
		t.Fatalf("read web-dev template claude settings: %v", err)
	}
	templateHooks := loadClaudeHooks(t, "web-dev template claude settings", templateData)

	var events []string
	for event, groups := range harnessHooks {
		if runsScionClaudeHook(groups) {
			events = append(events, event)
		}
	}
	sort.Strings(events)
	if len(events) == 0 {
		t.Fatal("embedded claude settings register no sciontool hook events")
	}

	for _, event := range events {
		if !runsScionClaudeHook(templateHooks[event]) {
			t.Errorf("web-dev template claude settings do not run sciontool hook --dialect=claude for %s", event)
		}
	}
}

// TestClaudeTemplateSettings_WebDevNotificationMatcherMatchesHarness pins the
// web-dev template's Notification matcher to the bundled harness one, which
// lists the Claude Code notification types that are waits for the user. A
// matcher Claude Code never emits (such as a tool name) means permission
// prompts from web-dev agents are never reported.
func TestClaudeTemplateSettings_WebDevNotificationMatcherMatchesHarness(t *testing.T) {
	harnessData, err := fs.ReadFile(FS, "claude/home/.claude/settings.json")
	if err != nil {
		t.Fatalf("read embedded claude settings: %v", err)
	}
	templateData, err := os.ReadFile(webDevClaudeSettingsPath)
	if err != nil {
		t.Fatalf("read web-dev template claude settings: %v", err)
	}

	matchers := func(groups []claudeHookGroup) []string {
		var out []string
		for _, g := range groups {
			out = append(out, g.Matcher)
		}
		return out
	}
	want := matchers(loadClaudeHooks(t, "embedded claude settings", harnessData)["Notification"])
	got := matchers(loadClaudeHooks(t, "web-dev template claude settings", templateData)["Notification"])
	if len(want) == 0 {
		t.Fatal("embedded claude settings register no Notification hook")
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("web-dev template Notification matchers = %q, want the harness matchers %q", got, want)
	}
}
