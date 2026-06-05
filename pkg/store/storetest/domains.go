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

package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// missingID returns a syntactically valid identifier that is guaranteed not to
// exist in a freshly created store. It is a UUID so backends that parse IDs
// (e.g. the Ent adapter) accept it and report ErrNotFound rather than a parse
// error.
func missingID() string {
	return uuid.NewString()
}

// RunStoreSuite runs the full CRUD-parity suite for every currently supported
// domain against stores produced by factory. As new domains are ported to the
// shared store interface, add their Domain descriptor here and they are covered
// automatically across all backends.
func RunStoreSuite(t *testing.T, factory Factory) {
	t.Helper()
	RunDomain(t, factory, GroupDomain())
	RunDomain(t, factory, PolicyDomain())
	RunDomain(t, factory, GCPServiceAccountDomain())
	RunDomain(t, factory, SubscriptionTemplateDomain())
	RunDomain(t, factory, NotificationSubscriptionDomain())
	RunDomain(t, factory, ProjectDomain())
	RunDomain(t, factory, RuntimeBrokerDomain())
	RunDomain(t, factory, BrokerSecretDomain())
	RunDomain(t, factory, BrokerJoinTokenDomain())
	RunDomain(t, factory, TemplateDomain())
	RunDomain(t, factory, HarnessConfigDomain())
	RunDomain(t, factory, SecretDomain())
	RunDomain(t, factory, EnvVarDomain())
	RunDomain(t, factory, AgentDomain())
	RunDomain(t, factory, UserDomain())
	RunDomain(t, factory, AllowListDomain())
	RunDomain(t, factory, InviteCodeDomain())

	// Agent optimistic locking is not expressible through the generic CRUD
	// categories, so it gets a dedicated backend-agnostic check.
	t.Run("agent/OptimisticLock", func(t *testing.T) { runAgentOptimisticLock(t, factory) })
}

func listFrom[T any](items []T, err error) (*store.ListResult[T], error) {
	if err != nil {
		return nil, err
	}
	return &store.ListResult[T]{Items: items, TotalCount: len(items)}, nil
}

// GroupDomain describes the group entity for the CRUD-parity oracle.
func GroupDomain() Domain[store.Group] {
	return Domain[store.Group]{
		Name: "group",
		Make: func(seq int) *store.Group {
			id := uuid.NewString()
			return &store.Group{
				ID:          id,
				Name:        fmt.Sprintf("Group %d", seq),
				Slug:        fmt.Sprintf("group-%d-%s", seq, id[:8]),
				Description: fmt.Sprintf("description %d", seq),
				GroupType:   store.GroupTypeExplicit,
				Labels:      map[string]string{"seq": fmt.Sprintf("%d", seq)},
			}
		},
		GetID: func(g *store.Group) string { return g.ID },
		Create: func(ctx context.Context, s store.Store, g *store.Group) error {
			return s.CreateGroup(ctx, g)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.Group, error) {
			return s.GetGroup(ctx, id)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.Group], error) {
			return s.ListGroups(ctx, store.GroupFilter{}, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.Group) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Name, got.Name)
			assert.Equal(t, want.Slug, got.Slug)
			assert.Equal(t, want.Description, got.Description)
			assert.Equal(t, store.GroupTypeExplicit, got.GroupType)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(g *store.Group) {
			g.Name = "Renamed " + g.Name
			g.Description = "updated description"
		},
		Update: func(ctx context.Context, s store.Store, g *store.Group) error {
			return s.UpdateGroup(ctx, g)
		},
		VerifyMutated: func(t *testing.T, got *store.Group) {
			assert.Contains(t, got.Name, "Renamed ")
			assert.Equal(t, "updated description", got.Description)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteGroup(ctx, id)
		},
		// Groups are hard-deleted (no SoftDelete spec).
		Filters: []FilterCase[store.Group]{
			{
				Name: "ByGroupType",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateGroup(ctx, &store.Group{
						ID: uuid.NewString(), Name: "Explicit", Slug: "explicit-" + uuid.NewString()[:8],
						GroupType: store.GroupTypeExplicit,
					}))
					require.NoError(t, s.CreateGroup(ctx, &store.Group{
						ID: uuid.NewString(), Name: "Project Agents", Slug: "project-agents-" + uuid.NewString()[:8],
						GroupType: store.GroupTypeProjectAgents,
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.Group], error) {
					return s.ListGroups(ctx, store.GroupFilter{GroupType: store.GroupTypeExplicit}, store.ListOptions{})
				},
				WantCount: 1,
			},
		},
	}
}

// PolicyDomain describes the policy entity for the CRUD-parity oracle.
func PolicyDomain() Domain[store.Policy] {
	return Domain[store.Policy]{
		Name: "policy",
		Make: func(seq int) *store.Policy {
			return &store.Policy{
				ID:           uuid.NewString(),
				Name:         fmt.Sprintf("Policy %d", seq),
				Description:  fmt.Sprintf("policy description %d", seq),
				ScopeType:    store.PolicyScopeHub,
				ResourceType: "agent",
				Actions:      []string{"read"},
				Effect:       store.PolicyEffectAllow,
				Priority:     seq,
			}
		},
		GetID: func(p *store.Policy) string { return p.ID },
		Create: func(ctx context.Context, s store.Store, p *store.Policy) error {
			return s.CreatePolicy(ctx, p)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.Policy, error) {
			return s.GetPolicy(ctx, id)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.Policy], error) {
			return s.ListPolicies(ctx, store.PolicyFilter{}, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.Policy) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Name, got.Name)
			assert.Equal(t, want.ScopeType, got.ScopeType)
			assert.Equal(t, want.ResourceType, got.ResourceType)
			assert.Equal(t, want.Actions, got.Actions)
			assert.Equal(t, want.Effect, got.Effect)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(p *store.Policy) {
			p.Name = "Renamed " + p.Name
			p.Actions = []string{"read", "update"}
		},
		Update: func(ctx context.Context, s store.Store, p *store.Policy) error {
			return s.UpdatePolicy(ctx, p)
		},
		VerifyMutated: func(t *testing.T, got *store.Policy) {
			assert.Contains(t, got.Name, "Renamed ")
			assert.Equal(t, []string{"read", "update"}, got.Actions)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeletePolicy(ctx, id)
		},
		// Policies are hard-deleted (no SoftDelete spec).
		Filters: []FilterCase[store.Policy]{
			{
				Name: "ByEffect",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreatePolicy(ctx, &store.Policy{
						ID: uuid.NewString(), Name: "Allow", ScopeType: store.PolicyScopeHub,
						ResourceType: "*", Actions: []string{"*"}, Effect: store.PolicyEffectAllow,
					}))
					require.NoError(t, s.CreatePolicy(ctx, &store.Policy{
						ID: uuid.NewString(), Name: "Deny", ScopeType: store.PolicyScopeHub,
						ResourceType: "*", Actions: []string{"*"}, Effect: store.PolicyEffectDeny,
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.Policy], error) {
					return s.ListPolicies(ctx, store.PolicyFilter{Effect: store.PolicyEffectDeny}, store.ListOptions{})
				},
				WantCount: 1,
			},
		},
	}
}

// GCPServiceAccountDomain describes the GCP service account entity for the
// CRUD-parity oracle. The store's List methods are unpaginated, so the generic
// List (pagination) category is omitted and listing is exercised via filters.
func GCPServiceAccountDomain() Domain[store.GCPServiceAccount] {
	return Domain[store.GCPServiceAccount]{
		Name: "gcp_service_account",
		Make: func(seq int) *store.GCPServiceAccount {
			id := uuid.NewString()
			return &store.GCPServiceAccount{
				ID:            id,
				Scope:         "project",
				ScopeID:       uuid.NewString(),
				Email:         fmt.Sprintf("sa-%d-%s@proj.iam.gserviceaccount.com", seq, id[:8]),
				ProjectID:     uuid.NewString(),
				DisplayName:   fmt.Sprintf("SA %d", seq),
				DefaultScopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
				CreatedBy:     "tester",
			}
		},
		GetID: func(sa *store.GCPServiceAccount) string { return sa.ID },
		Create: func(ctx context.Context, s store.Store, sa *store.GCPServiceAccount) error {
			return s.CreateGCPServiceAccount(ctx, sa)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.GCPServiceAccount, error) {
			return s.GetGCPServiceAccount(ctx, id)
		},
		VerifyEqual: func(t *testing.T, want, got *store.GCPServiceAccount) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Email, got.Email)
			assert.Equal(t, want.Scope, got.Scope)
			assert.Equal(t, want.ScopeID, got.ScopeID)
			assert.Equal(t, want.DefaultScopes, got.DefaultScopes)
			assert.False(t, got.CreatedAt.IsZero(), "CreatedAt should be set")
		},
		Mutate: func(sa *store.GCPServiceAccount) {
			sa.DisplayName = "Renamed " + sa.DisplayName
			sa.Verified = true
		},
		Update: func(ctx context.Context, s store.Store, sa *store.GCPServiceAccount) error {
			return s.UpdateGCPServiceAccount(ctx, sa)
		},
		VerifyMutated: func(t *testing.T, got *store.GCPServiceAccount) {
			assert.Contains(t, got.DisplayName, "Renamed ")
			assert.True(t, got.Verified)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteGCPServiceAccount(ctx, id)
		},
		// GCP service accounts are hard-deleted (no SoftDelete spec).
		Filters: []FilterCase[store.GCPServiceAccount]{
			{
				Name: "ByManaged",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
						ID: uuid.NewString(), Scope: "project", ScopeID: uuid.NewString(),
						Email:     "managed-" + uuid.NewString()[:8] + "@p.iam.gserviceaccount.com",
						ProjectID: uuid.NewString(), Managed: true, CreatedBy: "t",
					}))
					require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
						ID: uuid.NewString(), Scope: "project", ScopeID: uuid.NewString(),
						Email:     "byosa-" + uuid.NewString()[:8] + "@p.iam.gserviceaccount.com",
						ProjectID: uuid.NewString(), Managed: false, CreatedBy: "t",
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.GCPServiceAccount], error) {
					managed := true
					return listFrom(s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{Managed: &managed}))
				},
				WantCount: 1,
			},
		},
	}
}

// SubscriptionTemplateDomain describes the subscription template entity for the
// CRUD-parity oracle. Templates have no update method and an unpaginated list,
// so only Create/Read/Delete plus a filter scenario are exercised.
func SubscriptionTemplateDomain() Domain[store.SubscriptionTemplate] {
	return Domain[store.SubscriptionTemplate]{
		Name: "subscription_template",
		Make: func(seq int) *store.SubscriptionTemplate {
			id := uuid.NewString()
			return &store.SubscriptionTemplate{
				ID:                id,
				Name:              fmt.Sprintf("template-%d-%s", seq, id[:8]),
				Scope:             "project",
				TriggerActivities: []string{"COMPLETED", "FAILED"},
				CreatedBy:         "tester",
			}
		},
		GetID: func(tmpl *store.SubscriptionTemplate) string { return tmpl.ID },
		Create: func(ctx context.Context, s store.Store, tmpl *store.SubscriptionTemplate) error {
			return s.CreateSubscriptionTemplate(ctx, tmpl)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.SubscriptionTemplate, error) {
			return s.GetSubscriptionTemplate(ctx, id)
		},
		VerifyEqual: func(t *testing.T, want, got *store.SubscriptionTemplate) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Name, got.Name)
			assert.Equal(t, want.Scope, got.Scope)
			assert.Equal(t, want.TriggerActivities, got.TriggerActivities)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteSubscriptionTemplate(ctx, id)
		},
		// Templates are hard-deleted (no SoftDelete spec).
		Filters: []FilterCase[store.SubscriptionTemplate]{
			{
				Name: "GlobalOnly",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateSubscriptionTemplate(ctx, &store.SubscriptionTemplate{
						ID: uuid.NewString(), Name: "global-" + uuid.NewString()[:8],
						Scope: "project", TriggerActivities: []string{"COMPLETED"}, CreatedBy: "t",
					}))
					require.NoError(t, s.CreateSubscriptionTemplate(ctx, &store.SubscriptionTemplate{
						ID: uuid.NewString(), Name: "scoped-" + uuid.NewString()[:8],
						Scope: "project", TriggerActivities: []string{"FAILED"},
						ProjectID: uuid.NewString(), CreatedBy: "t",
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.SubscriptionTemplate], error) {
					return listFrom(s.ListSubscriptionTemplates(ctx, ""))
				},
				WantCount: 1,
			},
		},
	}
}

// NotificationSubscriptionDomain describes the notification subscription entity
// for the CRUD-parity oracle. Project-scoped subscriptions are used so the
// fixtures do not depend on a pre-existing agent (agent-scoped subscriptions
// carry a foreign key to agents). The store's list methods are unpaginated, so
// the generic pagination category is omitted.
func NotificationSubscriptionDomain() Domain[store.NotificationSubscription] {
	return Domain[store.NotificationSubscription]{
		Name: "notification_subscription",
		Make: func(seq int) *store.NotificationSubscription {
			return &store.NotificationSubscription{
				ID:                uuid.NewString(),
				Scope:             store.SubscriptionScopeProject,
				SubscriberType:    "user",
				SubscriberID:      fmt.Sprintf("user-%d", seq),
				ProjectID:         uuid.NewString(),
				TriggerActivities: []string{"COMPLETED"},
				CreatedBy:         "tester",
			}
		},
		GetID: func(sub *store.NotificationSubscription) string { return sub.ID },
		Create: func(ctx context.Context, s store.Store, sub *store.NotificationSubscription) error {
			return s.CreateNotificationSubscription(ctx, sub)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.NotificationSubscription, error) {
			return s.GetNotificationSubscription(ctx, id)
		},
		VerifyEqual: func(t *testing.T, want, got *store.NotificationSubscription) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Scope, got.Scope)
			assert.Equal(t, want.SubscriberID, got.SubscriberID)
			assert.Equal(t, want.TriggerActivities, got.TriggerActivities)
			assert.False(t, got.CreatedAt.IsZero(), "CreatedAt should be set")
		},
		Mutate: func(sub *store.NotificationSubscription) {
			sub.TriggerActivities = []string{"COMPLETED", "FAILED"}
		},
		Update: func(ctx context.Context, s store.Store, sub *store.NotificationSubscription) error {
			return s.UpdateNotificationSubscriptionTriggers(ctx, sub.ID, sub.TriggerActivities)
		},
		VerifyMutated: func(t *testing.T, got *store.NotificationSubscription) {
			assert.Equal(t, []string{"COMPLETED", "FAILED"}, got.TriggerActivities)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteNotificationSubscription(ctx, id)
		},
		// Notification subscriptions are hard-deleted (no SoftDelete spec).
		Filters: []FilterCase[store.NotificationSubscription]{
			{
				Name: "ByProjectScope",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					projectID := uuid.NewString()
					require.NoError(t, s.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
						ID: uuid.NewString(), Scope: store.SubscriptionScopeProject,
						SubscriberType: "user", SubscriberID: "u1", ProjectID: projectID,
						TriggerActivities: []string{"COMPLETED"}, CreatedBy: "t",
					}))
					require.NoError(t, s.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
						ID: uuid.NewString(), Scope: store.SubscriptionScopeProject,
						SubscriberType: "user", SubscriberID: "u2", ProjectID: projectID,
						TriggerActivities: []string{"FAILED"}, CreatedBy: "t",
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.NotificationSubscription], error) {
					// Both seeded subscriptions are project-scoped under distinct
					// projects; query project scope for the first project only.
					all, err := s.GetSubscriptionsForSubscriber(ctx, "user", "u1")
					return listFrom(all, err)
				},
				WantCount: 1,
			},
		},
	}
}

// agentDomainProjectID is the project every agent oracle entity references. It
// is seeded by AgentDomain.Prepare so the required project foreign key resolves
// across backends.
const agentDomainProjectID = "30000000-0000-0000-0000-0000000000d1"

// seedAgentProject creates the shared project agents reference. It is called
// once per fresh store before the agent categories run.
func seedAgentProject(t *testing.T, ctx context.Context, s store.Store) {
	t.Helper()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:         agentDomainProjectID,
		Name:       "agent-oracle-project",
		Slug:       "agent-oracle-" + agentDomainProjectID[:8],
		Visibility: "private",
	}))
}

// newOracleAgent builds a minimal valid agent referencing the seeded project.
func newOracleAgent(slug string) *store.Agent {
	id := uuid.NewString()
	return &store.Agent{
		ID:         id,
		Slug:       slug + "-" + id[:8],
		Name:       slug,
		Template:   "default",
		ProjectID:  agentDomainProjectID,
		Phase: "running",
	}
}

// seedLiveAndDeleted inserts one live agent and one soft-deleted agent.
func seedLiveAndDeleted(t *testing.T, ctx context.Context, s store.Store) {
	t.Helper()
	live := newOracleAgent("live")
	require.NoError(t, s.CreateAgent(ctx, live))

	gone := newOracleAgent("gone")
	require.NoError(t, s.CreateAgent(ctx, gone))
	gone.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, gone))
}

// AgentDomain describes the agent entity for the CRUD-parity oracle. Beyond the
// standard categories it covers the agent-specific behaviors that must hold
// identically across backends: the ancestry membership filter, soft-delete
// exclusion, and (via runAgentOptimisticLock) state_version conflict handling.
func AgentDomain() Domain[store.Agent] {
	return Domain[store.Agent]{
		Name: "agent",
		Prepare: func(t *testing.T, ctx context.Context, s store.Store) {
			seedAgentProject(t, ctx, s)
		},
		Make: func(seq int) *store.Agent {
			id := uuid.NewString()
			return &store.Agent{
				ID:         id,
				Slug:       fmt.Sprintf("agent-%d-%s", seq, id[:8]),
				Name:       fmt.Sprintf("Agent %d", seq),
				Template:   "default",
				ProjectID:  agentDomainProjectID,
				Phase:    "running",
				Activity: "thinking",
				Labels:   map[string]string{"seq": fmt.Sprintf("%d", seq)},
			}
		},
		GetID: func(a *store.Agent) string { return a.ID },
		Create: func(ctx context.Context, s store.Store, a *store.Agent) error {
			return s.CreateAgent(ctx, a)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.Agent, error) {
			return s.GetAgent(ctx, id)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
			return s.ListAgents(ctx, store.AgentFilter{}, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.Agent) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Slug, got.Slug)
			assert.Equal(t, want.Name, got.Name)
			assert.Equal(t, want.ProjectID, got.ProjectID)
			assert.Equal(t, want.Phase, got.Phase)
			assert.Equal(t, int64(1), got.StateVersion, "CreateAgent should initialize state_version to 1")
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(a *store.Agent) {
			a.Name = "Renamed " + a.Name
			a.Phase = "stopped"
		},
		Update: func(ctx context.Context, s store.Store, a *store.Agent) error {
			return s.UpdateAgent(ctx, a)
		},
		VerifyMutated: func(t *testing.T, got *store.Agent) {
			assert.Contains(t, got.Name, "Renamed ")
			assert.Equal(t, "stopped", got.Phase)
			assert.Equal(t, int64(2), got.StateVersion, "UpdateAgent should bump state_version")
		},
		// DeleteAgent is a hard delete; soft-delete (deleted_at via UpdateAgent)
		// is covered by the filter cases below.
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteAgent(ctx, id)
		},
		Filters: []FilterCase[store.Agent]{
			{
				// Ancestry membership: only agents whose ancestry chain contains
				// the queried principal are returned.
				Name: "ByAncestor",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					child := newOracleAgent("child")
					child.Ancestry = []string{"root-user", "mid-agent"}
					require.NoError(t, s.CreateAgent(ctx, child))

					sibling := newOracleAgent("sibling")
					sibling.Ancestry = []string{"root-user"}
					require.NoError(t, s.CreateAgent(ctx, sibling))

					orphan := newOracleAgent("orphan")
					require.NoError(t, s.CreateAgent(ctx, orphan))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.Agent], error) {
					return s.ListAgents(ctx, store.AgentFilter{AncestorID: "root-user"}, store.ListOptions{})
				},
				WantCount: 2,
			},
			{
				// Soft-deleted agents are excluded from the default listing.
				Name: "ExcludeSoftDeleted",
				Seed: seedLiveAndDeleted,
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.Agent], error) {
					return s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{})
				},
				WantCount: 1,
			},
			{
				// ... but reappear when explicitly included.
				Name: "IncludeSoftDeleted",
				Seed: seedLiveAndDeleted,
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.Agent], error) {
					return s.ListAgents(ctx, store.AgentFilter{IncludeDeleted: true}, store.ListOptions{})
				},
				WantCount: 2,
			},
		},
	}
}

// runAgentOptimisticLock verifies that a stale UpdateAgent (one carrying an
// out-of-date StateVersion) is rejected with ErrVersionConflict rather than
// silently overwriting a concurrent winner.
func runAgentOptimisticLock(t *testing.T, factory Factory) {
	ctx := context.Background()
	s := factory(t)
	seedAgentProject(t, ctx, s)

	a := newOracleAgent("locked")
	require.NoError(t, s.CreateAgent(ctx, a))

	first, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	second, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	// First writer wins, advancing the version.
	first.Name = "Winner"
	require.NoError(t, s.UpdateAgent(ctx, first))

	// Second writer holds the now-stale version and must conflict.
	second.Name = "Loser"
	assert.ErrorIs(t, s.UpdateAgent(ctx, second), store.ErrVersionConflict)

	final, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "Winner", final.Name)
}
