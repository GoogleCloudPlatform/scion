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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeCapabilities_AdminGetsAllActions(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-1", "admin@example.com", "Admin", "admin", "api")
	resource := Resource{Type: "agent", ID: "some-agent"}

	caps := srv.authzService.ComputeCapabilities(ctx, admin, resource)
	assert.Equal(t, []string{"read", "update", "delete", "start", "stop", "message", "attach"}, caps.Actions)
}

func TestComputeCapabilities_OwnerGetsAllActions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "user-owner-cap", Email: "owner-cap@test.com", DisplayName: "Owner", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser("user-owner-cap", "owner-cap@test.com", "Owner", "member", "api")
	resource := Resource{Type: "agent", ID: "agent-1", OwnerID: "user-owner-cap"}

	caps := srv.authzService.ComputeCapabilities(ctx, user, resource)
	assert.Equal(t, []string{"read", "update", "delete", "start", "stop", "message", "attach"}, caps.Actions)
}

func TestComputeCapabilities_PolicySubset(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "user-readonly-cap", Email: "readonly-cap@test.com", DisplayName: "ReadOnly", Role: "member", Status: "active",
	}))

	policy := &store.Policy{
		ID: "policy-ro-cap", Name: "Read Only", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: "policy-ro-cap", PrincipalType: "user", PrincipalID: "user-readonly-cap",
	}))

	user := NewAuthenticatedUser("user-readonly-cap", "readonly-cap@test.com", "ReadOnly", "member", "api")
	resource := Resource{Type: "agent", ID: "agent-1"}

	caps := srv.authzService.ComputeCapabilities(ctx, user, resource)
	assert.Equal(t, []string{"read"}, caps.Actions)
}

func TestComputeCapabilities_DefaultDenyEmpty(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "user-nopolicy-cap", Email: "nopolicy-cap@test.com", DisplayName: "NoPolicy", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser("user-nopolicy-cap", "nopolicy-cap@test.com", "NoPolicy", "member", "api")
	resource := Resource{Type: "agent", ID: "agent-1"}

	caps := srv.authzService.ComputeCapabilities(ctx, user, resource)
	assert.Equal(t, []string{}, caps.Actions)
}

func TestComputeCapabilitiesBatch_AdminGetsAll(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-batch", "admin-batch@example.com", "Admin", "admin", "api")
	resources := []Resource{
		{Type: "agent", ID: "agent-1"},
		{Type: "agent", ID: "agent-2"},
		{Type: "agent", ID: "agent-3"},
	}

	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, admin, resources, "agent")
	require.Len(t, caps, 3)
	for _, cap := range caps {
		assert.Equal(t, []string{"read", "update", "delete", "start", "stop", "message", "attach"}, cap.Actions)
	}
}

func TestComputeCapabilitiesBatch_MixedOwnership(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "user-mixed-cap", Email: "mixed-cap@test.com", DisplayName: "Mixed", Role: "member", Status: "active",
	}))

	// Policy grants read-only on agents
	policy := &store.Policy{
		ID: "policy-mixed-cap", Name: "Read Only", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: "policy-mixed-cap", PrincipalType: "user", PrincipalID: "user-mixed-cap",
	}))

	user := NewAuthenticatedUser("user-mixed-cap", "mixed-cap@test.com", "Mixed", "member", "api")
	resources := []Resource{
		{Type: "agent", ID: "agent-owned", OwnerID: "user-mixed-cap"}, // Owned
		{Type: "agent", ID: "agent-other", OwnerID: "other-user"},     // Not owned
	}

	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, user, resources, "agent")
	require.Len(t, caps, 2)

	// Owned resource gets all actions
	assert.Equal(t, []string{"read", "update", "delete", "start", "stop", "message", "attach"}, caps[0].Actions)

	// Non-owned resource gets only read from policy
	assert.Equal(t, []string{"read"}, caps[1].Actions)
}

func TestComputeCapabilities_AncestorGetsAllActions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "user-ancestor-cap", Email: "ancestor-cap@test.com", DisplayName: "Ancestor", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser("user-ancestor-cap", "ancestor-cap@test.com", "Ancestor", "member", "api")
	resource := Resource{
		Type:     "agent",
		ID:       "agent-descendant",
		OwnerID:  "someone-else",
		Ancestry: []string{"user-ancestor-cap", "agent-middle"},
	}

	caps := srv.authzService.ComputeCapabilities(ctx, user, resource)
	assert.Equal(t, []string{"read", "update", "delete", "start", "stop", "message", "attach"}, caps.Actions)
}

func TestComputeCapabilitiesBatch_AncestryAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "user-batch-ancestor", Email: "batch-ancestor@test.com", DisplayName: "BatchAnc", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser("user-batch-ancestor", "batch-ancestor@test.com", "BatchAnc", "member", "api")
	resources := []Resource{
		{Type: "agent", ID: "agent-descendant-1", OwnerID: "other", Ancestry: []string{"user-batch-ancestor", "agent-A"}},
		{Type: "agent", ID: "agent-unrelated", OwnerID: "other", Ancestry: []string{"other-user"}},
	}

	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, user, resources, "agent")
	require.Len(t, caps, 2)

	// Descendant gets all actions via ancestry
	assert.Equal(t, []string{"read", "update", "delete", "start", "stop", "message", "attach"}, caps[0].Actions)

	// Unrelated agent gets empty (no policy, not owner, not ancestor)
	assert.Equal(t, []string{}, caps[1].Actions)
}

func TestComputeScopeCapabilities(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-scope-cap", "admin-scope@example.com", "Admin", "admin", "api")

	caps := srv.authzService.ComputeScopeCapabilities(ctx, admin, "", "", "agent")
	assert.Equal(t, []string{"create", "list", "stop_all"}, caps.Actions)
}

func TestComputeScopeCapabilities_NoPolicy(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "user-noscope-cap", Email: "noscope-cap@test.com", DisplayName: "NoScope", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser("user-noscope-cap", "noscope-cap@test.com", "NoScope", "member", "api")
	caps := srv.authzService.ComputeScopeCapabilities(ctx, user, "", "", "agent")
	assert.Equal(t, []string{}, caps.Actions)
}

func TestAgentWithCapabilities_JSONStructure(t *testing.T) {
	awc := AgentWithCapabilities{
		Agent: store.Agent{
			ID:   "agent-json-1",
			Name: "Test Agent",
			Slug: "test-agent",
		},
		Cap: &Capabilities{
			Actions: []string{"read", "update"},
		},
	}

	data, err := json.Marshal(awc)
	require.NoError(t, err)

	// Verify flat JSON structure
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	// Agent fields should be at top level
	assert.Equal(t, "agent-json-1", result["id"])
	assert.Equal(t, "Test Agent", result["name"])
	assert.Equal(t, "test-agent", result["slug"])

	// _capabilities should be at top level (not nested under agent)
	capObj, ok := result["_capabilities"].(map[string]interface{})
	require.True(t, ok, "_capabilities should be a JSON object at the top level")
	actions, ok := capObj["actions"].([]interface{})
	require.True(t, ok, "actions should be an array")
	assert.Len(t, actions, 2)
	assert.Equal(t, "read", actions[0])
	assert.Equal(t, "update", actions[1])
}

func TestProjectWithCapabilities_JSONStructure(t *testing.T) {
	gwc := ProjectWithCapabilities{
		Project: store.Project{
			ID:   "project-json-1",
			Name: "Test Project",
		},
		Cap: &Capabilities{
			Actions: []string{"read", "manage"},
		},
	}

	data, err := json.Marshal(gwc)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	assert.Equal(t, "project-json-1", result["id"])
	assert.Equal(t, "Test Project", result["name"])

	capObj, ok := result["_capabilities"].(map[string]interface{})
	require.True(t, ok)
	actions, ok := capObj["actions"].([]interface{})
	require.True(t, ok)
	assert.Len(t, actions, 2)
}

func TestWithCapabilities_OmitsWhenNil(t *testing.T) {
	awc := AgentWithCapabilities{
		Agent: store.Agent{
			ID:   "agent-no-cap",
			Name: "No Caps",
		},
	}

	data, err := json.Marshal(awc)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	_, exists := result["_capabilities"]
	assert.False(t, exists, "_capabilities should be omitted when nil")
}

func TestComputeCapabilities_UnknownResourceType(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-unk", "admin@example.com", "Admin", "admin", "api")
	resource := Resource{Type: "unknown", ID: "some-id"}

	caps := srv.authzService.ComputeCapabilities(ctx, admin, resource)
	assert.Equal(t, []string{}, caps.Actions)
}

func TestComputeCapabilitiesBatch_EmptyList(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-empty", "admin@example.com", "Admin", "admin", "api")

	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, admin, nil, "agent")
	assert.Len(t, caps, 0)
}

func TestResourceBuilders(t *testing.T) {
	t.Run("agentResource", func(t *testing.T) {
		a := &store.Agent{ID: "a1", OwnerID: "u1", ProjectID: "g1", Labels: map[string]string{"env": "prod"}, Ancestry: []string{"u1"}}
		r := agentResource(a)
		assert.Equal(t, "agent", r.Type)
		assert.Equal(t, "a1", r.ID)
		assert.Equal(t, "u1", r.OwnerID)
		assert.Equal(t, "project", r.ParentType)
		assert.Equal(t, "g1", r.ParentID)
		assert.Equal(t, "prod", r.Labels["env"])
		assert.Equal(t, []string{"u1"}, r.Ancestry)
	})

	t.Run("projectResource", func(t *testing.T) {
		g := &store.Project{ID: "g1", OwnerID: "u1"}
		r := projectResource(g)
		assert.Equal(t, "project", r.Type)
		assert.Equal(t, "g1", r.ID)
		assert.Equal(t, "u1", r.OwnerID)
	})

	t.Run("templateResource", func(t *testing.T) {
		tmpl := &store.Template{ID: "t1", OwnerID: "u1"}
		r := templateResource(tmpl)
		assert.Equal(t, "template", r.Type)
		assert.Equal(t, "t1", r.ID)
		assert.Equal(t, "u1", r.OwnerID)
	})

	t.Run("groupResource", func(t *testing.T) {
		g := &store.Group{ID: "grp1", OwnerID: "u1"}
		r := groupResource(g)
		assert.Equal(t, "group", r.Type)
		assert.Equal(t, "grp1", r.ID)
		assert.Equal(t, "u1", r.OwnerID)
	})

	t.Run("userResource", func(t *testing.T) {
		u := &store.User{ID: "u1"}
		r := userResource(u)
		assert.Equal(t, "user", r.Type)
		assert.Equal(t, "u1", r.ID)
	})

	t.Run("policyResource", func(t *testing.T) {
		p := &store.Policy{ID: "p1", Labels: map[string]string{"team": "backend"}}
		r := policyResource(p)
		assert.Equal(t, "policy", r.Type)
		assert.Equal(t, "p1", r.ID)
		assert.Equal(t, "backend", r.Labels["team"])
	})
}
