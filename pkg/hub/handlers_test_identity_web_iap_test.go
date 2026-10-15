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

// Test identity web sessions on proxy-auth hubs (ptone/scion#4240, phase
// WS2). These tests run the real hub (SQLite store, auth middleware, route
// guards) under the real web server in proxy auth mode, with a real IAP
// authenticator that verifies assertions signed with a local key set, and
// drive it with cookies and assertion headers the way an IAP-fronted
// browser driver does.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- Harness ----------------------------------------------------------------

const tiDriverTokenAudience = "1234-ti-iap-client.apps.googleusercontent.com"

type tiIAPEnv struct {
	*tiWebEnv
	iap *tiIAP
}

// newTIIAPEnv builds a member-level test hub with a web session key in
// proxy auth mode (a real IAP authenticator, both on the hub and the web
// server) with the given driver principals.
func newTIIAPEnv(t *testing.T, drivers []string, cfgFns ...func(*WebServerConfig)) *tiIAPEnv {
	t.Helper()
	srv, st := newTestIdentityServer(t, true)
	iap := newTIIAP(t)
	srv.config.AuthMode = "proxy"
	srv.config.ProxyAuth = iap.auth
	srv.proxyDrivers = newDriverPrincipalSet(drivers)
	srv.authConfig.AuthMode = "proxy"
	srv.authConfig.ProxyAuthenticator = iap.auth
	srv.authConfig.ProxyUserProvisioner = MakeProxyUserProvisioner(srv)
	srv.authConfig.DriverPrincipals = srv.proxyDrivers
	fns := append([]func(*WebServerConfig){func(c *WebServerConfig) {
		c.AuthMode = "proxy"
		c.ProxyAuthenticator = iap.auth
		c.ProxyDriverPrincipals = drivers
	}}, cfgFns...)
	return &tiIAPEnv{tiWebEnv: newTIWebEnvOn(t, srv, st, tiWebKey(t), fns...), iap: iap}
}

// withAssertion adds a valid IAP assertion for email ("" adds none).
func (e *tiIAPEnv) withAssertion(email string, r tiReq) tiReq {
	if email == "" {
		return r
	}
	h := map[string]string{}
	for k, v := range r.headers {
		h[k] = v
	}
	h[IAPAssertionHeader] = e.iap.assertion(e.t, email)
	r.headers = h
	return r
}

func (e *tiIAPEnv) as(email string, r tiReq) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(e.withAssertion(email, r))
}

func tiRedeemReq(code string, cookies ...*http.Cookie) tiReq {
	b, _ := json.Marshal(map[string]string{"code": code})
	return tiReq{method: http.MethodPost, path: "/auth/test-identity/redeem", body: string(b), ctype: "application/json", cookies: cookies}
}

func (e *tiIAPEnv) redeemAs(email, code string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.as(email, tiRedeemReq(code, cookies...))
}

// loginAs mints a code for id and redeems it with a driver assertion for
// email, returning the session cookies.
func (e *tiIAPEnv) loginAs(email, id string) []*http.Cookie {
	e.t.Helper()
	rec := e.redeemAs(email, e.mint(id).Code)
	require.Equal(e.t, http.StatusNoContent, rec.Code, rec.Body.String())
	return tiWebApplyCookies(nil, rec)
}

func (e *tiIAPEnv) apiMeAs(email string, jar []*http.Cookie) *httptest.ResponseRecorder {
	return e.as(email, tiReq{path: "/api/v1/auth/me", cookies: jar})
}

func (e *tiIAPEnv) webMeAs(email string, jar []*http.Cookie) *httptest.ResponseRecorder {
	return e.as(email, tiReq{path: "/auth/me", cookies: jar})
}

func (e *tiIAPEnv) pageAs(email string, jar []*http.Cookie) *httptest.ResponseRecorder {
	return e.as(email, tiReq{path: "/projects", cookies: jar, browser: true})
}

// doFrom is tiWebEnv.do from a chosen source address.
func (e *tiWebEnv) doFrom(remote string, r tiReq) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	if r.method == "" {
		r.method = http.MethodGet
	}
	req := httptest.NewRequest(r.method, r.path, body)
	req.RemoteAddr = remote
	if r.ctype != "" {
		req.Header.Set("Content-Type", r.ctype)
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

// takeLastAuth returns the Authorization header the hub saw on the last
// request and resets it.
func (e *tiWebEnv) takeLastAuth() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.lastAuth
	e.lastAuth = "<none>"
	return v
}

// craftDriverSession is craftMarkedSession with a driver in both the
// session values and the marker cookie.
func (e *tiWebEnv) craftDriverSession(row *store.User, driver string) []*http.Cookie {
	e.t.Helper()
	codeID := generateID()
	expiry := *row.ExpiresAt
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	sess, err := e.ws.sessionStore.New(req, webSessionName)
	require.NoError(e.t, err)
	sess.Values[sessKeyUserID] = row.ID
	sess.Values[sessKeyUserEmail] = row.Email
	sess.Values[sessKeyUserName] = row.DisplayName
	sess.Values[sessKeyUserRole] = row.Role
	sess.Values[sessKeySessionGeneration] = row.SessionGeneration
	sess.Values[sessKeyTIMarker] = codeID
	sess.Values[sessKeyTIExpiry] = expiry.UnixMilli()
	sess.Values[sessKeyTIDriver] = driver
	require.NoError(e.t, sess.Save(req, rec))
	enc, err := e.srv.testIdentityWeb.codec.Encode(testIdentitySessionCookie, tiMarkerValue{UserID: row.ID, CodeID: codeID, Expiry: expiry.UnixMilli(), Driver: driver})
	require.NoError(e.t, err)
	return append(tiWebApplyCookies(nil, rec), &http.Cookie{Name: testIdentitySessionCookie, Value: enc})
}

// markerOf decodes the scion_ti cookie of a jar.
func (e *tiWebEnv) markerOf(jar []*http.Cookie) tiMarkerValue {
	e.t.Helper()
	var m tiMarkerValue
	for _, c := range jar {
		if c.Name == testIdentitySessionCookie {
			require.NoError(e.t, e.srv.testIdentityWeb.codec.Decode(testIdentitySessionCookie, c.Value, &m))
			return m
		}
	}
	e.t.Fatal("no marker cookie")
	return m
}

func tiNoUserRow(t *testing.T, st store.Store, email string) {
	t.Helper()
	_, err := st.GetUserByEmail(context.Background(), email)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no user row for %s: %v", email, err)
}

func tiAuditFor(t *testing.T, st store.Store, reason string) *store.MutationAuditRecord {
	t.Helper()
	for _, a := range tiWebAudits(t, st, testIdentityWebSessionRefuseMutation) {
		if strings.Contains(a.AfterSummary, `"reason":"`+reason+`"`) {
			a := a
			return &a
		}
	}
	t.Fatalf("no refusal audit with reason %s", reason)
	return nil
}

// --- A1: config, startup, and an empty driver list -----------------------------

// A non-proxy hub with a non-empty driverPrincipals list refuses to start.
func TestTIWebIAP_NonProxyHubWithDriversRefusesStartup(t *testing.T) {
	for _, mode := range []string{"", "oauth", "dev"} {
		cfg := testServerConfig()
		cfg.AuthMode = mode
		cfg.ProxyDriverPrincipals = []string{tiDriverEmail}
		_, _, err := tiaNewServer(t, cfg)
		assert.ErrorIs(t, err, config.ErrProxyDriverPrincipalsWithoutProxyAuth, "mode %q", mode)
	}
	cfg := testServerConfig()
	cfg.AuthMode = "proxy"
	cfg.ProxyAuth = newTIIAP(t).auth
	cfg.ProxyDriverPrincipals = []string{"*@ti-test.iam.gserviceaccount.com"}
	_, _, err := tiaNewServer(t, cfg)
	assert.ErrorIs(t, err, config.ErrInvalidProxyDriverPrincipal, "no wildcards")

	cfg.ProxyDriverPrincipals = []string{" UI-Driver@TI-Test.iam.gserviceaccount.com "}
	srv, _, err := tiaNewServer(t, cfg)
	require.NoError(t, err)
	assert.True(t, srv.proxyDrivers.contains(tiDriverEmail), "normalized")
	assert.True(t, srv.authConfig.DriverPrincipals.contains(tiDriverEmail), "Step 3a gets the same set")
}

// DECIDED (test-ident-lead, 15:21Z): a proxy hub with test identities at
// the member level and no driver principals starts; web sessions are
// unavailable (redeem 403, marked sessions cleared with 401), the member
// tier still runs, one startup line says so, and both status endpoints
// report web sessions unavailable with the reason.
func TestTIWebIAP_ProxyWithoutDriversStartsWithWebSessionsUnavailable(t *testing.T) {
	logs := tiWebCaptureLogs(t)
	iap := newTIIAP(t)
	cfg := testServerConfig()
	cfg.AuthMode = "proxy"
	cfg.ProxyAuth = iap.auth
	cfg.HubID = tiaHubID
	cfg.EnableTestIdentities = true
	cfg.TestMode = TestModeConfig{Level: TestModeMember, HubID: tiaHubID, HubClass: config.HubClassTest, MarkerHubID: tiaHubID, MarkerClass: config.HubClassTest}
	cfg.TestIdentityWebSessionKey = TestIdentityWebSessionKey{Key: tiWebKey(t)}
	srv, st, err := tiaNewServer(t, cfg)
	require.NoError(t, err, "no startup refusal")
	assert.Equal(t, 1, strings.Count(logs.String(), testIdentityWebNoDriversLogLine), logs.String())
	assert.Equal(t, "test-identity web sessions unavailable: proxy auth mode with no driverPrincipals", testIdentityWebNoDriversLogLine)

	ok, cause := srv.testIdentityWebSessionGate()
	assert.False(t, ok)
	assert.Equal(t, testIdentityWebCauseNoDriverPrincipals, cause)

	status := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	require.Equal(t, http.StatusOK, status.Code)
	assert.JSONEq(t, `{"testIdentities":true,"testHubAdmin":false,"testSuperAdmin":false,"testIdentityWebSessions":false,"testIdentityWebSessionsUnavailableReason":"proxy_auth_without_driver_principals","testIssuerAudience":""}`, status.Body.String())
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var summary HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &summary))
	assert.False(t, summary.Hub.TestIdentityWebSessionsEnabled)
	assert.Equal(t, testIdentityWebCauseNoDriverPrincipals, summary.Hub.TestIdentityWebSessionsUnavailableReason)
	assert.Contains(t, rec.Body.String(), `"test_identity_web_sessions_unavailable_reason":"proxy_auth_without_driver_principals"`)

	e := &tiIAPEnv{tiWebEnv: newTIWebEnvOn(t, srv, st, tiWebKey(t), func(c *WebServerConfig) {
		c.AuthMode = "proxy"
		c.ProxyAuthenticator = iap.auth
	}), iap: iap}
	fx := e.issue()
	assert.Equal(t, http.StatusNotFound, e.mintRec(e.issuerTok, fx.Identity.ID).Code, "no codes are minted")

	rec = e.redeemAs(tiDriverEmail, "scion_tiwc_x")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), testIdentityWebCauseNoDriverPrincipals)

	// No driver principal exists here, so the assertion alone would be an
	// ordinary user on /api/v1/; the requests carry none.
	row := e.row(fx.Identity.ID)
	jar := e.craftDriverSession(row, tiDriverEmail)
	for name, send := range map[string]func() *httptest.ResponseRecorder{
		"api":     func() *httptest.ResponseRecorder { return e.apiMeAs("", jar) },
		"page":    func() *httptest.ResponseRecorder { return e.pageAs("", jar) },
		"auth/me": func() *httptest.ResponseRecorder { return e.webMeAs("", jar) },
	} {
		rec := send()
		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.True(t, tiWebCleared(rec, webSessionName), "%s clears scion_sess", name)
	}

	// The member tier is unaffected.
	assert.Equal(t, http.StatusOK, tiAuthMe(t, srv, e.issue().AccessToken).Code)
}

var tiDriverKeyBodies = map[string]struct {
	body string
	keys []string
}{
	"principals nested":      {`{"server":{"auth":{"proxy":{"driver_principals":["ui-driver@ti-test.iam.gserviceaccount.com"]}}}}`, []string{config.ProxyDriverPrincipalsKey}},
	"principals flat dotted": {`{"server.auth.proxy.driver_principals":["a@example.com"]}`, []string{config.ProxyDriverPrincipalsKey}},
	"principals empty":       {`{"server":{"auth":{"proxy":{"driver_principals":[]}}}}`, []string{config.ProxyDriverPrincipalsKey}},
	"audience nested":        {`{"server":{"auth":{"proxy":{"driver_token_audience":"x.apps.googleusercontent.com"}}}}`, []string{config.ProxyDriverTokenAudienceKey}},
	"audience case folded":   {`{"Server":{"Auth":{"PROXY":{"Driver_Token_Audience":"x"}}}}`, []string{config.ProxyDriverTokenAudienceKey}},
	"both with others":       {`{"default_model":"m1","server":{"auth":{"proxy":{"provider":"iap","driver_token_audience":"x","driver_principals":null}}}}`, []string{config.ProxyDriverPrincipalsKey, config.ProxyDriverTokenAudienceKey}},
}

// Both keys are never API-writable: every settings write path answers 422,
// in file, DB and workstation mode.
func TestNeverAPIWritable_ProxyDriverKeysRefusedInFileMode(t *testing.T) {
	for name, c := range tiDriverKeyBodies {
		t.Run(name, func(t *testing.T) {
			rr, settingsPath := fileModePutServerConfig(t, &Server{}, c.body)
			assert.Equal(t, c.keys, tiaErrorKeys(t, rr))
			_, err := os.Stat(settingsPath)
			assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
		})
	}
}

func TestNeverAPIWritable_ProxyDriverKeysRefusedInDBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, c := range tiDriverKeyBodies {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				require.Equal(t, workstation, srv.layer0Editable())
				before := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, c.body)
				assert.Equal(t, c.keys, tiaErrorKeys(t, rr))
				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.NotContains(t, string(data), "driver_")
				assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
			})
		}
	}
}

// The server-config GET does not show the keys, and its body echoes back
// cleanly with the file values kept.
func TestNeverAPIWritable_GetDoesNotEchoProxyDriverKeys(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	settingsPath := tmpHome + "/.scion/settings.yaml"
	require.NoError(t, os.MkdirAll(tmpHome+"/.scion", 0o700))
	yaml := "schema_version: \"1\"\nserver:\n  auth:\n    mode: proxy\n    proxy:\n      provider: iap\n      iap:\n        audience: /projects/1/global/backendServices/2\n      driver_principals:\n        - " + tiDriverEmail + "\n      driver_token_audience: " + tiDriverTokenAudience + "\n"
	require.NoError(t, os.WriteFile(settingsPath, []byte(yaml), 0o644))
	srv := &Server{}
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	assert.NotContains(t, getRR.Body.String(), "driver_principals")
	assert.NotContains(t, getRR.Body.String(), tiDriverEmail)
	assert.NotContains(t, getRR.Body.String(), tiDriverTokenAudience)
	assert.Contains(t, getRR.Body.String(), "backendServices", "the rest of the proxy settings is shown")
	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
	assert.Equal(t, http.StatusOK, echo.Code, echo.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), tiDriverEmail)
	assert.Contains(t, string(data), tiDriverTokenAudience)
}

// --- A2: a driver principal is never a user ------------------------------------

func TestTIWebIAP_DriverPrincipalHasNoSession(t *testing.T) {
	e := newTIIAPEnv(t, []string{tiDriverEmail})

	// Web fresh-identity path: 403 before find-or-create.
	rec := e.pageAs(tiDriverEmail, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), driverPrincipalNoSessionReason)
	assert.Nil(t, tiWebSetCookie(rec, webSessionName), "no session is created")

	// API Step 3a (no bearer, no session): 403, no fall-through.
	for _, path := range []string{"/api/v1/auth/me", "/api/v1/projects"} {
		rec = e.as(tiDriverEmail, tiReq{path: path})
		assert.Equal(t, http.StatusForbidden, rec.Code, path)
		assert.Contains(t, rec.Body.String(), driverPrincipalNoSessionReason, path)
	}
	tiNoUserRow(t, e.st, tiDriverEmail)

	// The refusal is specific: another verified principal still signs in.
	rec = e.pageAs(tiNonDriverEmail, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err := e.st.GetUserByEmail(context.Background(), tiNonDriverEmail)
	assert.NoError(t, err)

	// C3: when the driver's test identity session ends, the next request
	// is refused and the one after that is the driver's 403; it is never
	// served as the driver.
	fx := e.issue()
	jar := e.loginAs(tiDriverEmail, fx.Identity.ID)
	require.Equal(t, http.StatusOK, e.pageAs(tiDriverEmail, jar).Code)
	require.NoError(t, e.st.IncrementSessionGeneration(context.Background(), fx.Identity.ID))
	rec = e.pageAs(tiDriverEmail, jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.True(t, tiWebCleared(rec, webSessionName))
	jar = tiWebApplyCookies(jar, rec)
	assert.Empty(t, jar)
	assert.Equal(t, http.StatusForbidden, e.pageAs(tiDriverEmail, jar).Code)
	assert.Equal(t, http.StatusForbidden, e.apiMeAs(tiDriverEmail, jar).Code)
	tiNoUserRow(t, e.st, tiDriverEmail)
}

// --- A3: redeem in proxy mode ---------------------------------------------------

// The driver refusals all come before the code is read, so a refused
// redeem never consumes the code (refusals after the consume, such as an
// identity that is no longer admitted, keep WS1's semantics: the code is
// used up).
func TestTIWebIAP_RedeemProxyMode(t *testing.T) {
	logs := tiWebCaptureLogs(t)
	e := newTIIAPEnv(t, []string{tiDriverEmail})
	fx := e.issue()
	code := e.mint(fx.Identity.ID).Code
	hashPrefix := testIdentityWebHashPrefix(testIdentityWebCodeHash(code))

	rec := e.redeemAs(tiNonDriverEmail, code)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "driver_principal_required")
	rec = e.redeemAs("", code)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "driver_assertion_missing")
	bad := tiRedeemReq(code)
	bad.headers = map[string]string{IAPAssertionHeader: e.iap.otherKeyAssertion(t, tiDriverEmail)}
	rec = e.do(bad)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "driver_assertion_invalid")
	for _, r := range []struct{ reason, email string }{
		{"driver_principal_required", tiNonDriverEmail},
		{"driver_assertion_missing", ""},
		{"driver_assertion_invalid", ""},
	} {
		a := tiAuditFor(t, e.st, r.reason)
		assert.JSONEq(t, fmt.Sprintf(`{"reason":%q,"code_hash_prefix":"","client_ip":"192.0.2.10","assertion_email":%q}`, r.reason, r.email), a.AfterSummary)
		b, _ := json.Marshal(a)
		assert.NotContains(t, string(b), code)
	}

	// Not consumed: the driver redeems the same code.
	rec = e.redeemAs(tiDriverEmail, code)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	jar := tiWebApplyCookies(nil, rec)
	assert.Equal(t, tiDriverEmail, e.sessionValues(jar)[sessKeyTIDriver], "tiDriver is the verified assertion email")
	assert.Equal(t, tiDriverEmail, e.markerOf(jar).Driver)
	created := tiWebAudits(t, e.st, testIdentityWebSessionCreateMutation)
	require.Len(t, created, 1)
	assert.Contains(t, created[0].AfterSummary, `"driver_principal":"`+tiDriverEmail+`"`)
	rec = e.apiMeAs(tiDriverEmail, jar)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), fx.Identity.Email)

	// Single use, and the failure audit carries the hash prefix, IP and
	// assertion email only (golden).
	rec = e.redeemAs(tiDriverEmail, code)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	a := tiAuditFor(t, e.st, "code_already_used")
	assert.JSONEq(t, fmt.Sprintf(`{"reason":"code_already_used","code_hash_prefix":%q,"client_ip":"192.0.2.10","assertion_email":%q}`, hashPrefix, tiDriverEmail), a.AfterSummary)
	assert.Equal(t, hashPrefix, a.TargetID)

	// Origin refusal behind the proxy is audited with the assertion email.
	origin := e.withAssertion(tiDriverEmail, tiRedeemReq(code))
	origin.headers["Origin"] = "https://other-origin.example"
	assert.Equal(t, http.StatusForbidden, e.do(origin).Code)
	assert.Contains(t, tiAuditFor(t, e.st, "origin_refused").AfterSummary, `"assertion_email":"`+tiDriverEmail+`"`)

	assert.NotContains(t, logs.String(), code)
}

// Non-proxy redeem is unchanged: no assertion is needed, one that is sent
// is ignored, and the session names no driver.
func TestTIWebIAP_NonProxyRedeemNeedsNoAssertion(t *testing.T) {
	e := newTIWebEnv(t)
	iap := newTIIAP(t)
	fx := e.issue()
	req := tiRedeemReq(e.mint(fx.Identity.ID).Code)
	req.headers = map[string]string{IAPAssertionHeader: iap.assertion(t, tiDriverEmail)}
	rec := e.do(req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	jar := tiWebApplyCookies(nil, rec)
	assert.Equal(t, "", e.sessionValues(jar)[sessKeyTIDriver])
	assert.Equal(t, "", e.markerOf(jar).Driver)
	assert.Contains(t, tiWebAudits(t, e.st, testIdentityWebSessionCreateMutation)[0].AfterSummary, `"driver_principal":"none"`)
	assert.Equal(t, http.StatusOK, e.apiMe(jar).Code)
}

// --- A6 (C8): per-email limit, apart from per-IP ------------------------------------

func TestTIWebIAP_RedeemRateLimitedPerEmail(t *testing.T) {
	e := newTIIAPEnv(t, []string{tiDriverEmail, tiDriver2Email})
	e.ws.testIdentityRedeemLimiter = newTestLoginLimiter()
	e.ws.testIdentityRedeemLimiter.burst = 2
	e.ws.testIdentityRedeemLimiter.perSecond = 0.0001
	unknown := testIdentityWebCodePrefix + "unknown-code-value"

	// Fresh source IPs, one email: the email's bucket trips.
	var codes []int
	for i := 1; i <= 3; i++ {
		codes = append(codes, e.doFrom(fmt.Sprintf("192.0.2.%d:5555", i), e.withAssertion(tiDriverEmail, tiRedeemReq(unknown))).Code)
	}
	assert.Equal(t, []int{http.StatusUnauthorized, http.StatusUnauthorized, http.StatusTooManyRequests}, codes)
	// Another email from another fresh IP is not limited by it.
	assert.Equal(t, http.StatusUnauthorized, e.doFrom("192.0.2.4:5555", e.withAssertion(tiDriver2Email, tiRedeemReq(unknown))).Code)

	// One source IP: its bucket trips whatever the email.
	codes = nil
	for _, email := range []string{tiDriver2Email, tiNonDriverEmail, tiNonDriverEmail} {
		codes = append(codes, e.doFrom("198.51.100.7:5555", e.withAssertion(email, tiRedeemReq(unknown))).Code)
	}
	assert.Equal(t, http.StatusTooManyRequests, codes[2], "%v", codes)
	assert.NotEqual(t, http.StatusTooManyRequests, codes[1], "%v", codes)

	audits := tiWebAudits(t, e.st, testIdentityWebSessionRefuseMutation)
	for _, a := range audits {
		assert.NotContains(t, a.AfterSummary, "unknown-code-value")
	}
}

// N3: behind the proxy the connection comes from the proxy, so the audit
// client_ip and the per-IP limit read the client through the hub's trusted
// proxies (X-Forwarded-For); a forwarding header from a peer that is not a
// trusted proxy is ignored.
func TestTIWebIAP_RedeemClientIPThroughTrustedProxy(t *testing.T) {
	e := newTIIAPEnv(t, []string{tiDriverEmail})
	e.srv.config.TrustedProxies = []string{"192.0.2.0/24"}
	clientIPs := func() map[string]bool {
		out := map[string]bool{}
		for _, a := range tiWebAudits(t, e.st, testIdentityWebSessionRefuseMutation) {
			var row struct {
				ClientIP string `json:"client_ip"`
			}
			require.NoError(t, json.Unmarshal([]byte(a.AfterSummary), &row))
			out[row.ClientIP] = true
		}
		return out
	}

	trusted := e.withAssertion("forwarded-1@example.com", tiRedeemReq("scion_tiwc_x"))
	trusted.headers["X-Forwarded-For"] = "203.0.113.7, 192.0.2.20"
	assert.Equal(t, http.StatusForbidden, e.doFrom("192.0.2.10:5555", trusted).Code)
	assert.Equal(t, map[string]bool{"203.0.113.7": true}, clientIPs(), "trusted peer: the first untrusted hop")

	untrusted := e.withAssertion("forwarded-2@example.com", tiRedeemReq("scion_tiwc_x"))
	untrusted.headers["X-Forwarded-For"] = "203.0.113.8"
	assert.Equal(t, http.StatusForbidden, e.doFrom("198.51.100.9:5555", untrusted).Code)
	assert.Equal(t, map[string]bool{"203.0.113.7": true, "198.51.100.9": true}, clientIPs(), "untrusted peer: the header is ignored")

	// The per-IP bucket is the forwarded client's, not the proxy's.
	e.ws.testIdentityRedeemLimiter = newTestLoginLimiter()
	e.ws.testIdentityRedeemLimiter.burst = 1
	e.ws.testIdentityRedeemLimiter.perSecond = 0.0001
	send := func(client, email string) int {
		r := e.withAssertion(email, tiRedeemReq("scion_tiwc_x"))
		r.headers["X-Forwarded-For"] = client
		return e.doFrom("192.0.2.10:5555", r).Code
	}
	assert.Equal(t, http.StatusForbidden, send("203.0.113.1", "limit-1@example.com"))
	assert.Equal(t, http.StatusTooManyRequests, send("203.0.113.1", "limit-2@example.com"), "same client again")
	assert.Equal(t, http.StatusForbidden, send("203.0.113.2", "limit-3@example.com"), "another client through the same proxy")

	// N7: IPv6 clients are grouped by /64, as off the proxy.
	assert.Equal(t, http.StatusForbidden, send("2001:db8:1:2::10", "limit-4@example.com"))
	assert.Equal(t, http.StatusTooManyRequests, send("2001:db8:1:2::99", "limit-5@example.com"), "same /64")
	assert.Equal(t, http.StatusForbidden, send("2001:db8:1:3::10", "limit-6@example.com"), "another /64")
}

// With no trusted proxies (the deployed default: no setting configures
// them yet), the redeem client IP is the connection's address and
// X-Forwarded-For is ignored, even from a peer that would otherwise be a
// proxy.
func TestTIWebIAP_RedeemClientIPWithoutTrustedProxies(t *testing.T) {
	e := newTIIAPEnv(t, []string{tiDriverEmail})
	require.Empty(t, e.srv.config.TrustedProxies)
	r := e.withAssertion("forwarded@example.com", tiRedeemReq("scion_tiwc_x"))
	r.headers["X-Forwarded-For"] = "203.0.113.7"
	r.headers["X-Real-IP"] = "203.0.113.8"
	assert.Equal(t, http.StatusForbidden, e.doFrom("192.0.2.10:5555", r).Code)
	a := tiAuditFor(t, e.st, "driver_principal_required")
	assert.Contains(t, a.AfterSummary, `"client_ip":"192.0.2.10"`)
	assert.NotContains(t, a.AfterSummary, "203.0.113.")
}

// R1 (C3 everywhere): a driver principal is a transport identity on every
// path that refuses the platform service account: it cannot sign in through
// the login flows, an ordinary session or hub-issued credential it already
// held stops working, and admins cannot create a user for it.
func TestTIWebIAP_DriverIsNeverAUserOnOtherPaths(t *testing.T) {
	const userDriver = "browser-driver@example.com"
	e := newTIIAPEnv(t, []string{tiDriverEmail, userDriver})
	ctx := context.Background()

	t.Run("CLI/OAuth login", func(t *testing.T) {
		for _, email := range []string{userDriver, tiDriverEmail} {
			rec := httptest.NewRecorder()
			e.srv.completeOAuthLogin(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/cli/token", nil),
				&OAuthUserInfo{Email: email, DisplayName: "Driver", Provider: "google"})
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", email, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "accessToken", email)
			tiNoUserRow(t, e.st, email)
		}
		rec := httptest.NewRecorder()
		e.srv.completeOAuthLogin(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/cli/token", nil),
			&OAuthUserInfo{Email: "someone-else@example.com", DisplayName: "Someone", Provider: "google"})
		assert.Equal(t, http.StatusOK, rec.Code, "another user still signs in: %s", rec.Body.String())
	})

	// A row and credentials from before the email was listed.
	pre := &store.User{ID: generateID(), Email: userDriver, Role: "member", Status: "active", Created: time.Now()}
	require.NoError(t, e.st.CreateUser(ctx, pre))

	t.Run("existing ordinary session", func(t *testing.T) {
		jar := loginSession(t, e.ws, pre.ID, pre.Email, pre.Role)
		rec := e.do(tiReq{path: "/projects", cookies: jar, browser: true})
		assert.NotEqual(t, http.StatusOK, rec.Code, "page: %s", rec.Body.String())
		assert.True(t, tiWebCleared(rec, webSessionName), "proxyAuth clears the session")
		rec = e.do(tiReq{path: "/api/v1/auth/me", cookies: jar})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.True(t, tiWebCleared(rec, webSessionName), "sessionToBearer clears the session")
		assert.NotContains(t, rec.Body.String(), userDriver)
	})

	t.Run("hub-issued tokens", func(t *testing.T) {
		access, refresh, _, err := e.srv.userTokenService.GenerateTokenPair(pre.ID, pre.Email, pre.DisplayName, pre.Role, ClientTypeWeb)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "access token: %s", rec.Body.String())
		rec = doRequestNoAuth(t, e.srv, http.MethodPost, "/api/v1/auth/refresh", AuthRefreshRequest{RefreshToken: refresh})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "refresh: %s", rec.Body.String())

		// Control: an ordinary user's tokens work in the same env, so the
		// 401s above can only come from the transport predicate.
		ord := &store.User{ID: generateID(), Email: "ordinary-token-user@example.com", Role: "member", Status: "active", Created: time.Now()}
		require.NoError(t, e.st.CreateUser(ctx, ord))
		oAccess, oRefresh, _, err := e.srv.userTokenService.GenerateTokenPair(ord.ID, ord.Email, ord.DisplayName, ord.Role, ClientTypeWeb)
		require.NoError(t, err)
		req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req.Header.Set("Authorization", "Bearer "+oAccess)
		rec = httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "ordinary access token: %s", rec.Body.String())
		rec = doRequestNoAuth(t, e.srv, http.MethodPost, "/api/v1/auth/refresh", AuthRefreshRequest{RefreshToken: oRefresh})
		assert.Equal(t, http.StatusOK, rec.Code, "ordinary refresh: %s", rec.Body.String())
	})

	t.Run("PAT", func(t *testing.T) {
		// mintPAT gives u a PAT on its own project, as the user would mint it.
		mintPAT := func(u *store.User, name string) string {
			ensureHubMembership(ctx, e.st, u.ID)
			projectID := generateID()
			require.NoError(t, e.st.CreateProject(ctx, &store.Project{ID: projectID, Name: name, Slug: name + "-" + projectID[:8], CreatedBy: u.ID}))
			ownerRD, err := e.st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
			require.NoError(t, err)
			_, err = e.st.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: ownerRD.ID, PrincipalType: store.RoleBindingPrincipalUser,
				PrincipalID: u.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test"})
			require.NoError(t, err)
			mintCtx := contextWithCredentialContext(
				contextWithIdentity(context.Background(), NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, "member", string(ClientTypeAPI))),
				CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"})
			key, _, err := e.srv.uatService.CreateToken(mintCtx, u.ID, name, projectID, []string{"agent:read"}, nil)
			require.NoError(t, err)
			return key
		}
		me := func(key string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			req.Header.Set("Authorization", "Bearer "+key)
			rec := httptest.NewRecorder()
			e.srv.Handler().ServeHTTP(rec, req)
			return rec
		}
		rec := me(mintPAT(pre, "ws2-driver-pat")) // minted before the email was listed
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "driver PAT: %s", rec.Body.String())

		// Control: an ordinary user's PAT, minted the same way, works.
		ordPAT := &store.User{ID: generateID(), Email: "ordinary-pat-user@example.com", Role: "member", Status: "active", Created: time.Now()}
		require.NoError(t, e.st.CreateUser(ctx, ordPAT))
		rec = me(mintPAT(ordPAT, "ws2-ordinary-pat"))
		assert.NotEqual(t, http.StatusUnauthorized, rec.Code, "ordinary PAT is authenticated: %s", rec.Body.String())
	})

	t.Run("admin-initiated creation", func(t *testing.T) {
		const another = "second-browser-driver@example.com"
		e2 := newTIIAPEnv(t, []string{another})
		rec := doRequest(t, e2.srv, http.MethodPost, "/api/v1/admin/allow-list", AllowListAddRequest{Email: another})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), proxyDriverPrincipalUserMessage)
		assert.NotContains(t, rec.Body.String(), "driver", "the answer does not reveal the driver list")
		rec = doRequest(t, e2.srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{Email: another})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), proxyDriverPrincipalUserMessage)
		assert.NotContains(t, rec.Body.String(), "driver", "the answer does not reveal the driver list")
		tiNoUserRow(t, e2.st, another)

		// Bulk invite and allow-list import: the ordinary email is
		// invited, the driver is skipped.
		rec = doRequest(t, e2.srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{{Email: another}, {Email: "bulk-ordinary@example.com"}}})
		assert.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "bulk invite: %d %s", rec.Code, rec.Body.String())
		_, err := e2.st.GetUserByEmail(ctx, "bulk-ordinary@example.com")
		assert.NoError(t, err, "bulk invite: the ordinary email is invited")
		tiNoUserRow(t, e2.st, another)
		rec = doRequest(t, e2.srv, http.MethodPost, "/api/v1/admin/allow-list/import", AllowListBulkAddRequest{Emails: []AllowListAddRequest{{Email: another}, {Email: "import-ordinary@example.com"}}})
		assert.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "import: %d %s", rec.Code, rec.Body.String())
		_, err = e2.st.GetUserByEmail(ctx, "import-ordinary@example.com")
		assert.NoError(t, err, "import: the ordinary email is added")
		tiNoUserRow(t, e2.st, another)
	})

	t.Run("user provisioning", func(t *testing.T) {
		const another = "provisioned-browser-driver@example.com"
		f := newProvisionFixture(t) // provisioning needs a hub without dev auth
		f.srv.proxyDrivers = newDriverPrincipalSet([]string{another})
		logs := tiWebCaptureLogs(t)
		rec := provisionAs(t, f.srv, f.superAdmin, map[string]interface{}{"email": another})
		// The refusal is attributable in the log: one line with the
		// endpoint, the actor, the email and the specific reason.
		var refusal string
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "user creation refused") {
				assert.Empty(t, refusal, "exactly one refusal line")
				refusal = line
			}
		}
		assert.Contains(t, refusal, `endpoint="user provisioning"`)
		assert.Contains(t, refusal, "actor="+f.superAdmin.ID)
		assert.Contains(t, refusal, "email="+another)
		assert.Contains(t, refusal, "reason="+proxyDriverPrincipalLogReason)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, provisionReasonEmailNotAllowed, resp.Error.Details["reason"], "a generic reason")
		assert.Equal(t, "email", resp.Error.Details["field"])
		assert.Equal(t, proxyDriverPrincipalUserMessage, resp.Error.Message)
		assert.NotContains(t, rec.Body.String(), "driver", "the answer does not reveal the driver list")
		tiNoUserRow(t, f.s, another)
		rec = provisionAs(t, f.srv, f.superAdmin, map[string]interface{}{"email": "provisioned-ordinary@example.com"})
		assert.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "ordinary email: %d %s", rec.Code, rec.Body.String())
	})

	t.Run("test-login", func(t *testing.T) {
		e3 := newTIIAPEnv(t, []string{userDriver}, func(c *WebServerConfig) { c.EnableTestLogin = true })
		login := func(email string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", strings.NewReader(`{"email":"`+email+`","role":"member"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", testLoginAuthHeader(t, e3.srv.userTokenService))
			rec := httptest.NewRecorder()
			e3.ws.Handler().ServeHTTP(rec, req)
			return rec
		}
		rec := login(userDriver)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		tiNoUserRow(t, e3.st, userDriver)
		rec = login("test-login-ordinary@example.com")
		assert.Equal(t, http.StatusOK, rec.Code, "ordinary test-login: %s", rec.Body.String())
	})
}

// --- A4: the driver binding, per consumer ---------------------------------------

// Every session consumer honours a test identity web session behind the
// proxy only with a valid assertion for the session's driver; a different
// driver, no assertion, or a session without a driver is cleared with 401.
func TestTIWebIAP_DriverBindingPerConsumer(t *testing.T) {
	type consumer struct {
		name string
		send func(e *tiIAPEnv, email string, jar []*http.Cookie) *httptest.ResponseRecorder
	}
	consumers := []consumer{
		{"sessionToBearer", func(e *tiIAPEnv, email string, jar []*http.Cookie) *httptest.ResponseRecorder {
			return e.apiMeAs(email, jar)
		}},
		{"proxyAuth", func(e *tiIAPEnv, email string, jar []*http.Cookie) *httptest.ResponseRecorder {
			return e.pageAs(email, jar)
		}},
		{"auth/me", func(e *tiIAPEnv, email string, jar []*http.Cookie) *httptest.ResponseRecorder {
			return e.webMeAs(email, jar)
		}},
		{"sessionAuth", func(e *tiIAPEnv, email string, jar []*http.Cookie) *httptest.ResponseRecorder {
			return tiServeMiddleware(e, e.ws.sessionAuthMiddleware, email, jar)
		}},
	}
	type sessionCase struct {
		name      string
		jar       func(e *tiIAPEnv, fx TestIdentityTokenResponse) []*http.Cookie
		email     string
		honoured  bool
		assertion string
	}
	login := func(e *tiIAPEnv, fx TestIdentityTokenResponse) []*http.Cookie {
		return e.loginAs(tiDriverEmail, fx.Identity.ID)
	}
	cases := []sessionCase{
		{name: "matching driver", jar: login, email: tiDriverEmail, honoured: true},
		{name: "another configured driver", jar: login, email: tiDriver2Email},
		{name: "a non-driver principal", jar: login, email: tiNonDriverEmail},
		{name: "no assertion", jar: login, email: ""},
		{name: "assertion from an untrusted key", jar: login, assertion: "other-key"},
		{name: "session without a driver", jar: func(e *tiIAPEnv, fx TestIdentityTokenResponse) []*http.Cookie {
			return e.craftDriverSession(e.row(fx.Identity.ID), "")
		}, email: tiDriverEmail},
		{name: "session for an unconfigured driver", jar: func(e *tiIAPEnv, fx TestIdentityTokenResponse) []*http.Cookie {
			return e.craftDriverSession(e.row(fx.Identity.ID), tiNonDriverEmail)
		}, email: tiNonDriverEmail},
	}
	for _, c := range consumers {
		for _, sc := range cases {
			t.Run(c.name+"/"+sc.name, func(t *testing.T) {
				e := newTIIAPEnv(t, []string{tiDriverEmail, tiDriver2Email})
				fx := e.issue()
				jar := sc.jar(e, fx)
				var rec *httptest.ResponseRecorder
				if sc.assertion == "other-key" {
					rec = tiServeWithHeader(e, c.name, jar, e.iap.otherKeyAssertion(t, tiDriverEmail))
				} else {
					rec = c.send(e, sc.email, jar)
				}
				if sc.honoured {
					assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					if c.name != "proxyAuth" {
						assert.Contains(t, rec.Body.String(), fx.Identity.Email)
					}
					assert.False(t, tiWebCleared(rec, webSessionName))
					return
				}
				// The session is cleared with 401. On /api/v1/ the request
				// then goes on without it and gets what its assertion alone
				// gets: 401 with none or an invalid one, 403 for a driver
				// (it has no session of its own), and another principal is
				// served as itself. Never as the test identity.
				want := http.StatusUnauthorized
				if c.name == "sessionToBearer" && sc.assertion == "" && sc.email != "" {
					want = http.StatusOK
					if sc.email == tiDriverEmail || sc.email == tiDriver2Email {
						want = http.StatusForbidden
					}
				}
				assert.Equal(t, want, rec.Code, rec.Body.String())
				assert.True(t, tiWebCleared(rec, webSessionName), "scion_sess cleared")
				assert.True(t, tiWebCleared(rec, testIdentitySessionCookie), "scion_ti cleared")
				assert.NotContains(t, rec.Body.String(), fx.Identity.Email)
				tiNoUserRow(t, e.st, tiDriverEmail)
			})
		}
	}
}

// tiServeMiddleware runs one web middleware directly in front of a handler
// that answers with the context user's email.
func tiServeMiddleware(e *tiIAPEnv, mw func(http.Handler) http.Handler, email string, jar []*http.Cookie) *httptest.ResponseRecorder {
	header := ""
	if email != "" {
		header = e.iap.assertion(e.t, email)
	}
	return tiServeMiddlewareHeader(mw, header, jar)
}

func tiServeMiddlewareHeader(mw func(http.Handler) http.Handler, assertion string, jar []*http.Cookie) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := getWebSessionUser(r.Context())
		if u == nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, u.Email)
	})
	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.RemoteAddr = "192.0.2.10:5555"
	if assertion != "" {
		req.Header.Set(IAPAssertionHeader, assertion)
	}
	for _, c := range jar {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)
	return rec
}

// tiServeWithHeader sends a consumer's request with a raw assertion header.
func tiServeWithHeader(e *tiIAPEnv, consumer string, jar []*http.Cookie, assertion string) *httptest.ResponseRecorder {
	h := map[string]string{IAPAssertionHeader: assertion}
	switch consumer {
	case "sessionToBearer":
		return e.do(tiReq{path: "/api/v1/auth/me", cookies: jar, headers: h})
	case "proxyAuth":
		return e.do(tiReq{path: "/projects", cookies: jar, headers: h, browser: true})
	case "auth/me":
		return e.do(tiReq{path: "/auth/me", cookies: jar, headers: h})
	default:
		return tiServeMiddlewareHeader(e.ws.sessionAuthMiddleware, assertion, jar)
	}
}

// Dev auth (when a proxy hub also runs it on loopback) uses the same
// classifier, binding included.
func TestTIWebIAP_DriverBindingDevAuth(t *testing.T) {
	e := newTIIAPEnv(t, []string{tiDriverEmail, tiDriver2Email}, func(c *WebServerConfig) {
		c.Host = "127.0.0.1"
		c.DevAuthToken = "ti-web-dev-token"
	})
	fx := e.issue()
	jar := e.loginAs(tiDriverEmail, fx.Identity.ID)
	rec := tiServeMiddleware(e, e.ws.devAuthMiddleware, tiDriverEmail, jar)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, fx.Identity.Email, rec.Body.String())
	rec = tiServeMiddleware(e, e.ws.devAuthMiddleware, tiDriver2Email, jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.True(t, tiWebCleared(rec, webSessionName))
	assert.NotContains(t, rec.Body.String(), DevUserID)
}

// Logout behind the proxy clears a test identity session whichever
// assertion comes with it.
func TestTIWebIAP_LogoutClears(t *testing.T) {
	for _, email := range []string{tiDriverEmail, tiDriver2Email, ""} {
		e := newTIIAPEnv(t, []string{tiDriverEmail, tiDriver2Email})
		fx := e.issue()
		jar := e.loginAs(tiDriverEmail, fx.Identity.ID)
		rec := e.as(email, tiReq{method: http.MethodPost, path: "/auth/logout", cookies: jar})
		assert.Equal(t, http.StatusOK, rec.Code, "assertion %q", email)
		assert.True(t, tiWebCleared(rec, webSessionName), "assertion %q", email)
		assert.True(t, tiWebCleared(rec, testIdentitySessionCookie), "assertion %q", email)
	}
}

// On a hub without proxy auth the binding is a no-op for driverless
// sessions (the WS1 suite), and a session that names a driver is refused.
func TestTIWebIAP_NonProxyHubRefusesDriverBoundSession(t *testing.T) {
	e := newTIWebEnv(t)
	fx := e.issue()
	jar := e.craftDriverSession(e.row(fx.Identity.ID), tiDriverEmail)
	rec := e.apiMe(jar)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.True(t, tiWebCleared(rec, webSessionName))
	assert.Equal(t, http.StatusOK, e.apiMe(e.login(fx.Identity.ID)).Code, "a driverless session works")
}

// Package-wide: only the classifier reads the test identity session values
// (marker, expiry, driver) to admit a session, and every consumer in web.go
// goes through classifyTestIdentitySession.
func TestTIWebIAP_EveryConsumerUsesTheClassifier(t *testing.T) {
	files, err := os.ReadDir(".")
	require.NoError(t, err)
	allowed := map[string]bool{
		"handlers_test_identity_web.go": true, // the classifier, redeem and the session helpers
		"test_identity_web_driver.go":   true, // the binding, called only by fixtureSessionValid
	}
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		src := string(data)
		for _, key := range []string{"Values[sessKeyTIExpiry]", "Values[sessKeyTIDriver]", "readTestIdentityMarker(r)", "marker.Driver", "testIdentityDriverBinding("} {
			if strings.Contains(src, key) {
				// web.go only checks for a marker's presence before calling
				// the classifier (proxyAuth); it never decides from it.
				if name == "web.go" && key == "readTestIdentityMarker(r)" {
					continue
				}
				t.Errorf("%s uses %s outside the classifier", name, key)
			}
		}
	}
	web, err := os.ReadFile("web.go")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, strings.Count(string(web), "classifyTestIdentitySession("), 7,
		"sessionToBearer, devAuth, proxyAuth, sessionAuth (2), SSE, /auth/me and logout")
	driver, err := os.ReadFile("test_identity_web_driver.go")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(driver), "classifyTestIdentitySession("), "fixtureSessionBearer")
}

// --- A7: the event stream ---------------------------------------------------------

func tiOpenStream(t *testing.T, e *tiIAPEnv, jar []*http.Cookie, assertion string) (*http.Response, <-chan struct{}) {
	t.Helper()
	server := httptest.NewServer(e.ws.Handler())
	t.Cleanup(server.Close)
	req, err := http.NewRequest(http.MethodGet, server.URL+"/events?sub=notification.ti", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	if assertion != "" {
		req.Header.Set(IAPAssertionHeader, assertion)
	}
	for _, c := range jar {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	closed := make(chan struct{})
	if resp.StatusCode == http.StatusOK {
		go func() {
			r := bufio.NewReader(resp.Body)
			for {
				if _, err := r.ReadString('\n'); err != nil {
					close(closed)
					return
				}
			}
		}()
	}
	return resp, closed
}

func tiExpectOpenThenClosed(t *testing.T, closed <-chan struct{}, revoke func(), within time.Duration) {
	t.Helper()
	select {
	case <-closed:
		t.Fatal("stream closed while the session is valid")
	case <-time.After(300 * time.Millisecond):
	}
	if revoke != nil {
		revoke()
	}
	select {
	case <-closed:
	case <-time.After(within):
		t.Fatal("stream still open")
	}
}

func TestTIWebIAP_EventStream(t *testing.T) {
	setup := func(t *testing.T) (*tiIAPEnv, TestIdentityTokenResponse, []*http.Cookie) {
		e := newTIIAPEnv(t, []string{tiDriverEmail, tiDriver2Email})
		e.ws.events = NewChannelEventPublisher()
		e.ws.config.SSEHeartbeatInterval = 100 * time.Millisecond
		fx := e.issue()
		return e, fx, e.loginAs(tiDriverEmail, fx.Identity.ID)
	}
	t.Run("binding fails at connect", func(t *testing.T) {
		e, _, jar := setup(t)
		for _, assertion := range []string{e.iap.assertion(t, tiDriver2Email), "", e.iap.otherKeyAssertion(t, tiDriverEmail)} {
			resp, _ := tiOpenStream(t, e, jar, assertion)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		}
	})
	t.Run("heartbeat after revocation", func(t *testing.T) {
		e, fx, jar := setup(t)
		resp, closed := tiOpenStream(t, e, jar, e.iap.assertion(t, tiDriverEmail))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		tiExpectOpenThenClosed(t, closed, func() {
			require.NoError(t, e.st.IncrementSessionGeneration(context.Background(), fx.Identity.ID))
		}, 2*time.Second)
	})
	t.Run("heartbeat after expiry", func(t *testing.T) {
		e, _, jar := setup(t)
		resp, closed := tiOpenStream(t, e, jar, e.iap.assertion(t, tiDriverEmail))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		tiExpectOpenThenClosed(t, closed, func() { e.clock.Advance(9 * time.Hour) }, 2*time.Second)
	})
	t.Run("heartbeat after the stream's assertion expires", func(t *testing.T) {
		e, _, jar := setup(t)
		// Valid for about two more seconds (exp plus the 30s IAP skew).
		resp, closed := tiOpenStream(t, e, jar, e.iap.assertionExpiring(t, tiDriverEmail, time.Now().Add(-28*time.Second)))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		tiExpectOpenThenClosed(t, closed, nil, 6*time.Second)
	})
}

// --- A5: the fixture session bearer rule ------------------------------------------

type tiBearerEnv struct {
	*tiIAPEnv
	kp    *gcvTestKeyPair
	fx    TestIdentityTokenResponse
	jar   []*http.Cookie
	valid string
}

func newTIBearerEnv(t *testing.T, audience string) *tiBearerEnv {
	t.Helper()
	kp := newGCVTestKeyPair("ti-driver-kid")
	endpoints := newSAJWKSEndpoints(kp)
	t.Cleanup(endpoints.close)
	e := newTIIAPEnv(t, []string{tiDriverEmail, tiDriver2Email}, func(c *WebServerConfig) {
		c.ProxyDriverTokenAudience = audience
	})
	e.ws.SetDriverTokenValidator(newTestValidator(endpoints))
	b := &tiBearerEnv{tiIAPEnv: e, kp: kp}
	b.fx = e.issue()
	b.jar = e.loginAs(tiDriverEmail, b.fx.Identity.ID)
	b.valid = b.googleToken(tiDriverEmail, nil)
	return b
}

// googleToken signs a Google service account ID token for email with the
// driver token audience, after mut.
func (b *tiBearerEnv) googleToken(email string, mut func(map[string]interface{})) string {
	claims := serviceAccountIDTokenClaims(email)
	claims["aud"] = tiDriverTokenAudience
	if mut != nil {
		mut(claims)
	}
	return signIDToken(b.kp, claims)
}

// call sends GET /api/v1/auth/me with the bearer, the assertion and the jar,
// and returns the answer and the Authorization header the hub saw.
func (b *tiBearerEnv) call(assertionEmail, bearer string, jar []*http.Cookie, extra map[string]string) (*httptest.ResponseRecorder, string) {
	b.t.Helper()
	b.takeLastAuth()
	h := map[string]string{"Authorization": "Bearer " + bearer}
	for k, v := range extra {
		h[k] = v
	}
	rec := b.as(assertionEmail, tiReq{path: "/api/v1/auth/me", cookies: jar, headers: h})
	return rec, b.takeLastAuth()
}

func TestTIWebIAP_BearerRuleTruthTable(t *testing.T) {
	logs := tiWebCaptureLogs(t)
	b := newTIBearerEnv(t, tiDriverTokenAudience)

	// All conditions hold: the hub sees the test identity's own token.
	rec, seen := b.call(tiDriverEmail, b.valid, b.jar, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), b.fx.Identity.Email)
	require.NotEqual(t, "Bearer "+b.valid, seen)
	claims, err := b.srv.userTokenService.ValidateUserToken(strings.TrimPrefix(seen, "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, b.fx.Identity.ID, claims.UserID)
	assert.Equal(t, ClientTypeWeb, claims.ClientType)
	assert.Contains(t, logs.String(), fixtureSessionOverExternalBearerReason)
	assert.NotContains(t, logs.String(), b.valid, "the driver token is never logged")

	otherKP := newGCVTestKeyPair("ti-driver-kid")
	// cleared: the session no longer passes the classifier on that request
	// (binding), so it is also cleared (design rule 3), while the bearer
	// still goes on unchanged.
	unchanged := map[string]struct {
		assertion string
		bearer    string
		jar       []*http.Cookie
		extra     map[string]string
		cleared   bool
	}{
		"no test identity session":           {tiDriverEmail, b.valid, nil, nil, false},
		"assertion of another driver":        {tiDriver2Email, b.googleToken(tiDriver2Email, nil), b.jar, nil, true},
		"no assertion":                       {"", b.valid, b.jar, nil, true},
		"token email is not the assertion's": {tiDriverEmail, b.googleToken(tiDriver2Email, nil), b.jar, nil, false},
		"token email is not a driver":        {tiDriverEmail, b.googleToken("worker@ti-test.iam.gserviceaccount.com", nil), b.jar, nil, false},
		"audience mismatch": {tiDriverEmail, b.googleToken(tiDriverEmail, func(c map[string]interface{}) {
			c["aud"] = "another-client.apps.googleusercontent.com"
		}), b.jar, nil, false},
		"audience list with another entry": {tiDriverEmail, b.googleToken(tiDriverEmail, func(c map[string]interface{}) {
			c["aud"] = []string{tiDriverTokenAudience, "another-client.apps.googleusercontent.com"}
		}), b.jar, nil, false},
		"bad signature": {tiDriverEmail, signIDToken(otherKP, func() map[string]interface{} {
			c := serviceAccountIDTokenClaims(tiDriverEmail)
			c["aud"] = tiDriverTokenAudience
			return c
		}()), b.jar, nil, false},
		"expired token": {tiDriverEmail, b.googleToken(tiDriverEmail, func(c map[string]interface{}) {
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
		}), b.jar, nil, false},
		"unverified email": {tiDriverEmail, b.googleToken(tiDriverEmail, func(c map[string]interface{}) {
			c["email_verified"] = false
		}), b.jar, nil, false},
		"not a JWT":                     {tiDriverEmail, "opaque-driver-token", b.jar, nil, false},
		"another credential header too": {tiDriverEmail, b.valid, b.jar, map[string]string{"X-Scion-Agent-Token": "x"}, false},
	}
	for name, c := range unchanged {
		rec, seen := b.call(c.assertion, c.bearer, c.jar, c.extra)
		assert.Equal(t, "Bearer "+c.bearer, seen, "%s: the bearer reaches the hub unchanged", name)
		assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden, "%s: %d %s", name, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), b.fx.Identity.Email, name)
		assert.Equal(t, c.cleared, tiWebCleared(rec, webSessionName), "%s: scion_sess cleared", name)
		assert.Equal(t, c.cleared, tiWebCleared(rec, testIdentitySessionCookie), "%s: scion_ti cleared", name)
	}

	// A hub-issued bearer is always unchanged, even with a valid test
	// identity session and assertion.
	issuer := b.row(b.issuerID)
	humanTok, _, err := b.srv.userTokenService.GenerateAccessToken(issuer.ID, issuer.Email, issuer.DisplayName, issuer.Role, ClientTypeWeb)
	require.NoError(t, err)
	rec, seen = b.call(tiDriverEmail, humanTok, b.jar, nil)
	assert.Equal(t, "Bearer "+humanTok, seen)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), issuer.Email)
	assert.NotContains(t, rec.Body.String(), b.fx.Identity.Email)
	for name, tok := range map[string]string{
		"fixture API token": b.fx.AccessToken,
		"PAT":               "scion_pat_not-a-real-token",
		"dev token":         testDevToken,
	} {
		_, seen = b.call(tiDriverEmail, tok, b.jar, nil)
		assert.Equal(t, "Bearer "+tok, seen, name)
	}

	// The jar was never updated from the answers above, so it still holds
	// the session cookie, which still works with its own driver.
	rec, _ = b.call(tiDriverEmail, b.valid, b.jar, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	// A session that no longer passes its checks (generation bumped) never
	// takes the place of the bearer, even with the right driver, assertion
	// and token.
	require.NoError(t, b.st.IncrementSessionGeneration(context.Background(), b.fx.Identity.ID))
	rec, seen = b.call(tiDriverEmail, b.valid, b.jar, nil)
	assert.Equal(t, "Bearer "+b.valid, seen, "revoked session: unchanged")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.True(t, tiWebCleared(rec, webSessionName), "revoked session: cleared")
	assert.True(t, tiWebCleared(rec, testIdentitySessionCookie), "revoked session: scion_ti cleared")
}

// With no driver token audience configured the rule never applies, and on
// a hub without proxy auth it never applies either.
func TestTIWebIAP_BearerRuleNeedsAudienceAndProxyMode(t *testing.T) {
	b := newTIBearerEnv(t, "")
	rec, seen := b.call(tiDriverEmail, b.valid, b.jar, nil)
	assert.Equal(t, "Bearer "+b.valid, seen)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	kp := newGCVTestKeyPair("ti-driver-kid")
	endpoints := newSAJWKSEndpoints(kp)
	t.Cleanup(endpoints.close)
	e := newTIWebEnv(t, func(c *WebServerConfig) { c.ProxyDriverTokenAudience = tiDriverTokenAudience })
	e.ws.SetDriverTokenValidator(newTestValidator(endpoints))
	iap := newTIIAP(t)
	fx := e.issue()
	jar := e.login(fx.Identity.ID)
	claims := serviceAccountIDTokenClaims(tiDriverEmail)
	claims["aud"] = tiDriverTokenAudience
	tok := signIDToken(kp, claims)
	e.takeLastAuth()
	rec = e.do(tiReq{path: "/api/v1/auth/me", cookies: jar, headers: map[string]string{
		"Authorization": "Bearer " + tok, IAPAssertionHeader: iap.assertion(t, tiDriverEmail),
	}})
	assert.Equal(t, "Bearer "+tok, e.takeLastAuth())
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// Downgrade only: the token the rule puts in place reaches exactly the
// decisions of the test identity's issued API token over the whole
// permission registry (the WS1 C4 comparison).
func TestTIWebIAP_BearerRuleNeverExceedsTheTestIdentity(t *testing.T) {
	b := newTIBearerEnv(t, tiDriverTokenAudience)
	projectID := tiCreateProject(t, b.srv, b.fx.AccessToken, "ti-web-iap-scope")
	rec, seen := b.call(tiDriverEmail, b.valid, b.jar, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	replaced := strings.TrimPrefix(seen, "Bearer ")
	require.NotEqual(t, b.valid, replaced)

	ctxFor := func(tok string) context.Context {
		var got context.Context
		h := UnifiedAuthMiddleware(b.srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Context() }))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(httptest.NewRecorder(), req)
		require.NotNil(t, got, "token must authenticate")
		return got
	}
	sessCtx, apiCtx := ctxFor(replaced), ctxFor(b.fx.AccessToken)
	sessID := GetIdentityFromContext(sessCtx).(UserIdentity)
	apiID := GetIdentityFromContext(apiCtx).(UserIdentity)
	assert.Equal(t, apiID.ID(), sessID.ID())
	assert.Equal(t, GetCredentialContextFromContext(apiCtx), GetCredentialContextFromContext(sessCtx))
	n := 0
	for _, p := range permissions.Registry {
		for _, res := range []Resource{{Type: p.Resource}, {Type: p.Resource, ID: projectID}, {Type: "project", ID: projectID}} {
			req := func(id UserIdentity) AuthzRequest {
				return AuthzRequest{
					Principal:      principalContextForIdentity(id),
					Credential:     credentialContextForIdentity(id),
					Resource:       res,
					Action:         Action(p.Action),
					Permission:     p.ID,
					TargetEvidence: hubCollectionEvidence(p.ID),
				}
			}
			assert.Equal(t, b.srv.authzService.Decide(apiCtx, req(apiID)), b.srv.authzService.Decide(sessCtx, req(sessID)), "%s on %+v", p.ID, res)
			n++
		}
	}
	assert.Greater(t, n, 100, "the whole registry is covered")
}
