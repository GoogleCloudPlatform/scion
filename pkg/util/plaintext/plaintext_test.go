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

package plaintext

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// unsafeOracle and breakOracle are written out here, not taken from the
// package, so a change to IsUnsafe or isBreak cannot change what the tests
// expect.
func unsafeOracle(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

func breakOracle(r rune) bool {
	return unicode.In(r, unicode.White_Space, unicode.Zl, unicode.Zp)
}

func TestClean(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		max            int
	}{
		{"plain", "Looks good.", "Looks good.", 240},
		{"trim and collapse", "  a \t\n\r b  \u2028\u2029 c  ", "a b c", 240},
		{"ESC sequence", "\x1b[31mred\x1b[0m", "[31mred[0m", 240},
		{"RTL override", "a\u202eb\u202cc", "abc", 240},
		{"isolates and zero width", "\u2066a\u2069\u200bb\ufeffc\u200d", "abc", 240},
		{"NUL BEL DEL NEL C1", "a\x00b\x07c\x7fd\u0085e\u009bf", "abcd ef", 240},
		{"line and paragraph separators", "a\u2028b\u2029c", "a b c", 240},
		{"invalid UTF-8", "a\xffb\xc3", "ab", 240},
		{"markdown and HTML kept as text", "**b** <i>x</i>", "**b** <i>x</i>", 240},
		{"mentions kept as text", "<!channel> @here <@U1>", "<!channel> @here <@U1>", 240},
		{"only unsafe runes", "\x1b\u202e\u200b\x00", "", 240},
		{"only whitespace", " \n\t\u2028 ", "", 240},
		{"at cap", "abcde", "abcde", 5},
		{"over cap", "abcdef", "abcd…", 5},
		{"over cap trims space before ellipsis", "abc defgh", "abc…", 5},
		{"cap counts runes not bytes", "ééééééé", "éééé…", 5},
		{"cap of one", "ab", "…", 1},
		{"zero cap", "abc", "", 0},
		{"negative cap", "abc", "", -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Clean(c.in, c.max); got != c.want {
				t.Errorf("Clean(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
			}
		})
	}
}

// TestClean_StripsEveryUnsafeRune is the kill test for the strip: for every
// code point that is Cc, Cf, Zl or Zp, or Unicode white space (oracles
// written out above), a text holding it between two letters comes out as
// the two letters, joined by one space for white space and the separators.
// A Clean that dropped the strip, the break case or any one category, or
// kept a rune from any of them, fails here.
func TestClean_StripsEveryUnsafeRune(t *testing.T) {
	checked, breaks, cc, cf := 0, 0, 0, 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // surrogates are not encodable
		}
		isBreak := breakOracle(r)
		if !isBreak && !unsafeOracle(r) {
			continue
		}
		checked++
		if unicode.Is(unicode.Cc, r) {
			cc++
		}
		if unicode.Is(unicode.Cf, r) {
			cf++
		}
		want := "ab"
		if isBreak {
			breaks++
			want = "a b"
		}
		if got := Clean("a"+string(r)+"b", 240); got != want {
			t.Errorf("%U: got %q, want %q", r, got, want)
		}
	}
	// Per category, so an oracle that matched too little cannot hide behind
	// the others: Cc is exactly 65 code points, Cf more than 150 in every
	// Unicode version Go has shipped, White_Space 25 (Zl and Zp included).
	if cc != 65 || cf < 150 || breaks < 25 || checked < cc+cf {
		t.Fatalf("checked %d code points (Cc %d, Cf %d, breaks %d); an oracle matched too little", checked, cc, cf, breaks)
	}
}

// TestClean_NamedRunes pins the runes the review conditions name, so a
// failure reads plainly: ESC, the RTL override, the separators, NEL, the
// zero-width space and the BOM.
func TestClean_NamedRunes(t *testing.T) {
	for r, want := range map[rune]string{
		'\x1b': "ab", '\u202e': "ab", '\u202d': "ab", '\u2066': "ab", '\u200b': "ab", '\ufeff': "ab",
		'\u2028': "a b", '\u2029': "a b", '\u0085': "a b", '\n': "a b", '\r': "a b",
	} {
		if got := Clean("a"+string(r)+"b", 240); got != want {
			t.Errorf("%U: got %q, want %q", r, got, want)
		}
	}
}

// TestClean_IsUnsafeMatchesOracle pins the exported set to Cc, Cf, Zl, Zp.
func TestClean_IsUnsafeMatchesOracle(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if IsUnsafe(r) != unsafeOracle(r) {
			t.Fatalf("IsUnsafe(%U) = %v", r, IsUnsafe(r))
		}
	}
}

// TestClean_OutputProperties checks, over a mixed hostile text cut at many
// caps, that the output is valid UTF-8, within the cap, single-spaced,
// trimmed and free of unsafe runes.
func TestClean_OutputProperties(t *testing.T) {
	in := strings.Repeat("\x1b[2Jé \u202e<b>x</b>\n\n@here <!channel>\u2028\xff zw\u200bsp ", 40)
	for max := 1; max <= 300; max++ {
		got := Clean(in, max)
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > max {
			t.Fatalf("max %d: %q", max, got)
		}
		if strings.Contains(got, "  ") || strings.TrimSpace(got) != got {
			t.Fatalf("max %d: spacing %q", max, got)
		}
		for _, r := range got {
			if r != ' ' && (unsafeOracle(r) || breakOracle(r)) {
				t.Fatalf("max %d: %U left in %q", max, r, got)
			}
		}
	}
}
