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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// grantLaunchDelivery gives agent what launch material selection needs to
// deliver its project-, hub- and broker-scope items on store s
// (material_delivery.go): an active root user admitted to the agent's
// project, the agent's stored row, and a recorded principal edge from the
// root. Rows that already exist are left as they are; missing ones are
// created. When d has no authorization service, a fresh one on s is wired
// in. It returns the service d uses.
//
// The root is the agent's first ancestry entry, else its owner; when that
// is empty or not a UUID, a user derived from the agent ID.
func grantLaunchDelivery(t *testing.T, s store.Store, d *HTTPAgentDispatcher, agent *store.Agent) *AuthzService {
	t.Helper()
	ctx := context.Background()
	require.NotNil(t, agent)
	require.NotEmpty(t, agent.ID)
	require.NotEmpty(t, agent.ProjectID, "launch delivery needs the agent's project")

	root := agent.OwnerID
	if len(agent.Ancestry) > 0 {
		root = agent.Ancestry[0]
	}
	// Store user IDs are UUIDs; a fixture whose owner is a plain label
	// gets a derived root user instead (the agent's empty ancestry lets
	// its recorded chain alone name the root).
	if _, err := uuid.Parse(root); err != nil {
		require.Empty(t, agent.Ancestry, "a non-UUID ancestry root cannot be stored")
		root = tid("launch-root-" + agent.ID)
	}
	if _, err := s.GetUser(ctx, root); err != nil {
		require.True(t, errors.Is(err, store.ErrNotFound), "root user lookup: %v", err)
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: root, Email: root + "@launch.test", DisplayName: root, Role: store.UserRoleMember, Status: store.UserStatusActive,
		}))
	}
	if _, err := s.GetProject(ctx, agent.ProjectID); err != nil {
		require.True(t, errors.Is(err, store.ErrNotFound), "project lookup: %v", err)
		slug := "launch-" + tidSlugSafe(agent.ProjectID)
		require.NoError(t, s.CreateProject(ctx, &store.Project{ID: agent.ProjectID, Name: slug, Slug: slug}))
	}
	grantProjectAccessOnly(t, s, root, agent.ProjectID)
	if _, err := s.GetAgent(ctx, agent.ID); err != nil {
		require.True(t, errors.Is(err, store.ErrNotFound), "agent lookup: %v", err)
		row := *agent
		if row.Slug == "" {
			row.Slug = "launch-" + tidSlugSafe(agent.ID)
		}
		if row.Name == "" {
			row.Name = row.Slug
		}
		// The store keeps only UUID principals; a plain-label owner or
		// creator is stored as the root. The caller's own copy, which the
		// dispatch reads, keeps its label.
		if _, err := uuid.Parse(row.OwnerID); row.OwnerID != "" && err != nil {
			row.OwnerID = root
		}
		if _, err := uuid.Parse(row.CreatedBy); row.CreatedBy != "" && err != nil {
			row.CreatedBy = root
		}
		require.NoError(t, s.CreateAgent(ctx, &row))
	}
	hdEdge(t, s, store.DelegationPrincipalUser, root, agent.ID, agent.ProjectID, nil)

	if d.authzService == nil {
		d.SetAuthzService(NewAuthzService(s, slog.Default()))
	}
	return d.authzService
}

// launchSecretBackend returns a local secret backend on s holding secrets,
// written as stored records so launch selection can authorize and fetch
// them (the in-memory mockSecretBackend serves values only through
// Resolve, which launch selection does not call). An empty injection mode
// is stored as always.
func launchSecretBackend(t *testing.T, s store.Store, hubID string, secrets []secret.SecretWithValue) *secret.LocalBackend {
	t.Helper()
	b := secret.NewLocalBackend(s, hubID, "test-secret")
	for _, sv := range secrets {
		mode := sv.InjectionMode
		if mode == "" {
			mode = store.InjectionModeAlways
		}
		_, _, err := b.Set(context.Background(), &secret.SetSecretInput{
			Name: sv.Name, Value: sv.Value, SecretType: sv.SecretType, Target: sv.Target,
			Scope: sv.Scope, ScopeID: sv.ScopeID, InjectionMode: mode,
			AllowProgeny: sv.AllowProgeny, CreatedBy: sv.CreatedBy,
		})
		require.NoError(t, err)
	}
	return b
}
