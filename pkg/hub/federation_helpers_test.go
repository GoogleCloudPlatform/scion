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
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// setupFederationTestServer generates an RSA key pair, starts a JWKS test server,
// and returns the private key, server, and kid for signing tokens.
func setupFederationTestServer(t *testing.T) (*rsa.PrivateKey, *httptest.Server, string) {
	t.Helper()
	kid := "test-fed-key-1"
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	jwks := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{
				Key:       &privKey.PublicKey,
				KeyID:     kid,
				Algorithm: string(jose.RS256),
				Use:       "sig",
			},
		},
	}
	jwksData, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("failed to marshal JWKS: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(srv.Close)

	return privKey, srv, kid
}

// signFederationToken creates a signed RS256 JWT for federation testing.
func signFederationToken(t *testing.T, key *rsa.PrivateKey, kid string, claims federationClaims) string {
	t.Helper()
	signerKey := jose.SigningKey{Algorithm: jose.RS256, Key: key}
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
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

// validFederationClaims returns a baseline valid claims set for testing.
func validFederationClaims(issuer, audience string) federationClaims {
	now := time.Now()
	return federationClaims{
		Claims: jwt.Claims{
			Issuer:    issuer,
			Subject:   "agent-123",
			Audience:  jwt.Audience{audience},
			IssuedAt:  jwt.NewNumericDate(now.Add(-1 * time.Minute)),
			Expiry:    jwt.NewNumericDate(now.Add(5 * time.Minute)),
			NotBefore: jwt.NewNumericDate(now.Add(-1 * time.Minute)),
		},
		ProjectID: "project-alpha",
		AgentName: "worker-1",
		Ancestry:  []string{"user:alice", "agent:root"},
		RootUser:  "user:alice",
	}
}

// signGenericToken creates a signed RS256 JWT with arbitrary claims for non-hub token testing.
func signGenericToken(t *testing.T, key *rsa.PrivateKey, kid string, claims interface{}) string {
	t.Helper()
	signerKey := jose.SigningKey{Algorithm: jose.RS256, Key: key}
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
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

// newTestAuthenticatorWithConfig creates a FederationAuthenticator from a full config.
func newTestAuthenticatorWithConfig(t *testing.T, cfg config.FederationConfig, expectedAudience string) *FederationAuthenticator {
	t.Helper()
	auth, err := NewFederationAuthenticator(cfg, expectedAudience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}
	return auth
}

// federationAuthCaptureBuffer builds a *slog.Logger that writes JSON lines to
// a buffer, so a test can assert on NewFederationAuthenticator's load-time
// log output directly, without touching the global default logger (the
// function takes its logger as an explicit parameter).
func federationAuthCaptureBuffer() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// countWarnLines returns how many JSON log lines in buf are level WARN with
// the given message.
func countWarnLines(t *testing.T, buf *bytes.Buffer, msg string) int {
	t.Helper()
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if rec["level"] == "WARN" && rec["msg"] == msg {
			count++
		}
	}
	return count
}

// newFedAuthPointer creates an atomic.Pointer pre-loaded with the given authenticator.
func newFedAuthPointer(auth *FederationAuthenticator) *atomic.Pointer[FederationAuthenticator] {
	p := &atomic.Pointer[FederationAuthenticator]{}
	if auth != nil {
		p.Store(auth)
	}
	return p
}
