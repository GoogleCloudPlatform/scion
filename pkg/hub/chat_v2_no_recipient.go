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

// mentionMatchesMember reports whether an @mention name refers to a
// project human member: their display name, its hyphenated slug (the form
// the web autocomplete inserts, e.g. "John Smith" -> "john-smith"), their
// email, or the email's local part, case-insensitively.
//
// It is a pure function. The human mention notification path
// (fireHumanMentionNotifications) mirrors these rules in its own copy; the
// two should be unified later.
func mentionMatchesMember(name string, m chatMemberEntry) bool {
	n := strings.ToLower(name)
	if m.DisplayName != "" {
		display := strings.ToLower(m.DisplayName)
		if n == display || n == strings.ReplaceAll(display, " ", "-") {
			return true
		}
	}
	if m.Email != "" {
		email := strings.ToLower(m.Email)
		if n == email {
			return true
		}
		if at := strings.IndexByte(email, '@'); at > 0 && n == email[:at] {
			return true
		}
	}
	return false
}

// threadMessageUnaddressed reports whether a thread message that resolved
// no agent recipient is provably addressed to no person, so it can be
// recorded as no_recipient. It returns false, keeping the previous state,
// whenever the message may be addressed to someone or that cannot be
// decided:
//
//   - it quote-replies to anything but the sender's own user message, in
//     any thread (another person, an agent, a bridged sender with no user
//     ID, any other sender kind), or to a message that cannot be found;
//   - it @mentions a project member other than the sender;
//   - a lookup it needs fails.
func (s *Server) threadMessageUnaddressed(ctx context.Context, projectID string, mentionNames []string, replyToID, senderUserID string) bool {
	if replyToID != "" {
		refMsgs, err := s.store.GetMessagesByIDs(ctx, []string{replyToID})
		if err != nil {
			return false
		}
		ref := refMsgs[replyToID]
		ownMessage := ref != nil && strings.HasPrefix(ref.Sender, "user:") && ref.SenderID == senderUserID
		if !ownMessage {
			return false
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
	// Check every member rather than resolving each name to one ID: two
	// members can share a display name (or one's name can equal another's
	// email local part), and a match on the sender must not hide a match on
	// someone else.
	for _, name := range mentionNames {
		for _, m := range members {
			if m.ID != senderUserID && mentionMatchesMember(name, m) {
				return true, nil
			}
		}
	}
	return false, nil
}

// projectHumanMembersStrict is resolveProjectHumanMembers without its
// best-effort error handling: a failed member listing or user lookup is
// returned instead of being skipped. Users are fetched in one batch; a
// member whose user record no longer exists is skipped.
func (s *Server) projectHumanMembersStrict(ctx context.Context, projectID string) ([]chatMemberEntry, error) {
	projectMembers, err := s.store.ListProjectMembers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	userIDs := make([]string, 0, len(projectMembers))
	seen := make(map[string]bool, len(projectMembers))
	for _, m := range projectMembers {
		if m == nil || seen[m.UserID] {
			continue
		}
		seen[m.UserID] = true
		userIDs = append(userIDs, m.UserID)
	}
	if len(userIDs) == 0 {
		return nil, nil
	}
	users, err := s.store.GetUsersByIDs(ctx, userIDs)
	if err != nil {
		return nil, err
	}
	humans := make([]chatMemberEntry, 0, len(userIDs))
	for _, id := range userIDs {
		u := users[id]
		if u == nil {
			continue
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
