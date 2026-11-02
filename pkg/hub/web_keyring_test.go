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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/gorilla/securecookie"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 1 of ptone/scion#1530: the keyring file reaches a real browser
// session. Two real WebServers share one real SQLite store; they differ
// only in their keyring files (which generation is active, and deadlines),
// as two replicas mid-rollout would. No session store is mocked: each
// server uses its own sessions.CookieStore built from its keyring. Only
// the Google endpoints are stubbed (mockOAuthTransport) and the Hub API
// behind the web server is a recorder of the Authorization header it gets.

// Synthetic fixtures only.
const (
	krLegacySecret = "test-only-legacy-session-secret"
	krGen1Secret   = "test-only-keyring-generation-1-0123456789abcdef"
	krGen2Secret   = "test-only-keyring-generation-2-0123456789abcdef"
	krUserEmail    = "keyring-user@example.com"
)

var krT0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

type krClock struct{ t time.Time }

func (c *krClock) now() time.Time { return c.t }

type krEntry struct {
	id, secret            string
	signUntil, verifyUntl *time.Time
}

// krKeyring builds and parses a version-1 keyring file.
func krKeyring(t *testing.T, active string, entries ...krEntry) *keyring.Keyring {
	t.Helper()
	keys := make([]any, 0, len(entries))
	// Every generation reads ID-less values (phase 4: with emit legacy the
	// active generation must be a legacy reader, and replicas mid-rollout
	// need compatible reader lists). Session cookies ignore this list.
	reads := make([]any, 0, len(entries))
	for _, e := range entries {
		reads = append(reads, e.id)
		m := map[string]any{"id": e.id, "secret": e.secret, "sign_until": nil, "verify_until": nil}
		if e.signUntil != nil {
			m["sign_until"] = e.signUntil.Format(time.RFC3339)
			m["verify_until"] = e.verifyUntl.Format(time.RFC3339)
		}
		keys = append(keys, m)
	}
	doc := map[string]any{
		"schema":   1,
		"revision": "test-rev-1",
		"signing": map[string]any{
			"active": active, "emit": "legacy", "keys": keys, "legacy_read_ids": reads,
		},
		"encryption": map[string]any{
			"active": "legacy", "write_format": "v1",
			"keys":               []any{map[string]any{"id": "legacy", "key_b64": base64.StdEncoding.EncodeToString(keyring.DeriveLegacyEncryptionKey(krLegacySecret))}},
			"legacy_v1_read_ids": []any{"legacy"},
		},
	}
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	k, err := keyring.Parse(data)
	require.NoError(t, err)
	require.NoError(t, k.CheckSessionPilot())
	return k
}

func krTime(d time.Duration) *time.Time {
	t := krT0.Add(d)
	return &t
}

// krReplica is one real WebServer over the shared store, with its own
// keyring. hubAuth records the Authorization header the mounted Hub API
// handler received.
type krReplica struct {
	ws      *WebServer
	hubAuth string
}

func newKRReplica(t *testing.T, st store.Store, k *keyring.Keyring, clk *krClock) *krReplica {
	t.Helper()
	r := &krReplica{}
	r.ws = newTestWebServer(t, WebServerConfig{
		SigningRing: k.Signing(),
		Now:         clk.now,
		BaseURL:     "http://localhost:8080",
	})
	r.ws.store = st
	r.ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{Google: OAuthProviderConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"}},
	}, nil)
	r.ws.oauthService.httpClient = &http.Client{Transport: &mockOAuthTransport{
		tokenJSON:    `{"access_token":"mock-google-access-token","token_type":"Bearer","expires_in":3600}`,
		userinfoJSON: `{"id":"1082345","email":"` + krUserEmail + `","verified_email":true,"name":"Keyring User"}`,
	}}
	// The user token service is fixed to the keyring's legacy entry here
	// (the derivation a legacy-mode hub uses), so these cookie tests do not
	// depend on which generation is active; hub.New builds it from the
	// signing ring (see server_jwt_keyring_test.go).
	legacy, ok := k.Signing().LegacySecret()
	require.True(t, ok)
	svc, err := NewUserTokenService(UserTokenConfig{SigningKey: deriveSharedSigningKey(legacy, SecretKeyUserSigningKey)})
	require.NoError(t, err)
	r.ws.userTokenSvc = svc
	r.ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hubAuth = req.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}), nil)
	return r
}

// serve runs one request with cookies on the replica's full handler chain.
func (r *krReplica) serve(t *testing.T, method, target string, cookies []*http.Cookie) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Accept", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.hubAuth = ""
	r.ws.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == webSessionName && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

// login runs /auth/login/google on from and /auth/callback/google on to,
// carrying the cookie between them like a browser, and returns the final
// session cookie. The callback succeeds only if to reads the OAuth state
// from's cookie holds.
func krLogin(t *testing.T, from, to *krReplica) *http.Cookie {
	t.Helper()
	loginResp := from.serve(t, http.MethodGet, "/auth/login/google", nil)
	require.Equal(t, http.StatusFound, loginResp.StatusCode)
	stateCookie := sessionCookie(loginResp)
	require.NotNil(t, stateCookie, "login must set the session cookie holding the OAuth state")
	loc, err := url.Parse(loginResp.Header.Get("Location"))
	require.NoError(t, err)
	state := loc.Query().Get("state")
	require.NotEmpty(t, state)

	cbResp := to.serve(t, http.MethodGet, "/auth/callback/google?code=test-code&state="+url.QueryEscape(state), []*http.Cookie{stateCookie})
	require.Equal(t, http.StatusFound, cbResp.StatusCode)
	require.Equal(t, "/", cbResp.Header.Get("Location"), "callback must complete the login (no state_mismatch)")
	c := sessionCookie(cbResp)
	require.NotNil(t, c, "callback must save the logged-in session")
	return c
}

// sessionCookieCount counts the Set-Cookie headers for the session cookie.
func sessionCookieCount(resp *http.Response) int {
	n := 0
	for _, c := range resp.Cookies() {
		if c.Name == webSessionName {
			n++
		}
	}
	return n
}

func decodesUnderActive(ws *WebServer, c *http.Cookie) bool {
	return keyring.DecodesWithActive(webSessionName, c.Value, ws.sessionCodecs)
}

func decodesUnderAny(ws *WebServer, c *http.Cookie) bool {
	var dst map[interface{}]interface{}
	return securecookie.DecodeMulti(webSessionName, c.Value, &dst, ws.sessionCodecs...) == nil
}

// krRollout returns the keyrings of two replicas mid-rollout: A still has
// gen1 active; B has activated gen2 and keeps gen1 as the outgoing entry
// (issuance closed at T0+1h, verification until T0+48h). Both carry the
// imported legacy entry, which every other purpose still uses.
func krRollout(t *testing.T) (a, b *keyring.Keyring) {
	legacy := krEntry{id: "legacy", secret: krLegacySecret}
	a = krKeyring(t, "gen1", legacy,
		krEntry{id: "gen1", secret: krGen1Secret},
		krEntry{id: "gen2", secret: krGen2Secret})
	b = krKeyring(t, "gen2", legacy,
		krEntry{id: "gen1", secret: krGen1Secret, signUntil: krTime(time.Hour), verifyUntl: krTime(48 * time.Hour)},
		krEntry{id: "gen2", secret: krGen2Secret})
	return a, b
}

// TestWebKeyring_CallbackStateSurvivesCrossReplicaHop: /auth/login on
// replica A (gen1 active) and /auth/callback on replica B (gen2 active)
// complete one login, and B saves the logged-in session under gen2.
func TestWebKeyring_CallbackStateSurvivesCrossReplicaHop(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	clk := &krClock{krT0}
	ka, kb := krRollout(t)
	a, b := newKRReplica(t, st, ka, clk), newKRReplica(t, st, kb, clk)

	c := krLogin(t, a, b)
	assert.True(t, decodesUnderActive(b.ws, c), "B saves under its active generation (gen2)")
	assert.False(t, decodesUnderActive(a.ws, c), "not under A's active generation (gen1)")
	assert.True(t, decodesUnderAny(a.ws, c), "A still reads it (gen2 is in its ring)")
}

// TestWebKeyring_OldCookieLoginSurvivesSecondReplica: a session minted by
// replica A under gen1 authenticates a Hub API request on replica B, which
// holds both generations, and B re-saves it under its active generation
// gen2 with the same values (the Hub tokens are not re-minted).
func TestWebKeyring_OldCookieLoginSurvivesSecondReplica(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	clk := &krClock{krT0}
	ka, kb := krRollout(t)
	a, b := newKRReplica(t, st, ka, clk), newKRReplica(t, st, kb, clk)

	old := krLogin(t, a, a)
	require.True(t, decodesUnderActive(a.ws, old))
	require.False(t, decodesUnderActive(b.ws, old))

	resp := b.serve(t, http.MethodGet, "/api/v1/projects", []*http.Cookie{old})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, strings.HasPrefix(b.hubAuth, "Bearer "), "B must forward the session's Hub token")
	_, err = b.ws.userTokenSvc.ValidateUserToken(strings.TrimPrefix(b.hubAuth, "Bearer "))
	require.NoError(t, err, "the forwarded token is valid on B (unchanged legacy signing material)")

	resaved := sessionCookie(resp)
	require.NotNil(t, resaved, "B must re-save a session read under a non-active generation")
	assert.Equal(t, 1, sessionCookieCount(resp), "exactly one Set-Cookie")
	assert.True(t, decodesUnderActive(b.ws, resaved), "the fresh cookie uses B's active pair")
	assert.False(t, decodesUnderActive(a.ws, resaved))

	// Same session values: the re-save carries the original Hub token.
	sessOld := decodeSession(t, b.ws, old)
	sessNew := decodeSession(t, b.ws, resaved)
	assert.Equal(t, sessOld[sessKeyHubAccessToken], sessNew[sessKeyHubAccessToken], "re-save must not re-mint the Hub token")
	assert.Equal(t, sessOld[sessKeyHubTokenExpiry], sessNew[sessKeyHubTokenExpiry], "re-save must not extend the Hub token expiry")
	assert.Equal(t, sessOld[sessKeyUserID], sessNew[sessKeyUserID])

	// A session already under the active generation is not re-saved.
	resp = b.serve(t, http.MethodGet, "/api/v1/projects", []*http.Cookie{resaved})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Nil(t, sessionCookie(resp), "no re-save under the active generation")
}

func decodeSession(t *testing.T, ws *WebServer, c *http.Cookie) map[interface{}]interface{} {
	t.Helper()
	var dst map[interface{}]interface{}
	require.NoError(t, securecookie.DecodeMulti(webSessionName, c.Value, &dst, ws.sessionCodecs...))
	return dst
}

// TestWebKeyring_ExpiredPreviousGenerationRefused: B accepts A's gen1
// session until gen1's verify_until and refuses it at verify_until, though
// the key is still in its ring and the cookie is within its 24h window.
func TestWebKeyring_ExpiredPreviousGenerationRefused(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	clk := &krClock{krT0}
	// The 24h cookie window is measured on the wall clock (securecookie);
	// verify_until is measured on clk, so move clk only.
	ka, kb := krRollout(t)
	a, b := newKRReplica(t, st, ka, clk), newKRReplica(t, st, kb, clk)
	old := krLogin(t, a, a)

	clk.t = krT0.Add(48*time.Hour - time.Second)
	resp := b.serve(t, http.MethodGet, "/api/v1/projects", []*http.Cookie{old})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, b.hubAuth, "accepted one second before verify_until")

	clk.t = krT0.Add(48 * time.Hour)
	assert.False(t, decodesUnderAny(b.ws, old), "gen1 codec refuses at verify_until")
	_ = b.serve(t, http.MethodGet, "/api/v1/projects", []*http.Cookie{old})
	assert.Empty(t, b.hubAuth, "no Hub token is forwarded from an expired generation's session")
}

// TestWebKeyring_IssuanceClosedFailsReadinessButVerifies: when the active
// generation reaches its sign_until, /readyz reports not ready
// (signing_issuance_closed) and no session is saved, while a session minted
// earlier still authenticates until verify_until; /healthz is unaffected.
func TestWebKeyring_IssuanceClosedFailsReadinessButVerifies(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	clk := &krClock{krT0}
	k := krKeyring(t, "gen1",
		krEntry{id: "legacy", secret: krLegacySecret},
		krEntry{id: "gen1", secret: krGen1Secret, signUntil: krTime(time.Hour), verifyUntl: krTime(24 * time.Hour)},
		krEntry{id: "gen2", secret: krGen2Secret})
	r := newKRReplica(t, st, k, clk)
	hubSrv := &Server{store: st, config: ServerConfig{Keyring: k, KeyringNow: clk.now}}

	readyz := func() (int, map[string]string) {
		rec := httptest.NewRecorder()
		hubSrv.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return rec.Code, body
	}

	code, body := readyz()
	require.Equal(t, http.StatusOK, code, body)
	minted := krLogin(t, r, r)

	clk.t = krT0.Add(time.Hour)
	code, body = readyz()
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, map[string]string{"status": "not_ready", "reason": "signing_issuance_closed"}, body,
		"the reason names no key material, ID or deadline")
	// Liveness is unaffected.
	healthz := r.serve(t, http.MethodGet, "/healthz", nil)
	assert.Equal(t, http.StatusOK, healthz.StatusCode)

	// Issuance stopped: the login handler cannot save its state.
	loginResp := r.serve(t, http.MethodGet, "/auth/login/google", nil)
	assert.Equal(t, http.StatusInternalServerError, loginResp.StatusCode)
	assert.Nil(t, sessionCookie(loginResp), "no session cookie is issued after sign_until")

	// Verification continues until verify_until.
	resp := r.serve(t, http.MethodGet, "/api/v1/projects", []*http.Cookie{minted})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, r.hubAuth, "a session minted before sign_until still authenticates")
}

// TestWebKeyring_NoActiveGenerationNotReady: a keyring whose signing ring
// has no usable active generation reports not ready with
// signing_no_active_generation (unreachable through the loader, which
// refuses such files; fail closed anyway).
func TestWebKeyring_NoActiveGenerationNotReady(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	hubSrv := &Server{store: st, config: ServerConfig{Keyring: &keyring.Keyring{}}}
	rec := httptest.NewRecorder()
	hubSrv.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, map[string]string{"status": "not_ready", "reason": "signing_no_active_generation"}, body)
}

// TestWebKeyring_LegacyReadinessUnchanged: without a keyring file /readyz
// never reports signing_issuance_closed.
func TestWebKeyring_LegacyReadinessUnchanged(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	for _, k := range []*keyring.Keyring{nil, keyring.Legacy(krLegacySecret)} {
		hubSrv := &Server{store: st, config: ServerConfig{Keyring: k, KeyringNow: func() time.Time { return krT0.AddDate(100, 0, 0) }}}
		rec := httptest.NewRecorder()
		hubSrv.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
}

// TestWebKeyring_LegacyCookiesWireCompatible: a legacy-configured
// WebServer (no keyring file) reads cookies of the pre-keyring store and
// writes cookies the pre-keyring store reads: the legacy adapter derives
// the same keys from SESSION_SECRET.
func TestWebKeyring_LegacyCookiesWireCompatible(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{SessionSecret: krLegacySecret})
	pre := securecookie.New(deriveSessionKey(krLegacySecret, "scion-session-hash"), deriveSessionKey(krLegacySecret, "scion-session-block"))
	pre.MaxAge(86400)
	require.Len(t, ws.sessionCodecs, 1)

	v, err := pre.Encode(webSessionName, map[interface{}]interface{}{sessKeyUserID: "u-1"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: webSessionName, Value: v})
	sess, err := ws.sessionStore.Get(req, webSessionName)
	require.NoError(t, err)
	assert.False(t, sess.IsNew)
	assert.Equal(t, "u-1", sess.Values[sessKeyUserID])

	rec := httptest.NewRecorder()
	require.NoError(t, sess.Save(req, rec))
	c := sessionCookie(rec.Result())
	require.NotNil(t, c)
	var dst map[interface{}]interface{}
	require.NoError(t, pre.Decode(webSessionName, c.Value, &dst))
	assert.Equal(t, "u-1", dst[sessKeyUserID])
	// Same cookie options as before.
	assert.Equal(t, 86400, ws.sessionStore.Options.MaxAge)
	assert.True(t, ws.sessionStore.Options.HttpOnly)
}

// TestWebKeyring_CookieSizeLimitKept: the 4096-byte securecookie limit
// stays in force under a keyring file, so an oversize session fails to
// save rather than emitting a cookie the browser drops.
func TestWebKeyring_CookieSizeLimitKept(t *testing.T) {
	ka, _ := krRollout(t)
	ws := newTestWebServer(t, WebServerConfig{SigningRing: ka.Signing()})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	sess, err := ws.sessionStore.New(req, webSessionName)
	require.NoError(t, err)
	sess.Values["blob"] = strings.Repeat("x", 5000)
	rec := httptest.NewRecorder()
	assert.Error(t, sess.Save(req, rec))
	assert.Nil(t, sessionCookie(rec.Result()))
}

// TestNew_KeyringModeRefusesExplicitSigningKeys: explicit agent or user
// JWT signing keys keep precedence in legacy mode but are refused with a
// keyring file, as is a SharedSigningSecret that is not the keyring's
// legacy entry.
func TestNew_KeyringModeRefusesExplicitSigningKeys(t *testing.T) {
	ka, _ := krRollout(t)
	base := ServerConfig{Keyring: ka, SharedSigningSecret: krLegacySecret}
	require.NoError(t, validateKeyringConfig(base))

	agent := base
	agent.AgentTokenConfig.SigningKey = []byte("explicit-agent-key-0123456789abcdef")
	_, err := New(agent, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AgentTokenConfig.SigningKey")

	user := base
	user.UserTokenConfig.SigningKey = []byte("explicit-user-key-0123456789abcdef")
	_, err = New(user, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UserTokenConfig.SigningKey")

	mismatch := base
	mismatch.SharedSigningSecret = "some-other-secret"
	err = validateKeyringConfig(mismatch)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), krLegacySecret)
	assert.NotContains(t, err.Error(), "some-other-secret")
	// The same mismatch with a secret of equal length, so the comparison
	// runs over every byte instead of stopping at the length check.
	sameLength := base
	sameLength.SharedSigningSecret = "test-only-wrong0-session-secret"
	require.Len(t, sameLength.SharedSigningSecret, len(krLegacySecret))
	err = validateKeyringConfig(sameLength)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SharedSigningSecret")
	assert.NotContains(t, err.Error(), krLegacySecret)
	assert.NotContains(t, err.Error(), sameLength.SharedSigningSecret)

	// Legacy mode: explicit keys are allowed (precedence unchanged).
	legacy := ServerConfig{Keyring: keyring.Legacy("s"), AgentTokenConfig: AgentTokenConfig{SigningKey: []byte("k")}}
	assert.NoError(t, validateKeyringConfig(legacy))
	assert.NoError(t, validateKeyringConfig(ServerConfig{UserTokenConfig: UserTokenConfig{SigningKey: []byte("k")}}))
}

// krPageLoad serves an authenticated page request (sessionAuthMiddleware
// path, not the /api bearer path) on r.
func krPageLoad(t *testing.T, r *krReplica, path string, c *http.Cookie) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.ws.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

// TestWebKeyring_PageLoadResavesUnderActive: an authenticated page load
// re-saves a session read under a non-active generation under the active
// one, once, with the same values (Hub token and expiry unchanged).
func TestWebKeyring_PageLoadResavesUnderActive(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	clk := &krClock{krT0}
	ka, kb := krRollout(t)
	a, b := newKRReplica(t, st, ka, clk), newKRReplica(t, st, kb, clk)
	old := krLogin(t, a, a)

	resp := krPageLoad(t, b, "/agents", old)
	require.Equal(t, http.StatusOK, resp.StatusCode, "the page loads for the old-generation session")
	require.Equal(t, 1, sessionCookieCount(resp), "exactly one Set-Cookie")
	resaved := sessionCookie(resp)
	require.NotNil(t, resaved)
	assert.True(t, decodesUnderActive(b.ws, resaved))
	before, after := decodeSession(t, b.ws, old), decodeSession(t, b.ws, resaved)
	for _, key := range []string{sessKeyUserID, sessKeyHubAccessToken, sessKeyHubRefreshToken, sessKeyHubTokenExpiry, sessKeySessionGeneration} {
		assert.Equal(t, before[key], after[key], "re-save keeps %s unchanged", key)
	}
	require.NotEmpty(t, before[sessKeyHubAccessToken], "the fixture session carries a Hub token")

	// Under the active generation already: no re-save.
	resp = krPageLoad(t, b, "/agents", resaved)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Zero(t, sessionCookieCount(resp))
}

// TestWebKeyring_NoResaveOffTheSuccessPath: no re-save on a public route,
// for a session without a user, or for a suspended user (the re-save runs
// only after the suspended-user and admin-mode checks).
func TestWebKeyring_NoResaveOffTheSuccessPath(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	clk := &krClock{krT0}
	ka, kb := krRollout(t)
	a, b := newKRReplica(t, st, ka, clk), newKRReplica(t, st, kb, clk)
	old := krLogin(t, a, a)

	// Public route.
	resp := krPageLoad(t, b, "/login", old)
	assert.Zero(t, sessionCookieCount(resp), "public route: no re-save")

	// A session with no user (only OAuth state), on the API path.
	loginResp := a.serve(t, http.MethodGet, "/auth/login/google", nil)
	stateOnly := sessionCookie(loginResp)
	require.NotNil(t, stateOnly)
	resp = b.serve(t, http.MethodGet, "/api/v1/projects", []*http.Cookie{stateOnly})
	assert.Zero(t, sessionCookieCount(resp), "no user: no re-save")
	assert.Empty(t, b.hubAuth)

	// Suspended user: refused, and no re-save.
	u, err := st.GetUserByEmail(context.Background(), krUserEmail)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, st.UpdateUser(context.Background(), u))
	resp = krPageLoad(t, b, "/agents", old)
	assert.NotEqual(t, http.StatusOK, resp.StatusCode, "a suspended user is refused")
	assert.Zero(t, sessionCookieCount(resp), "suspended user: no re-save")
}

// TestWebKeyring_RefreshSavesOnceUnderActive: when the bearer middleware
// refreshes the Hub token of an old-generation session, that save (under
// the active generation) is the only Set-Cookie: the re-save is skipped.
func TestWebKeyring_RefreshSavesOnceUnderActive(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	clk := &krClock{krT0}
	ka, kb := krRollout(t)
	a, b := newKRReplica(t, st, ka, clk), newKRReplica(t, st, kb, clk)
	old := krLogin(t, a, a)

	// The same session with its Hub token expiry in the past, still under
	// A's generation (gen1).
	vals := decodeSession(t, a.ws, old)
	vals[sessKeyHubTokenExpiry] = time.Now().Add(-time.Minute).UnixMilli()
	enc, err := securecookie.EncodeMulti(webSessionName, vals, a.ws.sessionCodecs...)
	require.NoError(t, err)
	stale := &http.Cookie{Name: webSessionName, Value: enc}
	require.False(t, decodesUnderActive(b.ws, stale))

	resp := b.serve(t, http.MethodGet, "/api/v1/projects", []*http.Cookie{stale})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 1, sessionCookieCount(resp), "the refresh save is the only Set-Cookie")
	saved := sessionCookie(resp)
	require.NotNil(t, saved)
	assert.True(t, decodesUnderActive(b.ws, saved))
	after := decodeSession(t, b.ws, saved)
	exp, _ := after[sessKeyHubTokenExpiry].(int64)
	assert.Greater(t, exp, time.Now().UnixMilli(), "the token was refreshed (new expiry in the future)")
}

// TestWebKeyring_ResaveSkippedWhenAlreadySetOrIssuanceClosed: the re-save
// adds no second Set-Cookie when the response already sets the session
// cookie, and does nothing (no failing save, no per-request warning) once
// the active generation's issuance is closed, while the old-generation
// session still authenticates.
func TestWebKeyring_ResaveSkippedWhenAlreadySetOrIssuanceClosed(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	clk := &krClock{krT0}
	ka, kb := krRollout(t)
	a, b := newKRReplica(t, st, ka, clk), newKRReplica(t, st, kb, clk)
	old := krLogin(t, a, a)

	// Already set on this response: no second Set-Cookie.
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	req.AddCookie(old)
	sess, err := b.ws.sessionStore.Get(req, webSessionName)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	rec.Header().Add("Set-Cookie", webSessionName+"=already-saved; Path=/")
	b.ws.resaveUnderActiveGeneration(rec, req, sess)
	assert.Len(t, rec.Header().Values("Set-Cookie"), 1, "no second session Set-Cookie")

	// Issuance closed on a replica whose active generation (gen1) reached
	// sign_until, reading a legacy-generation session (no deadlines).
	kc := krKeyring(t, "gen1",
		krEntry{id: "legacy", secret: krLegacySecret},
		krEntry{id: "gen1", secret: krGen1Secret, signUntil: krTime(time.Hour), verifyUntl: krTime(48 * time.Hour)})
	kl := krKeyring(t, "legacy",
		krEntry{id: "legacy", secret: krLegacySecret},
		krEntry{id: "gen1", secret: krGen1Secret, signUntil: krTime(time.Hour), verifyUntl: krTime(48 * time.Hour)})
	l, c := newKRReplica(t, st, kl, clk), newKRReplica(t, st, kc, clk)
	var logs bytes.Buffer
	c.ws.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	legacyCookie := krLogin(t, l, l)
	clk.t = krT0.Add(2 * time.Hour)
	resp := c.serve(t, http.MethodGet, "/api/v1/projects", []*http.Cookie{legacyCookie})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, c.hubAuth, "the legacy-generation session still authenticates")
	assert.Zero(t, sessionCookieCount(resp), "no re-save after sign_until")
	resp = krPageLoad(t, c, "/agents", legacyCookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Zero(t, sessionCookieCount(resp), "no page re-save after sign_until")
	// The re-save was skipped, not attempted and failed: a failing save
	// would also emit no cookie, but would log on every request.
	assert.NotContains(t, logs.String(), "Failed to re-save session", "no failing re-save after sign_until")
}
