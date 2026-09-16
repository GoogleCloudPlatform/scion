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

func TestReplaceMention(t *testing.T) {
	tests := []struct {
		name    string
		content string
		old     string
		new     string
		want    string
	}{
		{
			name:    "simple replacement",
			content: "Hey @john-smith please review",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "Hey @john@example.com please review",
		},
		{
			name:    "at start of string",
			content: "@john-smith please review",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "@john@example.com please review",
		},
		{
			name:    "at end of string",
			content: "Hey @john-smith",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "Hey @john@example.com",
		},
		{
			name:    "followed by punctuation",
			content: "Hey @john-smith, how are you?",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "Hey @john@example.com, how are you?",
		},
		{
			name:    "no partial match on longer slug",
			content: "Hey @john-smith-jones please review",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "Hey @john-smith-jones please review",
		},
		{
			name:    "case insensitive match",
			content: "Hey @John-Smith please review",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "Hey @john@example.com please review",
		},
		{
			name:    "multiple occurrences",
			content: "@john-smith and @john-smith again",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "@john@example.com and @john@example.com again",
		},
		{
			name:    "no match",
			content: "Hey @alice-bob please review",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "Hey @alice-bob please review",
		},
		{
			name:    "email to slug (inbound)",
			content: "Hey @john@example.com your PR is ready",
			old:     "john@example.com",
			new:     "john-smith",
			want:    "Hey @john-smith your PR is ready",
		},
		{
			name:    "email at end of string (inbound)",
			content: "PR ready @john@example.com",
			old:     "john@example.com",
			new:     "john-smith",
			want:    "PR ready @john-smith",
		},
		{
			name:    "no partial match on email prefix",
			content: "Hey @john please review",
			old:     "john@example.com",
			new:     "john-smith",
			want:    "Hey @john please review",
		},
		{
			name:    "followed by newline",
			content: "Hey @john-smith\nplease review",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "Hey @john@example.com\nplease review",
		},
		{
			name:    "only mention",
			content: "@john-smith",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "@john@example.com",
		},
		{
			name:    "empty content",
			content: "",
			old:     "john-smith",
			new:     "john@example.com",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceMention(tt.content, tt.old, tt.new)
			if got != tt.want {
				t.Errorf("replaceMention(%q, %q, %q) = %q, want %q",
					tt.content, tt.old, tt.new, got, tt.want)
			}
		})
	}
}

func TestTranslateMentionsOutbound(t *testing.T) {
	members := []chatMemberEntry{
		{Kind: "user", DisplayName: "John Smith", Email: "john@example.com"},
		{Kind: "user", DisplayName: "Alice Bob", Email: "alice@example.com"},
		{Kind: "agent", DisplayName: "Code Reviewer", Slug: "code-reviewer"},
	}

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "single human mention",
			content: "Hey @john-smith please review",
			want:    "Hey @john@example.com please review",
		},
		{
			name:    "multiple human mentions",
			content: "@john-smith and @alice-bob please review",
			want:    "@john@example.com and @alice@example.com please review",
		},
		{
			name:    "agent mention not translated",
			content: "Hey @code-reviewer please review",
			want:    "Hey @code-reviewer please review",
		},
		{
			name:    "mixed mentions",
			content: "@john-smith asked @code-reviewer to review",
			want:    "@john@example.com asked @code-reviewer to review",
		},
		{
			name:    "no mentions",
			content: "Just a regular message",
			want:    "Just a regular message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateMentionsOutbound(tt.content, members)
			if got != tt.want {
				t.Errorf("translateMentionsOutbound(%q) = %q, want %q",
					tt.content, got, tt.want)
			}
		})
	}
}

func TestTranslateMentionsInbound(t *testing.T) {
	members := []chatMemberEntry{
		{Kind: "user", DisplayName: "John Smith", Email: "john@example.com"},
		{Kind: "user", DisplayName: "Alice Bob", Email: "alice@example.com"},
	}

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "single email mention",
			content: "Hey @john@example.com your PR is ready",
			want:    "Hey @john-smith your PR is ready",
		},
		{
			name:    "multiple email mentions",
			content: "@john@example.com and @alice@example.com check this",
			want:    "@john-smith and @alice-bob check this",
		},
		{
			name:    "unknown email not translated",
			content: "Hey @unknown@example.com check this",
			want:    "Hey @unknown@example.com check this",
		},
		{
			name:    "no mentions",
			content: "Just a regular message",
			want:    "Just a regular message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateMentionsInbound(tt.content, members)
			if got != tt.want {
				t.Errorf("translateMentionsInbound(%q) = %q, want %q",
					tt.content, got, tt.want)
			}
		})
	}
}

func TestTranslateMentionsSkipsMissingFields(t *testing.T) {
	members := []chatMemberEntry{
		{Kind: "user", DisplayName: "John Smith", Email: ""},        // no email
		{Kind: "user", DisplayName: "", Email: "alice@example.com"}, // no display name
		{Kind: "user", DisplayName: "Valid User", Email: "valid@example.com"},
	}

	content := "@john-smith and @alice and @valid-user"
	got := translateMentionsOutbound(content, members)
	want := "@john-smith and @alice and @valid@example.com"
	if got != want {
		t.Errorf("translateMentionsOutbound with missing fields = %q, want %q", got, want)
	}
}
