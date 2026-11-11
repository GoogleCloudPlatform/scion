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
	"io"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GroupParticipantModeCleanupExecutor is a one-time cleanup of agent
// participant rows in group conversations that the message mode rules in
// group_message_mode.go no longer write on their own (ptone/scion#5520,
// follow-up to ptone/scion#4413). It is an optional maintenance migration
// (key "group-participant-mode-cleanup"); nothing runs it automatically.
//
// A row is removed when all of these hold:
//
//   - the conversation is a group conversation that is not soft-deleted;
//   - the row is an agent row that is still active (left_at is NULL);
//   - the agent is soft-deleted or missing, or its message mode is one for
//     which groupAgentJoinAllowed refuses every adder other than an
//     authorized human: none, lineage, branch, or a value outside the five
//     modes. groupModeKeepsParticipant is that rule.
//
// Rows of lineage and branch agents that a human added cannot be told apart
// from older rows, so they are removed too; a human whom the agent's mode
// lets message it re-adds the agent through the participant API, which
// applies groupAgentJoinAllowed.
//
// Removal is the soft remove every leave uses (RemoveParticipant sets
// left_at), so a later add of the same agent re-activates the row.
//
// The run only reports unless params["apply"] is "true" and the run is not
// a dry run. The log names conversation and agent IDs and counts, never
// user IDs or emails. It is safe to re-run: a second applied run removes 0.
type GroupParticipantModeCleanupExecutor struct {
	Store store.Store
}

// Reasons a row is removed, used as count keys and in the log.
const (
	groupCleanupReasonNone    = store.MessageModeNone
	groupCleanupReasonLineage = store.MessageModeLineage
	groupCleanupReasonBranch  = store.MessageModeBranch
	groupCleanupReasonUnknown = "unknown_mode"
	groupCleanupReasonDeleted = "agent_deleted"
	groupCleanupReasonMissing = "agent_missing"
)

// groupParticipantCleanupResult summarizes one run.
type groupParticipantCleanupResult struct {
	Applied              bool           `json:"applied"`
	ConversationsScanned int            `json:"conversationsScanned"`
	AgentRowsScanned     int            `json:"agentRowsScanned"`
	Kept                 int            `json:"kept"`
	Matched              int            `json:"matched"`
	Removed              int            `json:"removed"`
	AlreadyLeft          int            `json:"alreadyLeft"`
	Failed               int            `json:"failed"`
	ByReason             map[string]int `json:"byReason"`
}

// groupModeKeepsParticipant reports whether an agent with this stored mode
// may hold a group participant row that nobody vetted: exactly the modes
// for which groupAgentJoinAllowed allows any adder. It uses the same mode
// mapping as the gate (effectiveGroupMessageMode), with no other
// normalization, so a value the gate refuses is never kept.
func groupModeKeepsParticipant(mode string) bool {
	switch effectiveGroupMessageMode(mode) {
	case store.MessageModeProject, store.MessageModeHub:
		return true
	default:
		return false
	}
}

// groupParticipantCleanupReason returns the removal reason for an agent
// row, or "" to keep it. agent is nil when the agent does not exist.
func groupParticipantCleanupReason(agent *store.Agent) string {
	if agent == nil {
		return groupCleanupReasonMissing
	}
	if !agent.DeletedAt.IsZero() {
		return groupCleanupReasonDeleted
	}
	if groupModeKeepsParticipant(agent.MessageMode) {
		return ""
	}
	switch effectiveGroupMessageMode(agent.MessageMode) {
	case store.MessageModeNone:
		return groupCleanupReasonNone
	case store.MessageModeLineage:
		return groupCleanupReasonLineage
	case store.MessageModeBranch:
		return groupCleanupReasonBranch
	default:
		return groupCleanupReasonUnknown
	}
}

func (e *GroupParticipantModeCleanupExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	_, err := e.run(ctx, logger, params)
	return err
}

// run does the work of Run and returns the summary it logs.
func (e *GroupParticipantModeCleanupExecutor) run(ctx context.Context, logger io.Writer, params map[string]string) (groupParticipantCleanupResult, error) {
	apply := params["apply"] == "true" && params["dryRun"] != "true"
	result := groupParticipantCleanupResult{Applied: apply, ByReason: map[string]int{}}
	if !apply {
		_, _ = fmt.Fprintln(logger, `REPORT ONLY: no changes will be made. Run with params {"apply":"true"} to remove the rows listed below.`)
	}

	// reasons caches the decision per agent ID for this run.
	reasons := map[string]string{}
	reasonFor := func(agentID string) (string, error) {
		if r, ok := reasons[agentID]; ok {
			return r, nil
		}
		agent, err := e.Store.GetAgent(ctx, agentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		if err != nil {
			agent = nil
		}
		r := groupParticipantCleanupReason(agent)
		reasons[agentID] = r
		return r, nil
	}

	verb := "WOULD REMOVE"
	if apply {
		verb = "REMOVE"
	}

	cursor := ""
	const pageSize = 200
	for {
		page, err := e.Store.ListConversations(ctx, store.ConversationFilter{Kind: "group"}, store.ListOptions{
			Limit:          pageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return result, fmt.Errorf("list group conversations: %w", err)
		}

		for i := range page.Items {
			conv := &page.Items[i]
			if conv.Kind != "group" || conv.DeletedAt != nil {
				continue
			}
			result.ConversationsScanned++

			participants, err := e.Store.ListParticipants(ctx, conv.ID)
			if err != nil {
				return result, fmt.Errorf("list participants of conversation %s: %w", conv.ID, err)
			}
			for _, p := range participants {
				if p.PrincipalKind != "agent" || p.LeftAt != nil {
					continue
				}
				result.AgentRowsScanned++
				reason, err := reasonFor(p.PrincipalID)
				if err != nil {
					return result, fmt.Errorf("look up agent %s: %w", p.PrincipalID, err)
				}
				if reason == "" {
					result.Kept++
					continue
				}
				result.Matched++
				if !apply {
					result.ByReason[reason]++
					_, _ = fmt.Fprintf(logger, "  %s conversation=%s agent=%s reason=%s\n", verb, conv.ID, p.PrincipalID, reason)
					continue
				}
				err = e.Store.RemoveParticipant(ctx, conv.ID, "agent", p.PrincipalID)
				switch {
				case err == nil:
					result.Removed++
					result.ByReason[reason]++
					_, _ = fmt.Fprintf(logger, "  %s conversation=%s agent=%s reason=%s\n", verb, conv.ID, p.PrincipalID, reason)
				case errors.Is(err, store.ErrNotFound):
					// The row was left concurrently; nothing to do.
					result.AlreadyLeft++
				default:
					result.Failed++
					_, _ = fmt.Fprintf(logger, "  WARN conversation=%s agent=%s - failed to remove: %v\n", conv.ID, p.PrincipalID, err)
				}
			}
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	_, _ = fmt.Fprintf(logger, "Scanned %d group conversation(s), %d active agent row(s); kept %d.\n",
		result.ConversationsScanned, result.AgentRowsScanned, result.Kept)
	countVerb := "Would remove"
	count := result.Matched
	if apply {
		countVerb = "Removed"
		count = result.Removed
	}
	_, _ = fmt.Fprintf(logger, "%s %d row(s)%s.\n", countVerb, count, formatReasonCounts(result.ByReason))
	if result.AlreadyLeft > 0 {
		_, _ = fmt.Fprintf(logger, "%d row(s) were left concurrently.\n", result.AlreadyLeft)
	}
	if result.Failed > 0 {
		return result, fmt.Errorf("%d row(s) could not be removed; re-run to retry", result.Failed)
	}
	return result, nil
}

// formatReasonCounts renders counts as " (reason=n, ...)" in a stable
// order, or "" when there are none.
func formatReasonCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := " ("
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s=%d", k, counts[k])
	}
	return out + ")"
}
