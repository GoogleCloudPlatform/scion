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

// PostgreSQL concurrency tests for membership loss processing
// (ptone/scion#3433). They run in the T1 PostgreSQL CI job
// (make test-launch-store-postgres) and skip elsewhere.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMSPostgresFixture is newMSFixture on a PostgreSQL store.
func newMSPostgresFixture(t *testing.T, name string) *msFixture {
	t.Helper()
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	srv, s := testServerWithStore(t, cs)
	srv.membershipService.onMembershipLoss = nil
	f := &msFixture{
		t:         t,
		srv:       srv,
		s:         s,
		projectID: tid("mspg-" + name + "-project"),
		ownerID:   tid("mspg-" + name + "-owner"),
		userID:    tid("mspg-" + name + "-user"),
	}
	createRS1Project(t, s, f.projectID, f.ownerID)
	f.addUser(f.userID)
	f.addMember(f.userID, store.ProjectRoleMember)
	f.agentA = f.userAgent("pga-"+name, f.userID)
	f.childC = f.childAgent("pgc-"+name, f.agentA)
	return f
}

// prepareRemoval drops the user's bindings and enqueues a check, without
// processing it.
func (f *msFixture) prepareRemoval() {
	f.t.Helper()
	f.dropBindings(f.userID)
	require.NoError(f.t, enqueueMembershipLossTx(context.Background(), f.s, f.userID, f.projectID, store.MembershipLossTriggerMemberRemove, AuditActor{}))
}

func assertNoDeadlock(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		assert.NotContains(t, err.Error(), "40P01", "lock-order inversion (deadlock)")
		assert.NotContains(t, strings.ToLower(err.Error()), "deadlock")
	}
}

// runConcurrently starts both functions together and waits for both.
func runConcurrently(t *testing.T, a, b func()) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; a() }()
	go func() { defer wg.Done(); <-start; b() }()
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent operations did not finish (deadlock?)")
	}
}

func TestMembershipLossProcessor_vs_ProjectDelete_Postgres(t *testing.T) {
	f := newMSPostgresFixture(t, "projdel")
	ctx := context.Background()
	f.prepareRemoval()
	svc := NewProjectDeletionService(f.s, f.srv.authzService, slog.Default())
	owner := NewAuthenticatedUser(f.ownerID, f.ownerID+"@test.com", "Owner", "member", "web")
	var decision *ProjectDeleteDecision
	runConcurrently(t,
		func() { f.srv.drainMembershipLossChecks(ctx) },
		func() {
			_, decision = svc.Delete(setTestIdentity(ctx, owner), ProjectDeleteRequest{ProjectID: f.projectID, Actor: owner})
		})
	if decision != nil {
		assert.NotContains(t, decision.Reason, "40P01")
	}
	_, err := f.s.GetProject(ctx, f.projectID)
	if err == nil {
		assert.True(t, f.held(f.agentA.ID), "project kept: the agents are held")
	}
}

func TestMembershipLossProcessor_vs_AgentHardDelete_Postgres(t *testing.T) {
	f := newMSPostgresFixture(t, "harddel")
	ctx := context.Background()
	f.prepareRemoval()
	var delErr error
	runConcurrently(t,
		func() { f.srv.drainMembershipLossChecks(ctx) },
		func() { delErr = f.s.DeleteAgent(ctx, f.childC.ID) })
	assertNoDeadlock(t, delErr)
	if _, err := f.s.GetAgent(ctx, f.childC.ID); err != nil {
		holds, lerr := f.s.ListActiveAgentHolds(ctx, f.childC.ID)
		require.NoError(t, lerr)
		assert.Empty(t, holds, "a hard-deleted agent has no hold rows")
	}
	assert.True(t, f.held(f.agentA.ID))
}

func TestMembershipLossProcessor_vs_ReAdd_Postgres(t *testing.T) {
	ctx := context.Background()
	t.Run("readd_first", func(t *testing.T) {
		f := newMSPostgresFixture(t, "readd1")
		f.prepareRemoval()
		f.addMember(f.userID, store.ProjectRoleMember)
		f.srv.drainMembershipLossChecks(ctx)
		assert.False(t, f.held(f.agentA.ID))
	})
	t.Run("processor_first", func(t *testing.T) {
		f := newMSPostgresFixture(t, "readd2")
		f.prepareRemoval()
		f.srv.drainMembershipLossChecks(ctx)
		f.addMember(f.userID, store.ProjectRoleMember)
		assert.True(t, f.held(f.agentA.ID))
	})
	t.Run("concurrent", func(t *testing.T) {
		f := newMSPostgresFixture(t, "readd3")
		f.prepareRemoval()
		runConcurrently(t,
			func() { f.srv.drainMembershipLossChecks(ctx) },
			func() { f.addMember(f.userID, store.ProjectRoleMember) })
		// Either order is consistent: no hold (re-add first) or a hold that
		// stays until an owner acts.
		_ = f.held(f.agentA.ID)
	})
}

func TestMembershipLossProcessor_vs_CredentialMint_Postgres(t *testing.T) {
	f := newMSPostgresFixture(t, "mint")
	ctx := context.Background()
	f.prepareRemoval()
	var tok string
	runConcurrently(t,
		func() { f.srv.drainMembershipLossChecks(ctx) },
		func() {
			t2, err := f.srv.agentTokenService.GenerateAgentToken(f.agentA.ID, f.projectID, ScopesForRole(AgentRoleFull), f.agentA.Ancestry)
			if err == nil {
				tok = t2
			}
		})
	require.True(t, f.held(f.agentA.ID))
	if tok != "" {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/agents/"+f.agentA.ID, nil, tok)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "a credential minted concurrently is never valid")
	}
}

func TestMembershipLossProcessor_vs_ChildCreate_Postgres(t *testing.T) {
	f := newMSPostgresFixture(t, "child")
	ctx := context.Background()
	f.prepareRemoval()
	var child *store.Agent
	runConcurrently(t,
		func() { f.srv.drainMembershipLossChecks(ctx) },
		func() { child = f.childAgent("pg-concurrent-child", f.agentA) })
	require.NotNil(t, child)
	if !f.held(child.ID) {
		require.Error(t, f.srv.agentStanding(ctx, child.ID), "a child committed after the walk is refused live")
		_, err := f.srv.membershipFullSweep(ctx)
		require.NoError(t, err)
		assert.True(t, f.held(child.ID), "the next sweep holds it")
	}
}

func TestMembershipLossProcessor_TwoInstances_Postgres(t *testing.T) {
	f := newMSPostgresFixture(t, "two")
	ctx := context.Background()
	f.prepareRemoval()
	runConcurrently(t,
		func() { f.srv.drainMembershipLossChecks(ctx) },
		func() { f.srv.drainMembershipLossChecks(ctx) })
	for _, a := range []*store.Agent{f.agentA, f.childC} {
		holds, err := f.s.ListActiveAgentHolds(ctx, a.ID)
		require.NoError(t, err)
		assert.Len(t, holds, 1, "exactly one active hold per (agent, root)")
	}
	assert.Equal(t, 1, countAudits(t, f.s, mutationTypeMembershipLossProcessed, f.userID), "one summary per claimed check")
}

func TestMembershipLossProcessor_vs_UserDelete_Postgres(t *testing.T) {
	f := newMSPostgresFixture(t, "userdel")
	ctx := context.Background()
	f.prepareRemoval()
	var code int
	runConcurrently(t,
		func() { f.srv.drainMembershipLossChecks(ctx) },
		func() { code = doRequest(t, f.srv, http.MethodDelete, "/api/v1/users/"+f.userID, nil).Code })
	assert.NotEqual(t, http.StatusInternalServerError, code, "no deadlock between the processor and user delete")
}

// On PostgreSQL the reconciler's lock keys are distinct advisory locks: all
// three can be held at once.
func TestMembershipReconciler_LocksCoexist_Postgres(t *testing.T) {
	f := newMSPostgresFixture(t, "locks")
	ctx := context.Background()
	locker, ok := f.s.(store.AdvisoryLocker)
	require.True(t, ok, "the PostgreSQL store takes advisory locks")
	var releases []func() error
	defer func() {
		for _, r := range releases {
			_ = r()
		}
	}()
	for _, key := range []store.AdvisoryLockKey{store.LockMembershipStandingSweep, store.LockMembershipExpiryScan, store.LockMembershipStopRetry} {
		acquired, release, err := locker.TryAdvisoryLock(ctx, key)
		require.NoError(t, err)
		require.True(t, acquired, "lock %x is free while the others are held", key)
		releases = append(releases, release)
	}
}
