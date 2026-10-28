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

// PostgreSQL concurrency tests for the forced user delete
// (ptone/scion#2143). CI runs them in the pkg/hub PostgreSQL integration
// shards (make test-hub-postgres-integration SHARD=K); locally, make
// test-launch-store-postgres runs them too. They skip without PostgreSQL.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newForceDelPostgresFixture is newForceDelFixture on a PostgreSQL store.
func newForceDelPostgresFixture(t *testing.T, name string) *forceDelFixture {
	t.Helper()
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	srv, s := testServerWithStore(t, cs)
	disp := &engineStubDispatcher{}
	srv.SetDispatcher(disp)
	srv.config.SoftDeleteRetention = time.Hour
	enableUserDeleteForce(t, srv)
	ctx := context.Background()
	project := &store.Project{ID: tid("proj-fdpg-" + name), Name: "FDPG " + name, Slug: "fdpg-" + name}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{ID: tid("broker-fdpg-" + name), Name: "FDPG Broker " + name, Slug: "fdpg-broker-" + name,
		Status: store.BrokerStatusOnline, Endpoint: "http://localhost:9800"}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	return &forceDelFixture{srv: srv, s: s, disp: disp, project: project, broker: broker,
		dave: newActiveMember(t, s, "user-fdpg-"+name, "fdpg-"+name+"@test.com")}
}

// A restore racing the forced delete's transaction is either counted by
// it (listed, or the delete refuses with blockers changed) or refused
// because the user is being deleted. It never leaves a live agent outside
// the deletion.
func TestForceDeleteUser_RestoreRacesRequest_Postgres(t *testing.T) {
	for i := 0; i < 5; i++ {
		f := newForceDelPostgresFixture(t, "race-req-"+string(rune('a'+i)))
		ctx := context.Background()
		f.agent(t, "pgr-live-"+string(rune('a'+i)), nil)
		gone := softDeletedAgent(t, f.s, "pgr-gone-"+string(rune('a'+i)), f.project.ID, f.dave.ID, []string{f.dave.ID})
		entered := make(chan struct{})
		release := make(chan struct{})
		f.disp.setFn(blockingDelete(entered, release, nil))

		var restoreCode, deleteCode int
		var deleteBody []byte
		runConcurrently(t,
			func() {
				restoreCode = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+gone.ID+"/restore", nil).Code
			},
			func() {
				rec := f.forceDelete(t)
				deleteCode, deleteBody = rec.Code, rec.Body.Bytes()
			})
		assertNoDeadlock(t, nil)

		switch deleteCode {
		case http.StatusAccepted:
			d, err := f.s.GetUserDeletionByUser(ctx, f.dave.ID)
			require.NoError(t, err)
			recs, err := f.s.ListUserDeletionAgents(ctx, d.ID)
			require.NoError(t, err)
			listed := false
			for _, r := range recs {
				listed = listed || r.AgentID == gone.ID
			}
			if restoreCode == http.StatusOK {
				assert.True(t, listed, "a restore that committed first is listed by the delete")
			} else {
				assert.Equal(t, http.StatusConflict, restoreCode)
				assert.False(t, mustGetAgent(t, f.s, gone.ID).DeletedAt.IsZero(), "a refused restore leaves the agent deleted")
			}
		case http.StatusConflict:
			assert.Contains(t, string(deleteBody), ErrCodeUserDeleteBlockersChanged)
			assert.Equal(t, http.StatusOK, restoreCode, "only a committed restore changes the blockers")
		default:
			t.Fatalf("unexpected delete answer %d: %s", deleteCode, deleteBody)
		}
		close(release)
	}
}

// A restore racing the completion is refused (the user is deleting or
// gone), and once the deletion completes no active edge has the user as
// the delegator.
func TestForceDeleteUser_RestoreRacesCompletion_Postgres(t *testing.T) {
	f := newForceDelPostgresFixture(t, "race-done")
	ctx := context.Background()
	gone := softDeletedAgent(t, f.s, "pgc-gone", f.project.ID, f.dave.ID, []string{f.dave.ID})
	_, _, release := startHeldDeletion(t, f)

	var restoreCode int
	runConcurrently(t,
		func() {
			restoreCode = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+gone.ID+"/restore", nil).Code
		},
		func() { close(release) })
	f.waitCompleted(t)
	assert.Equal(t, http.StatusConflict, restoreCode)
	assert.False(t, mustGetAgent(t, f.s, gone.ID).DeletedAt.IsZero())
	left, err := f.s.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, f.dave.ID)
	require.NoError(t, err)
	assert.Empty(t, left)
}

// Two hub processes completing the same deletion at once: exactly one
// completion takes effect, with one audit record, and no deadlock.
func TestForceDeleteUser_TwoReconcilersOneCompletion_Postgres(t *testing.T) {
	f := newForceDelPostgresFixture(t, "two")
	ctx := context.Background()
	d := stoppedDeletionReadyToComplete(t, f)

	srv2, _ := testServerOnMigratedStore(t, f.s)
	var c1, c2 bool
	var e1, e2 error
	runConcurrently(t,
		func() { c1, e1 = f.srv.completeUserDeletion(ctx, d) },
		func() { c2, e2 = srv2.completeUserDeletion(ctx, d) })
	assertNoDeadlock(t, e1)
	assertNoDeadlock(t, e2)
	assert.True(t, c1 != c2, "exactly one completion takes effect (got %v, %v; errors %v, %v)", c1, c2, e1, e2)
	assert.Len(t, auditsOf(t, f.s, "user_delete", f.dave.ID), 1)
	got, err := f.s.GetUserDeletion(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserDeletionStateCompleted, got.State)
}
