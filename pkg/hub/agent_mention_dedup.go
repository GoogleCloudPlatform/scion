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

import (
	"context"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mentionDedupWindow bounds how far back recentDuplicateMention looks for a
// matching prior mention row. It is deliberately short: it only needs to
// cover the gap between an old CLI's client-side sendMentionMessages POST
// and the server's own synchronous fan-out on the same primary request.
const mentionDedupWindow = 2 * time.Minute

// mentionDedupLookbackLimit bounds how many recent rows recentDuplicateMention
// scans for a content match.
const mentionDedupLookbackLimit = 20

// recentDuplicateMention reports whether a TypeMention row already exists
// from senderID to recipientID within mentionDedupWindow whose body exactly
// matches msg. This is what lets an old CLI (which still POSTs its own
// Type=mention message for each body mention, cmd/message.go
// sendMentionMessages) coexist with a new hub, which has already delivered
// that same mention synchronously on the primary request: the old CLI's
// follow-up POST is recognized as a duplicate, not a second, deliberate
// mention.
func (s *Server) recentDuplicateMention(ctx context.Context, senderID, recipientID, msg string) (*store.Message, bool) {
	res, err := s.store.ListMessages(ctx, store.MessageFilter{
		SenderID:    senderID,
		RecipientID: recipientID,
		Type:        messages.TypeMention,
		After:       time.Now().Add(-mentionDedupWindow),
	}, store.ListOptions{Limit: mentionDedupLookbackLimit, SkipTotalCount: true})
	if err != nil {
		s.messageLog.Warn("recentDuplicateMention: list failed", "sender_id", senderID, "recipient_id", recipientID, "error", err)
		return nil, false
	}
	if res == nil {
		return nil, false
	}
	for i := range res.Items {
		// A row whose delivery already failed is not a duplicate to hide the
		// old CLI's follow-up POST behind: the mention was never actually
		// delivered, so this POST is the only chance for it to go out. Only
		// a row that is still pending or was actually dispatched counts as
		// the "already delivered" match this gate exists to recognize.
		if res.Items[i].DispatchState == store.MessageDispatchFailed {
			continue
		}
		if res.Items[i].Msg == msg {
			return &res.Items[i], true
		}
	}
	return nil, false
}
