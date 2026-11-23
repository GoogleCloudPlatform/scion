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

// Package eventfeed provides a transport-neutral, stateless conversation feed
// engine backed by commit-ordered per-conversation sequences and lossy wakeup
// notifications (ptone/scion#8136). It has no dependency on pkg/hub.
package eventfeed

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Event types (namespaced; maps 1:1 to a future SEP-3415 event name).
const (
	TypeConversationMessageCreated = "conversation.message.created"
)

// Event is the transport-neutral envelope. Every wire format is a projection.
type Event struct {
	EventID     string         `json:"eventId"`
	Type        string         `json:"type"`
	OccurredAt  time.Time      `json:"occurredAt"`
	Cursor      Cursor         `json:"cursor"`
	ResourceURI string         `json:"resourceUri"`
	Data        *store.Message `json:"data"`
}

// Cursor is opaque on every wire. v1 encoding:
// base64url("v1." + conversationID + "." + decimal(seq)).
// Bound to one conversation; versioned so the encoding can change.
type Cursor string

const cursorVersionV1 = "v1"

// EncodeCursor encodes a (conversationID, seq) pair into a v1 opaque cursor.
func EncodeCursor(conversationID string, seq int64) Cursor {
	raw := fmt.Sprintf("%s.%s.%d", cursorVersionV1, conversationID, seq)
	return Cursor(base64.RawURLEncoding.EncodeToString([]byte(raw)))
}

// DecodeCursor decodes a v1 opaque cursor and verifies that it is bound to
// expectedConversationID. Returns ErrInvalidCursor on any format, version,
// sequence, or conversation mismatch.
func DecodeCursor(c Cursor, expectedConversationID string) (int64, error) {
	if c == "" {
		return 0, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return 0, ErrInvalidCursor
	}
	parts := strings.Split(string(raw), ".")
	if len(parts) != 3 {
		return 0, ErrInvalidCursor
	}
	if parts[0] != cursorVersionV1 {
		return 0, ErrInvalidCursor
	}
	if parts[1] == "" || parts[1] != expectedConversationID {
		return 0, ErrInvalidCursor
	}
	seq, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || seq < 0 {
		return 0, ErrInvalidCursor
	}
	return seq, nil
}

// Filter selects which messages in a conversation are delivered.
type Filter string

const (
	// FilterOthers (default) delivers everything not sent by the caller.
	FilterOthers Filter = "others"
	// FilterAll delivers everything, including the caller's own sends.
	FilterAll Filter = "all"
	// FilterAddressed delivers messages where the caller is a recipient or
	// addressee (and not the sender).
	FilterAddressed Filter = "addressed"
)

// Start selects the initial position when neither Cursor nor AfterMessageID is set.
type Start string

const (
	// StartHead (default) delivers only messages committed after the poll begins.
	StartHead Start = "head"
	// StartBeginning replays retained history from the first message.
	StartBeginning Start = "beginning"
)

// PollRequest configures one Poll or Watch call.
type PollRequest struct {
	ConversationID string
	Cursor         Cursor        // resume point; empty -> AfterMessageID / Start
	AfterMessageID string        // alternative resume point: resolve to that message's seq
	Start          Start         // head (default) | beginning
	Filter         Filter        // others (default) | all | addressed
	Limit          int           // 1..200, default 50
	Wait           time.Duration // 0 = immediate; clamped to MaxWait
}

// PollResult is the outcome of one Poll call.
type PollResult struct {
	Events     []Event
	NextCursor Cursor        // ALWAYS set, including on empty/timeout results
	HasMore    bool          // more matching events are already available
	Truncated  bool          // rows between the cursor and the first event were deleted
	TimedOut   bool          // Wait elapsed with no matching events
	RetryAfter time.Duration // pacing hint (0 today)
}

var (
	// ErrInvalidCursor indicates a malformed, unknown-version, wrong-conversation,
	// or unresolvable cursor / after_message (HTTP 400).
	ErrInvalidCursor = errors.New("eventfeed: invalid cursor")
	// ErrCursorExpired indicates a cursor whose sequence is ahead of the
	// conversation head (e.g. after a DB restore) (HTTP 410).
	ErrCursorExpired = errors.New("eventfeed: cursor no longer valid")
	// ErrForbidden indicates authorization was denied (HTTP 403/404 per caller).
	ErrForbidden = errors.New("eventfeed: forbidden")
)

// Principal is the caller as the feed sees it (kind + id). Used for filters
// only; authorization is delegated to Authorizer.
type Principal struct {
	Kind string // "user" | "agent"
	ID   string
}

// Authorizer authorizes read access to conversationID and returns the
// conversation record on success.
type Authorizer func(ctx context.Context, conversationID string) (*store.Conversation, error)

// Store is the minimal storage surface required by Feed.
type Store interface {
	ConversationHeadSeq(ctx context.Context, conversationID string) (int64, error)
	MessageConversationSeq(ctx context.Context, conversationID, messageID string) (int64, error)
	ListConversationFeed(ctx context.Context, q store.ConversationFeedQuery) (store.ConversationFeedPage, error)
}

// Waker delivers best-effort wakeup notifications when a conversation may have
// new messages.
type Waker interface {
	// Subscribe returns a 1-slot coalescing channel that receives when the
	// conversation *may* have new messages, and a cancel func.
	Subscribe(conversationID string) (<-chan struct{}, func())
}
