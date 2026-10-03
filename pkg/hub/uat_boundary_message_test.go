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

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lineageAgent creates a lineage-mode agent in projectID with the given
// ancestry. Only ancestry users and the project owner may message it.
func lineageAgent(t *testing.T, s store.Store, projectID, suffix string, ancestry ...string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:          tid("uat-msg-agent-" + suffix),
		Slug:        "uat-msg-agent-" + suffix,
		Name:        "UAT Message Agent " + suffix,
		ProjectID:   projectID,
		Phase:       string(state.PhaseRunning),
		Ancestry:    ancestry,
		MessageMode: store.MessageModeLineage,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

func messageToken(t *testing.T, userID string, boundary TokenBoundary, selectors ...string) *ScopedUserIdentity {
	t.Helper()
	return NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(userID), boundary, selectors, tid("uat-msg-cred-"+userID+"-"+string(boundary.Kind)), bearerCeiling(t, selectors...), nil)
}

// TestUserMessage_ProjectBoundaryAppliesBeforeAncestryAndOwner pins that a
// project-boundary UAT cannot message an agent in another project, through
// ancestry or project ownership, while the same user's session can.
func TestUserMessage_ProjectBoundaryAppliesBeforeAncestryAndOwner(t *testing.T) {
	f := newBearerFixture(t, "msg-proj")
	ctx := context.Background()
	uatpMember(t, f.store, f.projectA, f.ownerB)
	inAncestry := lineageAgent(t, f.store, f.projectB, "msg-proj-ancestry", f.ownerB)
	ownedOnly := lineageAgent(t, f.store, f.projectB, "msg-proj-owner", tid("msg-proj-other-root"))

	token := messageToken(t, f.ownerB, projectBoundary(f.projectA), "agent:message")
	session := bearerUser(f.ownerB)

	for name, target := range map[string]*store.Agent{"ancestry": inAncestry, "project owner": ownedOnly} {
		allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, session, target, false)
		require.True(t, allowed, "%s: the session may message the agent: %s", name, reason)

		allowed, reason, _ = f.srv.authorizeAgentMessage(ctx, token, target, false)
		assert.False(t, allowed, "%s: a token bound to another project cannot message the agent", name)
		assert.Equal(t, ReasonMissingPermission, mapReasonToCode(reason), "%s: %s", name, reason)
	}

	own := messageToken(t, f.ownerB, projectBoundary(f.projectB), "agent:message")
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, own, inAncestry, false)
	assert.True(t, allowed, "a token bound to the agent's project may message through ancestry: %s", reason)
}

// TestUserMessage_HubBoundaryRequiresCurrentProjectAccess pins that a
// hub-boundary UAT messages an agent through ancestry or ownership only
// while its holder currently has access to the agent's project.
func TestUserMessage_HubBoundaryRequiresCurrentProjectAccess(t *testing.T) {
	f := newBearerFixture(t, "msg-hub")
	ctx := context.Background()
	retained := lineageAgent(t, f.store, f.projectB, "msg-hub-retained", f.ownerA)

	token := messageToken(t, f.ownerA, hubBoundary(), "agent:message")
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, token, retained, false)
	assert.False(t, allowed, "retained ancestry without project access does not admit a hub token")
	assert.Equal(t, ReasonMissingPermission, mapReasonToCode(reason), reason)

	uatpMember(t, f.store, f.projectB, f.ownerA)
	allowed, reason, _ = f.srv.authorizeAgentMessage(ctx, token, retained, false)
	assert.True(t, allowed, "with project access, ancestry admits the hub token: %s", reason)

	uatpDeleteProjectBinding(t, f.store, f.ownerA, f.projectB)
	allowed, _, _ = f.srv.authorizeAgentMessage(ctx, token, retained, false)
	assert.False(t, allowed, "removing project access denies the next message")

	ownerToken := messageToken(t, f.ownerB, hubBoundary(), "agent:message")
	ownedOnly := lineageAgent(t, f.store, f.projectB, "msg-hub-owner", tid("msg-hub-other-root"))
	allowed, reason, _ = f.srv.authorizeAgentMessage(ctx, ownerToken, ownedOnly, false)
	assert.True(t, allowed, "the project owner's hub token pierces through ownership: %s", reason)
}

// TestUserMessage_UATCeilingRequiresMessagePermission pins that a UAT whose
// ceiling lacks agent.message cannot message an agent, through ancestry or
// ownership, for either boundary kind.
func TestUserMessage_UATCeilingRequiresMessagePermission(t *testing.T) {
	f := newBearerFixture(t, "msg-ceiling")
	ctx := context.Background()
	target := lineageAgent(t, f.store, f.projectA, "msg-ceiling", f.ownerA)
	for name, boundary := range map[string]TokenBoundary{"hub": hubBoundary(), "project": projectBoundary(f.projectA)} {
		token := messageToken(t, f.ownerA, boundary, "agent:read")
		allowed, _, _ := f.srv.authorizeAgentMessage(ctx, token, target, false)
		assert.False(t, allowed, "%s boundary: a ceiling without agent.message cannot message", name)
	}
}

// TestUserMessage_InvalidBoundaryDenied pins that a UAT with an invalid
// boundary never messages an agent, even one whose ancestry names its
// holder.
func TestUserMessage_InvalidBoundaryDenied(t *testing.T) {
	f := newBearerFixture(t, "msg-invalid")
	ctx := context.Background()
	target := lineageAgent(t, f.store, f.projectA, "msg-invalid", f.ownerA)
	for name, boundary := range map[string]TokenBoundary{
		"empty":               {},
		"project without ID":  {Kind: BoundaryKindProject},
		"hub with project ID": {Kind: BoundaryKindHub, ProjectID: f.projectA},
	} {
		token := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.ownerA), boundary, []string{"agent:message"}, tid("msg-invalid-cred"), bearerCeiling(t, "agent:message"), nil)
		allowed, _, _ := f.srv.authorizeAgentMessage(ctx, token, target, false)
		assert.False(t, allowed, "%s boundary cannot message", name)
	}
}
