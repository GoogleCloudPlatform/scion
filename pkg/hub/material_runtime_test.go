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

// Package hub — the two delegating-agent-state characterizations that are
// pinned to this file by name, so a follow-up change can reference them
// directly: a retained soft-deleted parent or grandparent still resolves in
// the delegation ceiling and is followed at its stored role. A follow-up
// change changes or removes these characterizations when it lands the
// shared non-deleted-source rule in the chain evaluation, and adds its own
// acceptance regressions (which deny) in their place.
package hub

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestAgentSecretRead_ProjectScopeSoftDeletedParentDenies: a retained
// soft-deleted parent agent supplies no delegation authority in the shared
// chain evaluation, so the child's runtime secret read is refused through
// the concealed 404 path. Runtime material reads add no second, separate
// delegation traversal; the rule comes from the delegation ceiling.
func TestAgentSecretRead_ProjectScopeSoftDeletedParentDenies(t *testing.T) {
	f := newMaterialFixture(t, "soft-deleted-parent")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "SOFT_DEL_PARENT_KEY", "v", "", "", f.ProjectID)

	parentID := tid("soft-deleted-parent-agent")
	createDCAgent(t, f.Store, parentID, f.ProjectID, f.UserID, AgentRoleFull)
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, parentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	childID := tid("child-of-soft-deleted-parent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: childID, Slug: "child-soft-deleted-parent", Name: "child", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, parentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalAgent, parentID, store.DelegationPrincipalAgent, childID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	childToken, err := f.Server.agentTokenService.GenerateAgentToken(childID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, parentID})
	require.NoError(t, err)

	parent, err := f.Store.GetAgent(ctx, parentID)
	require.NoError(t, err)
	parent.DeletedAt = time.Now()
	require.NoError(t, f.Store.UpdateAgent(ctx, parent))

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+childID+"/secrets/SOFT_DEL_PARENT_KEY", nil, childToken)
	require.Equal(t, http.StatusNotFound, rec.Code, "retained soft-deleted parent supplies no authority: %s", rec.Body.String())
	require.NotContains(t, rec.Body.String(), "denied_by", "the concealed 404 carries no denial detail")
}

// TestAgentSecretRead_ProjectScopeSoftDeletedGrandparentDenies: a retained
// soft-deleted grandparent supplies no delegation authority at a deeper link,
// even with a live immediate parent.
func TestAgentSecretRead_ProjectScopeSoftDeletedGrandparentDenies(t *testing.T) {
	f := newMaterialFixture(t, "soft-deleted-grandparent")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "SOFT_DEL_GRANDPARENT_KEY", "v", "", "", f.ProjectID)

	grandparentID := tid("soft-deleted-grandparent")
	createDCAgent(t, f.Store, grandparentID, f.ProjectID, f.UserID, AgentRoleFull)
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, grandparentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	parentID := tid("parent-of-soft-deleted-grandparent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: parentID, Slug: "parent-soft-gp", Name: "parent", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, grandparentID},
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		Created:       time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalAgent, grandparentID, store.DelegationPrincipalAgent, parentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	childID := tid("child-of-parent-soft-gp")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: childID, Slug: "child-soft-gp", Name: "child", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, grandparentID, parentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalAgent, parentID, store.DelegationPrincipalAgent, childID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	childToken, err := f.Server.agentTokenService.GenerateAgentToken(childID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, grandparentID, parentID})
	require.NoError(t, err)

	grandparent, err := f.Store.GetAgent(ctx, grandparentID)
	require.NoError(t, err)
	grandparent.DeletedAt = time.Now()
	require.NoError(t, f.Store.UpdateAgent(ctx, grandparent))

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+childID+"/secrets/SOFT_DEL_GRANDPARENT_KEY", nil, childToken)
	require.Equal(t, http.StatusNotFound, rec.Code, "retained soft-deleted grandparent supplies no authority: %s", rec.Body.String())
	require.NotContains(t, rec.Body.String(), "denied_by", "the concealed 404 carries no denial detail")
}
