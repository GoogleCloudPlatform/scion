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

// The in-memory testLoginStore (handlers_test_login_test.go) has no
// transaction or audit support of its own. These methods let it serve the
// handler's transactional user write; audits are accepted and discarded.
// Audit behaviour is asserted against a real store below.

func (s *testLoginStore) WithTx(_ context.Context, fn func(tx store.Store) error) error {
	return fn(s)
}

func (s *testLoginStore) CreateMutationAudit(context.Context, *store.MutationAuditRecord) error {
	return nil
}

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

// doTestLogin posts body to the test-login handler with a valid challenge
// token from remoteAddr ("" keeps httptest's default).
func doTestLogin(t *testing.T, ws *WebServer, svc *UserTokenService, body, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, svc))
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	ws.handleTestLogin(rec, req)
	return rec
}

func decodeTestLoginResponse(t *testing.T, rec *httptest.ResponseRecorder) TestLoginResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp TestLoginResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
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

// The created field is also reported by the in-memory store path that the
// existing tests use, and is present in the JSON body.
func TestHandleTestLogin_CreatedField_JSON(t *testing.T) {
	ws, svc := newTestLoginWebServer(t, true)
	body := `{"email":"json@example.com"}`

	rec := doTestLogin(t, ws, svc, body, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, true, raw["created"])

	rec = doTestLogin(t, ws, svc, body, "")
	require.Equal(t, http.StatusOK, rec.Code)
	raw = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, false, raw["created"])
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
	var after map[string]any
	require.NoError(t, json.Unmarshal([]byte(a.AfterSummary), &after))
	assert.Equal(t, store.UserRoleMember, after["role"])
	assert.Equal(t, true, after["created"])

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

// AC: per-source-IP rate limit; the (N+1)th call in the window gets 429.
func TestHandleTestLogin_RateLimit(t *testing.T) {
	ws, svc := newTestLoginWebServer(t, true)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ws.testLoginLimiter.now = func() time.Time { return clock }

	const addr = "198.51.100.7:4000"
	body := `{"email":"ratelimit@example.com"}`
	for i := 0; i < testLoginRateBurst; i++ {
		require.Equal(t, http.StatusOK, doTestLogin(t, ws, svc, body, addr).Code, "call %d", i+1)
	}

	rec := doTestLogin(t, ws, svc, body, addr)
	assertTestLoginJSONError(t, rec, http.StatusTooManyRequests, ErrCodeRateLimited, "too many test-login requests")
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))

	// The limit is per source IP: another address is unaffected, and a
	// different port on the same address is the same source.
	assert.Equal(t, http.StatusOK, doTestLogin(t, ws, svc, body, "198.51.100.8:4000").Code)
	assert.Equal(t, http.StatusTooManyRequests, doTestLogin(t, ws, svc, body, "198.51.100.7:5000").Code)

	// Forwarding headers do not pick a different bucket.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(body))
	req.RemoteAddr = addr
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("Authorization", testLoginAuthHeader(t, svc))
	rec = httptest.NewRecorder()
	ws.handleTestLogin(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	// Tokens refill over time.
	clock = clock.Add(time.Duration(float64(time.Second) / testLoginRatePerSecond))
	assert.Equal(t, http.StatusOK, doTestLogin(t, ws, svc, body, addr).Code)
	assert.Equal(t, http.StatusTooManyRequests, doTestLogin(t, ws, svc, body, addr).Code)
}

// Rejected calls (here: bad challenge token) count toward the limit.
func TestHandleTestLogin_RateLimitCountsRejectedCalls(t *testing.T) {
	ws, svc := newTestLoginWebServer(t, true)
	ws.testLoginLimiter.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	const addr = "198.51.100.9:4000"
	for i := 0; i < testLoginRateBurst; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(`{}`))
		req.RemoteAddr = addr
		req.Header.Set("Authorization", "Bearer not-a-valid-token")
		rec := httptest.NewRecorder()
		ws.handleTestLogin(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	assert.Equal(t, http.StatusTooManyRequests, doTestLogin(t, ws, svc, `{"email":"a@example.com"}`, addr).Code)
}

// Idle buckets are swept once the table is large.
func TestTestLoginLimiter_SweepsIdleBuckets(t *testing.T) {
	l := newTestLoginLimiter()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }
	for i := 0; i < testLoginLimiterSweepSize; i++ {
		require.True(t, l.Allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256)))
	}
	clock = clock.Add(testLoginLimiterMaxAge + time.Second)
	require.True(t, l.Allow("10.9.9.9"))
	assert.Len(t, l.buckets, 1)
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
