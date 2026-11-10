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
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
)

// stdoutIsTerminal reports whether standard output is a terminal. Tests
// replace it.
var stdoutIsTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// escapeForTerminal returns s with every byte or character that a terminal
// could interpret, rather than print, written as a visible escape: C0
// controls other than newline and tab (ESC and CR included) and DEL as
// \xNN, C1 controls and Unicode format characters (bidirectional
// overrides included) as \u{NNNN}, invalid UTF-8 bytes as \xNN, and a
// backslash as \\ so the escapes cannot be forged. Printable text is
// kept, so the output shows exactly what is stored. Newlines and tabs are
// kept only when keepLines is set (a multi-line value); in a one-line
// field they are escaped too, so a value cannot add or shift lines.
func escapeForTerminal(s string, keepLines bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\\':
			b.WriteString(`\\`)
		case keepLines && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case (r >= 0x80 && r <= 0x9f) || unicode.Is(unicode.Cf, r):
			fmt.Fprintf(&b, `\u{%04x}`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// terminalText returns the one-line field s for text output: escaped by
// escapeForTerminal when standard output is a terminal, unchanged
// otherwise. Use it for
// values another principal chose (an agent-written key, target, agent
// slug or project name) printed in text form; JSON output is encoded by
// the JSON encoder instead.
func terminalText(s string) string {
	if stdoutIsTerminal() {
		return escapeForTerminal(s, false)
	}
	return s
}
