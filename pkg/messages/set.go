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
	SetPrefix        = "set["
	SetSuffix        = "]"
	MaxSetRecipients = 50
)

type RecipientKind string

const (
	RecipientAgent RecipientKind = "agent"
	RecipientUser  RecipientKind = "user"
)

type SetRecipient struct {
	Kind RecipientKind
	Name string
}

func (r SetRecipient) String() string {
	return string(r.Kind) + ":" + r.Name
}

func IsSetRecipient(s string) bool {
	return strings.HasPrefix(s, SetPrefix) && strings.HasSuffix(s, SetSuffix)
}

func ParseSetRecipient(s string) ([]SetRecipient, error) {
	if !IsSetRecipient(s) {
		return nil, fmt.Errorf("not a set recipient: must start with %q and end with %q", SetPrefix, SetSuffix)
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
	var recipients []SetRecipient

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
	if len(recipients) > MaxSetRecipients {
		return nil, fmt.Errorf("set[] contains %d recipients, maximum is %d", len(recipients), MaxSetRecipients)
	}

	return recipients, nil
}

func classifyRecipient(s string) (SetRecipient, error) {
	if strings.HasPrefix(s, "agent:") {
		name := strings.TrimPrefix(s, "agent:")
		if name == "" {
			return SetRecipient{}, fmt.Errorf("empty agent name in set[] element %q", s)
		}
		return SetRecipient{Kind: RecipientAgent, Name: name}, nil
	}
	if strings.HasPrefix(s, "user:") {
		name := strings.TrimPrefix(s, "user:")
		if name == "" {
			return SetRecipient{}, fmt.Errorf("empty user name in set[] element %q", s)
		}
		return SetRecipient{Kind: RecipientUser, Name: name}, nil
	}
	if strings.Contains(s, "@") {
		return SetRecipient{Kind: RecipientUser, Name: s}, nil
	}
	if strings.Contains(s, ":") {
		prefix := s[:strings.Index(s, ":")]
		return SetRecipient{}, fmt.Errorf("unknown recipient prefix %q in set[] element %q", prefix, s)
	}
	return SetRecipient{Kind: RecipientAgent, Name: s}, nil
}
