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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// testKeyPair holds a self-generated ES256 key pair for testing.
type testKeyPair struct {
	privateKey *ecdsa.PrivateKey
	kid        string
}

func newTestKeyPair(t *testing.T, kid string) *testKeyPair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate ES256 key: %v", err)
	}
	return &testKeyPair{privateKey: key, kid: kid}
}

// jwksJSON returns the JWKS JSON containing the public key.
func (kp *testKeyPair) jwksJSON(t *testing.T) []byte {
	t.Helper()
	jwk := jose.JSONWebKey{
		Key:       &kp.privateKey.PublicKey,
		KeyID:     kp.kid,
		Algorithm: string(jose.ES256),
		Use:       "sig",
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}}
	data, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("failed to marshal JWKS: %v", err)
	}
	return data
}

// signJWT creates a signed JWT compact serialization.
func (kp *testKeyPair) signJWT(t *testing.T, claims interface{}) string {
	t.Helper()
	signerKey := jose.SigningKey{Algorithm: jose.ES256, Key: kp.privateKey}
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kp.kid)
	signer, err := jose.NewSigner(signerKey, opts)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("failed to sign JWT: %v", err)
	}
	return raw
}

// startJWKSServer starts a test HTTP server serving the given JWKS JSON.
func startJWKSServer(t *testing.T, jwksData []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func makeTestClaims(sub, email, iss, aud string, iat, exp time.Time) map[string]interface{} {
	claims := map[string]interface{}{
		"iss":   iss,
		"sub":   sub,
		"aud":   aud,
		"email": email,
		"iat":   iat.Unix(),
		"exp":   exp.Unix(),
	}
	return claims
}

func TestIAPAuthenticator_ValidAssertion(t *testing.T) {
	kp := newTestKeyPair(t, "test-key-1")
	jwksSrv := startJWKSServer(t, kp.jwksJSON(t))

	now := time.Now()
	claims := makeTestClaims(
		"accounts.google.com:12345",
		"accounts.google.com:user@example.com",
		"https://cloud.google.com/iap",
		"/projects/123/global/backendServices/456",
		now.Add(-1*time.Minute),
		now.Add(5*time.Minute),
	)
	assertion := kp.signJWT(t, claims)

	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
		JWKSURL:  jwksSrv.URL,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(IAPAssertionHeader, assertion)

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if info == nil {
		t.Fatal("expected ProxyUserInfo, got nil")
	}
	if info.Subject != "12345" {
		t.Errorf("expected subject '12345', got %q", info.Subject)
	}
	if info.Email != "user@example.com" {
		t.Errorf("expected email 'user@example.com', got %q", info.Email)
	}
}

func TestIAPAuthenticator_MissingHeader(t *testing.T) {
	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
	}

	req := httptest.NewRequest("GET", "/", nil)
	// No assertion header set

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("expected nil error for missing header, got: %v", err)
	}
	if info != nil {
		t.Fatal("expected nil info for missing header")
	}
}

func TestIAPAuthenticator_BadSignature(t *testing.T) {
	kp1 := newTestKeyPair(t, "test-key-1")
	kp2 := newTestKeyPair(t, "test-key-1") // different key, same kid

	// JWKS has kp2's public key
	jwksSrv := startJWKSServer(t, kp2.jwksJSON(t))

	now := time.Now()
	claims := makeTestClaims(
		"accounts.google.com:12345",
		"accounts.google.com:user@example.com",
		"https://cloud.google.com/iap",
		"/projects/123/global/backendServices/456",
		now.Add(-1*time.Minute),
		now.Add(5*time.Minute),
	)
	// Sign with kp1's private key
	assertion := kp1.signJWT(t, claims)

	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
		JWKSURL:  jwksSrv.URL,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(IAPAssertionHeader, assertion)

	info, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for bad signature, got nil")
	}
	if info != nil {
		t.Fatal("expected nil info for bad signature")
	}
}

func TestIAPAuthenticator_WrongAudience(t *testing.T) {
	kp := newTestKeyPair(t, "test-key-1")
	jwksSrv := startJWKSServer(t, kp.jwksJSON(t))

	now := time.Now()
	claims := makeTestClaims(
		"accounts.google.com:12345",
		"accounts.google.com:user@example.com",
		"https://cloud.google.com/iap",
		"/projects/WRONG/global/backendServices/WRONG",
		now.Add(-1*time.Minute),
		now.Add(5*time.Minute),
	)
	assertion := kp.signJWT(t, claims)

	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
		JWKSURL:  jwksSrv.URL,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(IAPAssertionHeader, assertion)

	info, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for wrong audience, got nil")
	}
	if info != nil {
		t.Fatal("expected nil info for wrong audience")
	}
	t.Logf("expected error: %v", err)
}

func TestIAPAuthenticator_WrongIssuer(t *testing.T) {
	kp := newTestKeyPair(t, "test-key-1")
	jwksSrv := startJWKSServer(t, kp.jwksJSON(t))

	now := time.Now()
	claims := makeTestClaims(
		"accounts.google.com:12345",
		"accounts.google.com:user@example.com",
		"https://evil.example.com/iap",
		"/projects/123/global/backendServices/456",
		now.Add(-1*time.Minute),
		now.Add(5*time.Minute),
	)
	assertion := kp.signJWT(t, claims)

	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
		JWKSURL:  jwksSrv.URL,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(IAPAssertionHeader, assertion)

	info, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for wrong issuer, got nil")
	}
	if info != nil {
		t.Fatal("expected nil info for wrong issuer")
	}
	t.Logf("expected error: %v", err)
}

func TestIAPAuthenticator_ExpiredToken(t *testing.T) {
	kp := newTestKeyPair(t, "test-key-1")
	jwksSrv := startJWKSServer(t, kp.jwksJSON(t))

	now := time.Now()
	claims := makeTestClaims(
		"accounts.google.com:12345",
		"accounts.google.com:user@example.com",
		"https://cloud.google.com/iap",
		"/projects/123/global/backendServices/456",
		now.Add(-10*time.Minute),
		now.Add(-5*time.Minute), // expired 5 minutes ago (well past 30s skew)
	)
	assertion := kp.signJWT(t, claims)

	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
		JWKSURL:  jwksSrv.URL,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(IAPAssertionHeader, assertion)

	info, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if info != nil {
		t.Fatal("expected nil info for expired token")
	}
	t.Logf("expected error: %v", err)
}

func TestIAPAuthenticator_CustomIssuer(t *testing.T) {
	kp := newTestKeyPair(t, "test-key-1")
	jwksSrv := startJWKSServer(t, kp.jwksJSON(t))

	now := time.Now()
	customIssuer := "https://test.example.com/iap"
	claims := makeTestClaims(
		"accounts.google.com:12345",
		"accounts.google.com:user@test.com",
		customIssuer,
		"/projects/123/global/backendServices/456",
		now.Add(-1*time.Minute),
		now.Add(5*time.Minute),
	)
	assertion := kp.signJWT(t, claims)

	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
		Issuer:   customIssuer,
		JWKSURL:  jwksSrv.URL,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(IAPAssertionHeader, assertion)

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("expected no error with custom issuer, got: %v", err)
	}
	if info == nil {
		t.Fatal("expected ProxyUserInfo, got nil")
	}
	if info.Email != "user@test.com" {
		t.Errorf("expected email 'user@test.com', got %q", info.Email)
	}
}

func TestIAPAuthenticator_UnknownKidTriggersRefresh(t *testing.T) {
	kp1 := newTestKeyPair(t, "old-key")
	kp2 := newTestKeyPair(t, "new-key")

	// Start JWKS server initially with only old key
	var currentJWKS []byte
	currentJWKS = kp1.jwksJSON(t)

	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(currentJWKS)
	}))
	t.Cleanup(jwksSrv.Close)

	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
		JWKSURL:  jwksSrv.URL,
	}

	// First request with old key works
	now := time.Now()
	claims1 := makeTestClaims(
		"accounts.google.com:12345",
		"accounts.google.com:user@example.com",
		"https://cloud.google.com/iap",
		"/projects/123/global/backendServices/456",
		now.Add(-1*time.Minute),
		now.Add(5*time.Minute),
	)
	assertion1 := kp1.signJWT(t, claims1)
	req1 := httptest.NewRequest("GET", "/", nil)
	req1.Header.Set(IAPAssertionHeader, assertion1)
	info1, err := auth.Authenticate(req1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	if info1 == nil {
		t.Fatal("first request returned nil info")
	}

	// Now "rotate" keys — JWKS server returns both keys
	bothKeys := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{Key: &kp1.privateKey.PublicKey, KeyID: kp1.kid, Algorithm: string(jose.ES256), Use: "sig"},
			{Key: &kp2.privateKey.PublicKey, KeyID: kp2.kid, Algorithm: string(jose.ES256), Use: "sig"},
		},
	}
	bothData, _ := json.Marshal(bothKeys)
	currentJWKS = bothData

	// Reset the cache's fetch times to force refresh on unknown kid
	auth.initOnce.Do(func() {}) // ensure init ran
	auth.jwksCache.mu.Lock()
	auth.jwksCache.lastFetched = time.Time{}   // force proactive refresh
	auth.jwksCache.lastAttempted = time.Time{} // clear debounce window
	auth.jwksCache.mu.Unlock()

	// Second request with new key — should trigger JWKS refresh and succeed
	claims2 := makeTestClaims(
		"accounts.google.com:67890",
		"accounts.google.com:user2@example.com",
		"https://cloud.google.com/iap",
		"/projects/123/global/backendServices/456",
		now.Add(-1*time.Minute),
		now.Add(5*time.Minute),
	)
	assertion2 := kp2.signJWT(t, claims2)
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set(IAPAssertionHeader, assertion2)
	info2, err := auth.Authenticate(req2)
	if err != nil {
		t.Fatalf("second request (new kid) failed: %v", err)
	}
	if info2 == nil {
		t.Fatal("second request returned nil info")
	}
	if info2.Subject != "67890" {
		t.Errorf("expected subject '67890', got %q", info2.Subject)
	}
}

func TestIAPAuthenticator_StripPrefix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"accounts.google.com:12345", "12345"},
		{"accounts.google.com:user@example.com", "user@example.com"},
		{"12345", "12345"},                       // no prefix
		{"user@example.com", "user@example.com"}, // no prefix
		{"", ""},
	}
	for _, tt := range tests {
		got := stripIAPPrefix(tt.input)
		if got != tt.expected {
			t.Errorf("stripIAPPrefix(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestIAPAuthenticator_EmailLowercased(t *testing.T) {
	kp := newTestKeyPair(t, "test-key-1")
	jwksSrv := startJWKSServer(t, kp.jwksJSON(t))

	now := time.Now()
	claims := makeTestClaims(
		"accounts.google.com:12345",
		"accounts.google.com:User@EXAMPLE.COM",
		"https://cloud.google.com/iap",
		"/projects/123/global/backendServices/456",
		now.Add(-1*time.Minute),
		now.Add(5*time.Minute),
	)
	assertion := kp.signJWT(t, claims)

	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
		JWKSURL:  jwksSrv.URL,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(IAPAssertionHeader, assertion)

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Email != "user@example.com" {
		t.Errorf("expected lowercased email 'user@example.com', got %q", info.Email)
	}
}

func TestIAPAuthenticator_HDClaim(t *testing.T) {
	kp := newTestKeyPair(t, "test-key-1")
	jwksSrv := startJWKSServer(t, kp.jwksJSON(t))

	now := time.Now()
	claims := map[string]interface{}{
		"iss":   "https://cloud.google.com/iap",
		"sub":   "accounts.google.com:12345",
		"aud":   "/projects/123/global/backendServices/456",
		"email": "accounts.google.com:user@example.com",
		"hd":    "example.com",
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp.signJWT(t, claims)

	auth := &IAPAuthenticator{
		Audience: "/projects/123/global/backendServices/456",
		JWKSURL:  jwksSrv.URL,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(IAPAssertionHeader, assertion)

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Domain != "example.com" {
		t.Errorf("expected domain 'example.com', got %q", info.Domain)
	}
}

func TestIAPAuthenticator_Name(t *testing.T) {
	auth := &IAPAuthenticator{}
	if auth.Name() != "iap" {
		t.Errorf("expected Name()='iap', got %q", auth.Name())
	}
}

func TestJWKSCache_TransientFailure(t *testing.T) {
	kp := newTestKeyPair(t, "test-key-1")

	// Start a failing server
	failCount := 0
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCount++
		if failCount <= 1 {
			// First call succeeds
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(kp.jwksJSON(t))
		} else {
			// Subsequent calls fail
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(jwksSrv.Close)

	cache := &jwksCache{url: jwksSrv.URL, client: http.DefaultClient}

	// First fetch succeeds
	key, err := cache.GetKey(kp.kid)
	if err != nil {
		t.Fatalf("first GetKey failed: %v", err)
	}
	if key == nil {
		t.Fatal("first GetKey returned nil key")
	}

	// Force refresh by clearing lastFetched and lastAttempted
	cache.mu.Lock()
	cache.lastFetched = time.Time{}
	cache.lastAttempted = time.Time{}
	cache.mu.Unlock()

	// Second fetch with same kid still works (returns cached key even though refresh fails)
	key2, err := cache.GetKey(kp.kid)
	if err != nil {
		t.Fatalf("second GetKey failed: %v", err)
	}
	if key2 == nil {
		t.Fatal("second GetKey returned nil key")
	}
}

func TestJWKSCache_StampedePreventionDuringOutage(t *testing.T) {
	kp := newTestKeyPair(t, "test-key-1")

	fetchCount := 0
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		if fetchCount <= 1 {
			// First call succeeds — populate the cache
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(kp.jwksJSON(t))
		} else {
			// All subsequent calls fail (simulating a persistent outage)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(jwksSrv.Close)

	cache := &jwksCache{url: jwksSrv.URL, client: jwksSrv.Client()}

	// Populate cache with a successful fetch
	key, err := cache.GetKey(kp.kid)
	if err != nil {
		t.Fatalf("initial GetKey failed: %v", err)
	}
	if key == nil {
		t.Fatal("initial GetKey returned nil key")
	}
	if fetchCount != 1 {
		t.Fatalf("expected 1 fetch after initial GetKey, got %d", fetchCount)
	}

	// Reset lastAttempted to allow the next refresh attempt, but keep lastFetched
	// old enough that proactive refresh is desired
	cache.mu.Lock()
	cache.lastFetched = time.Time{}
	cache.lastAttempted = time.Time{}
	cache.mu.Unlock()

	// Now make multiple GetKey calls for an unknown kid during the outage.
	// Each call triggers refresh() (kid miss), but debounce should prevent
	// more than one actual fetch within the debounce window.
	unknownKid := "unknown-kid"
	for i := 0; i < 5; i++ {
		_, _ = cache.GetKey(unknownKid)
	}

	// Expect exactly 2 fetches total: 1 initial success + 1 failed attempt
	// within the debounce window. The remaining 4 calls should be debounced.
	if fetchCount != 2 {
		t.Errorf("expected 2 total fetches (1 initial + 1 debounced attempt), got %d", fetchCount)
	}

	// Verify the cache still serves the last-good key
	key2, err := cache.GetKey(kp.kid)
	if err != nil {
		t.Fatalf("GetKey for cached kid during outage failed: %v", err)
	}
	if key2 == nil {
		t.Fatal("expected last-good key to be served during outage")
	}
}

// ---- Generic JWT Proxy Authenticator tests ----

// testRSAKeyPair holds a self-generated RSA key pair for JWT provider tests.
type testRSAKeyPair struct {
	privateKey *rsa.PrivateKey
}

func newTestRSAKeyPair(t *testing.T) *testRSAKeyPair {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	return &testRSAKeyPair{privateKey: key}
}

// writePublicKeyPEM PKIX-encodes the public key and writes it to a temp file,
// returning the path.
func (kp *testRSAKeyPair) writePublicKeyPEM(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&kp.privateKey.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	block := &pem.Block{Type: "PUBLIC KEY", Bytes: der}
	path := filepath.Join(t.TempDir(), "pubkey.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("failed to write public key file: %v", err)
	}
	return path
}

// signJWT creates an RS256-signed compact JWT serialization.
func (kp *testRSAKeyPair) signJWT(t *testing.T, claims interface{}) string {
	t.Helper()
	signerKey := jose.SigningKey{Algorithm: jose.RS256, Key: kp.privateKey}
	signer, err := jose.NewSigner(signerKey, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("failed to sign JWT: %v", err)
	}
	return raw
}

// newTestJWTKeySource loads kp's public key via NewStaticJWTKeySource,
// failing the test on error.
func newTestJWTKeySource(t *testing.T, kp *testRSAKeyPair) jwtKeySource {
	t.Helper()
	ks, err := NewStaticJWTKeySource(kp.writePublicKeyPEM(t))
	if err != nil {
		t.Fatalf("NewStaticJWTKeySource failed: %v", err)
	}
	return ks
}

func TestJWTProxyAuthenticator_ValidAssertion(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	keySource := newTestJWTKeySource(t, kp)

	now := time.Now()
	claims := map[string]interface{}{
		"email": "User@Example.com",
		"sub":   "user-123",
		"name":  "Preston Holmes",
		"hd":    "example.com",
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		KeySource: keySource,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, assertion)

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if info == nil {
		t.Fatal("expected ProxyUserInfo, got nil")
	}
	if info.Subject != "user-123" {
		t.Errorf("expected subject 'user-123', got %q", info.Subject)
	}
	if info.Email != "user@example.com" {
		t.Errorf("expected lowercased email 'user@example.com', got %q", info.Email)
	}
	if info.DisplayName != "Preston Holmes" {
		t.Errorf("expected display name 'Preston Holmes', got %q", info.DisplayName)
	}
	if info.Domain != "example.com" {
		t.Errorf("expected domain 'example.com', got %q", info.Domain)
	}
}

func TestJWTProxyAuthenticator_MissingHeader(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		KeySource: newTestJWTKeySource(t, kp),
	}

	req := httptest.NewRequest("GET", "/", nil)
	// No assertion header set.

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("expected nil error for missing header, got: %v", err)
	}
	if info != nil {
		t.Fatal("expected nil info for missing header")
	}
}

func TestJWTProxyAuthenticator_CustomHeader(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	now := time.Now()
	claims := map[string]interface{}{
		"email": "user@example.com",
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Header:    "X-Custom-JWT",
		Algorithm: string(jose.RS256),
		KeySource: newTestJWTKeySource(t, kp),
	}

	// Default header must NOT work when a custom header is configured.
	reqDefault := httptest.NewRequest("GET", "/", nil)
	reqDefault.Header.Set(DefaultJWTHeader, assertion)
	if info, err := auth.Authenticate(reqDefault); err != nil || info != nil {
		t.Fatalf("expected fallthrough on default header when custom header configured, got info=%v err=%v", info, err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Custom-JWT", assertion)
	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if info == nil || info.Email != "user@example.com" {
		t.Fatalf("expected verified user, got info=%v", info)
	}
}

func TestJWTProxyAuthenticator_BadSignature(t *testing.T) {
	kp1 := newTestRSAKeyPair(t)
	kp2 := newTestRSAKeyPair(t)

	// Key source is built from kp2's public key, but the token is signed by kp1.
	keySource := newTestJWTKeySource(t, kp2)

	now := time.Now()
	claims := map[string]interface{}{
		"email": "user@example.com",
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp1.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		KeySource: keySource,
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, assertion)

	info, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for bad signature, got nil")
	}
	if info != nil {
		t.Fatal("expected nil info for bad signature")
	}
}

func TestJWTProxyAuthenticator_WrongIssuer(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	now := time.Now()
	claims := map[string]interface{}{
		"iss":   "https://evil.example.com",
		"email": "user@example.com",
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		Issuer:    "my-auth-proxy",
		KeySource: newTestJWTKeySource(t, kp),
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, assertion)

	info, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for wrong issuer, got nil")
	}
	if info != nil {
		t.Fatal("expected nil info for wrong issuer")
	}
	t.Logf("expected error: %v", err)
}

func TestJWTProxyAuthenticator_WrongAudience(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	now := time.Now()
	claims := map[string]interface{}{
		"aud":   "some-other-audience",
		"email": "user@example.com",
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		Audience:  "scion-hub",
		KeySource: newTestJWTKeySource(t, kp),
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, assertion)

	info, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for wrong audience, got nil")
	}
	if info != nil {
		t.Fatal("expected nil info for wrong audience")
	}
	t.Logf("expected error: %v", err)
}

func TestJWTProxyAuthenticator_ExpiredToken(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	now := time.Now()
	claims := map[string]interface{}{
		"email": "user@example.com",
		"iat":   now.Add(-10 * time.Minute).Unix(),
		"exp":   now.Add(-5 * time.Minute).Unix(), // expired well past the 30s skew
	}
	assertion := kp.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		KeySource: newTestJWTKeySource(t, kp),
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, assertion)

	info, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if info != nil {
		t.Fatal("expected nil info for expired token")
	}
	t.Logf("expected error: %v", err)
}

func TestJWTProxyAuthenticator_EmailLowercased(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	now := time.Now()
	claims := map[string]interface{}{
		"email": "User@EXAMPLE.COM",
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		KeySource: newTestJWTKeySource(t, kp),
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, assertion)

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Email != "user@example.com" {
		t.Errorf("expected lowercased email 'user@example.com', got %q", info.Email)
	}
}

func TestJWTProxyAuthenticator_ClaimMapping(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	now := time.Now()
	claims := map[string]interface{}{
		"user_email": "user@example.com",
		"user_id":    "12345",
		"full_name":  "Preston Holmes",
		"domain":     "example.com",
		"iat":        now.Add(-1 * time.Minute).Unix(),
		"exp":        now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		KeySource: newTestJWTKeySource(t, kp),
		Claims: JWTClaimMapping{
			Email:       "user_email",
			Subject:     "user_id",
			DisplayName: "full_name",
			Domain:      "domain",
		},
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, assertion)

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Subject != "12345" {
		t.Errorf("expected subject '12345', got %q", info.Subject)
	}
	if info.Email != "user@example.com" {
		t.Errorf("expected email 'user@example.com', got %q", info.Email)
	}
	if info.DisplayName != "Preston Holmes" {
		t.Errorf("expected display name 'Preston Holmes', got %q", info.DisplayName)
	}
	if info.Domain != "example.com" {
		t.Errorf("expected domain 'example.com', got %q", info.Domain)
	}
}

func TestJWTProxyAuthenticator_SubjectFallsBackToEmail(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	now := time.Now()
	claims := map[string]interface{}{
		"email": "user@example.com",
		// no "sub" claim
		"iat": now.Add(-1 * time.Minute).Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		KeySource: newTestJWTKeySource(t, kp),
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, assertion)

	info, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Subject != "user@example.com" {
		t.Errorf("expected subject to fall back to email 'user@example.com', got %q", info.Subject)
	}
}

func TestJWTProxyAuthenticator_MissingEmailClaim(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	now := time.Now()
	claims := map[string]interface{}{
		"sub": "user-123",
		"iat": now.Add(-1 * time.Minute).Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	assertion := kp.signJWT(t, claims)

	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
		KeySource: newTestJWTKeySource(t, kp),
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, assertion)

	info, err := auth.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for missing email claim, got nil")
	}
	if info != nil {
		t.Fatal("expected nil info for missing email claim")
	}
	t.Logf("expected error: %v", err)
}

func TestJWTProxyAuthenticator_NoAlgorithmConfigured(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	auth := &JWTProxyAuthenticator{
		KeySource: newTestJWTKeySource(t, kp),
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, "irrelevant.token.value")

	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("expected error when no algorithm is configured")
	}
}

func TestJWTProxyAuthenticator_NoKeySourceConfigured(t *testing.T) {
	auth := &JWTProxyAuthenticator{
		Algorithm: string(jose.RS256),
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(DefaultJWTHeader, "irrelevant.token.value")

	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("expected error when no key source is configured")
	}
}

func TestJWTProxyAuthenticator_Name(t *testing.T) {
	auth := &JWTProxyAuthenticator{}
	if auth.Name() != "jwt" {
		t.Errorf("expected Name()='jwt', got %q", auth.Name())
	}
}

func TestJWTStaticKeySource_LoadsPEMKey(t *testing.T) {
	kp := newTestRSAKeyPair(t)
	path := kp.writePublicKeyPEM(t)

	ks, err := NewStaticJWTKeySource(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	key1, err := ks.GetKey("some-kid")
	if err != nil {
		t.Fatalf("unexpected error from GetKey: %v", err)
	}
	key2, err := ks.GetKey("a-different-kid")
	if err != nil {
		t.Fatalf("unexpected error from GetKey: %v", err)
	}
	rsaKey1, ok := key1.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("expected *rsa.PublicKey, got %T", key1)
	}
	if !rsaKey1.Equal(key2.(*rsa.PublicKey)) {
		t.Error("expected GetKey to return the same key regardless of kid")
	}
	if !rsaKey1.Equal(&kp.privateKey.PublicKey) {
		t.Error("expected loaded key to match the original public key")
	}
}

func TestJWTStaticKeySource_MissingFile(t *testing.T) {
	_, err := NewStaticJWTKeySource(filepath.Join(t.TempDir(), "does-not-exist.pem"))
	if err == nil {
		t.Fatal("expected error for missing public key file")
	}
}

func TestJWTStaticKeySource_InvalidPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(path, []byte("not a pem file"), 0o600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	_, err := NewStaticJWTKeySource(path)
	if err == nil {
		t.Fatal("expected error for invalid PEM content")
	}
}

func TestIsAsymmetricJWTAlgorithm(t *testing.T) {
	tests := []struct {
		alg  string
		want bool
	}{
		{"RS256", true},
		{"RS384", true},
		{"RS512", true},
		{"ES256", true},
		{"ES384", true},
		{"ES512", true},
		{"PS256", true},
		{"PS384", true},
		{"PS512", true},
		{"HS256", false},
		{"none", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsAsymmetricJWTAlgorithm(tt.alg); got != tt.want {
			t.Errorf("IsAsymmetricJWTAlgorithm(%q) = %v, want %v", tt.alg, got, tt.want)
		}
	}
}
