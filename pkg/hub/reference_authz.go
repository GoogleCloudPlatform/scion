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
	"errors"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file holds the checks for references carried inside chat content: a
// reply target, a read watermark, an attachment, a thread or a conversation
// ID. A reference must belong to the conversation (or the conversation's
// project) it is used in; anything else is answered exactly as if it did not
// exist, and the reason is written to the server log only.
//
// Every helper here that reads a store or asks the authorization service
// answers "no" when that call fails. None of them defaults to allow.

// logReferenceRefused records why a reference was refused. The response the
// caller receives is the route's answer for a missing reference; the reason
// is only ever written here.
func logReferenceRefused(ctx context.Context, route, reason string, caller Identity) {
	var callerType, callerID string
	if caller != nil {
		callerType, callerID = caller.Type(), caller.ID()
	}
	slog.InfoContext(ctx, "reference refused",
		"route", route,
		"reason", reason,
		"caller_type", callerType,
		"caller", callerID,
	)
}

// sameConversation reports whether msg belongs to the current conversation.
// A match on either field is enough:
//
//   - convID != "" && msg.ConversationID == convID (history lists by
//     conversation when the conversation envelope switch is on, and a
//     visible row's ThreadID may then differ from the key)
//   - msg.ThreadID == threadKey (rows listed by thread key)
//
// A message of another conversation matches neither. Pure; no store access.
func sameConversation(msg *store.Message, threadKey, convID string) bool {
	if msg == nil {
		return false
	}
	if threadKey != "" && msg.ThreadID == threadKey {
		return true
	}
	return convID != "" && msg.ConversationID == convID
}

// attachmentUsableIn reports whether an attachment may be sent in a
// conversation. The rule follows the conversation kind, not a resolved
// project (an agent DM resolves to the agent's project, but files uploaded
// in a direct message carry no project):
//
//   - direct message: a file the sender uploaded in a direct message
//     (no project, uploaded by userID)
//   - topic: a file of the topic's project
//
// Pure; no store access.
func attachmentUsableIn(meta *AttachmentMeta, isDM bool, topicProjectID, userID string) bool {
	if meta == nil {
		return false
	}
	if isDM {
		return meta.ProjectID == "" && userID != "" && meta.UploadedBy == userID
	}
	return topicProjectID != "" && meta.ProjectID == topicProjectID
}

// conversationIDForKey returns the conversation ID of a chat key using the
// read-only resolvers history uses, or "" when none exists. A topic key is
// resolved within the topic's own project. A store error from the DM lookup
// is returned; the topic resolver reports a failed lookup as "no
// conversation", which callers treat as no match.
func (s *Server) conversationIDForKey(ctx context.Context, wcs WebChatStore, key string) (string, error) {
	if strings.HasPrefix(key, "dm:") {
		parts := strings.Split(key, ":")
		if len(parts) != 5 {
			return "", nil
		}
		res, err := messaging.ResolveDMConversationForRead(ctx, s.store, s.messageLog, parts[1], parts[2], parts[3], parts[4])
		if err != nil {
			return "", err
		}
		if res == nil {
			return "", nil
		}
		return res.ConversationID, nil
	}
	if wcs == nil {
		return "", nil
	}
	topic, err := wcs.GetTopic(ctx, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	if topic == nil {
		return "", nil
	}
	res := messaging.ResolveThreadConversationForRead(ctx, s.store, s.messageLog, key, topic.ProjectID,
		messaging.WithReadTopicLookup(wcs))
	if res == nil {
		return "", nil
	}
	return res.ConversationID, nil
}

// messageInChatConversation reports whether messageID names a stored message
// of the conversation key. A missing message is (false, nil); a store error
// is (false, err). The conversation ID is resolved only when the thread key
// does not already match and the message carries a conversation ID.
func (s *Server) messageInChatConversation(ctx context.Context, wcs WebChatStore, key, messageID string) (bool, error) {
	msg, err := s.store.GetMessage(ctx, messageID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if msg == nil {
		return false, nil
	}
	if sameConversation(msg, key, "") {
		return true, nil
	}
	if msg.ConversationID == "" {
		return false, nil
	}
	convID, err := s.conversationIDForKey(ctx, wcs, key)
	if err != nil {
		return false, err
	}
	return sameConversation(msg, key, convID), nil
}
