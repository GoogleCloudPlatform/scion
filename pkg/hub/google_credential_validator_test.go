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
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ---------------------------------------------------------------------------
// Production Google credential validator tests.
//
// These tests use httptest.Server + real NewGoogleCredentialValidator with a
// pinned fake network transport. They exercise the real RS256 signature
// verification, JWKS cache, tokeninfo/userinfo cross-check, and all fail-closed
// policy paths through the production implementation — not a mock interface.
// ---------------------------------------------------------------------------

// gcvTestKeyPair holds an RSA key pair and its JWKS kid for test JWT signing.
type gcvTestKeyPair struct {
	key *rsa.PrivateKey
	kid string
}

// newGCVTestKeyPair generates a fresh RSA-2048 key pair for testing.
func newGCVTestKeyPair(kid string) *gcvTestKeyPair {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(fmt.Sprintf("generate RSA key: %v", err))
	}
	return &gcvTestKeyPair{key: key, kid: kid}
}

// gcvJWKSJSON returns the JWKS JSON with the public key(s).
func gcvJWKSJSON(keys ...*gcvTestKeyPair) []byte {
	jwks := jose.JSONWebKeySet{}
	for _, kp := range keys {
		jwks.Keys = append(jwks.Keys, jose.JSONWebKey{
			Key:       &kp.key.PublicKey,
			KeyID:     kp.kid,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		})
	}
	data, err := json.Marshal(jwks)
	if err != nil {
		panic(fmt.Sprintf("marshal JWKS: %v", err))
	}
	return data
}

// signIDToken creates a signed JWT with the given claims using RS256.
func signIDToken(kp *gcvTestKeyPair, claims map[string]interface{}) string {
	signerOpts := (&jose.SignerOptions{}).WithType("JWT")
	signerOpts.WithHeader("kid", kp.kid)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: kp.key},
		signerOpts,
	)
	if err != nil {
		panic(fmt.Sprintf("create signer: %v", err))
	}

	data, err := json.Marshal(claims)
	if err != nil {
		panic(fmt.Sprintf("marshal claims: %v", err))
	}

	obj, err := signer.Sign(data)
	if err != nil {
		panic(fmt.Sprintf("sign JWT: %v", err))
	}

	serialized, err := obj.CompactSerialize()
	if err != nil {
		panic(fmt.Sprintf("serialize JWT: %v", err))
	}
	return serialized
}

// validIDTokenClaims returns valid Google ID token claims for testing.
func validIDTokenClaims() map[string]interface{} {
	now := time.Now()
	return map[string]interface{}{
		"iss":            googleIssuerHTTPS,
		"sub":            "google-sub-test-123",
		"aud":            "test-client-id.apps.googleusercontent.com",
		"email":          "user@gmail.com",
		"email_verified": true,
		"name":           "Test User",
		"picture":        "https://example.com/photo.jpg",
		"exp":            now.Add(30 * time.Minute).Unix(),
		"iat":            now.Unix(),
		"nbf":            now.Add(-1 * time.Minute).Unix(),
	}
}

// testEndpoints holds httptest servers for the Google API endpoints.
type testEndpoints struct {
	jwksServer      *httptest.Server
	tokenInfoServer *httptest.Server
	userInfoServer  *httptest.Server
}

// newTestEndpoints creates httptest servers with the given handlers.
func newTestEndpoints(jwksHandler, tokenInfoHandler, userInfoHandler http.Handler) *testEndpoints {
	return &testEndpoints{
		jwksServer:      httptest.NewServer(jwksHandler),
		tokenInfoServer: httptest.NewServer(tokenInfoHandler),
		userInfoServer:  httptest.NewServer(userInfoHandler),
	}
}

func (e *testEndpoints) close() {
	e.jwksServer.Close()
	e.tokenInfoServer.Close()
	e.userInfoServer.Close()
}

// googleURLRewriter is an http.RoundTripper that intercepts requests to
// Google's pinned API URLs and redirects them to local httptest servers.
type googleURLRewriter struct {
	jwksURL      string
	tokenInfoURL string
	userInfoURL  string
	transport    http.RoundTripper
}

func (r *googleURLRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	switch {
	case strings.HasPrefix(url, googleJWKSURL):
		req = req.Clone(req.Context())
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(r.jwksURL, "http://")
		req.URL.Path = "/"
	case strings.HasPrefix(url, googleTokenInfoURL):
		req = req.Clone(req.Context())
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(r.tokenInfoURL, "http://")
		req.URL.Path = "/"
	case strings.HasPrefix(url, googleUserInfoURL):
		req = req.Clone(req.Context())
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(r.userInfoURL, "http://")
		req.URL.Path = "/"
	}
	if r.transport != nil {
		return r.transport.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// newTestValidator creates a production validator wired to local test servers.
func newTestValidator(endpoints *testEndpoints) GoogleCredentialValidator {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("redirect not allowed to pinned Google endpoint: %s", req.URL)
		},
		Transport: &googleURLRewriter{
			jwksURL:      endpoints.jwksServer.URL,
			tokenInfoURL: endpoints.tokenInfoServer.URL,
			userInfoURL:  endpoints.userInfoServer.URL,
		},
	}
	return NewGoogleCredentialValidator(client)
}

// ---------------------------------------------------------------------------
// ID Token tests — cryptographic signature verification through production code
// ---------------------------------------------------------------------------

func TestProductionValidator_IDToken_ValidSignature(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("tokeninfo should not be called for ID token validation")
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("userinfo should not be called for ID token validation")
		}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	claims := validIDTokenClaims()
	token := signIDToken(kp, claims)

	identity, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("ValidateIDToken failed: %v", err)
	}

	if identity.Subject != "google-sub-test-123" {
		t.Errorf("subject = %q, want %q", identity.Subject, "google-sub-test-123")
	}
	if identity.Email != "user@gmail.com" {
		t.Errorf("email = %q, want %q", identity.Email, "user@gmail.com")
	}
	if !identity.EmailVerified {
		t.Error("expected email_verified=true")
	}
	if identity.DisplayName != "Test User" {
		t.Errorf("displayName = %q, want %q", identity.DisplayName, "Test User")
	}
	if identity.Issuer != googleCanonicalIssuer {
		t.Errorf("issuer = %q, want %q", identity.Issuer, googleCanonicalIssuer)
	}
	if identity.UpstreamExpiry.IsZero() {
		t.Error("expected non-zero upstream expiry")
	}
}

func TestProductionValidator_IDToken_InvalidSignature(t *testing.T) {
	kp1 := newGCVTestKeyPair("signing-key")
	kp2 := newGCVTestKeyPair("wrong-key")

	// JWKS serves kp2's public key, but token is signed with kp1.
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp2)); err != nil { // wrong key
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	token := signIDToken(kp1, validIDTokenClaims())

	_, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for invalid signature")
	}
	if !strings.Contains(err.Error(), "signature verification failed") {
		t.Errorf("error = %q, expected signature verification failure", err)
	}
}

func TestProductionValidator_IDToken_MissingExp(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	claims := validIDTokenClaims()
	delete(claims, "exp") // Remove exp claim
	token := signIDToken(kp, claims)

	_, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for missing exp claim")
	}
	if !strings.Contains(err.Error(), "missing exp") {
		t.Errorf("error = %q, expected missing exp message", err)
	}
}

func TestProductionValidator_IDToken_ExpiredToken(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	claims := validIDTokenClaims()
	claims["exp"] = time.Now().Add(-10 * time.Minute).Unix() // Expired 10 minutes ago
	claims["iat"] = time.Now().Add(-40 * time.Minute).Unix()
	claims["nbf"] = time.Now().Add(-40 * time.Minute).Unix()
	token := signIDToken(kp, claims)

	_, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestProductionValidator_IDToken_WrongAudience(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	claims := validIDTokenClaims()
	claims["aud"] = "wrong-client-id.apps.googleusercontent.com"
	token := signIDToken(kp, claims)

	_, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for wrong audience")
	}
	if !strings.Contains(err.Error(), "untrusted") || !strings.Contains(err.Error(), "audience") {
		t.Errorf("error = %q, expected untrusted audience message", err)
	}
}

func TestProductionValidator_IDToken_ServiceAccount(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	claims := validIDTokenClaims()
	claims["email"] = "sa@my-project.iam.gserviceaccount.com"
	token := signIDToken(kp, claims)

	// The validator classifies service accounts but does not reject them —
	// rejection (or admission, under other policy) is the caller's
	// responsibility (design §4.2(i)).
	identity, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("ValidateIDToken failed: %v", err)
	}
	if !identity.IsServiceAccount {
		t.Error("expected IsServiceAccount=true for a service-account email")
	}
}

func TestProductionValidator_IDToken_UnverifiedEmail(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	claims := validIDTokenClaims()
	claims["email_verified"] = false
	token := signIDToken(kp, claims)

	_, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for unverified email")
	}
	if !strings.Contains(err.Error(), "not verified") {
		t.Errorf("error = %q, expected email not verified message", err)
	}
}

func TestProductionValidator_IDToken_JWKSForceRefresh(t *testing.T) {
	kp1 := newGCVTestKeyPair("old-kid")
	kp2 := newGCVTestKeyPair("new-kid") // New key after rotation

	callCount := 0
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.Header().Set("Content-Type", "application/json")
			if callCount == 1 {
				// First call: only old key
				if _, err := w.Write(gcvJWKSJSON(kp1)); err != nil {
					t.Errorf("write initial JWKS response: %v", err)
				}
			} else {
				// Force refresh: include new key
				if _, err := w.Write(gcvJWKSJSON(kp1, kp2)); err != nil {
					t.Errorf("write refreshed JWKS response: %v", err)
				}
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	// Sign with old key first to warm the cache.
	token1 := signIDToken(kp1, validIDTokenClaims())
	_, err := validator.ValidateIDToken(t.Context(), token1,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("first validation failed: %v", err)
	}

	// Push the cache timestamp back beyond the 30-second force-refresh rate limit
	// so that the rotation test can actually trigger a refetch.
	if gcv, ok := validator.(*googleCredentialValidator); ok {
		gcv.jwksCache.mu.Lock()
		gcv.jwksCache.fetchedAt = time.Now().Add(-1 * time.Minute)
		gcv.jwksCache.mu.Unlock()
	}

	// Sign with new key — cache only has old key. Should force-refresh and succeed.
	token2 := signIDToken(kp2, validIDTokenClaims())
	identity, err := validator.ValidateIDToken(t.Context(), token2,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("validation after JWKS rotation failed: %v", err)
	}
	if identity.Subject != "google-sub-test-123" {
		t.Errorf("subject after rotation = %q, want %q", identity.Subject, "google-sub-test-123")
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 JWKS fetches (initial + force refresh), got %d", callCount)
	}
}

func TestProductionValidator_IDToken_JWKSRedirectRejected(t *testing.T) {
	// JWKS server returns a 302 redirect. The production validator's httpClient
	// has CheckRedirect that rejects redirects to pinned endpoints.
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://evil.example.com/jwks", http.StatusFound)
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	kp := newGCVTestKeyPair("test-kid")
	token := signIDToken(kp, validIDTokenClaims())

	_, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error when JWKS endpoint redirects")
	}
	// The error chain should include the redirect rejection or a fetch failure.
}

func TestProductionValidator_IDToken_BareIssuerAccepted(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	claims := validIDTokenClaims()
	claims["iss"] = googleIssuerBare // "accounts.google.com" (bare form)
	token := signIDToken(kp, claims)

	identity, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("bare issuer should be accepted: %v", err)
	}
	// Canonical issuer should be used in the result.
	if identity.Issuer != googleCanonicalIssuer {
		t.Errorf("issuer = %q, want canonical %q", identity.Issuer, googleCanonicalIssuer)
	}
}

func TestProductionValidator_IDToken_AudAzpDisagreement(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	claims := validIDTokenClaims()
	claims["aud"] = "client-A.apps.googleusercontent.com"
	claims["azp"] = "client-B.apps.googleusercontent.com" // disagrees with aud
	token := signIDToken(kp, claims)

	_, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"client-A.apps.googleusercontent.com", "client-B.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for aud/azp disagreement — even if both are individually allowed")
	}
	if !strings.Contains(err.Error(), "disagree") || !strings.Contains(err.Error(), "differs") {
		t.Errorf("error = %q, expected field disagreement message", err)
	}
}

// ---------------------------------------------------------------------------
// Access Token tests — tokeninfo/userinfo through production code
// ---------------------------------------------------------------------------

func TestProductionValidator_AccessToken_ValidExchange(t *testing.T) {
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("JWKS should not be called for access token validation")
		}),
		// tokeninfo endpoint
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("tokeninfo method = %s, want POST", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"azp":            "test-client-id.apps.googleusercontent.com",
				"aud":            "test-client-id.apps.googleusercontent.com",
				"sub":            "google-sub-access-456",
				"email":          "user@gmail.com",
				"email_verified": "true", // String form — tests flexBool
				"expires_in":     3600,
				"scope":          "openid email profile",
				"access_type":    "online",
			}); err != nil {
				t.Errorf("encode tokeninfo response: %v", err)
			}
		}),
		// userinfo endpoint
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"sub":            "google-sub-access-456",
				"email":          "user@gmail.com",
				"email_verified": true, // Boolean form
				"name":           "Access User",
				"picture":        "https://example.com/photo.jpg",
			}); err != nil {
				t.Errorf("encode userinfo response: %v", err)
			}
		}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	identity, err := validator.ValidateAccessToken(t.Context(), "test-access-token",
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("ValidateAccessToken failed: %v", err)
	}

	if identity.Subject != "google-sub-access-456" {
		t.Errorf("subject = %q, want %q", identity.Subject, "google-sub-access-456")
	}
	if identity.Email != "user@gmail.com" {
		t.Errorf("email = %q, want %q", identity.Email, "user@gmail.com")
	}
	if identity.Audience != "test-client-id.apps.googleusercontent.com" {
		t.Errorf("audience = %q, want %q", identity.Audience, "test-client-id.apps.googleusercontent.com")
	}
	if identity.DisplayName != "Access User" {
		t.Errorf("displayName = %q, want %q", identity.DisplayName, "Access User")
	}
}

func TestProductionValidator_AccessToken_AzpAudDisagreement(t *testing.T) {
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"azp":        "client-A.apps.googleusercontent.com",
				"aud":        "client-B.apps.googleusercontent.com", // disagreement
				"sub":        "sub-1",
				"email":      "user@gmail.com",
				"expires_in": 3600,
			}); err != nil {
				t.Errorf("encode tokeninfo response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	_, err := validator.ValidateAccessToken(t.Context(), "token",
		[]string{"client-A.apps.googleusercontent.com", "client-B.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for aud/azp disagreement — even if both individually allowed")
	}
	if !strings.Contains(err.Error(), "differs") {
		t.Errorf("error = %q, expected field disagreement message", err)
	}
}

func TestProductionValidator_AccessToken_MissingAzp(t *testing.T) {
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				// No azp field
				"aud":        "test-client-id.apps.googleusercontent.com",
				"sub":        "sub-1",
				"email":      "user@gmail.com",
				"expires_in": 3600,
			}); err != nil {
				t.Errorf("encode tokeninfo response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	_, err := validator.ValidateAccessToken(t.Context(), "token",
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for missing azp")
	}
	if !strings.Contains(err.Error(), "azp") {
		t.Errorf("error = %q, expected azp-related message", err)
	}
}

func TestProductionValidator_AccessToken_SubDisagreement(t *testing.T) {
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"azp":        "test-client-id.apps.googleusercontent.com",
				"aud":        "test-client-id.apps.googleusercontent.com",
				"sub":        "sub-from-tokeninfo",
				"email":      "user@gmail.com",
				"expires_in": 3600,
			}); err != nil {
				t.Errorf("encode tokeninfo response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"sub":            "sub-from-userinfo", // different!
				"email":          "user@gmail.com",
				"email_verified": true,
				"name":           "User",
			}); err != nil {
				t.Errorf("encode userinfo response: %v", err)
			}
		}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	_, err := validator.ValidateAccessToken(t.Context(), "token",
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for sub disagreement between tokeninfo and userinfo")
	}
	if !strings.Contains(err.Error(), "disagree") || !strings.Contains(err.Error(), "sub") {
		t.Errorf("error = %q, expected sub disagreement message", err)
	}
}

func TestProductionValidator_AccessToken_ExpiredToken(t *testing.T) {
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"azp":        "test-client-id.apps.googleusercontent.com",
				"aud":        "test-client-id.apps.googleusercontent.com",
				"sub":        "sub-1",
				"expires_in": 0, // no remaining lifetime
			}); err != nil {
				t.Errorf("encode tokeninfo response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	_, err := validator.ValidateAccessToken(t.Context(), "token",
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for expired access token")
	}
	if !strings.Contains(err.Error(), "lifetime") {
		t.Errorf("error = %q, expected no remaining lifetime message", err)
	}
}

func TestProductionValidator_AccessToken_ServiceAccount(t *testing.T) {
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"azp":        "test-client-id.apps.googleusercontent.com",
				"aud":        "test-client-id.apps.googleusercontent.com",
				"sub":        "sa-sub",
				"email":      "sa@my-project.iam.gserviceaccount.com",
				"expires_in": 3600,
			}); err != nil {
				t.Errorf("encode tokeninfo response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"sub":            "sa-sub",
				"email":          "sa@my-project.iam.gserviceaccount.com",
				"email_verified": true,
			}); err != nil {
				t.Errorf("encode userinfo response: %v", err)
			}
		}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	// The validator classifies service accounts but does not reject them —
	// rejection (or admission, under other policy) is the caller's
	// responsibility (design §4.2(i)).
	identity, err := validator.ValidateAccessToken(t.Context(), "sa-token",
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("ValidateAccessToken failed: %v", err)
	}
	if !identity.IsServiceAccount {
		t.Error("expected IsServiceAccount=true for a service-account email")
	}
}

// ---------------------------------------------------------------------------
// Tokeninfo schema pinning (Required #6) — azp/aud are the correct field
// names for https://oauth2.googleapis.com/tokeninfo, NOT issued_to/audience.
// ---------------------------------------------------------------------------

func TestProductionValidator_TokenInfoSchema_FieldTypes(t *testing.T) {
	// This test verifies that the production validator correctly decodes a
	// representative Google tokeninfo response using the documented field names.
	// Per https://oauth2.googleapis.com/tokeninfo the fields are:
	//   azp: authorized party (issued-to client)
	//   aud: audience
	//   sub: stable subject
	//   email: email address
	//   email_verified: bool or string "true"/"false"
	//   expires_in: integer seconds
	//   scope: space-separated scopes
	//   access_type: "online" or "offline"
	//
	// Note: The older v2 endpoint used "issued_to" and "audience", but the
	// current OAuth2 endpoint uses "azp" and "aud".

	t.Run("azp_is_authoritative_issued_client", func(t *testing.T) {
		endpoints := newTestEndpoints(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				// Representative response with all documented fields.
				if err := json.NewEncoder(w).Encode(map[string]interface{}{
					"azp":            "authorized-client.apps.googleusercontent.com",
					"aud":            "authorized-client.apps.googleusercontent.com",
					"sub":            "sub-schema-test",
					"email":          "user@gmail.com",
					"email_verified": "true", // string form
					"expires_in":     1800,
					"scope":          "openid https://www.googleapis.com/auth/userinfo.email",
					"access_type":    "online",
				}); err != nil {
					t.Errorf("encode tokeninfo response: %v", err)
				}
			}),
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]interface{}{
					"sub":            "sub-schema-test",
					"email":          "user@gmail.com",
					"email_verified": true,
					"name":           "Schema User",
				}); err != nil {
					t.Errorf("encode userinfo response: %v", err)
				}
			}),
		)
		defer endpoints.close()

		validator := newTestValidator(endpoints)
		identity, err := validator.ValidateAccessToken(t.Context(), "schema-test-token",
			[]string{"authorized-client.apps.googleusercontent.com"})
		if err != nil {
			t.Fatalf("ValidateAccessToken failed: %v", err)
		}
		// The azp field is used as the authoritative audience in the identity.
		if identity.Audience != "authorized-client.apps.googleusercontent.com" {
			t.Errorf("audience = %q, expected azp value", identity.Audience)
		}
	})

	t.Run("issued_to_field_is_not_decoded", func(t *testing.T) {
		// If a response used "issued_to" instead of "azp", it would not be
		// decoded because the struct tag is json:"azp". This test verifies
		// that the validator fails closed (missing azp) in that scenario.
		endpoints := newTestEndpoints(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]interface{}{
					"issued_to":  "test-client.apps.googleusercontent.com", // wrong field name
					"audience":   "test-client.apps.googleusercontent.com", // wrong field name
					"sub":        "sub-1",
					"email":      "user@gmail.com",
					"expires_in": 3600,
				}); err != nil {
					t.Errorf("encode tokeninfo response: %v", err)
				}
			}),
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		)
		defer endpoints.close()

		validator := newTestValidator(endpoints)
		_, err := validator.ValidateAccessToken(t.Context(), "wrong-schema-token",
			[]string{"test-client.apps.googleusercontent.com"})
		if err == nil {
			t.Fatal("expected error when tokeninfo uses wrong field names (issued_to/audience instead of azp/aud)")
		}
		// Should fail because azp is empty.
		if !strings.Contains(err.Error(), "azp") {
			t.Errorf("error = %q, expected azp-related failure", err)
		}
	})

	t.Run("email_verified_as_string", func(t *testing.T) {
		// Google's tokeninfo endpoint sometimes returns email_verified as a
		// string rather than a native boolean. Verify flexBool handles this.
		var resp googleTokenInfoResponse
		body := `{"azp":"client","aud":"client","sub":"s","email":"a@gmail.com","email_verified":"true","expires_in":3600}`
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal tokeninfo: %v", err)
		}
		if resp.EmailVerified == nil || !bool(*resp.EmailVerified) {
			t.Error("expected email_verified=true from string form")
		}
		if resp.AZP != "client" {
			t.Errorf("azp = %q, want %q", resp.AZP, "client")
		}
		if resp.AUD != "client" {
			t.Errorf("aud = %q, want %q", resp.AUD, "client")
		}
	})

	t.Run("email_verified_as_bool", func(t *testing.T) {
		var resp googleTokenInfoResponse
		body := `{"azp":"client","aud":"client","sub":"s","email":"a@gmail.com","email_verified":true,"expires_in":3600}`
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal tokeninfo: %v", err)
		}
		if resp.EmailVerified == nil || !bool(*resp.EmailVerified) {
			t.Error("expected email_verified=true from boolean form")
		}
	})
}

// ---------------------------------------------------------------------------
// flexInt64 — expires_in as string through production validator.
// ---------------------------------------------------------------------------

func TestProductionValidator_AccessToken_ExpiresInAsString(t *testing.T) {
	// Google's tokeninfo may return expires_in as a JSON string ("3600")
	// instead of a number (3600). Verify the production validator handles this.
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"azp":            "test-client.apps.googleusercontent.com",
				"aud":            "test-client.apps.googleusercontent.com",
				"sub":            "sub-string-expiry",
				"email":          "user@gmail.com",
				"email_verified": "true",
				"expires_in":     "1800", // string form, not number
				"scope":          "openid email",
				"access_type":    "online",
			}); err != nil {
				t.Errorf("encode tokeninfo response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"sub":            "sub-string-expiry",
				"email":          "user@gmail.com",
				"email_verified": true,
				"name":           "String Expiry User",
			}); err != nil {
				t.Errorf("encode userinfo response: %v", err)
			}
		}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)
	identity, err := validator.ValidateAccessToken(t.Context(), "string-expiry-token",
		[]string{"test-client.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("ValidateAccessToken with string expires_in failed: %v", err)
	}
	if identity.Subject != "sub-string-expiry" {
		t.Errorf("subject = %q, want %q", identity.Subject, "sub-string-expiry")
	}
	// Verify expiry is set (within 30min from now since expires_in=1800).
	if identity.UpstreamExpiry.IsZero() {
		t.Error("upstream expiry should be set from string-form expires_in")
	}
	expectedExpiry := time.Now().Add(1800 * time.Second)
	if identity.UpstreamExpiry.Before(expectedExpiry.Add(-5*time.Second)) ||
		identity.UpstreamExpiry.After(expectedExpiry.Add(5*time.Second)) {
		t.Errorf("upstream expiry = %v, expected ~%v", identity.UpstreamExpiry, expectedExpiry)
	}
}

// ---------------------------------------------------------------------------
// ID Token remaining-lifetime boundary tests.
// ---------------------------------------------------------------------------

func TestProductionValidator_IDToken_ExpiredWithinSkew(t *testing.T) {
	// Token that expired 30s ago — within the 2min skew window, so jwt.Validate
	// passes. But remaining <= 0, so the validator rejects with NoRemainingLifetime.
	kp := newGCVTestKeyPair("skew-kid")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	claims := validIDTokenClaims()
	claims["exp"] = time.Now().Add(-30 * time.Second).Unix() // expired 30s ago
	claims["iat"] = time.Now().Add(-35 * time.Minute).Unix()
	claims["nbf"] = time.Now().Add(-35 * time.Minute).Unix()
	token := signIDToken(kp, claims)

	validator := newTestValidator(endpoints)
	_, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err == nil {
		t.Fatal("expected error for token expired within skew window")
	}
	if !strings.Contains(err.Error(), "no remaining usable lifetime") {
		t.Errorf("error = %q, expected NoRemainingLifetime", err)
	}
}

func TestProductionValidator_IDToken_PositiveRemaining(t *testing.T) {
	// Token that expires in 10s — positive remaining, should pass.
	kp := newGCVTestKeyPair("remaining-kid")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	claims := validIDTokenClaims()
	claims["exp"] = time.Now().Add(10 * time.Second).Unix()
	token := signIDToken(kp, claims)

	validator := newTestValidator(endpoints)
	identity, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("ValidateIDToken failed for token with 10s remaining: %v", err)
	}
	if identity.Subject != "google-sub-test-123" {
		t.Errorf("subject = %q, want %q", identity.Subject, "google-sub-test-123")
	}
}

func TestProductionValidator_IDToken_LongRemaining(t *testing.T) {
	// Token that expires in 2min — well within range, should pass.
	kp := newGCVTestKeyPair("long-remaining-kid")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	claims := validIDTokenClaims()
	claims["exp"] = time.Now().Add(2 * time.Minute).Unix()
	token := signIDToken(kp, claims)

	validator := newTestValidator(endpoints)
	identity, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("ValidateIDToken failed for token with 2min remaining: %v", err)
	}
	if identity.Subject != "google-sub-test-123" {
		t.Errorf("subject = %q, want %q", identity.Subject, "google-sub-test-123")
	}
}

// ---------------------------------------------------------------------------
// JWKS DoS verification — bounded rate-limited force-refresh.
// ---------------------------------------------------------------------------

func TestProductionValidator_JWKS_ForceRefreshRateLimited(t *testing.T) {
	kp1 := newGCVTestKeyPair("valid-kid")

	fetchCount := 0
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fetchCount++
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(gcvJWKSJSON(kp1)); err != nil {
				t.Errorf("write JWKS response: %v", err)
			}
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	validator := newTestValidator(endpoints)

	// Warm the cache with a valid token.
	token := signIDToken(kp1, validIDTokenClaims())
	_, err := validator.ValidateIDToken(t.Context(), token,
		[]string{"test-client-id.apps.googleusercontent.com"})
	if err != nil {
		t.Fatalf("initial validation failed: %v", err)
	}

	initialFetches := fetchCount

	// Now try to force many JWKS refreshes by using unknown kids.
	// The rate limiter (30s) should prevent excessive fetches.
	unknownKP := newGCVTestKeyPair("unknown-kid")
	for i := 0; i < 10; i++ {
		unknownToken := signIDToken(unknownKP, validIDTokenClaims())
		if _, err := validator.ValidateIDToken(t.Context(), unknownToken,
			[]string{"test-client-id.apps.googleusercontent.com"}); err == nil {
			t.Errorf("validation %d unexpectedly accepted token with unknown key", i)
		}
	}

	// Should have at most 2 additional fetches: the initial get() call + one
	// force-refresh. The rate limiter (30s window) prevents additional ones.
	additionalFetches := fetchCount - initialFetches
	if additionalFetches > 3 {
		t.Errorf("expected at most 3 additional JWKS fetches due to rate limiting, got %d", additionalFetches)
	}
}

// ---------------------------------------------------------------------------
// JWT exp regression test (EM mandatory) — decode actual minted Hub JWT,
// validate cryptographic exp against response fields and upstream expiry.
// ---------------------------------------------------------------------------

func TestGEExchange_JWTExpCryptographicallyCapped(t *testing.T) {
	// Set up: upstream credential expires in 2 minutes, configured TTL is 5 minutes.
	// The minted Hub JWT's exp claim MUST be capped at ~2 minutes (the upstream
	// remaining lifetime), NOT the full configured 5 minutes.
	identity := validGmailIdentity()
	upstreamExpiry := time.Now().Add(2 * time.Minute)
	identity.UpstreamExpiry = upstreamExpiry

	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	tokenSvc, err := NewUserTokenService(UserTokenConfig{
		SigningKey:          []byte("test-signing-key-for-jwt-exp-test"),
		AccessTokenDuration: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create token service: %v", err)
	}

	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         5 * time.Minute,
		},
		validator,
		tokenSvc,
		newMemExtIDStore(),
		userStore,
		alwaysAuthorized,
		slog.Default(),
	)

	resp, status, err := svc.Exchange(t.Context(), &ExchangeRequest{
		Credential:     "upstream-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("exchange failed: %v (status=%d)", err, status)
	}

	// Parse the response expiry fields.
	hubExpiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expiresAt: %v", err)
	}
	upstreamExpiresAt, err := time.Parse(time.RFC3339, resp.UpstreamExpiresAt)
	if err != nil {
		t.Fatalf("parse upstreamExpiresAt: %v", err)
	}

	// Hub expiry must not exceed upstream expiry.
	if hubExpiresAt.After(upstreamExpiresAt.Add(5 * time.Second)) {
		t.Errorf("Hub expiresAt (%v) exceeds upstreamExpiresAt (%v) — token TTL not capped",
			hubExpiresAt, upstreamExpiresAt)
	}

	// Cryptographic validation: decode the actual minted JWT and check its exp claim.
	parsedToken, err := jwt.ParseSigned(resp.AccessToken, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		t.Fatalf("parse minted JWT: %v", err)
	}

	var claims UserTokenClaims
	if err := parsedToken.Claims([]byte("test-signing-key-for-jwt-exp-test"), &claims); err != nil {
		t.Fatalf("verify minted JWT signature: %v", err)
	}

	if claims.Expiry == nil {
		t.Fatal("minted JWT has no exp claim")
	}

	jwtExp := claims.Expiry.Time()

	// The JWT's cryptographic exp must be capped at approximately the upstream
	// remaining lifetime (2 minutes from exchange time), not the configured 5 minutes.
	now := time.Now()
	maxAllowedExp := now.Add(2*time.Minute + 10*time.Second) // 10s tolerance for test execution
	if jwtExp.After(maxAllowedExp) {
		t.Errorf("JWT exp = %v, but must be capped at upstream expiry (~2min from now = %v); "+
			"configured TTL of 5min was not capped", jwtExp, maxAllowedExp)
	}

	// The JWT exp should be close to the response expiresAt field.
	if diff := jwtExp.Sub(hubExpiresAt).Abs(); diff > 5*time.Second {
		t.Errorf("JWT exp (%v) and response expiresAt (%v) differ by %v — they should match",
			jwtExp, hubExpiresAt, diff)
	}

	// Validate using the token service to prove the JWT is cryptographically valid.
	validatedClaims, err := tokenSvc.ValidateUserToken(resp.AccessToken)
	if err != nil {
		t.Fatalf("ValidateUserToken failed on minted JWT: %v", err)
	}
	if validatedClaims.UserID != resp.User.ID {
		t.Errorf("validated user ID = %q, want %q", validatedClaims.UserID, resp.User.ID)
	}
}

// ---------------------------------------------------------------------------
// Provisioning authorization test — exercises real Hub policy path.
// ---------------------------------------------------------------------------

func TestGEExchange_ProvisioningRejectedByPolicy(t *testing.T) {
	// authChecker rejects all provisioning — simulates invite-only mode
	// where the user is not invited.
	neverAuthorized := func(_ context.Context, _ string) bool { return false }

	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: 5 * time.Minute,
	})

	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         5 * time.Minute,
		},
		validator,
		tokenSvc,
		newMemExtIDStore(),
		userStore,
		neverAuthorized, // reject all provisioning
		slog.Default(),
	)

	_, status, err := svc.Exchange(t.Context(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error when authChecker rejects provisioning")
	}
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("error = %q, expected authorization failure", err)
	}
}

func TestGEExchange_ProvisioningAllowedForExistingUser(t *testing.T) {
	// Even with a restrictive authChecker, existing users with email match
	// should still succeed because the authChecker is only called for new
	// user provisioning, not for existing user binding.
	neverAuthorized := func(_ context.Context, _ string) bool { return false }

	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	addUser(userStore, "existing-user-1", "user@gmail.com", "member", "active")
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: 5 * time.Minute,
	})

	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         5 * time.Minute,
		},
		validator,
		tokenSvc,
		newMemExtIDStore(),
		userStore,
		neverAuthorized, // reject provisioning, but existing user bypass
		slog.Default(),
	)

	resp, status, err := svc.Exchange(t.Context(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("expected success for existing user even with restrictive policy: %v (status=%d)", err, status)
	}
	if resp.User.ID != "existing-user-1" {
		t.Errorf("user ID = %q, want %q", resp.User.ID, "existing-user-1")
	}
}

func TestGEExchange_NilAuthCheckerFailsClosed(t *testing.T) {
	// When authChecker is nil, the default fail-closed checker should reject provisioning.
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: 5 * time.Minute,
	})

	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         5 * time.Minute,
		},
		validator,
		tokenSvc,
		newMemExtIDStore(),
		userStore,
		nil, // nil authChecker → fail closed
		slog.Default(),
	)

	_, status, err := svc.Exchange(t.Context(), &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected error when authChecker is nil (fail closed)")
	}
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
}

// ---------------------------------------------------------------------------
// Orphan user cleanup test — verifies orphan users are cleaned up after
// concurrent binding race.
// ---------------------------------------------------------------------------

func TestGEExchange_OrphanUserCleanup(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: 5 * time.Minute,
	})

	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         5 * time.Minute,
		},
		validator,
		tokenSvc,
		extStore,
		userStore,
		alwaysAuthorized,
		slog.Default(),
	)

	// First exchange: creates user + binding.
	resp1, _, err := svc.Exchange(t.Context(), &ExchangeRequest{
		Credential:     "token-1",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("first exchange: %v", err)
	}

	// Count users before second exchange.
	userCountBefore := len(userStore.users)

	// Second exchange with same identity: should find existing binding.
	resp2, _, err := svc.Exchange(t.Context(), &ExchangeRequest{
		Credential:     "token-2",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("second exchange: %v", err)
	}

	if resp1.User.ID != resp2.User.ID {
		t.Errorf("same identity resolved to different users: %s vs %s",
			resp1.User.ID, resp2.User.ID)
	}

	// No orphan users should exist.
	if len(userStore.users) != userCountBefore {
		t.Errorf("orphan users detected: had %d users, now have %d",
			userCountBefore, len(userStore.users))
	}
}
