// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2769 (PR7): a user who owns agents cannot be
// deleted, and a deleted user's user-scope secrets and env vars are removed.

// newActiveMember creates an active member user with no role bindings.
func newActiveMember(t *testing.T, s store.Store, id, email string) *store.User {
	t.Helper()
	u := &store.User{ID: tid(id), Email: email, DisplayName: id,
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

// createOwnedAgent creates an agent owned by ownerID in the project.
func createOwnedAgent(t *testing.T, s store.Store, slug, projectID, ownerID string) *store.Agent {
	t.Helper()
	a := &store.Agent{ID: tid("agent-" + slug), Slug: slug, Name: slug, ProjectID: projectID,
		Phase: "running", OwnerID: ownerID, CreatedBy: ownerID}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

// requireOwnsAgentsDenial asserts a 409 conflict whose details.agents lists
// exactly the given agents.
func requireOwnsAgentsDenial(t *testing.T, rec *httptest.ResponseRecorder, agents ...*store.Agent) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Agents []ownedAgentRef `json:"agents"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
	assert.Equal(t, userOwnsAgentsDeleteMessage, resp.Error.Message)
	want := make([]ownedAgentRef, 0, len(agents))
	for _, a := range agents {
		want = append(want, ownedAgentRef{ID: a.ID, Slug: a.Slug, ProjectID: a.ProjectID})
	}
	assert.ElementsMatch(t, want, resp.Error.Details.Agents)
}

func TestDeleteUser_OwnsLiveAgentDenied(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	running := createOwnedAgent(t, s, "dave-running", project.ID, dave.ID)
	// A stopped agent still counts: it is not deleted.
	stopped := &store.Agent{ID: tid("agent-dave-stopped"), Slug: "dave-stopped", Name: "dave-stopped",
		ProjectID: project.ID, Phase: "stopped", OwnerID: dave.ID, CreatedBy: dave.ID}
	require.NoError(t, s.CreateAgent(ctx, stopped))
	// An agent owned by someone else is not listed.
	other := newActiveMember(t, s, "user-erin", "erin@test.com")
	createOwnedAgent(t, s, "erin-agent", project.ID, other.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	requireOwnsAgentsDenial(t, rec, running, stopped)

	_, err := s.GetUser(ctx, dave.ID)
	require.NoError(t, err, "denied delete must keep the user")
}

func TestDeleteUser_OwnedAgentDeletedAllowed(t *testing.T) {
	for _, mode := range []string{"hard", "soft"} {
		t.Run(mode, func(t *testing.T) {
			srv, s, _, _, project := setupDemoPolicyTest(t)
			ctx := context.Background()
			dave := newActiveMember(t, s, "user-dave", "dave@test.com")
			a := createOwnedAgent(t, s, "dave-agent", project.ID, dave.ID)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
			requireOwnsAgentsDenial(t, rec, a)

			if mode == "hard" {
				require.NoError(t, s.DeleteAgent(ctx, a.ID))
			} else {
				// A soft-deleted agent is hidden from the agent list and
				// does not block the delete.
				now := time.Now()
				_, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{},
					store.DeletionFields{DeletedAt: &now})
				require.NoError(t, err)
			}

			rec = doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			_, err := s.GetUser(ctx, dave.ID)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestDeprecatedAllowListDelete_OwnsLiveAgentDenied(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	carol := newInvitedUser(t, s, "user-carol", "carol@test.com")
	a := createOwnedAgent(t, s, "carol-agent", project.ID, carol.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
	requireOwnsAgentsDenial(t, rec, a)
	_, err := s.GetUser(ctx, carol.ID)
	require.NoError(t, err, "denied delete must keep the invited user")

	require.NoError(t, s.DeleteAgent(ctx, a.ID))
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err = s.GetUser(ctx, carol.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// useLocalSecretBackend installs a local secret backend on srv.
func useLocalSecretBackend(t *testing.T, srv *Server, s store.Store) secret.SecretBackend {
	t.Helper()
	b := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(b)
	return b
}

// seedUserScopedData gives scopeID one user-scope secret and one user-scope
// env var.
func seedUserScopedData(t *testing.T, s store.Store, b secret.SecretBackend, scopeID string) {
	t.Helper()
	ctx := context.Background()
	_, _, err := b.Set(ctx, &secret.SetSecretInput{
		Name: "API_KEY", Value: "v-" + scopeID, SecretType: secret.TypeEnvironment,
		Scope: secret.ScopeUser, ScopeID: scopeID, CreatedBy: scopeID,
	})
	require.NoError(t, err)
	_, err = s.UpsertEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "LOG_LEVEL", Value: "debug",
		Scope: store.ScopeUser, ScopeID: scopeID, InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)
}

// userScopedCounts returns how many user-scope secrets and env vars scopeID has.
func userScopedCounts(t *testing.T, s store.Store, scopeID string) (secrets, envVars int) {
	t.Helper()
	ctx := context.Background()
	sec, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeUser, ScopeID: scopeID})
	require.NoError(t, err)
	ev, err := s.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeUser, ScopeID: scopeID})
	require.NoError(t, err)
	return len(sec), len(ev)
}

func TestDeleteUser_RemovesUserScopeSecretsAndEnvVars(t *testing.T) {
	for _, path := range []string{"users", "allow-list"} {
		t.Run(path, func(t *testing.T) {
			srv, s := testServer(t)
			b := useLocalSecretBackend(t, srv, s)
			var target *store.User
			url := ""
			if path == "users" {
				target = newActiveMember(t, s, "user-dave", "dave@test.com")
				url = "/api/v1/users/" + target.ID
			} else {
				target = newInvitedUser(t, s, "user-carol", "carol@test.com")
				url = "/api/v1/admin/allow-list/" + target.Email
			}
			other := newActiveMember(t, s, "user-erin", "erin@test.com")
			seedUserScopedData(t, s, b, target.ID)
			seedUserScopedData(t, s, b, other.ID)

			rec := doRequest(t, srv, http.MethodDelete, url, nil)
			require.Less(t, rec.Code, 300, rec.Body.String())

			sec, ev := userScopedCounts(t, s, target.ID)
			assert.Zero(t, sec, "deleted user's user-scope secrets must be removed")
			assert.Zero(t, ev, "deleted user's user-scope env vars must be removed")
			sec, ev = userScopedCounts(t, s, other.ID)
			assert.Equal(t, 1, sec, "another user's secrets must be kept")
			assert.Equal(t, 1, ev, "another user's env vars must be kept")
		})
	}
}

// failingDeleteBackend is a secret backend whose Delete always fails.
type failingDeleteBackend struct {
	secret.SecretBackend
}

func (b *failingDeleteBackend) Delete(context.Context, string, string, string) error {
	return errors.New("backend unavailable")
}

func TestDeleteUser_SecretBackendDeleteErrorStillSucceeds(t *testing.T) {
	srv, s := testServer(t)
	b := useLocalSecretBackend(t, srv, s)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	seedUserScopedData(t, s, b, dave.ID)
	srv.SetSecretBackend(&failingDeleteBackend{SecretBackend: b})
	logs := captureSlogDefault(t)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := s.GetUser(context.Background(), dave.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "the delete must commit despite the backend error")
	sec, ev := userScopedCounts(t, s, dave.ID)
	assert.Equal(t, 1, sec, "the secret the backend failed to delete is left for the sweep")
	assert.Zero(t, ev, "env vars are still removed")
	out := logs.String()
	assert.True(t, strings.Contains(out, "level=WARN") &&
		strings.Contains(out, "failed to remove user-scope secret") &&
		strings.Contains(out, "backend unavailable"), out)
}

func TestSweepOrphanedUserScopedData(t *testing.T) {
	srv, s := testServer(t)
	b := useLocalSecretBackend(t, srv, s)
	live := newActiveMember(t, s, "user-erin", "erin@test.com")
	missing := tid("user-gone")
	seedUserScopedData(t, s, b, live.ID)
	seedUserScopedData(t, s, b, missing)

	n, err := srv.sweepOrphanedUserScopedData(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	sec, ev := userScopedCounts(t, s, missing)
	assert.Zero(t, sec, "a missing user's secrets must be swept")
	assert.Zero(t, ev, "a missing user's env vars must be swept")
	sec, ev = userScopedCounts(t, s, live.ID)
	assert.Equal(t, 1, sec, "a live user's secrets must be kept")
	assert.Equal(t, 1, ev, "a live user's env vars must be kept")

	// Idempotent.
	n, err = srv.sweepOrphanedUserScopedData(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
}

// sweepGetUserErrStore fails GetUser with a non-not-found error.
type sweepGetUserErrStore struct {
	store.Store
}

func (s *sweepGetUserErrStore) GetUser(context.Context, string) (*store.User, error) {
	return nil, errors.New("database unavailable")
}

func TestSweepOrphanedUserScopedData_LookupErrorKeepsValues(t *testing.T) {
	srv, s := testServer(t)
	b := useLocalSecretBackend(t, srv, s)
	missing := tid("user-gone")
	seedUserScopedData(t, s, b, missing)
	srv.store = &sweepGetUserErrStore{Store: s}
	t.Cleanup(func() { srv.store = s })

	n, err := srv.sweepOrphanedUserScopedData(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	sec, ev := userScopedCounts(t, s, missing)
	assert.Equal(t, 1, sec)
	assert.Equal(t, 1, ev)
}
