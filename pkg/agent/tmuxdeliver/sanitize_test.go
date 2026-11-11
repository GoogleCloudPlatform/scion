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

package tmuxdeliver_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
)

// TestSanitize pins each removed class and what survives (design v2.22 §5.3).
func TestSanitize(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// Unchanged.
		{"plain ascii", "hello, world!", "hello, world!"},
		{"newline and tab kept", "a\nb\tc\n", "a\nb\tc\n"},
		{"multi-byte utf-8", "héllo 日本 🙂", "héllo 日本 🙂"},
		// U+0100 is C4 80 and U+2026 is E2 80 A6: their continuation bytes
		// fall in 0x80-0x9F but they are printable code points, so they stay.
		{"continuation bytes in 0x80-0x9f", "Ā…ā", "Ā…ā"},
		{"empty", "", ""},
		// C0.
		{"carriage return removed", "a\r\nb", "a\nb"},
		{"bell removed", "a\x07b", "ab"},
		{"nul removed", "a\x00b", "ab"},
		{"bare esc removed", "a\x1bb", "ab"},
		// DEL.
		{"del removed", "a\x7fb", "ab"},
		// Paste markers: ESC is removed, so the rest is inert text.
		{"paste start marker", "x\x1b[200~y", "x[200~y"},
		{"paste end marker", "x\x1b[201~rm -rf /\r", "x[201~rm -rf /"},
		// A marker split by a removed byte cannot be re-formed: ESC is
		// removed too, so joining the halves leaves no ESC.
		{"split paste end marker", "x\x1b[20\x7f1~y", "x[201~y"},
		{"split by esc itself", "x\x1b\x1b[201~y", "x[201~y"},
		// C1, as UTF-8 code points.
		{"8-bit csi code point", "a\u009b201~b", "a201~b"},
		{"c1 nel", "a\u0085b", "ab"},
		{"c1 range ends", "a\u0080b\u009fc", "abc"},
		// Invalid UTF-8 becomes U+FFFD, never a raw C1 byte.
		{"raw 8-bit csi byte", "a\x9b201~b", "a�201~b"},
		{"raw invalid byte", "a\xffb", "a�b"},
		{"truncated sequence", "a\xe2\x80", "a��"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tmuxdeliver.Sanitize(tc.in); got != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitize_EveryC0AndC1 removes every C0 control except \n and \t, DEL,
// and every C1 code point.
func TestSanitize_EveryC0AndC1(t *testing.T) {
	for r := rune(0); r <= 0x9f; r++ {
		if r >= 0x20 && r < 0x7f {
			continue
		}
		in := "a" + string(r) + "b"
		want := "ab"
		if r == '\n' || r == '\t' {
			want = in
		}
		if got := tmuxdeliver.Sanitize(in); got != want {
			t.Errorf("Sanitize(U+%04X) = %q, want %q", r, got, want)
		}
	}
}

// TestSanitize_BidiKept pins the choice to keep Unicode bidi and isolate
// controls: they are not terminal control sequences.
func TestSanitize_BidiKept(t *testing.T) {
	for _, r := range []rune{0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069} {
		in := "a" + string(r) + "b"
		if got := tmuxdeliver.Sanitize(in); got != in {
			t.Errorf("Sanitize(U+%04X) = %q, want it kept", r, got)
		}
	}
}

// TestSanitize_NoControlSurvives checks, over every one- and two-byte input,
// that the output is valid UTF-8 with no ESC, no C1 and no C0 other than \n
// and \t.
func TestSanitize_NoControlSurvives(t *testing.T) {
	check := func(in string) {
		out := tmuxdeliver.Sanitize(in)
		if !utf8.ValidString(out) {
			t.Fatalf("Sanitize(%q) = %q: invalid UTF-8", in, out)
		}
		for _, r := range out {
			if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				t.Fatalf("Sanitize(%q) = %q: U+%04X survived", in, out, r)
			}
		}
		if strings.Contains(out, "\x1b") {
			t.Fatalf("Sanitize(%q) = %q: ESC survived", in, out)
		}
	}
	for a := 0; a < 256; a++ {
		check(string([]byte{byte(a)}))
		for b := 0; b < 256; b++ {
			check(string([]byte{byte(a), byte(b)}))
		}
	}
}
