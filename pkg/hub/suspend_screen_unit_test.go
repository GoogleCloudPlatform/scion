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

package hub

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeScreen(t *testing.T) {
	t.Run("empty and whitespace-only return empty", func(t *testing.T) {
		for _, in := range []string{"", "\n\n\n", "   \t\r\n  \n"} {
			got, trunc := normalizeScreen(in)
			assert.Empty(t, got)
			assert.False(t, trunc)
		}
	})

	t.Run("preserves ANSI and trims trailing blank lines", func(t *testing.T) {
		raw := "\x1b[32mline 1\x1b[0m\nline 2\n   \n\n\t\n"
		got, trunc := normalizeScreen(raw)
		assert.Equal(t, "\x1b[32mline 1\x1b[0m\nline 2\n", got)
		assert.False(t, trunc)
	})

	t.Run("coerces invalid UTF-8 and strips NULs", func(t *testing.T) {
		raw := "hi\x00there \xff\xfe end\n"
		got, trunc := normalizeScreen(raw)
		assert.True(t, utf8.ValidString(got))
		assert.NotContains(t, got, "\x00")
		assert.Equal(t, "hithere \uFFFD end\n", got)
		assert.False(t, trunc)
	})

	t.Run("truncates oversized multiline output at line start keeping tail", func(t *testing.T) {
		line := strings.Repeat("a", 99) + "\n" // 100 bytes per line
		// 700 lines = 70,000 bytes > 64 KiB (65,536)
		var b strings.Builder
		for i := 0; i < 700; i++ {
			if i == 699 {
				b.WriteString("TAIL-MARKER\n")
			} else {
				b.WriteString(line)
			}
		}
		got, trunc := normalizeScreen(b.String())
		assert.True(t, trunc)
		assert.LessOrEqual(t, len(got), suspendScreenMaxBytes)
		assert.True(t, strings.HasPrefix(got, strings.Repeat("a", 99)+"\n"), "cut must land on a line start")
		assert.True(t, strings.HasSuffix(got, "TAIL-MARKER\n"))
		assert.True(t, utf8.ValidString(got))
	})

	t.Run("truncates single huge line on UTF-8 rune boundary", func(t *testing.T) {
		// Repeat a 3-byte UTF-8 rune so the raw slice cut lands mid-rune.
		raw := strings.Repeat("€", suspendScreenMaxBytes) // 3 * 64 KiB bytes, no newlines
		got, trunc := normalizeScreen(raw)
		assert.True(t, trunc)
		assert.LessOrEqual(t, len(got), suspendScreenMaxBytes)
		require.NotEmpty(t, got)
		assert.True(t, utf8.ValidString(got))
	})
}
