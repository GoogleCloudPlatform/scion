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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	s, err := newTestStore(t, ":memory:")
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

// blockingUserScopeListStore blocks the sweep's unscoped list of user-scope
// env vars until release is closed or the caller's ctx ends.
type blockingUserScopeListStore struct {
	store.Store
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

// Release unblocks the list. It is safe to call more than once.
func (s *blockingUserScopeListStore) Release() {
	s.releaseOnce.Do(func() { close(s.release) })
}

// DB forwards to the wrapped store's raw *sql.DB so New()'s D4
// membership-index migration runs against the real store.
func (s *blockingUserScopeListStore) DB() *sql.DB {
	if p, ok := s.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (s *blockingUserScopeListStore) ListEnvVars(ctx context.Context, filter store.EnvVarFilter) ([]store.EnvVar, error) {
	if filter.Scope == store.ScopeUser && filter.ScopeID == "" {
		s.enteredOnce.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.ListEnvVars(ctx, filter)
}

// TestNew_UserScopedDataSweepLookupRunsInBackground: the sweep's database
// lookups run in the background goroutine, so New() returns while they are
// still blocked and the done channel closes only after they finish.
func TestNew_UserScopedDataSweepLookupRunsInBackground(t *testing.T) {
	inner, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, inner.Migrate(ctx))
	missing := tid("user-gone")
	_, err = inner.UpsertEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "LOG_LEVEL", Value: "debug",
		Scope: store.ScopeUser, ScopeID: missing, InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)
	bs := &blockingUserScopeListStore{Store: inner, entered: make(chan struct{}), release: make(chan struct{})}

	// New() runs in a goroutine: a lookup inside New() would block it until
	// release, so it would not return before the hang guard fires.
	type newResult struct {
		srv *Server
		err error
	}
	built := make(chan newResult, 1)
	go func() {
		srv, err := New(testServerConfig(), bs)
		built <- newResult{srv, err}
	}()
	var res newResult
	select {
	case res = <-built:
	case <-time.After(30 * time.Second):
		t.Cleanup(func() {
			bs.Release()
			if r := <-built; r.srv != nil {
				select {
				case <-r.srv.userScopedDataSweepDone:
				case <-time.After(30 * time.Second):
					t.Error("startup sweep did not end after release")
				}
				_ = r.srv.Shutdown(context.Background())
			}
			_ = inner.Close()
		})
		t.Fatal("New() must not wait for the sweep lookup")
	}
	if res.srv != nil {
		t.Cleanup(func() {
			_ = res.srv.Shutdown(context.Background())
			_ = inner.Close()
		})
	} else {
		t.Cleanup(func() { _ = inner.Close() })
	}
	require.NoError(t, res.err)
	srv := res.srv
	require.NotNil(t, srv.userScopedDataSweepDone)
	// Registered after the Shutdown/Close cleanup, so it runs first: the
	// sweep goroutine ends before the store closes, on every path.
	t.Cleanup(func() {
		bs.Release()
		select {
		case <-srv.userScopedDataSweepDone:
		case <-time.After(30 * time.Second):
			t.Error("startup sweep did not end after release")
		}
	})

	select {
	case <-bs.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("startup sweep lookup did not start")
	}
	select {
	case <-srv.userScopedDataSweepDone:
		t.Fatal("startup sweep finished while its lookup was blocked")
	default:
	}

	bs.Release()
	select {
	case <-srv.userScopedDataSweepDone:
	case <-time.After(30 * time.Second):
		t.Fatal("startup sweep did not finish")
	}
	_, ev := userScopedCounts(t, inner, missing)
	assert.Zero(t, ev, "the startup sweep must remove a missing user's env vars")
}

// ctxFreeUserScopeListStore runs the user-scope lists without the caller's
// cancellation, so only the user lookups see an ended ctx.
type ctxFreeUserScopeListStore struct {
	store.Store
}

func (s *ctxFreeUserScopeListStore) ListEnvVars(ctx context.Context, filter store.EnvVarFilter) ([]store.EnvVar, error) {
	return s.Store.ListEnvVars(context.WithoutCancel(ctx), filter)
}

func (s *ctxFreeUserScopeListStore) ListSecrets(ctx context.Context, filter store.SecretFilter) ([]store.Secret, error) {
	return s.Store.ListSecrets(context.WithoutCancel(ctx), filter)
}

// TestSweepOrphanedUserScopedData_ExpiredContextKeepsValues: when the sweep's
// ctx has ended by the time of the user lookups, the sweep returns the ctx
// error and removes nothing.
func TestSweepOrphanedUserScopedData_ExpiredContextKeepsValues(t *testing.T) {
	srv, s := testServer(t)
	b := useLocalSecretBackend(t, srv, s)
	missing := tid("user-gone")
	seedUserScopedData(t, s, b, missing)
	srv.store = &ctxFreeUserScopeListStore{Store: s}
	t.Cleanup(func() { srv.store = s })

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	removed, kept, err := srv.sweepOrphanedUserScopedData(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "user lookup")
	assert.Zero(t, removed)
	assert.Zero(t, kept)
	sec, ev := userScopedCounts(t, s, missing)
	assert.Equal(t, 1, sec, "an expired sweep must keep the secrets")
	assert.Equal(t, 1, ev, "an expired sweep must keep the env vars")
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
// deleted cannot be restored (409). A descendant (owned by an agent, the
// user at its ancestry root) is checked against its root user only: with
// its parent agent gone (purged) it is refused when the root user is
// deleted and restored when the root user exists.
func TestRestoreAgent_OwnerUserDeletedRefused(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	daveAgent := softDeletedAgent(t, s, "dave-agent", project.ID, dave.ID, []string{dave.ID})
	// Sub-agents owned by an agent that is gone (purged): not a user owner.
	parentID := tid("agent-gone-parent")
	child := softDeletedAgent(t, s, "child-agent", project.ID, parentID, []string{dave.ID, parentID})
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	erinParentID := tid("agent-gone-erin-parent")
	erinChild := softDeletedAgent(t, s, "erin-child-agent", project.ID, erinParentID, []string{erin.ID, erinParentID})

	// The soft-deleted agent does not block the delete.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+daveAgent.ID+"/restore", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "the user or agent it belongs to no longer exists")
	got, err := s.GetAgent(ctx, daveAgent.ID)
	require.NoError(t, err)
	assert.False(t, got.DeletedAt.IsZero(), "a refused restore must leave the agent deleted")

	// The root user dave is gone: refused like dave's own agent.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+child.ID+"/restore", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "the user or agent it belongs to no longer exists")
	got, err = s.GetAgent(ctx, child.ID)
	require.NoError(t, err)
	assert.False(t, got.DeletedAt.IsZero(), "a refused restore must leave the agent deleted")

	// The root user erin exists: the purged parent does not block it.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+erinChild.ID+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetAgent(ctx, erinChild.ID)
	require.NoError(t, err)
	assert.True(t, got.DeletedAt.IsZero(), "a descendant whose root user exists is restored")
}

// TestRestoreAgent_ScheduledAgentOfDeletedUserRefused: a soft-deleted agent
// the scheduler dispatched for a user's schedule (no owner, the user only
// as CreatedBy) cannot be restored once that user is deleted (409), the
// same as the user's own agents; one whose creator user exists is restored.
func TestRestoreAgent_ScheduledAgentOfDeletedUserRefused(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	daves := scheduledAgent("dave-sched", project.ID, dave.ID, "stopped")
	daves.DeletedAt = time.Now().Add(-time.Hour)
	require.NoError(t, s.CreateAgent(ctx, daves))
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	erins := scheduledAgent("erin-sched", project.ID, erin.ID, "stopped")
	erins.DeletedAt = time.Now().Add(-time.Hour)
	require.NoError(t, s.CreateAgent(ctx, erins))

	// The soft-deleted scheduled agent does not block the delete.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+daves.ID+"/restore", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
	assert.Equal(t, "cannot restore the agent: the user or agent it belongs to no longer exists", resp.Error.Message)
	got, err := s.GetAgent(ctx, daves.ID)
	require.NoError(t, err)
	assert.False(t, got.DeletedAt.IsZero(), "a refused restore must leave the agent deleted")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+erins.ID+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
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

// TestCommitAgentCreate_DescendantRootUserMissing: a descendant create (the
// owner is an existing parent agent) whose ancestry root user no longer
// exists fails in the create transaction and writes nothing.
func TestCommitAgentCreate_DescendantRootUserMissing(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	gone := tid("user-gone")
	parent := &store.Agent{ID: tid("agent-live-parent"), Slug: "live-parent", Name: "live-parent",
		ProjectID: project.ID, Phase: "running", OwnerID: gone, CreatedBy: gone, Ancestry: []string{gone}}
	require.NoError(t, s.CreateAgent(ctx, parent))

	a := &store.Agent{ID: tid("agent-orphan-child"), Slug: "orphan-child", Name: "orphan-child",
		ProjectID: project.ID, Phase: "created", OwnerID: parent.ID, CreatedBy: parent.ID,
		Ancestry: []string{gone, parent.ID}}
	err := srv.commitAgentCreate(ctx, agentCreateWrite{
		Provenance: store.AuthorityProvenance{ProvenanceVersion: 1},
		Agent:      a,
		Slug:       a.Slug,
		Edge: &store.DelegationEdge{DelegatorType: store.DelegationPrincipalAgent, DelegatorID: parent.ID,
			DelegateType: store.DelegationPrincipalAgent, ScopeType: store.RoleScopeProject,
			ScopeID: project.ID, Role: string(AgentRoleNone), Active: true},
		Audit: &store.MutationAuditRecord{MutationType: mutationTypeAgentDelegation},
	})
	require.ErrorIs(t, err, errAgentOwnerUserMissing)
	_, err = s.GetAgent(ctx, a.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a refused create must write no agent")
}

// TestDeleteUser_DescendantAgentDenied: an agent started by one of the
// user's agents (the user is its ancestry root) blocks the delete even after
// the user's own agent is soft-deleted; a soft-deleted descendant does not.
func TestDeleteUser_DescendantAgentDenied(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	parent := &store.Agent{ID: tid("agent-dave-parent"), Slug: "dave-parent", Name: "dave-parent",
		ProjectID: project.ID, Phase: "running", OwnerID: dave.ID, CreatedBy: dave.ID,
		Ancestry: []string{dave.ID}}
	require.NoError(t, s.CreateAgent(ctx, parent))
	child := &store.Agent{ID: tid("agent-dave-child"), Slug: "dave-child", Name: "dave-child",
		ProjectID: project.ID, Phase: "stopped", OwnerID: parent.ID, CreatedBy: parent.ID,
		Ancestry: []string{dave.ID, parent.ID}}
	require.NoError(t, s.CreateAgent(ctx, child))
	// A soft-deleted descendant does not count.
	softDeletedAgent(t, s, "dave-gone-child", project.ID, parent.ID, []string{dave.ID, parent.ID})

	// Both are listed once each (the parent matches both the owner and the
	// ancestry query).
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	requireOwnsAgentsDenial(t, rec, parent, child)

	now := time.Now()
	_, err := s.UpdateAgentDeletion(ctx, parent.ID, store.DeletionPredicate{},
		store.DeletionFields{DeletedAt: &now})
	require.NoError(t, err)

	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	requireOwnsAgentsDenial(t, rec, child)
	_, err = s.GetUser(ctx, dave.ID)
	require.NoError(t, err, "denied delete must keep the user")
}

// scheduledAgent returns an agent shaped like the scheduler's dispatch
// (dispatchAgentEventHandler): no owner, no ancestry, and the schedule's
// creator recorded only as CreatedBy.
func scheduledAgent(slug, projectID, createdBy, phase string) *store.Agent {
	return &store.Agent{ID: tid("agent-" + slug), Slug: slug, Name: slug, ProjectID: projectID,
		Phase: phase, Detached: true, CreatedBy: createdBy}
}

// TestDeleteUser_ScheduledAgentDenied: an agent the scheduler dispatched for
// the user's schedule (OwnerID empty, CreatedBy the user) blocks the delete
// on both delete paths, stopped or not; a soft-deleted one does not, and
// neither does an agent with another owner that names the user as creator.
func TestDeleteUser_ScheduledAgentDenied(t *testing.T) {
	t.Run("users delete", func(t *testing.T) {
		srv, s, _, _, project := setupDemoPolicyTest(t)
		ctx := context.Background()
		dave := newActiveMember(t, s, "user-dave", "dave@test.com")
		running := scheduledAgent("dave-sched-running", project.ID, dave.ID, "running")
		require.NoError(t, s.CreateAgent(ctx, running))
		stopped := scheduledAgent("dave-sched-stopped", project.ID, dave.ID, "stopped")
		require.NoError(t, s.CreateAgent(ctx, stopped))
		gone := scheduledAgent("dave-sched-gone", project.ID, dave.ID, "stopped")
		gone.DeletedAt = time.Now().Add(-time.Hour)
		require.NoError(t, s.CreateAgent(ctx, gone))
		erin := newActiveMember(t, s, "user-erin", "erin@test.com")
		erins := scheduledAgent("erin-owned", project.ID, dave.ID, "running")
		erins.OwnerID = erin.ID
		require.NoError(t, s.CreateAgent(ctx, erins))

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
		requireOwnsAgentsDenial(t, rec, running, stopped)
		_, err := s.GetUser(ctx, dave.ID)
		require.NoError(t, err, "denied delete must keep the user")

		require.NoError(t, s.DeleteAgent(ctx, running.ID))
		require.NoError(t, s.DeleteAgent(ctx, stopped.ID))
		rec = doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	})
	t.Run("allow-list delete", func(t *testing.T) {
		srv, s, _, _, project := setupDemoPolicyTest(t)
		ctx := context.Background()
		carol := newInvitedUser(t, s, "user-carol", "carol@test.com")
		a := scheduledAgent("carol-sched", project.ID, carol.ID, "running")
		require.NoError(t, s.CreateAgent(ctx, a))

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
		requireOwnsAgentsDenial(t, rec, a)
		_, err := s.GetUser(ctx, carol.ID)
		require.NoError(t, err, "denied delete must keep the invited user")
	})
}

// commitScheduledCreate writes a, a scheduler-shaped agent, through
// commitAgentCreate as the scheduled dispatch does: its delegation edge's
// delegator is the creator, of kind creatorKind.
func commitScheduledCreate(ctx context.Context, srv *Server, a *store.Agent, creatorKind string) error {
	return srv.commitAgentCreate(ctx, agentCreateWrite{
		Provenance: store.AuthorityProvenance{ProvenanceVersion: 1},
		Agent:      a,
		Slug:       a.Slug,
		Edge: &store.DelegationEdge{DelegatorType: creatorKind, DelegatorID: a.CreatedBy,
			DelegateType: store.DelegationPrincipalAgent, ScopeType: store.RoleScopeProject,
			ScopeID: a.ProjectID, Role: string(AgentRoleNone), Active: true},
		Audit: &store.MutationAuditRecord{MutationType: mutationTypeAgentDelegation},
	})
}

// TestScheduledCreate_CreatorUserMissing: a scheduler-shaped create whose
// creator is neither a user nor an agent fails closed and writes nothing; one
// whose creator is an existing agent (an agent's schedule) succeeds.
func TestScheduledCreate_CreatorUserMissing(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	r := newUserLockRecordingStore(s)
	srv.store = r

	a := scheduledAgent("gone-sched", project.ID, tid("user-gone"), "created")
	err := commitScheduledCreate(ctx, srv, a, store.DelegationPrincipalUser)
	require.ErrorIs(t, err, errAgentOwnerUserMissing)
	_, err = s.GetAgent(ctx, a.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a refused create must write no agent")

	creator := &store.Agent{ID: tid("agent-sched-creator"), Slug: "sched-creator", Name: "sched-creator",
		ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, creator))
	b := scheduledAgent("agent-sched", project.ID, creator.ID, "created")
	require.NoError(t, commitScheduledCreate(ctx, srv, b, store.DelegationPrincipalAgent))
}

// userLockRecordingStore records, in order, the user-row locks, agent
// lists and agent writes made through it, including inside WithTx.
// LockUserRow fails with lockErr when it is set and lockErrFor is empty or
// names the locked ID.
type userLockRecordingStore struct {
	store.Store
	mu         *sync.Mutex
	events     *[]string
	lockErr    error
	lockErrFor string
}

func newUserLockRecordingStore(s store.Store) *userLockRecordingStore {
	return &userLockRecordingStore{Store: s, mu: &sync.Mutex{}, events: &[]string{}}
}

func (r *userLockRecordingStore) record(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.events = append(*r.events, e)
}

// index returns the position of the first event equal to e, or -1.
func (r *userLockRecordingStore) index(e string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, got := range *r.events {
		if got == e {
			return i
		}
	}
	return -1
}

func (r *userLockRecordingStore) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), *r.events...)
}

func (r *userLockRecordingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return r.Store.WithTx(ctx, func(tx store.Store) error {
		c := *r
		c.Store = tx
		return fn(&c)
	})
}

func (r *userLockRecordingStore) LockUserRow(ctx context.Context, id string, exclusive bool) error {
	r.record(fmt.Sprintf("lock:%s:%t", id, exclusive))
	if r.lockErr != nil && (r.lockErrFor == "" || r.lockErrFor == id) {
		return r.lockErr
	}
	return r.Store.LockUserRow(ctx, id, exclusive)
}

func (r *userLockRecordingStore) ListAgents(ctx context.Context, f store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	switch {
	case f.OwnerID != "":
		r.record("list:owner:" + f.OwnerID)
	case f.AncestorID != "":
		r.record("list:ancestor:" + f.AncestorID)
	case f.CreatedBy != "":
		r.record("list:createdby:" + f.CreatedBy)
	}
	return r.Store.ListAgents(ctx, f, opts)
}

func (r *userLockRecordingStore) CreateAgent(ctx context.Context, a *store.Agent) error {
	r.record("create:" + a.Slug)
	return r.Store.CreateAgent(ctx, a)
}

func (r *userLockRecordingStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	r.record("update:" + a.ID)
	return r.Store.UpdateAgent(ctx, a)
}

// requireBefore asserts both events were recorded, first before second.
func requireBefore(t *testing.T, r *userLockRecordingStore, first, second string) {
	t.Helper()
	i, j := r.index(first), r.index(second)
	require.GreaterOrEqual(t, i, 0, "%q not recorded; events: %v", first, r.snapshot())
	require.GreaterOrEqual(t, j, 0, "%q not recorded; events: %v", second, r.snapshot())
	require.Less(t, i, j, "%q must come before %q; events: %v", first, second, r.snapshot())
}

// TestUserRowLocks_DeleteExclusiveCreateRestoreShared pins where the hub
// takes the user-row lock (ptone/scion#2769): both delete paths lock the
// user's row exclusively before listing the user's agents, and agent create
// and restore lock the guard user's row (owner, ancestry root, or schedule
// creator; lockAgentGuardUserTx) shared before writing the agent. On
// SQLite the lock is a plain read, so only the call order is checked here;
// the PostgreSQL lock semantics are covered in pkg/store/integrationtest.
func TestUserRowLocks_DeleteExclusiveCreateRestoreShared(t *testing.T) {
	t.Run("users delete", func(t *testing.T) {
		srv, s, _, _, _ := setupDemoPolicyTest(t)
		dave := newActiveMember(t, s, "user-dave", "dave@test.com")
		r := newUserLockRecordingStore(s)
		srv.store = r
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		requireBefore(t, r, "lock:"+dave.ID+":true", "list:owner:"+dave.ID)
		requireBefore(t, r, "lock:"+dave.ID+":true", "list:ancestor:"+dave.ID)
		requireBefore(t, r, "lock:"+dave.ID+":true", "list:createdby:"+dave.ID)
	})
	t.Run("allow-list delete", func(t *testing.T) {
		srv, s, _, _, _ := setupDemoPolicyTest(t)
		carol := newInvitedUser(t, s, "user-carol", "carol@test.com")
		r := newUserLockRecordingStore(s)
		srv.store = r
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requireBefore(t, r, "lock:"+carol.ID+":true", "list:owner:"+carol.ID)
		requireBefore(t, r, "lock:"+carol.ID+":true", "list:ancestor:"+carol.ID)
		requireBefore(t, r, "lock:"+carol.ID+":true", "list:createdby:"+carol.ID)
	})
	t.Run("create", func(t *testing.T) {
		f := newUATCreateFixture(t, "lock-create")
		r := newUserLockRecordingStore(f.store)
		f.srv.store = r
		rec := f.create(t, authUser(f.creator), CreateAgentRequest{Name: "lock-create"})
		require.Less(t, rec.Code, 300, rec.Body.String())
		requireBefore(t, r, "lock:"+f.creator.ID+":false", "create:lock-create")
		assert.Equal(t, -1, r.index("lock:"+f.creator.ID+":true"), "create must not lock exclusively")
	})
	t.Run("descendant create", func(t *testing.T) {
		// An agent started by one of the user's agents: its owner is the
		// parent agent, so the lock goes to the ancestry root, the user.
		srv, s, _, _, project := setupDemoPolicyTest(t)
		ctx := context.Background()
		dave := newActiveMember(t, s, "user-dave", "dave@test.com")
		parent := createOwnedAgent(t, s, "dave-parent", project.ID, dave.ID)
		r := newUserLockRecordingStore(s)
		srv.store = r
		child := &store.Agent{ID: tid("agent-dave-child"), Slug: "dave-child", Name: "dave-child",
			ProjectID: project.ID, Phase: "created", OwnerID: parent.ID, CreatedBy: parent.ID,
			Ancestry: []string{dave.ID, parent.ID}}
		require.NoError(t, srv.commitAgentCreate(ctx, agentCreateWrite{
			Provenance: store.AuthorityProvenance{ProvenanceVersion: 1},
			Agent:      child,
			Slug:       child.Slug,
			Edge: &store.DelegationEdge{DelegatorType: store.DelegationPrincipalAgent, DelegatorID: parent.ID,
				DelegateType: store.DelegationPrincipalAgent, ScopeType: store.RoleScopeProject,
				ScopeID: project.ID, Role: string(AgentRoleNone), Active: true},
			Audit: &store.MutationAuditRecord{MutationType: mutationTypeAgentDelegation},
		}))
		requireBefore(t, r, "lock:"+dave.ID+":false", "create:dave-child")
		assert.Equal(t, -1, r.index("lock:"+dave.ID+":true"), "create must not lock exclusively")
	})
	t.Run("scheduled create", func(t *testing.T) {
		// The scheduler's agent has no owner or ancestry and records the
		// principal of the schedule's latest revision only as CreatedBy; the
		// lock goes to that user.
		srv, s, _, _, project := setupDemoPolicyTest(t)
		dave := newActiveMember(t, s, "user-dave", "dave@test.com")
		r := newUserLockRecordingStore(s)
		srv.store = r
		a := scheduledAgent("dave-sched", project.ID, dave.ID, "created")
		require.NoError(t, commitScheduledCreate(context.Background(), srv, a, store.DelegationPrincipalUser))
		requireBefore(t, r, "lock:"+dave.ID+":false", "create:dave-sched")
		assert.Equal(t, -1, r.index("lock:"+dave.ID+":true"), "create must not lock exclusively")
	})
	t.Run("restore", func(t *testing.T) {
		srv, s, _, _, project := setupDemoPolicyTest(t)
		dave := newActiveMember(t, s, "user-dave", "dave@test.com")
		a := softDeletedAgent(t, s, "dave-agent", project.ID, dave.ID, []string{dave.ID})
		r := newUserLockRecordingStore(s)
		srv.store = r
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restore", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requireBefore(t, r, "lock:"+dave.ID+":false", "update:"+a.ID)
		assert.Equal(t, -1, r.index("lock:"+dave.ID+":true"), "restore must not lock exclusively")
	})
	t.Run("scheduled restore", func(t *testing.T) {
		// A scheduled agent records the user only as CreatedBy; restore
		// locks that user, the same choice as the scheduled create.
		srv, s, _, _, project := setupDemoPolicyTest(t)
		dave := newActiveMember(t, s, "user-dave", "dave@test.com")
		a := scheduledAgent("dave-sched", project.ID, dave.ID, "stopped")
		a.DeletedAt = time.Now().Add(-time.Hour)
		require.NoError(t, s.CreateAgent(context.Background(), a))
		r := newUserLockRecordingStore(s)
		srv.store = r
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restore", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requireBefore(t, r, "lock:"+dave.ID+":false", "update:"+a.ID)
		assert.Equal(t, -1, r.index("lock:"+dave.ID+":true"), "restore must not lock exclusively")
	})
}

// TestCreateAgent_DescendantRootUserMissingReturns409: an agent creating a
// child through the HTTP route, when its ancestry root user's row is gone
// at commit time, gets 409 conflict; nothing is written and the quota
// reservation is released.
func TestCreateAgent_DescendantRootUserMissingReturns409(t *testing.T) {
	f := newChainFixture(t, "root-gone")
	setProjectAgentCeiling(t, f.store, 10)
	parent, _ := f.sessionParent(t, "root-gone-p")
	require.Equal(t, []string{f.creator.ID}, parent.Ancestry)
	token := f.agentToken(t, parent.ID)
	real := f.store
	r := newUserLockRecordingStore(real)
	r.lockErr = store.ErrNotFound
	r.lockErrFor = f.creator.ID
	f.srv.store = r

	rec := f.createAsParent(t, token, CreateAgentRequest{Name: "root-gone-c"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
	assert.Equal(t, "cannot create the agent: the user or agent it belongs to no longer exists", resp.Error.Message)
	require.GreaterOrEqual(t, r.index("lock:"+f.creator.ID+":false"), 0, "the create must lock the root user")

	_, err := real.GetAgentBySlug(context.Background(), f.proj.ID, "root-gone-c")
	require.ErrorIs(t, err, store.ErrNotFound, "a refused create must write no agent")
	assert.Equal(t, []string{parent.ID},
		activeReservationResources(t, real, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, f.proj.ID),
		"the refused create must release its reservation (only the parent's remains)")
}

// TestRestoreAgent_LegacyParentOwnerRestored: a legacy child whose parent
// agent had an empty ancestry records the parent as both owner and ancestry
// root ([P]). The parent is an agent, not a missing user, so the restore
// succeeds.
func TestRestoreAgent_LegacyParentOwnerRestored(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	parent := &store.Agent{ID: tid("agent-legacy-parent"), Slug: "legacy-parent", Name: "legacy-parent",
		ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, parent))
	child := softDeletedAgent(t, s, "legacy-child", project.ID, parent.ID, []string{parent.ID})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+child.ID+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(ctx, child.ID)
	require.NoError(t, err)
	assert.True(t, got.DeletedAt.IsZero())
}

// TestRestoreAgent_EmptyAncestryOwnerMissingRefused: an agent with an empty
// ancestry (recorded before ancestry was) whose owner is neither a user nor
// an agent is refused like a deleted user's agent; one whose owner user or
// owner agent exists is restored.
func TestRestoreAgent_EmptyAncestryOwnerMissingRefused(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	gone := softDeletedAgent(t, s, "legacy-gone", project.ID, tid("user-gone"), nil)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	daves := softDeletedAgent(t, s, "legacy-dave", project.ID, dave.ID, nil)
	parent := &store.Agent{ID: tid("agent-legacy-owner"), Slug: "legacy-owner", Name: "legacy-owner",
		ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, parent))
	agentOwned := softDeletedAgent(t, s, "legacy-agent-owned", project.ID, parent.ID, nil)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+gone.ID+"/restore", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "the user or agent it belongs to no longer exists")
	got, err := s.GetAgent(ctx, gone.ID)
	require.NoError(t, err)
	assert.False(t, got.DeletedAt.IsZero(), "a refused restore must leave the agent deleted")

	for _, a := range []*store.Agent{daves, agentOwned} {
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restore", nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", a.Slug, rec.Body.String())
	}
}

// TestRestoreAgent_PurgedLegacyRootAgentRefused pins an accepted limit
// (ptone/scion#2769). Legacy agent L has an empty ancestry. Its child B
// records [L], and B's soft-deleted child C records [L, B]. Once L's row is
// gone, C's guard candidate is L only, and a missing root cannot be told
// apart from a deleted user, so the restore is refused with the neutral
// text and C's row is left unchanged.
func TestRestoreAgent_PurgedLegacyRootAgentRefused(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	legacy := &store.Agent{ID: tid("agent-legacy-root"), Slug: "legacy-root", Name: "legacy-root",
		ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, legacy))
	parent := &store.Agent{ID: tid("agent-legacy-mid"), Slug: "legacy-mid", Name: "legacy-mid",
		ProjectID: project.ID, Phase: "running", OwnerID: legacy.ID, CreatedBy: legacy.ID,
		Ancestry: []string{legacy.ID}}
	require.NoError(t, s.CreateAgent(ctx, parent))
	grandchild := softDeletedAgent(t, s, "legacy-grandchild", project.ID, parent.ID,
		[]string{legacy.ID, parent.ID})
	require.NoError(t, s.DeleteAgent(ctx, legacy.ID))
	before, err := s.GetAgent(ctx, grandchild.ID)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+grandchild.ID+"/restore", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
	assert.Equal(t, "cannot restore the agent: the user or agent it belongs to no longer exists", resp.Error.Message)
	after, err := s.GetAgent(ctx, grandchild.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refused restore must not change the agent row")
}

// TestCreateAgent_OwnerUserMissingReturns409: when the owner user's row is
// gone at commit time, the create handler answers 409 conflict and releases
// the quota reservations it took.
func TestCreateAgent_OwnerUserMissingReturns409(t *testing.T) {
	f := newUATCreateFixture(t, "owner-gone")
	setProjectAgentCeiling(t, f.store, 10)
	real := f.store
	r := newUserLockRecordingStore(real)
	r.lockErr = store.ErrNotFound
	f.srv.store = r

	rec := f.create(t, authUser(f.creator), CreateAgentRequest{Name: "owner-gone"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
	assert.Equal(t, "cannot create the agent: the user or agent it belongs to no longer exists", resp.Error.Message)
	require.GreaterOrEqual(t, r.index("lock:"+f.creator.ID+":false"), 0, "the create must take the owner lock")

	_, err := real.GetAgentBySlug(context.Background(), f.proj.ID, "owner-gone")
	require.ErrorIs(t, err, store.ErrNotFound, "a refused create must write no agent")
	assert.Empty(t, activeReservationResources(t, real, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, f.proj.ID),
		"the refused create must release its project reservation")

	// Control: the same create without the fault holds a reservation, so
	// the empty list above is a release, not a reservation never taken.
	r.lockErr = nil
	rec = f.create(t, authUser(f.creator), CreateAgentRequest{Name: "owner-gone"})
	require.Less(t, rec.Code, 300, rec.Body.String())
	created, err := real.GetAgentBySlug(context.Background(), f.proj.ID, "owner-gone")
	require.NoError(t, err)
	assert.Equal(t, []string{created.ID},
		activeReservationResources(t, real, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, f.proj.ID))
}
