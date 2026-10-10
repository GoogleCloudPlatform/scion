//go:build !no_sqlite

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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureTestLoginEmail is the synthetic user the real-store tests sign in as.
const fixtureTestLoginEmail = "testlogin-fixture@example.com"

// newRealStoreTestLogin returns a test-login WebServer backed by a migrated,
// seeded store (hub-members group and hub-viewer role exist).
func newRealStoreTestLogin(t *testing.T) (*WebServer, *UserTokenService, store.Store) {
	t.Helper()
	_, s := testServer(t)
	ws := NewWebServer(WebServerConfig{EnableTestLogin: true})
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	ws.SetStore(s)
	return ws, tokenSvc, s
}

func testLoginAudits(t *testing.T, s store.Store) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testLoginMutationType})
	require.NoError(t, err)
	return recs
}

func allMutationAudits(t *testing.T, s store.Store) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{})
	require.NoError(t, err)
	return recs
}

// writeTrackingStore wraps a store and records every call to a write method
// the test-login path could reach (user row, hub grants, audit, and the
// transaction that holds them).
type writeTrackingStore struct {
	store.Store
	mu     sync.Mutex
	writes []string
}

func (w *writeTrackingStore) record(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, name)
}

func (w *writeTrackingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	w.record("WithTx")
	return w.Store.WithTx(ctx, fn)
}

func (w *writeTrackingStore) CreateUser(ctx context.Context, u *store.User) error {
	w.record("CreateUser")
	return w.Store.CreateUser(ctx, u)
}

func (w *writeTrackingStore) UpdateUser(ctx context.Context, u *store.User) error {
	w.record("UpdateUser")
	return w.Store.UpdateUser(ctx, u)
}

func (w *writeTrackingStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	w.record("CreateMutationAudit")
	return w.Store.CreateMutationAudit(ctx, r)
}

func (w *writeTrackingStore) AddGroupMember(ctx context.Context, m *store.GroupMember) error {
	w.record("AddGroupMember")
	return w.Store.AddGroupMember(ctx, m)
}

func (w *writeTrackingStore) RemoveGroupMember(ctx context.Context, groupID, memberType, memberID string) error {
	w.record("RemoveGroupMember")
	return w.Store.RemoveGroupMember(ctx, groupID, memberType, memberID)
}

func (w *writeTrackingStore) CreateRoleBinding(ctx context.Context, rb *store.RoleBinding) (*store.RoleBinding, error) {
	w.record("CreateRoleBinding")
	return w.Store.CreateRoleBinding(ctx, rb)
}

func (w *writeTrackingStore) DeleteRoleBinding(ctx context.Context, id string) error {
	w.record("DeleteRoleBinding")
	return w.Store.DeleteRoleBinding(ctx, id)
}

// testLoginUserSnapshot is everything a refused createOnly call must leave
// untouched: the user row, its hub grants, and the audit table.
type testLoginUserSnapshot struct {
	User     *store.User
	Grants   hubRoleGrantState
	Bindings []*store.RoleBinding
	Audits   []*store.MutationAuditRecord
}

func snapshotTestLoginUser(t *testing.T, s store.Store, email string) testLoginUserSnapshot {
	t.Helper()
	ctx := context.Background()
	u, err := s.GetUserByEmail(ctx, email)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
	require.NoError(t, err)
	return testLoginUserSnapshot{
		User:     u,
		Grants:   observeHubRoleGrants(t, s, u.ID),
		Bindings: bindings,
		Audits:   allMutationAudits(t, s),
	}
}

// AC: createOnly with an existing email returns 409 before any store write;
// the row (role, display name), its hub grants and the audit table are
// unchanged.
func TestHandleTestLogin_CreateOnlyExisting_ConflictBeforeAnyWrite(t *testing.T) {
	ws, svc, s := newRealStoreTestLogin(t)

	// Existing member with hub-members membership and one audit row.
	decodeTestLoginResponse(t, doTestLogin(t, ws, svc,
		`{"email":"`+fixtureTestLoginEmail+`","role":"member","displayName":"Fixture User"}`, ""))
	before := snapshotTestLoginUser(t, s, fixtureTestLoginEmail)
	require.Equal(t, hubRoleGrantState{InHubMembers: true}, before.Grants)
	require.Len(t, testLoginAudits(t, s), 1)

	tracker := &writeTrackingStore{Store: s}
	ws.SetStore(tracker)

	// A different role and display name: any write would be visible.
	rec := doTestLogin(t, ws, svc,
		`{"email":"`+fixtureTestLoginEmail+`","role":"viewer","displayName":"Changed","createOnly":true}`, "")
	assertTestLoginJSONError(t, rec, http.StatusConflict, ErrCodeConflict, "user already exists")

	assert.Empty(t, tracker.writes, "a refused createOnly call must not reach any store write")
	assert.Empty(t, rec.Result().Cookies(), "a refused call must not set a session")

	after := snapshotTestLoginUser(t, s, fixtureTestLoginEmail)
	assert.Equal(t, before, after, "user row, hub grants and audits must be unchanged")
	assert.Equal(t, store.UserRoleMember, after.User.Role)
	assert.Equal(t, "Fixture User", after.User.DisplayName)
}

// createOnly on a fresh email creates the user as usual.
func TestHandleTestLogin_CreateOnlyFresh_Creates(t *testing.T) {
	ws, svc, s := newRealStoreTestLogin(t)

	resp := decodeTestLoginResponse(t, doTestLogin(t, ws, svc,
		`{"email":"`+fixtureTestLoginEmail+`","role":"viewer","createOnly":true}`, ""))
	assert.True(t, resp.Created)
	assert.Equal(t, store.UserRoleViewer, resp.User.Role)

	u, err := s.GetUserByEmail(context.Background(), fixtureTestLoginEmail)
	require.NoError(t, err)
	assert.Equal(t, resp.User.ID, u.ID)
	assert.Equal(t, hubRoleGrantState{HubViewerBindings: 1}, observeHubRoleGrants(t, s, u.ID))
	assert.Len(t, testLoginAudits(t, s), 1)
}

// AC: created is true on a fresh email and false on an existing one.
func TestHandleTestLogin_CreatedField(t *testing.T) {
	ws, svc, _ := newRealStoreTestLogin(t)
	body := `{"email":"` + fixtureTestLoginEmail + `","role":"member"}`

	first := decodeTestLoginResponse(t, doTestLogin(t, ws, svc, body, ""))
	assert.True(t, first.Created, "fresh email")

	second := decodeTestLoginResponse(t, doTestLogin(t, ws, svc, body, ""))
	assert.False(t, second.Created, "existing email")
	assert.Equal(t, first.User.ID, second.User.ID)
}

// AC: exactly one mutation_audits row per successful call, type test_login,
// carrying the uid and the old and new role (no old role on create).
func TestHandleTestLogin_AuditRowPerSuccessfulCall(t *testing.T) {
	ws, svc, s := newRealStoreTestLogin(t)

	created := decodeTestLoginResponse(t, doTestLogin(t, ws, svc,
		`{"email":"`+fixtureTestLoginEmail+`","role":"member","displayName":"Fixture User"}`, ""))
	uid := created.User.ID

	audits := testLoginAudits(t, s)
	require.Len(t, audits, 1, "one audit row after the first call")
	a := audits[0]
	assert.Equal(t, testLoginMutationType, a.MutationType)
	assert.Equal(t, "user", a.TargetType)
	assert.Equal(t, uid, a.TargetID)
	assert.Empty(t, a.BeforeSummary, "no old role on create")
	assert.Equal(t, "system", a.ActorPrincipalKind)
	assert.Equal(t, testLoginAuditActorID, a.ActorPrincipalID)
	assert.Equal(t, testLoginAuditCredentialType, a.ActorCredentialType)
	assert.Empty(t, a.ActorCredentialID, "the challenge credential is recorded by type only")
	var after map[string]any
	require.NoError(t, json.Unmarshal([]byte(a.AfterSummary), &after))
	assert.Equal(t, store.UserRoleMember, after["role"])
	assert.Equal(t, true, after["created"])
	assert.NotContains(t, a.AfterSummary, fixtureTestLoginEmail, "no personal data in the audit summary")
	assert.NotContains(t, a.AfterSummary, "Fixture User", "no personal data in the audit summary")
	assert.ElementsMatch(t, []string{"role", "created"}, testLoginSummaryKeys(after))

	// Role change on an existing user.
	decodeTestLoginResponse(t, doTestLogin(t, ws, svc,
		`{"email":"`+fixtureTestLoginEmail+`","role":"admin"}`, ""))
	audits = testLoginAudits(t, s)
	require.Len(t, audits, 2, "one audit row per successful call")
	var update *store.MutationAuditRecord
	for _, r := range audits {
		if r.ID != a.ID {
			update = r
		}
	}
	require.NotNil(t, update)
	assert.Equal(t, testLoginMutationType, update.MutationType)
	assert.Equal(t, uid, update.TargetID)
	var beforeSum, afterSum map[string]any
	require.NoError(t, json.Unmarshal([]byte(update.BeforeSummary), &beforeSum))
	require.NoError(t, json.Unmarshal([]byte(update.AfterSummary), &afterSum))
	assert.Equal(t, store.UserRoleMember, beforeSum["role"], "old role")
	assert.Equal(t, store.UserRoleAdmin, afterSum["role"], "new role")
	assert.Equal(t, false, afterSum["created"])

	// Refused and invalid calls add no audit rows.
	assertTestLoginJSONError(t, doTestLogin(t, ws, svc,
		`{"email":"`+fixtureTestLoginEmail+`","createOnly":true}`, ""), http.StatusConflict, ErrCodeConflict, "user already exists")
	require.Equal(t, http.StatusBadRequest, doTestLogin(t, ws, svc,
		`{"email":"`+fixtureTestLoginEmail+`","role":"owner"}`, "").Code)
	assert.Len(t, testLoginAudits(t, s), 2, "failed calls must not write audit rows")
	assert.Len(t, allMutationAudits(t, s), 2, "test-login writes no other audit types")
}

// failingAuditStore fails CreateMutationAudit inside transactions, so the
// test can check that the user write rolls back with the audit.
type failingAuditStore struct {
	store.Store
}

func (f *failingAuditStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&failingAuditTx{Store: tx})
	})
}

type failingAuditTx struct {
	store.Store
}

func (f *failingAuditTx) CreateMutationAudit(context.Context, *store.MutationAuditRecord) error {
	return errors.New("injected audit failure")
}

// The audit is durable with the user write: if it cannot be written, the
// call fails and the user row is not created.
func TestHandleTestLogin_AuditFailureRollsBackUser(t *testing.T) {
	ws, svc, s := newRealStoreTestLogin(t)
	ws.SetStore(&failingAuditStore{Store: s})

	rec := doTestLogin(t, ws, svc, `{"email":"`+fixtureTestLoginEmail+`","role":"member"}`, "")
	assertTestLoginJSONError(t, rec, http.StatusInternalServerError, ErrCodeInternalError, "failed to create user")

	_, err := s.GetUserByEmail(context.Background(), fixtureTestLoginEmail)
	assert.ErrorIs(t, err, store.ErrNotFound, "user row must roll back with the failed audit")
	assert.Empty(t, testLoginAudits(t, s))
}

// tokenFragments returns the full token plus every 12-byte window of its
// payload and signature segments. The JWT header segment is skipped: it is
// the same for every token the service signs and carries no secret.
func tokenFragments(token string) []string {
	frags := []string{token}
	parts := strings.Split(token, ".")
	for i, p := range parts {
		if i == 0 && len(parts) == 3 {
			continue
		}
		const win = 12
		for j := 0; j+win <= len(p); j++ {
			frags = append(frags, p[j:j+win])
		}
	}
	return frags
}

func assertNoTokenFragment(t *testing.T, where, haystack string, tokens map[string]string) {
	t.Helper()
	for name, tok := range tokens {
		for _, frag := range tokenFragments(tok) {
			if strings.Contains(haystack, frag) {
				t.Errorf("%s contains a substring of the %s", where, name)
				break
			}
		}
	}
}

// AC: no token or token substring in the audit row, the logs, or any
// response field other than accessToken/refreshToken.
func TestHandleTestLogin_NoTokenInAuditLogsOrResponse(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ws, svc, s := newRealStoreTestLogin(t)

	challenge := testLoginAuthHeader(t, svc)
	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", challenge)
		rec := httptest.NewRecorder()
		ws.handleTestLogin(rec, req)
		return rec
	}

	tokens := map[string]string{"challenge token": strings.TrimPrefix(challenge, "Bearer ")}
	var bodies []string
	for i, body := range []string{
		`{"email":"` + fixtureTestLoginEmail + `","role":"member"}`, // create
		`{"email":"` + fixtureTestLoginEmail + `","role":"admin"}`,  // update
	} {
		rec := call(body)
		resp := decodeTestLoginResponse(t, rec)
		require.NotEmpty(t, resp.AccessToken)
		require.NotEmpty(t, resp.RefreshToken)
		tokens[fmt.Sprintf("access token %d", i)] = resp.AccessToken
		tokens[fmt.Sprintf("refresh token %d", i)] = resp.RefreshToken

		var raw map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
		delete(raw, "accessToken")
		delete(raw, "refreshToken")
		rest, err := json.Marshal(raw)
		require.NoError(t, err)
		bodies = append(bodies, string(rest))
	}
	// A refused call too.
	require.Equal(t, http.StatusConflict, call(`{"email":"`+fixtureTestLoginEmail+`","createOnly":true}`).Code)

	audits := testLoginAudits(t, s)
	require.Len(t, audits, 2)
	for _, a := range audits {
		raw, err := json.Marshal(a)
		require.NoError(t, err)
		assertNoTokenFragment(t, "audit row", string(raw), tokens)
	}
	for _, b := range bodies {
		assertNoTokenFragment(t, "response (other fields)", b, tokens)
	}
	assertNoTokenFragment(t, "logs", logs.String(), tokens)
}

// racingCreateStore simulates a concurrent test-login that creates the same
// email between this call's lookup and its transaction: WithTx first
// inserts the competing user through the base store, so the in-transaction
// CreateUser hits the real unique constraint.
type racingCreateStore struct {
	store.Store
	competitor *store.User
	raced      bool
	raceErr    error // error inserting the competitor, if any
}

func (r *racingCreateStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !r.raced {
		r.raced = true
		if err := r.CreateUser(ctx, r.competitor); err != nil {
			r.raceErr = err
			return err
		}
	}
	return r.Store.WithTx(ctx, fn)
}

func TestHandleTestLogin_ConcurrentCreateRace(t *testing.T) {
	for _, tc := range []struct {
		name       string
		createOnly bool
		status     int
		code       string
		message    string
	}{
		{"createOnly returns 409", true, http.StatusConflict, ErrCodeConflict, "user already exists"},
		{"without createOnly returns 500", false, http.StatusInternalServerError, ErrCodeInternalError, "failed to create user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, svc, s := newRealStoreTestLogin(t)
			competitor := &store.User{
				ID:          tid("competing-user"),
				Email:       fixtureTestLoginEmail,
				DisplayName: "Competitor",
				Role:        store.UserRoleViewer,
				Status:      store.UserStatusActive,
				Created:     time.Now().Add(-time.Minute).UTC().Truncate(time.Second),
			}
			racer := &racingCreateStore{Store: s, competitor: competitor}
			ws.SetStore(racer)

			rec := doTestLogin(t, ws, svc, fmt.Sprintf(
				`{"email":%q,"role":"admin","displayName":"Mine","createOnly":%t}`, fixtureTestLoginEmail, tc.createOnly), "")
			require.True(t, racer.raced, "the competing insert must have run")
			require.NoError(t, racer.raceErr, "the competing insert must succeed so the handler's insert is the one that conflicts")
			assertTestLoginJSONError(t, rec, tc.status, tc.code, tc.message)
			assert.Empty(t, rec.Result().Cookies(), "no session on a failed call")
			assert.Empty(t, testLoginAudits(t, s), "no test_login audit row on a failed call")

			got, err := s.GetUserByEmail(context.Background(), fixtureTestLoginEmail)
			require.NoError(t, err)
			assert.Equal(t, competitor.ID, got.ID, "competing row is the one stored")
			assert.Equal(t, store.UserRoleViewer, got.Role, "competing row role unchanged")
			assert.Equal(t, "Competitor", got.DisplayName, "competing row display name unchanged")
		})
	}
}

// AC (e): the test_fixture refusal hook is a no-op until Phase 2a adds the
// kind field: it refuses no user, and signing in as an existing user of
// any role still works.
func TestHandleTestLogin_FixtureRefusalIsNoOp(t *testing.T) {
	for _, u := range []*store.User{
		{},
		{ID: "u1", Email: "a@example.com", Role: store.UserRoleAdmin, Status: store.UserStatusActive},
		{ID: "u2", Email: "b@example.com", Role: store.UserRoleViewer, Status: "suspended"},
	} {
		assert.False(t, testLoginRefusesUser(u), "no user is refused before Phase 2a")
	}

	ws, svc, _ := newRealStoreTestLogin(t)
	for _, role := range []string{store.UserRoleViewer, store.UserRoleMember, store.UserRoleAdmin} {
		resp := decodeTestLoginResponse(t, doTestLogin(t, ws, svc,
			`{"email":"`+fixtureTestLoginEmail+`","role":"`+role+`"}`, ""))
		assert.Equal(t, role, resp.User.Role)
	}
}

func testLoginSummaryKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
