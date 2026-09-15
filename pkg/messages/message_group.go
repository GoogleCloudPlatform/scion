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
	"fmt"
	"log/slog"
	"strings"
)

const (
	// GroupPrefix is the canonical prefix for the group recipient syntax.
	GroupPrefix = "group["
	// MaxGroupRecipients is the maximum number of recipients in a message group.
	MaxGroupRecipients = 50

	legacySetPrefix = "set["
	groupSuffix     = "]"
)

type RecipientKind string

const (
	RecipientAgent RecipientKind = "agent"
	RecipientUser  RecipientKind = "user"
)

// GroupRecipient represents a single recipient within a message group.
type GroupRecipient struct {
	Kind RecipientKind
	Name string
}

func (r GroupRecipient) String() string {
	return string(r.Kind) + ":" + r.Name
}

// IsGroupRecipient reports whether s uses the group recipient syntax (group[...] or legacy set[...]).
func IsGroupRecipient(s string) bool {
	return (strings.HasPrefix(s, GroupPrefix) || strings.HasPrefix(s, legacySetPrefix)) && strings.HasSuffix(s, groupSuffix)
}

// ParseGroupRecipient parses a group recipient string (e.g. "group[agent:a,user:b]")
// into a slice of GroupRecipient values. The legacy "set[...]" syntax is also accepted
// but logs a deprecation warning.
func ParseGroupRecipient(s string) ([]GroupRecipient, error) {
	if !IsGroupRecipient(s) {
		return nil, fmt.Errorf("not a group recipient: must start with %q and end with %q", GroupPrefix, groupSuffix)
	}

	var inner string
	if strings.HasPrefix(s, GroupPrefix) {
		inner = s[len(GroupPrefix) : len(s)-len(groupSuffix)]
	} else {
		slog.Warn("set[] syntax is deprecated; use group[] instead")
		inner = s[len(legacySetPrefix) : len(s)-len(groupSuffix)]
	}

	if strings.Contains(inner, legacySetPrefix) || strings.Contains(inner, GroupPrefix) {
		return nil, fmt.Errorf("nested group[] recipients are not allowed")
	}

	if strings.TrimSpace(inner) == "" {
		return nil, fmt.Errorf("empty group[] recipient")
	}

	parts := strings.Split(inner, ",")

	seen := make(map[string]bool, len(parts))
	var recipients []GroupRecipient

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		r, err := classifyRecipient(part)
		if err != nil {
			return nil, err
		}

		key := r.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		recipients = append(recipients, r)
	}

	if len(recipients) == 0 {
		return nil, fmt.Errorf("empty group[] recipient")
	}
	if len(recipients) == 1 {
		return nil, fmt.Errorf("group[] must contain at least 2 recipients; use a direct recipient instead")
	}
	if len(recipients) > MaxGroupRecipients {
		return nil, fmt.Errorf("group[] contains %d recipients, maximum is %d", len(recipients), MaxGroupRecipients)
	}

	return recipients, nil
}

// FormatGroupRecipients builds a group[...] string from a sender identity and a
// list of recipient identities. The sender is included as the first element so
// that the full group is represented. All identities should be prefixed
// (e.g. "user:alice", "agent:coder").
func FormatGroupRecipients(sender string, recipients []string) string {
	var b strings.Builder
	b.WriteString(GroupPrefix)
	b.WriteString(sender)
	for _, r := range recipients {
		b.WriteByte(',')
		b.WriteString(r)
	}
	b.WriteString(groupSuffix)
	return b.String()
}

func classifyRecipient(s string) (GroupRecipient, error) {
	if strings.HasPrefix(s, "agent:") {
		name := strings.TrimPrefix(s, "agent:")
		if name == "" {
			return GroupRecipient{}, fmt.Errorf("empty agent name in group[] element %q", s)
		}
		return GroupRecipient{Kind: RecipientAgent, Name: name}, nil
	}
	if strings.HasPrefix(s, "user:") {
		name := strings.TrimPrefix(s, "user:")
		if name == "" {
			return GroupRecipient{}, fmt.Errorf("empty user name in group[] element %q", s)
		}
		return GroupRecipient{Kind: RecipientUser, Name: name}, nil
	}
	if strings.Contains(s, "@") {
		return GroupRecipient{Kind: RecipientUser, Name: s}, nil
	}
	if strings.Contains(s, ":") {
		prefix := s[:strings.Index(s, ":")]
		if prefix == "conv" {
			return GroupRecipient{}, fmt.Errorf("conv: addresses a conversation directly and cannot be wrapped in group[] — send to %q as the sole recipient instead", s)
		}
		return GroupRecipient{}, fmt.Errorf("unknown recipient prefix %q in group[] element %q", prefix, s)
	}
	return GroupRecipient{Kind: RecipientAgent, Name: s}, nil
}
