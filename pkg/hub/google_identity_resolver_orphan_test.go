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

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentOnBindRaceStore is raceExtIDStore whose first CreateExternalIdentity
// first runs onRace with the user the call binds: what a concurrent sign-in
// that acted through the provisioned row would have done before the
// resolver's orphan cleanup runs.
type agentOnBindRaceStore struct {
	*raceExtIDStore
	onRace func(userID string)
}

func (s *agentOnBindRaceStore) CreateExternalIdentity(ctx context.Context, b *store.ExternalIdentityBinding) error {
	if fn := s.onRace; fn != nil {
		s.onRace = nil
		fn(b.UserID)
	}
	return s.raceExtIDStore.CreateExternalIdentity(ctx, b)
}

// resolveLosingBindingRace resolves a new Gmail identity whose binding
// insert loses to a binding of the same subject for winner, running onRace
// first. It returns the resolved user.
func resolveLosingBindingRace(t *testing.T, srv *Server, s store.Store, winner *store.User, onRace func(userID string)) *store.User {
	t.Helper()
	identity := validGmailIdentity()
	identity.Email = "loser@gmail.com"
	ext := &agentOnBindRaceStore{
		raceExtIDStore: newRaceExtIDStore(&store.ExternalIdentityBinding{
			ID: tid("winner-binding"), Provider: "google", Issuer: googleCanonicalIssuer,
			Subject: identity.Subject, UserID: winner.ID, Email: winner.Email,
		}),
		onRace: onRace,
	}
	resolver := newTestResolver(s, ext, alwaysAuthorized, nil)
	resolver.SetOrphanUserCleanup(srv.cleanupOrphanedProvisionedUser)
	u, err := resolver.Resolve(context.Background(), identity, ResolvePolicy{})
	require.NoError(t, err)
	require.Equal(t, winner.ID, u.ID, "the resolution goes to the winner")
	return u
}

// The provisioned row that lost the binding race, with nothing attached,
// is deleted by the guarded cleanup.
func TestGoogleResolverOrphanCleanup_DeletesUnusedRow(t *testing.T) {
	srv, s := testServer(t)
	winner := newActiveMember(t, s, "user-winner", "winner@gmail.com")
	var loserID string
	resolveLosingBindingRace(t, srv, s, winner, func(userID string) { loserID = userID })
	require.NotEmpty(t, loserID)
	_, err := s.GetUser(context.Background(), loserID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the unused provisioned row is removed")
}

// A provisioned row that gained an agent before the cleanup ran (a
// concurrent sign-in acted through it) is kept, with its agent; deleting it
// would leave the agent with no user.
func TestGoogleResolverOrphanCleanup_KeepsRowThatGainedAnAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	winner := newActiveMember(t, s, "user-winner", "winner@gmail.com")
	projectID := pvSeedProject(t, s, "orphan-cleanup-project")
	var loserID string
	var agent *store.Agent
	resolveLosingBindingRace(t, srv, s, winner, func(userID string) {
		loserID = userID
		agent = createOwnedAgent(t, s, "raced-agent", projectID, userID)
	})
	u, err := s.GetUser(ctx, loserID)
	require.NoError(t, err, "a row an agent depends on is kept")
	assert.Equal(t, store.UserStatusActive, u.Status)
	assert.True(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero())
}

// A provisioned row that gained an active delegation to another user's
// agent is kept too.
func TestGoogleResolverOrphanCleanup_KeepsRowThatDelegates(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	winner := newActiveMember(t, s, "user-winner", "winner@gmail.com")
	projectID := pvSeedProject(t, s, "orphan-cleanup-edge-project")
	other := createOwnedAgent(t, s, "winner-agent", projectID, winner.ID)
	var loserID string
	var edge *store.DelegationEdge
	resolveLosingBindingRace(t, srv, s, winner, func(userID string) {
		loserID = userID
		edge = createUserEdge(t, s, userID, store.DelegationPrincipalAgent, other.ID, projectID)
	})
	_, err := s.GetUser(ctx, loserID)
	require.NoError(t, err)
	e, err := s.GetDelegationEdge(ctx, edge.ID)
	require.NoError(t, err)
	assert.True(t, e.Active, "the kept row's delegation is untouched")
}
