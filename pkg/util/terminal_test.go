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

package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestColorEnabledDecision(t *testing.T) {
	cases := []struct {
		name    string
		isTTY   bool
		noColor string
		want    bool
	}{
		{"terminal, NO_COLOR unset", true, "", true},
		{"terminal, NO_COLOR set", true, "1", false},
		{"pipe, NO_COLOR unset", false, "", false},
		{"pipe, NO_COLOR set", false, "1", false},
	}
	for _, tc := range cases {
		if got := colorEnabled(tc.isTTY, tc.noColor); got != tc.want {
			t.Errorf("%s: colorEnabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestColorForStripsWhenNotTerminal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	in := Bold + Yellow + "warning" + Reset
	if got := ColorFor(f, in); got != "warning" {
		t.Errorf("ColorFor(non-terminal) = %q, want %q", got, "warning")
	}
}

func TestColorForHonoursNoColorOnTerminal(t *testing.T) {
	orig := isTerminalFile
	isTerminalFile = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminalFile = orig })

	in := BgRed + White + "Error: boom" + Reset

	t.Setenv("NO_COLOR", "")
	if got := ColorFor(os.Stderr, in); got != in {
		t.Errorf("ColorFor(terminal) = %q, want colour kept", got)
	}

	t.Setenv("NO_COLOR", "1")
	if got := ColorFor(os.Stderr, in); got != "Error: boom" {
		t.Errorf("ColorFor(terminal, NO_COLOR=1) = %q, want %q", got, "Error: boom")
	}
}

func TestStripANSIBanner(t *testing.T) {
	if got := StripANSI(GetBanner()); ansiEscape.MatchString(got) {
		t.Errorf("StripANSI left escape codes in the banner: %q", got)
	}
}
