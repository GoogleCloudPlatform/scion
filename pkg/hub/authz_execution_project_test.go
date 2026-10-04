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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedExecutionAgent stores an agent row in projectID with the given
// ancestry and records the typed delegation edges of its chain in that
// project: chain[0] is the source user, chain[1:] are intermediate agents
// (stored if absent), and each link delegates to the next, the last to the
// agent.
func seedExecutionAgent(t *testing.T, s store.Store, agentID, projectID string, ancestry, chain []string) {
	t.Helper()
	ctx := context.Background()
	require.NotEmpty(t, chain, "chain names the source user")
	storeAgentIfAbsent := func(id string, anc []string) {
		if _, err := s.GetAgent(ctx, id); err == nil {
			return
		}
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: id, Slug: "exec-" + id[:8], Name: "exec-" + id[:8],
			ProjectID: projectID, Phase: "running",
			OwnerID: chain[0], CreatedBy: chain[0], Ancestry: anc,
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}))
	}
	for i := 1; i < len(chain); i++ {
		storeAgentIfAbsent(chain[i], append([]string(nil), chain[:i]...))
	}
	storeAgentIfAbsent(agentID, ancestry)

	for i := 0; i < len(chain); i++ {
		delegatorType := store.DelegationPrincipalAgent
		if i == 0 {
			delegatorType = store.DelegationPrincipalUser
		}
		delegate := agentID
		if i+1 < len(chain) {
			delegate = chain[i+1]
		}
		seedRecordedDelegationEdge(t, s, delegatorType, chain[i], store.DelegationPrincipalAgent, delegate,
			store.RoleScopeProject, projectID, string(AgentRoleFull))
	}
}

// execAgent is a local agent identity in projectID whose ancestry is
// rooted at the golden fixture's secret owner.
func execAgent(id, projectID string, ancestry []string) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: id},
		ProjectID: projectID,
		Ancestry:  ancestry,
		Scopes:    allRegisteredAgentScopes(),
	}}
}

// agentGetErrStore fails GetAgent for one ID with an error other than
// store.ErrNotFound.
type agentGetErrStore struct {
	store.Store
	failID string
}

func (s *agentGetErrStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.failID {
		return nil, errors.New("agent lookup unavailable")
	}
	return s.Store.GetAgent(ctx, id)
}

// A progeny read, including a personal-skill read, requires the agent's
// authoritative source user to hold live admission to the agent's current
// project. Point reads and the list predicate agree on every shape.
func TestExecutionProject_ProgenyParity(t *testing.T) {
	type shape struct {
		name    string
		setup   func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService)
		allowed bool
	}
	secretOwnerAncestry := func(f *goldenFixture) []string { return []string{f.projectOwnerID} }
	for _, tc := range []shape{
		{"user root admitted to the agent project", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id := tid("exec-user-root")
			seedExecutionAgent(t, f.store, id, f.projectAlpha.ID, secretOwnerAncestry(f), []string{f.projectOwnerID})
			return execAgent(id, f.projectAlpha.ID, secretOwnerAncestry(f)), f.authz
		}, true},
		{"user root without admission to the agent project", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id := tid("exec-user-root-beta")
			seedExecutionAgent(t, f.store, id, f.projectBeta.ID, secretOwnerAncestry(f), []string{f.projectOwnerID})
			return execAgent(id, f.projectBeta.ID, secretOwnerAncestry(f)), f.authz
		}, false},
		{"agent chain to an admitted user", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id, mid := tid("exec-chain"), tid("exec-chain-mid")
			anc := []string{f.projectOwnerID, mid}
			seedExecutionAgent(t, f.store, id, f.projectAlpha.ID, anc, []string{f.projectOwnerID, mid})
			return execAgent(id, f.projectAlpha.ID, anc), f.authz
		}, true},
		{"soft-deleted intermediate agent", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id, mid := tid("exec-chain-del"), tid("exec-chain-del-mid")
			anc := []string{f.projectOwnerID, mid}
			seedExecutionAgent(t, f.store, id, f.projectAlpha.ID, anc, []string{f.projectOwnerID, mid})
			softDeleteStoredAgent(t, f.store, mid)
			return execAgent(id, f.projectAlpha.ID, anc), f.authz
		}, false},
		{"stored agent without delegation edges", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id := tid("exec-no-edge")
			require.NoError(t, f.store.CreateAgent(context.Background(), &store.Agent{
				ID: id, Slug: "exec-no-edge", Name: "exec-no-edge", ProjectID: f.projectAlpha.ID,
				OwnerID: f.projectOwnerID, CreatedBy: f.projectOwnerID, Ancestry: secretOwnerAncestry(f),
			}))
			return execAgent(id, f.projectAlpha.ID, secretOwnerAncestry(f)), f.authz
		}, false},
		{"stored agent with empty ancestry and no edges", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id := tid("exec-empty-ancestry")
			require.NoError(t, f.store.CreateAgent(context.Background(), &store.Agent{
				ID: id, Slug: "exec-empty-ancestry", Name: "exec-empty-ancestry", ProjectID: f.projectAlpha.ID,
				CreatedBy: f.projectOwnerID,
			}))
			return execAgent(id, f.projectAlpha.ID, secretOwnerAncestry(f)), f.authz
		}, false},
		{"no stored agent row", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			return execAgent(tid("exec-no-row"), f.projectAlpha.ID, secretOwnerAncestry(f)), f.authz
		}, false},
		{"edge recorded in another project", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id := tid("exec-edge-other")
			require.NoError(t, f.store.CreateAgent(context.Background(), &store.Agent{
				ID: id, Slug: "exec-edge-other", Name: "exec-edge-other", ProjectID: f.projectAlpha.ID,
				OwnerID: f.projectOwnerID, CreatedBy: f.projectOwnerID, Ancestry: secretOwnerAncestry(f),
			}))
			createDCEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, store.DelegationPrincipalAgent, id,
				store.RoleScopeProject, f.projectBeta.ID, string(AgentRoleFull))
			return execAgent(id, f.projectAlpha.ID, secretOwnerAncestry(f)), f.authz
		}, false},
		{"token project differs from the stored agent project", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id := tid("exec-token-mismatch")
			seedExecutionAgent(t, f.store, id, f.projectBeta.ID, secretOwnerAncestry(f), []string{f.projectOwnerID})
			return execAgent(id, f.projectAlpha.ID, secretOwnerAncestry(f)), f.authz
		}, false},
		{"two active edges", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id := tid("exec-two-edges")
			seedExecutionAgent(t, f.store, id, f.projectAlpha.ID, secretOwnerAncestry(f), []string{f.projectOwnerID})
			st := &extraEdgeStore{Store: f.store, delegateID: id, extra: &store.DelegationEdge{
				DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.projectAdminID,
				DelegateType: store.DelegationPrincipalAgent, DelegateID: id,
				ScopeType: store.RoleScopeProject, ScopeID: f.projectAlpha.ID, Role: string(AgentRoleFull), Active: true,
			}}
			return execAgent(id, f.projectAlpha.ID, secretOwnerAncestry(f)), NewAuthzService(st, f.authz.logger)
		}, false},
		{"super-admin source without project membership", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			adminID := tid("exec-super-admin")
			createTestUserWithRole(t, f.store, adminID, "exec-admin@golden.test", "admin", store.SystemRoleSuperAdmin)
			id := tid("exec-admin-agent")
			seedExecutionAgent(t, f.store, id, f.projectBeta.ID, secretOwnerAncestry(f), []string{adminID})
			return execAgent(id, f.projectBeta.ID, secretOwnerAncestry(f)), f.authz
		}, true},
		{"suspended source user", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id := tid("exec-suspended-src")
			seedExecutionAgent(t, f.store, id, f.projectAlpha.ID, secretOwnerAncestry(f), []string{f.projectAdminID})
			setUserStatus(t, f.store, f.projectAdminID, store.UserStatusSuspended)
			return execAgent(id, f.projectAlpha.ID, secretOwnerAncestry(f)), f.authz
		}, false},
		{"agent lookup error", func(t *testing.T, f *goldenFixture) (AgentIdentity, *AuthzService) {
			id := tid("exec-store-err")
			seedExecutionAgent(t, f.store, id, f.projectAlpha.ID, secretOwnerAncestry(f), []string{f.projectOwnerID})
			return execAgent(id, f.projectAlpha.ID, secretOwnerAncestry(f)), NewAuthzService(&agentGetErrStore{Store: f.store, failID: id}, f.authz.logger)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGoldenFixture(t)
			agent, authz := tc.setup(t, f)
			res := Resource{Type: "secret", ID: f.secretID}

			d := decidePerm(authz, agent, res, ActionRead, permissionProjectSecretRead, true)
			assert.Equal(t, tc.allowed, d.Allowed, "reason %q", d.Reason)
			r := relationshipResult(t, d, RelationshipRuleProgeny)
			if tc.allowed {
				assert.True(t, r.Accepted)
			} else {
				assert.Equal(t, RelationshipRejectExecutionProject, r.RejectedBy, "detail %q", r.Detail)
				assert.Equal(t, "relationship grant restricted by execution_project", d.Reason)
			}

			src := SharingSource{Kind: "secret", ID: f.secretID, OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true}
			pred := authz.ProgenyListPredicate(context.Background(), principalContextForIdentity(agent), "secret")
			assert.Equal(t, tc.allowed, pred.Matches(src), "list predicate agrees with the point read")

			skill := skillScopeResource(store.SkillScopeUser, f.projectOwnerID)
			sd := decidePerm(authz, agent, skill, ActionRead, "skill.read", false)
			assert.Equal(t, tc.allowed, sd.Allowed, "personal-skill progeny read: reason %q", sd.Reason)
		})
	}
}

// The execution-project stage applies only to rules that derive access from
// the source user's resources: the hub skill catalog stays readable for an
// agent whose source user has no admission to its project.
func TestExecutionProject_CatalogSkillReadUnaffected(t *testing.T) {
	f := newGoldenFixture(t)
	id := tid("exec-catalog")
	seedExecutionAgent(t, f.store, id, f.projectBeta.ID, []string{f.projectOwnerID}, []string{f.projectOwnerID})
	agent := execAgent(id, f.projectBeta.ID, []string{f.projectOwnerID})

	catalog := decidePerm(f.authz, agent, skillScopeResource(store.SkillScopeGlobal, ""), ActionRead, "skill.read", false)
	assert.True(t, catalog.Allowed, "catalog skill read: reason %q", catalog.Reason)
	owned := decidePerm(f.authz, agent, skillScopeResource(store.SkillScopeUser, f.projectOwnerID), ActionRead, "skill.read", true)
	assert.False(t, owned.Allowed, "personal-skill progeny read: reason %q", owned.Reason)
	assert.Equal(t, RelationshipRejectExecutionProject, relationshipResult(t, owned, RelationshipRuleProgeny).RejectedBy)
}

// extraEdgeStore adds one more delegation edge for delegateID.
type extraEdgeStore struct {
	store.Store
	delegateID string
	extra      *store.DelegationEdge
}

func (s *extraEdgeStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	edges, err := s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	if err != nil || delegateID != s.delegateID {
		return edges, err
	}
	return append(edges, s.extra), nil
}

// A sharing source owned by an agent requires that agent's delegation chain
// to hold the read permission: a source agent whose user delegator has no
// role in the project supplies nothing, on point reads and in the list.
func TestRelationshipRules_AgentSourceRequiresDelegation(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()
	sourceUser := tid("q9-source-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{ID: sourceUser, Email: "q9@golden.test", DisplayName: "q9", Role: "member", Status: "active"}))
	sourceAgent := tid("q9-source-agent")
	seedExecutionAgent(t, f.store, sourceAgent, f.projectAlpha.ID, []string{sourceUser}, []string{sourceUser})

	readerID := tid("q9-reader")
	anc := []string{f.projectOwnerID, sourceAgent}
	seedExecutionAgent(t, f.store, readerID, f.projectAlpha.ID, anc, []string{f.projectOwnerID})
	reader := execAgent(readerID, f.projectAlpha.ID, anc)

	src := SharingSource{Kind: "secret", ID: "q9-src", OwnerID: sourceAgent, Policy: SharingPolicyOptInRequired, OptedIn: true}
	releaseBuiltinProgenyAdapter(t, f.authz, "secret")
	require.NoError(t, f.authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "secret", perms: []string{permissionProjectSecretRead}, sources: []SharingSource{src}}))

	check := func(want bool) {
		t.Helper()
		d := decidePerm(f.authz, reader, Resource{Type: "secret", ID: src.ID}, ActionRead, permissionProjectSecretRead, true)
		assert.Equal(t, want, d.Allowed, "reason %q", d.Reason)
		if !want {
			assert.Equal(t, RelationshipRejectSourceInactive, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
		}
		pred := f.authz.ProgenyListPredicate(ctx, principalContextForIdentity(reader), "secret")
		assert.Equal(t, want, pred.Matches(src), "list predicate agrees")
	}
	check(false)

	createDCUser(t, f.store, sourceUser, "q9@golden.test", f.projectAlpha.ID, store.ProjectRoleOwner)
	check(true)
}
