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

import "testing"

func TestTruncatePreview(t *testing.T) {
	cases := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{"negative maxLen returns empty string", "hello", -1, ""},
		{"shorter than maxLen is unchanged", "hi", 10, "hi"},
		{"equal to maxLen is unchanged", "hello", 5, "hello"},
		{"longer than maxLen is truncated with ellipsis", "hello world", 5, "hello..."},
		{"zero maxLen on non-empty string truncates to ellipsis only", "hi", 0, "..."},
		{"zero maxLen on empty string is unchanged", "", 0, ""},
		{"multi-byte CJK runes are truncated by rune count, not bytes", "你好世界", 2, "你好..."},
		{"multi-byte emoji runes are truncated by rune count, not bytes", "😀😃😄😁😆", 3, "😀😃😄..."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncatePreview(tc.s, tc.maxLen); got != tc.want {
				t.Errorf("truncatePreview(%q, %d) = %q, want %q", tc.s, tc.maxLen, got, tc.want)
			}
		})
	}
}
