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
)

// matchHumanMentionIDs resolves @mention names against a project's human
// members and returns the matched user IDs in mention order, without
// duplicates. A name matches a member's display name, its hyphenated slug
// (the form the web autocomplete inserts, e.g. "John Smith" -> "john-smith"),
// their email, or the email's local part, case-insensitively.
//
// It is a pure function: no store access and no side effects. The human
// mention notification path (fireHumanMentionNotifications) uses the same
// matching rules.
func matchHumanMentionIDs(humanMembers []chatMemberEntry, mentionNames []string) []string {
	if len(humanMembers) == 0 || len(mentionNames) == 0 {
		return nil
	}
	lookup := make(map[string]string)
	for _, m := range humanMembers {
		if m.DisplayName != "" {
			lookup[strings.ToLower(m.DisplayName)] = m.ID
			if slug := strings.ToLower(strings.ReplaceAll(m.DisplayName, " ", "-")); slug != strings.ToLower(m.DisplayName) {
				lookup[slug] = m.ID
			}
		}
		if m.Email != "" {
			lookup[strings.ToLower(m.Email)] = m.ID
			if at := strings.IndexByte(m.Email, '@'); at > 0 {
				lookup[strings.ToLower(m.Email[:at])] = m.ID
			}
		}
	}
	var out []string
	seen := make(map[string]bool)
	for _, name := range mentionNames {
		id, ok := lookup[strings.ToLower(name)]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// threadMessageUnaddressed reports whether a thread message that resolved
// no agent recipient is also addressed to no person, so it can be recorded
// as no_recipient. A message is addressed to a person when it @mentions a
// project member other than the sender, or quote-replies to a message in
// this thread that another person sent. It returns false whenever that
// cannot be decided (a failed lookup, or a reply to an agent message whose
// agent override did not apply), so the previous state is kept.
func (s *Server) threadMessageUnaddressed(ctx context.Context, key, projectID string, mentionNames []string, replyToID, senderUserID string) bool {
	if replyToID != "" {
		refMsgs, err := s.store.GetMessagesByIDs(ctx, []string{replyToID})
		if err != nil {
			return false
		}
		if ref := refMsgs[replyToID]; ref != nil && ref.ThreadID == key {
			if strings.HasPrefix(ref.Sender, "agent:") {
				return false
			}
			if strings.HasPrefix(ref.Sender, "user:") && ref.SenderID != "" && ref.SenderID != senderUserID {
				return false
			}
		}
	}
	return !s.mentionsProjectHuman(ctx, projectID, mentionNames, senderUserID)
}

// mentionsProjectHuman reports whether a message is addressed to at least
// one mentioned project member other than its sender.
func (s *Server) mentionsProjectHuman(ctx context.Context, projectID string, mentionNames []string, senderUserID string) bool {
	if projectID == "" || len(mentionNames) == 0 {
		return false
	}
	for _, id := range matchHumanMentionIDs(s.resolveProjectHumanMembers(ctx, projectID), mentionNames) {
		if id != senderUserID {
			return true
		}
	}
	return false
}
