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

package transportauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeIAPMiddleware simulates Google IAP by rejecting requests that lack a
// valid transport OIDC token. It checks both Authorization and
// Proxy-Authorization headers (IAP accepts either).
func fakeIAPMiddleware(expectedToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if auth := r.Header.Get("Proxy-Authorization"); auth != "" {
			token = auth
		} else if auth := r.Header.Get("Authorization"); auth != "" {
			token = auth
		}

		expected := "Bearer " + expectedToken
		if token != expected {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html><body>Sign in with Google</body></html>"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestFakeIAP_RejectsWithoutToken(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := httptest.NewServer(fakeIAPMiddleware("valid-token", backend))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestFakeIAP_AcceptsWithToken(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := httptest.NewServer(fakeIAPMiddleware("valid-token", backend))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/health", nil)
	req.Header.Set("Proxy-Authorization", "Bearer valid-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestWrap_ThroughFakeIAP(t *testing.T) {
	const testToken = "test-oidc-token"

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := httptest.NewServer(fakeIAPMiddleware(testToken, backend))
	defer srv.Close()

	// Create a token source that returns our test token
	src := NewInjectedSource()
	src.SetToken(testToken, time.Now().Add(time.Hour))

	// Wrap transport with IAP mode (Proxy-Authorization)
	client := &http.Client{
		Transport: Wrap(http.DefaultTransport, src, HeaderProxyAuthorization),
	}

	resp, err := client.Get(srv.URL + "/api/v1/runtime-brokers/heartbeat")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestWrap_FailsThroughFakeIAP_WrongToken(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(fakeIAPMiddleware("correct-token", backend))
	defer srv.Close()

	src := NewInjectedSource()
	src.SetToken("wrong-token", time.Now().Add(time.Hour))

	client := &http.Client{
		Transport: Wrap(http.DefaultTransport, src, HeaderProxyAuthorization),
	}

	resp, err := client.Get(srv.URL + "/api/v1/runtime-brokers/heartbeat")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestApplyHeaders_ThroughFakeIAP(t *testing.T) {
	const testToken = "ws-oidc-token"

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("connected"))
	})

	srv := httptest.NewServer(fakeIAPMiddleware(testToken, backend))
	defer srv.Close()

	src := NewInjectedSource()
	src.SetToken(testToken, time.Now().Add(time.Hour))

	headers := http.Header{}
	if err := ApplyHeaders(headers, src, HeaderProxyAuthorization); err != nil {
		t.Fatalf("ApplyHeaders failed: %v", err)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/connect", nil)
	for k, v := range headers {
		req.Header[k] = v
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestApplyHeaders_WithExistingHMACHeaders(t *testing.T) {
	const testToken = "hmac-plus-oidc"

	src := NewInjectedSource()
	src.SetToken(testToken, time.Now().Add(time.Hour))

	headers := http.Header{}
	// Simulate HMAC headers already present
	headers.Set("Authorization", "HMAC broker-id:signature")
	headers.Set("X-Scion-Broker-ID", "broker-id")

	// Apply transport auth with IAP mode (Proxy-Authorization)
	if err := ApplyHeaders(headers, src, HeaderProxyAuthorization); err != nil {
		t.Fatalf("ApplyHeaders failed: %v", err)
	}

	// HMAC Authorization header should be preserved
	if got := headers.Get("Authorization"); got != "HMAC broker-id:signature" {
		t.Errorf("Authorization header modified: %q", got)
	}

	// Proxy-Authorization should have the OIDC token
	if got := headers.Get("Proxy-Authorization"); got != "Bearer "+testToken {
		t.Errorf("Proxy-Authorization: expected %q, got %q", "Bearer "+testToken, got)
	}
}
