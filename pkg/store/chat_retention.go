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
	"context"
	"time"
)

// ChatRetentionPhaseMain is the cursor phase of the main retention window:
// the oldest-first walk of messages over the created index.
const ChatRetentionPhaseMain = "main"

// Chat retention run triggers and statuses (chat_retention_runs).
const (
	ChatRetentionTriggerSweep = "sweep"

	ChatRetentionStatusRunning = "running"
	ChatRetentionStatusOK      = "ok"
	ChatRetentionStatusPartial = "partial"
	ChatRetentionStatusFailed  = "failed"
)

// ChatRetentionCandidate is one message the main-window walk returned.
type ChatRetentionCandidate struct {
	ID             string
	Created        time.Time
	ProjectID      string
	ConversationID string // "" when the message has no conversation
}

// ChatRetentionPosition is a position in the (created, id) walk. The zero
// value is the start of the walk.
type ChatRetentionPosition struct {
	Created time.Time
	ID      string
}

// IsStart reports whether p is the start of the walk.
func (p ChatRetentionPosition) IsStart() bool { return p.Created.IsZero() && p.ID == "" }

// ChatRetentionCursor is the persisted resume point of one phase.
type ChatRetentionCursor struct {
	Phase string
	// Position is the last message the walk returned; zero = the start.
	Position ChatRetentionPosition
	// ResetAt is when the position was last reset to the start; nil when
	// it never was.
	ResetAt *time.Time
	// LastAppliedCutoff is the cutoff of the last tick whose walk reached
	// it (returned a short chunk); nil when none did yet.
	LastAppliedCutoff *time.Time
	UpdatedAt         time.Time
}

// ChatRetentionCounts are the rows one purge deleted, attributed to one
// project. It never carries a message ID or content.
type ChatRetentionCounts struct {
	// Main counts messages deleted by the main window.
	Main int64 `json:"main,omitempty"`
	// Addressees, Mentions, Stars, Ext and Attachments count the
	// dependent rows deleted with those messages: message_addressees,
	// webchat_mention, webchat_message_star, webchat_message_ext and
	// webchat_message_attachment (links; attachment metadata and bytes
	// are left to the attachment GC in this phase).
	Addressees  int64 `json:"addressees,omitempty"`
	Mentions    int64 `json:"mentions,omitempty"`
	Stars       int64 `json:"stars,omitempty"`
	Ext         int64 `json:"ext,omitempty"`
	Attachments int64 `json:"attachments,omitempty"`
	// UnlinkedAttachments counts attachments left with no message link
	// by the purge. Their metadata rows stay for the attachment GC.
	UnlinkedAttachments int64 `json:"unlinked_attachments,omitempty"`
}

// Add adds o to c.
func (c *ChatRetentionCounts) Add(o ChatRetentionCounts) {
	c.Main += o.Main
	c.Addressees += o.Addressees
	c.Mentions += o.Mentions
	c.Stars += o.Stars
	c.Ext += o.Ext
	c.Attachments += o.Attachments
	c.UnlinkedAttachments += o.UnlinkedAttachments
}

// ChatRetentionPurgeResult is the outcome of one purge transaction.
type ChatRetentionPurgeResult struct {
	// ByProject holds the deleted rows per project ID.
	ByProject map[string]ChatRetentionCounts
	// UnlinkedAttachmentIDs are the attachments the purge left with no
	// message link. Their metadata rows are kept (the attachment GC
	// removes them with their bytes).
	UnlinkedAttachmentIDs []string
}

// Total sums r over every project.
func (r ChatRetentionPurgeResult) Total() ChatRetentionCounts {
	var t ChatRetentionCounts
	for _, c := range r.ByProject {
		t.Add(c)
	}
	return t
}

// ChatRetentionPolicy is the policy a run applied, recorded on its row.
type ChatRetentionPolicy struct {
	MessagesRetentionDays int       `json:"messages_retention_days"`
	MainCutoff            time.Time `json:"main_cutoff"`
}

// ChatRetentionRun is one chat_retention_runs row: the audit record of a
// retention run, with the policy and per-project counts and never a
// message ID or content.
type ChatRetentionRun struct {
	ID         string
	Trigger    string
	DryRun     bool
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     string
	Actor      string
	Policy     ChatRetentionPolicy
	Counts     map[string]ChatRetentionCounts
	Error      string
}

// ChatRetentionStore is the persistence of chat message retention (design
// chat message retention §6). The selection and the purge are raw SQL over
// the database the ent client and the web chat tables share, so one
// transaction deletes a message and every dependent row.
type ChatRetentionStore interface {
	// SelectChatRetentionCandidates returns up to limit messages created
	// before cutoff and after the position after, oldest first in
	// (created, id) order, skipping pending and deferred messages. It is
	// served by the created index; it never counts or scans the table.
	SelectChatRetentionCandidates(ctx context.Context, cutoff time.Time, after ChatRetentionPosition, limit int) ([]ChatRetentionCandidate, error)

	// PurgeChatMessages deletes the given messages with their
	// message_addressees, webchat_mention, webchat_message_ext,
	// webchat_message_attachment and webchat_message_star rows, and
	// stores cursor, in one transaction. projectOf maps each ID to its
	// project for the counts. Deletes are idempotent: an ID that is
	// already gone counts nothing.
	PurgeChatMessages(ctx context.Context, ids []string, projectOf map[string]string, cursor ChatRetentionCursor) (ChatRetentionPurgeResult, error)

	// GetChatRetentionCursor returns the cursor of phase, or a zero cursor
	// for phase (Position at the start) when none is stored.
	GetChatRetentionCursor(ctx context.Context, phase string) (ChatRetentionCursor, error)

	// SaveChatRetentionCursor stores cursor (upsert by phase), including
	// LastAppliedCutoff.
	SaveChatRetentionCursor(ctx context.Context, cursor ChatRetentionCursor) error

	// LatestChatRetentionRun returns the most recent run with the given
	// trigger started at or after since, or nil when there is none.
	LatestChatRetentionRun(ctx context.Context, trigger string, since time.Time) (*ChatRetentionRun, error)

	// SaveChatRetentionRun inserts run when its ID is empty (setting it)
	// and otherwise updates its finished_at, status, policy, counts and
	// error.
	SaveChatRetentionRun(ctx context.Context, run *ChatRetentionRun) error

	// PruneChatRetentionRuns deletes runs started before before and
	// returns how many it deleted.
	PruneChatRetentionRuns(ctx context.Context, before time.Time) (int, error)
}
