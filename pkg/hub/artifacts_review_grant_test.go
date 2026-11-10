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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// The shared fixture for agent-owned artifact tests: a home project with a
// delegating user, an owner, an admin and a member, and agents delegated
// through recorded edges. (The review-only grant path, design D24, it was
// first written for is removed, ptone/scion#5145.)

type reviewGrantFixture struct {
	srv                    *Server
	s                      store.Store
	home, other            string
	delegator, admin       string
	owner, member, outside string
	otherAdmin             string
	agent, child           *store.Agent
	noEdge                 *store.Agent
	sibling                *store.Agent
}

// newReviewGrantFixture builds project home with a member user that
// delegated to agent (a recorded session edge) and, through agent, to
// child; an admin, an owner and a plain member of home; and a user who is
// admin of another project only. noEdge is an agent created by delegator
// with no delegation edge at all.
func newReviewGrantFixture(t *testing.T) reviewGrantFixture {
	t.Helper()
	srv, s := testServer(t)
	f := reviewGrantFixture{
		srv: srv, s: s,
		home: tid("rg-home"), other: tid("rg-other"),
		delegator: tid("rg-delegator"), admin: tid("rg-admin"), owner: tid("rg-owner"),
		member: tid("rg-member"), outside: tid("rg-outside"), otherAdmin: tid("rg-other-admin"),
	}
	createDCProject(t, s, f.home, "rg-home")
	createDCProject(t, s, f.other, "rg-other")
	createDCUser(t, s, f.delegator, "rg-delegator@test.com", f.home, store.ProjectRoleMember)
	createDCUser(t, s, f.admin, "rg-admin@test.com", f.home, store.ProjectRoleAdmin)
	createDCUser(t, s, f.owner, "rg-owner@test.com", f.home, store.ProjectRoleOwner)
	createDCUser(t, s, f.member, "rg-member@test.com", f.home, store.ProjectRoleMember)
	createDCUser(t, s, f.outside, "rg-outside@test.com", f.other, store.ProjectRoleMember)
	createDCUser(t, s, f.otherAdmin, "rg-other-admin@test.com", f.other, store.ProjectRoleAdmin)
	ensureEdgeBackfillComplete(t, s)

	newAgent := func(name string) *store.Agent {
		id := tid("rg-agent-" + name)
		createDCAgent(t, s, id, f.home, f.delegator, AgentRoleFull)
		a, err := s.GetAgent(context.Background(), id)
		require.NoError(t, err)
		return a
	}
	edge := func(delegatorType, delegatorID, delegateID string, cred store.SourceCredentialKind) {
		require.NoError(t, s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
			DelegatorType: delegatorType, DelegatorID: delegatorID,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: delegateID,
			ScopeType: store.RoleScopeProject, ScopeID: f.home, Role: string(AgentRoleFull), Active: true,
			AuthorityProvenance: recordedProv(delegatorType, delegatorID, cred),
			EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}))
	}
	f.agent = newAgent("owner")
	edge(store.DelegationPrincipalUser, f.delegator, f.agent.ID, store.SourceCredentialSession)
	f.child = newAgent("child")
	edge(store.DelegationPrincipalAgent, f.agent.ID, f.child.ID, store.SourceCredentialAgent)
	f.noEdge = newAgent("no-edge")
	// sibling is delegated by member, a second user in the same project.
	f.sibling = newAgent("sibling")
	edge(store.DelegationPrincipalUser, f.member, f.sibling.ID, store.SourceCredentialSession)
	return f
}

func (f reviewGrantFixture) user(id string) Identity {
	return NewAuthenticatedUser(id, id+"@test.com", id, "member", "web")
}
