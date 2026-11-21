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

package messaging

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReduceAttachmentName(t *testing.T) {
	safe := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty falls back to attachment",
			input: "",
			want:  "attachment",
		},
		{
			name:  "short name unchanged",
			input: "report-2026_1.txt",
			want:  "report-2026_1.txt",
		},
		{
			name:  "leading dot gets attachment prefix",
			input: ".env",
			want:  "attachment-.env",
		},
		{
			name:  "leading dash gets attachment prefix",
			input: "-flag.txt",
			want:  "attachment--flag.txt",
		},
		{
			name:  "exact limit with extension unchanged",
			input: strings.Repeat("a", 116) + ".txt",
			want:  strings.Repeat("a", 116) + ".txt",
		},
		{
			name:  "one over limit preserves extension",
			input: strings.Repeat("a", 117) + ".txt",
			want:  strings.Repeat("a", 116) + ".txt",
		},
		{
			name:  "truncation does not expose earlier blocked-looking suffix",
			input: strings.Repeat("x", 117) + ".sh_notes.txt",
			want:  strings.Repeat("x", 116) + ".txt",
		},
		{
			name:  "multi-dot name preserves final extension",
			input: strings.Repeat("x", 120) + ".tar.gz",
			want:  strings.Repeat("x", 117) + ".gz",
		},
		{
			name:  "leading dot near limit preserves extension after prefix",
			input: "." + strings.Repeat("a", 115) + ".txt",
			want:  "attachment-." + strings.Repeat("a", 104) + ".txt",
		},
		{
			name:  "leading dash near limit preserves extension after prefix",
			input: "-" + strings.Repeat("a", 115) + ".pdf",
			want:  "attachment--" + strings.Repeat("a", 104) + ".pdf",
		},
		{
			name:  "multibyte utf8 stem preserves extension",
			input: strings.Repeat("é", 120) + ".txt",
			want:  strings.Repeat("_", 116) + ".txt",
		},
		{
			name:  "no extension truncates to limit",
			input: strings.Repeat("x", 150),
			want:  strings.Repeat("x", 120),
		},
		{
			name:  "trailing dot alone is not kept as extension",
			input: strings.Repeat("x", 125) + ".",
			want:  strings.Repeat("x", 120),
		},
		{
			name:  "longest preservable extension keeps 1-rune stem",
			input: strings.Repeat("a", 10) + "." + strings.Repeat("b", 118),
			want:  "a." + strings.Repeat("b", 118),
		},
		{
			name:  "extension at limit falls back to prefix truncation",
			input: "a." + strings.Repeat("b", 119),
			want:  "a." + strings.Repeat("b", 118),
		},
		{
			name:  "extension over limit falls back to prefix truncation",
			input: "a." + strings.Repeat("b", 130),
			want:  "a." + strings.Repeat("b", 118),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReduceAttachmentName(tt.input)
			if got != tt.want {
				t.Errorf("ReduceAttachmentName(%q) = %q (len %d), want %q (len %d)",
					tt.input, got, utf8.RuneCountInString(got), tt.want, utf8.RuneCountInString(tt.want))
			}
			if !safe.MatchString(got) {
				t.Errorf("ReduceAttachmentName(%q) = %q contains unsafe characters", tt.input, got)
			}
			if strings.HasPrefix(got, ".") || strings.HasPrefix(got, "-") {
				t.Errorf("ReduceAttachmentName(%q) = %q starts with '.' or '-'", tt.input, got)
			}
			if utf8.RuneCountInString(got) > MaxAttachmentNameRunes {
				t.Errorf("ReduceAttachmentName(%q) has %d runes > %d", tt.input, utf8.RuneCountInString(got), MaxAttachmentNameRunes)
			}
		})
	}
}
