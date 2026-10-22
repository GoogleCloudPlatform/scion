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
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type trackedRequestBody struct {
	io.Reader
	reads, closes int
	closeErr      error
}

func (b *trackedRequestBody) Read(p []byte) (int, error) {
	b.reads++
	if b.closes != 0 {
		return 0, os.ErrClosed
	}
	return b.Reader.Read(p)
}

func (b *trackedRequestBody) Close() error {
	b.closes++
	return b.closeErr
}

type requestBodyAuthFunc func(*http.Request) error

func (f requestBodyAuthFunc) ApplyAuth(req *http.Request) error { return f(req) }
func (requestBodyAuthFunc) Refresh() (bool, error)              { return false, nil }

type requestBodyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f requestBodyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAuthenticatedHTTPClient_AuthErrorClosesBody(t *testing.T) {
	authErr := errors.New("credentials unavailable")
	for _, name := range []string{"body", "empty_body", "close_error", "nil_body", "no_body"} {
		t.Run(name, func(t *testing.T) {
			tracked := &trackedRequestBody{Reader: strings.NewReader("payload")}
			var body io.ReadCloser = tracked
			switch name {
			case "empty_body":
				tracked.Reader = strings.NewReader("")
			case "close_error":
				tracked.closeErr = errors.New("close failed")
			case "nil_body":
				body = nil
			case "no_body":
				body = http.NoBody
			}
			authCalls := 0
			auth := requestBodyAuthFunc(func(req *http.Request) error {
				authCalls++
				req.Header.Set("Authorization", "Bearer test-token")
				return authErr
			})
			base := requestBodyRoundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("failed authentication must not reach the underlying transport")
				return nil, nil
			})
			client := NewTransport("https://hub.invalid", WithAuth(auth),
				WithHTTPClient(&http.Client{Transport: base})).AuthenticatedHTTPClient()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://hub.invalid/upload", body)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if resp != nil || !errors.Is(err, authErr) {
				t.Fatalf("response = %v, error = %v; want original auth error", resp, err)
			}
			if authCalls != 1 || tracked.reads != 0 {
				t.Errorf("auth calls = %d, body reads = %d; want 1, 0", authCalls, tracked.reads)
			}
			wantCloses := 0
			if body == tracked {
				wantCloses = 1
			}
			if tracked.closes != wantCloses {
				t.Errorf("body close calls = %d, want %d", tracked.closes, wantCloses)
			}
			if req.Header.Get("Authorization") != "" || req.Header.Get("User-Agent") != "" {
				t.Error("authentication mutated the caller's headers")
			}
		})
	}
}

func TestAuthenticatedHTTPClient_AuthErrorClosesFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "request-body-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.WriteString("payload"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	base := requestBodyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid HMAC credentials must prevent sending the request")
		return nil, nil
	})
	client := NewTransport("https://hub.invalid", WithAuth(&HMACAuth{}),
		WithHTTPClient(&http.Client{Transport: base})).AuthenticatedHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://hub.invalid/upload", file)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := client.Do(req); resp != nil || err == nil || !strings.Contains(err.Error(), "HMAC auth requires brokerID and secretKey") {
		t.Fatalf("response = %v, error = %v; want HMAC validation error", resp, err)
	}
	if _, err := file.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Errorf("reading request file after auth failure: %v; want os.ErrClosed", err)
	}
}

func TestAuthenticatedHTTPClient_DelegatesBodyOwnership(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		for _, transportFails := range []bool{false, true} {
			name := "no_auth"
			if authenticated {
				name = "authenticated"
			}
			if transportFails {
				name += "/transport_error"
			} else {
				name += "/success"
			}
			t.Run(name, func(t *testing.T) {
				body := &trackedRequestBody{Reader: strings.NewReader("payload")}
				transportErr := errors.New("transport failed")
				calls := 0
				base := requestBodyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if body.closes != 0 || req.Body != body {
						t.Fatal("request body closed or replaced before delegation")
					}
					if got := req.Header.Get("Authorization"); (got == "Bearer test-token") != authenticated {
						t.Errorf("unexpected Authorization header %q", got)
					}
					data, err := io.ReadAll(req.Body)
					_ = req.Body.Close()
					if err != nil || string(data) != "payload" {
						t.Fatalf("request body = %q, error = %v", data, err)
					}
					if transportFails {
						return nil, transportErr
					}
					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
				})
				var auth Authenticator
				if authenticated {
					auth = &BearerAuth{Token: "test-token"}
				}
				client := NewTransport("https://hub.invalid", WithAuth(auth),
					WithHTTPClient(&http.Client{Transport: base})).AuthenticatedHTTPClient()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://hub.invalid/upload", body)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if transportFails {
					if resp != nil || !errors.Is(err, transportErr) {
						t.Fatalf("response = %v, error = %v; want transport error", resp, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					_ = resp.Body.Close()
				}
				if calls != 1 || body.closes != 1 {
					t.Errorf("transport calls = %d, body closes = %d; want 1, 1", calls, body.closes)
				}
			})
		}
	}
}
