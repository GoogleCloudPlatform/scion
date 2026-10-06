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

package storetest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// descFixture is a project, a second project and a root user for the
// delegation descendant conformance tests.
type descFixture struct {
	s       store.Store
	project string
	other   string
	user    string
	seq     int
}

func newDescFixture(t *testing.T, ctx context.Context, s store.Store) *descFixture {
	t.Helper()
	f := &descFixture{s: s, project: uuid.NewString(), other: uuid.NewString(), user: uuid.NewString()}
	for _, p := range []string{f.project, f.other} {
		require.NoError(t, s.CreateProject(ctx, &store.Project{ID: p, Name: "desc-" + p[:8], Slug: "desc-" + p[:8]}))
	}
	return f
}

// agent creates an agent in project with the given legacy links.
func (f *descFixture) agent(t *testing.T, ctx context.Context, project string, mutate func(*store.Agent)) string {
	t.Helper()
	f.seq++
	return seedProjectAgent(t, ctx, f.s, project, fmt.Sprintf("desc-%d", f.seq), mutate)
}

// edge records a delegation edge from (fromType, fromID) to agent to in
// project scope.
func (f *descFixture) edge(t *testing.T, ctx context.Context, fromType, fromID, to, project string, d store.Deactivation) {
	t.Helper()
	e := &store.DelegationEdge{
		DelegatorType: fromType,
		DelegatorID:   fromID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    to,
		ScopeType:     "project",
		ScopeID:       project,
		Role:          "full",
		Active:        d.Cause == "",
		Deactivation:  d,
	}
	require.NoError(t, f.s.CreateDelegationEdge(ctx, e))
}

func (f *descFixture) userEdge(t *testing.T, ctx context.Context, to string) {
	f.edge(t, ctx, store.DelegationPrincipalUser, f.user, to, f.project, store.Deactivation{})
}

func (f *descFixture) agentEdge(t *testing.T, ctx context.Context, from, to string) {
	f.edge(t, ctx, store.DelegationPrincipalAgent, from, to, f.project, store.Deactivation{})
}

func (f *descFixture) query() store.DescendantQuery {
	return store.DescendantQuery{RootType: store.DelegationPrincipalUser, RootID: f.user, ProjectID: f.project}
}

func refsByID(refs []store.DescendantRef) map[string]store.DescendantRef {
	out := make(map[string]store.DescendantRef, len(refs))
	for _, r := range refs {
		out[r.AgentID] = r
	}
	return out
}

func refIDs(refs []store.DescendantRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.AgentID
	}
	return out
}

// listDescendants runs the walk under a deadline, so a walk that does not
// terminate fails the test instead of hanging it.
func listDescendants(t *testing.T, s store.Store, q store.DescendantQuery) (store.DescendantResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type out struct {
		res store.DescendantResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := s.ListDelegationDescendants(ctx, q)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-ctx.Done():
		t.Fatalf("ListDelegationDescendants did not return: %v", ctx.Err())
		return store.DescendantResult{}, nil
	}
}

// DelegationDescendantsConformance verifies ListDelegationDescendants
// against a store.Store backend.
func DelegationDescendantsConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("delegation_descendants", func(t *testing.T) {
		t.Run("user to agent to child by edges", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			a := f.agent(t, ctx, f.project, nil)
			child := f.agent(t, ctx, f.project, nil)
			f.userEdge(t, ctx, a)
			f.agentEdge(t, ctx, a, child)

			res, err := listDescendants(t, s, f.query())
			require.NoError(t, err)
			require.Len(t, res.Agents, 2)
			assert.Equal(t, store.DescendantRef{AgentID: a, Depth: 1, Link: store.DescendantLinkEdge, EdgeActive: true}, res.Agents[0])
			assert.Equal(t, store.DescendantRef{AgentID: child, ViaID: a, Depth: 2, Link: store.DescendantLinkEdge, EdgeActive: true}, res.Agents[1])
		})

		t.Run("scheduled child by edge and by created_by only", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			a := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.OwnerID = f.user; ag.CreatedBy = f.user })
			f.userEdge(t, ctx, a)
			// A scheduled child with its own edge from the user (the
			// schedule's revision principal).
			byEdge := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.CreatedBy = f.user })
			f.userEdge(t, ctx, byEdge)
			// A scheduled child with no edge and no owner, created by a.
			byCreator := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.CreatedBy = a })

			res, err := listDescendants(t, s, f.query())
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{a, byEdge}, refIDs(res.Agents), "without legacy links only edges are followed")

			q := f.query()
			q.LegacyLinks = true
			res, err = listDescendants(t, s, q)
			require.NoError(t, err)
			got := refsByID(res.Agents)
			require.Len(t, got, 3)
			assert.Equal(t, store.DescendantLinkEdge, got[a].Link)
			assert.Equal(t, store.DescendantLinkEdge, got[byEdge].Link)
			// Legacy links carry no edge state.
			assert.Equal(t, store.DescendantRef{AgentID: byCreator, ViaID: a, Depth: 2, Link: store.DescendantLinkCreatedBy}, got[byCreator])
		})

		t.Run("owner and ancestry links", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			owned := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.OwnerID = f.user })
			otherOwner := uuid.NewString()
			// Ancestry applies only to agents without an owner.
			seeded := f.agent(t, ctx, f.project, func(ag *store.Agent) {
				ag.Ancestry = []string{f.user}
			})
			f.agent(t, ctx, f.project, func(ag *store.Agent) {
				ag.OwnerID = otherOwner
				ag.Ancestry = []string{f.user}
			})
			ownedChild := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.OwnerID = owned; ag.CreatedBy = otherOwner })
			// Owned by someone else and created by the user: owner wins.
			f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.OwnerID = otherOwner; ag.CreatedBy = f.user })

			q := f.query()
			q.LegacyLinks = true
			res, err := listDescendants(t, s, q)
			require.NoError(t, err)
			got := refsByID(res.Agents)
			require.Len(t, got, 3)
			assert.Equal(t, store.DescendantRef{AgentID: owned, Depth: 1, Link: store.DescendantLinkOwner}, got[owned])
			assert.Equal(t, store.DescendantRef{AgentID: seeded, Depth: 1, Link: store.DescendantLinkAncestry}, got[seeded])
			assert.Equal(t, store.DescendantRef{AgentID: ownedChild, ViaID: owned, Depth: 2, Link: store.DescendantLinkOwner}, got[ownedChild])
		})

		t.Run("soft-deleted agents via agent_soft_delete edges", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			live := f.agent(t, ctx, f.project, nil)
			gone := f.agent(t, ctx, f.project, nil)
			hard := f.agent(t, ctx, f.project, nil)
			f.userEdge(t, ctx, live)
			now := time.Now()
			f.edge(t, ctx, store.DelegationPrincipalAgent, live, gone, f.project, store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &now, OpID: uuid.NewString()})
			f.edge(t, ctx, store.DelegationPrincipalAgent, live, hard, f.project, store.Deactivation{Cause: store.EdgeDeactivationAgentHardDelete, At: &now, OpID: uuid.NewString()})
			require.NoError(t, s.DeleteAgent(ctx, hard))
			ag, err := s.GetAgent(ctx, gone)
			require.NoError(t, err)
			ag.DeletedAt = now
			require.NoError(t, s.UpdateAgent(ctx, ag))
			// A soft-deleted legacy child of the user.
			legacyGone := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.OwnerID = f.user })
			lg, err := s.GetAgent(ctx, legacyGone)
			require.NoError(t, err)
			lg.DeletedAt = now
			require.NoError(t, s.UpdateAgent(ctx, lg))

			q := f.query()
			q.LegacyLinks = true
			res, err := listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Equal(t, []string{live}, refIDs(res.Agents))

			q.IncludeSoftDeleted = true
			res, err = listDescendants(t, s, q)
			require.NoError(t, err)
			got := refsByID(res.Agents)
			require.Len(t, got, 3, "a hard-deleted agent is never returned")
			assert.Equal(t, store.DescendantRef{
				AgentID: gone, ViaID: live, Depth: 2, Link: store.DescendantLinkEdge,
				EdgeActive: false, EdgeDeactivationCause: store.EdgeDeactivationAgentSoftDelete,
			}, got[gone])
			assert.Equal(t, store.DescendantLinkOwner, got[legacyGone].Link)
		})

		t.Run("the walk continues below deleted agents", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			now := time.Now()
			// user -> softChild (soft-deleted) -> softGrand, and
			// user -> hardChild (hard-deleted) -> hardGrand; no ancestry.
			softChild := f.agent(t, ctx, f.project, nil)
			hardChild := f.agent(t, ctx, f.project, nil)
			softGrand := f.agent(t, ctx, f.project, nil)
			hardGrand := f.agent(t, ctx, f.project, nil)
			ownedGrand := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.OwnerID = softChild })
			f.edge(t, ctx, store.DelegationPrincipalUser, f.user, softChild, f.project, store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &now, OpID: uuid.NewString()})
			f.edge(t, ctx, store.DelegationPrincipalUser, f.user, hardChild, f.project, store.Deactivation{Cause: store.EdgeDeactivationAgentHardDelete, At: &now, OpID: uuid.NewString()})
			f.agentEdge(t, ctx, softChild, softGrand)
			f.agentEdge(t, ctx, hardChild, hardGrand)
			ag, err := s.GetAgent(ctx, softChild)
			require.NoError(t, err)
			ag.DeletedAt = now
			require.NoError(t, s.UpdateAgent(ctx, ag))
			require.NoError(t, s.DeleteAgent(ctx, hardChild))

			for _, includeSoftDeleted := range []bool{false, true} {
				q := f.query()
				q.IncludeSoftDeleted = includeSoftDeleted
				res, err := listDescendants(t, s, q)
				require.NoError(t, err)
				got := refsByID(res.Agents)
				assert.Equal(t, store.DescendantRef{AgentID: softGrand, ViaID: softChild, Depth: 2, Link: store.DescendantLinkEdge, EdgeActive: true}, got[softGrand], "includeSoftDeleted=%v", includeSoftDeleted)
				assert.Equal(t, store.DescendantRef{AgentID: hardGrand, ViaID: hardChild, Depth: 2, Link: store.DescendantLinkEdge, EdgeActive: true}, got[hardGrand], "includeSoftDeleted=%v", includeSoftDeleted)
				assert.NotContains(t, got, hardChild, "a hard-deleted agent is never returned")
				_, softReturned := got[softChild]
				assert.Equal(t, includeSoftDeleted, softReturned, "a soft-deleted agent is returned only with IncludeSoftDeleted")

				q.LegacyLinks = true
				res, err = listDescendants(t, s, q)
				require.NoError(t, err)
				got = refsByID(res.Agents)
				assert.Equal(t, store.DescendantRef{AgentID: ownedGrand, ViaID: softChild, Depth: 2, Link: store.DescendantLinkOwner}, got[ownedGrand], "includeSoftDeleted=%v", includeSoftDeleted)
				assert.Contains(t, got, softGrand)
				assert.Contains(t, got, hardGrand)
			}
		})

		t.Run("purged agents are expanded but never returned", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			deletedAt := time.Now().Add(-time.Hour)
			purged := f.agent(t, ctx, f.project, nil)
			grand := f.agent(t, ctx, f.project, nil)
			f.edge(t, ctx, store.DelegationPrincipalUser, f.user, purged, f.project, store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &deletedAt, OpID: uuid.NewString()})
			f.agentEdge(t, ctx, purged, grand)
			ag, err := s.GetAgent(ctx, purged)
			require.NoError(t, err)
			ag.DeletedAt = deletedAt
			require.NoError(t, s.UpdateAgent(ctx, ag))
			n, err := s.PurgeDeletedAgents(ctx, time.Now())
			require.NoError(t, err)
			require.GreaterOrEqual(t, n, 1)
			_, err = s.GetAgent(ctx, purged)
			require.ErrorIs(t, err, store.ErrNotFound)

			q := f.query()
			q.IncludeSoftDeleted = true
			q.SkipHeldForRoot = true
			res, err := listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Equal(t, []string{grand}, refIDs(res.Agents), "the purged agent is not returned; the agent below it is")

			var holds []*store.AgentHold
			for _, r := range res.Agents {
				holds = append(holds, newHold(f.project, r.AgentID, f.user))
			}
			n, err = s.CreateAgentHolds(ctx, holds)
			require.NoError(t, err, "every returned agent can be held")
			assert.Equal(t, len(holds), n)

			res, err = listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Empty(t, res.Agents)
		})

		t.Run("a soft-deleted agent with an active edge is filtered on its row", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			x := f.agent(t, ctx, f.project, nil)
			child := f.agent(t, ctx, f.project, nil)
			f.userEdge(t, ctx, x)
			f.agentEdge(t, ctx, x, child)
			ag, err := s.GetAgent(ctx, x)
			require.NoError(t, err)
			ag.DeletedAt = time.Now()
			require.NoError(t, s.UpdateAgent(ctx, ag))

			res, err := listDescendants(t, s, f.query())
			require.NoError(t, err)
			assert.Equal(t, []string{child}, refIDs(res.Agents))

			q := f.query()
			q.IncludeSoftDeleted = true
			res, err = listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Equal(t, []string{x, child}, refIDs(res.Agents))
			assert.True(t, res.Agents[0].EdgeActive)
		})

		t.Run("edge links keep their state with ancestry as the create path writes it", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			// The create path sets owner_id and created_by to the creator
			// and ancestry to the creator chain.
			created := func(creator string, ancestry []string) string {
				return f.agent(t, ctx, f.project, func(ag *store.Agent) {
					ag.OwnerID = creator
					ag.CreatedBy = creator
					ag.Ancestry = ancestry
				})
			}
			a := created(f.user, []string{f.user})
			f.userEdge(t, ctx, a)
			b := created(a, []string{f.user, a})
			f.agentEdge(t, ctx, a, b)
			c := created(b, []string{f.user, a, b})
			f.agentEdge(t, ctx, b, c)
			// moved was created by a, then its ownership moved to another
			// user, with a new edge from that user.
			newOwner := uuid.NewString()
			moved := created(a, []string{f.user, a})
			mv, err := s.GetAgent(ctx, moved)
			require.NoError(t, err)
			mv.OwnerID = newOwner
			require.NoError(t, s.UpdateAgent(ctx, mv))
			f.edge(t, ctx, store.DelegationPrincipalUser, newOwner, moved, f.project, store.Deactivation{})

			q := f.query()
			q.LegacyLinks = true
			res, err := listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Equal(t, []store.DescendantRef{
				{AgentID: a, Depth: 1, Link: store.DescendantLinkEdge, EdgeActive: true},
				{AgentID: b, ViaID: a, Depth: 2, Link: store.DescendantLinkEdge, EdgeActive: true},
				{AgentID: c, ViaID: b, Depth: 3, Link: store.DescendantLinkEdge, EdgeActive: true},
			}, res.Agents, "the moved agent is not reached from the old root")
		})

		t.Run("IDs are compared in canonical form", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			byEdge := f.agent(t, ctx, f.project, nil)
			f.userEdge(t, ctx, byEdge)
			owned := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.OwnerID = f.user })
			seeded := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.Ancestry = []string{f.user} })
			// An agent reached by an edge written with an upper-case
			// delegate ID and by its owner link is returned once.
			both := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.OwnerID = byEdge })
			f.edge(t, ctx, store.DelegationPrincipalAgent, byEdge, strings.ToUpper(both), f.project, store.Deactivation{})

			q := f.query()
			q.LegacyLinks = true
			want, err := listDescendants(t, s, q)
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{byEdge, owned, seeded, both}, refIDs(want.Agents))
			got := refsByID(want.Agents)
			assert.Equal(t, store.DescendantRef{AgentID: both, ViaID: byEdge, Depth: 2, Link: store.DescendantLinkEdge, EdgeActive: true}, got[both])

			for _, root := range []string{"{" + f.user + "}", strings.ToUpper(f.user), "urn:uuid:" + f.user} {
				q := f.query()
				q.LegacyLinks = true
				q.RootID = root
				q.ProjectID = strings.ToUpper(f.project)
				res, err := listDescendants(t, s, q)
				require.NoError(t, err, root)
				assert.Equal(t, want.Agents, res.Agents, "root %s", root)
			}
		})

		t.Run("a hold for the root ID skips the agent whatever the root type", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			root := f.agent(t, ctx, f.project, nil)
			held := f.agent(t, ctx, f.project, nil)
			below := f.agent(t, ctx, f.project, nil)
			f.agentEdge(t, ctx, root, held)
			f.agentEdge(t, ctx, held, below)
			// Holds always record a user root; the walk matches holds on
			// the root ID, the same key the hold insert uses.
			n, err := s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.project, held, root)})
			require.NoError(t, err)
			require.Equal(t, 1, n)

			q := f.query()
			q.RootType, q.RootID = store.DelegationPrincipalAgent, root
			q.SkipHeldForRoot = true
			res, err := listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Equal(t, []string{below}, refIDs(res.Agents))
		})

		t.Run("other-project edges and agents are excluded", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			a := f.agent(t, ctx, f.project, nil)
			f.userEdge(t, ctx, a)
			elsewhere := f.agent(t, ctx, f.other, func(ag *store.Agent) { ag.OwnerID = f.user })
			f.edge(t, ctx, store.DelegationPrincipalUser, f.user, elsewhere, f.other, store.Deactivation{})
			elsewhereChild := f.agent(t, ctx, f.other, func(ag *store.Agent) { ag.OwnerID = a })
			f.edge(t, ctx, store.DelegationPrincipalAgent, a, elsewhereChild, f.other, store.Deactivation{})

			q := f.query()
			q.LegacyLinks = true
			res, err := listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Equal(t, []string{a}, refIDs(res.Agents))
		})

		t.Run("cycles terminate", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			// a and b delegate to each other: a -> b by an active edge,
			// b -> a by an agent_soft_delete edge and by owner_id.
			a := f.agent(t, ctx, f.project, nil)
			b := f.agent(t, ctx, f.project, func(ag *store.Agent) { ag.OwnerID = a })
			f.userEdge(t, ctx, a)
			f.agentEdge(t, ctx, a, b)
			now := time.Now()
			f.edge(t, ctx, store.DelegationPrincipalAgent, b, a, f.project, store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &now, OpID: uuid.NewString()})
			ag, err := s.GetAgent(ctx, a)
			require.NoError(t, err)
			ag.OwnerID = b
			require.NoError(t, s.UpdateAgent(ctx, ag))

			q := f.query()
			q.LegacyLinks = true
			q.IncludeSoftDeleted = true
			res, err := listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Equal(t, []string{a, b}, refIDs(res.Agents))

			// An agent root inside the cycle is never returned.
			q.RootType, q.RootID = store.DelegationPrincipalAgent, a
			res, err = listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Equal(t, []string{b}, refIDs(res.Agents))
		})

		t.Run("max depth returns the limit error with the nodes found", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			chain := make([]string, 4)
			for i := range chain {
				chain[i] = f.agent(t, ctx, f.project, nil)
				if i == 0 {
					f.userEdge(t, ctx, chain[i])
				} else {
					f.agentEdge(t, ctx, chain[i-1], chain[i])
				}
			}
			q := f.query()
			q.MaxDepth = 2
			res, err := listDescendants(t, s, q)
			require.ErrorIs(t, err, store.ErrDescendantLimit)
			assert.Equal(t, chain[:2], refIDs(res.Agents))

			q.MaxDepth = 4
			res, err = listDescendants(t, s, q)
			require.NoError(t, err, "a tree exactly MaxDepth deep is not over the limit")
			assert.Equal(t, chain, refIDs(res.Agents))
		})

		t.Run("max nodes returns the limit error with the nodes found", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			var kids []string
			for i := 0; i < 5; i++ {
				k := f.agent(t, ctx, f.project, nil)
				f.userEdge(t, ctx, k)
				kids = append(kids, k)
			}
			q := f.query()
			q.MaxNodes = 3
			res, err := listDescendants(t, s, q)
			require.ErrorIs(t, err, store.ErrDescendantLimit)
			assert.Len(t, res.Agents, 3)

			q.MaxNodes = 5
			res, err = listDescendants(t, s, q)
			require.NoError(t, err, "exactly MaxNodes descendants is not over the limit")
			assert.ElementsMatch(t, kids, refIDs(res.Agents))
		})

		t.Run("repeated calls with holds advance through the tree", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			// 10 agents (2.5 x MaxNodes): three children of the user, then
			// two more levels.
			var all []string
			mk := func(parent string) string {
				id := f.agent(t, ctx, f.project, nil)
				if parent == "" {
					f.userEdge(t, ctx, id)
				} else {
					f.agentEdge(t, ctx, parent, id)
				}
				all = append(all, id)
				return id
			}
			a1, a2 := mk(""), mk("")
			mk("")
			b1 := mk(a1)
			mk(a1)
			mk(a1)
			mk(a2)
			mk(a2)
			mk(b1)
			mk(b1)
			require.Len(t, all, 10)

			// A hold for a different root does not skip an agent.
			other := newHold(f.project, a1, uuid.NewString())
			_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{other})
			require.NoError(t, err)

			q := f.query()
			q.MaxNodes = 4
			q.SkipHeldForRoot = true
			seen := map[string]bool{}
			calls := 0
			for {
				calls++
				require.LessOrEqual(t, calls, 5, "the walk must make progress")
				res, err := listDescendants(t, s, q)
				if err != nil {
					require.ErrorIs(t, err, store.ErrDescendantLimit)
					require.Len(t, res.Agents, 4)
				}
				var holds []*store.AgentHold
				for _, r := range res.Agents {
					assert.False(t, seen[r.AgentID], "agent %s returned twice", r.AgentID)
					seen[r.AgentID] = true
					holds = append(holds, newHold(f.project, r.AgentID, f.user))
				}
				n, herr := s.CreateAgentHolds(ctx, holds)
				require.NoError(t, herr)
				require.Equal(t, len(holds), n)
				if err == nil {
					break
				}
			}
			assert.Equal(t, 3, calls)
			assert.Len(t, seen, len(all))
			for _, id := range all {
				assert.True(t, seen[id], "agent %s never returned", id)
			}

			// Everything is held now: an empty result and no error.
			res, err := listDescendants(t, s, q)
			require.NoError(t, err)
			assert.Empty(t, res.Agents)
		})

		t.Run("invalid queries are refused", func(t *testing.T) {
			s := factory(t)
			f := newDescFixture(t, ctx, s)
			for name, mutate := range map[string]func(*store.DescendantQuery){
				"root type": func(q *store.DescendantQuery) { q.RootType = "group" },
				"root id":   func(q *store.DescendantQuery) { q.RootID = "" },
				"root uuid": func(q *store.DescendantQuery) { q.RootID = "not-a-uuid" },
				"agent root uuid": func(q *store.DescendantQuery) {
					q.RootType, q.RootID = store.DelegationPrincipalAgent, "agent-1"
				},
				"project":    func(q *store.DescendantQuery) { q.ProjectID = "not-a-uuid" },
				"no project": func(q *store.DescendantQuery) { q.ProjectID = "" },
				"neg depth":  func(q *store.DescendantQuery) { q.MaxDepth = -1 },
				"neg nodes":  func(q *store.DescendantQuery) { q.MaxNodes = -1 },
			} {
				q := f.query()
				mutate(&q)
				_, err := s.ListDelegationDescendants(ctx, q)
				assert.ErrorIs(t, err, store.ErrInvalidInput, name)
			}
		})
	})
}
