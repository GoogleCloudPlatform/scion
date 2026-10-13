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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// countingBaseValidator is a GoogleCredentialValidator whose ID/access token
// results are scripted per-call, counting how many times each method is
// actually invoked. Proves the caching decorator's cache-hit and
// singleflight behaviour: a mutation that skips the cache lookup, or that
// lets concurrent callers each dial upstream, must move this counter.
type countingBaseValidator struct {
	mu sync.Mutex

	idTokenCalls     int
	accessTokenCalls int

	idTokenResult *ValidatedGoogleIdentity
	idTokenErr    error

	accessTokenResult *ValidatedGoogleIdentity
	accessTokenErr    error

	// delay, if set, is slept before returning — widens the window
	// for concurrent callers to race into the same singleflight key.
	delay time.Duration
}

// fakeCacheMetrics records every RecordGoogleValidatorCache call. Safe for
// concurrent use (needed for the singleflight/concurrent scenarios above).
type fakeCacheMetrics struct {
	mu      sync.Mutex
	results []GoogleValidatorCacheResult
}

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

func (v *countingBaseValidator) ValidateIDToken(_ context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	v.mu.Lock()
	v.idTokenCalls++
	v.mu.Unlock()
	if v.delay > 0 {
		time.Sleep(v.delay)
	}
	return v.idTokenResult, v.idTokenErr
}

func (v *countingBaseValidator) ValidateAccessToken(_ context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	v.mu.Lock()
	v.accessTokenCalls++
	v.mu.Unlock()
	if v.delay > 0 {
		time.Sleep(v.delay)
	}
	return v.accessTokenResult, v.accessTokenErr
}

func (v *countingBaseValidator) totalIDTokenCalls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.idTokenCalls
}

func (v *countingBaseValidator) totalAccessTokenCalls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.accessTokenCalls
}

func (f *fakeCacheMetrics) RecordGoogleValidatorCache(result GoogleValidatorCacheResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, result)
}

func (f *fakeCacheMetrics) all() []GoogleValidatorCacheResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]GoogleValidatorCacheResult, len(f.results))
	copy(out, f.results)
	return out
}

func (f *fakeCacheMetrics) count(result GoogleValidatorCacheResult) int {
	n := 0
	for _, r := range f.all() {
		if r == result {
			n++
		}
	}
	return n
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
