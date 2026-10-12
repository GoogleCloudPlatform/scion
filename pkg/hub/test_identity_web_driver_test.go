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

	// The control is an authoritative identity (a gmail address), so only
	// the driver refusal can tell the two apart.
	u, err := resolver.Resolve(context.Background(), identity("someone@gmail.com", false), ResolvePolicy{PreAuthorized: true})
	require.NoError(t, err, "another identity still resolves")
	assert.Equal(t, "someone@gmail.com", u.Email)
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
				if id, ok := call.Fun.(*ast.Ident); ok {
					if out[id.Name] == nil {
						out[id.Name] = map[string][]tiCall{}
					}
					out[id.Name]["-"] = append(out[id.Name]["-"], tiCall{fn: fd.Name.Name, pos: call.Pos()})
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

// tiPairs returns every call of method name in the package as sorted
// "function receiver" pairs, whatever the receiver expression.
func tiPairs(census map[string]map[string][]tiCall, name string) []string {
	var out []string
	for recv, cs := range census[name] {
		for _, c := range cs {
			out = append(out, c.fn+" "+recv)
		}
	}
	sort.Strings(out)
	return out
}

// tiTransportCheck returns the position of the first transport identity
// check in fn: a call of an isTransportIdentity predicate or a driver set's
// contains.
func tiTransportCheck(census map[string]map[string][]tiCall, fn string) token.Pos {
	first := token.NoPos
	consider := func(c tiCall) {
		if c.fn == fn && (first == token.NoPos || c.pos < first) {
			first = c.pos
		}
	}
	for _, cs := range census["isTransportIdentity"] {
		for _, c := range cs {
			consider(c)
		}
	}
	for recv, cs := range census["contains"] {
		if strings.HasSuffix(recv, "proxyDrivers") || strings.HasSuffix(recv, "DriverPrincipals") || strings.HasSuffix(recv, "driverPrincipals") {
			for _, c := range cs {
				consider(c)
			}
		}
	}
	return first
}

// The transport identity invariant (WS2 C3, advisor addenda): a transport
// identity (the platform service account or a proxy driver principal)
// never becomes, signs in as, or is served as a hub user. The census is
// anchored on two enumerable syntactic sets of the package, matched on the
// method name whatever the receiver, so a new site under any name fails
// this test until it is handled and listed:
//   - every isReservedPlatformIdentity call is inside one of the unified
//     isTransportIdentity predicates, so every site that refuses the
//     platform service account refuses driver principals too;
//   - every CreateUser call is in a function that runs a transport check
//     before it, or in a helper whose every caller does, or is the one
//     listed exemption.
//
// It also keeps the ingress lists explicit: proxy assertions are verified
// only at API Step 3a, on the web fresh path and by the binding/redeem
// helper; GoogleIdentityResolver.Resolve is the only place an external
// Google identity becomes a user; each refusal comes before the lookup or
// provisioning of its function.
func TestTIWebIAP_TransportIdentityInvariant(t *testing.T) {
	census, fset := tiCensus(t)

	// 1. isReservedPlatformIdentity is called only by the predicates.
	assert.Equal(t, []string{"isTransportIdentity -", "isTransportIdentity -", "isTransportIdentity -", "isTransportIdentity -"},
		tiPairs(census, "isReservedPlatformIdentity"), "isReservedPlatformIdentity callers")

	// 2. Every CreateUser call site.
	exempt := map[string]string{
		"seedDevUser": "seeds the config-fixed dev pseudo-user (DevUserID); no request input reaches it",
	}
	// Helpers whose callers carry the check, with every caller listed.
	helperCallers := map[string][]string{
		"createPendingUserTx": {"handleAdminUserInvite", "handleProvisionUser"},
		"provisionNewUser":    {"Resolve"},
	}
	creators := map[string]bool{}
	for _, pair := range tiPairs(census, "CreateUser") {
		creators[strings.Fields(pair)[0]] = true
	}
	assert.Equal(t, map[string]bool{
		"handleAdminAllowListAdd": true, "handleAdminAllowListImport": true, "handleAdminUserInviteBulk": true,
		"createPendingUserTx": true, "provisionNewUser": true, "provisionUser": true, "handleTestLogin": true,
		"seedDevUser": true, "proxyAuthMiddleware": true, "handleOAuthCallback": true,
	}, creators, "every function that calls CreateUser")
	for fn := range creators {
		if reason, ok := exempt[fn]; ok {
			t.Logf("exempt: %s (%s)", fn, reason)
			continue
		}
		create := token.NoPos
		for _, cs := range census["CreateUser"] {
			for _, c := range cs {
				if c.fn == fn && (create == token.NoPos || c.pos < create) {
					create = c.pos
				}
			}
		}
		if callers, ok := helperCallers[fn]; ok {
			got := map[string]bool{}
			for _, pair := range append(tiPairs(census, fn), tiFuncCalls(census, fn)...) {
				got[strings.Fields(pair)[0]] = true
			}
			want := map[string]bool{}
			for _, c := range callers {
				want[c] = true
			}
			assert.Equal(t, want, got, "callers of helper %s", fn)
			for _, caller := range callers {
				check, call := tiTransportCheck(census, caller), tiFirstCallOf(census, fn, caller)
				assert.True(t, check != token.NoPos && check < call, "%s checks a transport identity (%v) before calling %s (%v)", caller, fset.Position(check), fn, fset.Position(call))
			}
			continue
		}
		check := tiTransportCheck(census, fn)
		assert.True(t, check != token.NoPos && check < create, "%s checks a transport identity (%v) before CreateUser (%v)", fn, fset.Position(check), fset.Position(create))
	}

	// 3. Proxy assertions.
	assert.Equal(t, []string{
		"UnifiedAuthMiddleware cfg.ProxyAuthenticator",
		"UnifiedAuthMiddleware fedAuth",
		"proxyAuthMiddleware ws.config.ProxyAuthenticator",
		"verifiedProxyAssertion ws.config.ProxyAuthenticator",
	}, tiPairs(census, "Authenticate"), "every Authenticate call")
	assert.Equal(t, []string{"handleTestIdentityRedeem ws", "handleTestIdentityRedeem ws", "testIdentityDriverBinding ws"},
		tiPairs(census, "verifiedProxyAssertion"), "verifiedProxyAssertion feeds only the binding and redeem")
	assert.Equal(t, []string{"UnifiedAuthMiddleware cfg"}, tiPairs(census, "ProxyUserProvisioner"), "ProxyUserProvisioner callers")
	auth, refuse3a, prov := tiFirst(census["Authenticate"], "cfg.ProxyAuthenticator", "UnifiedAuthMiddleware"),
		tiFirst(census["contains"], "cfg.DriverPrincipals", "UnifiedAuthMiddleware"),
		tiFirst(census["ProxyUserProvisioner"], "cfg", "UnifiedAuthMiddleware")
	assert.True(t, refuse3a != token.NoPos && auth < refuse3a && refuse3a < prov, "Step 3a: assertion %v, refusal %v, provisioner %v",
		fset.Position(auth), fset.Position(refuse3a), fset.Position(prov))
	wauth, wrefuse := tiFirst(census["Authenticate"], "ws.config.ProxyAuthenticator", "proxyAuthMiddleware"),
		tiFirst(census["contains"], "ws.proxyDrivers", "proxyAuthMiddleware")
	wlookup := tiFirst(census["GetUserByEmail"], "ws.store", "proxyAuthMiddleware")
	assert.True(t, wrefuse != token.NoPos && wauth < wrefuse && wrefuse < wlookup, "web fresh path: assertion %v, refusal %v, lookup %v",
		fset.Position(wauth), fset.Position(wrefuse), fset.Position(wlookup))

	// 4. External Google identities: Resolve is the only provisioning
	// ingress (every .Resolve call in the package is listed; only the two
	// GoogleIdentityResolver calls resolve users), and it refuses a
	// transport identity before its first lookup.
	assert.Equal(t, []string{
		"Exchange s.resolver",
		"agentPTYPath rt.router",
		"authenticateExternalBearer cfg.GoogleResolver",
		"brokerPTYPath rt.router",
		"cleanupNFSSharedDirTree runtime.NewNFSBackend()",
		"resolveAsNeededForKeys d.secretBackend",
		"resolveNFSSharedDirPath runtime.NewNFSBackend()",
		"resolveOutboundRouting messaging",
		"resolveSecrets d.secretBackend",
	}, tiPairs(census, "Resolve"), "every Resolve call (the GoogleIdentityResolver ones: Exchange, authenticateExternalBearer)")
	assert.Equal(t, []string{
		"Exchange s.validator",
		"ValidateIDToken c.base",
		"authenticateExternalBearer cfg.GoogleValidator",
		"fixtureSessionBearer ws.driverTokenValidator",
		"verifyWorkloadToken a.validator",
	}, tiPairs(census, "ValidateIDToken"), "every Google ID token verification (the cache decorator, the driver token and workload delegation never create a user)")
	rrefuse := tiFirst(census["isTransportIdentity"], "r", "Resolve")
	rfirst := tiFirst(census["GetExternalIdentity"], "r.extIDs", "Resolve")
	assert.True(t, rrefuse != token.NoPos && rfirst != token.NoPos && rrefuse < rfirst, "Resolve refuses a transport identity (%v) before its first lookup (%v)",
		fset.Position(rrefuse), fset.Position(rfirst))
}

// tiFuncCalls returns the plain (non-selector) calls of fn as "caller -"
// pairs.
func tiFuncCalls(census map[string]map[string][]tiCall, fn string) []string {
	var out []string
	for _, c := range census[fn]["-"] {
		out = append(out, c.fn+" -")
	}
	return out
}

// tiFirstCallOf returns the first call of fn (any receiver, or a plain
// call) inside caller.
func tiFirstCallOf(census map[string]map[string][]tiCall, fn, caller string) token.Pos {
	first := token.NoPos
	for _, cs := range census[fn] {
		for _, c := range cs {
			if c.fn == caller && (first == token.NoPos || c.pos < first) {
				first = c.pos
			}
		}
	}
	return first
}
