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

// formatInboxProse renders the summary-of-state prose line for an inbox
// category, mirroring web/src/utils/inbox-prose.ts (design §6.2, §9).
func formatInboxProse(category string, count int, capped bool, breakdown map[string]int) string {
	switch category {
	case hubclient.InboxCategoryDecisions:
		if count <= 0 {
			return inboxNothingWaitingProse
		}
		return appendAgentsProjects(
			formatCountNoun(count, capped, "decision", "decisions"),
			breakdown[hubclient.InboxBreakdownAgents],
			breakdown[hubclient.InboxBreakdownProjects],
		)
	case "agent_secrets":
		if count <= 0 {
			return inboxNothingWaitingProse
		}
		return appendAgentsProjects(
			formatCountNoun(count, capped, "secret request", "secret requests"),
			breakdown[hubclient.InboxBreakdownAgents],
			breakdown[hubclient.InboxBreakdownProjects],
		)
	case hubclient.InboxCategoryWaiting:
		if count <= 0 {
			return inboxNothingWaitingProse
		}
		base := formatCountNoun(count, capped, "agent waiting", "agents waiting")
		if projects := breakdown[hubclient.InboxBreakdownProjects]; projects > 0 {
			return fmt.Sprintf("%s in %s", base, pluralizeInbox(projects, "project", "projects"))
		}
		return base
	case hubclient.InboxCategoryPendingSecrets:
		if count <= 0 {
			return inboxNothingWaitingProse
		}
		return appendAgentsProjects(
			formatCountNoun(count, capped, "pending secret", "pending secrets"),
			breakdown[hubclient.InboxBreakdownAgents],
			breakdown[hubclient.InboxBreakdownProjects],
		)
	case hubclient.InboxCategoryReviews:
		if count <= 0 {
			return inboxNothingWaitingProse
		}
		return formatCountNoun(count, capped, "pending review", "pending reviews")
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

		var clauses []string
		if unread > 0 {
			clauses = append(clauses, formatCountNoun(unread, capped, "unread conversation", "unread conversations"))
		}
		if mentions > 0 {
			clauses = append(clauses, fmt.Sprintf("%d with mentions", mentions))
		}
		if starred > 0 {
			clauses = append(clauses, pluralizeInbox(starred, "starred message", "starred messages"))
		}
		if len(clauses) == 0 {
			return inboxNothingWaitingProse
		}
		line := strings.Join(clauses, ", ")
		if projects > 0 {
			line += " across " + pluralizeInbox(projects, "project", "projects")
		}
		return line
	default:
		if count <= 0 {
			return inboxNothingWaitingProse
		}
		return formatCountNoun(count, capped, "item waiting", "items waiting")
	}
}

func formatCountNoun(count int, capped bool, singular, plural string) string {
	if capped {
		return fmt.Sprintf("%d+ %s", count, plural)
	}
	return pluralizeInbox(count, singular, plural)
}

func appendAgentsProjects(base string, agents, projects int) string {
	out := base
	if agents > 0 {
		out += " from " + pluralizeInbox(agents, "agent", "agents")
	}
	if projects > 0 {
		out += " in " + pluralizeInbox(projects, "project", "projects")
	}
	return out
}
