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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const (
	entraTestClientID = "entra-client-id"
	entraTestKID      = "entra-key-1"
	entraTestSub      = "entra-sub-123"
	entraTestEmail    = "First.Last@example.com"
)

// fakeEntraIdP is an OIDC provider that behaves like Microsoft Entra ID:
// its userinfo response has no email_verified claim, and its ID token
// carries the email and xms_edov optional claims.
type fakeEntraIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	// Hooks a test changes before the exchange. idClaims and
	// discoveryIssuer receive the provider's base URL.
	idClaims        func(base string) map[string]interface{}
	userinfo        map[string]interface{}
	discoveryIssuer func(base string) string
	signingKey      *rsa.PrivateKey // nil = key (the published one)
	hmacKey         []byte          // set = sign with HS256 instead
	ecKey           bool            // set = sign with ES256 and a fresh EC key
	kid             string          // "" = entraTestKID
	jwksStatus      int             // 0 = 200
	omitIDToken     bool
}

func newFakeEntraIdP(t *testing.T) *fakeEntraIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	f := &fakeEntraIdP{t: t, key: key}
	f.idClaims = func(base string) map[string]interface{} {
		now := time.Now()
		return map[string]interface{}{
			"iss":      base,
			"aud":      entraTestClientID,
			"sub":      entraTestSub,
			"iat":      now.Unix(),
			"nbf":      now.Unix(),
			"exp":      now.Add(time.Hour).Unix(),
			"email":    entraTestEmail,
			"xms_edov": true,
		}
	}
	f.userinfo = map[string]interface{}{
		"sub":   entraTestSub,
		"email": entraTestEmail,
		"name":  "First Last",
	}
	f.discoveryIssuer = func(base string) string { return base }
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEntraIdP) serve(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.discoveryIssuer(base),
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
			"userinfo_endpoint":      base + "/userinfo",
			"jwks_uri":               base + "/keys",
		})
	case "/keys":
		if f.jwksStatus != 0 {
			w.WriteHeader(f.jwksStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &f.key.PublicKey, KeyID: entraTestKID, Algorithm: string(jose.RS256), Use: "sig",
		}}})
	case "/token":
		resp := map[string]interface{}{
			"access_token": "entra-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if !f.omitIDToken {
			resp["id_token"] = f.sign(f.idClaims(base))
		}
		_ = json.NewEncoder(w).Encode(resp)
	case "/userinfo":
		if r.Header.Get("Authorization") != "Bearer entra-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(f.userinfo)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeEntraIdP) sign(claims map[string]interface{}) string {
	f.t.Helper()
	kid := entraTestKID
	if f.kid != "" {
		kid = f.kid
	}
	var sk jose.SigningKey
	switch {
	case f.hmacKey != nil:
		sk = jose.SigningKey{Algorithm: jose.HS256, Key: jose.JSONWebKey{Key: f.hmacKey, KeyID: kid}}
	case f.ecKey:
		ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			f.t.Fatalf("ec key: %v", err)
		}
		sk = jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: ec, KeyID: kid}}
	default:
		k := f.key
		if f.signingKey != nil {
			k = f.signingKey
		}
		sk = jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k, KeyID: kid}}
	}
	signer, err := jose.NewSigner(sk, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		f.t.Fatalf("signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	return raw
}

func (f *fakeEntraIdP) service() *OAuthService {
	return NewOAuthService(OAuthConfig{}, &config.OIDCLoginConfig{
		Enabled:      true,
		IssuerURL:    f.srv.URL,
		ClientID:     entraTestClientID,
		ClientSecret: "entra-secret",
	})
}

func (f *fakeEntraIdP) exchange() (*OAuthUserInfo, error) {
	return f.service().exchangeOIDCCode(context.Background(), "code", "https://hub.example.com/auth/callback/oidc")
}

// editClaims wraps the provider's ID token claims with edit.
func (f *fakeEntraIdP) editClaims(edit func(c map[string]interface{})) {
	base := f.idClaims
	f.idClaims = func(b string) map[string]interface{} {
		c := base(b)
		edit(c)
		return c
	}
}

// TestOIDCLogin_EntraIDTokenVouchesForEmail covers the Entra ID shape:
// userinfo has no email_verified, and the ID token carries the same email
// with xms_edov.
func TestOIDCLogin_EntraIDTokenVouchesForEmail(t *testing.T) {
	f := newFakeEntraIdP(t)
	u, err := f.exchange()
	if err != nil {
		t.Fatalf("exchangeOIDCCode() = %v, want sign-in", err)
	}
	if u.ID != entraTestSub || u.Email != entraTestEmail || u.DisplayName != "First Last" || u.Provider != "oidc" {
		t.Errorf("user = %+v", u)
	}
}

func TestOIDCLogin_IDTokenEmailVerifiedClaimVouches(t *testing.T) {
	for _, v := range []interface{}{true, "true", "TRUE", "1", 1} {
		f := newFakeEntraIdP(t)
		f.editClaims(func(c map[string]interface{}) {
			delete(c, "xms_edov")
			c["email_verified"] = v
		})
		if _, err := f.exchange(); err != nil {
			t.Errorf("email_verified=%#v: exchangeOIDCCode() = %v, want sign-in", v, err)
		}
	}
}

func TestOIDCLogin_IDTokenEmailMatchIsCaseInsensitive(t *testing.T) {
	f := newFakeEntraIdP(t)
	f.userinfo["email"] = strings.ToLower(entraTestEmail)
	u, err := f.exchange()
	if err != nil {
		t.Fatalf("exchangeOIDCCode() = %v, want sign-in", err)
	}
	if u.Email != strings.ToLower(entraTestEmail) {
		t.Errorf("Email = %q, want the userinfo address", u.Email)
	}
}

// TestOIDCLogin_IDTokenRefusals: every way the ID token can fail to vouch
// leaves the sign-in refused, and the error says why.
func TestOIDCLogin_IDTokenRefusals(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	claims := func(edit func(c map[string]interface{})) func(f *fakeEntraIdP) {
		return func(f *fakeEntraIdP) { f.editClaims(edit) }
	}
	cases := []struct {
		name  string
		setup func(f *fakeEntraIdP)
		want  string
	}{
		{"no ID token", func(f *fakeEntraIdP) { f.omitIDToken = true }, "no ID token"},
		{"signed by an unpublished key", func(f *fakeEntraIdP) { f.signingKey = otherKey }, "signature or claims are invalid"},
		{"HMAC signed", func(f *fakeEntraIdP) { f.hmacKey = []byte("entra-secret-entra-secret-entra-secret") }, "not a supported signed JWT"},
		{"wrong issuer", claims(func(c map[string]interface{}) { c["iss"] = "https://evil.example.com" }), "not valid for this client"},
		{"wrong audience", claims(func(c map[string]interface{}) { c["aud"] = "some-other-app" }), "not valid for this client"},
		{"expired", claims(func(c map[string]interface{}) {
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
			c["nbf"] = time.Now().Add(-2 * time.Hour).Unix()
		}), "not valid for this client"},
		{"no exp", claims(func(c map[string]interface{}) { delete(c, "exp") }), "no exp claim"},
		{"several audiences without azp", claims(func(c map[string]interface{}) {
			c["aud"] = []string{entraTestClientID, "another-app"}
		}), "several audiences and no azp"},
		{"azp names another client", claims(func(c map[string]interface{}) { c["azp"] = "another-app" }), "azp is not this client"},
		{"not yet valid", claims(func(c map[string]interface{}) {
			c["nbf"] = time.Now().Add(time.Hour).Unix()
		}), "not valid for this client"},
		{"unknown kid", func(f *fakeEntraIdP) { f.kid = "rotated-away" }, "no signing key"},
		{"ES256 header against the RSA key", func(f *fakeEntraIdP) { f.ecKey = true }, "signature or claims are invalid"},
		{"JWKS endpoint failing", func(f *fakeEntraIdP) { f.jwksStatus = http.StatusInternalServerError }, "no signing key"},
		{"non-ASCII case fold", claims(func(c map[string]interface{}) {
			c["email"] = strings.Replace(entraTestEmail, "s", "\u017f", 1) // long s folds to s under strings.EqualFold
		}), "email does not match"},
		{"different subject", claims(func(c map[string]interface{}) { c["sub"] = "someone-else" }), "subject does not match"},
		{"different email", claims(func(c map[string]interface{}) { c["email"] = "attacker@example.com" }), "email does not match"},
		{"no email claim", claims(func(c map[string]interface{}) { delete(c, "email") }), "no email claim"},
		{"xms_edov false", claims(func(c map[string]interface{}) { c["xms_edov"] = false }), "neither email_verified nor xms_edov"},
		{"xms_edov missing", claims(func(c map[string]interface{}) { delete(c, "xms_edov") }), "neither email_verified nor xms_edov"},
		{"ID token email_verified false overrides xms_edov", claims(func(c map[string]interface{}) {
			c["email_verified"] = false
			c["xms_edov"] = true
		}), "neither email_verified nor xms_edov"},
		{"discovery issuer differs from issuer_url", func(f *fakeEntraIdP) {
			f.discoveryIssuer = func(string) string { return "https://login.example.com/other-tenant/v2.0" }
		}, "does not match issuer_url"},
		{"discovery document without issuer", func(f *fakeEntraIdP) {
			f.discoveryIssuer = func(string) string { return "" }
		}, "discovery document has no issuer"},
		{"userinfo email_verified null", func(f *fakeEntraIdP) { f.userinfo["email_verified"] = nil }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEntraIdP(t)
			tc.setup(f)
			_, err := f.exchange()
			if err == nil {
				t.Fatal("exchangeOIDCCode() signed in, want refusal")
			}
			if !strings.Contains(err.Error(), "unverified email") {
				t.Errorf("error = %v, want the unverified-email refusal", err)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestOIDCLogin_UserinfoTrueIgnoresIDToken: when userinfo states
// email_verified=true the ID token is never consulted, so even an invalid
// one does not affect providers that already send the claim
// (Keycloak-shaped regression check).
func TestOIDCLogin_UserinfoTrueIgnoresIDToken(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeEntraIdP(t)
	f.userinfo["email_verified"] = true
	f.signingKey = otherKey
	f.editClaims(func(c map[string]interface{}) { c["aud"] = "some-other-app" })
	if _, err := f.exchange(); err != nil {
		t.Fatalf("exchangeOIDCCode() = %v, want sign-in", err)
	}
}

// TestOIDCLogin_IssuerURLTrailingSlash: issuer_url with a trailing slash
// still matches the discovered issuer and the token's iss.
func TestOIDCLogin_IssuerURLTrailingSlash(t *testing.T) {
	f := newFakeEntraIdP(t)
	svc := NewOAuthService(OAuthConfig{}, &config.OIDCLoginConfig{
		Enabled: true, IssuerURL: f.srv.URL + "/", ClientID: entraTestClientID, ClientSecret: "entra-secret",
	})
	if _, err := svc.exchangeOIDCCode(context.Background(), "code", "https://hub.example.com/auth/callback/oidc"); err != nil {
		t.Fatalf("exchangeOIDCCode() = %v, want sign-in", err)
	}
}

// TestOIDCIDTokenKeySource_Concurrent exercises the key source cache from
// many goroutines (run with -race).
func TestOIDCIDTokenKeySource_Concurrent(t *testing.T) {
	svc := newFakeEntraIdP(t).service()
	done := make(chan jwtKeySource, 16)
	for i := 0; i < 16; i++ {
		go func() {
			ks, err := svc.oidcIDTokenKeySource("https://idp.example.com/keys")
			if err != nil {
				t.Error(err)
			}
			done <- ks
		}()
	}
	first := <-done
	for i := 1; i < 16; i++ {
		if ks := <-done; ks != first {
			t.Error("concurrent callers got different key sources")
		}
	}
}

func TestASCIIEqualFold(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"First.Last@Example.com", "first.last@example.com", true},
		{"a@example.com", "a@example.com", true},
		{"a@example.com", "ab@example.com", false},
		{"\u212a@example.com", "k@example.com", false},
		{"\u00c9@example.com", "\u00e9@example.com", false},
	} {
		if got := asciiEqualFold(tc.a, tc.b); got != tc.want {
			t.Errorf("asciiEqualFold(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestOIDCLogin_SeveralAudiencesWithAZP: several audiences are accepted
// when azp names this client.
func TestOIDCLogin_SeveralAudiencesWithAZP(t *testing.T) {
	f := newFakeEntraIdP(t)
	f.editClaims(func(c map[string]interface{}) {
		c["aud"] = []string{entraTestClientID, "another-app"}
		c["azp"] = entraTestClientID
	})
	if _, err := f.exchange(); err != nil {
		t.Fatalf("exchangeOIDCCode() = %v, want sign-in", err)
	}
}

// TestOIDCLogin_UserinfoFalseWinsOverIDToken: a provider that says
// email_verified=false in userinfo is refused even when the ID token says
// true; the ID token is consulted only when the claim is absent.
func TestOIDCLogin_UserinfoFalseWinsOverIDToken(t *testing.T) {
	f := newFakeEntraIdP(t)
	f.userinfo["email_verified"] = false
	f.editClaims(func(c map[string]interface{}) { c["email_verified"] = true })
	if _, err := f.exchange(); err == nil || !strings.Contains(err.Error(), "unverified email") {
		t.Fatalf("exchangeOIDCCode() = %v, want unverified-email refusal", err)
	}
}

// TestOIDCLogin_UserinfoTrueNeedsNoIDToken: providers that state
// email_verified=true in userinfo keep working with no ID token at all.
func TestOIDCLogin_UserinfoTrueNeedsNoIDToken(t *testing.T) {
	f := newFakeEntraIdP(t)
	f.userinfo["email_verified"] = true
	f.omitIDToken = true
	if _, err := f.exchange(); err != nil {
		t.Fatalf("exchangeOIDCCode() = %v, want sign-in", err)
	}
}

// TestOIDCUserInfo_AbsentEmailVerifiedWithoutIDTokenRefused: the direct
// userinfo path (no ID token to consult) refuses an absent claim, as before.
func TestOIDCUserInfo_AbsentEmailVerifiedWithoutIDTokenRefused(t *testing.T) {
	f := newFakeEntraIdP(t)
	_, err := f.service().getOIDCUserInfo(context.Background(), "entra-access-token", f.srv.URL+"/userinfo")
	if err == nil || !strings.Contains(err.Error(), "unverified email") {
		t.Fatalf("getOIDCUserInfo() = %v, want unverified-email refusal", err)
	}
}

func TestOIDCIDTokenKeySource(t *testing.T) {
	svc := newFakeEntraIdP(t).service()
	if _, err := svc.oidcIDTokenKeySource("http://idp.example.com/keys"); err == nil {
		t.Error("oidcIDTokenKeySource(http non-loopback) = nil error, want refusal")
	}
	if _, err := svc.oidcIDTokenKeySource(""); err == nil {
		t.Error(`oidcIDTokenKeySource("") = nil error, want refusal`)
	}
	a, err := svc.oidcIDTokenKeySource("https://idp.example.com/keys")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := svc.oidcIDTokenKeySource("https://idp.example.com/keys")
	if a != b {
		t.Error("key source not reused for the same jwks_uri")
	}
	c, _ := svc.oidcIDTokenKeySource("https://idp.example.com/other-keys")
	if c == a {
		t.Error("key source reused after jwks_uri changed")
	}
}

func TestClaimIsTrue(t *testing.T) {
	for _, tc := range []struct {
		v    interface{}
		want bool
	}{
		{true, true}, {false, false}, {"true", true}, {"True", true}, {"1", true},
		{"false", false}, {"0", false}, {"yes", false}, {float64(1), true},
		{float64(0), false}, {nil, false}, {[]interface{}{true}, false},
	} {
		if got := claimIsTrue(tc.v); got != tc.want {
			t.Errorf("claimIsTrue(%#v) = %v, want %v", tc.v, got, tc.want)
		}
	}
}
