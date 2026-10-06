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

// holdFixture seeds a project with agents for the agent hold conformance
// tests.
type holdFixture struct {
	projectID string
	agents    []string
}

func seedHoldFixture(t *testing.T, ctx context.Context, s store.Store, agents int) holdFixture {
	t.Helper()
	f := holdFixture{projectID: uuid.NewString()}
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:   f.projectID,
		Name: "hold-project",
		Slug: "hold-project-" + f.projectID[:8],
	}))
	for i := 0; i < agents; i++ {
		f.agents = append(f.agents, seedProjectAgent(t, ctx, s, f.projectID, fmt.Sprintf("hold-%d", i), nil))
	}
	return f
}

// seedProjectAgent creates an agent in projectID, applying mutate (when
// non-nil) before the insert, and returns its ID.
func seedProjectAgent(t *testing.T, ctx context.Context, s store.Store, projectID, slug string, mutate func(*store.Agent)) string {
	t.Helper()
	id := uuid.NewString()
	a := &store.Agent{
		ID:        id,
		Slug:      slug + "-" + id[:8],
		Name:      slug,
		Template:  "default",
		ProjectID: projectID,
		Phase:     "running",
	}
	if mutate != nil {
		mutate(a)
	}
	require.NoError(t, s.CreateAgent(ctx, a))
	return id
}

func newHold(projectID, agentID, rootID string) *store.AgentHold {
	return &store.AgentHold{
		AgentID:           agentID,
		ProjectID:         projectID,
		Cause:             store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser,
		RootPrincipalID:   rootID,
		Trigger:           store.MembershipLossTriggerMemberRemove,
		ActorKind:         "user",
		ActorID:           uuid.NewString(),
		CorrelationID:     uuid.NewString(),
	}
}

// AgentHoldConformance verifies the AgentHoldStore contract against a
// store.Store backend.
func AgentHoldConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("agent_hold", func(t *testing.T) {
		t.Run("create is idempotent per agent and root while active", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 1)
			a := f.agents[0]
			rootU, rootV := uuid.NewString(), uuid.NewString()

			n, err := s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, rootU), newHold(f.projectID, a, rootV)})
			require.NoError(t, err)
			assert.Equal(t, 2, n)

			// Same (agent, root) again, with fresh IDs: nothing inserted.
			n, err = s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, rootU), newHold(f.projectID, a, rootV)})
			require.NoError(t, err)
			assert.Equal(t, 0, n)

			// A duplicate inside one batch inserts once.
			rootW := uuid.NewString()
			n, err = s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, rootW), newHold(f.projectID, a, rootW)})
			require.NoError(t, err)
			assert.Equal(t, 1, n)

			holds, err := s.ListActiveAgentHolds(ctx, a)
			require.NoError(t, err)
			require.Len(t, holds, 3)
			roots := map[string]int{}
			for _, h := range holds {
				roots[h.RootPrincipalID]++
				assert.Equal(t, a, h.AgentID)
				assert.Equal(t, f.projectID, h.ProjectID)
				assert.Equal(t, store.AgentHoldCauseOwnerAccessEnded, h.Cause)
				assert.Equal(t, store.MembershipLossTriggerMemberRemove, h.Trigger)
				assert.Nil(t, h.ClearedAt)
				assert.False(t, h.CreatedAt.IsZero())
			}
			assert.Equal(t, map[string]int{rootU: 1, rootV: 1, rootW: 1}, roots)
		})

		t.Run("fields round-trip", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 2)
			h := newHold(f.projectID, f.agents[1], uuid.NewString())
			h.ViaAgentID = f.agents[0]
			h.Trigger = store.MembershipLossTriggerGroupChange
			n, err := s.CreateAgentHolds(ctx, []*store.AgentHold{h})
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.NotEmpty(t, h.ID)

			holds, err := s.ListActiveAgentHolds(ctx, f.agents[1])
			require.NoError(t, err)
			require.Len(t, holds, 1)
			got := holds[0]
			assert.Equal(t, h.ID, got.ID)
			assert.Equal(t, f.agents[0], got.ViaAgentID)
			assert.Equal(t, store.DelegationPrincipalUser, got.RootPrincipalType)
			assert.Equal(t, h.RootPrincipalID, got.RootPrincipalID)
			assert.Equal(t, store.MembershipLossTriggerGroupChange, got.Trigger)
			assert.Equal(t, h.ActorKind, got.ActorKind)
			assert.Equal(t, h.ActorID, got.ActorID)
			assert.Equal(t, h.CorrelationID, got.CorrelationID)
		})

		t.Run("invalid holds are refused", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 1)
			for name, mutate := range map[string]func(*store.AgentHold){
				"unknown cause":   func(h *store.AgentHold) { h.Cause = "other" },
				"unknown trigger": func(h *store.AgentHold) { h.Trigger = "other" },
				"no root":         func(h *store.AgentHold) { h.RootPrincipalID = "" },
				"root not a UUID": func(h *store.AgentHold) { h.RootPrincipalID = "not-a-uuid" },
				"agent root":      func(h *store.AgentHold) { h.RootPrincipalType = store.DelegationPrincipalAgent },
				"no root type":    func(h *store.AgentHold) { h.RootPrincipalType = "" },
				"already cleared": func(h *store.AgentHold) { h.ClearReason = "x" },
				"missing agent":   func(h *store.AgentHold) { h.AgentID = uuid.NewString() },
			} {
				h := newHold(f.projectID, f.agents[0], uuid.NewString())
				mutate(h)
				_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{h})
				assert.ErrorIs(t, err, store.ErrInvalidInput, name)
			}
			held, err := s.HasActiveAgentHold(ctx, f.agents[0])
			require.NoError(t, err)
			assert.False(t, held)
		})

		t.Run("root principal ID is stored in canonical form", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 1)
			a := f.agents[0]
			root := uuid.New()
			n, err := s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, "{"+strings.ToUpper(root.String())+"}")})
			require.NoError(t, err)
			require.Equal(t, 1, n)
			// The canonical form names the same active hold.
			n, err = s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, root.String())})
			require.NoError(t, err)
			assert.Equal(t, 0, n)
			holds, err := s.ListActiveAgentHolds(ctx, a)
			require.NoError(t, err)
			require.Len(t, holds, 1)
			assert.Equal(t, root.String(), holds[0].RootPrincipalID)
		})

		t.Run("clear by a user ends the hold and keeps history", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 2)
			a, other := f.agents[0], f.agents[1]
			rootU := uuid.NewString()
			_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{
				newHold(f.projectID, a, rootU),
				newHold(f.projectID, a, uuid.NewString()),
				newHold(f.projectID, other, rootU),
			})
			require.NoError(t, err)

			held, err := s.HasActiveAgentHold(ctx, a)
			require.NoError(t, err)
			require.True(t, held)

			userID := uuid.NewString()
			n, err := s.ClearAgentHolds(ctx, a, store.ClearActor{Kind: store.ClearActorUser, ID: userID}, "resumed")
			require.NoError(t, err)
			assert.Equal(t, 2, n)

			held, err = s.HasActiveAgentHold(ctx, a)
			require.NoError(t, err)
			assert.False(t, held)
			holds, err := s.ListActiveAgentHolds(ctx, a)
			require.NoError(t, err)
			assert.Empty(t, holds)

			// Other agents are untouched.
			held, err = s.HasActiveAgentHold(ctx, other)
			require.NoError(t, err)
			assert.True(t, held)

			// A cleared row does not block a new active hold for the same
			// (agent, root).
			n, err = s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, rootU)})
			require.NoError(t, err)
			assert.Equal(t, 1, n)
		})

		t.Run("clear refuses a non-user actor", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 1)
			a := f.agents[0]
			_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, uuid.NewString())})
			require.NoError(t, err)

			for _, by := range []store.ClearActor{
				{Kind: "agent", ID: a},
				{Kind: "system", ID: "system"},
				{Kind: "", ID: uuid.NewString()},
				{Kind: store.ClearActorUser, ID: ""},
				{Kind: store.ClearActorUser, ID: "not-a-uuid"},
			} {
				n, err := s.ClearAgentHolds(ctx, a, by, "resumed")
				assert.ErrorIs(t, err, store.ErrInvalidActor, "actor %+v", by)
				assert.Equal(t, 0, n)
			}
			held, err := s.HasActiveAgentHold(ctx, a)
			require.NoError(t, err)
			assert.True(t, held, "a refused clear must leave the hold active")
		})

		t.Run("hard delete of the agent removes its holds", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 2)
			a, keep := f.agents[0], f.agents[1]
			rootU := uuid.NewString()
			_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, rootU), newHold(f.projectID, keep, rootU)})
			require.NoError(t, err)

			require.NoError(t, s.DeleteAgent(ctx, a))

			holds, err := s.ListActiveAgentHolds(ctx, a)
			require.NoError(t, err)
			assert.Empty(t, holds)
			page, err := s.ListActiveAgentHoldsByProject(ctx, f.projectID, store.ListOptions{})
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			assert.Equal(t, keep, page.Items[0].AgentID)
		})

		t.Run("soft delete keeps holds", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 1)
			a := f.agents[0]
			_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, uuid.NewString())})
			require.NoError(t, err)
			ag, err := s.GetAgent(ctx, a)
			require.NoError(t, err)
			ag.DeletedAt = time.Now()
			require.NoError(t, s.UpdateAgent(ctx, ag))
			ag, err = s.GetAgent(ctx, a)
			require.NoError(t, err)
			require.False(t, ag.DeletedAt.IsZero(), "the agent must be soft-deleted")
			held, err := s.HasActiveAgentHold(ctx, a)
			require.NoError(t, err)
			assert.True(t, held)
			holds, err := s.ListActiveAgentHolds(ctx, a)
			require.NoError(t, err)
			assert.Len(t, holds, 1)
		})

		t.Run("phase change keeps holds", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 1)
			a := f.agents[0]
			_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, uuid.NewString())})
			require.NoError(t, err)
			ag, err := s.GetAgent(ctx, a)
			require.NoError(t, err)
			ag.Phase = "stopped"
			require.NoError(t, s.UpdateAgent(ctx, ag))
			held, err := s.HasActiveAgentHold(ctx, a)
			require.NoError(t, err)
			assert.True(t, held)
		})

		t.Run("list by project paginates and is project-scoped", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 5)
			g := seedHoldFixture(t, ctx, s, 1)
			rootU := uuid.NewString()
			var holds []*store.AgentHold
			for _, a := range f.agents {
				holds = append(holds, newHold(f.projectID, a, rootU))
			}
			holds = append(holds, newHold(g.projectID, g.agents[0], rootU))
			_, err := s.CreateAgentHolds(ctx, holds)
			require.NoError(t, err)
			_, err = s.ClearAgentHolds(ctx, f.agents[4], store.ClearActor{Kind: store.ClearActorUser, ID: uuid.NewString()}, "resumed")
			require.NoError(t, err)

			seen := map[string]bool{}
			cursor := ""
			pages := 0
			for {
				page, err := s.ListActiveAgentHoldsByProject(ctx, f.projectID, store.ListOptions{Limit: 2, Cursor: cursor})
				require.NoError(t, err)
				pages++
				assert.LessOrEqual(t, len(page.Items), 2)
				for _, h := range page.Items {
					assert.Equal(t, f.projectID, h.ProjectID)
					assert.False(t, seen[h.AgentID], "agent listed twice")
					seen[h.AgentID] = true
				}
				if page.NextCursor == "" {
					break
				}
				cursor = page.NextCursor
				require.Less(t, pages, 10)
			}
			assert.Equal(t, 2, pages)
			assert.Len(t, seen, 4)
			assert.False(t, seen[f.agents[4]], "cleared holds are not listed")
		})

		t.Run("create participates in WithTx", func(t *testing.T) {
			s := factory(t)
			f := seedHoldFixture(t, ctx, s, 1)
			a := f.agents[0]
			errRollback := fmt.Errorf("rollback")
			err := s.WithTx(ctx, func(tx store.Store) error {
				n, err := tx.CreateAgentHolds(ctx, []*store.AgentHold{newHold(f.projectID, a, uuid.NewString())})
				require.NoError(t, err)
				require.Equal(t, 1, n)
				held, err := tx.HasActiveAgentHold(ctx, a)
				require.NoError(t, err)
				require.True(t, held)
				return errRollback
			})
			require.ErrorIs(t, err, errRollback)
			held, err := s.HasActiveAgentHold(ctx, a)
			require.NoError(t, err)
			assert.False(t, held)
		})
	})
}
