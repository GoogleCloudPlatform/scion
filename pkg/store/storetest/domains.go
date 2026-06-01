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
	RunDomain(t, factory, TemplateDomain())
	RunDomain(t, factory, HarnessConfigDomain())
	RunDomain(t, factory, SecretDomain())
	RunDomain(t, factory, EnvVarDomain())
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
