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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func TestDispatchAgentEventHandler_UserAuthoredChildRoleSurvivesMigration(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)
	ctx := context.Background()

	err := srv.dispatchAgentEventHandler()(ctx, store.ScheduledEvent{
		ID:        tid("scheduled-dispatch-user-role"),
		ProjectID: project.ID,
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"scheduled-user-child","task":"scheduled work"}`,
		CreatedBy: user.ID,
		FireAt:    time.Now(),
	})
	require.NoError(t, err)

	child, err := s.GetAgentBySlug(ctx, project.ID, "scheduled-user-child")
	require.NoError(t, err)
	require.NotNil(t, child.AppliedConfig)
	require.Equal(t, string(AgentRoleNone), child.AppliedConfig.AgentRole)
	require.True(t, child.AppliedConfig.NoAuth)

	dispatcher := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
	dispatcher.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{SecretMeta: secret.SecretMeta{Name: "CLAUDE_AUTH", SecretType: "file", Target: "~/.claude/.credentials.json"}, Value: "secret-data"},
			{SecretMeta: secret.SecretMeta{Name: "API_KEY", SecretType: "environment", Target: "API_KEY"}, Value: "key-value"},
		},
	})
	req, err := dispatcher.buildCreateRequest(ctx, child, "TestScheduledNoAuth")
	require.NoError(t, err)
	require.True(t, req.NoAuth)
	require.Empty(t, req.ResolvedSecrets)
	require.NotContains(t, req.ResolvedEnv, "API_KEY")

	require.NoError(t, s.Migrate(ctx))

	child, err = s.GetAgent(ctx, child.ID)
	require.NoError(t, err)
	require.NotNil(t, child.AppliedConfig)
	require.Equal(t, string(AgentRoleNone), child.AppliedConfig.AgentRole)
	require.True(t, child.AppliedConfig.NoAuth)
}
