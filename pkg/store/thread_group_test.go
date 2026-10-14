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

package store

import (
	"errors"
	"strings"
	"testing"
)

func TestThreadGroupNameKey(t *testing.T) {
	cases := map[string]string{
		"Release":          "release",
		"  RELEASE  ":      "release",
		"Re\x00lea\tse":    "release",
		"\u0085 Release\n": "release",
		"Équipe":           "équipe",
		"ÄÖÜ":              "äöü",
		"":                 "",
	}
	for in, want := range cases {
		if got := ThreadGroupNameKey(in); got != want {
			t.Errorf("ThreadGroupNameKey(%q) = %q, want %q", in, got, want)
		}
	}
	if ThreadGroupNameKey("Release") != ThreadGroupNameKey(" release ") {
		t.Error("keys of case and whitespace variants differ")
	}
}

func TestValidateThreadGroupName(t *testing.T) {
	valid := map[string]string{
		"Release":                  "Release",
		"  Release notes  ":        "Release notes",
		"🚀 launch":                 "🚀 launch",
		strings.Repeat("é", 100):   strings.Repeat("é", 100),
		strings.Repeat("x", 100):   strings.Repeat("x", 100),
		"no-regexp: allowed / ok!": "no-regexp: allowed / ok!",
	}
	for in, want := range valid {
		got, err := ValidateThreadGroupName(in)
		if err != nil || got != want {
			t.Errorf("ValidateThreadGroupName(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
	invalid := []string{
		"",
		"   ",
		"a\x00b",
		"a\nb",
		"tab\there",
		"\u0085x",
		strings.Repeat("é", 101),
		strings.Repeat("x", 101),
	}
	for _, in := range invalid {
		if _, err := ValidateThreadGroupName(in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ValidateThreadGroupName(%q) err = %v, want ErrInvalidInput", in, err)
		}
	}
}

func TestSanitizeThreadGroupName(t *testing.T) {
	cases := map[string]string{
		"Release":                       "Release",
		"  Release  ":                   "Release",
		"Re\x00lea\tse\n":               "Release",
		"\u0085\x07":                    "",
		"   ":                           "",
		"":                              "",
		strings.Repeat("é", 101):        strings.Repeat("é", 100),
		strings.Repeat("x", 99) + " yz": strings.Repeat("x", 99),
	}
	for in, want := range cases {
		got := SanitizeThreadGroupName(in)
		if got != want {
			t.Errorf("SanitizeThreadGroupName(%q) = %q, want %q", in, got, want)
			continue
		}
		if got == "" {
			continue
		}
		if v, err := ValidateThreadGroupName(got); err != nil || v != got {
			t.Errorf("ValidateThreadGroupName(SanitizeThreadGroupName(%q)) = %q, %v; want it accepted unchanged", in, v, err)
		}
		if ThreadGroupNameKey(got) != ThreadGroupNameKey(in) && len([]rune(strings.TrimSpace(in))) <= MaxThreadGroupNameRunes {
			t.Errorf("sanitizing %q changed its name key", in)
		}
	}
}
