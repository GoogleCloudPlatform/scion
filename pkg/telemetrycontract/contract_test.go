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

package telemetrycontract

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidTokenType(t *testing.T) {
	for _, v := range TokenTypes {
		if !ValidTokenType(v) {
			t.Errorf("ValidTokenType(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "bogus", "Input", "cache-read"} {
		if ValidTokenType(v) {
			t.Errorf("ValidTokenType(%q) = true, want false", v)
		}
	}
}

func TestSummableTokenTypesExcludesReasoning(t *testing.T) {
	for _, v := range SummableTokenTypes {
		if v == TokenTypeReasoning {
			t.Fatal("SummableTokenTypes must not include TokenTypeReasoning: it already overlaps TokenTypeOutput")
		}
		if !ValidTokenType(v) {
			t.Errorf("SummableTokenTypes contains invalid token type %q", v)
		}
	}
	if len(SummableTokenTypes) != 4 {
		t.Errorf("len(SummableTokenTypes) = %d, want 4", len(SummableTokenTypes))
	}
}

func TestResolveModelLabelPrecedence(t *testing.T) {
	cases := []struct {
		name, native, fallback, want string
	}{
		{"native wins over fallback", "provider/model-a", "model-b", "provider/model-a"},
		{"fallback when native empty", "", "model-b", "model-b"},
		{"fallback when native is whitespace", "  ", "model-b", "model-b"},
		{"unknown when both empty", "", "", UnknownModel},
		{"unknown when both whitespace", " ", "\t", UnknownModel},
		{"native is trimmed", " model-a\n", "", "model-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveModelLabel(tc.native, tc.fallback); got != tc.want {
				t.Errorf("ResolveModelLabel(%q, %q) = %q, want %q", tc.native, tc.fallback, got, tc.want)
			}
		})
	}
}

func TestResolveModelLabelTruncatesOnRuneBoundary(t *testing.T) {
	ascii := strings.Repeat("m", MaxModelLabelBytes+10)
	if got := ResolveModelLabel(ascii, ""); len(got) != MaxModelLabelBytes {
		t.Errorf("len(ResolveModelLabel(long ascii)) = %d, want %d", len(got), MaxModelLabelBytes)
	}
	// 3-byte runes: 128 is not a multiple of 3, so the cut must walk back.
	wide := strings.Repeat("€", 50)
	got := ResolveModelLabel(wide, "")
	if !utf8.ValidString(got) {
		t.Fatalf("ResolveModelLabel split a rune: %q", got)
	}
	if len(got) != 126 {
		t.Errorf("len(ResolveModelLabel(wide)) = %d, want 126 (42 whole runes)", len(got))
	}
}
