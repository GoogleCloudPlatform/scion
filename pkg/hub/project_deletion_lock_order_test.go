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
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/groupmembership"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectDeletionService_LockOrderNoDeadlock (ptone/scion#2769 PR4,
// review r3 L2; PostgreSQL only): the production project-delete path must
// lock the project's agent rows before its project-group cascade deletes any
// agent membership. Purge and finalize-hard lock an agent row FOR UPDATE,
// then delete the agent's group memberships, then the agent. If
// ProjectDeletionService deleted a project group (and with it the agent's
// membership) before locking the agent, it would hold the membership row
// while waiting for the agent row, and the purge/finalize side would wait on
// that membership: deadlock (SQLSTATE 40P01).
//
// T1 plays the purge/finalize side by hand, with the same statements: lock
// agent A FOR UPDATE, wait until the service is blocked on a lock, delete
// A's memberships, commit. Neither side may fail.
func TestProjectDeletionService_LockOrderNoDeadlock(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	const timeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	seedRoleDefinitions(ctx, cs)

	projectID := uuid.NewString()
	ownerID := uuid.NewString()
	createRS3Project(t, cs, projectID, ownerID)

	a := &store.Agent{ID: uuid.NewString(), Slug: "lock-a", Name: "lock-a", ProjectID: projectID, Phase: "running"}
	require.NoError(t, cs.CreateAgent(ctx, a))
	groupID := uuid.NewString()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID: groupID, Name: "lock-group", Slug: "lock-group-" + groupID[:8],
		GroupType: store.GroupTypeExplicit, ProjectID: projectID,
	}))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupID, MemberType: store.GroupMemberTypeAgent, MemberID: a.ID, Role: store.GroupMemberRoleMember,
	}))

	svc := NewProjectDeletionService(cs, NewAuthzService(cs, slog.Default()), slog.Default())
	actor := NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web")
	svcCtx := setTestIdentity(ctx, actor)

	// T1: the purge/finalize side. Lock agent A first.
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	t1Done := false
	defer func() {
		if !t1Done {
			_ = tx.Rollback()
		}
	}()
	aID := uuid.MustParse(a.ID)
	_, err = tx.Agent.Query().Where(agent.IDEQ(aID)).ForUpdate().IDs(ctx)
	require.NoError(t, err)

	// T2: the production project delete, which must wait for T1's lock on A.
	type outcome struct {
		result   *ProjectDeleteResult
		decision *ProjectDeleteDecision
	}
	done := make(chan outcome, 1)
	go func() {
		r, d := svc.Delete(svcCtx, ProjectDeleteRequest{ProjectID: projectID, Actor: actor})
		done <- outcome{r, d}
	}()

	// Wait until T2 is blocked on a lock. With the fix it blocks on
	// LockProjectAgents before the cascade; without it, it has already
	// deleted A's membership (via DeleteGroup) and blocks in DeleteProject.
	db := cs.DB()
	require.NotNil(t, db)
	require.Eventually(t, func() bool {
		var waiting int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			return false
		}
		return waiting > 0
	}, timeout/2, 20*time.Millisecond, "the project delete never waited on agent A's row lock")

	// T1 now deletes A's memberships, as purge and finalize-hard do after
	// locking the agent, and commits.
	_, t1Err := tx.GroupMembership.Delete().Where(groupmembership.AgentIDIn(aID)).Exec(ctx)
	if t1Err == nil {
		t1Err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	t1Done = true

	var got outcome
	select {
	case got = <-done:
	case <-time.After(timeout):
		t.Fatal("the project delete did not finish")
	}
	assert.NoError(t, t1Err, "the agent-first transaction must not fail (40P01 means a lock-order inversion)")
	if got.decision != nil {
		t.Errorf("project delete failed (40P01 means a lock-order inversion): %+v", *got.decision)
	}
	if t.Failed() {
		return
	}
	require.NotNil(t, got.result)
	assert.Equal(t, 1, got.result.CascadeSummary.Groups)
	_, err = cs.GetProject(ctx, projectID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.GetAgent(ctx, a.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}
