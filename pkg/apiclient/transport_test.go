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

package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewTransport(t *testing.T) {
	tr := NewTransport("https://example.com")
	if tr.BaseURL != "https://example.com" {
		t.Errorf("expected base URL 'https://example.com', got %q", tr.BaseURL)
	}
	if tr.HTTPClient == nil {
		t.Error("expected HTTP client to be initialized")
	}
	if tr.UserAgent != "scion-client/1.0" {
		t.Errorf("expected user agent 'scion-client/1.0', got %q", tr.UserAgent)
	}
}

func TestNewTransportWithOptions(t *testing.T) {
	customClient := &http.Client{Timeout: 60 * time.Second}
	tr := NewTransport("https://example.com",
		WithHTTPClient(customClient),
		WithUserAgent("test-client/2.0"),
		WithRetry(3, 2*time.Second),
	)

	if tr.HTTPClient != customClient {
		t.Error("expected custom HTTP client")
	}
	if tr.UserAgent != "test-client/2.0" {
		t.Errorf("expected user agent 'test-client/2.0', got %q", tr.UserAgent)
	}
	if tr.MaxRetries != 3 {
		t.Errorf("expected max retries 3, got %d", tr.MaxRetries)
	}
	if tr.RetryWait != 2*time.Second {
		t.Errorf("expected retry wait 2s, got %v", tr.RetryWait)
	}
}

func TestTransportGet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/test" {
			t.Errorf("expected path /api/v1/test, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	tr := NewTransport(server.URL)
	resp, err := tr.Get(context.Background(), "/api/v1/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestTransportPost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}

		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		if body["name"] != "test" {
			t.Errorf("expected name 'test', got %q", body["name"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "123"})
	}))
	defer server.Close()

	tr := NewTransport(server.URL)
	resp, err := tr.Post(context.Background(), "/api/v1/resources", map[string]string{"name": "test"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected status 201, got %d", resp.StatusCode)
	}
}

func TestTransportWithAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("expected Authorization 'Bearer test-token', got %q", auth)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := NewTransport(server.URL, WithAuth(&BearerAuth{Token: "test-token"}))
	resp, err := tr.Get(context.Background(), "/api/v1/protected", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
}

func TestDecodeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "123",
			"name": "test",
		})
	}))
	defer server.Close()

	tr := NewTransport(server.URL)
	resp, err := tr.Get(context.Background(), "/api/v1/resource", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	type Resource struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	result, err := DecodeResponse[Resource](resp)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if result.ID != "123" {
		t.Errorf("expected id '123', got %q", result.ID)
	}
	if result.Name != "test" {
		t.Errorf("expected name 'test', got %q", result.Name)
	}
}

func TestDecodeResponseError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "not_found",
				"message": "Resource not found",
			},
		})
	}))
	defer server.Close()

	tr := NewTransport(server.URL)
	resp, err := tr.Get(context.Background(), "/api/v1/missing", nil)
	if err != nil {
		t.Fatalf("unexpected network error: %v", err)
	}

	type Resource struct {
		ID string `json:"id"`
	}

	_, err = DecodeResponse[Resource](resp)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T", err)
	}

	if !apiErr.IsNotFound() {
		t.Errorf("expected not found error, got status %d", apiErr.StatusCode)
	}
	if apiErr.Code != "not_found" {
		t.Errorf("expected code 'not_found', got %q", apiErr.Code)
	}
}

// TestDoNoRetry_Ignores5xxRetryConfig proves DoNoRetry sends exactly one
// request even when the Transport is built with WithRetry(>0) and the server
// answers with a 5xx — the case Do itself would retry.
func TestDoNoRetry_Ignores5xxRetryConfig(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	tr := NewTransport(server.URL, WithRetry(3, time.Millisecond))
	resp, err := tr.PostNoRetry(context.Background(), "/api/v1/keys", map[string]string{"keys": "C-c"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected exactly 1 server hit despite WithRetry(3, ...) and a 5xx response, got %d", got)
	}
}

// countingErrorRoundTripper is an http.RoundTripper that always fails
// (simulating a connection-level error, e.g. connection refused/reset) and
// counts how many times it was invoked. Unlike counting hits on a real
// httptest.Server, this counts transport-level attempts directly: a retry
// loop that reuses the same *http.Request (as Transport.Do's does) can
// resend an already-partially-consumed request body, which a real server
// may or may not observe as a distinct "hit" depending on exactly when the
// first attempt's connection died — that timing let a mutated DoNoRetry
// (routed through the retrying Do) pass a server-hit-counting version of
// this test by accident. Counting RoundTrip calls has no such gap: every
// attempt, successful or not, is exactly one call.
type countingErrorRoundTripper struct {
	calls int32
	err   error
}

func (rt *countingErrorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&rt.calls, 1)
	return nil, rt.err
}

// TestDoNoRetry_NoRetryOnNetworkError proves a connection-level failure is
// not retried: exactly one RoundTrip call, even with WithRetry(3, ...)
// configured.
func TestDoNoRetry_NoRetryOnNetworkError(t *testing.T) {
	rt := &countingErrorRoundTripper{err: errors.New("simulated connection refused")}
	tr := NewTransport("http://127.0.0.1:0",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithRetry(3, time.Millisecond))

	_, err := tr.PostNoRetry(context.Background(), "/api/v1/keys", map[string]string{"keys": "C-c"}, nil)
	if err == nil {
		t.Fatal("expected an error from a connection that could not be established")
	}
	if got := atomic.LoadInt32(&rt.calls); got != 1 {
		t.Errorf("expected exactly 1 RoundTrip call (connection loss must not be replayed) despite WithRetry(3, ...), got %d", got)
	}
}

// TestDo_RetriesOnNetworkError is TestDoNoRetry_NoRetryOnNetworkError's
// mutation control: it proves the exact same RoundTripper/assertion
// machinery actually distinguishes retry from no-retry — Do, with the same
// WithRetry(3, ...) configuration, visibly retries a connection-level
// failure (4 RoundTrip calls: the original attempt plus 3 retries). If
// DoNoRetry's test above were ever satisfied by a mutation that routed it
// through Do, this control would be the one demonstrating the test
// machinery itself still works.
func TestDo_RetriesOnNetworkError(t *testing.T) {
	rt := &countingErrorRoundTripper{err: errors.New("simulated connection refused")}
	tr := NewTransport("http://127.0.0.1:0",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithRetry(3, time.Millisecond))

	_, err := tr.Post(context.Background(), "/api/v1/resources", map[string]string{"name": "test"}, nil)
	if err == nil {
		t.Fatal("expected an error from a connection that could not be established")
	}
	if got := atomic.LoadInt32(&rt.calls); got != 4 {
		t.Errorf("expected exactly 4 RoundTrip calls (1 original + 3 retries), got %d", got)
	}
}

// TestDoNoRetry_DoesNotFollowRedirect proves a 3xx response is returned to
// the caller as-is — never automatically re-sent to its Location — and that
// the origin server sees exactly one request even though it issued a
// redirect to itself.
func TestDoNoRetry_DoesNotFollowRedirect(t *testing.T) {
	var hits int32
	var mux http.ServeMux
	mux.HandleFunc("/api/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, "/api/v1/keys-new", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/api/v1/keys-new", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	tr := NewTransport(server.URL)
	resp, err := tr.PostNoRetry(context.Background(), "/api/v1/keys", map[string]string{"keys": "C-c"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("expected the 307 to be returned unchanged, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected exactly 1 server hit (redirect must not be followed/replayed), got %d", got)
	}
}

// TestDo_StillRetriesOn5xx is a control proving Do's own retry behavior is
// unchanged by DoNoRetry's addition — the two must keep different policies.
func TestDo_StillRetriesOn5xx(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := NewTransport(server.URL, WithRetry(3, time.Millisecond))
	resp, err := tr.Post(context.Background(), "/api/v1/resources", map[string]string{"name": "test"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected eventual 200 after retries, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("expected exactly 3 server hits (2 failures + 1 success), got %d", got)
	}
}

// TestDo_RetryResendsFullBodyOn5xx proves a bodied POST that gets a 503 is
// retried with its full body: the server sees two requests carrying the
// identical JSON payload, and the caller gets the second attempt's 200.
// Without the GetBody rewind the second attempt carries a drained body and
// fails client-side (ContentLength=N with Body length 0).
func TestDo_RetryResendsFullBodyOn5xx(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := NewTransport(server.URL, WithRetry(3, time.Millisecond))
	resp, err := tr.Post(context.Background(), "/api/v1/resources", map[string]string{"name": "test"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 after one retry, got %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected exactly 2 server hits (503 then 200), got %d", len(bodies))
	}
	const want = `{"name":"test"}`
	for i, b := range bodies {
		if b != want {
			t.Errorf("attempt %d: server received body %q, want %q", i+1, b, want)
		}
	}
}

// flakyBodyRoundTripper fails the first failures calls — with a transport
// error, or with failStatus when it is non-zero — and answers 200
// afterwards, recording the body every call carried. Unlike a real
// *http.Transport it never rewinds a drained body on its own, so it observes
// exactly what Transport.Do hands each attempt.
type flakyBodyRoundTripper struct {
	failures   int
	failStatus int
	calls      int
	bodies     []string
}

func (rt *flakyBodyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls++
	var b []byte
	if req.Body != nil {
		b, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	rt.bodies = append(rt.bodies, string(b))
	status := http.StatusOK
	if rt.calls <= rt.failures {
		if rt.failStatus == 0 {
			return nil, errors.New("simulated connection reset")
		}
		status = rt.failStatus
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// TestDo_RetryResendsFullBodyOn5xx_RoundTripper is the RoundTripper-level
// twin of TestDo_RetryResendsFullBodyOn5xx. Over a real socket, Go's
// *http.Transport can itself recover a drained body via GetBody on some
// connection paths, which hides a missing rewind in Do; this variant has no
// such fallback, so it fails if Do stops rewinding on the 5xx path.
func TestDo_RetryResendsFullBodyOn5xx_RoundTripper(t *testing.T) {
	rt := &flakyBodyRoundTripper{failures: 2, failStatus: http.StatusServiceUnavailable}
	tr := NewTransport("http://hub.invalid",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithRetry(3, time.Millisecond))

	resp, err := tr.Post(context.Background(), "/api/v1/resources", map[string]string{"name": "test"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 after retries, got %d", resp.StatusCode)
	}
	if rt.calls != 3 {
		t.Fatalf("expected 3 RoundTrip calls (2 x 503 + 1 x 200), got %d", rt.calls)
	}
	const want = `{"name":"test"}`
	for i, b := range rt.bodies {
		if b != want {
			t.Errorf("attempt %d carried body %q, want %q", i+1, b, want)
		}
	}
}

// TestDo_RetryResendsFullBodyOnTransportError covers the transient
// transport-error retry path: the retried attempt carries the same body as
// the failed one.
func TestDo_RetryResendsFullBodyOnTransportError(t *testing.T) {
	rt := &flakyBodyRoundTripper{failures: 2}
	tr := NewTransport("http://hub.invalid",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithRetry(3, time.Millisecond))

	resp, err := tr.Post(context.Background(), "/api/v1/resources", map[string]string{"name": "test"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if rt.calls != 3 {
		t.Fatalf("expected 3 RoundTrip calls (2 failures + 1 success), got %d", rt.calls)
	}
	const want = `{"name":"test"}`
	for i, b := range rt.bodies {
		if b != want {
			t.Errorf("attempt %d carried body %q, want %q", i+1, b, want)
		}
	}
}

// opaqueReader hides the concrete reader type so http.NewRequest cannot
// populate GetBody: the request body is not rewindable.
type opaqueReader struct{ r io.Reader }

func (o opaqueReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func newOpaqueBodyRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url,
		opaqueReader{strings.NewReader(`{"name":"test"}`)})
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if req.GetBody != nil {
		t.Fatal("test setup: expected a request without GetBody")
	}
	return req
}

// TestDo_NonRewindableBodyNotRetriedOn5xx proves a bodied request without
// GetBody is sent once and its first 5xx response is returned unchanged,
// even with WithRetry configured.
func TestDo_NonRewindableBodyNotRetriedOn5xx(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	tr := NewTransport(server.URL, WithRetry(3, time.Millisecond))
	resp, err := tr.Do(context.Background(), newOpaqueBodyRequest(t, server.URL+"/api/v1/resources"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected the first 503 to be returned, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected exactly 1 server hit for a non-rewindable body, got %d", got)
	}
}

// TestDo_NonRewindableBodyNotRetriedOnTransportError is the transport-error
// counterpart: one RoundTrip call, and the first error is returned.
func TestDo_NonRewindableBodyNotRetriedOnTransportError(t *testing.T) {
	rt := &countingErrorRoundTripper{err: errors.New("simulated connection reset")}
	tr := NewTransport("http://hub.invalid",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithRetry(3, time.Millisecond))

	_, err := tr.Do(context.Background(), newOpaqueBodyRequest(t, "http://hub.invalid/api/v1/resources"))
	if err == nil {
		t.Fatal("expected the transport error to be returned")
	}
	if !strings.Contains(err.Error(), "simulated connection reset") {
		t.Errorf("expected the first attempt's error, got %v", err)
	}
	if got := atomic.LoadInt32(&rt.calls); got != 1 {
		t.Errorf("expected exactly 1 RoundTrip call for a non-rewindable body, got %d", got)
	}
}

// TestDoNoRetry_BodiedPOSTHitOnceOn503 pins the no-retry send for a bodied
// POST against a 503 now that Do genuinely retries bodied requests.
func TestDoNoRetry_BodiedPOSTHitOnceOn503(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	tr := NewTransport(server.URL, WithRetry(3, time.Millisecond))
	resp, err := tr.PostNoRetry(context.Background(), "/api/v1/agents/a/keys", map[string]string{"keys": "Escape"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected exactly 1 server hit on the no-retry path, got %d", got)
	}
}
