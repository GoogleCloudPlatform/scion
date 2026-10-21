// Package oidc verifies Google-signed ID tokens, such as the one Cloud
// Scheduler attaches when it calls the mediator's /reconcile endpoint.
//
// The mediator service is publicly reachable (GitHub cannot present Google
// credentials), so /reconcile cannot rely on Cloud Run's invoker check and
// verifies the token itself: RS256 signature against Google's JWKS, issuer,
// audience (MEDIATOR_URL), expiry, and the caller's service-account email.
package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultCertsURL is Google's OAuth2 JWKS endpoint.
const DefaultCertsURL = "https://www.googleapis.com/oauth2/v3/certs"

const (
	clockSkew       = 60 * time.Second
	defaultKeysTTL  = time.Hour
	minRefetchDelay = time.Minute
	maxTokenLen     = 8 << 10
)

var validIssuers = map[string]bool{"https://accounts.google.com": true, "accounts.google.com": true}

// Verifier checks Google ID tokens for one audience and one caller email.
type Verifier struct {
	CertsURL string // default DefaultCertsURL
	HTTP     *http.Client
	Audience string
	Email    string
	Now      func() time.Time

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	keysExpire  time.Time
	lastFetched time.Time
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Verify returns nil if token is a valid Google ID token for v.Audience
// issued to v.Email. Error messages never include the token.
func (v *Verifier) Verify(ctx context.Context, token string) error {
	if v.Audience == "" || v.Email == "" {
		return errors.New("oidc: verifier has no audience or email configured")
	}
	if len(token) > maxTokenLen {
		return errors.New("oidc: token too long")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("oidc: malformed token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return errors.New("oidc: malformed header")
	}
	if header.Alg != "RS256" {
		return fmt.Errorf("oidc: unsupported alg %q", header.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errors.New("oidc: malformed signature")
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return errors.New("oidc: bad signature")
	}

	var c struct {
		Iss           string          `json:"iss"`
		Aud           json.RawMessage `json:"aud"`
		Exp           json.Number     `json:"exp"`
		Iat           json.Number     `json:"iat"`
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
	}
	if err := decodeSegment(parts[1], &c); err != nil {
		return errors.New("oidc: malformed claims")
	}
	if !validIssuers[c.Iss] {
		return fmt.Errorf("oidc: issuer %q is not Google", c.Iss)
	}
	if !audienceMatches(c.Aud, v.Audience) {
		return errors.New("oidc: wrong audience")
	}
	now := v.now()
	exp, err1 := c.Exp.Int64()
	iat, err2 := c.Iat.Int64()
	if err1 != nil || err2 != nil {
		return errors.New("oidc: missing exp or iat")
	}
	if !now.Before(time.Unix(exp, 0).Add(clockSkew)) {
		return errors.New("oidc: token expired")
	}
	if time.Unix(iat, 0).After(now.Add(clockSkew)) {
		return errors.New("oidc: token issued in the future")
	}
	if !strings.EqualFold(c.Email, v.Email) {
		return fmt.Errorf("oidc: caller %q is not the reconciler service account", c.Email)
	}
	if ev := strings.Trim(string(c.EmailVerified), `"`); ev != "true" {
		return errors.New("oidc: email not verified")
	}
	return nil
}

func decodeSegment(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	return d.Decode(out)
}

func audienceMatches(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// key returns the public key for kid, fetching the JWKS when it is stale or
// does not contain kid (at most once a minute for unknown kids).
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if k, ok := v.keys[kid]; ok && now.Before(v.keysExpire) {
		return k, nil
	}
	// Fetch at most once a minute, whether the key set is missing, stale or
	// lacks kid, so unauthenticated callers cannot drive a fetch per request.
	if v.lastFetched.IsZero() || now.Sub(v.lastFetched) >= minRefetchDelay {
		if err := v.fetch(ctx, now); err != nil {
			if k, ok := v.keys[kid]; ok {
				return k, nil // keep serving the last good key set
			}
			return nil, err
		}
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("oidc: unknown signing key")
}

func (v *Verifier) fetch(ctx context.Context, now time.Time) error {
	v.lastFetched = now
	u := v.CertsURL
	if u == "" {
		u = DefaultCertsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	hc := v.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("oidc: fetching JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: fetching JWKS: HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return errors.New("oidc: decoding JWKS failed")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := new(big.Int).SetBytes(e).Int64()
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp)}
	}
	if len(keys) == 0 {
		return errors.New("oidc: JWKS has no RSA keys")
	}
	v.keys = keys
	v.keysExpire = now.Add(maxAge(resp.Header.Get("Cache-Control")))
	return nil
}

func maxAge(cc string) time.Duration {
	for _, d := range strings.Split(cc, ",") {
		d = strings.TrimSpace(d)
		if s, ok := strings.CutPrefix(d, "max-age="); ok {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return defaultKeysTTL
}
