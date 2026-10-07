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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The production create paths write an agent with a resolvable root on
// their own: with the test store's default-owner hook off, a user create, an
// agent's child create and a scheduled create each produce a row whose chain
// resolves to the creating user (ptone/scion#3433).
func TestProductionCreatePathsWriteResolvableRoot(t *testing.T) {
	testAgentOwnerHookDisabled.Store(true)
	t.Cleanup(func() { testAgentOwnerHookDisabled.Store(false) })

	f := bypassAgentsSetup(t)
	ctx := context.Background()
	// As every agent row after the delegation-edge backfill, the calling
	// agent carries an edge from its owner; it may create agents.
	ms := &msFixture{t: t, srv: f.srv, s: f.store, projectID: f.proj.ID}
	ms.edge(store.DelegationPrincipalUser, f.owner.ID, f.caller)
	f.caller.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}
	require.NoError(t, f.store.UpdateAgent(ctx, f.caller))

	requireRoot := func(t *testing.T, slug string) {
		t.Helper()
		a, err := f.store.GetAgentBySlug(ctx, f.proj.ID, slug)
		require.NoError(t, err)
		assert.True(t, a.OwnerID != "" || a.CreatedBy != "" || len(a.Ancestry) > 0,
			"the create path recorded no owner, creator or ancestry")
		root, err := f.srv.resolveChainRoot(ctx, f.store, a, true)
		require.NoError(t, err)
		assert.Equal(t, f.owner.ID, root)
	}

	t.Run("user_create", func(t *testing.T) {
		rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "root-user-create"})
		require.Less(t, rec.Code, 300, rec.Body.String())
		requireRoot(t, "root-user-create")
	})
	t.Run("agent_child_create", func(t *testing.T) {
		rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
			CreateAgentRequest{Name: "root-child-create"}, ScopesForRole(AgentRoleFull)...)
		require.Less(t, rec.Code, 300, rec.Body.String())
		requireRoot(t, "root-child-create")
	})
	t.Run("scheduled_create", func(t *testing.T) {
		err := f.srv.dispatchAgentEventHandler()(ctx, store.ScheduledEvent{
			ID: "evt-root", ProjectID: f.proj.ID, EventType: "dispatch_agent",
			Payload: `{"agentName":"root-sched-create","task":"t"}`, CreatedBy: f.caller.ID,
		})
		require.NoError(t, err)
		requireRoot(t, "root-sched-create")
	})
}
