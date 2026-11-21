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
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// oidcIDTokenAlgorithms are the signature algorithms accepted on an OIDC
// login ID token. Only asymmetric algorithms: an HMAC-signed ID token would
// be keyed with the client secret, which proves nothing about the provider.
var oidcIDTokenAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
}

// oidcIDTokenLeeway is the clock skew allowed on exp, nbf and iat.
const oidcIDTokenLeeway = time.Minute

// oidcIDTokenEmailClaims are the ID token claims read beyond the registered
// ones. email_verified is the standard claim; xms_edov is Microsoft Entra
// ID's "email domain owner verified" optional claim. Both are decoded
// loosely (see claimIsTrue) because providers differ in how they encode
// booleans.
type oidcIDTokenEmailClaims struct {
	Email         string      `json:"email"`
	EmailVerified interface{} `json:"email_verified"`
	XMSEdov       interface{} `json:"xms_edov"`
	AZP           string      `json:"azp"`
}

// oidcIDTokenVouchesForEmail reports whether the OIDC ID token rawIDToken,
// returned by the same token exchange as the userinfo response, vouches for
// email as the verified address of the user sub. It returns the claim that
// vouched ("email_verified" or "xms_edov"), or an error saying why the token
// does not vouch.
//
// It is consulted only when the userinfo response omits email_verified
// (Microsoft Entra ID's userinfo endpoint never sends it and cannot be
// customised). The token must verify against the issuer's published keys,
// name the configured issuer and this client, be current, and belong to the
// same subject as the userinfo response; its email must equal the userinfo
// email.
//
// No nonce is checked: the hub sends none, and the ID token arrives directly
// from the token endpoint over TLS in answer to a confidential client's
// code exchange, which OpenID Connect Core 1.0 section 3.1.3.7 allows to
// stand in for further validation of the token's origin.
func (s *OAuthService) oidcIDTokenVouchesForEmail(rawIDToken string, doc *OIDCDiscoveryDoc, sub, email string) (string, error) {
	if s.oidcLoginConfig == nil {
		return "", errors.New("OIDC login is not configured")
	}
	if strings.TrimSpace(rawIDToken) == "" {
		return "", errors.New("the token response carried no ID token")
	}
	if doc == nil {
		return "", errors.New("no OIDC discovery document")
	}

	// OpenID Connect Discovery 1.0 section 4.3: the discovered issuer must
	// equal the issuer the configuration was fetched for.
	// The issuer is required in a discovery document (section 3).
	configured := strings.TrimRight(s.oidcLoginConfig.IssuerURL, "/")
	expectedIssuer := doc.Issuer
	if expectedIssuer == "" {
		return "", errors.New("the provider's discovery document has no issuer")
	}
	if strings.TrimRight(expectedIssuer, "/") != configured {
		return "", fmt.Errorf("discovery issuer %q does not match issuer_url %q", upstreamText(doc.Issuer), s.oidcLoginConfig.IssuerURL)
	}

	keys, err := s.oidcIDTokenKeySource(doc.JWKSURI)
	if err != nil {
		return "", err
	}

	tok, err := jwt.ParseSigned(rawIDToken, oidcIDTokenAlgorithms)
	if err != nil {
		return "", fmt.Errorf("ID token is not a supported signed JWT: %w", err)
	}
	if len(tok.Headers) != 1 {
		return "", errors.New("ID token must carry exactly one signature")
	}
	key, err := keys.GetKey(tok.Headers[0].KeyID)
	if err != nil {
		return "", fmt.Errorf("no signing key for the ID token: %w", err)
	}

	var std jwt.Claims
	var extra oidcIDTokenEmailClaims
	if err := tok.Claims(key, &std, &extra); err != nil {
		return "", fmt.Errorf("ID token signature or claims are invalid: %w", err)
	}
	if std.Expiry == nil {
		return "", errors.New("ID token has no exp claim")
	}
	clientID := s.oidcLoginConfig.ClientID
	if err := std.ValidateWithLeeway(jwt.Expected{
		Issuer:      expectedIssuer,
		AnyAudience: jwt.Audience{clientID},
		Time:        time.Now(),
	}, oidcIDTokenLeeway); err != nil {
		return "", fmt.Errorf("ID token is not valid for this client: %w", err)
	}
	// OpenID Connect Core 1.0 section 3.1.3.7: with several audiences the
	// token must name this client as its authorized party, and an azp that
	// is present must be this client.
	if len(std.Audience) > 1 && extra.AZP == "" {
		return "", errors.New("ID token has several audiences and no azp")
	}
	if extra.AZP != "" && extra.AZP != clientID {
		return "", errors.New("ID token azp is not this client")
	}
	// OpenID Connect Core 1.0 section 5.3.2: the userinfo sub must match
	// the ID token sub.
	if std.Subject == "" || std.Subject != sub {
		return "", errors.New("ID token subject does not match the userinfo subject")
	}

	if strings.TrimSpace(extra.Email) == "" {
		return "", errors.New("ID token has no email claim (for Microsoft Entra ID, add the email optional claim to the ID token)")
	}
	if !asciiEqualFold(strings.TrimSpace(extra.Email), strings.TrimSpace(email)) {
		return "", errors.New("ID token email does not match the userinfo email")
	}
	switch {
	case claimIsTrue(extra.EmailVerified):
		return "email_verified", nil
	case extra.EmailVerified == nil && claimIsTrue(extra.XMSEdov):
		return "xms_edov", nil
	}
	return "", errors.New("ID token marks the address verified with neither email_verified nor xms_edov (for Microsoft Entra ID, add the xms_edov optional claim to the ID token)")
}

// oidcIDTokenKeySource returns the key source for jwksURI, reusing the
// cached one while the discovered jwks_uri is unchanged. The source fetches
// lazily and refreshes on an unknown key ID (jwksCache).
func (s *OAuthService) oidcIDTokenKeySource(jwksURI string) (jwtKeySource, error) {
	if jwksURI == "" {
		return nil, errors.New("the provider's discovery document has no jwks_uri")
	}
	if err := requireHTTPSOrLoopback(jwksURI); err != nil {
		return nil, fmt.Errorf("jwks_uri: %w", err)
	}
	s.oidcIDTokenKeysMu.Lock()
	defer s.oidcIDTokenKeysMu.Unlock()
	if s.oidcIDTokenKeys == nil || s.oidcIDTokenKeysURL != jwksURI {
		s.oidcIDTokenKeys = NewJWKSURLKeySource(jwksURI)
		s.oidcIDTokenKeysURL = jwksURI
	}
	return s.oidcIDTokenKeys, nil
}

// requireHTTPSOrLoopback accepts an https URL, or an http URL whose host is
// localhost or 127.0.0.1 (local development), matching the rule for the
// other discovered OIDC endpoints.
func requireHTTPSOrLoopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", upstreamText(raw), err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if host := u.Hostname(); u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1") {
		return nil
	}
	return fmt.Errorf("%q must use https", upstreamText(raw))
}

// asciiEqualFold reports whether a and b are equal ignoring ASCII case
// only. Unlike strings.EqualFold it does not fold non-ASCII letters (the
// Kelvin sign K does not match "k"), so two addresses match only when they
// differ in ASCII letter case.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// claimIsTrue reports whether a decoded JSON claim means true: the boolean
// true, the string "true" (any case) or "1", or the number 1.
func claimIsTrue(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true") || t == "1"
	case float64:
		return t == 1
	}
	return false
}
