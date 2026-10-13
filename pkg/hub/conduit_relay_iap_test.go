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
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const relayIAPAudience = "/projects/123/global/backendServices/456"

// relayIAPFixture is a hub in external relay mode with Google IAP proxy
// authentication, verified against a test key endpoint the relay pin
// points at.
type relayIAPFixture struct {
	*relayOpsFixture
	cf   *conduitFixture
	kp   *testKeyPair
	jwks string
}

func newRelayIAPFixture(t *testing.T) *relayIAPFixture {
	t.Helper()
	f, cf := newUserRelayOps(t)
	kp := newTestKeyPair(t, "iap-kid")
	jwks := startJWKSServer(t, kp.jwksJSON(t))
	prev := conduitRelayIAPJWKSURL
	conduitRelayIAPJWKSURL = jwks.URL
	t.Cleanup(func() { conduitRelayIAPJWKSURL = prev })
	cf.srv.authConfig.ProxyAuthenticator = &IAPAuthenticator{Audience: relayIAPAudience, JWKSURL: jwks.URL}
	cf.srv.authConfig.ProxyUserProvisioner = func(_ context.Context, info *ProxyUserInfo) (UserIdentity, error) {
		if info.Email != "owner@conduit.test" {
			return nil, ErrAccessDenied
		}
		return cf.owner, nil
	}
	return &relayIAPFixture{relayOpsFixture: f, cf: cf, kp: kp, jwks: jwks.URL}
}

// assertion signs an IAP assertion for the owner with aud, valid from now
// until exp.
func (f *relayIAPFixture) assertion(t *testing.T, kp *testKeyPair, aud string, exp time.Time) http.Header {
	t.Helper()
	now := time.Now()
	tok := kp.signJWT(t, makeTestClaims("sub-owner", "owner@conduit.test", DefaultIAPIssuer, aud, now.Add(-time.Minute), exp))
	return http.Header{"X-Goog-Iap-Jwt-Assertion": {tok}}
}

// TestRelayAdmit_IAPAssertion: a valid IAP assertion is
// admitted through a relay, with the proxy session's expiry from
// admission; a bad signature, the wrong audience or an expired assertion
// each get 401; a raw IAP identity header has no effect even under a
// trusted-proxy setting covering the forwarded address.
func TestRelayAdmit_IAPAssertion(t *testing.T) {
	f := newRelayIAPFixture(t)
	before := time.Now()
	adm := f.mustAdmit(t, f.assertion(t, f.kp, relayIAPAudience, time.Now().Add(10*time.Minute)), "s-iap")
	assert.Equal(t, f.cf.owner.ID(), adm.PrincipalID)
	maxAge := f.cf.srv.conduitProxySessionMaxAge()
	assert.WithinRange(t, adm.CredentialExpiry, before.Add(maxAge).Add(-time.Second), time.Now().Add(maxAge).Add(time.Second),
		"admission plus proxy_session_max_age, not the assertion's expiry")

	other := newTestKeyPair(t, "iap-kid")
	for name, h := range map[string]http.Header{
		"bad signature":  f.assertion(t, other, relayIAPAudience, time.Now().Add(10*time.Minute)),
		"wrong audience": f.assertion(t, f.kp, "/projects/123/global/backendServices/999", time.Now().Add(10*time.Minute)),
		"expired":        f.assertion(t, f.kp, relayIAPAudience, time.Now().Add(-10*time.Minute)),
	} {
		_, err := f.admit(t, h, "s-bad")
		assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), name)
	}

	f.cf.srv.authConfig.TrustedProxies = []string{"192.0.2.0/24"}
	raw := http.Header{"X-Goog-Authenticated-User-Email": {"accounts.google.com:owner@conduit.test"}, "X-Goog-Authenticated-User-Id": {"accounts.google.com:1"}}
	var resp relay.HubAdmitResponse
	err := f.hub.Call(context.Background(), relay.HubAdmitPath, relay.HubAdmitRequest{
		RelayInstanceID: "relay-0", RelayGeneration: f.relayGen, SessionID: "s-raw", Host: relayPublicHost, Headers: raw,
	}, &resp)
	require.Error(t, err, "raw IAP identity headers admitted as %+v", resp)
}

// TestRelayAdmit_IAPRequiresPinnedAuthenticator: a relay-forwarded proxy
// credential is admitted only through the IAP authenticator with a
// configured audience and Google's issuer and key endpoint. Without a
// proxy authenticator the assertion is ignored (no fallback to headers);
// the generic JWT proxy authenticator, a custom issuer or key endpoint and
// an empty audience are refused.
func TestRelayAdmit_IAPRequiresPinnedAuthenticator(t *testing.T) {
	f := newRelayIAPFixture(t)
	valid := func() http.Header { return f.assertion(t, f.kp, relayIAPAudience, time.Now().Add(10*time.Minute)) }

	t.Run("no proxy authenticator, trusted-proxy headers only", func(t *testing.T) {
		prev := f.cf.srv.authConfig.ProxyAuthenticator
		f.cf.srv.authConfig.ProxyAuthenticator = nil
		f.cf.srv.authConfig.TrustedProxies = []string{"192.0.2.0/24"}
		defer func() { f.cf.srv.authConfig.ProxyAuthenticator, f.cf.srv.authConfig.TrustedProxies = prev, nil }()
		_, err := f.admit(t, valid(), "s-legacy")
		require.Error(t, err, "the assertion header is ignored without an IAP authenticator")
	})

	for name, auth := range map[string]ProxyAuthenticator{
		"generic JWT proxy authenticator": &JWTProxyAuthenticator{Header: IAPAssertionHeader, Algorithm: string(jose.ES256), KeySource: NewJWKSURLKeySource(f.jwks), Audience: relayIAPAudience, Issuer: DefaultIAPIssuer},
		"custom issuer":                   &IAPAuthenticator{Audience: relayIAPAudience, JWKSURL: f.jwks, Issuer: DefaultIAPIssuer + "/custom"},
		"custom key endpoint":             &IAPAuthenticator{Audience: relayIAPAudience, JWKSURL: f.jwks + "/other"},
	} {
		t.Run(name, func(t *testing.T) {
			prev := f.cf.srv.authConfig.ProxyAuthenticator
			f.cf.srv.authConfig.ProxyAuthenticator = auth
			defer func() { f.cf.srv.authConfig.ProxyAuthenticator = prev }()
			_, err := f.admit(t, valid(), "s-unpinned")
			require.Error(t, err, "admitted through %s", name)
		})
	}

	t.Run("empty audience", func(t *testing.T) {
		f.cf.srv.authConfig.ProxyAuthenticator = &IAPAuthenticator{JWKSURL: f.jwks}
		defer func() {
			f.cf.srv.authConfig.ProxyAuthenticator = &IAPAuthenticator{Audience: relayIAPAudience, JWKSURL: f.jwks}
		}()
		assert.False(t, f.cf.srv.relayIAPPinned())
		_, err := f.admit(t, f.assertion(t, f.kp, "", time.Now().Add(10*time.Minute)), "s-empty-aud")
		require.Error(t, err)
	})

	// The pinned configuration admits (the refusals above are meaningful).
	f.mustAdmit(t, valid(), "s-pinned")
}

// TestRelayIAPSession_UserStateRecheckedAndExpiryFixed: every re-check of a
// relay-held IAP session re-reads the user (a suspended user is closed with
// 4401 at the next re-check); its credential expiry stays at admission plus
// proxy_session_max_age: past it a re-check does not re-issue the token,
// RPCs get 401 and a refresh is refused.
func TestRelayIAPSession_UserStateRecheckedAndExpiryFixed(t *testing.T) {
	t.Run("suspended user dropped", func(t *testing.T) {
		f := newRelayIAPFixture(t)
		adm := f.mustAdmit(t, f.assertion(t, f.kp, relayIAPAudience, time.Now().Add(10*time.Minute)), "s-iap")
		res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-iap", AdmissionToken: adm.AdmissionToken})
		require.True(t, res[0].OK)
		setUserStatus(t, f.cf.store, f.cf.owner.ID(), store.UserStatusSuspended)
		res = sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-iap", AdmissionToken: adm.AdmissionToken})
		assert.True(t, res[0].Close)
		assert.Equal(t, uint32(4401), res[0].CloseCode)
	})

	t.Run("re-issue before the cap keeps the cap", func(t *testing.T) {
		f := newRelayIAPFixture(t)
		now := relayClock(t, time.Now())
		adm := f.mustAdmit(t, f.assertion(t, f.kp, relayIAPAudience, time.Now().Add(10*time.Minute)), "s-iap")
		capAt := adm.CredentialExpiry
		require.False(t, capAt.IsZero())
		// Past half of the token's lifetime, before the cap.
		*now = now.Add(capAt.Sub(*now) * 2 / 3)
		require.True(t, now.Before(capAt))
		res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-iap", AdmissionToken: adm.AdmissionToken})
		require.True(t, res[0].OK)
		require.NotEmpty(t, res[0].AdmissionToken, "re-issued past half of its lifetime")
		c, err := f.cf.srv.verifyAdmissionToken(res[0].AdmissionToken, admissionCheck{sessionID: "s-iap", relay: "relay-0", relayGen: f.relayGen, allowExpired: true}, *now)
		require.NoError(t, err)
		assert.Equal(t, capAt.Unix(), c.CredentialExpiry, "the credential expiry is unchanged")
		assert.LessOrEqual(t, c.Expiry, capAt.Unix(), "the re-issued token ends at or before the cap")
	})

	t.Run("expiry fixed at admission", func(t *testing.T) {
		f := newRelayIAPFixture(t)
		now := relayClock(t, time.Now())
		adm := f.mustAdmit(t, f.assertion(t, f.kp, relayIAPAudience, time.Now().Add(10*time.Minute)), "s-iap")
		require.False(t, adm.CredentialExpiry.IsZero())
		*now = adm.CredentialExpiry.Add(time.Minute)
		res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-iap", AdmissionToken: adm.AdmissionToken})
		require.True(t, res[0].OK, "the user is still active")
		assert.Empty(t, res[0].AdmissionToken, "no re-issue moves the cap")
		_, err := rpcCall(t, f.hub, "s-iap", adm.AdmissionToken, "/v1/unknown")
		assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "RPC past the cap")
		err = f.hub.Call(context.Background(), relay.HubSessionPath("s-iap", "refresh"), relay.HubRefreshRequest{AdmissionToken: adm.AdmissionToken, Credential: []byte("x")}, nil)
		assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "refresh past the cap")
	})
}
