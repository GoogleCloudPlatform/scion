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

func TestDeleteUser_OwnsAgentDenied(t *testing.T) {
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

func TestDeprecatedAllowListDelete_OwnsAgentDenied(t *testing.T) {
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

	removed, kept, err := srv.sweepOrphanedUserScopedData(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.Zero(t, kept)

	sec, ev := userScopedCounts(t, s, missing)
	assert.Zero(t, sec, "a missing user's secrets must be swept")
	assert.Zero(t, ev, "a missing user's env vars must be swept")
	sec, ev = userScopedCounts(t, s, live.ID)
	assert.Equal(t, 1, sec, "a live user's secrets must be kept")
	assert.Equal(t, 1, ev, "a live user's env vars must be kept")

	// Idempotent.
	removed, kept, err = srv.sweepOrphanedUserScopedData(context.Background())
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.Zero(t, kept)
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

	removed, kept, err := srv.sweepOrphanedUserScopedData(context.Background())
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.Zero(t, kept)
	sec, ev := userScopedCounts(t, s, missing)
	assert.Equal(t, 1, sec)
	assert.Equal(t, 1, ev)
}

// seedNoBackendSecretRows gives scopeID three user-scope secret rows written
// directly to the store, as the backends would have left them:
//   - DB_VALUE: a value stored in the hub database (local backend, plaintext
//     legacy/dev mode; an encrypted value is stored the same way),
//   - EXT_REF: an external reference only (GCP Secret Manager: no value in
//     the database),
//   - BOTH: a value in the database that also names an external reference.
func seedNoBackendSecretRows(t *testing.T, s store.Store, scopeID string) {
	t.Helper()
	ctx := context.Background()
	for _, sec := range []*store.Secret{
		{Key: "DB_VALUE", EncryptedValue: "plaintext-value"},
		{Key: "EXT_REF", SecretRef: "gcpsm:projects/p/secrets/ext-" + scopeID},
		{Key: "BOTH", EncryptedValue: "enc-value", SecretRef: "gcpsm:projects/p/secrets/both-" + scopeID},
	} {
		sec.ID = api.NewUUID()
		sec.SecretType = "environment"
		sec.Scope = store.ScopeUser
		sec.ScopeID = scopeID
		require.NoError(t, s.CreateSecret(ctx, sec))
	}
}

// userScopedSecretKeys returns the keys of scopeID's user-scope secrets.
func userScopedSecretKeys(t *testing.T, s store.Store, scopeID string) []string {
	t.Helper()
	rows, err := s.ListSecrets(context.Background(), store.SecretFilter{Scope: store.ScopeUser, ScopeID: scopeID})
	require.NoError(t, err)
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	return keys
}

// TestDeleteUser_NoSecretBackend: with no secret backend configured, the
// deleted user's secret rows whose value is stored in the hub database are
// deleted; a row that only references an external backend is kept for a
// later sweep. A dropped external reference is logged at Warn.
func TestDeleteUser_NoSecretBackend(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(nil)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	seedNoBackendSecretRows(t, s, dave.ID)
	seedNoBackendSecretRows(t, s, erin.ID)
	_, err := s.UpsertEnvVar(context.Background(), &store.EnvVar{
		ID: api.NewUUID(), Key: "LOG_LEVEL", Value: "debug",
		Scope: store.ScopeUser, ScopeID: dave.ID, InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)
	logs := captureSlogDefault(t)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Equal(t, []string{"EXT_REF"}, userScopedSecretKeys(t, s, dave.ID),
		"rows holding a value in the hub database must be deleted; the external-only row is kept")
	_, ev := userScopedCounts(t, s, dave.ID)
	assert.Zero(t, ev, "env vars are removed")
	assert.ElementsMatch(t, []string{"DB_VALUE", "EXT_REF", "BOTH"}, userScopedSecretKeys(t, s, erin.ID),
		"another user's rows must be kept")

	out := logs.String()
	assert.True(t, strings.Contains(out, "level=WARN") &&
		strings.Contains(out, "dropping external secret reference") &&
		strings.Contains(out, "gcpsm:projects/p/secrets/both-"+dave.ID), out)

	// The kept row is retried by the sweep: with still no backend it stays
	// and the user counts as kept, not removed.
	removed, kept, err := srv.sweepOrphanedUserScopedData(context.Background())
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.Equal(t, 1, kept)
	assert.Equal(t, []string{"EXT_REF"}, userScopedSecretKeys(t, s, dave.ID))
}

// TestSweepOrphanedUserScopedData_CountsKeptSeparately: a missing user whose
// secret delete fails counts as kept, not removed.
func TestSweepOrphanedUserScopedData_CountsKeptSeparately(t *testing.T) {
	srv, s := testServer(t)
	b := useLocalSecretBackend(t, srv, s)
	gone := tid("user-gone")
	failing := tid("user-gone-2")
	seedUserScopedData(t, s, b, gone)
	seedUserScopedData(t, s, b, failing)
	srv.SetSecretBackend(&selectiveFailingDeleteBackend{SecretBackend: b, failScopeID: failing})

	removed, kept, err := srv.sweepOrphanedUserScopedData(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.Equal(t, 1, kept)
	sec, _ := userScopedCounts(t, s, gone)
	assert.Zero(t, sec)
	sec, _ = userScopedCounts(t, s, failing)
	assert.Equal(t, 1, sec)
}

// selectiveFailingDeleteBackend fails Delete for one scope ID.
type selectiveFailingDeleteBackend struct {
	secret.SecretBackend
	failScopeID string
}

func (b *selectiveFailingDeleteBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	if scopeID == b.failScopeID {
		return errors.New("backend unavailable")
	}
	return b.SecretBackend.Delete(ctx, name, scope, scopeID)
}

// TestDeleteUser_OwnsAgentDeniedListsEveryPage: the 409 lists every owned
// agent, not just the first page.
func TestDeleteUser_OwnsAgentDeniedListsEveryPage(t *testing.T) {
	prev := ownedAgentsPageSize
	ownedAgentsPageSize = 2
	t.Cleanup(func() { ownedAgentsPageSize = prev })

	srv, s, _, _, project := setupDemoPolicyTest(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	var agents []*store.Agent
	for _, slug := range []string{"dave-a", "dave-b", "dave-c", "dave-d", "dave-e"} {
		agents = append(agents, createOwnedAgent(t, s, slug, project.ID, dave.ID))
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	requireOwnsAgentsDenial(t, rec, agents...)
}

// TestNew_SchedulesUserScopedDataSweep: building a server runs the startup
// sweep in the background, removing a missing user's values.
func TestNew_SchedulesUserScopedDataSweep(t *testing.T) {
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, s.Migrate(ctx))
	missing := tid("user-gone")
	_, err = s.UpsertEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "LOG_LEVEL", Value: "debug",
		Scope: store.ScopeUser, ScopeID: missing, InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)

	srv, _ := testServerWithStore(t, s)
	require.NotNil(t, srv.userScopedDataSweepDone)
	select {
	case <-srv.userScopedDataSweepDone:
	case <-time.After(30 * time.Second):
		t.Fatal("startup sweep did not finish")
	}
	_, ev := userScopedCounts(t, s, missing)
	assert.Zero(t, ev, "the startup sweep must remove a missing user's env vars")
}

// softDeletedAgent creates a soft-deleted agent in the project.
func softDeletedAgent(t *testing.T, s store.Store, slug, projectID, ownerID string, ancestry []string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("agent-" + slug), Slug: slug, Name: slug, ProjectID: projectID,
		Phase: "stopped", OwnerID: ownerID, CreatedBy: ownerID, Ancestry: ancestry,
		AppliedConfig: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{}},
		DeletedAt:     time.Now().Add(-time.Hour),
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

// TestRestoreAgent_OwnerUserDeletedRefused: an agent whose owner user was
// deleted cannot be restored (409); agents owned by an agent principal are
// unaffected, even when that agent no longer exists.
func TestRestoreAgent_OwnerUserDeletedRefused(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	daveAgent := softDeletedAgent(t, s, "dave-agent", project.ID, dave.ID, []string{dave.ID})
	// A sub-agent owned by an agent that is gone: not a user owner.
	parentID := tid("agent-gone-parent")
	child := softDeletedAgent(t, s, "child-agent", project.ID, parentID, []string{dave.ID, parentID})

	// The soft-deleted agent does not block the delete.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+daveAgent.ID+"/restore", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "owner no longer exists")
	got, err := s.GetAgent(ctx, daveAgent.ID)
	require.NoError(t, err)
	assert.False(t, got.DeletedAt.IsZero(), "a refused restore must leave the agent deleted")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+child.ID+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetAgent(ctx, child.ID)
	require.NoError(t, err)
	assert.True(t, got.DeletedAt.IsZero(), "an agent-owned agent is restored")
}

// TestRestoreAgent_OwnerUserExistsRestored: the owner check does not block
// restoring an agent whose owner user exists.
func TestRestoreAgent_OwnerUserExistsRestored(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	a := softDeletedAgent(t, s, "dave-agent", project.ID, dave.ID, []string{dave.ID})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestCommitAgentCreate_OwnerUserMissing: an agent create whose owner user
// no longer exists fails in the create transaction and writes nothing. (The
// create tests through the HTTP route cover an owner user that exists.)
func TestCommitAgentCreate_OwnerUserMissing(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	gone := tid("user-gone")

	a := &store.Agent{ID: tid("agent-orphan"), Slug: "orphan", Name: "orphan", ProjectID: project.ID,
		Phase: "created", OwnerID: gone, CreatedBy: gone}
	err := srv.commitAgentCreate(ctx, agentCreateWrite{
		Provenance: store.AuthorityProvenance{ProvenanceVersion: 1},
		Agent:      a,
		Slug:       "orphan",
		Edge: &store.DelegationEdge{DelegatorType: store.DelegationPrincipalUser, DelegatorID: gone,
			DelegateType: store.DelegationPrincipalAgent, ScopeType: store.RoleScopeProject,
			ScopeID: project.ID, Role: string(AgentRoleNone), Active: true},
		Audit: &store.MutationAuditRecord{MutationType: mutationTypeAgentDelegation},
	})
	require.ErrorIs(t, err, errAgentOwnerUserMissing)
	_, err = s.GetAgent(ctx, a.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a refused create must write no agent")
}
