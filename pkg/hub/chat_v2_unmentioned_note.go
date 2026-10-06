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
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Unmentioned thread replies.
//
// A thread reply with no @mention and no reply-to target may have been
// meant for the agent that last posted in the thread. When no agent
// receives it, it is never silently routed to that agent. Instead the hub
// appends a note to the stored body that @mentions the most recent human
// poster other than the sender, who gets the ordinary human mention
// notification, and names the most recent agent poster. No agent is
// invoked. A reply that reaches a live default agent is left untouched.
//
// The note is a suffix on the message body rather than a separate message
// so it reuses the existing persist, publish and mention notification
// paths with no new message kind or schema change.

// unmentionedNoteScanLimit bounds how many recent thread messages are read
// to find the most recent agent and human posters.
const unmentionedNoteScanLimit = 50

// unmentionedReplyNote builds the note addressed to mention. poster, the
// most recent agent poster, is named only when non-empty.
func unmentionedReplyNote(mention, poster string) string {
	var b strings.Builder
	b.WriteString("[note to @")
	b.WriteString(mention)
	b.WriteString(": this may be a reply that did not mention the agent it was replying to")
	if poster != "" {
		b.WriteString(", so it may need the attention of @")
		b.WriteString(poster)
	}
	b.WriteString("]")
	return b.String()
}

// appendUnmentionedNote returns content with the note appended, and false
// when the result would exceed the message length limit (the caller then
// keeps the original content and behaviour).
func appendUnmentionedNote(content, mention, poster string) (string, bool) {
	note := unmentionedReplyNote(mention, poster)
	out := note
	if content != "" {
		out = content + "\n\n" + note
	}
	if utf8.RuneCountInString(out) > messages.MaxMessageLength {
		return content, false
	}
	return out, true
}

// recentThreadPosters returns the slug of the most recent agent poster in
// thread key, and the most recent human poster other than senderUserID
// who is in members (nil when there is none). Mention fan-out copies are
// ignored.
func (s *Server) recentThreadPosters(ctx context.Context, key, senderUserID string, members []chatMemberEntry) (string, *chatMemberEntry, error) {
	res, err := s.store.ListMessages(ctx, store.MessageFilter{
		ThreadID:    key,
		ExcludeType: messages.TypeMention,
	}, store.ListOptions{Limit: unmentionedNoteScanLimit, SkipTotalCount: true})
	if err != nil {
		return "", nil, err
	}
	byID := make(map[string]*chatMemberEntry, len(members))
	for i := range members {
		byID[members[i].ID] = &members[i]
	}
	var agentSlug string
	var human *chatMemberEntry
	for _, m := range res.Items {
		if agentSlug == "" {
			if slug, ok := strings.CutPrefix(m.Sender, "agent:"); ok && slug != "" {
				agentSlug = slug
			}
		}
		if human == nil && strings.HasPrefix(m.Sender, "user:") &&
			m.SenderID != "" && m.SenderID != senderUserID {
			human = byID[m.SenderID]
		}
		if agentSlug != "" && human != nil {
			break
		}
	}
	return agentSlug, human, nil
}

// memberMentionToken returns an @mention token (without the @) that
// resolves to m through the human mention path: the hyphenated display
// name the web autocomplete inserts, else the email local part. It
// returns "" when neither survives mention extraction intact.
func memberMentionToken(m chatMemberEntry) string {
	var candidates []string
	if m.DisplayName != "" {
		candidates = append(candidates, strings.ReplaceAll(m.DisplayName, " ", "-"))
	}
	if at := strings.IndexByte(m.Email, '@'); at > 0 {
		candidates = append(candidates, m.Email[:at])
	}
	for _, c := range candidates {
		got := messages.ExtractMentions("@" + c)
		if len(got) == 1 && got[0] == c && mentionMatchesMember(c, m) {
			return c
		}
	}
	return ""
}

// unmentionedHumanNote returns content with a note to the most recent
// human poster (other than the sender) for a thread reply that reaches no
// agent, plus that person's mention token. ok is false, and content is
// unchanged, when there is no such person or a lookup fails.
func (s *Server) unmentionedHumanNote(ctx context.Context, projectID, key, senderUserID, content string) (string, string, bool) {
	members, err := s.projectHumanMembersStrict(ctx, projectID)
	if err != nil || len(members) == 0 {
		return content, "", false
	}
	poster, human, err := s.recentThreadPosters(ctx, key, senderUserID, members)
	if err != nil || human == nil {
		return content, "", false
	}
	token := memberMentionToken(*human)
	if token == "" {
		return content, "", false
	}
	out, ok := appendUnmentionedNote(content, token, poster)
	if !ok {
		return content, "", false
	}
	return out, token, true
}
