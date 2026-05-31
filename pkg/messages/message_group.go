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
	"strings"
)

const (
	// SetPrefix is the wire-format prefix for the group recipient syntax.
	// Retained as "set[" for backward compatibility with existing CLI usage.
	SetPrefix = "set["
	// SetSuffix is the wire-format suffix for the group recipient syntax.
	SetSuffix = "]"
	// MaxGroupRecipients is the maximum number of recipients in a message group.
	MaxGroupRecipients = 50
)

// Deprecated: Use MaxGroupRecipients instead.
const MaxSetRecipients = MaxGroupRecipients

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

// SetRecipient is a deprecated alias for GroupRecipient.
// Deprecated: Use GroupRecipient instead.
type SetRecipient = GroupRecipient

// IsGroupRecipient reports whether s uses the group recipient syntax (set[...]).
func IsGroupRecipient(s string) bool {
	return strings.HasPrefix(s, SetPrefix) && strings.HasSuffix(s, SetSuffix)
}

// IsSetRecipient is a deprecated alias for IsGroupRecipient.
// Deprecated: Use IsGroupRecipient instead.
func IsSetRecipient(s string) bool {
	return IsGroupRecipient(s)
}

// ParseGroupRecipient parses a group recipient string (e.g. "set[agent:a,user:b]")
// into a slice of GroupRecipient values.
func ParseGroupRecipient(s string) ([]GroupRecipient, error) {
	if !IsGroupRecipient(s) {
		return nil, fmt.Errorf("not a group recipient: must start with %q and end with %q", SetPrefix, SetSuffix)
	}

	inner := s[len(SetPrefix) : len(s)-len(SetSuffix)]
	if strings.Contains(inner, SetPrefix) {
		return nil, fmt.Errorf("nested set[] recipients are not allowed")
	}

	if strings.TrimSpace(inner) == "" {
		return nil, fmt.Errorf("empty set[] recipient")
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
		return nil, fmt.Errorf("empty set[] recipient")
	}
	if len(recipients) == 1 {
		return nil, fmt.Errorf("set[] must contain at least 2 recipients; use a direct recipient instead")
	}
	if len(recipients) > MaxGroupRecipients {
		return nil, fmt.Errorf("set[] contains %d recipients, maximum is %d", len(recipients), MaxGroupRecipients)
	}

	return recipients, nil
}

// ParseSetRecipient is a deprecated alias for ParseGroupRecipient.
// Deprecated: Use ParseGroupRecipient instead.
func ParseSetRecipient(s string) ([]GroupRecipient, error) {
	return ParseGroupRecipient(s)
}

// FormatGroupRecipients builds a set[...] string from a sender identity and a
// list of recipient identities. The sender is included as the first element so
// that the full group is represented. All identities should be prefixed
// (e.g. "user:alice", "agent:coder").
func FormatGroupRecipients(sender string, recipients []string) string {
	var b strings.Builder
	b.WriteString(SetPrefix)
	b.WriteString(sender)
	for _, r := range recipients {
		b.WriteByte(',')
		b.WriteString(r)
	}
	b.WriteString(SetSuffix)
	return b.String()
}

// FormatSetRecipients is a deprecated alias for FormatGroupRecipients.
// Deprecated: Use FormatGroupRecipients instead.
func FormatSetRecipients(sender string, recipients []string) string {
	return FormatGroupRecipients(sender, recipients)
}

func classifyRecipient(s string) (GroupRecipient, error) {
	if strings.HasPrefix(s, "agent:") {
		name := strings.TrimPrefix(s, "agent:")
		if name == "" {
			return GroupRecipient{}, fmt.Errorf("empty agent name in set[] element %q", s)
		}
		return GroupRecipient{Kind: RecipientAgent, Name: name}, nil
	}
	if strings.HasPrefix(s, "user:") {
		name := strings.TrimPrefix(s, "user:")
		if name == "" {
			return GroupRecipient{}, fmt.Errorf("empty user name in set[] element %q", s)
		}
		return GroupRecipient{Kind: RecipientUser, Name: name}, nil
	}
	if strings.Contains(s, "@") {
		return GroupRecipient{Kind: RecipientUser, Name: s}, nil
	}
	if strings.Contains(s, ":") {
		prefix := s[:strings.Index(s, ":")]
		return GroupRecipient{}, fmt.Errorf("unknown recipient prefix %q in set[] element %q", prefix, s)
	}
	return GroupRecipient{Kind: RecipientAgent, Name: s}, nil
}
