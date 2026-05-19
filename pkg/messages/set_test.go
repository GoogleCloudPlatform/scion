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

package messages

import (
	"strings"
	"testing"
)

func TestIsSetRecipient(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"set[agent:a,agent:b]", true},
		{"set[]", true},
		{"set[a]", true},
		{"agent:foo", false},
		{"user:bar", false},
		{"set[incomplete", false},
		{"incomplete]", false},
		{"", false},
	}
	for _, tt := range tests {
		got := IsSetRecipient(tt.input)
		if got != tt.want {
			t.Errorf("IsSetRecipient(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseSetRecipient_Valid(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []SetRecipient
	}{
		{
			name:  "two agents",
			input: "set[agent:reviewer,agent:deploy-bot]",
			want: []SetRecipient{
				{Kind: RecipientAgent, Name: "reviewer"},
				{Kind: RecipientAgent, Name: "deploy-bot"},
			},
		},
		{
			name:  "mixed agent and user",
			input: "set[agent:reviewer,user:alice@example.com]",
			want: []SetRecipient{
				{Kind: RecipientAgent, Name: "reviewer"},
				{Kind: RecipientUser, Name: "alice@example.com"},
			},
		},
		{
			name:  "bare names default to agent",
			input: "set[reviewer,deploy-bot]",
			want: []SetRecipient{
				{Kind: RecipientAgent, Name: "reviewer"},
				{Kind: RecipientAgent, Name: "deploy-bot"},
			},
		},
		{
			name:  "bare email defaults to user",
			input: "set[agent:bot,alice@example.com]",
			want: []SetRecipient{
				{Kind: RecipientAgent, Name: "bot"},
				{Kind: RecipientUser, Name: "alice@example.com"},
			},
		},
		{
			name:  "user prefix without email",
			input: "set[user:alice,agent:bot]",
			want: []SetRecipient{
				{Kind: RecipientUser, Name: "alice"},
				{Kind: RecipientAgent, Name: "bot"},
			},
		},
		{
			name:  "whitespace trimmed",
			input: "set[ agent:a , agent:b , user:c ]",
			want: []SetRecipient{
				{Kind: RecipientAgent, Name: "a"},
				{Kind: RecipientAgent, Name: "b"},
				{Kind: RecipientUser, Name: "c"},
			},
		},
		{
			name:  "deduplication",
			input: "set[agent:a,agent:b,agent:a]",
			want: []SetRecipient{
				{Kind: RecipientAgent, Name: "a"},
				{Kind: RecipientAgent, Name: "b"},
			},
		},
		{
			name:  "three recipients all types",
			input: "set[agent:reviewer,user:alice@example.com,deploy-bot]",
			want: []SetRecipient{
				{Kind: RecipientAgent, Name: "reviewer"},
				{Kind: RecipientUser, Name: "alice@example.com"},
				{Kind: RecipientAgent, Name: "deploy-bot"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSetRecipient(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d recipients, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i].Kind != tt.want[i].Kind || got[i].Name != tt.want[i].Name {
					t.Errorf("recipient[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseSetRecipient_Errors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "not a set",
			input:   "agent:foo",
			wantErr: "not a set recipient",
		},
		{
			name:    "empty set",
			input:   "set[]",
			wantErr: "empty set[]",
		},
		{
			name:    "single element",
			input:   "set[agent:a]",
			wantErr: "at least 2 recipients",
		},
		{
			name:    "nested set",
			input:   "set[agent:a,set[agent:b,agent:c]]",
			wantErr: "nested set[]",
		},
		{
			name:    "unknown prefix",
			input:   "set[foo:bar,agent:a]",
			wantErr: "unknown recipient prefix",
		},
		{
			name:    "empty agent name",
			input:   "set[agent:,agent:b]",
			wantErr: "empty agent name",
		},
		{
			name:    "empty user name",
			input:   "set[user:,agent:b]",
			wantErr: "empty user name",
		},
		{
			name:    "whitespace only",
			input:   "set[  ]",
			wantErr: "empty set[]",
		},
		{
			name:    "all duplicates collapse to single",
			input:   "set[agent:a,agent:a]",
			wantErr: "at least 2 recipients",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSetRecipient(tt.input)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestParseSetRecipient_MaxLimit(t *testing.T) {
	parts := make([]string, MaxSetRecipients+1)
	for i := range parts {
		parts[i] = "agent:a" + strings.Repeat("x", 3) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	input := "set[" + strings.Join(parts, ",") + "]"
	_, err := ParseSetRecipient(input)
	if err == nil {
		t.Fatal("expected error for exceeding max recipients")
	}
	if !strings.Contains(err.Error(), "maximum is") {
		t.Errorf("error %q does not mention maximum", err.Error())
	}
}

func TestFormatSetRecipients(t *testing.T) {
	tests := []struct {
		name       string
		sender     string
		recipients []string
		want       string
	}{
		{
			name:       "user sender with two agents",
			sender:     "user:alice",
			recipients: []string{"agent:coder", "agent:reviewer"},
			want:       "set[user:alice,agent:coder,agent:reviewer]",
		},
		{
			name:       "agent sender with agents",
			sender:     "agent:lead",
			recipients: []string{"agent:coder", "agent:reviewer"},
			want:       "set[agent:lead,agent:coder,agent:reviewer]",
		},
		{
			name:       "mixed recipients",
			sender:     "user:bob@example.com",
			recipients: []string{"agent:deploy", "user:carol@example.com"},
			want:       "set[user:bob@example.com,agent:deploy,user:carol@example.com]",
		},
		{
			name:       "single recipient",
			sender:     "user:alice",
			recipients: []string{"agent:coder"},
			want:       "set[user:alice,agent:coder]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatSetRecipients(tt.sender, tt.recipients)
			if got != tt.want {
				t.Errorf("FormatSetRecipients() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatSetRecipients_Roundtrip(t *testing.T) {
	sender := "user:alice"
	recipients := []string{"agent:coder", "agent:reviewer"}
	formatted := FormatSetRecipients(sender, recipients)

	parsed, err := ParseSetRecipient(formatted)
	if err != nil {
		t.Fatalf("roundtrip parse failed: %v", err)
	}
	if len(parsed) != 3 {
		t.Fatalf("expected 3 parsed recipients (sender + 2), got %d", len(parsed))
	}
	if parsed[0].String() != "user:alice" {
		t.Errorf("parsed[0] = %q, want %q", parsed[0].String(), "user:alice")
	}
	if parsed[1].String() != "agent:coder" {
		t.Errorf("parsed[1] = %q, want %q", parsed[1].String(), "agent:coder")
	}
	if parsed[2].String() != "agent:reviewer" {
		t.Errorf("parsed[2] = %q, want %q", parsed[2].String(), "agent:reviewer")
	}
}

func TestSetRecipientString(t *testing.T) {
	r := SetRecipient{Kind: RecipientAgent, Name: "reviewer"}
	if r.String() != "agent:reviewer" {
		t.Errorf("String() = %q, want %q", r.String(), "agent:reviewer")
	}
	r = SetRecipient{Kind: RecipientUser, Name: "alice"}
	if r.String() != "user:alice" {
		t.Errorf("String() = %q, want %q", r.String(), "user:alice")
	}
}
