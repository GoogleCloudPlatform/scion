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
// A thread reply that resolves no agent recipient (no default agent, no
// reply-to agent, and no @mention that names an agent or a project member;
// a mention of nobody, such as a typo, counts as none) may have been meant
// for the agent that last posted in the thread. It is never silently routed
// to that agent. Instead, when an agent has posted in the thread, the hub
// appends a note to the stored body that @mentions the most recent human
// poster other than the sender (else the thread creator), who gets the
// ordinary human mention notification, and names the most recent agent
// poster by plain slug so that agent does not read as addressed. No agent
// is invoked. In a thread where no agent has posted there is no agent the
// reply could have been meant for, so it gets no note and stays
// no_recipient. A reply that reaches a live default agent is left
// untouched.
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
		b.WriteString(", so it may need the attention of ")
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
// thread key, and the IDs of human posters other than senderUserID, most
// recent first and without duplicates. Mention fan-out copies are ignored.
func (s *Server) recentThreadPosters(ctx context.Context, key, senderUserID string) (string, []string, error) {
	res, err := s.store.ListMessages(ctx, store.MessageFilter{
		ThreadID:    key,
		ExcludeType: messages.TypeMention,
	}, store.ListOptions{Limit: unmentionedNoteScanLimit, SkipTotalCount: true})
	if err != nil {
		return "", nil, err
	}
	var agentSlug string
	var humans []string
	seen := make(map[string]bool)
	for _, m := range res.Items {
		if agentSlug == "" {
			if slug, ok := strings.CutPrefix(m.Sender, "agent:"); ok && slug != "" {
				agentSlug = slug
			}
		}
		if strings.HasPrefix(m.Sender, "user:") && m.SenderID != "" &&
			m.SenderID != senderUserID && !seen[m.SenderID] {
			seen[m.SenderID] = true
			humans = append(humans, m.SenderID)
		}
	}
	return agentSlug, humans, nil
}

// memberMentionToken returns an @mention token (without the @) that
// resolves to m, and to no other member, through the human mention path:
// the hyphenated display name the web autocomplete inserts, else the email
// local part, else the full email. Uniqueness matters because the mention
// notifier resolves each token to a single member. A candidate equal to a
// project agent slug (agentSlugs, lower-cased) is skipped so the note never
// reads as addressing that agent. It returns "" when no candidate survives
// mention extraction intact and uniquely.
func memberMentionToken(m chatMemberEntry, members []chatMemberEntry, agentSlugs map[string]bool) string {
	var candidates []string
	if m.DisplayName != "" {
		candidates = append(candidates, strings.ReplaceAll(m.DisplayName, " ", "-"))
	}
	if at := strings.IndexByte(m.Email, '@'); at > 0 {
		candidates = append(candidates, m.Email[:at], m.Email)
	}
	for _, c := range candidates {
		got := messages.ExtractMentions("@" + c)
		if len(got) != 1 || got[0] != c || !mentionMatchesMember(c, m) || agentSlugs[strings.ToLower(c)] {
			continue
		}
		unique := true
		for _, o := range members {
			if o.ID != m.ID && mentionMatchesMember(c, o) {
				unique = false
				break
			}
		}
		if unique {
			return c
		}
	}
	return ""
}

// unmentionedHumanNote returns content with a note for a thread reply that
// reaches no agent, plus the mention token it adds. The note goes to the
// first of these who is a project member other than the sender and can be
// mentioned unambiguously: the human posters in the thread, most recent
// first, then the thread creator (creatorID). ok is false, and content is
// unchanged, when no agent has posted in the thread, there is no such
// person, or a lookup fails.
func (s *Server) unmentionedHumanNote(ctx context.Context, members func() ([]chatMemberEntry, error), projectID, key, creatorID, senderUserID, content string) (string, string, bool) {
	all, err := members()
	if err != nil || len(all) == 0 {
		return content, "", false
	}
	poster, humans, err := s.recentThreadPosters(ctx, key, senderUserID)
	if err != nil || poster == "" {
		return content, "", false
	}
	agents, err := listAllProjectAgents(ctx, s.store, projectID)
	if err != nil {
		return content, "", false
	}
	agentSlugs := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentSlugs[strings.ToLower(a.Slug)] = true
	}
	if creatorID != "" && creatorID != senderUserID {
		humans = append(humans, creatorID)
	}
	byID := make(map[string]chatMemberEntry, len(all))
	for _, m := range all {
		byID[m.ID] = m
	}
	for _, id := range humans {
		m, ok := byID[id]
		if !ok {
			continue
		}
		token := memberMentionToken(m, all, agentSlugs)
		if token == "" {
			continue
		}
		out, ok := appendUnmentionedNote(content, token, poster)
		if !ok {
			return content, "", false
		}
		return out, token, true
	}
	return content, "", false
}
