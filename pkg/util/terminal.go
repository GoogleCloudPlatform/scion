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
	"regexp"

	"golang.org/x/term"
)

// IsTerminal returns true if the current process is running in an interactive terminal.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// ansiEscape matches ANSI CSI escape sequences (colour, bold, reset, ...).
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// isTerminalFile reports whether f is a terminal. It is a variable so tests
// can simulate a terminal.
var isTerminalFile = func(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// colorEnabled is the pure decision behind ColorEnabled: colour only on a
// terminal, and never when NO_COLOR is set to a non-empty value
// (https://no-color.org).
func colorEnabled(isTTY bool, noColor string) bool {
	return isTTY && noColor == ""
}

// ColorEnabled reports whether ANSI colour codes may be written to f: f must
// be a terminal and NO_COLOR must be unset or empty.
func ColorEnabled(f *os.File) bool {
	return colorEnabled(isTerminalFile(f), os.Getenv("NO_COLOR"))
}

// StripANSI removes ANSI escape sequences from s.
func StripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// ColorFor returns s unchanged when colour is enabled for f (see
// ColorEnabled), and s with every ANSI escape sequence removed otherwise.
// Wrap any text built with the colour constants in this package before
// writing it to f, so piped output and NO_COLOR stay free of escape codes.
func ColorFor(f *os.File, s string) string {
	if ColorEnabled(f) {
		return s
	}
	return StripANSI(s)
}
