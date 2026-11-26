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

package cmd

import (
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const inboxNothingWaitingProse = "Nothing Waiting"

type inboxLinkKey string

const (
	// inboxLinkPrimary resolves to the category's hub href; Phases 3 and 4 add
	// chat-* and reviews-* link keys mirroring web/src/utils/inbox-prose.ts.
	inboxLinkPrimary inboxLinkKey = "primary"
)

type inboxProseSegment struct {
	Text string       `json:"text"`
	Link inboxLinkKey `json:"link,omitempty"`
}

// formatInboxProseSegments returns the structured prose segments for an inbox
// category, or nil when nothing is waiting, mirroring web/src/utils/inbox-prose.ts
// (design §6.2, §9, §14.3).
func formatInboxProseSegments(category string, count int, capped bool, breakdown map[string]int) []inboxProseSegment {
	switch category {
	case hubclient.InboxCategoryDecisions:
		if count <= 0 {
			return nil
		}
		return primaryWithAgentsProjects(
			formatCountNoun(count, capped, "decision", "decisions"),
			breakdown[hubclient.InboxBreakdownAgents],
			breakdown[hubclient.InboxBreakdownProjects],
		)
	case hubclient.InboxCategoryAgentSecrets:
		if count <= 0 {
			return nil
		}
		return primaryWithAgentsProjects(
			formatCountNoun(count, capped, "secret request", "secret requests"),
			breakdown[hubclient.InboxBreakdownAgents],
			breakdown[hubclient.InboxBreakdownProjects],
		)
	case hubclient.InboxCategoryWaiting:
		if count <= 0 {
			return nil
		}
		segments := []inboxProseSegment{
			{Text: formatCountNoun(count, capped, "agent waiting", "agents waiting"), Link: inboxLinkPrimary},
		}
		if projects := breakdown[hubclient.InboxBreakdownProjects]; projects > 0 {
			segments = appendUnlinkedSegment(segments, " in "+pluralizeInbox(projects, "project", "projects"))
		}
		return segments
	case hubclient.InboxCategoryPendingSecrets:
		if count <= 0 {
			return nil
		}
		return primaryWithAgentsProjects(
			formatCountNoun(count, capped, "pending secret", "pending secrets"),
			breakdown[hubclient.InboxBreakdownAgents],
			breakdown[hubclient.InboxBreakdownProjects],
		)
	case hubclient.InboxCategoryReviews:
		if count <= 0 {
			return nil
		}
		return []inboxProseSegment{
			{Text: formatCountNoun(count, capped, "pending review", "pending reviews"), Link: inboxLinkPrimary},
		}
	case hubclient.InboxCategoryChat:
		unread := count
		if breakdown != nil {
			if u, ok := breakdown[hubclient.InboxBreakdownUnreadConversations]; ok {
				unread = u
			}
		}
		mentions := breakdown[hubclient.InboxBreakdownMentions]
		starred := breakdown["starredMessages"]
		projects := breakdown[hubclient.InboxBreakdownProjects]

		var segments []inboxProseSegment
		hasClause := false
		if unread > 0 {
			segments = append(segments, inboxProseSegment{
				Text: formatCountNoun(unread, capped, "unread conversation", "unread conversations"),
				Link: inboxLinkPrimary,
			})
			hasClause = true
		}
		if mentions > 0 {
			prefix := ""
			if hasClause {
				prefix = ", "
			}
			segments = appendUnlinkedSegment(segments, fmt.Sprintf("%s%d with mentions", prefix, mentions))
			hasClause = true
		}
		if starred > 0 {
			prefix := ""
			if hasClause {
				prefix = ", "
			}
			segments = appendUnlinkedSegment(segments, prefix+pluralizeInbox(starred, "starred message", "starred messages"))
			hasClause = true
		}
		if !hasClause {
			return nil
		}
		if projects > 0 {
			segments = appendUnlinkedSegment(segments, " across "+pluralizeInbox(projects, "project", "projects"))
		}
		return segments
	default:
		if count <= 0 {
			return nil
		}
		return []inboxProseSegment{
			{Text: formatCountNoun(count, capped, "item waiting", "items waiting"), Link: inboxLinkPrimary},
		}
	}
}

// formatInboxProse renders the summary-of-state prose line for an inbox
// category, mirroring web/src/utils/inbox-prose.ts (design §6.2, §9, §14.3).
func formatInboxProse(category string, count int, capped bool, breakdown map[string]int) string {
	segments := formatInboxProseSegments(category, count, capped, breakdown)
	if len(segments) == 0 {
		return inboxNothingWaitingProse
	}
	var b strings.Builder
	for _, seg := range segments {
		b.WriteString(seg.Text)
	}
	return b.String()
}

func formatCountNoun(count int, capped bool, singular, plural string) string {
	if capped {
		return fmt.Sprintf("%d+ %s", count, plural)
	}
	return pluralizeInbox(count, singular, plural)
}

func appendUnlinkedSegment(segments []inboxProseSegment, text string) []inboxProseSegment {
	if text == "" {
		return segments
	}
	if n := len(segments); n > 0 && segments[n-1].Link == "" {
		segments[n-1].Text += text
		return segments
	}
	return append(segments, inboxProseSegment{Text: text})
}

func primaryWithAgentsProjects(primaryText string, agents, projects int) []inboxProseSegment {
	segments := []inboxProseSegment{{Text: primaryText, Link: inboxLinkPrimary}}
	var suffix string
	if agents > 0 {
		suffix += " from " + pluralizeInbox(agents, "agent", "agents")
	}
	if projects > 0 {
		suffix += " in " + pluralizeInbox(projects, "project", "projects")
	}
	return appendUnlinkedSegment(segments, suffix)
}
