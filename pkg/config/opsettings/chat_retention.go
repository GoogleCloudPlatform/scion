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

package opsettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// ChatRetentionSectionName is the name of the operational settings section
// that bounds how long the hub keeps chat messages (hub.chat_retention).
const ChatRetentionSectionName = "chat_retention"

// Compiled defaults and bounds of the chat_retention section.
const (
	// ChatRetentionMinDays is the shortest messages_retention_days a hub
	// admin may set.
	ChatRetentionMinDays = 7
	// ChatRetentionMaxDays bounds messages_retention_days (100 years).
	ChatRetentionMaxDays = 36500

	// ChatRetentionDefaultGraceDays is the default
	// deleted_conversation_grace_days: the messages of a soft-deleted
	// conversation or topic are purged this long after the deletion.
	ChatRetentionDefaultGraceDays = 30
	// ChatRetentionMinGraceDays is the shortest grace a hub admin may set.
	ChatRetentionMinGraceDays = 1

	ChatRetentionDefaultChunkSize = 200
	ChatRetentionMinChunkSize     = 25
	ChatRetentionMaxChunkSize     = 1000

	// The pause and budget bounds keep a tick inside the hub scheduler's
	// 55 s handler context: a 40 s budget plus one 5 s chunk timeout
	// leaves room for the end-of-tick writes, and a pause never runs past
	// the budget.
	ChatRetentionDefaultPauseMS = 500
	ChatRetentionMaxPauseMS     = 10000

	ChatRetentionDefaultTickBudgetSeconds = 30
	ChatRetentionMaxTickBudgetSeconds     = 40

	ChatRetentionDefaultTickIntervalMinutes = 10
	ChatRetentionMaxTickIntervalMinutes     = 1440
)

// ChatRetentionSettings is the stored chat_retention section. DB-only
// (runtime state), no settings.yaml representation, written through PUT
// /api/v1/admin/chat-retention. Absent row = compiled defaults: disabled,
// no window.
type ChatRetentionSettings struct {
	// Enabled turns the retention sweep on. Default false. It doubles as
	// the legal hold: while it is false nothing is purged.
	Enabled *bool `json:"enabled,omitempty"`
	// MessagesRetentionDays is the hub-wide age window. Absent = keep
	// forever. When set it is at least ChatRetentionMinDays.
	MessagesRetentionDays *int `json:"messages_retention_days,omitempty"`
	// DeletedConversationGraceDays is how long after a conversation or
	// topic is soft-deleted its messages are purged, whatever the window
	// (while enabled). Absent = ChatRetentionDefaultGraceDays.
	DeletedConversationGraceDays *int `json:"deleted_conversation_grace_days,omitempty"`
	// Sweep holds the sweep tuning; absent fields take their defaults.
	Sweep *ChatRetentionSweepSettings `json:"sweep,omitempty"`
}

// ChatRetentionSweepSettings is the sweep tuning of the chat_retention
// section. Every field is optional.
type ChatRetentionSweepSettings struct {
	// ChunkSize is the largest number of messages deleted in one
	// transaction (25..1000, default 200).
	ChunkSize *int `json:"chunk_size,omitempty"`
	// PauseMS is the pause after each chunk, in milliseconds (default 500).
	PauseMS *int `json:"pause_ms,omitempty"`
	// TickBudgetSeconds is the wall time one tick may run (default 30).
	TickBudgetSeconds *int `json:"tick_budget_seconds,omitempty"`
	// TickIntervalMinutes is the time between ticks (default 10).
	TickIntervalMinutes *int `json:"tick_interval_minutes,omitempty"`
}

// ChatRetentionConfig is the resolved chat_retention section.
type ChatRetentionConfig struct {
	Enabled bool
	// MessagesRetentionDays is 0 when no window is set (keep forever).
	MessagesRetentionDays int
	// DeletedConversationGraceDays is always set (at least
	// ChatRetentionMinGraceDays).
	DeletedConversationGraceDays int
	ChunkSize                    int
	PauseMS                      int
	TickBudgetSeconds            int
	TickIntervalMinutes          int
}

// DefaultChatRetentionConfig is the resolved value of an absent section.
func DefaultChatRetentionConfig() ChatRetentionConfig {
	return ChatRetentionConfig{
		DeletedConversationGraceDays: ChatRetentionDefaultGraceDays,
		ChunkSize:                    ChatRetentionDefaultChunkSize,
		PauseMS:                      ChatRetentionDefaultPauseMS,
		TickBudgetSeconds:            ChatRetentionDefaultTickBudgetSeconds,
		TickIntervalMinutes:          ChatRetentionDefaultTickIntervalMinutes,
	}
}

// HasWindow reports whether a retention window is set.
func (c ChatRetentionConfig) HasWindow() bool { return c.MessagesRetentionDays > 0 }

// Pause is PauseMS as a duration.
func (c ChatRetentionConfig) Pause() time.Duration {
	return time.Duration(c.PauseMS) * time.Millisecond
}

// Grace is DeletedConversationGraceDays as a duration.
func (c ChatRetentionConfig) Grace() time.Duration {
	return time.Duration(c.DeletedConversationGraceDays) * 24 * time.Hour
}

// TickBudget is TickBudgetSeconds as a duration.
func (c ChatRetentionConfig) TickBudget() time.Duration {
	return time.Duration(c.TickBudgetSeconds) * time.Second
}

// TickInterval is TickIntervalMinutes as a duration.
func (c ChatRetentionConfig) TickInterval() time.Duration {
	return time.Duration(c.TickIntervalMinutes) * time.Minute
}

// Settings returns the stored document form of c, with every field set.
// A config without a window stores no messages_retention_days.
func (c ChatRetentionConfig) Settings() ChatRetentionSettings {
	enabled := c.Enabled
	grace := c.DeletedConversationGraceDays
	chunk, pause, budget, interval := c.ChunkSize, c.PauseMS, c.TickBudgetSeconds, c.TickIntervalMinutes
	s := ChatRetentionSettings{
		Enabled:                      &enabled,
		DeletedConversationGraceDays: &grace,
		Sweep: &ChatRetentionSweepSettings{
			ChunkSize:           &chunk,
			PauseMS:             &pause,
			TickBudgetSeconds:   &budget,
			TickIntervalMinutes: &interval,
		},
	}
	if c.HasWindow() {
		days := c.MessagesRetentionDays
		s.MessagesRetentionDays = &days
	}
	return s
}

// ParseChatRetentionDoc resolves a stored chat_retention document. Unknown
// fields and out-of-range values are errors: callers treat an error as
// "retention off" (fail closed), never as a default window.
func ParseChatRetentionDoc(raw json.RawMessage) (ChatRetentionConfig, error) {
	var s ChatRetentionSettings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return ChatRetentionConfig{}, fmt.Errorf("chat_retention settings: %w", err)
	}
	return s.Resolve()
}

// Resolve applies the compiled defaults to s and checks every bound.
func (s ChatRetentionSettings) Resolve() (ChatRetentionConfig, error) {
	c := DefaultChatRetentionConfig()
	if s.Enabled != nil {
		c.Enabled = *s.Enabled
	}
	if s.MessagesRetentionDays != nil {
		d := *s.MessagesRetentionDays
		if d < ChatRetentionMinDays || d > ChatRetentionMaxDays {
			return ChatRetentionConfig{}, fmt.Errorf("chat_retention settings: messages_retention_days must be between %d and %d", ChatRetentionMinDays, ChatRetentionMaxDays)
		}
		c.MessagesRetentionDays = d
	}
	if s.DeletedConversationGraceDays != nil {
		g := *s.DeletedConversationGraceDays
		if g < ChatRetentionMinGraceDays || g > ChatRetentionMaxDays {
			return ChatRetentionConfig{}, fmt.Errorf("chat_retention settings: deleted_conversation_grace_days must be between %d and %d", ChatRetentionMinGraceDays, ChatRetentionMaxDays)
		}
		c.DeletedConversationGraceDays = g
	}
	if sw := s.Sweep; sw != nil {
		for _, f := range []struct {
			name     string
			v        *int
			min, max int
			dst      *int
		}{
			{"chunk_size", sw.ChunkSize, ChatRetentionMinChunkSize, ChatRetentionMaxChunkSize, &c.ChunkSize},
			{"pause_ms", sw.PauseMS, 0, ChatRetentionMaxPauseMS, &c.PauseMS},
			{"tick_budget_seconds", sw.TickBudgetSeconds, 1, ChatRetentionMaxTickBudgetSeconds, &c.TickBudgetSeconds},
			{"tick_interval_minutes", sw.TickIntervalMinutes, 1, ChatRetentionMaxTickIntervalMinutes, &c.TickIntervalMinutes},
		} {
			if f.v == nil {
				continue
			}
			if *f.v < f.min || *f.v > f.max {
				return ChatRetentionConfig{}, fmt.Errorf("chat_retention settings: sweep.%s must be between %d and %d", f.name, f.min, f.max)
			}
			*f.dst = *f.v
		}
	}
	return c, nil
}

// chatRetentionSchema is the hand-written JSON schema of the section; it is
// runtime/API-owned state with no $defs in settings-v1.schema.json. Its
// bounds match Resolve.
func chatRetentionSchema() map[string]interface{} {
	intRange := func(min, max int) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "minimum": min, "maximum": max}
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"enabled":                         map[string]interface{}{"type": "boolean"},
			"messages_retention_days":         intRange(ChatRetentionMinDays, ChatRetentionMaxDays),
			"deleted_conversation_grace_days": intRange(ChatRetentionMinGraceDays, ChatRetentionMaxDays),
			"sweep": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"chunk_size":            intRange(ChatRetentionMinChunkSize, ChatRetentionMaxChunkSize),
					"pause_ms":              intRange(0, ChatRetentionMaxPauseMS),
					"tick_budget_seconds":   intRange(1, ChatRetentionMaxTickBudgetSeconds),
					"tick_interval_minutes": intRange(1, ChatRetentionMaxTickIntervalMinutes),
				},
				"additionalProperties": false,
			},
		},
		"additionalProperties": false,
	}
}
