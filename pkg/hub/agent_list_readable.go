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
// agree with the items. A row whose read decision fails is denied by
// AuthorizeReadBatch and is dropped. Past authorizedListMaxCandidates
// candidates the total is a lower bound and the page may be short with a
// resume cursor (see authorizedListResult).
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
		agentResource, cursorFor, s.authzService.AuthorizeReadBatch)
}

// listReadableAgentsLegacy is listReadableAgents over the legacy
// (created DESC, id DESC) store order and its created,id cursor.
func (s *Server) listReadableAgentsLegacy(ctx context.Context, identity Identity, filter store.AgentFilter, cursor, binding string, limit int) (authorizedListResult[store.Agent], error) {
	return s.listReadableAgents(ctx, identity, cursor, limit,
		func(ctx context.Context, cursor string, limit int) (*store.ListResult[store.Agent], error) {
			return s.store.ListAgents(ctx, filter, store.ListOptions{
				Limit: limit, Cursor: cursor, CursorBinding: binding, SkipTotalCount: true,
			})
		},
		func(a *store.Agent) string { return authorizedListCursor(a.Created, a.ID, binding) })
}

// listReadableAgentsSorted is listReadableAgents over the sorted-mode store
// order for (sort, dir) and its v2 cursor.
func (s *Server) listReadableAgentsSorted(ctx context.Context, identity Identity, filter store.AgentFilter, p agentListParams, binding string) (authorizedListResult[store.Agent], error) {
	return s.listReadableAgents(ctx, identity, p.cursor, p.limit,
		func(ctx context.Context, cursor string, limit int) (*store.ListResult[store.Agent], error) {
			opts := store.ListOptions{
				Limit: limit, SortBy: p.sort, SortDir: p.dir, CursorBinding: binding, SkipTotalCount: true,
			}
			if cursor != "" {
				cur, err := store.DecodeAgentCursor(cursor, p.sort, p.dir, binding)
				if err != nil {
					return nil, err
				}
				opts.SortCursor = &cur
			}
			return s.store.ListAgents(ctx, filter, opts)
		},
		func(a *store.Agent) string {
			row := agentsort.KeyFor(p.sort, a.ID, a.Created, a.Updated, a.LastActivityEvent)
			return store.EncodeAgentCursor(p.sort, p.dir, row.K, row.Created, a.ID, binding)
		})
}

// readableAgentMembers returns the members identity can read, in order.
// A member whose read decision fails is dropped.
func (s *Server) readableAgentMembers(ctx context.Context, identity Identity, members []store.AgentMember) ([]store.AgentMember, error) {
	resources := make([]Resource, len(members))
	for i, m := range members {
		resources[i] = memberResource(m)
	}
	allowed, err := s.authzService.AuthorizeReadBatch(ctx, identity, resources)
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
