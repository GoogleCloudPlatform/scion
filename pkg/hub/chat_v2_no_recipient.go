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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// matchHumanMentionIDs resolves @mention names against a project's human
// members and returns the matched user IDs in mention order, without
// duplicates. A name matches a member's display name, its hyphenated slug
// (the form the web autocomplete inserts, e.g. "John Smith" -> "john-smith"),
// their email, or the email's local part, case-insensitively.
//
// It is a pure function: no store access and no side effects. The human
// mention notification path (fireHumanMentionNotifications) mirrors these
// rules in its own copy; the two should be unified later.
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
// no agent recipient is provably addressed to no person, so it can be
// recorded as no_recipient. It returns false, keeping the previous state,
// whenever the message may be addressed to someone or that cannot be
// decided:
//
//   - it quote-replies to a message in this thread that anyone other than
//     the sender's own user identity sent (another person, an agent, a
//     bridged sender with no user ID, or any other sender kind);
//   - it @mentions a project member other than the sender;
//   - a lookup it needs fails.
func (s *Server) threadMessageUnaddressed(ctx context.Context, key, projectID string, mentionNames []string, replyToID, senderUserID string) bool {
	if replyToID != "" {
		refMsgs, err := s.store.GetMessagesByIDs(ctx, []string{replyToID})
		if err != nil {
			return false
		}
		if ref := refMsgs[replyToID]; ref != nil && ref.ThreadID == key {
			ownMessage := strings.HasPrefix(ref.Sender, "user:") && ref.SenderID == senderUserID
			if !ownMessage {
				return false
			}
		}
	}
	if len(mentionNames) == 0 {
		return true
	}
	mentioned, err := s.mentionsProjectHuman(ctx, projectID, mentionNames, senderUserID)
	return err == nil && !mentioned
}

// mentionsProjectHuman reports whether a message is addressed to at least
// one mentioned project member other than its sender. A store error is
// returned rather than read as "no member mentioned".
func (s *Server) mentionsProjectHuman(ctx context.Context, projectID string, mentionNames []string, senderUserID string) (bool, error) {
	if projectID == "" || len(mentionNames) == 0 {
		return false, nil
	}
	members, err := s.projectHumanMembersStrict(ctx, projectID)
	if err != nil {
		return false, err
	}
	for _, id := range matchHumanMentionIDs(members, mentionNames) {
		if id != senderUserID {
			return true, nil
		}
	}
	return false, nil
}

// projectHumanMembersStrict is resolveProjectHumanMembers without its
// best-effort error handling: a failed member listing, or a failed user
// lookup other than not-found, is returned instead of being skipped.
func (s *Server) projectHumanMembersStrict(ctx context.Context, projectID string) ([]chatMemberEntry, error) {
	projectMembers, err := s.store.ListProjectMembers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var humans []chatMemberEntry
	seen := make(map[string]bool)
	for _, m := range projectMembers {
		if seen[m.UserID] {
			continue
		}
		seen[m.UserID] = true
		u, err := s.store.GetUser(ctx, m.UserID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		humans = append(humans, chatMemberEntry{
			ID:          u.ID,
			Kind:        "user",
			DisplayName: u.DisplayName,
			Email:       u.Email,
		})
	}
	return humans, nil
}
