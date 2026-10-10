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

// Default limits for one member fan-out; see chatMemberFanoutLimits.
const (
	defaultChatMemberFanoutTimeout       = 10 * time.Second
	defaultChatMemberFanoutMaxRecipients = 500
)

// chatMemberFanoutLimits bounds one member fan-out. The zero value uses the
// defaults; tests set smaller values on the server.
//
// Members past maxRecipients get no copy on their user subject. They still
// see the message on project.<id>.chat.message while one of their clients is
// subscribed to it, and their unread count catches up on its next refresh.
type chatMemberFanoutLimits struct {
	// timeout bounds one fan-out; a membership write before it has its
	// own bound (threadMembershipTimeout).
	timeout time.Duration
	// maxRecipients bounds how many members receive a copy.
	maxRecipients int
}

// withDefaults returns l with every zero field replaced by its default.
func (l chatMemberFanoutLimits) withDefaults() chatMemberFanoutLimits {
	if l.timeout <= 0 {
		l.timeout = defaultChatMemberFanoutTimeout
	}
	if l.maxRecipients <= 0 {
		l.maxRecipients = defaultChatMemberFanoutMaxRecipients
	}
	return l
}

// snapshotThreadMessage copies msg and attachments for a background job:
// callers keep updating their copy (for example its dispatch state) after
// publishing it.
func snapshotThreadMessage(msg *store.Message, attachments []AttachmentRef) (*store.Message, []AttachmentRef) {
	snapshot := *msg
	return &snapshot, append([]AttachmentRef(nil), attachments...)
}

// fanOutThreadMessageToMembersAsync runs fanOutThreadMessageToMembers in the
// background, detached from the request's cancellation. Publish paths call
// it after the message is stored and its project-subject event is
// published; it never blocks or fails them. Paths that also record thread
// membership for the message use recordThreadMembersThenFanOutAsync, or
// record membership before publishing, so new members are recipients.
func (s *Server) fanOutThreadMessageToMembersAsync(ctx context.Context, msg *store.Message, attachments []AttachmentRef) {
	if !isWebThreadMessage(msg) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	msg, attachments = snapshotThreadMessage(msg, attachments)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.messageLog.Error("chat member fan-out: panic",
					"thread", msg.ThreadID, "panic", fmt.Sprint(rec))
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, s.chatMemberFanout.withDefaults().timeout)
		defer cancel()
		s.fanOutThreadMessageToMembers(ctx, msg, attachments)
	}()
}

// recordThreadMembersThenFanOutAsync records m's thread membership and then
// fans msg out to the thread's members, in that order, in one background
// job. A user who becomes a member through this message (its sender, or a
// human it @mentions) is then among its recipients: for a mentioned user
// who is not watching the project, that copy is the real-time signal that
// replaced the MENTION notification.
func (s *Server) recordThreadMembersThenFanOutAsync(m threadMembership, msg *store.Message, attachments []AttachmentRef) {
	record := m.ThreadKey != "" && !strings.HasPrefix(m.ThreadKey, "dm:") && m.ProjectID != "" &&
		(m.UserID != "" || len(m.MentionedUserIDs) > 0)
	fanOut := isWebThreadMessage(msg)
	if !record && !fanOut {
		return
	}
	if fanOut {
		msg, attachments = snapshotThreadMessage(msg, attachments)
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.messageLog.Error("thread membership and fan-out: panic",
					"thread", m.ThreadKey, "panic", fmt.Sprint(rec))
			}
		}()
		if record {
			ctx, cancel := context.WithTimeout(context.Background(), threadMembershipTimeout)
			s.recordThreadMembers(ctx, m)
			cancel()
		}
		if fanOut {
			ctx, cancel := context.WithTimeout(context.Background(), s.chatMemberFanout.withDefaults().timeout)
			defer cancel()
			s.fanOutThreadMessageToMembers(ctx, msg, attachments)
		}
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

	bound := s.chatMemberFanout.withDefaults().maxRecipients
	var recipients []string
	seen := make(map[string]bool, len(participants))
	for _, p := range participants {
		if p.PrincipalKind != "user" || p.PrincipalID == "" || p.LeftAt != nil || seen[p.PrincipalID] {
			continue
		}
		seen[p.PrincipalID] = true
		// Once the deadline passes every lookup below fails, so stop and
		// say so rather than silently skipping the remaining members.
		if err := ctx.Err(); err != nil {
			s.messageLog.Warn("chat member fan-out: ran out of time; publishing to members found so far",
				"thread", msg.ThreadID, "recipients", len(recipients), "error", err)
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
		if len(recipients) >= bound {
			s.messageLog.Warn("chat member fan-out: members over bound; truncating",
				"thread", msg.ThreadID, "bound", bound)
			break
		}
		recipients = append(recipients, u.ID)
	}
	if len(recipients) == 0 {
		return
	}
	s.events.PublishChatMemberMessage(ctx, msg, attachments, recipients)
}
