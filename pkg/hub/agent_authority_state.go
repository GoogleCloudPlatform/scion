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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file computes an agent's authority state (api.AgentAuthority) at
// read, from the agent's recorded delegation chain (ptone/scion#2143).
// Nothing stores the state: it follows every principal change as soon as
// that change is committed. The state is descriptive only. Every state
// other than live already fails closed in the decisions that need the
// chain; this only makes it visible.
//
// States:
//   - live: the chain reaches an active user;
//   - orphaned: the agent has an active project delegation, but the chain
//     reaches a delegator that is deleted (delegator_deleted), a root user
//     that is not active (delegator_inactive), an agent delegator with no
//     active delegation (delegator_revoked), or cannot be resolved
//     (chain_invalid);
//   - revoked: the agent has no active project delegation, and its latest
//     deactivated one carries a recorded cause, which is the state's cause
//     (for example user_delete_cascade or delegator_deleted);
//   - unrecorded: the chain was never recorded, wholly or in part, and
//     reaches no deleted principal; or the agent has no active project
//     delegation and the one deactivated last carries no recorded cause.

// AgentAuthorityState returns agent's authority state. The principal ID
// is always set when a cause names one; callers that answer a reader
// apply the reader's visibility (Server.authorityForReader). A lookup
// fault returns an error.
func (a *AuthzService) AgentAuthorityState(ctx context.Context, agent *store.Agent) (api.AgentAuthority, error) {
	return newAgentAuthorityResolver(a).resolve(ctx, agent)
}

// agentAuthorityResolver computes authority states for one request. Its
// reads are memoized, so a list whose agents share delegators reads each
// edge set, agent, user and tombstone once. Edge reads go through the
// request-scoped delegation edge caches (getCachedDelegationEdges). It is
// not safe for concurrent use.
type agentAuthorityResolver struct {
	a          *AuthzService
	edges      map[string][]*store.DelegationEdge // active edges by agent ID
	allEdges   map[string][]*store.DelegationEdge // every edge by agent ID
	users      map[string]lookupResult[store.User]
	agents     map[string]lookupResult[store.Agent]
	tombstones map[string]*store.UserDeletion // nil value: no tombstone
}

// lookupResult is a memoized single-row read: the row, or found=false
// for a row that does not exist.
type lookupResult[T any] struct {
	row   *T
	found bool
}

func newAgentAuthorityResolver(a *AuthzService) *agentAuthorityResolver {
	return &agentAuthorityResolver{
		a:          a,
		edges:      map[string][]*store.DelegationEdge{},
		allEdges:   map[string][]*store.DelegationEdge{},
		users:      map[string]lookupResult[store.User]{},
		agents:     map[string]lookupResult[store.Agent]{},
		tombstones: map[string]*store.UserDeletion{},
	}
}

// activeProjectEdges is AuthzService.activeProjectEdges over the cached,
// memoized edge read.
func (r *agentAuthorityResolver) activeProjectEdges(ctx context.Context, agentID, projectID string) ([]*store.DelegationEdge, error) {
	all, ok := r.edges[agentID]
	if !ok {
		var err error
		all, err = r.a.getCachedDelegationEdges(ctx, store.DelegationPrincipalAgent, agentID)
		if err != nil {
			return nil, fmt.Errorf("delegation edge lookup for agent %s: %w", agentID, err)
		}
		r.edges[agentID] = all
	}
	var active []*store.DelegationEdge
	for _, e := range filterEdgesByScope(all, store.RoleScopeProject, projectID) {
		if e.Active {
			active = append(active, e)
		}
	}
	return active, nil
}

func (r *agentAuthorityResolver) getUser(ctx context.Context, id string) (*store.User, error) {
	if res, ok := r.users[id]; ok {
		if !res.found {
			return nil, store.ErrNotFound
		}
		return res.row, nil
	}
	u, err := r.a.store.GetUser(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && u == nil):
		r.users[id] = lookupResult[store.User]{}
		return nil, store.ErrNotFound
	case err != nil:
		return nil, err
	}
	r.users[id] = lookupResult[store.User]{row: u, found: true}
	return u, nil
}

func (r *agentAuthorityResolver) getAgent(ctx context.Context, id string) (*store.Agent, error) {
	if res, ok := r.agents[id]; ok {
		if !res.found {
			return nil, store.ErrNotFound
		}
		return res.row, nil
	}
	ag, err := r.a.store.GetAgent(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && ag == nil):
		r.agents[id] = lookupResult[store.Agent]{}
		return nil, store.ErrNotFound
	case err != nil:
		return nil, err
	}
	r.agents[id] = lookupResult[store.Agent]{row: ag, found: true}
	return ag, nil
}

// tombstone returns the forced-deletion record of userID, or nil.
func (r *agentAuthorityResolver) tombstone(ctx context.Context, userID string) (*store.UserDeletion, error) {
	if d, ok := r.tombstones[userID]; ok {
		return d, nil
	}
	d, err := r.a.store.GetUserDeletionByUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		d, err = nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("user deletion lookup %s: %w", userID, err)
	}
	r.tombstones[userID] = d
	return d, nil
}

// latestDeactivatedProjectEdge returns the agent's inactive edge in
// projectID that was deactivated last, and whether the agent has any edge
// in projectID at all.
func (r *agentAuthorityResolver) latestDeactivatedProjectEdge(ctx context.Context, agentID, projectID string) (*store.DelegationEdge, bool, error) {
	all, ok := r.allEdges[agentID]
	if !ok {
		var err error
		all, err = r.a.store.ListAllDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID)
		if err != nil {
			return nil, false, fmt.Errorf("delegation edge history for agent %s: %w", agentID, err)
		}
		r.allEdges[agentID] = all
	}
	scoped := filterEdgesByScope(all, store.RoleScopeProject, projectID)
	var latest *store.DelegationEdge
	for _, e := range scoped {
		if e.Active {
			continue
		}
		if latest == nil || !edgeDeactivatedAt(e).Before(edgeDeactivatedAt(latest)) {
			latest = e
		}
	}
	return latest, len(scoped) > 0, nil
}

// edgeDeactivatedAt is when e was deactivated: its recorded time, or its
// last update for an edge deactivated before times were recorded.
func edgeDeactivatedAt(e *store.DelegationEdge) time.Time {
	if e.At != nil {
		return *e.At
	}
	return e.UpdatedAt
}

// principal describes the principal (kind, id) for an authority cause,
// with deleted set from the tombstone or a missing row for a user, and
// from a missing or soft-deleted row for an agent. since is when the
// principal was deleted, when that is known.
func (r *agentAuthorityResolver) principal(ctx context.Context, kind, id string) (*api.AgentAuthorityPrincipal, *time.Time, error) {
	p := &api.AgentAuthorityPrincipal{Kind: kind, ID: id}
	switch kind {
	case store.DelegationPrincipalUser:
		d, err := r.tombstone(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if d != nil {
			at := d.RequestedAt
			p.Deleted = true
			return p, &at, nil
		}
		if _, err := r.getUser(ctx, id); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return nil, nil, fmt.Errorf("provenance user lookup %s: %w", id, err)
			}
			p.Deleted = true
		}
	case store.DelegationPrincipalAgent:
		ag, err := r.getAgent(ctx, id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return nil, nil, fmt.Errorf("provenance agent lookup %s: %w", id, err)
			}
			p.Deleted = true
			return p, nil, nil
		}
		if !ag.DeletedAt.IsZero() {
			at := ag.DeletedAt
			p.Deleted = true
			return p, &at, nil
		}
	}
	return p, nil, nil
}

// resolve computes agent's authority state.
func (r *agentAuthorityResolver) resolve(ctx context.Context, agent *store.Agent) (api.AgentAuthority, error) {
	if agent == nil || agent.ID == "" {
		return api.AgentAuthority{}, ErrProvenanceRequest
	}
	if !agent.DeletedAt.IsZero() {
		return r.withoutActiveEdge(ctx, agent)
	}

	var trace provenanceChainTrace
	// The legacy flag lets the walk continue through unrecorded hops, so a
	// chain that is unrecorded in part reports a deleted delegator too.
	// The result is descriptive only.
	root, err := r.a.walkProvenanceChain(ctx, agent, true, r, &trace)
	switch {
	case err == nil:
		if root.RootUser.Status == store.UserStatusActive {
			if trace.unrecorded {
				return api.AgentAuthority{State: api.AgentAuthorityUnrecorded}, nil
			}
			return api.AgentAuthority{State: api.AgentAuthorityLive}, nil
		}
		p, since, err := r.principal(ctx, store.DelegationPrincipalUser, root.RootUser.ID)
		if err != nil {
			return api.AgentAuthority{}, err
		}
		return api.AgentAuthority{State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseDelegatorInactive, Principal: p, Since: since}, nil

	case errors.Is(err, ErrProvenanceMissing):
		if trace.depth == 0 {
			return r.withoutActiveEdge(ctx, agent)
		}
		return r.delegatorWithoutActiveEdge(ctx, agent, trace)

	case errors.Is(err, ErrProvenanceInactive):
		p, since, err := r.principal(ctx, trace.principal.Kind, trace.principal.ID)
		if err != nil {
			return api.AgentAuthority{}, err
		}
		// The walk reported the delegator missing or deleted.
		p.Deleted = true
		return api.AgentAuthority{State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseDelegatorDeleted, Principal: p, Since: since}, nil

	case errors.Is(err, ErrProvenanceUnrecorded),
		errors.Is(err, ErrProvenanceChain) && trace.sentinel:
		return api.AgentAuthority{State: api.AgentAuthorityUnrecorded}, nil

	case errors.Is(err, ErrProvenanceChain),
		errors.Is(err, ErrProvenanceAmbiguous),
		errors.Is(err, ErrProvenanceMismatch):
		return api.AgentAuthority{State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseChainInvalid}, nil

	default:
		return api.AgentAuthority{}, err
	}
}

// withoutActiveEdge is the state of an agent with no active project
// delegation: revoked with the recorded cause of the one deactivated
// last, or unrecorded when it has none or its cause was not recorded.
func (r *agentAuthorityResolver) withoutActiveEdge(ctx context.Context, agent *store.Agent) (api.AgentAuthority, error) {
	latest, _, err := r.latestDeactivatedProjectEdge(ctx, agent.ID, agent.ProjectID)
	if err != nil {
		return api.AgentAuthority{}, err
	}
	if latest == nil || latest.Cause == "" {
		return api.AgentAuthority{State: api.AgentAuthorityUnrecorded}, nil
	}
	p, _, err := r.principal(ctx, latest.DelegatorType, latest.DelegatorID)
	if err != nil {
		return api.AgentAuthority{}, err
	}
	at := edgeDeactivatedAt(latest)
	return api.AgentAuthority{State: api.AgentAuthorityRevoked, Cause: string(latest.Cause), Principal: p, Since: &at}, nil
}

// delegatorWithoutActiveEdge is the state of an agent whose chain reaches
// an agent delegator (trace.delegateID) with no active project
// delegation: orphaned (delegator_revoked) when that delegator had one,
// unrecorded when it never had any.
func (r *agentAuthorityResolver) delegatorWithoutActiveEdge(ctx context.Context, agent *store.Agent, trace provenanceChainTrace) (api.AgentAuthority, error) {
	latest, hasAny, err := r.latestDeactivatedProjectEdge(ctx, trace.delegateID, agent.ProjectID)
	if err != nil {
		return api.AgentAuthority{}, err
	}
	if !hasAny {
		return api.AgentAuthority{State: api.AgentAuthorityUnrecorded}, nil
	}
	p, _, err := r.principal(ctx, store.DelegationPrincipalAgent, trace.delegateID)
	if err != nil {
		return api.AgentAuthority{}, err
	}
	out := api.AgentAuthority{State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseDelegatorRevoked, Principal: p}
	if latest != nil {
		at := edgeDeactivatedAt(latest)
		out.Since = &at
	}
	return out, nil
}
