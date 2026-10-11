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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// setupPassthroughServer creates a test server with a broker owned by
// the given owner user, a project, and optionally sets the broker host SA.
// The owner user is created and added to hub/project membership before the
// project group and policy are set up, so the user has create-agent access.
func setupPassthroughServer(t *testing.T, owner *store.User, hostSAEmail, hostProjectID string) (*Server, store.Store, *store.Project, *store.RuntimeBroker) {
	t.Helper()
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s := testServer(t)
	ctx := context.Background()

	// The user must exist before seedProjectCreatorMembership so that
	// the FK constraint on the group owner succeeds and the user is added to
	// the project members group.
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("project-pt"),
		Name:      "Passthrough Test Project",
		Slug:      "passthrough-test-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	broker := &store.RuntimeBroker{
		ID:                         tid("broker-pt"),
		Name:                       "PT Test Broker",
		Slug:                       "pt-test-broker",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  owner.ID,
		GCPHostServiceAccountEmail: hostSAEmail,
		GCPHostProjectID:           hostProjectID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	srv.SetDispatcher(disp)
	return srv, s, project, broker
}

// ptUser creates a store.User value for test setup. The caller must pass the
// result to setupPassthroughServer (which persists and groups it) before using
// it in requests.
func ptUser(id, email, role string) *store.User {
	return &store.User{
		ID:          id,
		Email:       email,
		DisplayName: email,
		Role:        role,
		Status:      "active",
		Created:     time.Now(),
	}
}

// setupPassthroughSandboxServer is like setupPassthroughServer but creates a
// broker with a cloudrun-sandbox profile so the passthrough-to-assign
// translation fires.
func setupPassthroughSandboxServer(
	t *testing.T,
	owner *store.User,
	hostSAEmail, hostProjectID string,
) (*Server, store.Store, *store.Project, *store.RuntimeBroker) {
	t.Helper()
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("project-sandbox-pt"),
		Name:      "Sandbox PT Test Project",
		Slug:      "sandbox-pt-test-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	broker := &store.RuntimeBroker{
		ID:                         tid("broker-sandbox-pt"),
		Name:                       "Sandbox PT Test Broker",
		Slug:                       "sandbox-pt-test-broker",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  owner.ID,
		GCPHostServiceAccountEmail: hostSAEmail,
		GCPHostProjectID:           hostProjectID,
		Profiles: []store.BrokerProfile{
			{Name: "default", Type: "cloudrun-sandbox", Available: true},
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	srv.SetDispatcher(disp)
	return srv, s, project, broker
}
