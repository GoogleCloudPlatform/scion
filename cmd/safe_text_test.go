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
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSafeText: control characters, line separators and bidirectional
// controls become U+FFFD; other text, including non-ASCII, is unchanged.
func TestSafeText(t *testing.T) {
	for in, want := range map[string]string{
		"Report":                        "Report",
		"Bericht über Größe — 日本":       "Bericht über Größe — 日本",
		"a\x1b]52;c;aGk=\x07b":          "a\uFFFD]52;c;aGk=\uFFFDb",
		"row\nfake\trow\r":              "row\uFFFDfake\uFFFDrow\uFFFD",
		"x\u0085y\u009bz\x7f":           "x\uFFFDy\uFFFDz\uFFFD",
		"p\u2028q\u2029r":               "p\uFFFDq\uFFFDr",
		"\u202eevil\u202c\u2066i\u2069": "\uFFFDevil\uFFFD\uFFFDi\uFFFD",
		"a\u200eb\u200fc\u061cd":        "a\uFFFDb\uFFFDc\uFFFDd",
		"zw\u200bs\ufeffp":              "zw\uFFFDs\uFFFDp",
	} {
		assert.Equal(t, want, safeText(in), "%q", in)
	}
	// Scripts, combining marks and emoji pass through unchanged, joiners
	// included.
	for _, s := range []string{
		"e\u0301 n\u0303",                       // combining marks
		"\U0001F600 \U0001F469\u200d\U0001F4BB", // emoji, with a zero-width joiner sequence
		"\u05e9\u05dc\u05d5\u05dd",              // Hebrew letters
		"\u0645\u0631\u062d\u0628\u0627",        // Arabic letters
		"\u0928\u094d\u200c\u0928",              // Devanagari with a zero-width non-joiner
	} {
		assert.Equal(t, s, safeText(s), "%q", s)
	}
}
