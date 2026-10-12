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
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Store-free tests of the WS2 driver principal rules (ptone/scion#4240).
// They run in the no_sqlite build too; the routed tests against a real
// store are in handlers_test_identity_web_iap_test.go.

const (
	tiDriverIAPAudience = "/projects/1/global/backendServices/2"
	tiDriverEmail       = "ui-driver@ti-test.iam.gserviceaccount.com"
	tiDriver2Email      = "second-driver@ti-test.iam.gserviceaccount.com"
	tiNonDriverEmail    = "someone@example.com"
)

// tiIAP is a real IAPAuthenticator verifying assertions signed with a local
// key set (no network): the "verified assertion" fixture of the WS2 tests.
type tiIAP struct {
	kp   *testKeyPair
	auth *IAPAuthenticator
}

func newTIIAP(t *testing.T) *tiIAP {
	t.Helper()
	kp := newTestKeyPair(t, "ti-iap-key")
	jwks := startJWKSServer(t, kp.jwksJSON(t))
	return &tiIAP{kp: kp, auth: &IAPAuthenticator{Audience: tiDriverIAPAudience, JWKSURL: jwks.URL}}
}

// assertion returns a valid IAP assertion for email.
func (i *tiIAP) assertion(t *testing.T, email string) string {
	t.Helper()
	now := time.Now()
	return i.assertionExpiring(t, email, now.Add(5*time.Minute))
}

// assertionExpiring returns an IAP assertion for email with the given exp.
func (i *tiIAP) assertionExpiring(t *testing.T, email string, exp time.Time) string {
	t.Helper()
	return i.kp.signJWT(t, makeTestClaims("accounts.google.com:sub-"+email, "accounts.google.com:"+email,
		DefaultIAPIssuer, tiDriverIAPAudience, time.Now().Add(-time.Minute), exp))
}

// otherKeyAssertion returns an assertion for email signed with a key the
// authenticator does not trust.
func (i *tiIAP) otherKeyAssertion(t *testing.T, email string) string {
	t.Helper()
	other := newTestKeyPair(t, "ti-iap-key")
	now := time.Now()
	return other.signJWT(t, makeTestClaims("accounts.google.com:sub-"+email, "accounts.google.com:"+email,
		DefaultIAPIssuer, tiDriverIAPAudience, now.Add(-time.Minute), now.Add(5*time.Minute)))
}

func TestDriverPrincipalSet(t *testing.T) {
	var empty driverPrincipalSet
	assert.True(t, empty.empty())
	assert.False(t, empty.contains(tiDriverEmail))
	assert.Nil(t, newDriverPrincipalSet(nil))

	s := newDriverPrincipalSet([]string{" UI-Driver@TI-Test.iam.gserviceaccount.com ", ""})
	assert.False(t, s.empty())
	assert.True(t, s.contains(tiDriverEmail))
	assert.True(t, s.contains("UI-DRIVER@ti-test.iam.gserviceaccount.com"), "case-insensitive")
	assert.False(t, s.contains(""))
	assert.False(t, s.contains("driver@ti-test.iam.gserviceaccount.com"), "exact match, no suffix match")
	assert.False(t, s.contains("ui-driver@ti-test.iam.gserviceaccount.com.example"), "exact match, no prefix match")
	assert.Len(t, s, 1, "empty entries are skipped")
}

// WS2 A2 at API Step 3a (store-free): a verified assertion for a driver
// principal is refused with 403 driver_principal_has_no_session before the
// provisioner runs, so it never becomes a user. Any other verified
// assertion still provisions as before.
func TestDriverPrincipal_RefusedAtStep3a(t *testing.T) {
	iap := newTIIAP(t)
	var provisioned []string
	cfg := AuthConfig{
		ProxyAuthenticator: iap.auth,
		ProxyUserProvisioner: func(_ context.Context, info *ProxyUserInfo) (UserIdentity, error) {
			provisioned = append(provisioned, info.Email)
			return NewAuthenticatedUser("u-"+info.Email, info.Email, "", "member", "web"), nil
		},
		DriverPrincipals: newDriverPrincipalSet([]string{tiDriverEmail}),
		Logger:           slog.Default(),
	}
	do := func(assertion string) (*httptest.ResponseRecorder, *probeResult) {
		result := &probeResult{}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		req.Header.Set(IAPAssertionHeader, assertion)
		rec := httptest.NewRecorder()
		UnifiedAuthMiddleware(cfg)(probeHandler(result)).ServeHTTP(rec, req)
		return rec, result
	}

	rec, result := do(iap.assertion(t, tiDriverEmail))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), driverPrincipalNoSessionReason)
	assert.False(t, result.reached, "never served as the driver")
	assert.Empty(t, provisioned, "the driver is never provisioned")

	rec, result = do(iap.assertion(t, tiNonDriverEmail))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, result.reached)
	assert.Equal(t, []string{tiNonDriverEmail}, provisioned)

	// Without a driver list nothing changes for the same principal.
	cfg.DriverPrincipals = nil
	rec, _ = do(iap.assertion(t, tiDriverEmail))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// C3: a driver principal's Google ID token on the external-bearer path is
// refused by the resolver before it can find or create an account for it.
func TestDriverPrincipal_RefusedOnExternalBearer(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()
	userStore := newFakeUserStore()
	resolver := NewGoogleIdentityResolver(userStore, newMemExtIDStore(), alwaysAuthorized, nil, slog.Default())
	resolver.SetDriverPrincipals(newDriverPrincipalSet([]string{tiDriverEmail}))
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"ti-test"})

	w, result := doExternalBearerRequest(cfg, signIDToken(kp, serviceAccountIDTokenClaims(tiDriverEmail)))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.False(t, result.reached)
	_, err := userStore.GetUserByEmail(context.Background(), tiDriverEmail)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no account for the driver: %v", err)

	// Another service account of the same allowed project still works.
	w, result = doExternalBearerRequest(cfg, signIDToken(kp, serviceAccountIDTokenClaims("worker@ti-test.iam.gserviceaccount.com")))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, result.reached)
}

// C3 for both Google paths: GoogleIdentityResolver.Resolve, which the
// external-bearer path and the GE exchange share, never resolves or
// provisions a driver principal, user email or service account alike.
func TestDriverPrincipal_RefusedByGoogleIdentityResolver(t *testing.T) {
	userEmailDriver := "browser-driver@example.com"
	userStore := newFakeUserStore()
	resolver := NewGoogleIdentityResolver(userStore, newMemExtIDStore(), alwaysAuthorized, nil, slog.Default())
	resolver.SetDriverPrincipals(newDriverPrincipalSet([]string{tiDriverEmail, userEmailDriver}))
	identity := func(email string, sa bool) *ValidatedGoogleIdentity {
		return &ValidatedGoogleIdentity{Subject: "sub-" + email, Email: email, EmailVerified: true,
			Issuer: googleIssuerHTTPS, IsServiceAccount: sa, UpstreamExpiry: time.Now().Add(time.Hour)}
	}
	for _, c := range []struct {
		email string
		sa    bool
	}{{tiDriverEmail, true}, {userEmailDriver, false}, {"Browser-Driver@Example.com", false}} {
		u, err := resolver.Resolve(context.Background(), identity(c.email, c.sa), ResolvePolicy{PreAuthorized: true})
		assert.Nil(t, u, c.email)
		assert.ErrorIs(t, err, ErrAccessDenied, c.email)
	}
	assert.Empty(t, userStore.users, "no account is created for a driver")

	u, err := resolver.Resolve(context.Background(), identity("someone@example.com", false), ResolvePolicy{PreAuthorized: true})
	require.NoError(t, err, "another identity still resolves")
	assert.Equal(t, "someone@example.com", u.Email)
}

// The redeem rate-limit key of an email never collides with a source IP key.
func TestTestIdentityRedeemEmailRateKey(t *testing.T) {
	assert.Equal(t, "proxy-email:"+tiDriverEmail, testIdentityRedeemEmailRateKey("UI-Driver@ti-test.iam.gserviceaccount.com"))
	assert.NotEqual(t, testLoginRateKey("192.0.2.10:5555"), testIdentityRedeemEmailRateKey("192.0.2.10"))
}

// The server-config GET hides both WS2 keys without changing the value it
// was given.
func TestHideNeverAPIWritableProxyKeys(t *testing.T) {
	proxy := &config.V1ProxyConfig{Provider: "iap", DriverPrincipals: []string{tiDriverEmail}, DriverTokenAudience: "aud"}
	srv := &config.V1ServerConfig{Auth: &config.V1AuthConfig{Mode: "proxy", Proxy: proxy}}
	hideNeverAPIWritableProxyKeys(srv)
	b, err := json.Marshal(srv)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "driver_principals")
	assert.NotContains(t, string(b), "driver_token_audience")
	assert.NotContains(t, string(b), tiDriverEmail)
	assert.Contains(t, string(b), `"provider":"iap"`)
	assert.Equal(t, []string{tiDriverEmail}, proxy.DriverPrincipals, "the original value is unchanged")
	assert.Equal(t, "aud", proxy.DriverTokenAudience)

	hideNeverAPIWritableProxyKeys(nil)
	hideNeverAPIWritableProxyKeys(&config.V1ServerConfig{})
	hideNeverAPIWritableProxyKeys(&config.V1ServerConfig{Auth: &config.V1AuthConfig{}})
}

// The settings write paths refuse both keys in every spelling the body
// matcher folds.
func TestNeverAPIWritable_ProxyDriverKeysMatched(t *testing.T) {
	for body, want := range map[string][]string{
		`{"server":{"auth":{"proxy":{"driver_principals":["a@example.com"]}}}}`:                     {config.ProxyDriverPrincipalsKey},
		`{"server.auth.proxy.driver_principals":[]}`:                                                {config.ProxyDriverPrincipalsKey},
		`{"Server":{"AUTH":{"Proxy":{"Driver_Token_Audience":"x"}}}}`:                               {config.ProxyDriverTokenAudienceKey},
		`{"server.auth":{"proxy":{"driver_token_audience":"x","driver_principals":null}}}`:          {config.ProxyDriverPrincipalsKey, config.ProxyDriverTokenAudienceKey},
		`{"server":{"auth":{"proxy":{"provider":"iap","require_trusted_proxy_ip":true}}}}`:          nil,
		`{"server":{"auth":{"proxy":{"iap":{"audience":"/projects/1/global/backendServices/2"}}}}}`: nil,
	} {
		got := (&Server{}).gateProtectedSettingsKeys([]byte(body))
		if want == nil {
			assert.Empty(t, got, body)
			continue
		}
		assert.Equal(t, want, got, body)
	}
}

// tiCall is one call site found by tiCensus: the enclosing function and the
// position of the call.
type tiCall struct {
	fn  string
	pos token.Pos
}

// tiCensus parses every non-test .go file of the package and returns, per
// selector name, the call sites x.<name>(...) with the selector expression
// x rendered as source-like text ("cfg.ProxyAuthenticator").
func tiCensus(t *testing.T) (map[string]map[string][]tiCall, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	out := map[string]map[string][]tiCall{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				recv := tiExprText(sel.X)
				if out[sel.Sel.Name] == nil {
					out[sel.Sel.Name] = map[string][]tiCall{}
				}
				out[sel.Sel.Name][recv] = append(out[sel.Sel.Name][recv], tiCall{fn: fd.Name.Name, pos: call.Pos()})
				return true
			})
		}
	}
	return out, fset
}

func tiExprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return tiExprText(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return tiExprText(v.Fun) + "()"
	default:
		return "?"
	}
}

func tiFuncs(calls map[string][]tiCall, recvSuffix string) []string {
	set := map[string]bool{}
	for recv, cs := range calls {
		if strings.HasSuffix(recv, recvSuffix) {
			for _, c := range cs {
				set[c.fn] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func tiFirst(calls map[string][]tiCall, recvSuffix, fn string) token.Pos {
	var first token.Pos
	for recv, cs := range calls {
		if !strings.HasSuffix(recv, recvSuffix) {
			continue
		}
		for _, c := range cs {
			if c.fn == fn && (first == token.NoPos || c.pos < first) {
				first = c.pos
			}
		}
	}
	return first
}

// Advisor recommendation (C3 by construction): the driver refusal sites are
// the only places in the package where a verified proxy assertion or an
// external Google identity becomes a user, and each refusal comes before
// the user lookup or provisioning in its function. A new ingress path makes
// this test fail until it is given the same refusal and listed here.
func TestTIWebIAP_DriverRefusalSitesAreTheOnlyIngress(t *testing.T) {
	census, fset := tiCensus(t)

	// Proxy assertions: verified only at API Step 3a, on the web
	// fresh-identity path, and by the driver binding / redeem helper,
	// which never creates a user.
	assert.Equal(t, []string{"UnifiedAuthMiddleware", "proxyAuthMiddleware", "verifiedProxyAssertion"},
		tiFuncs(census["Authenticate"], "ProxyAuthenticator"), "every ProxyAuthenticator.Authenticate caller")
	assert.Equal(t, []string{"handleTestIdentityRedeem", "testIdentityDriverBinding"},
		tiFuncs(census["verifiedProxyAssertion"], "ws"), "verifiedProxyAssertion feeds only the binding and redeem")
	// Step 3a: the driver refusal comes after the assertion and before the
	// provisioner, which nothing else calls.
	assert.Equal(t, []string{"UnifiedAuthMiddleware"}, tiFuncs(census["ProxyUserProvisioner"], "cfg"), "ProxyUserProvisioner callers")
	auth, refuse3a, prov := tiFirst(census["Authenticate"], "ProxyAuthenticator", "UnifiedAuthMiddleware"),
		tiFirst(census["contains"], "cfg.DriverPrincipals", "UnifiedAuthMiddleware"),
		tiFirst(census["ProxyUserProvisioner"], "cfg", "UnifiedAuthMiddleware")
	require.NotEqual(t, token.NoPos, refuse3a, "Step 3a has a driver refusal")
	assert.True(t, auth < refuse3a && refuse3a < prov, "Step 3a: assertion %v, refusal %v, provisioner %v", fset.Position(auth), fset.Position(refuse3a), fset.Position(prov))
	// Web fresh path: the refusal comes after the assertion and before the
	// find-or-create.
	wauth, wrefuse := tiFirst(census["Authenticate"], "ProxyAuthenticator", "proxyAuthMiddleware"),
		tiFirst(census["contains"], "ws.proxyDrivers", "proxyAuthMiddleware")
	wlookup := tiFirst(census["GetUserByEmail"], "ws.store", "proxyAuthMiddleware")
	wcreate := tiFirst(census["CreateUser"], "ws.store", "proxyAuthMiddleware")
	require.NotEqual(t, token.NoPos, wrefuse, "the web fresh path has a driver refusal")
	assert.True(t, wauth < wrefuse && wrefuse < wlookup && wrefuse < wcreate, "web fresh path: assertion %v, refusal %v, lookup %v, create %v",
		fset.Position(wauth), fset.Position(wrefuse), fset.Position(wlookup), fset.Position(wcreate))

	// External Google identities: resolved to a user only through
	// GoogleIdentityResolver.Resolve, from the external-bearer path and the
	// GE exchange, and Resolve refuses a driver before anything else.
	resolveCallers := map[string]bool{}
	for recv, cs := range census["Resolve"] {
		if strings.HasSuffix(recv, "GoogleResolver") || strings.HasSuffix(recv, "s.resolver") {
			for _, c := range cs {
				resolveCallers[c.fn] = true
			}
		}
	}
	assert.Equal(t, map[string]bool{"authenticateExternalBearer": true, "Exchange": true}, resolveCallers, "GoogleIdentityResolver.Resolve callers")
	assert.Equal(t, []string{"Exchange", "ValidateIDToken", "authenticateExternalBearer", "fixtureSessionBearer", "verifyWorkloadToken"},
		tiFuncs(census["ValidateIDToken"], ""), "every Google ID token verification site (the caching decorator, the driver token and workload delegation never create a user)")
	rrefuse := tiFirst(census["contains"], "r.driverPrincipals", "Resolve")
	rfirst := tiFirst(census["GetExternalIdentity"], "r.extIDs", "Resolve")
	require.NotEqual(t, token.NoPos, rrefuse, "Resolve has a driver refusal")
	require.NotEqual(t, token.NoPos, rfirst)
	assert.True(t, rrefuse < rfirst, "Resolve refuses a driver (%v) before its first lookup (%v)", fset.Position(rrefuse), fset.Position(rfirst))
}
