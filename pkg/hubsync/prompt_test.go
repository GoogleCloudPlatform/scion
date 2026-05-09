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

func TestConfirmAction_NoAutoConfirm_DefaultYes(t *testing.T) {
	// When not auto-confirming and stdin returns EOF/error, it falls back to defaultYes.
	// With defaultYes=true, should return true.
	result := ConfirmAction("Test prompt", true, false)
	if !result {
		t.Error("ConfirmAction with defaultYes=true should return true on stdin EOF")
	}
}

func TestConfirmAction_NoAutoConfirm_DefaultNo(t *testing.T) {
	// When not auto-confirming and stdin returns EOF/error, it falls back to defaultYes.
	// With defaultYes=false, should return false.
	result := ConfirmAction("Test prompt", false, false)
	if result {
		t.Error("ConfirmAction with defaultYes=false should return false on stdin EOF")
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

	choice, selectedID := ShowMatchingProjectsPrompt("widgets", matches, "widgets-3", true)
	if choice != ProjectChoiceLink {
		t.Errorf("expected ProjectChoiceLink, got %v", choice)
	}
	if selectedID != "id-1" {
		t.Errorf("expected selected ID 'id-1', got %q", selectedID)
	}
}
