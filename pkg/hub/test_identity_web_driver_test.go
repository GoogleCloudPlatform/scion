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
	"log/slog"
	"net/http"
	"net/http/httptest"
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
// refused before the resolver can find or create an account for it.
func TestDriverPrincipal_RefusedOnExternalBearer(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()
	userStore := newFakeUserStore()
	resolver := NewGoogleIdentityResolver(userStore, newMemExtIDStore(), alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"ti-test"})
	cfg.DriverPrincipals = newDriverPrincipalSet([]string{tiDriverEmail})

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
