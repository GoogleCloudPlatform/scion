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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RoutingPlan is the result of resolveRoutingAgents. Agents[0] is the primary
// recipient; Agents[1:] are secondary mention recipients. MentionNames and
// MentionResults correspond 1:1 with the extracted and resolved mentions.
type RoutingPlan struct {
	Agents         []*store.Agent           // ordered; first is primary
	MentionNames   []string                 // extracted @-mention tokens (original case)
	MentionResults []messages.MentionResult // per-mention resolution outcome

	// UnresolvedMentions collects mention slugs that did not resolve to any
	// agent in the project. Callers may surface these as diagnostics.
	UnresolvedMentions []string
}

// agentLister is the interface needed to list agents for routing resolution.
// The store.Store interface satisfies this.
type agentLister interface {
	ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error)
}

// listAllProjectAgents paginates through all non-deleted agents in a project.
// It deliberately replaces the earlier fixed 200-agent limit.
func listAllProjectAgents(ctx context.Context, lister agentLister, projectID string) ([]store.Agent, error) {
	var all []store.Agent
	cursor := ""
	prevCursor := ""
	for {
		// Check context before each page fetch to avoid looping after cancellation.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("listing agents for project %s: %w", projectID, err)
		}
		page, err := lister.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{
			Limit:  200,
			Cursor: cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("listing agents for project %s: %w", projectID, err)
		}
		for i := range page.Items {
			if page.Items[i].DeletedAt.IsZero() {
				all = append(all, page.Items[i])
			}
		}
		if page.NextCursor == "" {
			break
		}
		// Guard against a buggy store returning the same cursor indefinitely.
		if page.NextCursor == prevCursor {
			return nil, fmt.Errorf("listing agents for project %s: pagination returned repeated cursor %q", projectID, page.NextCursor)
		}
		prevCursor = cursor
		cursor = page.NextCursor
	}
	return all, nil
}

// resolveRoutingAgents determines the ordered set of agent recipients for a
// message based on its content and the caller-supplied default agent.
//
// The planner extracts @-mentions from content, resolves them against project
// agents via paginated listing, applies the leading-mention override rule, and
// deduplicates by agent ID. The default agent is inserted at the front when
// the message has no leading mention, or when no mentions resolve.
//
// content may already have had a leading "!" stripped; the caller handles
// interrupt semantics.
//
// defaultAgent may be nil when the caller has no configured default.
//
// An empty Agents slice means no routing recipient was found; the caller
// decides the error behavior (broker returns 422; native chat falls through
// to human-to-human).
func resolveRoutingAgents(
	ctx context.Context,
	lister agentLister,
	projectID string,
	content string,
	defaultAgent *store.Agent,
) (RoutingPlan, error) {
	plan := RoutingPlan{}

	// Step 1: Extract @-mention tokens.
	plan.MentionNames = messages.ExtractMentions(content)

	// Step 2: List all project agents for mention resolution.
	projectAgents, err := listAllProjectAgents(ctx, lister, projectID)
	if err != nil {
		return plan, err
	}

	// Build lookup structures.
	agentInfos := make([]messages.AgentInfo, 0, len(projectAgents))
	agentBySlug := make(map[string]*store.Agent, len(projectAgents))
	for i := range projectAgents {
		a := &projectAgents[i]
		agentInfos = append(agentInfos, messages.AgentInfo{Slug: a.Slug, Name: a.Name})
		agentBySlug[strings.ToLower(a.Slug)] = a
	}

	// Step 3: Resolve mentions against known agents.
	// ResolveMentions expects the primary recipient slug for deduplication.
	// We pass "" here because the primary is not yet known — deduplication
	// of the default against mentions is done below by agent ID.
	plan.MentionResults = messages.ResolveMentions(plan.MentionNames, agentInfos, "")

	// Collect resolved agents and unresolved names.
	var mentionedAgents []*store.Agent
	for _, mr := range plan.MentionResults {
		switch mr.Status {
		case "delivered":
			if a, ok := agentBySlug[strings.ToLower(mr.Slug)]; ok {
				mentionedAgents = append(mentionedAgents, a)
			}
		case "not_found":
			plan.UnresolvedMentions = append(plan.UnresolvedMentions, mr.Slug)
		}
	}

	// Step 4: Determine primary using leading-mention override.
	if len(mentionedAgents) > 0 {
		isLeading := len(plan.MentionNames) > 0 &&
			strings.EqualFold(mentionedAgents[0].Slug, plan.MentionNames[0]) &&
			messages.IsLeadingMention(content, plan.MentionNames[0])

		if isLeading {
			// Leading mention overrides the default: mentioned agents as-is,
			// first mentioned is primary. Dedup the default if it appears
			// later in the list.
			plan.Agents = deduplicateAgentsByID(mentionedAgents)
		} else {
			// Non-leading (additive): default is primary, mentions are secondary.
			if defaultAgent != nil {
				// Remove default from mentions if also @-mentioned, then prepend.
				deduped := make([]*store.Agent, 0, len(mentionedAgents))
				for _, a := range mentionedAgents {
					if a.ID != defaultAgent.ID {
						deduped = append(deduped, a)
					}
				}
				plan.Agents = append([]*store.Agent{defaultAgent}, deduped...)
			} else {
				// No default: first resolved mention is primary.
				plan.Agents = deduplicateAgentsByID(mentionedAgents)
			}
		}
	} else if defaultAgent != nil {
		// No mentions resolved: default agent is sole recipient.
		plan.Agents = []*store.Agent{defaultAgent}
	}
	// else: plan.Agents is empty — caller handles the no-recipient case.

	return plan, nil
}

// deduplicateAgentsByID removes duplicate agent entries by ID, preserving order.
func deduplicateAgentsByID(agents []*store.Agent) []*store.Agent {
	seen := make(map[string]bool, len(agents))
	out := make([]*store.Agent, 0, len(agents))
	for _, a := range agents {
		if !seen[a.ID] {
			seen[a.ID] = true
			out = append(out, a)
		}
	}
	return out
}
