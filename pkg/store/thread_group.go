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
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Thread group limits (ptone/scion#4424, design section 1). They are server
// constants that the hub API enforces with HTTP 422. The store does not
// enforce them, because the automatic migration of personal groups (design
// section 5a, rule 5) may exceed the per-project group limit on purpose.
const (
	// MaxThreadGroupNameRunes is the longest thread group name, in runes,
	// after trimming.
	MaxThreadGroupNameRunes = 100
	// MaxThreadGroupsPerProject is the most thread groups the API lets a
	// project hold.
	MaxThreadGroupsPerProject = 100
	// MaxThreadsPerThreadGroup is the most threads the API lets one thread
	// group hold.
	MaxThreadsPerThreadGroup = 500
)

// Thread group creator kinds.
const (
	ThreadGroupCreatorUser  = "user"
	ThreadGroupCreatorAgent = "agent"
)

// ThreadGroup is a named, project-scoped folder of group conversations (chat
// threads) that every member of the project sees (ptone/scion#4424).
type ThreadGroup struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	// NameKey is ThreadGroupNameKey(Name). The store computes it; it is
	// unique per project.
	NameKey       string    `json:"-"`
	CreatedBy     string    `json:"createdBy"`
	CreatedByKind string    `json:"createdByKind"` // user | agent
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	// Members is filled by GetThreadGroup and ListThreadGroups, ordered by
	// AddedAt. It is never nil on a group they return.
	Members []ThreadGroupMember `json:"members"`
}

// ThreadGroupMember files one group conversation into one thread group. A
// conversation is in at most one thread group.
type ThreadGroupMember struct {
	ID             string    `json:"id"`
	GroupID        string    `json:"groupId"`
	ProjectID      string    `json:"projectId"`
	ConversationID string    `json:"conversationId"`
	AddedBy        string    `json:"addedBy"`
	AddedAt        time.Time `json:"addedAt"`
}

// stripControlChars removes every Unicode control character from s.
func stripControlChars(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// ThreadGroupNameKey returns the key that thread group names are unique on
// within a project: control characters are removed, then the name is trimmed
// and lowercased with strings.ToLower. It is the only place the key is
// computed, so the hub, the store and the migration of personal groups agree,
// and the unique index needs no database collation.
func ThreadGroupNameKey(name string) string {
	return strings.ToLower(strings.TrimSpace(stripControlChars(name)))
}

// ValidateThreadGroupName checks a thread group name and returns it trimmed.
// A valid name has no control characters and is 1 to
// MaxThreadGroupNameRunes runes long after trimming. Any other name returns
// an error wrapping ErrInvalidInput. The rule is specific to thread groups;
// it is not the thread name rule.
func ValidateThreadGroupName(name string) (string, error) {
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("thread group name must not contain control characters: %w", ErrInvalidInput)
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("thread group name is required: %w", ErrInvalidInput)
	}
	if n := utf8.RuneCountInString(trimmed); n > MaxThreadGroupNameRunes {
		return "", fmt.Errorf("thread group name is %d characters, longer than %d: %w", n, MaxThreadGroupNameRunes, ErrInvalidInput)
	}
	return trimmed, nil
}
