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

// Secret-value reads by an agent whose chain fails the standing check above
// the agent (ptone/scion#3433). The runtime precheck leaves these refusals
// to the per-item checks; each row reads a project secret value through
// /api/v1/agent/secrets and the single-key GET and must be refused per item
// with the not-found response shape.

import (
	"context"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// materialChainAgent creates an agent in f's project with an ancestry and,
// when delegatorType is set, a recorded edge from (delegatorType,
// delegatorID); it returns a recorded token able to read project secrets.
func materialChainAgent(t *testing.T, f *materialFixture, name string, ancestry []string, delegatorType, delegatorID string) (string, string) {
	t.Helper()
	id := tid("mmc-" + name)
	owner := ancestry[0]
	if len(ancestry) > 1 {
		owner = ancestry[len(ancestry)-1]
	}
	require.NoError(t, f.Store.CreateAgent(context.Background(), &store.Agent{
		ID: id, Slug: "mmc-" + name, Name: "mmc-" + name, ProjectID: f.ProjectID, Phase: "running",
		OwnerID: owner, CreatedBy: owner, Ancestry: ancestry,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}))
	if delegatorType != "" {
		seedRecordedDelegationEdge(t, f.Store, delegatorType, delegatorID, store.DelegationPrincipalAgent, id,
			store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	}
	token := mintRecordedMaterialToken(t, f.Server, f.Store, id, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, ancestry)
	return id, token
}

func requireChainStandingReason(t *testing.T, f *materialFixture, agentID, reason string) {
	t.Helper()
	err := f.Server.agentStanding(context.Background(), agentID)
	requireStandingReason(t, err, reason)
	require.True(t, standingRefusalLeftToItems(err), "the precheck leaves %s to the per-item checks", reason)
}

func TestMaterialChainStanding_HeldParent(t *testing.T) {
	f := newMaterialFixture(t, "mmc-held")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "MMC_HELD_KEY", "v", "", "", f.ProjectID)
	parent, _ := materialChainAgent(t, f, "held-parent", []string{f.UserID}, store.DelegationPrincipalUser, f.UserID)
	child, token := materialChainAgent(t, f, "held-child", []string{f.UserID, parent}, store.DelegationPrincipalAgent, parent)
	_, err := f.Store.CreateAgentHolds(context.Background(), []*store.AgentHold{{
		AgentID: parent, ProjectID: f.ProjectID, Cause: store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: f.UserID,
		Trigger: store.MembershipLossTriggerMemberRemove, ActorKind: "system", ActorID: "hub", CorrelationID: "test",
	}})
	require.NoError(t, err)
	requireChainStandingReason(t, f, child, standingReasonChainHeld)
	assertProjectDenied(t, f, child, token, "MMC_HELD_KEY")
}

func TestMaterialChainStanding_DeletedParent(t *testing.T) {
	f := newMaterialFixture(t, "mmc-deleted")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "MMC_DELETED_KEY", "v", "", "", f.ProjectID)
	parent, _ := materialChainAgent(t, f, "deleted-parent", []string{f.UserID}, store.DelegationPrincipalUser, f.UserID)
	child, token := materialChainAgent(t, f, "deleted-child", []string{f.UserID, parent}, store.DelegationPrincipalAgent, parent)
	p, err := f.Store.GetAgent(context.Background(), parent)
	require.NoError(t, err)
	p.DeletedAt = p.Updated.Add(1)
	require.NoError(t, f.Store.UpdateAgent(context.Background(), p))
	requireChainStandingReason(t, f, child, standingReasonChainDeleted)
	assertProjectDenied(t, f, child, token, "MMC_DELETED_KEY")
}

func TestMaterialChainStanding_BrokenChain(t *testing.T) {
	f := newMaterialFixture(t, "mmc-broken")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "MMC_BROKEN_KEY", "v", "", "", f.ProjectID)
	parent, _ := materialChainAgent(t, f, "broken-parent", []string{f.UserID}, "", "") // no edge
	child, token := materialChainAgent(t, f, "broken-child", []string{f.UserID, parent}, store.DelegationPrincipalAgent, parent)
	requireChainStandingReason(t, f, child, standingReasonChainBroken)
	assertProjectDenied(t, f, child, token, "MMC_BROKEN_KEY")
}

// The deepest agent sits at walk depth 12 (a direct child of the user is
// depth 1): one past the chain bound shared by the standing check and the
// delegation ceiling (an agent at depth 11 is still within it).
func TestMaterialChainStanding_BeyondDepthBound(t *testing.T) {
	f := newMaterialFixture(t, "mmc-depth")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "MMC_DEPTH_KEY", "v", "", "", f.ProjectID)
	ancestry := []string{f.UserID}
	delegatorType, delegatorID := store.DelegationPrincipalUser, f.UserID
	var id, token string
	for i := 1; i <= standingMaxChainDepth+2; i++ {
		id, token = materialChainAgent(t, f, fmt.Sprintf("depth-%d", i), append([]string{}, ancestry...), delegatorType, delegatorID)
		ancestry = append(ancestry, id)
		delegatorType, delegatorID = store.DelegationPrincipalAgent, id
	}
	requireChainStandingReason(t, f, id, standingReasonChainTooDeep)
	assertProjectDenied(t, f, id, token, "MMC_DEPTH_KEY")
}

func TestMaterialChainStanding_MissingRootUser(t *testing.T) {
	f := newMaterialFixture(t, "mmc-noroot")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "MMC_NOROOT_KEY", "v", "", "", f.ProjectID)
	deleg := tid("mmc-noroot-delegator")
	createDCUser(t, f.Store, deleg, "mmc-noroot-delegator@test.com", f.ProjectID, store.ProjectRoleOwner)
	child, token := materialChainAgent(t, f, "noroot-child", []string{f.UserID}, store.DelegationPrincipalUser, deleg)
	require.NoError(t, f.Store.DeleteUser(context.Background(), deleg))
	requireChainStandingReason(t, f, child, standingReasonRootMissing)
	assertProjectDenied(t, f, child, token, "MMC_NOROOT_KEY")
}
