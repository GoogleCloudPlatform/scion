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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sessionAdminFor(t *testing.T, s store.Store, project *store.Project, suffix string) Identity {
	t.Helper()
	user := newReincarnateAuthzUser(t, s, suffix)
	grantProjectRole(t, s, user.ID, project.ID, store.ProjectRoleAdmin)
	return NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
}

// The per-agent shared dir backend change (sharedDirBackends,
// allowEmptySharedDir) was removed: server.project_storage places every
// agent's shared dirs. An older client that still sends it gets a 400
// naming the replacement instead of a reincarnation that silently ignores
// it, and nothing is dispatched or changed.
func TestReincarnateAgent_RemovedSharedDirBackendOptionsRejected(t *testing.T) {
	for name, body := range map[string]ReincarnateAgentRequest{
		"backend change":         {SharedDirBackends: map[string]string{"notes": "nfs"}},
		"backend change dry run": {DryRun: true, SharedDirBackends: map[string]string{"notes": "local"}},
		"allow empty only":       {AllowEmptySharedDir: true},
	} {
		t.Run(name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, nil)
			identity := sessionAdminFor(t, s, project, "sd-removed")
			before := agent.StateVersion

			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, identity, body), agent.ID)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "server.project_storage")
			assert.Zero(t, disp.reprovisionCalls)
			after, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, before, after.StateVersion)
		})
	}
}
