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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The principal check on federation credentials (ptone/scion#5080): a
// credential naming a test identity or the platform service account is
// refused, whatever its claims say. The peer signs these credentials with
// its own key, as a peer that issues them anyway would.

// peerTokPlatformAuthSA is the receiving hub's platform service account in
// these tests.
const peerTokPlatformAuthSA = "platform-transport@hub-recv.iam.gserviceaccount.com"

// A user access credential from a peer hub naming a test identity is
// refused, for every reserved test identity domain.
func TestFederationPrincipalCheck_TestIdentityRefused(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-human", "human@corp.com")), "sub-human", "human@corp.com")

	for name, email := range map[string]string{
		"member fixture":      "driver-1@scion-fixture.invalid",
		"hub admin":           "driver-2@scion-test-hub-admin.invalid",
		"super admin":         "driver-3@scion-test-super-admin.invalid",
		"mixed case":          "Driver-4@SCION-Fixture.Invalid",
		"subdomain":           "driver-5@run.scion-fixture.invalid",
		"federated synthetic": "fed+abc123@idp.example.com.invalid",
	} {
		t.Run(name, func(t *testing.T) {
			assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-"+name, email)))
		})
	}
}

// The reserved domains match by exact domain or subdomain suffix
// (store.IsReservedTestIdentityEmail, emailInDomain in pkg/store), not by
// substring: look-alike domains are accepted like any other email.
func TestFederationPrincipalCheck_LookAlikeDomainsAccepted(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	for _, email := range []string{
		"a@scion-fixture.invalid.example.com",
		"b@xscion-fixture.invalid",
		"c@scion-test-hub-admin.invalid.example.com",
		"d@xscion-test-hub-admin.invalid",
		"e@scion-test-super-admin.invalid.example.com",
		"f@xscion-test-super-admin.invalid",
	} {
		t.Run(email, func(t *testing.T) {
			assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-"+email, email)), "sub-"+email, email)
		})
	}
}

// A credential naming this hub's platform service account is refused
// (email compared case-insensitively); a normal user's credential is still
// accepted with the account configured. On the peer hub user access path
// the service account email rule in extractHubUserClaims refuses the
// account first, so the issuer_type user cases are the ones that show the
// platform service account rule on its own: there, other service account
// emails are still accepted.
func TestFederationPrincipalCheck_PlatformServiceAccountRefused(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	f.srv.authConfig.PlatformAuthSA = peerTokPlatformAuthSA

	assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-platform", peerTokPlatformAuthSA)))
	assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-platform-upper", "Platform-Transport@HUB-RECV.iam.gserviceaccount.com")))

	assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-human", "human@corp.com")), "sub-human", "human@corp.com")

	t.Run("issuer_type user: platform service account", func(t *testing.T) {
		assertPeerTokRefused(t, f, f.userIssuerToken(t, "idp-platform", peerTokPlatformAuthSA))
	})
	t.Run("issuer_type user: platform service account, mixed case", func(t *testing.T) {
		assertPeerTokRefused(t, f, f.userIssuerToken(t, "idp-platform-upper", "Platform-Transport@HUB-RECV.iam.gserviceaccount.com"))
	})
	t.Run("issuer_type user: test identity", func(t *testing.T) {
		assertPeerTokRefused(t, f, f.userIssuerToken(t, "idp-fixture", "driver-1@scion-fixture.invalid"))
	})
	t.Run("issuer_type user: other service account accepted", func(t *testing.T) {
		identity, _, rec := f.through(t, f.userIssuerToken(t, "idp-worker", "worker@proj.iam.gserviceaccount.com"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		fu, ok := identity.(*FederatedUserIdentity)
		require.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
		assert.Equal(t, "worker@proj.iam.gserviceaccount.com", fu.Email())
	})
}

// peerTokDriverPrincipal and peerTokDriverPrincipalSA are proxy driver
// principals of the receiving hub in these tests: one outside and one inside
// the gserviceaccount.com domain.
const (
	peerTokDriverPrincipal   = "iap-driver@corp.example.com"
	peerTokDriverPrincipalSA = "iap-driver@hub-recv.iam.gserviceaccount.com"
)

// A credential naming one of this hub's proxy driver principals is refused
// with the uniform federation refusal (email compared case-insensitively),
// on the peer hub user access path and for an issuer_type user issuer; a
// normal user's credential and other service account emails are still
// accepted with drivers configured.
func TestFederationPrincipalCheck_DriverPrincipalRefused(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	f.srv.authConfig.DriverPrincipals = newDriverPrincipalSet([]string{peerTokDriverPrincipal, peerTokDriverPrincipalSA})

	assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-driver", peerTokDriverPrincipal)))
	assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-driver-upper", "IAP-Driver@Corp.Example.com")))

	assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-human", "human@corp.com")), "sub-human", "human@corp.com")

	t.Run("issuer_type user: driver principal", func(t *testing.T) {
		assertPeerTokRefused(t, f, f.userIssuerToken(t, "idp-driver", peerTokDriverPrincipal))
	})
	t.Run("issuer_type user: service account driver principal", func(t *testing.T) {
		assertPeerTokRefused(t, f, f.userIssuerToken(t, "idp-driver-sa", peerTokDriverPrincipalSA))
	})
	t.Run("issuer_type user: other service account accepted", func(t *testing.T) {
		identity, _, rec := f.through(t, f.userIssuerToken(t, "idp-worker", "worker@proj.iam.gserviceaccount.com"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		fu, ok := identity.(*FederatedUserIdentity)
		require.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
		assert.Equal(t, "worker@proj.iam.gserviceaccount.com", fu.Email())
	})

	// Control: with no driver principals configured, the same emails are
	// accepted, so the refusals above come from the driver principal rule.
	t.Run("no drivers configured: same emails accepted", func(t *testing.T) {
		f.srv.authConfig.DriverPrincipals = nil
		assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-driver-ctl", peerTokDriverPrincipal)), "sub-driver-ctl", peerTokDriverPrincipal)
		identity, _, rec := f.through(t, f.userIssuerToken(t, "idp-driver-sa-ctl", peerTokDriverPrincipalSA))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		fu, ok := identity.(*FederatedUserIdentity)
		require.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
		assert.Equal(t, peerTokDriverPrincipalSA, fu.Email())
	})
}

// userIssuerToken returns a credential for subject and email signed by the
// issuer_type user issuer of the fixture.
func (f *peerTokFixture) userIssuerToken(t *testing.T, subject, email string) string {
	t.Helper()
	now := time.Now()
	return f.signUser(t, map[string]interface{}{
		"iss":   peerTokUserIssuerURL,
		"sub":   subject,
		"aud":   []string{federationTokenAudience},
		"iat":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(10 * time.Minute).Unix(),
		"email": email,
	})
}

// Authenticate is a wrapper over AuthenticateWithOptions: called with no
// options it still runs the principal check and the kind check, and the
// options it is given reach AuthenticateWithOptions.
func TestFederationPrincipalCheck_AuthenticateWrapper(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	auth := f.srv.federationAuth.Load()
	require.NotNil(t, auth)

	t.Run("no options: principal check runs", func(t *testing.T) {
		_, err := auth.Authenticate(f.userIssuerToken(t, "idp-fixture", "driver-1@scion-fixture.invalid"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "credential names a test identity")

		identity, err := auth.Authenticate(f.userIssuerToken(t, "idp-human", "human@corp.com"))
		require.NoError(t, err)
		_, ok := identity.(*FederatedUserIdentity)
		assert.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
	})

	peerUser := f.signAsPeer(t, peerUserClaims("sub-human", "human@corp.com"))
	t.Run("no options: kind check runs", func(t *testing.T) {
		_, err := auth.Authenticate(peerUser)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "user access credentials from hub issuers are not accepted")
	})

	t.Run("options reach AuthenticateWithOptions", func(t *testing.T) {
		identity, err := auth.Authenticate(peerUser, FederationAuthOptions{AcceptHubUserTokens: true})
		require.NoError(t, err)
		_, ok := identity.(*FederatedUserIdentity)
		assert.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)

		platform := f.userIssuerToken(t, "idp-platform", peerTokPlatformAuthSA)
		_, err = auth.Authenticate(platform, FederationAuthOptions{PlatformAuthSA: peerTokPlatformAuthSA})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "credential names a transport identity of this hub")
		_, err = auth.Authenticate(platform)
		require.NoError(t, err, "without the option the account is another service account email")
	})
}

// A credential minted by the peer for a normal human user is still accepted
// with the platform service account configured, and agent credentials are
// unchanged.
func TestFederationPrincipalCheck_HumanAndAgentAccepted(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	f.srv.authConfig.PlatformAuthSA = peerTokPlatformAuthSA

	token, u := f.mintUserToken(t)
	assertPeerTokUser(t, f, token, u.ID, u.Email)

	fa := assertPeerTokAgent(t, f, f.mintAgentToken(t))
	assert.Equal(t, f.peerAgent.Subject, fa.RemoteAgentID())
}

// federationPrincipalRefusal by identity type: it reads the email of user
// and service account identities, and never refuses an agent identity.
// The platform service account and proxy driver principals are refused
// through AuthConfig.isTransportIdentity.
func TestFederationPrincipalRefusal(t *testing.T) {
	opts := FederationAuthOptions{
		PlatformAuthSA:   peerTokPlatformAuthSA,
		DriverPrincipals: newDriverPrincipalSet([]string{peerTokDriverPrincipal}),
	}
	const iss = "https://issuer.example.com"
	for _, tc := range []struct {
		name     string
		identity FederatedIdentity
		opts     FederationAuthOptions
		refused  bool
	}{
		{"user: human", NewFederatedUserIdentity(iss, "s1", "human@corp.com", "H", "viewer", nil), opts, false},
		{"user: test identity", NewFederatedUserIdentity(iss, "s2", "x@scion-fixture.invalid", "X", "viewer", nil), opts, true},
		{"user: platform service account", NewFederatedUserIdentity(iss, "s3", peerTokPlatformAuthSA, "P", "viewer", nil), opts, true},
		{"service: platform service account", NewFederatedServiceIdentity(iss, "s4", peerTokPlatformAuthSA, nil), opts, true},
		{"service: platform service account, none configured", NewFederatedServiceIdentity(iss, "s5", peerTokPlatformAuthSA, nil), FederationAuthOptions{}, false},
		{"service: other service account", NewFederatedServiceIdentity(iss, "s6", "worker@proj.iam.gserviceaccount.com", nil), opts, false},
		{"service: test identity", NewFederatedServiceIdentity(iss, "s7", "x@scion-test-hub-admin.invalid", nil), opts, true},
		{"user: driver principal", NewFederatedUserIdentity(iss, "s8", peerTokDriverPrincipal, "D", "viewer", nil), opts, true},
		{"service: driver principal", NewFederatedServiceIdentity(iss, "s9", "IAP-Driver@corp.example.com", nil), opts, true},
		{"user: driver principal, none configured", NewFederatedUserIdentity(iss, "s10", peerTokDriverPrincipal, "D", "viewer", nil), FederationAuthOptions{}, false},
		{"agent", NewFederatedAgentIdentity(iss, "agent-1", "project-a", "worker", "user:root", []string{"user:root"}, nil), opts, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := federationPrincipalRefusal(tc.identity, tc.opts)
			if tc.refused {
				assert.NotEmpty(t, reason)
			} else {
				assert.Empty(t, reason)
			}
		})
	}
}

// A user access credential from a peer hub naming an email in the
// gserviceaccount.com domain or any subdomain of it is refused (exact domain
// or subdomain suffix, trailing dot ignored); a normal domain email and
// look-alike domains are accepted. issuer_type user issuers are not
// affected.
func TestFederationPrincipalCheck_ServiceAccountUserTokenRefused(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-human", "human@corp.com")), "sub-human", "human@corp.com")

	for name, email := range map[string]string{
		"iam":           "driver@proj.iam.gserviceaccount.com",
		"appspot":       "proj@appspot.gserviceaccount.com",
		"developer":     "123-compute@developer.gserviceaccount.com",
		"system":        "svc@system.gserviceaccount.com",
		"mixed case":    "Driver@Proj.IAM.GServiceAccount.com",
		"cloudservices": "123456789@cloudservices.gserviceaccount.com",
		"cloudbuild":    "123456789@cloudbuild.gserviceaccount.com",
		"other managed": "service-123@gcp-sa-example.iam.gserviceaccount.com",
		"bare domain":   "svc@gserviceaccount.com",
		"trailing dot":  "driver@proj.iam.gserviceaccount.com.",
	} {
		t.Run(name, func(t *testing.T) {
			assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-"+name, email)))
		})
	}

	for name, email := range map[string]string{
		"look-alike prefix": "a@xgserviceaccount.com",
		"look-alike suffix": "b@proj.iam.gserviceaccount.com.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-"+name, email)), "sub-"+name, email)
		})
	}

	t.Run("issuer_type user unchanged", func(t *testing.T) {
		now := time.Now()
		identity, _, rec := f.through(t, f.signUser(t, map[string]interface{}{
			"iss":   peerTokUserIssuerURL,
			"sub":   "idp-sa",
			"aud":   []string{federationTokenAudience},
			"iat":   now.Add(-time.Minute).Unix(),
			"exp":   now.Add(10 * time.Minute).Unix(),
			"email": "worker@proj.iam.gserviceaccount.com",
		}))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		_, ok := identity.(*FederatedUserIdentity)
		assert.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
	})
}

// googleUserIssuerFixture is the Google issuer (issuer_type user, with an
// expected_audience) trusted through a real, validated
// FederationAuthenticator whose JWKS is a test server, so credentials it
// signs verify on the federation-token path. cfg carries the same
// authenticator for UnifiedAuthMiddleware.
type googleUserIssuerFixture struct {
	auth *FederationAuthenticator
	cfg  AuthConfig
	sign func(t *testing.T, claims interface{}) string
}

// newGoogleUserIssuerFixture builds the Google user issuer with the given
// allowed_domains and allowed_emails. validator and resolver serve the
// external-bearer path of cfg and may be nil when only the federation-token
// path is exercised.
func newGoogleUserIssuerFixture(t *testing.T, allowedDomains, allowedEmails []string, validator GoogleCredentialValidator, resolver *GoogleIdentityResolver) *googleUserIssuerFixture {
	t.Helper()
	key, jwks, kid := setupFederationTestServer(t)
	auth := newTestAuthenticatorWithConfig(t, config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{{
			IssuerURL:        googleIssuerHTTPS,
			IssuerType:       string(IssuerTypeUser),
			JWKSURL:          jwks.URL,
			ExpectedAudience: externalBearerTestAudience,
			AllowedDomains:   allowedDomains,
			AllowedEmails:    allowedEmails,
		}},
	}, "https://hub.example.com")
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	cfg := AuthConfig{
		Mode:           "production",
		UserTokenSvc:   userTokenSvc,
		FederationAuth: federationAuthPointer(auth),
		Logger:         slog.Default(),
	}
	if validator != nil {
		cfg.GoogleValidator = validator
		cfg.GoogleResolver = resolver
	}
	return &googleUserIssuerFixture{
		auth: auth,
		cfg:  cfg,
		sign: func(t *testing.T, claims interface{}) string { return signGenericToken(t, key, kid, claims) },
	}
}

// token returns a Google user credential for subject and email.
func (g *googleUserIssuerFixture) token(t *testing.T, subject, email string) string {
	t.Helper()
	now := time.Now()
	return g.sign(t, map[string]interface{}{
		"iss":            googleIssuerHTTPS,
		"sub":            subject,
		"aud":            []string{externalBearerTestAudience},
		"iat":            now.Add(-time.Minute).Unix(),
		"exp":            now.Add(10 * time.Minute).Unix(),
		"email":          email,
		"email_verified": true,
	})
}

// through sends token in the federation-token header through
// UnifiedAuthMiddleware and reports whether the handler ran.
func (g *googleUserIssuerFixture) through(t *testing.T, token string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	result := &probeResult{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set(FederationTokenHeader, token)
	rec := httptest.NewRecorder()
	UnifiedAuthMiddleware(g.cfg)(probeHandler(result)).ServeHTTP(rec, req)
	return rec, result.reached
}

// On the federation-token path, the issuer's allowed_domains applies to a
// user identity as on the external bearer path and to hub user access
// credentials: an empty list adds no constraint, the domain match is exact
// and case-insensitive with no subdomain match, an email whose domain
// cannot be parsed is refused, and allowed_emails applies unchanged
// alongside it. The refusal cause is asserted on Authenticate; the
// middleware answers every refusal with the same 401.
func TestFederationPrincipalCheck_UserIssuerAllowedDomains(t *testing.T) {
	const (
		causeDomains = "not in allowed_domains"
		causeEmails  = "not in allowed_emails"
	)
	for _, tc := range []struct {
		name    string
		domains []string
		emails  []string
		email   string
		cause   string // "" means admitted
	}{
		{name: "unlisted domain refused", domains: []string{"corp.com"}, email: "user@elsewhere.com", cause: causeDomains},
		{name: "listed domain admitted", domains: []string{"corp.com"}, email: "user@corp.com"},
		{name: "second listed domain admitted", domains: []string{"corp.com", "partner.com"}, email: "user@partner.com"},
		{name: "mixed-case list entry admitted", domains: []string{"Corp.COM"}, email: "user@corp.com"},
		{name: "mixed-case email domain admitted", domains: []string{"corp.com"}, email: "user@CORP.com"},
		{name: "subdomain of listed domain refused", domains: []string{"corp.com"}, email: "user@sub.corp.com", cause: causeDomains},
		{name: "suffix look-alike refused", domains: []string{"corp.com"}, email: "user@xcorp.com", cause: causeDomains},
		{name: "empty list adds no constraint", email: "user@anything-goes.com"},
		{name: "two @ refused", domains: []string{"corp.com"}, email: "user@x@corp.com", cause: causeDomains},
		{name: "trailing dot refused", domains: []string{"corp.com"}, email: "user@corp.com.", cause: causeDomains},
		{name: "no @ refused", domains: []string{"corp.com"}, email: "corp.com", cause: causeDomains},
		{name: "empty email refused", domains: []string{"corp.com"}, email: "", cause: causeDomains},
		{name: "both lists: in both admitted", domains: []string{"corp.com"}, emails: []string{"*@corp.com", "*@elsewhere.com"}, email: "alice@corp.com"},
		{name: "both lists: emails pass, domain unlisted refused", domains: []string{"corp.com"}, emails: []string{"*@corp.com", "*@elsewhere.com"}, email: "bob@elsewhere.com", cause: causeDomains},
		{name: "both lists: listed domain, email unlisted refused", domains: []string{"corp.com"}, emails: []string{"alice@corp.com"}, email: "bob@corp.com", cause: causeEmails},
		{name: "emails only: unchanged", emails: []string{"alice@elsewhere.com"}, email: "alice@elsewhere.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGoogleUserIssuerFixture(t, tc.domains, tc.emails, nil, nil)
			token := g.token(t, "google-sub", tc.email)

			identity, err := g.auth.Authenticate(token)
			rec, reached := g.through(t, token)
			if tc.cause != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.cause)
				assert.Nil(t, identity)
				assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
				assert.False(t, reached, "the handler never runs")
				return
			}
			require.NoError(t, err)
			fu, ok := identity.(*FederatedUserIdentity)
			require.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
			assert.Equal(t, tc.email, fu.Email())
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.True(t, reached)
		})
	}
}

// Parity: for the same issuer allowed_domains, the external bearer path
// (Google ID token), the hub user access credential path (peer hub issuer
// with accept_user_tokens) and the federation-token path (Google user
// issuer) admit and refuse the same user emails. Each path's refusal is
// also checked for its allowed_domains cause, since the middleware answers
// every refusal with the same 401.
func TestFederationPrincipalCheck_AllowedDomainsParity(t *testing.T) {
	const causeDomains = "not in allowed_domains"
	f := newPeerTokFixture(t, nil)
	for _, tc := range []struct {
		name     string
		domains  []string
		email    string
		admitted bool
	}{
		{"listed domain", []string{"corp.com"}, "user@corp.com", true},
		{"unlisted domain", []string{"corp.com"}, "user@elsewhere.com", false},
		{"mixed-case list entry", []string{"Corp.COM"}, "user@corp.com", true},
		{"subdomain of listed domain", []string{"corp.com"}, "user@sub.corp.com", false},
		{"empty list", nil, "user@anything-goes.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := tc.email[len("user@"):]

			// External bearer: a Google ID token in the Authorization header.
			kp := newGCVTestKeyPair("parity-kid")
			endpoints := newTestEndpoints(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(gcvJWKSJSON(kp))
				}),
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
			)
			defer endpoints.close()
			resolver := NewGoogleIdentityResolver(newFakeUserStore(), newMemExtIDStore(), alwaysAuthorized, nil, slog.Default())
			g := newGoogleUserIssuerFixture(t, tc.domains, nil, newTestValidator(endpoints), resolver)
			claims := validIDTokenClaims()
			claims["aud"] = externalBearerTestAudience
			claims["email"] = tc.email
			claims["hd"] = domain
			idToken := signIDToken(kp, claims)
			w, result := doExternalBearerRequest(g.cfg, idToken)
			externalBearer := w.Code == http.StatusOK && result.reached

			// Federation token: a Google user credential in the
			// federation-token header, same issuer configuration.
			fedToken := g.token(t, "google-sub", tc.email)
			rec, reached := g.through(t, fedToken)
			federationToken := rec.Code == http.StatusOK && reached

			// Hub user token: a peer hub user access credential, the same
			// allowed_domains on the peer issuer.
			f.configure(t, func(c *config.TrustedIssuerConfig) { c.AllowedDomains = tc.domains })
			hubToken := f.signAsPeer(t, peerUserClaims("sub-parity", tc.email))
			_, _, hubRec := f.through(t, hubToken)
			hubUserToken := hubRec.Code == http.StatusOK

			assert.Equal(t, tc.admitted, externalBearer, "external bearer: %s", w.Body.String())
			assert.Equal(t, tc.admitted, hubUserToken, "hub user token: %s", hubRec.Body.String())
			assert.Equal(t, tc.admitted, federationToken, "federation token: %s", rec.Body.String())
			if tc.admitted {
				return
			}

			// The refusal cause on each path is allowed_domains.
			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			_, _, ebErr := authenticateExternalBearer(context.Background(), req, idToken, g.cfg)
			assert.True(t, errors.Is(ebErr, errDomainNotAllowed), "external bearer cause: %v", ebErr)

			_, fedErr := g.auth.Authenticate(fedToken)
			require.Error(t, fedErr, "federation token")
			assert.Contains(t, fedErr.Error(), causeDomains, "federation token cause")

			hubAuth := f.srv.federationAuth.Load()
			require.NotNil(t, hubAuth)
			_, hubErr := hubAuth.Authenticate(hubToken, FederationAuthOptions{AcceptHubUserTokens: true})
			require.Error(t, hubErr, "hub user token")
			assert.Contains(t, hubErr.Error(), causeDomains, "hub user token cause")
		})
	}
}
