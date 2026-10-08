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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
)

// agentListAppliesReadRule reports whether the agent-list rule applies to
// the caller of ctx. It applies to every caller except an agent: an agent's
// own agent listings keep their documented sibling-listing behaviour.
func agentListAppliesReadRule(ctx context.Context) bool {
	return GetAgentIdentityFromContext(ctx) == nil
}

// listReadableAgents pages the agents matching a list request through
// authorizedList, keeping only the agents identity can read.
//
// Agent-list rule (ptone/scion#3346): for a user caller, an agent appears
// in an agent list, its pages and its totalCount only if the caller can
// read that agent. listAgents and listProjectAgents both apply it, so the
// two endpoints return the same set for the same project.
//
// The total is the readable count from authorizedList's count pass, and
// the page is filled from readable rows only, so paging and totalCount
// agree with the items. Rows are decided by AuthorizeListReadBatch: on a
// scoped token, agent:list (or project:read on a token bound to the
// agent's project) satisfies the token-scope part of the read decision;
// the rest of the decision runs for every row. A row whose read decision
// fails is denied and dropped. Past authorizedListMaxCandidates
// (2000) candidates, counted before any read decision, the total is a
// lower bound and TotalCountApproximate is set; the page may then be short
// with a resume cursor (see authorizedListResult).
//
// fetch reads one batch of candidates after cursor; cursorFor mints the
// resume cursor for a returned row in the same format fetch accepts.
func (s *Server) listReadableAgents(
	ctx context.Context,
	identity Identity,
	requestCursor string,
	limit int,
	fetch func(ctx context.Context, cursor string, limit int) (*store.ListResult[store.Agent], error),
	cursorFor func(*store.Agent) string,
) (authorizedListResult[store.Agent], error) {
	return authorizedList(ctx, identity, requestCursor, limit,
		func(ctx context.Context, cursor string, limit int) (authorizedCandidatePage[store.Agent], error) {
			page, err := fetch(ctx, cursor, limit)
			if err != nil {
				return authorizedCandidatePage[store.Agent]{}, err
			}
			return authorizedCandidatePage[store.Agent]{Items: page.Items, NextCursor: page.NextCursor}, nil
		},
		agentResource, cursorFor, s.authorizeListReadTimed)
}

// listAgentsLegacyPage returns one legacy-order (created DESC, id DESC)
// page of filter, applying the agent-list rule when it applies to the
// caller; otherwise it is the store page unchanged.
func (s *Server) listAgentsLegacyPage(ctx context.Context, identity Identity, filter store.AgentFilter, cursor, binding string, limit int) (authorizedListResult[store.Agent], error) {
	fetch := func(ctx context.Context, cursor string, limit int, skipTotal bool) (*store.ListResult[store.Agent], error) {
		defer perfPhaseStart(ctx, perfPhaseListDBRead)()
		return s.store.ListAgents(ctx, filter, store.ListOptions{
			Limit: limit, Cursor: cursor, CursorBinding: binding, SkipTotalCount: skipTotal,
		})
	}
	if !agentListAppliesReadRule(ctx) {
		page, err := fetch(ctx, cursor, limit, false)
		if err != nil {
			return authorizedListResult[store.Agent]{}, err
		}
		return authorizedListResult[store.Agent]{Items: page.Items, NextCursor: page.NextCursor, TotalCount: page.TotalCount}, nil
	}
	return s.listReadableAgents(ctx, identity, cursor, limit,
		func(ctx context.Context, cursor string, limit int) (*store.ListResult[store.Agent], error) {
			return fetch(ctx, cursor, limit, true)
		},
		func(a *store.Agent) string { return authorizedListCursor(a.Created, a.ID, binding) })
}

// listAgentsSortedPage returns one sorted-mode page of filter for
// (p.sort, p.dir) with its v2 cursor, applying the agent-list rule when it
// applies to the caller; otherwise it is the store page unchanged.
func (s *Server) listAgentsSortedPage(ctx context.Context, identity Identity, filter store.AgentFilter, p agentListParams, binding string) (authorizedListResult[store.Agent], error) {
	fetch := func(ctx context.Context, cursor string, limit int, skipTotal bool) (*store.ListResult[store.Agent], error) {
		opts := store.ListOptions{
			Limit: limit, SortBy: p.sort, SortDir: p.dir, CursorBinding: binding, SkipTotalCount: skipTotal,
		}
		if cursor != "" {
			cur, err := store.DecodeAgentCursor(cursor, p.sort, p.dir, binding)
			if err != nil {
				return nil, err
			}
			opts.SortCursor = &cur
		}
		defer perfPhaseStart(ctx, perfPhaseListDBRead)()
		return s.store.ListAgents(ctx, filter, opts)
	}
	if !agentListAppliesReadRule(ctx) {
		page, err := fetch(ctx, p.cursor, p.limit, false)
		if err != nil {
			return authorizedListResult[store.Agent]{}, err
		}
		return authorizedListResult[store.Agent]{Items: page.Items, NextCursor: page.NextCursor, TotalCount: page.TotalCount}, nil
	}
	return s.listReadableAgents(ctx, identity, p.cursor, p.limit,
		func(ctx context.Context, cursor string, limit int) (*store.ListResult[store.Agent], error) {
			return fetch(ctx, cursor, limit, true)
		},
		func(a *store.Agent) string {
			row := agentsort.KeyFor(p.sort, a.ID, a.Created, a.Updated, a.LastActivityEvent)
			return store.EncodeAgentCursor(p.sort, p.dir, row.K, row.Created, a.ID, binding)
		})
}

// readableAgentRows returns the rows of items the agent-list rule keeps
// for the caller, in order. A row whose read decision fails is dropped.
func (s *Server) readableAgentRows(ctx context.Context, identity Identity, items []store.Agent) ([]store.Agent, error) {
	if !agentListAppliesReadRule(ctx) {
		return items, nil
	}
	allowed, err := s.authorizeListReadTimed(ctx, identity, agentResources(items))
	if err != nil {
		return nil, err
	}
	out := make([]store.Agent, 0, len(items))
	for i := range items {
		if allowed[i] {
			out = append(out, items[i])
		}
	}
	return out, nil
}

// readableAgentMembers is readableAgentRows for narrow member rows.
func (s *Server) readableAgentMembers(ctx context.Context, identity Identity, members []store.AgentMember) ([]store.AgentMember, error) {
	if !agentListAppliesReadRule(ctx) {
		return members, nil
	}
	resources := make([]Resource, len(members))
	for i, m := range members {
		resources[i] = memberResource(m)
	}
	allowed, err := s.authorizeListReadTimed(ctx, identity, resources)
	if err != nil {
		return nil, err
	}
	out := make([]store.AgentMember, 0, len(members))
	for i, m := range members {
		if allowed[i] {
			out = append(out, m)
		}
	}
	return out, nil
}

// agentResources returns the authorization Resource of each agent.
func agentResources(items []store.Agent) []Resource {
	resources := make([]Resource, len(items))
	for i := range items {
		resources[i] = agentResource(&items[i])
	}
	return resources
}

// authorizeListReadTimed is AuthorizeListReadBatch timed as the
// list_read_authz phase of the request's perf trace. With tracing off the
// timer is the shared no-op; arguments, results and errors pass through
// unchanged either way.
func (s *Server) authorizeListReadTimed(ctx context.Context, identity Identity, resources []Resource) ([]bool, error) {
	done := perfPhaseStart(ctx, perfPhaseListReadAuthz)
	defer done()
	return s.authzService.AuthorizeListReadBatch(ctx, identity, resources)
}
