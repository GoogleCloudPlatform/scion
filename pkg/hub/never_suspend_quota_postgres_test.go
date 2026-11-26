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

//go:build integration

package hub

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestNeverSuspend_QuotaConcurrencyAndIsolation_Postgres exercises row-only
// max_never_suspend_agents_per_project enforcement on a real PostgreSQL
// database with a multi-connection pool:
//  1. K parallel PATCH auto->never calls at cap N-1 admit exactly one row and
//     leave the project at count N.
//  2. Across two Hub replicas sharing one Postgres database, holding
//     LockNeverSuspendQuota on replica A blocks replica B's never-suspend lock
//     with ErrQuotaLockContention, while a concurrent Reserve for
//     max_agents_per_project in the same project (LockQuotaEnforcement)
//     succeeds without lock contention.
func TestNeverSuspend_QuotaConcurrencyAndIsolation_Postgres(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t)
	open := func() *entadapter.CompositeStore {
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 10, MaxIdleConns: 5})
		require.NoError(t, err)
		return entadapter.NewCompositeStore(client)
	}

	srvA, sA := testServerWithStore(t, open())
	srvB, _ := testServerWithStore(t, open())
	setNeverSuspendExperiment(t, srvA, true)
	setNeverSuspendExperiment(t, srvB, true)

	ownerID := tid("ns-pg-owner")
	createTestUserWithRole(t, sA, ownerID, "ns-pg-owner@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	owner, err := sA.GetUser(ctx, ownerID)
	require.NoError(t, err)

	projectID := tid("ns-pg-project")
	require.NoError(t, sA.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "NS PG Project", Slug: "ns-pg-project",
		GitRemote: "https://github.com/example/ns-pg.git",
		OwnerID:   ownerID, CreatedBy: ownerID,
	}))
	createTestUserWithProjectRole(t, sA, ownerID, owner.Email, projectID, store.ProjectRoleOwner)

	// Set max_never_suspend_agents_per_project = 2 and max_agents_per_project = 20.
	nsDef, err := sA.GetLimitDefinitionByName(ctx, store.LimitMaxNeverSuspendAgentsPerProject)
	require.NoError(t, err)
	nsDef.DefaultValue = 2
	_, err = sA.UpdateLimitDefinition(ctx, nsDef)
	require.NoError(t, err)

	projDef, err := sA.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	projDef.DefaultValue = 20
	_, err = sA.UpdateLimitDefinition(ctx, projDef)
	require.NoError(t, err)

	t.Run("K parallel PATCH auto->never at cap N-1 admit exactly one on Postgres", func(t *testing.T) {
		// Seed 1 existing never row (cap=2, 1 slot remaining).
		require.NoError(t, sA.CreateAgent(ctx, &store.Agent{
			ID: tid("ns-pg-seed"), Slug: "ns-pg-seed", Name: "ns-pg-seed",
			ProjectID: projectID, OwnerID: ownerID, CreatedBy: ownerID,
			Phase: string(state.PhaseRunning), SuspendPolicy: store.SuspendPolicyNever,
		}))

		const K = 6
		agentIDs := make([]string, K)
		for i := 0; i < K; i++ {
			a := &store.Agent{
				ID: tid(fmt.Sprintf("ns-pg-pa-%d", i)), Slug: fmt.Sprintf("ns-pg-pa-%d", i),
				Name: fmt.Sprintf("ns-pg-pa-%d", i), ProjectID: projectID,
				OwnerID: ownerID, CreatedBy: ownerID,
				Phase: string(state.PhaseRunning), SuspendPolicy: store.SuspendPolicyAuto,
			}
			require.NoError(t, sA.CreateAgent(ctx, a))
			agentIDs[i] = a.ID
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var admitted, rejected atomic.Int32
		for i := 0; i < K; i++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				<-start
				rec := doRequestAsUser(t, srvA, owner, http.MethodPatch, "/api/v1/agents/"+id, map[string]any{
					"suspendPolicy": store.SuspendPolicyNever,
				})
				switch rec.Code {
				case http.StatusOK:
					admitted.Add(1)
				case http.StatusTooManyRequests:
					rejected.Add(1)
				default:
					t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
				}
			}(agentIDs[i])
		}
		close(start)
		wg.Wait()

		assert.Equal(t, int32(1), admitted.Load(), "exactly one parallel PATCH must be admitted")
		assert.Equal(t, int32(K-1), rejected.Load(), "remaining parallel PATCHes must get 429")
		cnt, err := sA.CountNeverSuspendAgentsInProject(ctx, projectID)
		require.NoError(t, err)
		assert.Equal(t, int64(2), cnt)
	})

	t.Run("LockNeverSuspendQuota isolates from max_agents_per_project Reserve across replicas", func(t *testing.T) {
		parked := make(chan struct{})
		release := make(chan struct{})
		errCh := make(chan error, 1)
		go func() {
			errCh <- srvA.withNeverSuspendQuotaLock(ctx, ownerID, projectID, func(int64) error {
				close(parked)
				<-release
				return nil
			})
		}()

		select {
		case <-parked:
		case <-time.After(15 * time.Second):
			close(release)
			t.Fatal("replica A did not acquire LockNeverSuspendQuota")
		}

		// Replica B's never-suspend lock on the same project must contend.
		errB := srvB.withNeverSuspendQuotaLock(ctx, ownerID, projectID, func(int64) error {
			return nil
		})
		require.ErrorIs(t, errB, ErrQuotaLockContention, "replica B must contend on LockNeverSuspendQuota")

		// Replica B's max_agents_per_project Reserve on the SAME project must NOT contend.
		created, resErr := srvB.quotaService.Reserve(ctx, store.LimitMaxAgentsPerProject, ownerID, store.QuotaScopeProject, projectID, tid("ns-pg-auto-agent"))
		require.NoError(t, resErr, "max_agents_per_project Reserve must not contend with LockNeverSuspendQuota")
		assert.True(t, created)

		close(release)
		require.NoError(t, <-errCh)
	})
}
