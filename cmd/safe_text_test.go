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
		"Report":                  "Report",
		"Bericht über Größe — 日本": "Bericht über Größe — 日本",
		"a\x1b]52;c;aGk=\x07b":    "a�]52;c;aGk=�b",
		"row\nfake\trow\r":        "row�fake�row�",
		"x\u0085y\u009bz\x7f":     "x�y�z�",
		"p q r":                   "p�q�r",
		"‮evil‬⁦i⁩":               "�evil��i�",
	} {
		assert.Equal(t, want, safeText(in), "%q", in)
	}
}
