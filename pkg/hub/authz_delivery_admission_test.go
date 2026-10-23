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

// Tests for hub_delivery admission (ptone/scion#2228 part 2b): the
// relationship stage-4 source check and the step-10 admission walk.

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubDelivery_Stage4ParityForOtherPrincipals pins relationship stage 4
// (relationshipSourceStage) for every principal type other than
// hubDeliveryIdentity. The expected outcomes are literals recorded before
// the hub_delivery stage-4 arm existed, and the test passes unchanged after
// it: stage 4 for these principals is the owner-activity check followed by
// the owner's delegation-chain check, exactly as before.
func TestHubDelivery_Stage4ParityForOtherPrincipals(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	suspendedID := tid("hd-parity-suspended-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: suspendedID, Email: "suspended@golden.test", DisplayName: "Suspended",
		Role: "member", Status: store.UserStatusSuspended,
	}))

	newAgent := func(id string, ancestry []string, deleted bool) {
		a := &store.Agent{
			ID: id, Slug: "slug-" + id[:8], Name: "name-" + id[:8],
			ProjectID: f.projectAlpha.ID, Phase: string(state.PhaseRunning),
			OwnerID: ancestry[0], CreatedBy: ancestry[0], Ancestry: ancestry,
		}
		if deleted {
			a.DeletedAt = time.Now()
		}
		require.NoError(t, f.store.CreateAgent(ctx, a))
	}

	// An agent owner whose single recorded principal edge comes from an
	// active super-admin: the owner's delegation chain holds every
	// permission.
	liveAgentOwner := tid("hd-parity-live-agent-owner")
	newAgent(liveAgentOwner, []string{f.superAdminID}, false)
	seedRecordedDelegationEdge(t, f.store, store.DelegationPrincipalUser, f.superAdminID, store.DelegationPrincipalAgent,
		liveAgentOwner, store.RoleScopeProject, f.projectAlpha.ID, string(AgentRoleFull))

	deletedAgentOwner := tid("hd-parity-deleted-agent-owner")
	newAgent(deletedAgentOwner, []string{f.superAdminID}, true)

	suspendedRootAgentOwner := tid("hd-parity-suspended-root-agent-owner")
	newAgent(suspendedRootAgentOwner, []string{suspendedID}, false)

	sources := []struct {
		name       string
		ownerID    string
		wantHolds  bool
		wantDetail string
	}{
		{"user_owner_active", f.projectOwnerID, true, ""},
		{"user_owner_suspended", suspendedID, false, "sharing source owner is not active"},
		{"owner_empty", "", false, "sharing source has no owner"},
		{"owner_unknown", tid("hd-parity-unknown-owner"), false, "sharing source owner not found"},
		{"agent_owner_live", liveAgentOwner, true, ""},
		{"agent_owner_deleted", deletedAgentOwner, false, "sharing source owner agent is deleted"},
		{"agent_owner_root_suspended", suspendedRootAgentOwner, false, "sharing source owner agent's root user is not active"},
	}

	owner := NewAuthenticatedUser(f.projectOwnerID, "owner@golden.test", "Owner", "member", "web")
	storedAgent, err := f.store.GetAgent(ctx, f.agentAlpha.ID)
	require.NoError(t, err)
	principals := map[string]PrincipalContext{
		"user":         principalContextForIdentity(owner),
		"agent_jwt":    principalContextForIdentity(progenyPairAgent(tid("hd-parity-jwt"), f.projectAlpha.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())),
		"stored_agent": principalContextForIdentity(&storedAgentIdentity{agent: storedAgent}),
	}

	resource := Resource{Type: "secret", ID: f.secretID}
	for pName, principal := range principals {
		for _, src := range sources {
			for _, action := range []Action{ActionDeliver, ActionRead} {
				t.Run(pName+"/"+src.name+"/"+string(action), func(t *testing.T) {
					s := &SharingSource{Kind: "secret", ID: f.secretID, OwnerID: src.ownerID, Policy: SharingPolicyOptInRequired, OptedIn: true}
					holds, detail := f.authz.relationshipSourceStage(ctx, principal, s, resource, action, "secret.deliver")
					assert.Equal(t, src.wantHolds, holds, "detail %q", detail)
					assert.Equal(t, src.wantDetail, detail)
				})
			}
		}
	}
}
