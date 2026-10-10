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
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// chatMemberFanoutTimeout bounds one background member fan-out.
const chatMemberFanoutTimeout = 10 * time.Second

// chatMemberFanoutMaxRecipients bounds how many members one thread message
// is fanned out to. Members past the bound still receive the message on the
// project subject; hitting it is logged.
const chatMemberFanoutMaxRecipients = 500

// fanOutThreadMessageToMembersAsync runs fanOutThreadMessageToMembers in the
// background, detached from the request's cancellation. Publish paths call
// it after the message is stored and its project-subject event is
// published; it never blocks or fails them.
func (s *Server) fanOutThreadMessageToMembersAsync(ctx context.Context, msg *store.Message, attachments []AttachmentRef) {
	if !isWebThreadMessage(msg) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	// Snapshot the message: callers keep updating their copy (for example
	// its dispatch state) after publishing it.
	snapshot := *msg
	msg = &snapshot
	attachments = append([]AttachmentRef(nil), attachments...)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.messageLog.Error("chat member fan-out: panic",
					"thread", msg.ThreadID, "panic", fmt.Sprint(rec))
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, chatMemberFanoutTimeout)
		defer cancel()
		s.fanOutThreadMessageToMembers(ctx, msg, attachments)
	}()
}

// isWebThreadMessage reports whether msg is a web chat message in a project
// topic thread: the messages PublishUserMessage sends on
// project.<id>.chat.message.
func isWebThreadMessage(msg *store.Message) bool {
	return msg != nil && msg.Channel == "web" && msg.ProjectID != "" && msg.ThreadID != "" &&
		!strings.HasPrefix(msg.ThreadID, "dm:") && !strings.HasPrefix(msg.ThreadID, "agent:")
}

// fanOutThreadMessageToMembers publishes a thread message on
// user.<id>.chat.message to the thread's members, so a client can keep an
// unread count current without subscribing to every project.
//
// Recipients are the active user participants (conversation_participants,
// left_at unset) of the topic's conversation, and only when the topic
// exists, is not deleted, and belongs to the message's project, and the
// conversation is a group conversation of that same project. Participant
// rows are a listing index, not authorization, so each recipient must also
// be an active (not suspended) user who can read the project now. A user
// who left the thread, or lost project access, gets nothing here.
func (s *Server) fanOutThreadMessageToMembers(ctx context.Context, msg *store.Message, attachments []AttachmentRef) {
	if !isWebThreadMessage(msg) {
		return
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil || s.authzService == nil {
		return
	}

	// GetTopic excludes soft-deleted topics.
	topic, err := wcs.GetTopic(ctx, msg.ThreadID)
	if err != nil || topic == nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.messageLog.Warn("chat member fan-out: topic lookup failed",
				"thread", msg.ThreadID, "error", err)
		}
		return
	}
	if topic.DeletedAt != nil || topic.ProjectID != msg.ProjectID || topic.ConversationID == "" {
		return
	}
	if msg.ConversationID != "" && msg.ConversationID != topic.ConversationID {
		return
	}
	conv, err := s.store.GetConversation(ctx, topic.ConversationID)
	if err != nil || conv == nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.messageLog.Warn("chat member fan-out: conversation lookup failed",
				"thread", msg.ThreadID, "error", err)
		}
		return
	}
	if conv.Kind != "group" || conv.ProjectID == nil || *conv.ProjectID != msg.ProjectID || conv.DeletedAt != nil {
		return
	}

	participants, err := s.store.ListParticipants(ctx, conv.ID)
	if err != nil {
		s.messageLog.Warn("chat member fan-out: participant lookup failed",
			"thread", msg.ThreadID, "error", err)
		return
	}
	if len(participants) == 0 {
		return
	}
	project, err := s.store.GetProject(ctx, msg.ProjectID)
	if err != nil || project == nil {
		s.messageLog.Warn("chat member fan-out: project lookup failed",
			"thread", msg.ThreadID, "error", err)
		return
	}
	resource := projectResource(project)

	var recipients []string
	seen := make(map[string]bool, len(participants))
	for _, p := range participants {
		if p.PrincipalKind != "user" || p.PrincipalID == "" || p.LeftAt != nil || seen[p.PrincipalID] {
			continue
		}
		seen[p.PrincipalID] = true
		if len(recipients) >= chatMemberFanoutMaxRecipients {
			s.messageLog.Warn("chat member fan-out: members over bound; truncating",
				"thread", msg.ThreadID, "bound", chatMemberFanoutMaxRecipients)
			break
		}
		u, err := s.store.GetUser(ctx, p.PrincipalID)
		if err != nil || u == nil || u.Status == store.UserStatusSuspended {
			continue
		}
		identity := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "")
		if !s.authzService.CheckAccess(ctx, identity, resource, ActionRead).Allowed {
			continue
		}
		recipients = append(recipients, u.ID)
	}
	if len(recipients) == 0 {
		return
	}
	s.events.PublishChatMemberMessage(ctx, msg, attachments, recipients)
}
