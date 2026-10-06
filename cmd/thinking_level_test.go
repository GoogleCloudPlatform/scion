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
	"strings"
	"testing"
)

func TestParseThinkingLevel(t *testing.T) {
	valid := map[string]int{
		"0": 0, "1": 1, "50": 50, "100": 100, " 42 ": 42,
		"low": 25, "medium": 50, "high": 75, "max": 100,
		"LOW": 25, "Medium": 50, "HiGh": 75, "MAX": 100, " high ": 75,
	}
	for in, want := range valid {
		got, err := parseThinkingLevel(in)
		if err != nil {
			t.Errorf("parseThinkingLevel(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseThinkingLevel(%q) = %d, want %d", in, got, want)
		}
	}

	for _, in := range []string{"-1", "101", "1000", "xhigh", "minimal", "1.5", "abc", "", "high!"} {
		_, err := parseThinkingLevel(in)
		if err == nil {
			t.Errorf("parseThinkingLevel(%q): expected an error", in)
			continue
		}
		if !strings.Contains(err.Error(), "--thinking-level") {
			t.Errorf("parseThinkingLevel(%q): error %q does not name the flag", in, err)
		}
	}
}

// TestStartThinkingLevelFlagAcceptsShorthands checks that --thinking-level is
// a string flag (so names parse at the cobra layer), defaults to unset, and
// documents the shorthand mapping.
func TestStartThinkingLevelFlagAcceptsShorthands(t *testing.T) {
	f := startCmd.Flags().Lookup("thinking-level")
	if f == nil {
		t.Fatal("start has no --thinking-level flag")
	}
	if f.Value.Type() != "string" {
		t.Fatalf("--thinking-level type = %q, want string", f.Value.Type())
	}
	if f.DefValue != "" {
		t.Errorf("--thinking-level default = %q, want unset", f.DefValue)
	}
	for _, want := range []string{"0-100", "low (25)", "medium (50)", "high (75)", "max (100)"} {
		if !strings.Contains(f.Usage, want) {
			t.Errorf("--thinking-level help %q is missing %q", f.Usage, want)
		}
	}

	fs := startCmd.Flags()
	saved := thinkingLevelFlag
	t.Cleanup(func() {
		thinkingLevelFlag = saved
		f.Changed = false
	})
	if err := fs.Set("thinking-level", "High"); err != nil {
		t.Fatalf("setting --thinking-level High: %v", err)
	}
	got, err := parseThinkingLevel(thinkingLevelFlag)
	if err != nil || got != 75 {
		t.Errorf("--thinking-level High parsed to %d, %v; want 75", got, err)
	}
}

// TestRunAgentRejectsExplicitEmptyThinkingLevel checks that an explicit
// --thinking-level "" fails validation instead of being treated as unset.
func TestRunAgentRejectsExplicitEmptyThinkingLevel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Chdir(tmp)

	f := startCmd.Flags().Lookup("thinking-level")
	saved := thinkingLevelFlag
	t.Cleanup(func() {
		thinkingLevelFlag = saved
		f.Changed = false
	})
	if err := startCmd.Flags().Set("thinking-level", ""); err != nil {
		t.Fatalf("setting --thinking-level: %v", err)
	}

	err := RunAgent(startCmd, []string{"agent-x"}, false)
	if err == nil || !strings.Contains(err.Error(), "--thinking-level") {
		t.Fatalf("RunAgent with --thinking-level \"\": got %v, want a --thinking-level error", err)
	}
}
