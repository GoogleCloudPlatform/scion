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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review-grant authority on agent-owned artifacts (design D24,
// ptone/scion#4014): artifactHost.MayGrantReview.

type reviewGrantFixture struct {
	srv                    *Server
	s                      store.Store
	home, other            string
	delegator, admin       string
	owner, member, outside string
	otherAdmin             string
	agent, child           *store.Agent
	noEdge                 *store.Agent
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
	return f
}

func (f reviewGrantFixture) user(id string) Identity {
	return NewAuthenticatedUser(id, id+"@test.com", id, "member", "web")
}

func (f reviewGrantFixture) may(host *artifactHost, identity Identity, agentID, home string) bool {
	return host.MayGrantReview(contextWithIdentity(context.Background(), identity), agentID, home)
}

// TestArtifactHostMayGrantReview: the delegating user (root of the live
// recorded chain) and home-project administrators may; nobody else.
func TestArtifactHostMayGrantReview(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	agentToken := artifactTestAgent(f.agent.ID, f.home, ScopesForRole(AgentRoleFull)...)

	cases := []struct {
		name     string
		identity Identity
		agentID  string
		home     string
		want     bool
	}{
		{"delegating user", f.user(f.delegator), f.agent.ID, f.home, true},
		{"delegating user, agent-spawned agent", f.user(f.delegator), f.child.ID, f.home, true},
		{"delegating user, artifact homed in another project", f.user(f.delegator), f.agent.ID, f.other, true},
		{"home-project admin", f.user(f.admin), f.agent.ID, f.home, true},
		{"home-project owner", f.user(f.owner), f.agent.ID, f.home, true},
		{"home-project admin, agent with no edge", f.user(f.admin), f.noEdge.ID, f.home, true},

		{"owning agent", agentToken, f.agent.ID, f.home, false},
		{"plain project member", f.user(f.member), f.agent.ID, f.home, false},
		{"user from another project", f.user(f.outside), f.agent.ID, f.home, false},
		{"admin of another project", f.user(f.otherAdmin), f.agent.ID, f.home, false},
		{"home admin, artifact homed elsewhere", f.user(f.admin), f.agent.ID, f.other, false},
		{"creator of an agent with no edge", f.user(f.delegator), f.noEdge.ID, f.home, false},
		{"unknown agent", f.user(f.delegator), tid("rg-missing"), f.home, false},
		{"empty agent", f.user(f.admin), "", f.home, false},
		{"empty home", f.user(f.admin), f.agent.ID, "", false},
		{"no identity", nil, f.agent.ID, f.home, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.identity != nil {
				ctx = contextWithIdentity(ctx, tc.identity)
			}
			assert.Equal(t, tc.want, host.MayGrantReview(ctx, tc.agentID, tc.home))
		})
	}
}

// TestArtifactHostMayGrantReview_ScopedToken: a scoped user access token is
// held to its own limits for project administration; the delegating user's
// token still identifies that user.
func TestArtifactHostMayGrantReview_ScopedToken(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	adminUser := NewAuthenticatedUser(f.admin, "rg-admin@test.com", "admin", "member", "api")
	delegatorUser := NewAuthenticatedUser(f.delegator, "rg-delegator@test.com", "delegator", "member", "api")

	assert.False(t, f.may(host, artifactTestUAT(t, adminUser, f.home, "artifact:manage"), f.agent.ID, f.home),
		"admin token without project:manage")
	assert.True(t, f.may(host, artifactTestUAT(t, adminUser, f.home, "artifact:manage", "project:manage"), f.agent.ID, f.home),
		"admin token with project:manage")
	assert.True(t, f.may(host, artifactTestUAT(t, delegatorUser, f.home, "artifact:manage"), f.agent.ID, f.home),
		"delegating user's token")
}

// TestArtifactHostMayGrantReview_ChainLive: the delegating user is resolved
// at request time, so revoking the delegation, deactivating the user,
// removing them from the project or failing the lookup takes the authority
// away, for the agent and for agents below it.
func TestArtifactHostMayGrantReview_ChainLive(t *testing.T) {
	t.Run("delegation revoked", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		host := newArtifactHost(f.srv)
		require.True(t, f.may(host, f.user(f.delegator), f.agent.ID, f.home))
		revokeDelegateEdges(t, f.s, f.agent.ID)
		assert.False(t, f.may(host, f.user(f.delegator), f.agent.ID, f.home))
		assert.False(t, f.may(host, f.user(f.delegator), f.child.ID, f.home), "agent below a revoked edge")
		assert.True(t, f.may(host, f.user(f.admin), f.agent.ID, f.home), "home admin unaffected")
	})
	t.Run("delegating user inactive", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		host := newArtifactHost(f.srv)
		setUserStatus(t, f.s, f.delegator, "suspended")
		assert.False(t, f.may(host, f.user(f.delegator), f.agent.ID, f.home))
	})
	t.Run("delegating user left the project", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		host := newArtifactHost(f.srv)
		bindings, err := f.s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, f.delegator)
		require.NoError(t, err)
		for _, b := range bindings {
			if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.home {
				require.NoError(t, f.s.DeleteRoleBinding(context.Background(), b.ID))
			}
		}
		assert.False(t, f.may(host, f.user(f.delegator), f.agent.ID, f.home))
	})
	t.Run("chain lookup fails", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		faulty := &Server{authzService: NewAuthzService(&faultStore{Store: f.s, edges: true}, slog.Default())}
		host := newArtifactHost(faulty)
		assert.False(t, f.may(host, f.user(f.delegator), f.agent.ID, f.home))
	})
	t.Run("no authz service", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		assert.False(t, f.may(newArtifactHost(&Server{}), f.user(f.admin), f.agent.ID, f.home))
		assert.False(t, f.may(&artifactHost{}, f.user(f.admin), f.agent.ID, f.home))
	})
}
