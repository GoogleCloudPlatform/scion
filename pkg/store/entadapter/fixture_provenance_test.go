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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStamp() store.FixtureProvenance {
	return store.FixtureProvenance{RootID: uuid.NewString(), IssuerID: uuid.NewString()}
}

func newStampTestProject(t *testing.T, cs *CompositeStore, stamp store.FixtureProvenance) *store.Project {
	t.Helper()
	id := uuid.NewString()
	p := &store.Project{ID: id, Name: "stamp-" + id[:8], Slug: "stamp-" + id[:8], CreatedBy: uuid.NewString(), Fixture: stamp}
	require.NoError(t, cs.CreateProject(context.Background(), p))
	return p
}

// The four store creates write the stamp the model carries, and every read
// returns it; an empty stamp stays NULL.
func TestFixtureProvenance_CreateWritesAndReadsStamp(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	stamp := newStamp()
	proj := newStampTestProject(t, cs, stamp)
	gotProj, err := cs.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	assert.Equal(t, stamp, gotProj.Fixture)

	agentID := uuid.NewString()
	a := &store.Agent{ID: agentID, Slug: "a-" + agentID[:8], Name: "a-" + agentID[:8], ProjectID: proj.ID,
		Phase: "created", CreatedBy: stamp.RootID, OwnerID: stamp.RootID, Ancestry: []string{stamp.RootID}, Fixture: stamp}
	require.NoError(t, cs.CreateAgent(ctx, a))
	gotAgent, err := cs.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, stamp, gotAgent.Fixture)

	sc := &store.Schedule{ID: uuid.NewString(), ProjectID: proj.ID, Name: "s", CronExpr: "0 * * * *",
		EventType: "message", Fixture: stamp}
	require.NoError(t, cs.CreateSchedule(ctx, sc))
	gotSchedule, err := cs.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, stamp, gotSchedule.Fixture)

	gid := uuid.NewString()
	g := &store.Group{ID: gid, Name: "g", Slug: "g-" + gid[:8], Fixture: stamp}
	require.NoError(t, cs.CreateGroup(ctx, g))
	gotGroup, err := cs.GetGroup(ctx, gid)
	require.NoError(t, err)
	assert.Equal(t, stamp, gotGroup.Fixture)

	plain := newStampTestProject(t, cs, store.FixtureProvenance{})
	gotPlain, err := cs.GetProject(ctx, plain.ID)
	require.NoError(t, err)
	assert.True(t, gotPlain.Fixture.IsZero())
}

// A half-set or malformed stamp is refused at create and writes no row.
func TestFixtureProvenance_CreateRefusesPartialStamp(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	proj := newStampTestProject(t, cs, store.FixtureProvenance{})

	bad := map[string]store.FixtureProvenance{
		"root only":        {RootID: uuid.NewString()},
		"issuer only":      {IssuerID: uuid.NewString()},
		"malformed root":   {RootID: "not-a-uuid", IssuerID: uuid.NewString()},
		"malformed issuer": {RootID: uuid.NewString(), IssuerID: "not-a-uuid"},
	}
	for name, stamp := range bad {
		t.Run(name, func(t *testing.T) {
			id := uuid.NewString()
			p := &store.Project{ID: id, Name: "p-" + id[:8], Slug: "p-" + id[:8], Fixture: stamp}
			require.ErrorIs(t, cs.CreateProject(ctx, p), store.ErrInvalidInput)
			_, err := cs.GetProject(ctx, id)
			require.ErrorIs(t, err, store.ErrNotFound)

			a := &store.Agent{ID: uuid.NewString(), Slug: "a-" + id[:8], Name: "a-" + id[:8], ProjectID: proj.ID, Phase: "created", Fixture: stamp}
			require.ErrorIs(t, cs.CreateAgent(ctx, a), store.ErrInvalidInput)
			_, err = cs.GetAgent(ctx, a.ID)
			require.ErrorIs(t, err, store.ErrNotFound)

			sc := &store.Schedule{ID: uuid.NewString(), ProjectID: proj.ID, Name: "s", CronExpr: "0 * * * *", EventType: "message", Fixture: stamp}
			require.ErrorIs(t, cs.CreateSchedule(ctx, sc), store.ErrInvalidInput)

			g := &store.Group{ID: uuid.NewString(), Name: "g", Slug: "g-" + id[:8], Fixture: stamp}
			require.ErrorIs(t, cs.CreateGroup(ctx, g), store.ErrInvalidInput)
		})
	}
}

// UpdateAgent cannot change the stamp, created_by or ancestry; UpdateProject
// cannot change the stamp or created_by; UpdateGroup cannot change the
// stamp. Each update itself still applies.
func TestFixtureProvenance_UpdatesCannotChangeStampOrLineage(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	stamp := newStamp()
	proj := newStampTestProject(t, cs, stamp)
	origCreatedBy := proj.CreatedBy
	gotProj, err := cs.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	gotProj.Name = "renamed project"
	gotProj.Fixture = store.FixtureProvenance{}
	gotProj.CreatedBy = uuid.NewString()
	require.NoError(t, cs.UpdateProject(ctx, gotProj))
	gotProj, err = cs.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed project", gotProj.Name)
	assert.Equal(t, stamp, gotProj.Fixture, "UpdateProject must not clear the stamp")
	assert.Equal(t, origCreatedBy, gotProj.CreatedBy, "UpdateProject must not change created_by")

	// A legacy (unstamped) project cannot gain a stamp either.
	plain := newStampTestProject(t, cs, store.FixtureProvenance{})
	gotPlain, err := cs.GetProject(ctx, plain.ID)
	require.NoError(t, err)
	gotPlain.Fixture = newStamp()
	require.NoError(t, cs.UpdateProject(ctx, gotPlain))
	gotPlain, err = cs.GetProject(ctx, plain.ID)
	require.NoError(t, err)
	assert.True(t, gotPlain.Fixture.IsZero(), "UpdateProject must not add a stamp")

	agentID := uuid.NewString()
	creator := uuid.NewString()
	a := &store.Agent{ID: agentID, Slug: "u-" + agentID[:8], Name: "u-" + agentID[:8], ProjectID: proj.ID,
		Phase: "created", CreatedBy: creator, OwnerID: creator, Ancestry: []string{creator}, Fixture: stamp}
	require.NoError(t, cs.CreateAgent(ctx, a))
	got, err := cs.GetAgent(ctx, agentID)
	require.NoError(t, err)
	got.TaskSummary = "updated"
	got.Fixture = newStamp()
	got.CreatedBy = uuid.NewString()
	got.Ancestry = []string{uuid.NewString()}
	require.NoError(t, cs.UpdateAgent(ctx, got))
	got, err = cs.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, "updated", got.TaskSummary)
	assert.Equal(t, stamp, got.Fixture, "UpdateAgent must not change the stamp")
	assert.Equal(t, creator, got.CreatedBy, "UpdateAgent must not change created_by")
	assert.Equal(t, []string{creator}, got.Ancestry, "UpdateAgent must not change ancestry")

	gid := uuid.NewString()
	g := &store.Group{ID: gid, Name: "g", Slug: "ug-" + gid[:8], Fixture: stamp}
	require.NoError(t, cs.CreateGroup(ctx, g))
	gotGroup, err := cs.GetGroup(ctx, gid)
	require.NoError(t, err)
	gotGroup.Name = "renamed group"
	gotGroup.Fixture = store.FixtureProvenance{}
	require.NoError(t, cs.UpdateGroup(ctx, gotGroup))
	gotGroup, err = cs.GetGroup(ctx, gid)
	require.NoError(t, err)
	assert.Equal(t, "renamed group", gotGroup.Name)
	assert.Equal(t, stamp, gotGroup.Fixture, "UpdateGroup must not change the stamp")
}

// The stamp is on the resource row, so it survives deletion of the test
// identity's user row.
func TestFixtureProvenance_StampSurvivesFixtureUserDelete(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	issuer := &store.User{ID: uuid.NewString(), Email: "issuer-" + uuid.NewString()[:8] + "@example.com", DisplayName: "Issuer", Role: store.UserRoleMember}
	require.NoError(t, cs.CreateUser(ctx, issuer))
	fx := newFixtureUser(issuer.ID, time.Now().Add(time.Hour))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, fx))
	stamp := store.FixtureProvenance{RootID: fx.ID, IssuerID: issuer.ID}

	proj := newStampTestProject(t, cs, stamp)
	agentID := uuid.NewString()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "s-" + agentID[:8], Name: "s-" + agentID[:8],
		ProjectID: proj.ID, Phase: "created", CreatedBy: fx.ID, OwnerID: fx.ID, Ancestry: []string{fx.ID}, Fixture: stamp}))

	require.NoError(t, cs.DeleteUser(ctx, fx.ID))
	_, err := cs.GetUser(ctx, fx.ID)
	require.ErrorIs(t, err, store.ErrNotFound)

	gotAgent, err := cs.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, stamp, gotAgent.Fixture)
	gotProj, err := cs.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	assert.Equal(t, stamp, gotProj.Fixture)
}
