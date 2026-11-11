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

package hubsync

import (
	"strings"
	"testing"
)

func TestConfirmAction_AutoConfirm(t *testing.T) {
	tests := []struct {
		name       string
		defaultYes bool
	}{
		{"defaultYes=true", true},
		{"defaultYes=false", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConfirmAction("Test prompt", tt.defaultYes, true)
			if !result {
				t.Errorf("ConfirmAction with autoConfirm=true should always return true, got false (defaultYes=%v)", tt.defaultYes)
			}
		})
	}
}

func TestConfirmAction_NoTerminal_AnswersNo(t *testing.T) {
	// Without a terminal on stdin, ConfirmAction must not take a Yes
	// default: it answers No whatever the default.
	for _, defaultYes := range []bool{true, false} {
		withPromptIO(t, strings.NewReader("y\n"), false)
		if ConfirmAction("Test prompt", defaultYes, false) {
			t.Errorf("ConfirmAction(defaultYes=%v) without a terminal = true, want false", defaultYes)
		}
	}
}

func TestConfirmAction_Terminal(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		defaultYes bool
		want       bool
	}{
		{"enter takes default yes", "\n", true, true},
		{"enter takes default no", "\n", false, false},
		{"explicit yes", "y\n", false, true},
		{"explicit no", "n\n", true, false},
		{"end of input is no", "", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withPromptIO(t, strings.NewReader(tt.input), true)
			if got := ConfirmAction("Test prompt", tt.defaultYes, false); got != tt.want {
				t.Errorf("ConfirmAction = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNextSlugFromMatches(t *testing.T) {
	tests := []struct {
		name     string
		baseSlug string
		matches  []ProjectMatch
		want     string
	}{
		{
			name:     "no matches",
			baseSlug: "widgets",
			matches:  nil,
			want:     "",
		},
		{
			name:     "one match with base slug",
			baseSlug: "widgets",
			matches: []ProjectMatch{
				{Slug: "widgets"},
			},
			want: "widgets-1",
		},
		{
			name:     "two matches with serial",
			baseSlug: "widgets",
			matches: []ProjectMatch{
				{Slug: "widgets"},
				{Slug: "widgets-1"},
			},
			want: "widgets-2",
		},
		{
			name:     "gap in serial",
			baseSlug: "widgets",
			matches: []ProjectMatch{
				{Slug: "widgets"},
				{Slug: "widgets-3"},
			},
			want: "widgets-4",
		},
		{
			name:     "no base slug match but serial exists",
			baseSlug: "widgets",
			matches: []ProjectMatch{
				{Slug: "widgets-2"},
			},
			want: "widgets-3",
		},
		{
			name:     "unrelated slugs only",
			baseSlug: "widgets",
			matches: []ProjectMatch{
				{Slug: "gadgets"},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NextSlugFromMatches(tt.baseSlug, tt.matches)
			if got != tt.want {
				t.Errorf("NextSlugFromMatches(%q, ...) = %q, want %q", tt.baseSlug, got, tt.want)
			}
		})
	}
}

func TestShowMatchingProjectsPrompt_AutoConfirm(t *testing.T) {
	matches := []ProjectMatch{
		{ID: "id-1", Name: "widgets", Slug: "widgets"},
		{ID: "id-2", Name: "widgets (2)", Slug: "widgets-2"},
	}

	withPromptIO(t, strings.NewReader(""), false)
	choice, selectedID, err := ShowMatchingProjectsPrompt("widgets", matches, "widgets-3", true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if choice != ProjectChoiceLink {
		t.Errorf("expected ProjectChoiceLink, got %v", choice)
	}
	if selectedID != "id-1" {
		t.Errorf("expected selected ID 'id-1', got %q", selectedID)
	}
}

// TestShowProjectLinkOrDisablePrompt_TwoChoices checks that the unlinked
// project prompt offers only link and cancel, and that every input path
// resolves to one of them.
func TestShowProjectLinkOrDisablePrompt_TwoChoices(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input       string
		tty         bool
		autoConfirm bool
		want        LinkOrDisableChoice
	}{
		{name: "choice 1 links", input: "1\n", tty: true, want: LinkOrDisableLink},
		{name: "choice 2 cancels", input: "2\n", tty: true, want: LinkOrDisableCancel},
		{name: "c cancels", input: "c\n", tty: true, want: LinkOrDisableCancel},
		{name: "former disable number 3 is invalid, then cancel", input: "3\n2\n", tty: true, want: LinkOrDisableCancel},
		{name: "auto-confirm links", autoConfirm: true, want: LinkOrDisableLink},
		{name: "no terminal cancels", tty: false, want: LinkOrDisableCancel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := withPromptIO(t, strings.NewReader(tc.input), tc.tty)
			got := ShowProjectLinkOrDisablePrompt("widgets", tc.autoConfirm)
			if got != tc.want {
				t.Errorf("choice = %v, want %v", got, tc.want)
			}
			text := out.String()
			if !strings.Contains(text, "[1] Link and sync project now") || !strings.Contains(text, "[2] Cancel") {
				t.Errorf("prompt does not offer link and cancel:\n%s", text)
			}
			if strings.Contains(text, "[3]") || strings.Contains(strings.ToLower(text), "disable") {
				t.Errorf("prompt still offers a third or disable choice:\n%s", text)
			}
		})
	}
}
