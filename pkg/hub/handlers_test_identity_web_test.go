//go:build !no_sqlite && (!hubshard || hubshard_3)

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

// Test identity web sessions (ptone/scion#4240, phase WS1). The tests run
// the real hub (SQLite store, auth middleware, route guards) mounted under
// the real web server, and drive it with cookies only, the way a browser
// does.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- Harness ----------------------------------------------------------------

type tiWebClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *tiWebClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *tiWebClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type tiWebEnv struct {
	t         *testing.T
	srv       *Server
	st        store.Store
	ws        *WebServer
	clock     *tiWebClock
	issuerID  string
	issuerTok string

	mu       sync.Mutex
	lastAuth string
}

func tiWebKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, testIdentityWebKeyLen)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

const tiWebSharedSecret = "ti-web-synthetic-shared-secret"

// newTIWebEnv builds a hub with test identities on and a hub-local web
// session key, and a web server in front of it.
func newTIWebEnv(t *testing.T, cfgFns ...func(*WebServerConfig)) *tiWebEnv {
	t.Helper()
	srv, st := newTestIdentityServer(t, true)
	return newTIWebEnvOn(t, srv, st, tiWebKey(t), cfgFns...)
}

func newTIWebEnvOn(t *testing.T, srv *Server, st store.Store, key []byte, cfgFns ...func(*WebServerConfig)) *tiWebEnv {
	t.Helper()
	srv.SetDispatcher(&deleteGuardDispatcher{})
	srv.testIdentityWeb = newTestIdentityWebState(TestIdentityWebSessionKey{Key: key})
	clock := &tiWebClock{now: time.Now()}
	srv.testIdentities.now = clock.Now
	srv.authConfig.Now = clock.Now
	cfg := WebServerConfig{SessionSecret: tiWebSharedSecret}
	for _, fn := range cfgFns {
		fn(&cfg)
	}
	ws := newTestWebServer(t, cfg)
	ws.SetStore(st)
	ws.SetUserTokenService(srv.userTokenService)
	ws.SetAuthzService(srv.authzService)
	ws.SetTestIdentityWebSessions(srv)
	e := &tiWebEnv{t: t, srv: srv, st: st, ws: ws, clock: clock}
	ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.lastAuth = r.Header.Get("Authorization")
		e.mu.Unlock()
		// Rebuilt per request so a test can change the auth config.
		srv.Handler().ServeHTTP(w, r)
	}), func(context.Context) error { return nil })
	e.issuerID, e.issuerTok = tiIssuer(t, srv, st, "ti-web-issuer-"+generateID()[:6])
	return e
}

// issue issues a member test identity.
func (e *tiWebEnv) issue() TestIdentityTokenResponse {
	e.t.Helper()
	return tiIssue(e.t, e.srv, e.issuerTok, map[string]string{"role": "member", "purpose": "ws1"})
}

// mintRec calls the mint route with token.
func (e *tiWebEnv) mintRec(token, id string) *httptest.ResponseRecorder {
	e.t.Helper()
	return doRequestWithToken(e.t, e.srv, token, http.MethodPost, "/api/v1/test-identities/"+id+"/web-login-code", nil)
}

// mint mints a code as the issuer, requiring 201.
func (e *tiWebEnv) mint(id string) TestIdentityWebCodeResponse {
	e.t.Helper()
	rec := e.mintRec(e.issuerTok, id)
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp TestIdentityWebCodeResponse
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

type tiReq struct {
	method  string
	path    string
	body    string
	ctype   string
	cookies []*http.Cookie
	headers map[string]string
	browser bool
}

func (e *tiWebEnv) do(r tiReq) *httptest.ResponseRecorder {
	e.t.Helper()
	if r.method == "" {
		r.method = http.MethodGet
	}
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	req := httptest.NewRequest(r.method, r.path, body)
	req.RemoteAddr = "192.0.2.10:5555"
	if r.ctype != "" {
		req.Header.Set("Content-Type", r.ctype)
	}
	if r.browser {
		req.Header.Set("Accept", "text/html")
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	for _, c := range r.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.ws.Handler().ServeHTTP(rec, req)
	return rec
}

// redeem posts the code as JSON with the given cookies.
func (e *tiWebEnv) redeem(code string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	b, _ := json.Marshal(map[string]string{"code": code})
	return e.do(tiReq{method: http.MethodPost, path: "/auth/test-identity/redeem", body: string(b), ctype: "application/json", cookies: cookies})
}

// login issues nothing: it mints and redeems a code for id and returns the
// session cookies.
func (e *tiWebEnv) login(id string) []*http.Cookie {
	e.t.Helper()
	rec := e.redeem(e.mint(id).Code)
	require.Equal(e.t, http.StatusNoContent, rec.Code, rec.Body.String())
	return tiWebApplyCookies(nil, rec)
}

// tiWebApplyCookies updates a cookie jar with a response's Set-Cookie headers.
func tiWebApplyCookies(jar []*http.Cookie, rec *httptest.ResponseRecorder) []*http.Cookie {
	out := map[string]*http.Cookie{}
	order := []string{}
	for _, c := range jar {
		out[c.Name] = c
		order = append(order, c.Name)
	}
	for _, c := range rec.Result().Cookies() {
		if _, ok := out[c.Name]; !ok {
			order = append(order, c.Name)
		}
		if c.MaxAge < 0 {
			delete(out, c.Name)
			continue
		}
		out[c.Name] = &http.Cookie{Name: c.Name, Value: c.Value}
	}
	var res []*http.Cookie
	for _, n := range order {
		if c, ok := out[n]; ok {
			res = append(res, c)
			delete(out, n)
		}
	}
	return res
}

// tiWebCleared reports whether rec expired cookie name.
func tiWebCleared(rec *httptest.ResponseRecorder, name string) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

func tiWebSetCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// sessionValues decodes scion_sess from a jar.
func (e *tiWebEnv) sessionValues(jar []*http.Cookie) map[interface{}]interface{} {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range jar {
		req.AddCookie(c)
	}
	sess, err := e.ws.sessionStore.Get(req, webSessionName)
	require.NoError(e.t, err)
	return sess.Values
}

// craftMarkedSession hand-builds the two cookies of a test identity web
// session for row with the given marker values, signed with the web
// server's stores (and so its hub's key).
func (e *tiWebEnv) craftMarkedSession(row *store.User, codeID string, expiry time.Time, gen int64) []*http.Cookie {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	sess, err := e.ws.sessionStore.New(req, webSessionName)
	require.NoError(e.t, err)
	sess.Values[sessKeyUserID] = row.ID
	sess.Values[sessKeyUserEmail] = row.Email
	sess.Values[sessKeyUserName] = row.DisplayName
	sess.Values[sessKeyUserRole] = row.Role
	sess.Values[sessKeySessionGeneration] = gen
	sess.Values[sessKeyTIMarker] = codeID
	sess.Values[sessKeyTIExpiry] = expiry.UnixMilli()
	sess.Values[sessKeyTIDriver] = ""
	require.NoError(e.t, sess.Save(req, rec))
	codec := e.srv.testIdentityWeb.codec
	require.NotNil(e.t, codec)
	enc, err := codec.Encode(testIdentitySessionCookie, tiMarkerValue{UserID: row.ID, CodeID: codeID, Expiry: expiry.UnixMilli()})
	require.NoError(e.t, err)
	return append(tiWebApplyCookies(nil, rec), &http.Cookie{Name: testIdentitySessionCookie, Value: enc})
}

func (e *tiWebEnv) row(id string) *store.User {
	e.t.Helper()
	u, err := e.st.GetUser(context.Background(), id)
	require.NoError(e.t, err)
	return u
}

func (e *tiWebEnv) apiMe(jar []*http.Cookie) *httptest.ResponseRecorder {
	return e.do(tiReq{path: "/api/v1/auth/me", cookies: jar})
}

func (e *tiWebEnv) webMe(jar []*http.Cookie) *httptest.ResponseRecorder {
	return e.do(tiReq{path: "/auth/me", cookies: jar})
}

func (e *tiWebEnv) page(jar []*http.Cookie) *httptest.ResponseRecorder {
	return e.do(tiReq{path: "/projects", cookies: jar, browser: true})
}

// tiWebCaptureLogs sends the default logger to a buffer for the test.
func tiWebCaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&tiWebLockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type tiWebLockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *tiWebLockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func tiWebAudits(t *testing.T, st store.Store, mutationType string) []store.MutationAuditRecord {
	t.Helper()
	res, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType, Limit: 500})
	require.NoError(t, err)
	out := make([]store.MutationAuditRecord, 0, len(res))
	for _, r := range res {
		out = append(out, *r)
	}
	return out
}

// tiWebAddProvider makes a broker serve projectID so agents can be created.
func tiWebAddProvider(t *testing.T, s store.Store, projectID string) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{ID: tid("ti-web-broker-" + projectID), Name: "ti web broker", Slug: "ti-web-broker-" + projectID[:8], Status: store.BrokerStatusOnline, Endpoint: "http://localhost:9800"}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: projectID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline}))
}

// --- AC1: gate off ----------------------------------------------------------

// With the member level off both routes answer 404, and a hand-crafted
// marked cookie is tiWebCleared by sessionToBearer and by proxyAuth.
func TestTestIdentityWeb_GateOff(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()
	row := e.row(fx.Identity.ID)
	jar := e.craftMarkedSession(row, generateID(), *row.ExpiresAt, row.SessionGeneration)
	// Sanity: the crafted session works while the gate is on.
	require.Equal(t, http.StatusOK, e.apiMe(jar).Code)

	// "Restart" with the member level off.
	e.srv.testIdentities = newTestIdentityState(false)
	e.srv.testIdentities.now = e.clock.Now
	e.srv.authConfig.TestIdentitiesEnabled = false

	rec := e.mintRec(e.issuerTok, fx.Identity.ID)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = e.redeem("scion_tiwc_anything")
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = e.do(tiReq{path: "/auth/test-identity/redeem"})
	assert.Equal(t, http.StatusNotFound, rec.Code, "GET while off is 404 too")

	rec = e.apiMe(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.True(t, tiWebCleared(rec, webSessionName), "sessionToBearer clears scion_sess")
	assert.True(t, tiWebCleared(rec, testIdentitySessionCookie), "sessionToBearer clears scion_ti")

	// proxyAuth (proxy mode) clears a marked session before any lookup.
	pe := newTIWebEnvOn(t, e.srv, e.st, tiWebKey(t), func(c *WebServerConfig) {
		c.AuthMode = "proxy"
		c.ProxyAuthenticator = &fakeProxyAuthenticator{}
	})
	pjar := pe.craftMarkedSession(row, generateID(), *row.ExpiresAt, row.SessionGeneration)
	rec = pe.page(pjar)
	assert.True(t, tiWebCleared(rec, webSessionName), "proxyAuth clears scion_sess: %d", rec.Code)
	assert.True(t, tiWebCleared(rec, testIdentitySessionCookie), "proxyAuth clears scion_ti")
}

// --- AC2 / DEC-8: the hub-local key -------------------------------------------

// No flag exists for web sessions: the gate is the member level, the key
// and not proxy mode. Neither config carries a web-session switch.
func TestTestIdentityWeb_NoNewFlag(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(ServerConfig{}), reflect.TypeOf(WebServerConfig{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Type.Kind() == reflect.Bool {
				assert.NotContains(t, strings.ToLower(f.Name), "websession", "%s.%s", typ.Name(), f.Name)
			}
		}
	}
	srv := &Server{testIdentities: newTestIdentityState(true)}
	ok, cause := srv.testIdentityWebSessionGate()
	assert.False(t, ok)
	assert.Equal(t, testIdentityWebCauseNoKey, cause)
}

// DEC-8 (5): a missing, unreadable or corrupt key leaves web sessions
// unavailable: mint and redeem 404 with the named reason, marked sessions
// tiWebCleared, the indicator false, a startup log line; the member tier still
// works.
func TestTestIdentityWeb_KeyUnavailableFailsClosed(t *testing.T) {
	for _, k := range []TestIdentityWebSessionKey{
		{Unavailable: "marker_absent"},
		{Unavailable: "marker_unreadable"},
		{Key: []byte("short")},
		{Key: make([]byte, testIdentityWebKeyLen)},
	} {
		t.Run(k.String(), func(t *testing.T) {
			logs := tiWebCaptureLogs(t)
			e := newTIWebEnv(t)
			fx := e.issue()
			row := e.row(fx.Identity.ID)
			jar := e.craftMarkedSession(row, generateID(), *row.ExpiresAt, row.SessionGeneration)

			e.srv.testIdentityWeb = newTestIdentityWebState(k)
			e.srv.logTestIdentityWebSessionState()
			assert.Contains(t, logs.String(), "Test identity web sessions are unavailable")
			assert.Contains(t, logs.String(), testIdentityWebCauseNoKey)

			rec := e.mintRec(e.issuerTok, fx.Identity.ID)
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), testIdentityWebReasonUnavailable)
			assert.Contains(t, rec.Body.String(), testIdentityWebCauseNoKey)
			rec = e.redeem("scion_tiwc_x")
			require.Equal(t, http.StatusNotFound, rec.Code)
			assert.Contains(t, rec.Body.String(), testIdentityWebCauseNoKey)

			rec = e.apiMe(jar)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.True(t, tiWebCleared(rec, webSessionName))

			status := doRequestNoAuth(t, e.srv, http.MethodGet, "/api/v1/test-infra/status", nil)
			assert.Contains(t, status.Body.String(), `"testIdentityWebSessions":false`)
			assert.Contains(t, status.Body.String(), `"testIdentities":true`)

			// The member tier is unaffected.
			fx2 := e.issue()
			assert.NotEmpty(t, fx2.AccessToken)
			assert.Equal(t, http.StatusOK, tiAuthMe(t, e.srv, fx2.AccessToken).Code)
		})
	}
}

// DEC-8 (5) at construction: newTestIdentityWebState never builds a codec
// from a bad key, and the key never reaches any formatted config.
func TestTestIdentityWeb_KeyStateConstruction(t *testing.T) {
	assert.Nil(t, newTestIdentityWebState(TestIdentityWebSessionKey{}).codec)
	assert.Equal(t, "key_missing", newTestIdentityWebState(TestIdentityWebSessionKey{}).unavailable)
	assert.Equal(t, "key_invalid", newTestIdentityWebState(TestIdentityWebSessionKey{Key: make([]byte, 63)}).unavailable)
	assert.Equal(t, "key_invalid", newTestIdentityWebState(TestIdentityWebSessionKey{Key: make([]byte, 65)}).unavailable)
	key := tiWebKey(t)
	assert.NotNil(t, newTestIdentityWebState(TestIdentityWebSessionKey{Key: key}).codec)
	cfg := ServerConfig{TestIdentityWebSessionKey: TestIdentityWebSessionKey{Key: key}}
	for _, s := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg.TestIdentityWebSessionKey)} {
		assert.NotContains(t, s, fmt.Sprintf("%v", key))
		assert.NotContains(t, s, string(key))
	}
}

// DEC-8 (3): the key is expanded into distinct hash and block keys, neither
// equal to the key itself or to the human session store's keys, and the
// marker cookie does not decode with the human session keys (or the other
// way round).
func TestTestIdentityWeb_KeyDomainSeparated(t *testing.T) {
	key := tiWebKey(t)
	h, b := deriveTestIdentityWebKeys(key)
	assert.Len(t, h, 32)
	assert.Len(t, b, 32)
	assert.NotEqual(t, h, b)
	assert.NotEqual(t, key[:32], h)
	assert.NotEqual(t, key[:32], b)
	// Even a session secret equal to the key bytes gives different keys.
	assert.NotEqual(t, deriveSessionKey(string(key), "scion-session-hash"), h)
	assert.NotEqual(t, deriveSessionKey(string(key), "scion-session-block"), b)

	codec := newTestIdentityWebState(TestIdentityWebSessionKey{Key: key}).codec
	enc, err := codec.Encode(testIdentitySessionCookie, tiMarkerValue{UserID: "u", CodeID: "c", Expiry: 1})
	require.NoError(t, err)
	human := securecookie.New(deriveSessionKey(tiWebSharedSecret, "scion-session-hash"), deriveSessionKey(tiWebSharedSecret, "scion-session-block"))
	var v map[string]interface{}
	assert.Error(t, human.Decode(testIdentitySessionCookie, enc, &v), "the human store cannot read a marker")
	humanEnc, err := human.Encode(testIdentitySessionCookie, map[string]interface{}{"u": "u"})
	require.NoError(t, err)
	var m tiMarkerValue
	assert.Error(t, codec.Decode(testIdentitySessionCookie, humanEnc, &m), "the human store cannot write a marker")
}

// DEC-8 (6) and (4): a session from hub A is refused on hub B, even when
// both hubs share the human session secret and the database; and a new key
// on the same hub (re-mark) refuses every existing session.
func TestTestIdentityWeb_CrossHubAndRemarkRefused(t *testing.T) {
	a := newTIWebEnv(t)
	fx := a.issue()
	jar := a.login(fx.Identity.ID)
	require.Equal(t, http.StatusOK, a.apiMe(jar).Code)

	srvB, _ := testServerWithStore(t, a.st)
	enableTestIdentitiesForTest(srvB)
	b := newTIWebEnvOn(t, srvB, a.st, tiWebKey(t))
	rec := b.apiMe(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "hub B must not accept hub A's session")
	assert.True(t, tiWebCleared(rec, webSessionName))
	rec = b.page(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "Test identity session ended")

	// Re-mark on hub A: a new key.
	a.srv.testIdentityWeb = newTestIdentityWebState(TestIdentityWebSessionKey{Key: tiWebKey(t)})
	rec = a.apiMe(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a new key refuses existing sessions")
}

// --- AC3: mint ----------------------------------------------------------------

func TestTestIdentityWeb_Mint(t *testing.T) {
	logs := tiWebCaptureLogs(t)
	e := newTIWebEnv(t)
	fx := e.issue()

	rec := e.mintRec(e.issuerTok, fx.Identity.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Len(t, raw, 3, "code, codeId, expiresAt and nothing else")
	code, _ := raw["code"].(string)
	require.True(t, strings.HasPrefix(code, testIdentityWebCodePrefix))
	assert.Equal(t, 1, strings.Count(rec.Body.String(), testIdentityWebCodePrefix), "exactly one code")
	var resp TestIdentityWebCodeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.WithinDuration(t, e.clock.Now().Add(testIdentityWebCodeTTL), resp.ExpiresAt, time.Second)

	// Another issuer: 404.
	_, otherTok := tiIssuer(t, e.srv, e.st, "ti-web-other-issuer")
	assert.Equal(t, http.StatusNotFound, e.mintRec(otherTok, fx.Identity.ID).Code)
	// The fixture's own bearer: 403.
	assert.Equal(t, http.StatusForbidden, e.mintRec(fx.AccessToken, fx.Identity.ID).Code)
	// A human user id and a missing id: 404.
	assert.Equal(t, http.StatusNotFound, e.mintRec(e.issuerTok, e.issuerID).Code)
	assert.Equal(t, http.StatusNotFound, e.mintRec(e.issuerTok, generateID()).Code)
	// An expired identity: 409.
	short := tiIssue(t, e.srv, e.issuerTok, map[string]any{"lifetimeSeconds": 60})
	e.clock.Advance(2 * time.Minute)
	assert.Equal(t, http.StatusConflict, e.mintRec(e.issuerTok, short.Identity.ID).Code)

	// The admin kinds (phase A1) are not admitted.
	for _, kind := range []string{"test_hub_admin", "test_super_admin", store.UserKindHuman, ""} {
		assert.False(t, testIdentityWebKindAdmitted(&store.User{Kind: kind}), kind)
	}
	assert.True(t, testIdentityWebKindAdmitted(&store.User{Kind: store.UserKindTestFixture}))

	// Audit: one issue row, no code anywhere in it; no code in the logs.
	audits := tiWebAudits(t, e.st, testIdentityWebCodeIssueMutation)
	require.Len(t, audits, 1)
	assert.Equal(t, fx.Identity.ID, audits[0].TargetID)
	assert.Equal(t, e.issuerID, audits[0].ActorPrincipalID)
	b, _ := json.Marshal(audits[0])
	assert.NotContains(t, string(b), code)
	assert.NotContains(t, string(b), code[len(testIdentityWebCodePrefix):])
	assert.Contains(t, audits[0].AfterSummary, resp.CodeID)
	assert.NotContains(t, logs.String(), code[len(testIdentityWebCodePrefix):])
	assert.Contains(t, logs.String(), "test identity web-login code issued")
}

// --- AC4: single use and the five-minute window --------------------------

func TestTestIdentityWeb_RedeemSingleUseAndWindow(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()

	t.Run("two concurrent redeems, exactly one session", func(t *testing.T) {
		code := e.mint(fx.Identity.ID).Code
		var wg sync.WaitGroup
		codes := make(chan int, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				codes <- e.redeem(code).Code
			}()
		}
		close(start)
		wg.Wait()
		close(codes)
		var got []int
		for c := range codes {
			got = append(got, c)
		}
		assert.ElementsMatch(t, []int{http.StatusNoContent, http.StatusUnauthorized}, got)
	})

	t.Run("4m59s redeems", func(t *testing.T) {
		code := e.mint(fx.Identity.ID).Code
		e.clock.Advance(4*time.Minute + 59*time.Second)
		assert.Equal(t, http.StatusNoContent, e.redeem(code).Code)
	})

	t.Run("5m+1s is refused", func(t *testing.T) {
		code := e.mint(fx.Identity.ID).Code
		e.clock.Advance(5*time.Minute + time.Second)
		rec := e.redeem(code)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), testIdentityWebErrInvalidCode)
	})

	t.Run("a second mint invalidates the first", func(t *testing.T) {
		first := e.mint(fx.Identity.ID).Code
		second := e.mint(fx.Identity.ID).Code
		assert.Equal(t, http.StatusUnauthorized, e.redeem(first).Code)
		assert.Equal(t, http.StatusNoContent, e.redeem(second).Code)
	})
}

// --- AC5: request shape and origin ----------------------------------------

func TestTestIdentityWeb_RedeemRequestShape(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()
	body := func(code string) string { b, _ := json.Marshal(map[string]string{"code": code}); return string(b) }

	code := e.mint(fx.Identity.ID).Code
	rec := e.do(tiReq{method: http.MethodPost, path: "/auth/test-identity/redeem", body: body(code), ctype: "application/json", headers: map[string]string{"Origin": "https://other-origin.example"}})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = e.do(tiReq{method: http.MethodPost, path: "/auth/test-identity/redeem", body: body(code), ctype: "application/json", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = e.do(tiReq{method: http.MethodPost, path: "/auth/test-identity/redeem", body: "code=" + url.QueryEscape(code), ctype: "application/x-www-form-urlencoded"})
	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	rec = e.do(tiReq{method: http.MethodGet, path: "/auth/test-identity/redeem?code=" + url.QueryEscape(code)})
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
	rec = e.do(tiReq{method: http.MethodPost, path: "/auth/test-identity/redeem?code=" + url.QueryEscape(code), body: "{}", ctype: "application/json"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a code in the query string is ignored")
	// None of the refusals consumed the code; a same-origin request works.
	rec = e.do(tiReq{method: http.MethodPost, path: "/auth/test-identity/redeem", body: body(code), ctype: "application/json; charset=utf-8", headers: map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}})
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

// --- AC5a: failure audit and rate limit (C8) -------------------------------

func TestTestIdentityWeb_FailureAuditAndRateLimit(t *testing.T) {
	logs := tiWebCaptureLogs(t)
	e := newTIWebEnv(t)
	fx := e.issue()

	// Separate identities: a new mint deletes the identity's previous
	// code row, consumed or not.
	usedFx := e.issue()
	used := e.mint(usedFx.Identity.ID).Code
	require.Equal(t, http.StatusNoContent, e.redeem(used).Code)
	expired := e.mint(fx.Identity.ID).Code
	e.clock.Advance(6 * time.Minute)
	unknown := testIdentityWebCodePrefix + "unknown-code-value"

	cases := []struct {
		reason string
		send   func() *httptest.ResponseRecorder
		code   string
	}{
		{"unknown_code", func() *httptest.ResponseRecorder { return e.redeem(unknown) }, unknown},
		{"code_already_used", func() *httptest.ResponseRecorder { return e.redeem(used) }, used},
		{"code_expired", func() *httptest.ResponseRecorder { return e.redeem(expired) }, expired},
		{"malformed_request", func() *httptest.ResponseRecorder {
			return e.do(tiReq{method: http.MethodPost, path: "/auth/test-identity/redeem", body: `{"code":1}`, ctype: "application/json"})
		}, ""},
		{"origin_refused", func() *httptest.ResponseRecorder {
			return e.do(tiReq{method: http.MethodPost, path: "/auth/test-identity/redeem", body: `{"code":"` + unknown + `"}`, ctype: "application/json", headers: map[string]string{"Origin": "https://other-origin.example"}})
		}, unknown},
	}
	for _, c := range cases {
		before := len(tiWebAudits(t, e.st, testIdentityWebSessionRefuseMutation))
		rec := c.send()
		assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden, "%s: %d", c.reason, rec.Code)
		audits := tiWebAudits(t, e.st, testIdentityWebSessionRefuseMutation)
		require.Len(t, audits, before+1, c.reason)
		var found *store.MutationAuditRecord
		for i := range audits {
			if strings.Contains(audits[i].AfterSummary, `"reason":"`+c.reason+`"`) {
				found = &audits[i]
			}
		}
		require.NotNil(t, found, c.reason)
		if c.code != "" {
			b, _ := json.Marshal(found)
			assert.NotContains(t, string(b), c.code, c.reason)
			if c.reason != "origin_refused" {
				assert.Contains(t, found.AfterSummary, testIdentityWebHashPrefix(testIdentityWebCodeHash(c.code)), "hash prefix recorded")
			}
		}
		assert.Contains(t, found.AfterSummary, "192.0.2.10")
	}
	for _, c := range []string{unknown, used, expired} {
		assert.NotContains(t, logs.String(), c)
	}

	// Per-IP limit.
	e.ws.testIdentityRedeemLimiter = newTestLoginLimiter()
	e.ws.testIdentityRedeemLimiter.burst = 2
	e.ws.testIdentityRedeemLimiter.perSecond = 0.0001
	assert.NotEqual(t, http.StatusTooManyRequests, e.redeem(unknown).Code)
	assert.NotEqual(t, http.StatusTooManyRequests, e.redeem(unknown).Code)
	assert.Equal(t, http.StatusTooManyRequests, e.redeem(unknown).Code)
}

// --- AC5b (C7): generation snapshot -------------------------------------------

func TestTestIdentityWeb_GenerationBumpRefuses(t *testing.T) {
	e := newTIWebEnv(t)
	e.ws.events = NewChannelEventPublisher()
	fx := e.issue()
	jar := e.login(fx.Identity.ID)
	vals := e.sessionValues(jar)
	assert.Equal(t, e.row(fx.Identity.ID).SessionGeneration, vals[sessKeySessionGeneration], "sessGen from the live row")
	require.Equal(t, http.StatusOK, e.page(jar).Code)

	require.NoError(t, e.st.IncrementSessionGeneration(context.Background(), fx.Identity.ID))

	rec := e.page(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "Test identity session ended")
	assert.Empty(t, rec.Header().Get("Location"), "no login redirect")
	assert.Equal(t, http.StatusUnauthorized, e.apiMe(jar).Code)
	assert.Equal(t, http.StatusUnauthorized, e.do(tiReq{path: "/events?sub=notification.ti", cookies: jar}).Code)
}

// --- AC5c (C5): the session's own expiry is enforced explicitly -------------

// The cookie's timestamp is still valid and the identity row is live, but
// the session's tiExpiry has passed: every session-gated route refuses.
func TestTestIdentityWeb_ExplicitSessionExpiry(t *testing.T) {
	e := newTIWebEnv(t)
	e.ws.events = NewChannelEventPublisher()
	fx := e.issue()
	row := e.row(fx.Identity.ID)
	jar := e.craftMarkedSession(row, generateID(), e.clock.Now().Add(time.Minute), row.SessionGeneration)
	require.Equal(t, http.StatusOK, e.apiMe(jar).Code)

	e.clock.Advance(2 * time.Minute)
	require.True(t, e.clock.Now().Before(*row.ExpiresAt), "the identity itself is live")

	assert.Equal(t, http.StatusUnauthorized, e.page(jar).Code, "sessionAuth")
	assert.Equal(t, http.StatusUnauthorized, e.webMe(jar).Code, "/auth/me")
	assert.Equal(t, http.StatusUnauthorized, e.apiMe(jar).Code, "sessionToBearer")
	assert.Equal(t, http.StatusUnauthorized, e.do(tiReq{path: "/events?sub=notification.ti", cookies: jar}).Code, "SSE")
	res := e.ws.classifyTestIdentitySession(tiWebRequest(jar), tiWebSession(t, e.ws, jar), nil)
	assert.Equal(t, "session_expired", res.reason)
}

func tiWebRequest(jar []*http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range jar {
		req.AddCookie(c)
	}
	return req
}

func tiWebSession(t *testing.T, ws *WebServer, jar []*http.Cookie) *sessions.Session {
	t.Helper()
	s, err := ws.sessionStore.Get(tiWebRequest(jar), webSessionName)
	require.NoError(t, err)
	return s
}

// --- AC5d (C4): client_type has no authorization effect -----------------------

// The token minted per request for a web session (client type web) and the
// identity's issued API token (client type api) reach identical decisions
// over the whole permission registry, the session-only gate and every
// permission route guard.
func TestTestIdentityWeb_ClientTypeScopeEquality(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()
	row := e.row(fx.Identity.ID)
	projectID := tiCreateProject(t, e.srv, fx.AccessToken, "ti-web-scope")

	webTok, _, err := e.srv.userTokenService.GenerateAccessTokenWithTTL(row.ID, row.Email, row.DisplayName, row.Role, ClientTypeWeb, time.Minute)
	require.NoError(t, err)
	apiTok, _, err := e.srv.userTokenService.GenerateAccessTokenWithTTL(row.ID, row.Email, row.DisplayName, row.Role, ClientTypeAPI, time.Minute)
	require.NoError(t, err)

	ctxFor := func(tok string) context.Context {
		var got context.Context
		h := UnifiedAuthMiddleware(e.srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Context() }))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(httptest.NewRecorder(), req)
		require.NotNil(t, got, "token must authenticate")
		return got
	}
	webCtx, apiCtx := ctxFor(webTok), ctxFor(apiTok)
	webID := GetIdentityFromContext(webCtx).(UserIdentity)
	apiID := GetIdentityFromContext(apiCtx).(UserIdentity)
	assert.Equal(t, "web", webID.(*AuthenticatedUser).ClientType())
	assert.Equal(t, "api", apiID.(*AuthenticatedUser).ClientType())
	assert.Equal(t, GetCredentialContextFromContext(apiCtx), GetCredentialContextFromContext(webCtx))
	pw, pa := principalContextForIdentity(webID), principalContextForIdentity(apiID)
	assert.Equal(t, pa.Kind, pw.Kind)
	assert.Equal(t, pa.ID, pw.ID)
	assert.Equal(t, sessionCredentialAllowed(apiCtx), sessionCredentialAllowed(webCtx), "session-only gate")

	n := 0
	for _, p := range permissions.Registry {
		for _, res := range []Resource{{Type: p.Resource}, {Type: p.Resource, ID: projectID}, {Type: "project", ID: projectID}} {
			req := func(ctx context.Context, id UserIdentity) AuthzRequest {
				return AuthzRequest{
					Principal:      principalContextForIdentity(id),
					Credential:     credentialContextForIdentity(id),
					Resource:       res,
					Action:         Action(p.Action),
					Permission:     p.ID,
					TargetEvidence: hubCollectionEvidence(p.ID),
				}
			}
			dw := e.srv.authzService.Decide(webCtx, req(webCtx, webID))
			da := e.srv.authzService.Decide(apiCtx, req(apiCtx, apiID))
			assert.Equal(t, da, dw, "%s on %+v", p.ID, res)
			n++
		}
	}
	assert.Greater(t, n, 100, "the whole registry is covered")

	routes := 0
	for pattern, meta := range routeMetadataTable {
		if meta.Permission == "" {
			continue
		}
		rw := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(webCtx)
		ra := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(apiCtx)
		dw := e.srv.routePermissionDecision(rw, meta)
		da := e.srv.routePermissionDecision(ra, meta)
		assert.Equal(t, da.outcome, dw.outcome, pattern)
		assert.Equal(t, da.reason, dw.reason, pattern)
		routes++
	}
	assert.Greater(t, routes, 10)
}

// --- AC5e (C1): proxy mode ----------------------------------------------------

func TestTestIdentityWeb_ProxyModeRefused(t *testing.T) {
	e := newTIWebEnv(t, func(c *WebServerConfig) {
		c.AuthMode = "proxy"
		c.ProxyAuthenticator = &fakeProxyAuthenticator{}
	})
	fx := e.issue()
	row := e.row(fx.Identity.ID)
	jar := e.craftMarkedSession(row, generateID(), *row.ExpiresAt, row.SessionGeneration)

	rec := e.redeem("scion_tiwc_x", jar...)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.True(t, tiWebCleared(rec, webSessionName), "redeem clears a marked session")
	assert.True(t, tiWebCleared(rec, testIdentitySessionCookie))
	assert.Len(t, tiWebAudits(t, e.st, testIdentityWebSessionRefuseMutation), 1)

	rec = e.apiMe(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.True(t, tiWebCleared(rec, webSessionName))
	rec = e.page(jar)
	assert.True(t, tiWebCleared(rec, webSessionName))

	// The hub side reports proxy mode as unavailable too.
	e.srv.config.AuthMode = "proxy"
	ok, cause := e.srv.testIdentityWebSessionGate()
	assert.False(t, ok)
	assert.Equal(t, testIdentityWebCauseProxyMode, cause)
	assert.Equal(t, http.StatusNotFound, e.mintRec(e.issuerTok, fx.Identity.ID).Code)
	status := doRequestNoAuth(t, e.srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	assert.Contains(t, status.Body.String(), `"testIdentityWebSessions":false`)
}

// --- AC6: fixation ------------------------------------------------------------

func TestTestIdentityWeb_RedeemReplacesIncomingSession(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()
	human := tiWebHumanSession(t, e.ws, e.issuerID, "issuer@example.com", "admin")
	code := e.mint(fx.Identity.ID).Code

	rec := e.redeem(code, human...)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	jar := tiWebApplyCookies(human, rec)
	vals := e.sessionValues(jar)
	assert.Equal(t, fx.Identity.ID, vals[sessKeyUserID])
	assert.Equal(t, fx.Identity.Email, vals[sessKeyUserEmail])
	assert.Equal(t, store.UserRoleMember, vals[sessKeyUserRole])
	for _, k := range []string{sessKeyHubAccessToken, sessKeyHubRefreshToken, sessKeyHubTokenExpiry, sessKeyReturnTo, sessKeyOAuthState} {
		_, ok := vals[k]
		assert.False(t, ok, "%s must not survive", k)
	}
	keys := map[string]bool{}
	for k := range vals {
		keys[k.(string)] = true
	}
	assert.Equal(t, map[string]bool{
		sessKeyUserID: true, sessKeyUserEmail: true, sessKeyUserName: true, sessKeyUserAvatar: true, sessKeyUserRole: true,
		sessKeySessionGeneration: true, sessKeyTIMarker: true, sessKeyTIExpiry: true, sessKeyTIDriver: true,
	}, keys)
}

// tiWebHumanSession is loginSession plus a stored hub token pair.
func tiWebHumanSession(t *testing.T, ws *WebServer, userID, email, role string) []*http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(req, webSessionName)
	require.NoError(t, err)
	access, refresh, _, err := ws.userTokenSvc.GenerateTokenPair(userID, email, "Human", role, ClientTypeWeb)
	require.NoError(t, err)
	sess.Values[sessKeyUserID] = userID
	sess.Values[sessKeyUserEmail] = email
	sess.Values[sessKeyUserRole] = role
	sess.Values[sessKeyHubAccessToken] = access
	sess.Values[sessKeyHubRefreshToken] = refresh
	sess.Values[sessKeyReturnTo] = "/admin"
	require.NoError(t, sess.Save(req, rec))
	return rec.Result().Cookies()
}

// --- AC7: no tokens, Max-Age, minted JWT expiry ------------------------------

func TestTestIdentityWeb_SessionHoldsNoTokens(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()
	code := e.mint(fx.Identity.ID).Code
	rec := e.redeem(code)
	require.Equal(t, http.StatusNoContent, rec.Code)
	row := e.row(fx.Identity.ID)
	remaining := row.ExpiresAt.Sub(e.clock.Now())

	for _, name := range []string{webSessionName, testIdentitySessionCookie} {
		c := tiWebSetCookie(rec, name)
		require.NotNil(t, c, name)
		assert.Positive(t, c.MaxAge)
		assert.LessOrEqual(t, time.Duration(c.MaxAge)*time.Second, remaining+time.Second, name)
		assert.True(t, c.HttpOnly, name)
		assert.Equal(t, http.SameSiteLaxMode, c.SameSite, name)
	}
	jar := tiWebApplyCookies(nil, rec)
	vals := e.sessionValues(jar)
	for _, k := range []string{sessKeyHubAccessToken, sessKeyHubRefreshToken, sessKeyHubTokenExpiry} {
		_, ok := vals[k]
		assert.False(t, ok, k)
	}

	require.Equal(t, http.StatusOK, e.apiMe(jar).Code)
	e.mu.Lock()
	auth := e.lastAuth
	e.mu.Unlock()
	require.True(t, strings.HasPrefix(auth, "Bearer "))
	claims, err := e.srv.userTokenService.ValidateUserToken(strings.TrimPrefix(auth, "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, ClientTypeWeb, claims.ClientType)
	assert.Equal(t, fx.Identity.ID, claims.UserID)
	exp := claims.Expiry.Time()
	assert.False(t, exp.After(time.Now().Add(testIdentityWebTokenTTL+time.Second)), "at most five minutes")
	assert.False(t, exp.After(row.ExpiresAt.Add(time.Second)), "never past the identity")

	// Near the end of the identity, the token is cut to what is left.
	e.clock.Advance(remaining - 90*time.Second)
	require.Equal(t, http.StatusOK, e.apiMe(jar).Code)
	e.mu.Lock()
	auth = e.lastAuth
	e.mu.Unlock()
	claims, err = e.srv.userTokenService.ValidateUserToken(strings.TrimPrefix(auth, "Bearer "))
	require.NoError(t, err)
	assert.LessOrEqual(t, time.Until(claims.Expiry.Time()), 91*time.Second)
}

// --- AC8: fidelity through the browser path -----------------------------------

// With the cookie only, the identity creates a project, creates, starts and
// deletes an agent, deletes the project, and /auth/me names it.
func TestTestIdentityWeb_FidelityThroughCookie(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()
	jar := e.login(fx.Identity.ID)

	api := func(method, path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		if body == nil {
			b = nil
		}
		return e.do(tiReq{method: method, path: path, body: string(b), ctype: "application/json", cookies: jar})
	}
	rec := api(http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "ti-web-fidelity"})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusOK, "create project: %d %s", rec.Code, rec.Body.String())
	var p store.Project
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	tiWebAddProvider(t, e.st, p.ID)

	rec = api(http.MethodPost, "/api/v1/projects/"+p.ID+"/agents", CreateAgentRequest{Name: "ti-web-agent"})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted, "create agent: %d %s", rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.Agent)
	stored, err := e.st.GetAgent(context.Background(), created.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, fx.Identity.ID, stored.OwnerID)

	rec = api(http.MethodPost, "/api/v1/agents/"+created.Agent.ID+"/start", nil)
	require.True(t, rec.Code >= 200 && rec.Code < 300, "start: %d %s", rec.Code, rec.Body.String())
	rec = api(http.MethodDelete, "/api/v1/agents/"+created.Agent.ID, nil)
	require.True(t, rec.Code >= 200 && rec.Code < 300, "delete agent: %d %s", rec.Code, rec.Body.String())
	rec = api(http.MethodDelete, "/api/v1/projects/"+p.ID, nil)
	require.True(t, rec.Code >= 200 && rec.Code < 300, "delete project: %d %s", rec.Code, rec.Body.String())

	rec = e.webMe(jar)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var me map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	assert.Equal(t, fx.Identity.Email, me["email"])
	ti, ok := me["testIdentity"].(map[string]any)
	require.True(t, ok, "testIdentity present for a test identity session")
	assert.NotEmpty(t, ti["expiresAt"])
	rec = e.apiMe(jar)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), fx.Identity.Email)

	// The page shell and SSR data carry the chip input too.
	rec = e.page(jar)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"testIdentity":{"expiresAt"`)
}

// --- AC9: the session dies with the row -----------------------------------

func TestTestIdentityWeb_DeathWithRow(t *testing.T) {
	assertDead := func(t *testing.T, e *tiWebEnv, jar []*http.Cookie) {
		t.Helper()
		rec := e.page(jar)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "page")
		assert.Contains(t, rec.Body.String(), "Test identity session ended")
		assert.Empty(t, rec.Header().Get("Location"), "no OAuth redirect")
		assert.Equal(t, http.StatusUnauthorized, e.webMe(jar).Code, "/auth/me")
		assert.Equal(t, http.StatusUnauthorized, e.apiMe(jar).Code, "/api/v1")
	}
	t.Run("delete", func(t *testing.T) {
		e := newTIWebEnv(t)
		fx := e.issue()
		jar := e.login(fx.Identity.ID)
		require.Equal(t, http.StatusOK, e.page(jar).Code)
		rec := doRequestWithToken(t, e.srv, e.issuerTok, http.MethodDelete, "/api/v1/test-identities/"+fx.Identity.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assertDead(t, e, jar)
	})
	t.Run("expiry", func(t *testing.T) {
		e := newTIWebEnv(t)
		fx := e.issue()
		jar := e.login(fx.Identity.ID)
		e.clock.Advance(2 * time.Hour)
		assertDead(t, e, jar)
	})
	t.Run("restart with the member level off", func(t *testing.T) {
		e := newTIWebEnv(t)
		fx := e.issue()
		jar := e.login(fx.Identity.ID)
		e.srv.testIdentities = newTestIdentityState(false)
		e.srv.authConfig.TestIdentitiesEnabled = false
		assertDead(t, e, jar)
	})
	t.Run("open event stream closes within one heartbeat", func(t *testing.T) {
		e := newTIWebEnv(t)
		e.ws.events = NewChannelEventPublisher()
		e.ws.sseHeartbeat = 100 * time.Millisecond
		fx := e.issue()
		jar := e.login(fx.Identity.ID)
		server := httptest.NewServer(e.ws.Handler())
		t.Cleanup(server.Close)

		req, err := http.NewRequest(http.MethodGet, server.URL+"/events?sub=notification.ti", nil)
		require.NoError(t, err)
		req.Header.Set("Accept", "text/event-stream")
		for _, c := range jar {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		closed := make(chan struct{})
		go func() {
			r := bufio.NewReader(resp.Body)
			for {
				if _, err := r.ReadString('\n'); err != nil {
					close(closed)
					return
				}
			}
		}()
		// Still open after a few heartbeats.
		select {
		case <-closed:
			t.Fatal("stream closed while the identity is live")
		case <-time.After(350 * time.Millisecond):
		}
		rec := doRequestWithToken(t, e.srv, e.issuerTok, http.MethodDelete, "/api/v1/test-identities/"+fx.Identity.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code)
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatal("stream still open after the identity was deleted")
		}
	})
}

// --- AC10: the member clamp through the session path ------------------------

func TestTestIdentityWeb_CrossUserIsolation(t *testing.T) {
	e := newTIWebEnv(t)
	a, b := e.issue(), e.issue()
	jarA, jarB := e.login(a.Identity.ID), e.login(b.Identity.ID)
	create := func(jar []*http.Cookie, name string) string {
		bb, _ := json.Marshal(CreateProjectRequest{Name: name})
		rec := e.do(tiReq{method: http.MethodPost, path: "/api/v1/projects", body: string(bb), ctype: "application/json", cookies: jar})
		require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusOK, rec.Body.String())
		var p store.Project
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		return p.ID
	}
	pa, pb := create(jarA, "ti-web-iso-a"), create(jarB, "ti-web-iso-b")
	agentB := tid("ti-web-iso-b-agent")
	createCredTestAgent(t, e.st, agentB, pb, b.Identity.ID)
	refused := func(code int) bool { return code == http.StatusForbidden || code == http.StatusNotFound }
	for _, r := range []tiReq{
		{method: http.MethodGet, path: "/api/v1/projects/" + pb},
		{method: http.MethodDelete, path: "/api/v1/projects/" + pb},
		{method: http.MethodGet, path: "/api/v1/agents/" + agentB},
		{method: http.MethodDelete, path: "/api/v1/agents/" + agentB},
	} {
		r.cookies = jarA
		rec := e.do(r)
		assert.True(t, refused(rec.Code), "A on B %s %s: %d", r.method, r.path, rec.Code)
	}
	assert.Equal(t, http.StatusOK, e.do(tiReq{path: "/api/v1/projects/" + pa, cookies: jarA}).Code)
	assert.Equal(t, http.StatusOK, e.do(tiReq{path: "/api/v1/agents/" + agentB, cookies: jarB}).Code)
}

// --- AC11: still refused ----------------------------------------------------

// An unmarked session for a test identity (any session not made by
// redeem) is tiWebCleared on every path, while web sessions are available.
func TestTestIdentityWeb_UnmarkedSessionCleared(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()
	jar := loginSession(t, e.ws, fx.Identity.ID, fx.Identity.Email, "member")

	rec := e.apiMe(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.True(t, tiWebCleared(rec, webSessionName))
	rec = e.page(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.True(t, tiWebCleared(rec, webSessionName))
	assert.Equal(t, http.StatusUnauthorized, e.webMe(jar).Code)

	// A marker cookie for a different identity does not mark this session.
	other := e.issue()
	ojar := e.login(other.Identity.ID)
	var marker *http.Cookie
	for _, c := range ojar {
		if c.Name == testIdentitySessionCookie {
			marker = c
		}
	}
	require.NotNil(t, marker)
	row := e.row(fx.Identity.ID)
	mixed := append(loginSession(t, e.ws, row.ID, row.Email, "member"), marker)
	assert.Equal(t, http.StatusUnauthorized, e.apiMe(mixed).Code)

	// A human session with a stray marker cookie is unaffected.
	human := append(tiWebHumanSession(t, e.ws, e.issuerID, "issuer@example.com", "admin"), marker)
	rec = e.apiMe(human)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// tiWebGetUserErrStore fails every user read with a store error.
type tiWebGetUserErrStore struct{ store.Store }

func (tiWebGetUserErrStore) GetUser(context.Context, string) (*store.User, error) {
	return nil, fmt.Errorf("synthetic store failure")
}

// A scion_sess session made with only the shared session keys that names a
// test identity uid, with an ordinary email and neither a scion_ti cookie
// nor marker values, is decided from the user row by every consumer that
// honours scion_sess (R1; advisor criteria 1-5 at 23:20Z): it is cleared or
// refused, and no token is minted or forwarded for it.
func TestTestIdentityWeb_UnmarkedSessionDecidedFromRow(t *testing.T) {
	unmarked := func(t *testing.T, e *tiWebEnv, uid string) []*http.Cookie {
		return loginSession(t, e.ws, uid, "someone@example.com", "member")
	}
	minted := func(e *tiWebEnv) string {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.lastAuth
	}
	devCfg := func(c *WebServerConfig) { c.Host = "127.0.0.1"; c.DevAuthToken = "ti-web-dev-token" }
	proxyCfg := func(c *WebServerConfig) { c.AuthMode = "proxy"; c.ProxyAuthenticator = &fakeProxyAuthenticator{} }

	t.Run("sessionToBearer", func(t *testing.T) {
		e := newTIWebEnv(t)
		fx := e.issue()
		rec := e.apiMe(unmarked(t, e, fx.Identity.ID))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.True(t, tiWebCleared(rec, webSessionName))
		assert.Empty(t, minted(e), "no token minted or forwarded")
	})
	t.Run("sessionAuth page", func(t *testing.T) {
		e := newTIWebEnv(t)
		fx := e.issue()
		rec := e.page(unmarked(t, e, fx.Identity.ID))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "Test identity session ended")
		assert.True(t, tiWebCleared(rec, webSessionName))
	})
	t.Run("/auth/me", func(t *testing.T) {
		e := newTIWebEnv(t)
		fx := e.issue()
		rec := e.webMe(unmarked(t, e, fx.Identity.ID))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.True(t, tiWebCleared(rec, webSessionName))
		assert.NotContains(t, rec.Body.String(), fx.Identity.ID)
	})
	t.Run("SSE", func(t *testing.T) {
		e := newTIWebEnv(t)
		e.ws.events = NewChannelEventPublisher()
		fx := e.issue()
		rec := e.do(tiReq{path: "/events?sub=notification.ti", cookies: unmarked(t, e, fx.Identity.ID)})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.NotContains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	})
	t.Run("logout (proxy mode)", func(t *testing.T) {
		e := newTIWebEnv(t, proxyCfg)
		fx := e.issue()
		rec := e.do(tiReq{method: http.MethodPost, path: "/auth/logout", cookies: unmarked(t, e, fx.Identity.ID)})
		assert.True(t, tiWebCleared(rec, webSessionName))
	})
	t.Run("proxyAuth", func(t *testing.T) {
		e := newTIWebEnv(t, proxyCfg)
		fx := e.issue()
		rec := e.page(unmarked(t, e, fx.Identity.ID))
		assert.True(t, tiWebCleared(rec, webSessionName), "status %d", rec.Code)
	})
	t.Run("devAuth page", func(t *testing.T) {
		e := newTIWebEnv(t, devCfg)
		fx := e.issue()
		rec := e.page(unmarked(t, e, fx.Identity.ID))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "Test identity session ended")
		assert.True(t, tiWebCleared(rec, webSessionName))
	})
	t.Run("devAuth /auth/me", func(t *testing.T) {
		e := newTIWebEnv(t, devCfg)
		fx := e.issue()
		rec := e.webMe(unmarked(t, e, fx.Identity.ID))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.NotContains(t, rec.Body.String(), fx.Identity.ID)
		assert.NotContains(t, rec.Body.String(), DevUserID)
	})
	t.Run("SSE under dev auth", func(t *testing.T) {
		e := newTIWebEnv(t, devCfg)
		e.ws.events = NewChannelEventPublisher()
		fx := e.issue()
		jar := unmarked(t, e, fx.Identity.ID)
		rec := e.do(tiReq{path: "/events?sub=notification.ti", cookies: jar})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		// The stream decides at accept time on its own too: with a context
		// user already set (as any admitting middleware would), the handler
		// refuses before it subscribes or writes stream headers.
		req := tiWebRequest(jar)
		req.URL.RawQuery = "sub=notification.ti"
		req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, &webSessionUser{UserID: fx.Identity.ID, Email: "someone@example.com"}))
		rec = httptest.NewRecorder()
		e.ws.handleSSE(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.NotContains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	})
	t.Run("store error fails closed", func(t *testing.T) {
		e := newTIWebEnv(t)
		fx := e.issue()
		jar := unmarked(t, e, fx.Identity.ID)
		e.ws.store = tiWebGetUserErrStore{e.st}
		rec := e.apiMe(jar)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Empty(t, minted(e), "no token minted on a store error")
		// /auth/me grants no authority: an unhinted session keeps the
		// degraded session-fields answer (TestWebAuthMe_StoreError...),
		// while a hinted one is refused.
		assert.Equal(t, http.StatusOK, e.webMe(jar).Code)
		hinted := loginSession(t, e.ws, fx.Identity.ID, fx.Identity.Email, "member")
		assert.NotEqual(t, http.StatusOK, e.webMe(hinted).Code, "hinted + store error is refused")
		rec = e.apiMe(hinted)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Empty(t, minted(e), "no token minted for a hinted session on a store error")

		// The event stream refuses at accept time on a store error.
		e.ws.events = NewChannelEventPublisher()
		req := tiWebRequest(jar)
		req.URL.RawQuery = "sub=notification.ti"
		req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, &webSessionUser{UserID: fx.Identity.ID}))
		sse := httptest.NewRecorder()
		e.ws.handleSSE(sse, req)
		assert.Equal(t, http.StatusUnauthorized, sse.Code)
		assert.NotContains(t, sse.Header().Get("Content-Type"), "text/event-stream")
	})
	t.Run("store error fails closed under dev auth", func(t *testing.T) {
		e := newTIWebEnv(t, devCfg)
		fx := e.issue()
		jar := unmarked(t, e, fx.Identity.ID)
		e.ws.store = tiWebGetUserErrStore{e.st}
		rec := e.page(jar)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), DevUserID)
	})
	t.Run("no store with test identities on: deny closed", func(t *testing.T) {
		e := newTIWebEnv(t)
		fx := e.issue()
		jar := unmarked(t, e, fx.Identity.ID)
		e.ws.store = nil
		rec := e.apiMe(jar)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Empty(t, minted(e), "no token minted without a store")
		res := e.ws.classifyTestIdentitySession(tiWebRequest(jar), tiWebSession(t, e.ws, jar), nil)
		assert.Equal(t, tiSessionUnverifiable, res.state)
		assert.Equal(t, http.StatusUnauthorized, e.webMe(jar).Code, "/auth/me denies without a store")
	})
	t.Run("no store with test identities on: dev auth denies", func(t *testing.T) {
		e := newTIWebEnv(t, devCfg)
		fx := e.issue()
		jar := unmarked(t, e, fx.Identity.ID)
		e.ws.store = nil
		rec := e.page(jar)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), DevUserID)
	})

	// An ordinary user's overflow session (no stored token) still works.
	e := newTIWebEnv(t)
	human := loginSession(t, e.ws, e.issuerID, "issuer@example.com", "admin")
	assert.Equal(t, http.StatusOK, e.apiMe(human).Code)
	assert.Equal(t, http.StatusOK, e.webMe(human).Code)
}

// tiWebCountingStore counts the web server's user row reads.
type tiWebCountingStore struct {
	store.Store
	mu sync.Mutex
	n  int
}

func (c *tiWebCountingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Store.GetUser(ctx, id)
}

func (c *tiWebCountingStore) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// The classifier's kind-cache fast path: (a) a cache miss always reads the
// row and never answers NotFixture unread; (b) the full-read trigger set is
// a hint or a cached test_fixture kind; a cached human with no hint is
// answered from the cache; a missing user is never cached.
func TestTestIdentityWeb_ClassifierKindCache(t *testing.T) {
	e := newTIWebEnv(t)
	cs := &tiWebCountingStore{Store: e.st}
	e.ws.store = cs
	classify := func(jar []*http.Cookie) tiSessionResult {
		return e.ws.classifyTestIdentitySession(tiWebRequest(jar), tiWebSession(t, e.ws, jar), nil)
	}

	human := loginSession(t, e.ws, e.issuerID, "issuer@example.com", "admin")
	before := cs.reads()
	assert.Equal(t, tiSessionNotFixture, classify(human).state)
	assert.Equal(t, before+1, cs.reads(), "(a) a cache miss reads the row")
	assert.Equal(t, tiSessionNotFixture, classify(human).state)
	assert.Equal(t, before+1, cs.reads(), "a cached human with no hint is answered from the cache")
	assert.Equal(t, http.StatusOK, e.apiMe(human).Code)
	assert.Equal(t, before+1, cs.reads(), "repeat requests hit the cache")

	// (b) a cached test_fixture kind always reads the row.
	fx := e.issue()
	unmarked := loginSession(t, e.ws, fx.Identity.ID, "someone@example.com", "member")
	before = cs.reads()
	assert.Equal(t, tiSessionInvalid, classify(unmarked).state)
	assert.Equal(t, tiSessionInvalid, classify(unmarked).state)
	assert.Equal(t, before+2, cs.reads(), "a cached fixture kind reads the row every time")
	assert.Equal(t, tiKindFixture, e.ws.testIdentityKindCached(fx.Identity.ID))

	// (b) any hint reads the row, even for a cached human.
	other := e.issue()
	var marker *http.Cookie
	for _, c := range e.login(other.Identity.ID) {
		if c.Name == testIdentitySessionCookie {
			marker = c
		}
	}
	require.NotNil(t, marker)
	hinted := append(append([]*http.Cookie{}, human...), marker)
	before = cs.reads()
	assert.Equal(t, tiSessionNotFixture, classify(hinted).state, "a stray marker does not mark a human session")
	assert.Equal(t, tiSessionNotFixture, classify(hinted).state)
	assert.Equal(t, before+2, cs.reads(), "a hint forces the row read")

	// A missing user is never cached.
	ghost := loginSession(t, e.ws, generateID(), "ghost@example.com", "member")
	before = cs.reads()
	assert.Equal(t, tiSessionUserMissing, classify(ghost).state)
	assert.Equal(t, tiSessionUserMissing, classify(ghost).state)
	assert.Equal(t, before+2, cs.reads())

	// A non-fixture row with a reserved-domain email (which a real store
	// refuses to create) is cached as a test identity row, so it never
	// takes the fast path: every request reads the row.
	t.Run("reserved-domain email on a non-fixture row", func(t *testing.T) {
		e := newTIWebEnv(t)
		rs := &tiWebReservedEmailStore{Store: e.st, uid: e.issuerID}
		e.ws.store = rs
		jar := loginSession(t, e.ws, e.issuerID, "issuer@example.com", "admin")
		req := func() tiSessionResult {
			return e.ws.classifyTestIdentitySession(tiWebRequest(jar), tiWebSession(t, e.ws, jar), nil)
		}
		before := rs.reads()
		assert.Equal(t, tiSessionInvalid, req().state)
		assert.Equal(t, tiSessionInvalid, req().state)
		assert.Equal(t, tiKindFixture, e.ws.testIdentityKindCached(e.issuerID))
		assert.Equal(t, before+2, rs.reads(), "never answered from the cache")
	})
}

// tiWebReservedEmailStore returns uid's row with its email moved into the
// reserved test identity domain, counting reads.
type tiWebReservedEmailStore struct {
	store.Store
	uid string
	mu  sync.Mutex
	n   int
}

func (c *tiWebReservedEmailStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	u, err := c.Store.GetUser(ctx, id)
	if err != nil || id != c.uid {
		return u, err
	}
	cp := *u
	cp.Email = "moved@" + store.TestFixtureEmailDomain
	return &cp, nil
}

func (c *tiWebReservedEmailStore) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// In proxy mode the redeem refusal is rate-limited before it is audited:
// burst+1 requests give one 429 and exactly burst audit rows (N3).
func TestTestIdentityWeb_ProxyRedeemLimitedBeforeAudit(t *testing.T) {
	e := newTIWebEnv(t, func(c *WebServerConfig) {
		c.AuthMode = "proxy"
		c.ProxyAuthenticator = &fakeProxyAuthenticator{}
	})
	const burst = 3
	e.ws.testIdentityRedeemLimiter = newTestLoginLimiter()
	e.ws.testIdentityRedeemLimiter.burst = burst
	e.ws.testIdentityRedeemLimiter.perSecond = 0.0001
	var last int
	for i := 0; i < burst+1; i++ {
		last = e.redeem("scion_tiwc_x").Code
	}
	assert.Equal(t, http.StatusTooManyRequests, last)
	assert.Len(t, tiWebAudits(t, e.st, testIdentityWebSessionRefuseMutation), burst)
}

// A login onto a browser that holds a test identity session replaces it
// cleanly: the test identity values and marker cookie are dropped, and the
// new human session works on the next request (N2).
func TestTestIdentityWeb_LoginReplacesTestIdentitySession(t *testing.T) {
	e := newTIWebEnv(t, func(c *WebServerConfig) { c.EnableTestLogin = true })
	fx := e.issue()
	jar := e.login(fx.Identity.ID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(`{"email":"human-after-ti@example.com","role":"member"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testLoginAuthHeader(t, e.srv.userTokenService))
	for _, c := range jar {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.ws.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, tiWebCleared(rec, testIdentitySessionCookie), "the marker cookie is expired")
	jar = tiWebApplyCookies(jar, rec)
	vals := e.sessionValues(jar)
	for _, k := range []string{sessKeyTIMarker, sessKeyTIExpiry, sessKeyTIDriver} {
		_, ok := vals[k]
		assert.False(t, ok, k)
	}

	rec = e.apiMe(jar)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "human-after-ti@example.com")
}

// Under dev auth, a test identity session that fails its checks ends: the
// request is never served as the dev user.
func TestTestIdentityWeb_DevAuthNoFallThrough(t *testing.T) {
	e := newTIWebEnv(t, func(c *WebServerConfig) {
		c.Host = "127.0.0.1"
		c.DevAuthToken = "ti-web-dev-token"
	})
	fx := e.issue()
	jar := e.login(fx.Identity.ID)
	rec := e.webMe(jar)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), fx.Identity.Email)

	require.NoError(t, e.st.IncrementSessionGeneration(context.Background(), fx.Identity.ID))
	rec = e.page(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "Test identity session ended")
	assert.NotContains(t, rec.Body.String(), DevUserID)
}

// Logout clears both cookies (G-e), including in proxy mode.
func TestTestIdentityWeb_LogoutClears(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()
	jar := e.login(fx.Identity.ID)
	rec := e.do(tiReq{method: http.MethodPost, path: "/auth/logout", cookies: jar})
	assert.True(t, tiWebCleared(rec, webSessionName))
	assert.True(t, tiWebCleared(rec, testIdentitySessionCookie))

	pe := newTIWebEnvOn(t, e.srv, e.st, tiWebKey(t), func(c *WebServerConfig) {
		c.AuthMode = "proxy"
		c.ProxyAuthenticator = &fakeProxyAuthenticator{}
	})
	row := e.row(fx.Identity.ID)
	pjar := pe.craftMarkedSession(row, generateID(), *row.ExpiresAt, row.SessionGeneration)
	rec = pe.do(tiReq{method: http.MethodPost, path: "/auth/logout", cookies: pjar})
	assert.True(t, tiWebCleared(rec, webSessionName), "proxy-mode logout clears a test identity session")
	assert.True(t, tiWebCleared(rec, testIdentitySessionCookie))
}

// --- A5: indicator ------------------------------------------------------------

func TestTestIdentityWeb_Indicator(t *testing.T) {
	e := newTIWebEnv(t)
	status := doRequestNoAuth(t, e.srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	assert.JSONEq(t, `{"testIdentities":true,"testHubAdmin":false,"testSuperAdmin":false,"testIdentityWebSessions":true}`, status.Body.String())

	rec := doRequest(t, e.srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Hub.TestIdentityWebSessionsEnabled)
	assert.Contains(t, rec.Body.String(), `"test_identity_web_sessions_enabled":true`)

	srv, _ := newTestIdentityServer(t, true) // no key
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"test_identity_web_sessions_enabled":false`)
}

// A web server with no session store (a bare test server) can hold no test
// identity session: the SSE re-check reports invalid, and /auth/me and
// logout do not touch the missing store.
func TestTestIdentityWeb_NilSessionStoreFailsClosed(t *testing.T) {
	ws := &WebServer{}
	req := httptest.NewRequest(http.MethodGet, "/events?sub=notification.ti", nil)
	req.AddCookie(&http.Cookie{Name: testIdentitySessionCookie, Value: "x"})
	assert.False(t, ws.sseTestIdentitySessionStillValid(req))

	me := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	me = me.WithContext(context.WithValue(me.Context(), webUserContextKey{}, &webSessionUser{UserID: "u", Email: "u@example.com"}))
	rec := httptest.NewRecorder()
	assert.NotPanics(t, func() { ws.handleAuthMe(rec, me) })
	assert.Equal(t, http.StatusOK, rec.Code)

	pws := &WebServer{config: WebServerConfig{AuthMode: "proxy"}}
	rec = httptest.NewRecorder()
	assert.NotPanics(t, func() { pws.handleLogout(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil)) })
	assert.Equal(t, http.StatusOK, rec.Code)
}

// The startup log line names availability without key material.
func TestTestIdentityWeb_StartupLogLine(t *testing.T) {
	logs := tiWebCaptureLogs(t)
	key := tiWebKey(t)
	srv := &Server{testIdentities: newTestIdentityState(true), testIdentityWeb: newTestIdentityWebState(TestIdentityWebSessionKey{Key: key})}
	srv.logTestIdentityWebSessionState()
	assert.Contains(t, logs.String(), "Test identity web sessions are available")
	assert.NotContains(t, logs.String(), fmt.Sprintf("%x", key))

	logs.Reset()
	(&Server{}).logTestIdentityWebSessionState()
	assert.Empty(t, logs.String(), "nothing logged with the member level off")
}
