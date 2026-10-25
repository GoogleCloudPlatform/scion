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

package runtimebroker

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

func TestBrokerAuthMiddleware_Disabled(t *testing.T) {
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()

	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_AllowUnauthenticated(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: true,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request without any HMAC headers
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()

	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200 for unauthenticated request, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_RequireAuth(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request without any HMAC headers
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()

	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for missing auth headers, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_ValidSignature(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a properly signed request
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req, "test-host", secret)

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200 for valid signature, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestBrokerAuthMiddleware_InvalidSignature(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a request with wrong signature
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set(apiclient.HeaderBrokerID, "test-host")
	req.Header.Set(apiclient.HeaderTimestamp, timestamp)
	req.Header.Set(apiclient.HeaderNonce, "test-nonce")
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString([]byte("invalid-signature")))

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for invalid signature, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_ExpiredTimestamp(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a request with old timestamp
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	oldTimestamp := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	nonce := "test-nonce"

	req.Header.Set(apiclient.HeaderBrokerID, "test-host")
	req.Header.Set(apiclient.HeaderTimestamp, oldTimestamp)
	req.Header.Set(apiclient.HeaderNonce, nonce)

	// Compute valid signature with old timestamp
	canonical := apiclient.BuildCanonicalString(req, oldTimestamp, nonce)
	sig := apiclient.ComputeHMAC(secret, canonical)
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString(sig))

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for expired timestamp, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_FutureTimestamp(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a request with future timestamp
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	futureTimestamp := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	nonce := "test-nonce"

	req.Header.Set(apiclient.HeaderBrokerID, "test-host")
	req.Header.Set(apiclient.HeaderTimestamp, futureTimestamp)
	req.Header.Set(apiclient.HeaderNonce, nonce)

	// Compute valid signature with future timestamp
	canonical := apiclient.BuildCanonicalString(req, futureTimestamp, nonce)
	sig := apiclient.ComputeHMAC(secret, canonical)
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString(sig))

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for future timestamp, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_MissingHeaders(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{
			name: "missing timestamp",
			headers: map[string]string{
				apiclient.HeaderBrokerID:  "host-id",
				apiclient.HeaderNonce:     "nonce",
				apiclient.HeaderSignature: "sig",
			},
		},
		{
			name: "missing signature",
			headers: map[string]string{
				apiclient.HeaderBrokerID:  "host-id",
				apiclient.HeaderTimestamp: "123",
				apiclient.HeaderNonce:     "nonce",
			},
		},
		{
			name: "missing host ID only",
			headers: map[string]string{
				apiclient.HeaderTimestamp: "123",
				apiclient.HeaderNonce:     "nonce",
				apiclient.HeaderSignature: "sig",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			rr := httptest.NewRecorder()
			middleware.Middleware(handler).ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("Expected status 401, got %d", rr.Code)
			}
		})
	}
}

func TestBrokerAuthMiddleware_InvalidTimestampFormat(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(apiclient.HeaderBrokerID, "host-id")
	req.Header.Set(apiclient.HeaderTimestamp, "not-a-number")
	req.Header.Set(apiclient.HeaderNonce, "nonce")
	req.Header.Set(apiclient.HeaderSignature, "sig")

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_InvalidSignatureEncoding(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(apiclient.HeaderBrokerID, "host-id")
	req.Header.Set(apiclient.HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(apiclient.HeaderNonce, "nonce")
	req.Header.Set(apiclient.HeaderSignature, "not-valid-base64!!!")

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_UpdateSecretKey(t *testing.T) {
	oldSecret := []byte("old-secret-key")
	newSecret := []byte("new-secret-key-32bytes!12345678")

	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            oldSecret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create request signed with new secret
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req, "test-host", newSecret)

	// Should fail with old secret
	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 before key update, got %d", rr.Code)
	}

	// Update to new secret
	middleware.UpdateSecretKey(newSecret)

	// Should succeed now
	req = httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req, "test-host", newSecret)
	rr = httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("Expected 200 after key update, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_SetEnabled(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              false,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Should pass when disabled
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("Expected 200 when disabled, got %d", rr.Code)
	}

	// Enable auth
	middleware.SetEnabled(true)

	// Should fail without auth
	req = httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr = httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 when enabled, got %d", rr.Code)
	}
}

func TestDefaultBrokerAuthConfig(t *testing.T) {
	cfg := DefaultBrokerAuthConfig()

	if cfg.Enabled {
		t.Error("Expected Enabled to be false by default")
	}
	if cfg.MaxClockSkew != 5*time.Minute {
		t.Errorf("Expected MaxClockSkew 5m, got %v", cfg.MaxClockSkew)
	}
	if !cfg.AllowUnauthenticated {
		t.Error("Expected AllowUnauthenticated to be true by default")
	}
}

// signRequest signs an HTTP request with HMAC authentication.
func signRequest(req *http.Request, brokerID string, secret []byte) {
	signRequestWithNonce(req, brokerID, secret, randomTestNonce())
}

// randomTestNonce returns a fresh nonce, as the real signers do, so two
// requests signed in the same second are distinct requests.
func randomTestNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.URLEncoding.EncodeToString(b)
}

// signRequestWithNonce signs req with the given nonce (which may be empty).
func signRequestWithNonce(req *http.Request, brokerID string, secret []byte, nonce string) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	req.Header.Set(apiclient.HeaderBrokerID, brokerID)
	req.Header.Set(apiclient.HeaderTimestamp, timestamp)
	req.Header.Set(apiclient.HeaderNonce, nonce)

	canonical := apiclient.BuildCanonicalString(req, timestamp, nonce)
	sig := apiclient.ComputeHMAC(secret, canonical)
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString(sig))
}

// brokerAuthTarget is one of the two broker auth middlewares, wrapped
// around a handler that counts the requests it serves.
type brokerAuthTarget struct {
	name    string
	handler http.Handler
	served  *atomic.Int32
}

// brokerAuthTargets returns the single-key and the multi-key middleware,
// both requiring signatures by secret with the given clock skew.
func brokerAuthTargets(secret []byte, skew time.Duration) []brokerAuthTarget {
	newServed := func() (*atomic.Int32, http.Handler) {
		n := &atomic.Int32{}
		return n, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(http.StatusOK)
		})
	}

	singleServed, singleNext := newServed()
	single := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:      true,
		MaxClockSkew: skew,
		SecretKey:    secret,
	})

	multiServed, multiNext := newServed()
	multi := NewMultiKeyBrokerAuthMiddleware(true, skew, false)
	multi.UpdateKeys([]secretKeyEntry{{hubName: "hub-1", secretKey: secret}})

	return []brokerAuthTarget{
		{name: "single-key", handler: single.Middleware(singleNext), served: singleServed},
		{name: "multi-key", handler: multi.Middleware(multiNext), served: multiServed},
	}
}

const repeatTestPath = "/api/v1/agents/a1/stop"

var repeatTestBody = []byte(`{"projectId":"p1"}`)

// newRepeatTestRequest returns an unsigned POST with the fixed test path
// and body.
func newRepeatTestRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, repeatTestPath, bytes.NewReader(repeatTestBody))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// copyOfSigned returns a new request with the same method, path, body and
// headers as signed: the exact same signed request, sent again.
func copyOfSigned(signed *http.Request) *http.Request {
	req := newRepeatTestRequest()
	req.Header = signed.Header.Clone()
	return req
}

func serveOnce(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestBrokerAuth_ExactRepeatRefused: a valid signed request is served once;
// the exact same request sent again is refused.
func TestBrokerAuth_ExactRepeatRefused(t *testing.T) {
	secret := []byte("repeat-test-secret-32bytes!!!!!!")
	for _, tg := range brokerAuthTargets(secret, 5*time.Minute) {
		t.Run(tg.name, func(t *testing.T) {
			signed := newRepeatTestRequest()
			signRequest(signed, "broker-1", secret)
			again := copyOfSigned(signed)

			if rr := serveOnce(tg.handler, signed); rr.Code != http.StatusOK {
				t.Fatalf("first request: got %d, want 200 (%s)", rr.Code, rr.Body.String())
			}
			rr := serveOnce(tg.handler, again)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("repeated request: got %d, want 401", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "request already served") {
				t.Errorf("repeated request: body %q does not name the cause", rr.Body.String())
			}
			if got := tg.served.Load(); got != 1 {
				t.Errorf("handler served %d requests, want 1", got)
			}
		})
	}
}

// TestBrokerAuth_FreshNonceSameContentAccepted: a second request with the
// same method, path and body but its own nonce and signature is served.
func TestBrokerAuth_FreshNonceSameContentAccepted(t *testing.T) {
	secret := []byte("repeat-test-secret-32bytes!!!!!!")
	for _, tg := range brokerAuthTargets(secret, 5*time.Minute) {
		t.Run(tg.name, func(t *testing.T) {
			for i := 0; i < 3; i++ {
				req := newRepeatTestRequest()
				signRequest(req, "broker-1", secret)
				if rr := serveOnce(tg.handler, req); rr.Code != http.StatusOK {
					t.Fatalf("request %d: got %d, want 200 (%s)", i, rr.Code, rr.Body.String())
				}
			}
			if got := tg.served.Load(); got != 3 {
				t.Errorf("handler served %d requests, want 3", got)
			}
		})
	}
}

// TestBrokerAuth_RepeatRefusedUntilWindowEnds: a repeat is refused for as
// long as the request's timestamp is accepted, and by the clock skew check
// once it is not. Dropping the remembered entry after that point is
// covered by TestSeenOnce_EvictsAfterWindow.
func TestBrokerAuth_RepeatRefusedUntilWindowEnds(t *testing.T) {
	secret := []byte("repeat-test-secret-32bytes!!!!!!")
	const skew = 2 * time.Second
	for _, tg := range brokerAuthTargets(secret, skew) {
		t.Run(tg.name, func(t *testing.T) {
			t.Parallel()
			signed := newRepeatTestRequest()
			signRequest(signed, "broker-1", secret)
			ts, err := strconv.ParseInt(signed.Header.Get(apiclient.HeaderTimestamp), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			windowEnd := time.Unix(ts, 0).Add(skew)

			if rr := serveOnce(tg.handler, copyOfSigned(signed)); rr.Code != http.StatusOK {
				t.Fatalf("first request: got %d, want 200 (%s)", rr.Code, rr.Body.String())
			}
			rr := serveOnce(tg.handler, copyOfSigned(signed))
			if time.Now().After(windowEnd) {
				t.Fatalf("the repeat was sent after the window ended; the environment is too slow for a %v window", skew)
			}
			if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "request already served") {
				t.Fatalf("repeat inside the window: got %d %q, want 401 request already served", rr.Code, rr.Body.String())
			}

			time.Sleep(time.Until(windowEnd.Add(1100 * time.Millisecond)))
			rr = serveOnce(tg.handler, copyOfSigned(signed))
			if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "timestamp outside acceptable range") {
				t.Fatalf("repeat after the window: got %d %q, want 401 timestamp outside acceptable range", rr.Code, rr.Body.String())
			}
			if got := tg.served.Load(); got != 1 {
				t.Errorf("handler served %d requests, want 1", got)
			}
		})
	}
}

// TestBrokerAuth_MissingNonceRefused: a request signed with an empty nonce
// is refused even though its signature is valid.
func TestBrokerAuth_MissingNonceRefused(t *testing.T) {
	secret := []byte("repeat-test-secret-32bytes!!!!!!")
	for _, tg := range brokerAuthTargets(secret, 5*time.Minute) {
		t.Run(tg.name, func(t *testing.T) {
			req := newRepeatTestRequest()
			signRequestWithNonce(req, "broker-1", secret, "")
			rr := serveOnce(tg.handler, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "missing X-Scion-Nonce header") {
				t.Errorf("body %q does not name the missing nonce", rr.Body.String())
			}
			if got := tg.served.Load(); got != 0 {
				t.Errorf("handler served %d requests, want 0", got)
			}
		})
	}
}

// TestBrokerAuth_BadSignatureDoesNotUseNonce: a request refused for its
// signature is not remembered, so the correctly signed request with the
// same nonce is still served.
func TestBrokerAuth_BadSignatureDoesNotUseNonce(t *testing.T) {
	secret := []byte("repeat-test-secret-32bytes!!!!!!")
	for _, tg := range brokerAuthTargets(secret, 5*time.Minute) {
		t.Run(tg.name, func(t *testing.T) {
			nonce := randomTestNonce()
			bad := newRepeatTestRequest()
			signRequestWithNonce(bad, "broker-1", []byte("other-secret-32bytes!!!!!!!!!!!!"), nonce)
			if rr := serveOnce(tg.handler, bad); rr.Code != http.StatusUnauthorized {
				t.Fatalf("wrongly signed request: got %d, want 401", rr.Code)
			}

			good := newRepeatTestRequest()
			signRequestWithNonce(good, "broker-1", secret, nonce)
			if rr := serveOnce(tg.handler, good); rr.Code != http.StatusOK {
				t.Fatalf("correctly signed request: got %d, want 200 (%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestBrokerAuth_ConcurrentRepeatsServedOnce: many copies of one signed
// request sent at once are served exactly once, while distinct requests
// sent at once are all served. Run under -race in CI.
func TestBrokerAuth_ConcurrentRepeatsServedOnce(t *testing.T) {
	secret := []byte("repeat-test-secret-32bytes!!!!!!")
	const n = 32
	for _, tg := range brokerAuthTargets(secret, 5*time.Minute) {
		t.Run(tg.name, func(t *testing.T) {
			signed := newRepeatTestRequest()
			signRequest(signed, "broker-1", secret)
			copies := make([]*http.Request, n)
			distinct := make([]*http.Request, n)
			for i := range copies {
				copies[i] = copyOfSigned(signed)
				distinct[i] = newRepeatTestRequest()
				signRequest(distinct[i], "broker-1", secret)
			}

			var ok, refused atomic.Int32
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(2)
				go func(req *http.Request) {
					defer wg.Done()
					<-start
					switch serveOnce(tg.handler, req).Code {
					case http.StatusOK:
						ok.Add(1)
					case http.StatusUnauthorized:
						refused.Add(1)
					}
				}(copies[i])
				go func(req *http.Request) {
					defer wg.Done()
					<-start
					if rr := serveOnce(tg.handler, req); rr.Code != http.StatusOK {
						t.Errorf("distinct request: got %d, want 200 (%s)", rr.Code, rr.Body.String())
					}
				}(distinct[i])
			}
			close(start)
			wg.Wait()

			if ok.Load() != 1 || refused.Load() != n-1 {
				t.Errorf("copies: %d served, %d refused; want 1 served, %d refused", ok.Load(), refused.Load(), n-1)
			}
			if got := tg.served.Load(); got != n+1 {
				t.Errorf("handler served %d requests, want %d", got, n+1)
			}
		})
	}
}

// respelledSignatures returns the other spellings of the base64 signature
// sig that decode to the same bytes: the last data character before the
// padding carries unused low bits, which the decoder ignores.
func respelledSignatures(t *testing.T, sig string) []string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	want, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.TrimRight(sig, "=")
	last := len(i) - 1
	pos := strings.IndexByte(alphabet, i[last])
	if pos < 0 {
		t.Fatalf("unexpected signature character %q", i[last])
	}
	var out []string
	for low := 0; low < 4; low++ {
		c := alphabet[(pos&^3)|low]
		if c == i[last] {
			continue
		}
		alt := sig[:last] + string(c) + sig[last+1:]
		got, err := base64.StdEncoding.DecodeString(alt)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("spelling %q does not decode to the same signature bytes", alt)
		}
		out = append(out, alt)
	}
	if len(out) != 3 {
		t.Fatalf("got %d other spellings, want 3", len(out))
	}
	return out
}

// TestBrokerAuth_RespelledSignatureRefused: the same signed request with
// its signature header written in another base64 spelling of the same
// bytes is the same request, and is refused after the first is served.
func TestBrokerAuth_RespelledSignatureRefused(t *testing.T) {
	secret := []byte("repeat-test-secret-32bytes!!!!!!")
	for _, tg := range brokerAuthTargets(secret, 5*time.Minute) {
		t.Run(tg.name, func(t *testing.T) {
			signed := newRepeatTestRequest()
			signRequest(signed, "broker-1", secret)
			if rr := serveOnce(tg.handler, copyOfSigned(signed)); rr.Code != http.StatusOK {
				t.Fatalf("first request: got %d, want 200 (%s)", rr.Code, rr.Body.String())
			}
			for _, alt := range respelledSignatures(t, signed.Header.Get(apiclient.HeaderSignature)) {
				req := copyOfSigned(signed)
				req.Header.Set(apiclient.HeaderSignature, alt)
				rr := serveOnce(tg.handler, req)
				if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "request already served") {
					t.Errorf("signature spelled %q: got %d %q, want 401 request already served", alt, rr.Code, rr.Body.String())
				}
			}
			if got := tg.served.Load(); got != 1 {
				t.Errorf("handler served %d requests, want 1", got)
			}
		})
	}
}

// reframedTestHeaders returns the headers of a request signed by secret
// with signed headers "Content-Type;<last>", and a copy whose nonce takes
// in the signed Content-Type line and whose signed headers list only
// <last>. Both have the same canonical signed input, which the function
// asserts, so the copy carries the same valid signature.
func reframedTestHeaders(t *testing.T, newReq func() *http.Request, brokerID string, secret []byte, last string) (signed, reframed http.Header) {
	t.Helper()
	req := newReq()
	req.Header.Set(apiclient.HeaderSignedHeaders, "Content-Type;"+last)
	if err := (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: secret}).ApplyAuth(req); err != nil {
		t.Fatal(err)
	}
	signed = req.Header.Clone()

	reframed = signed.Clone()
	reframed[apiclient.HeaderNonce] = []string{signed.Get(apiclient.HeaderNonce) + "\ncontent-type:" + signed.Get("Content-Type")}
	reframed[apiclient.HeaderSignedHeaders] = []string{last}

	canonical := func(h http.Header) []byte {
		r := newReq()
		r.Header = h
		return apiclient.BuildCanonicalString(r, h.Get(apiclient.HeaderTimestamp), h.Get(apiclient.HeaderNonce))
	}
	if !bytes.Equal(canonical(signed), canonical(reframed)) {
		t.Fatal("the re-framed request does not have the same canonical signed input")
	}
	return signed, reframed
}

// TestBrokerAuth_ReframedNonceRefused: the same signed request with a
// signed header line moved into its nonce (same canonical input, same
// signature, different nonce text) is refused after the first is served.
func TestBrokerAuth_ReframedNonceRefused(t *testing.T) {
	secret := []byte("repeat-test-secret-32bytes!!!!!!")
	for _, tg := range brokerAuthTargets(secret, 5*time.Minute) {
		t.Run(tg.name, func(t *testing.T) {
			signed, reframed := reframedTestHeaders(t, func() *http.Request {
				req := newRepeatTestRequest()
				req.Header.Set("X-A", "a-value")
				return req
			}, "broker-1", secret, "X-A")

			first := newRepeatTestRequest()
			first.Header = signed
			if rr := serveOnce(tg.handler, first); rr.Code != http.StatusOK {
				t.Fatalf("first request: got %d, want 200 (%s)", rr.Code, rr.Body.String())
			}
			again := newRepeatTestRequest()
			again.Header = reframed
			rr := serveOnce(tg.handler, again)
			if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "request already served") {
				t.Fatalf("re-framed request: got %d %q, want 401 request already served", rr.Code, rr.Body.String())
			}
			if got := tg.served.Load(); got != 1 {
				t.Errorf("handler served %d requests, want 1", got)
			}
		})
	}
}

// TestBrokerAuth_SlowBodyRepeatRefused: a copy of a served request that
// passes the clock skew check just before its window ends, but whose body
// arrives only after another request swept past the window, is refused.
func TestBrokerAuth_SlowBodyRepeatRefused(t *testing.T) {
	secret := []byte("repeat-test-secret-32bytes!!!!!!")
	const skew = 3 * time.Second
	for _, tg := range brokerAuthTargets(secret, skew) {
		t.Run(tg.name, func(t *testing.T) {
			t.Parallel()
			signed := newRepeatTestRequest()
			signRequest(signed, "broker-1", secret)
			ts, err := strconv.ParseInt(signed.Header.Get(apiclient.HeaderTimestamp), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			windowEnd := time.Unix(ts, 0).Add(skew)

			if rr := serveOnce(tg.handler, copyOfSigned(signed)); rr.Code != http.StatusOK {
				t.Fatalf("first request: got %d, want 200 (%s)", rr.Code, rr.Body.String())
			}

			// The copy passes the clock skew check 1.5s before its window
			// ends; its body is held until released.
			time.Sleep(time.Until(windowEnd.Add(-1500 * time.Millisecond)))
			pr, pw := io.Pipe()
			t.Cleanup(func() { _ = pw.CloseWithError(io.ErrUnexpectedEOF) })
			slow := httptest.NewRequest(http.MethodPost, repeatTestPath, pr)
			slow.ContentLength = int64(len(repeatTestBody))
			slow.Header = signed.Header.Clone()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- serveOnce(tg.handler, slow) }()

			// Another request, after the copy's window has ended.
			time.Sleep(time.Until(windowEnd.Add(500 * time.Millisecond)))
			other := newRepeatTestRequest()
			signRequest(other, "broker-1", secret)
			if rr := serveOnce(tg.handler, other); rr.Code != http.StatusOK {
				t.Fatalf("other request: got %d, want 200 (%s)", rr.Code, rr.Body.String())
			}

			if _, err := pw.Write(repeatTestBody); err != nil {
				t.Fatal(err)
			}
			_ = pw.Close()
			rr := <-done
			if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "request already served") {
				t.Fatalf("slow copy: got %d %q, want 401 request already served", rr.Code, rr.Body.String())
			}
			if got := tg.served.Load(); got != 2 {
				t.Errorf("handler served %d requests, want 2", got)
			}
		})
	}
}
