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

// Grant issuance racing a reincarnation claim (.design/agent-delegation.md
// §16.2, §17.2 #33): in either order, and concurrently, no unrevoked grant
// is left once both have settled. The issuance either is refused (the
// claim committed first) or its grant is revoked by the claim's hook (the
// issuance committed first). The _Postgres test runs the same cases on a
// PostgreSQL store with real row locks; it runs in the pkg/hub shard
// Postgres (integration) CI job and skips elsewhere.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// raceHookStore runs afterLock inside a transaction right after the agent
// row lock of an issuance (LockAgentRows), and afterClaim right after a
// reincarnation claim's compare-and-swap (ClaimAgentReincarnation), so a
// test can hold one transaction open while the other starts. Both hooks
// are optional and run at most once each.
type raceHookStore struct {
	store.Store
	mu         sync.Mutex
	afterLock  func()
	afterClaim func()
}

func (s *raceHookStore) take(hook *func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := *hook
	*hook = nil
	return h
}

func (s *raceHookStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error { return fn(&raceHookTx{Store: tx, parent: s}) })
}

type raceHookTx struct {
	store.Store
	parent *raceHookStore
}

func (t *raceHookTx) LockAgentRows(ctx context.Context, ids []string) error {
	if err := t.Store.LockAgentRows(ctx, ids); err != nil {
		return err
	}
	if h := t.parent.take(&t.parent.afterLock); h != nil {
		h()
	}
	return nil
}

func (t *raceHookTx) ClaimAgentReincarnation(ctx context.Context, agentID string, expectedVersion int64, at time.Time) (int64, error) {
	v, err := t.Store.ClaimAgentReincarnation(ctx, agentID, expectedVersion, at)
	if err != nil {
		return v, err
	}
	if h := t.parent.take(&t.parent.afterClaim); h != nil {
		h()
	}
	return v, nil
}

// issuanceClaimWorld is a project, an issuer and a way to make fresh
// agents owned by the issuer, on srv's store wrapped in a raceHookStore.
type issuanceClaimWorld struct {
	srv       *Server
	hooks     *raceHookStore
	projectID string
	issuer    *store.User
	n         int
}

func newIssuanceClaimWorld(t *testing.T, srv *Server, name string) *issuanceClaimWorld {
	t.Helper()
	w := &issuanceClaimWorld{srv: srv}
	w.hooks, _ = installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *raceHookStore {
		return &raceHookStore{Store: inner}
	})
	ctx := context.Background()
	project := &store.Project{ID: tid(name + "-project"), Name: name, Slug: name + "-project"}
	require.NoError(t, srv.store.CreateProject(ctx, project))
	w.projectID = project.ID
	w.issuer = hubMemberUser(t, srv.store, name+"-issuer")
	return w
}

// agent creates a fresh stopped agent owned by the issuer and returns the
// stored row.
func (w *issuanceClaimWorld) agent(t *testing.T) *store.Agent {
	t.Helper()
	ctx := context.Background()
	w.n++
	slug := fmt.Sprintf("race-%s-%d", tidSlugSafe(t.Name()), w.n)
	a := &store.Agent{
		ID: uuid.NewString(), Name: slug, Slug: slug, ProjectID: w.projectID,
		OwnerID: w.issuer.ID, Ancestry: []string{w.issuer.ID}, Phase: "stopped",
	}
	require.NoError(t, w.srv.store.CreateAgent(ctx, a))
	got, err := w.srv.store.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	return got
}

// issue runs the issuance transaction for snap, as handleCreateAgentDelegation
// does after its checks, attributed to the issuer as the issuance request
// would be, and returns the grant it tried to write.
func (w *issuanceClaimWorld) issue(snap store.Agent) (*store.AgentDelegationGrant, error) {
	ctx := context.Background()
	now := time.Now().UTC()
	grant := &store.AgentDelegationGrant{
		ID: uuid.NewString(), AgentID: snap.ID, AgentProjectID: snap.ProjectID, AgentGeneration: snap.Generation,
		AgentStateVersion: snap.StateVersion, IssuerUserID: w.issuer.ID, BoundaryKind: "hub",
		CeilingVersion: 1, CeilingPermissionIDs: []string{"agent.read"}, Name: "race",
		MaxCredentialTTLSeconds: agentDelegationCredentialDefaultTTL, ExpiresAt: now.Add(time.Hour),
		Created: now, IssuanceAuditID: uuid.NewString(),
	}
	err := w.srv.store.WithTx(ctx, func(tx store.Store) error {
		return writeAgentDelegationGrantTx(ctx, tx, &snap, grant, AuditActor{PrincipalKind: "user", PrincipalID: w.issuer.ID}, now)
	})
	return grant, err
}

// claim runs the reincarnation claim transaction for snap.
func (w *issuanceClaimWorld) claim(snap store.Agent) error {
	rec := &store.AgentReincarnation{
		AgentID:               snap.ID,
		FromGeneration:        snap.Generation,
		ToGeneration:          snap.Generation + 1,
		State:                 store.AgentReincarnationStatePending,
		PreviousAppliedConfig: snap.AppliedConfig,
	}
	return w.srv.reincarnateClaimTx(context.Background(), &snap, rec, nil, nil, AuditActor{})
}

// assertNoUsableGrant checks the settled outcome for agent: the claim
// committed, and either the issuance was refused and wrote no grant, or
// its grant is revoked with agent_reincarnated. It returns which.
func (w *issuanceClaimWorld) assertNoUsableGrant(t *testing.T, agentID string, issueErr, claimErr error) (issued bool) {
	t.Helper()
	assertNoDeadlock(t, issueErr)
	assertNoDeadlock(t, claimErr)
	require.NoError(t, claimErr, "the claim commits in either order")
	grants, err := w.srv.store.ListAgentDelegationGrants(context.Background(), store.AgentDelegationGrantFilter{AgentID: agentID})
	require.NoError(t, err)
	if issueErr != nil {
		require.ErrorIs(t, issueErr, errAgentDelegationAgentChanged, "an issuance after the claim is refused as agent_reincarnating")
		assert.Empty(t, grants, "a refused issuance writes no grant")
		return false
	}
	require.Len(t, grants, 1)
	require.NotNil(t, grants[0].RevokedAt, "a grant issued before the claim is revoked by it")
	assert.Equal(t, revokeReasonAgentReincarnated, grants[0].RevokeReason)
	return true
}

// raceHold is how long a hook keeps its transaction open after it has
// signalled, so the other transaction reaches its lock and waits.
const raceHold = 300 * time.Millisecond

func runIssuanceClaimRace(t *testing.T, srv *Server, name string) {
	w := newIssuanceClaimWorld(t, srv, name)

	t.Run("issuance then claim", func(t *testing.T) {
		snap := *w.agent(t)
		_, issueErr := w.issue(snap)
		require.NoError(t, issueErr)
		assert.True(t, w.assertNoUsableGrant(t, snap.ID, issueErr, w.claim(snap)))
	})

	t.Run("claim then issuance", func(t *testing.T) {
		snap := *w.agent(t)
		claimErr := w.claim(snap)
		_, issueErr := w.issue(snap)
		assert.False(t, w.assertNoUsableGrant(t, snap.ID, issueErr, claimErr))
	})

	t.Run("claim waits on an issuance holding the agent row", func(t *testing.T) {
		snap := *w.agent(t)
		locked := make(chan struct{})
		w.hooks.mu.Lock()
		w.hooks.afterLock = func() { close(locked); time.Sleep(raceHold) }
		w.hooks.mu.Unlock()
		var issueErr, claimErr error
		runConcurrently(t,
			func() { _, issueErr = w.issue(snap) },
			func() { <-locked; claimErr = w.claim(snap) })
		assert.True(t, w.assertNoUsableGrant(t, snap.ID, issueErr, claimErr), "the issuance committed first")
	})

	t.Run("issuance waits on a claim holding the agent row", func(t *testing.T) {
		snap := *w.agent(t)
		claimed := make(chan struct{})
		w.hooks.mu.Lock()
		w.hooks.afterClaim = func() { close(claimed); time.Sleep(raceHold) }
		w.hooks.mu.Unlock()
		var issueErr, claimErr error
		runConcurrently(t,
			func() { claimErr = w.claim(snap) },
			func() { <-claimed; _, issueErr = w.issue(snap) })
		assert.False(t, w.assertNoUsableGrant(t, snap.ID, issueErr, claimErr), "the claim committed first")
	})

	t.Run("started together", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			snap := *w.agent(t)
			var issueErr, claimErr error
			runConcurrently(t,
				func() { _, issueErr = w.issue(snap) },
				func() { claimErr = w.claim(snap) })
			w.assertNoUsableGrant(t, snap.ID, issueErr, claimErr)
		}
	})
}

func TestAgentDelegationIssuanceVsReincarnateClaim(t *testing.T) {
	srv, _ := testServer(t)
	runIssuanceClaimRace(t, srv, "adt-race")
}

func TestAgentDelegationIssuanceVsReincarnateClaim_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	srv, _ := testServerWithStore(t, entadapter.NewCompositeStore(enttest.NewClient(t)))
	runIssuanceClaimRace(t, srv, "adt-race-pg")
}
