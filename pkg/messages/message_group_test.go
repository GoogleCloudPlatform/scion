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

func TestIsGroupRecipient(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"group[agent:a,agent:b]", true},
		{"group[]", true},
		{"group[a]", true},
		{"set[agent:a,agent:b]", true},
		{"set[]", true},
		{"set[a]", true},
		{"agent:foo", false},
		{"user:bar", false},
		{"set[incomplete", false},
		{"group[incomplete", false},
		{"incomplete]", false},
		{"", false},
	}
	for _, tt := range tests {
		got := IsGroupRecipient(tt.input)
		if got != tt.want {
			t.Errorf("IsGroupRecipient(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsSetRecipient_DeprecatedAlias(t *testing.T) {
	if !IsSetRecipient("set[agent:a,agent:b]") {
		t.Error("IsSetRecipient should return true for valid group recipient")
	}
	if IsSetRecipient("agent:foo") {
		t.Error("IsSetRecipient should return false for non-group recipient")
	}
}

func TestParseGroupRecipient_Valid(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []GroupRecipient
	}{
		{
			name:  "group prefix two agents",
			input: "group[agent:reviewer,agent:deploy-bot]",
			want: []GroupRecipient{
				{Kind: RecipientAgent, Name: "reviewer"},
				{Kind: RecipientAgent, Name: "deploy-bot"},
			},
		},
		{
			name:  "legacy set prefix two agents",
			input: "set[agent:reviewer,agent:deploy-bot]",
			want: []GroupRecipient{
				{Kind: RecipientAgent, Name: "reviewer"},
				{Kind: RecipientAgent, Name: "deploy-bot"},
			},
		},
		{
			name:  "mixed agent and user",
			input: "group[agent:reviewer,user:alice@example.com]",
			want: []GroupRecipient{
				{Kind: RecipientAgent, Name: "reviewer"},
				{Kind: RecipientUser, Name: "alice@example.com"},
			},
		},
		{
			name:  "bare names default to agent",
			input: "group[reviewer,deploy-bot]",
			want: []GroupRecipient{
				{Kind: RecipientAgent, Name: "reviewer"},
				{Kind: RecipientAgent, Name: "deploy-bot"},
			},
		},
		{
			name:  "bare email defaults to user",
			input: "group[agent:bot,alice@example.com]",
			want: []GroupRecipient{
				{Kind: RecipientAgent, Name: "bot"},
				{Kind: RecipientUser, Name: "alice@example.com"},
			},
		},
		{
			name:  "user prefix without email",
			input: "group[user:alice,agent:bot]",
			want: []GroupRecipient{
				{Kind: RecipientUser, Name: "alice"},
				{Kind: RecipientAgent, Name: "bot"},
			},
		},
		{
			name:  "whitespace trimmed",
			input: "group[ agent:a , agent:b , user:c ]",
			want: []GroupRecipient{
				{Kind: RecipientAgent, Name: "a"},
				{Kind: RecipientAgent, Name: "b"},
				{Kind: RecipientUser, Name: "c"},
			},
		},
		{
			name:  "deduplication",
			input: "group[agent:a,agent:b,agent:a]",
			want: []GroupRecipient{
				{Kind: RecipientAgent, Name: "a"},
				{Kind: RecipientAgent, Name: "b"},
			},
		},
		{
			name:  "three recipients all types",
			input: "group[agent:reviewer,user:alice@example.com,deploy-bot]",
			want: []GroupRecipient{
				{Kind: RecipientAgent, Name: "reviewer"},
				{Kind: RecipientUser, Name: "alice@example.com"},
				{Kind: RecipientAgent, Name: "deploy-bot"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGroupRecipient(tt.input)
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

func TestParseGroupRecipient_Errors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "not a group",
			input:   "agent:foo",
			wantErr: "not a group recipient",
		},
		{
			name:    "empty group",
			input:   "group[]",
			wantErr: "empty group[]",
		},
		{
			name:    "empty legacy set",
			input:   "set[]",
			wantErr: "empty group[]",
		},
		{
			name:    "single element",
			input:   "group[agent:a]",
			wantErr: "at least 2 recipients",
		},
		{
			name:    "nested group",
			input:   "group[agent:a,group[agent:b,agent:c]]",
			wantErr: "nested group[]",
		},
		{
			name:    "nested set inside group",
			input:   "group[agent:a,set[agent:b,agent:c]]",
			wantErr: "nested group[]",
		},
		{
			name:    "unknown prefix",
			input:   "group[foo:bar,agent:a]",
			wantErr: "unknown recipient prefix",
		},
		{
			name:    "empty agent name",
			input:   "group[agent:,agent:b]",
			wantErr: "empty agent name",
		},
		{
			name:    "empty user name",
			input:   "group[user:,agent:b]",
			wantErr: "empty user name",
		},
		{
			name:    "whitespace only",
			input:   "group[  ]",
			wantErr: "empty group[]",
		},
		{
			name:    "all duplicates collapse to single",
			input:   "group[agent:a,agent:a]",
			wantErr: "at least 2 recipients",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseGroupRecipient(tt.input)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestParseGroupRecipient_MaxLimit(t *testing.T) {
	parts := make([]string, MaxGroupRecipients+1)
	for i := range parts {
		parts[i] = "agent:a" + strings.Repeat("x", 3) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	input := "group[" + strings.Join(parts, ",") + "]"
	_, err := ParseGroupRecipient(input)
	if err == nil {
		t.Fatal("expected error for exceeding max recipients")
	}
	if !strings.Contains(err.Error(), "maximum is") {
		t.Errorf("error %q does not mention maximum", err.Error())
	}
}

func TestFormatGroupRecipients(t *testing.T) {
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
			want:       "group[user:alice,agent:coder,agent:reviewer]",
		},
		{
			name:       "agent sender with agents",
			sender:     "agent:lead",
			recipients: []string{"agent:coder", "agent:reviewer"},
			want:       "group[agent:lead,agent:coder,agent:reviewer]",
		},
		{
			name:       "mixed recipients",
			sender:     "user:bob@example.com",
			recipients: []string{"agent:deploy", "user:carol@example.com"},
			want:       "group[user:bob@example.com,agent:deploy,user:carol@example.com]",
		},
		{
			name:       "single recipient",
			sender:     "user:alice",
			recipients: []string{"agent:coder"},
			want:       "group[user:alice,agent:coder]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatGroupRecipients(tt.sender, tt.recipients)
			if got != tt.want {
				t.Errorf("FormatGroupRecipients() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatGroupRecipients_Roundtrip(t *testing.T) {
	sender := "user:alice"
	recipients := []string{"agent:coder", "agent:reviewer"}
	formatted := FormatGroupRecipients(sender, recipients)

	if !strings.HasPrefix(formatted, GroupPrefix) {
		t.Errorf("FormatGroupRecipients should emit group[ prefix, got %q", formatted)
	}

	parsed, err := ParseGroupRecipient(formatted)
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

func TestGroupRecipientString(t *testing.T) {
	r := GroupRecipient{Kind: RecipientAgent, Name: "reviewer"}
	if r.String() != "agent:reviewer" {
		t.Errorf("String() = %q, want %q", r.String(), "agent:reviewer")
	}
	r = GroupRecipient{Kind: RecipientUser, Name: "alice"}
	if r.String() != "user:alice" {
		t.Errorf("String() = %q, want %q", r.String(), "user:alice")
	}
}

func TestDeprecatedAliases(t *testing.T) {
	// ParseSetRecipient should work as alias for ParseGroupRecipient
	parsed, err := ParseSetRecipient("set[agent:a,agent:b]")
	if err != nil {
		t.Fatalf("ParseSetRecipient alias failed: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 recipients, got %d", len(parsed))
	}

	// FormatSetRecipients should work as alias for FormatGroupRecipients
	formatted := FormatSetRecipients("user:alice", []string{"agent:a"})
	if formatted != "group[user:alice,agent:a]" {
		t.Errorf("FormatSetRecipients alias = %q, want %q", formatted, "group[user:alice,agent:a]")
	}

	// MaxSetRecipients should equal MaxGroupRecipients
	if MaxSetRecipients != MaxGroupRecipients {
		t.Errorf("MaxSetRecipients (%d) != MaxGroupRecipients (%d)", MaxSetRecipients, MaxGroupRecipients)
	}
}
