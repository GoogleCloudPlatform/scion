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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// streamReader hides its length from net/http, so a request built on it
// is sent with chunked framing.
type streamReader struct{ r io.Reader }

func (s *streamReader) Read(p []byte) (int, error) { return s.r.Read(p) }

const bodyCoverageSecret = "body-coverage-secret-0123456789ab"

// bodyCoverageServers serves a handler that records whether it ran behind
// each broker auth middleware.
func bodyCoverageServers(t *testing.T) map[string]struct {
	srv     *httptest.Server
	reached *bool
} {
	t.Helper()
	out := map[string]struct {
		srv     *httptest.Server
		reached *bool
	}{}
	handler := func(reached *bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*reached = true
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		})
	}

	single := new(bool)
	m := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:      true,
		MaxClockSkew: 5 * time.Minute,
		SecretKey:    []byte(bodyCoverageSecret),
	})
	out["single-key"] = struct {
		srv     *httptest.Server
		reached *bool
	}{httptest.NewServer(m.Middleware(handler(single))), single}

	multi := new(bool)
	mk := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, false)
	mk.UpdateKeys([]secretKeyEntry{{hubName: "hub-a", secretKey: []byte(bodyCoverageSecret)}})
	out["multi-key"] = struct {
		srv     *httptest.Server
		reached *bool
	}{httptest.NewServer(mk.Middleware(handler(multi))), multi}

	for _, s := range out {
		t.Cleanup(s.srv.Close)
	}
	return out
}

func signedRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	auth := &apiclient.HMACAuth{BrokerID: "broker-1", SecretKey: []byte(bodyCoverageSecret)}
	if err := auth.ApplyAuth(req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestBrokerAuthMiddlewares_SignedBodyCoverage(t *testing.T) {
	const body = `{"name":"agent-1"}`
	cases := []struct {
		name   string
		build  func(t *testing.T, base string) *http.Request
		wantOK bool
	}{
		{"sized POST verifies", func(t *testing.T, base string) *http.Request {
			return signedRequest(t, http.MethodPost, base+"/api/v1/agents", body)
		}, true},
		{"bodyless GET verifies", func(t *testing.T, base string) *http.Request {
			return signedRequest(t, http.MethodGet, base+"/api/v1/agents/a1/logs", "")
		}, true},
		{"bodyless DELETE verifies", func(t *testing.T, base string) *http.Request {
			return signedRequest(t, http.MethodDelete, base+"/api/v1/agents/a1", "")
		}, true},
		{"bodyless POST verifies", func(t *testing.T, base string) *http.Request {
			return signedRequest(t, http.MethodPost, base+"/api/v1/agents/a1/stop", "")
		}, true},
		{"chunked body added to a signed bodyless POST is refused", func(t *testing.T, base string) *http.Request {
			req := signedRequest(t, http.MethodPost, base+"/api/v1/agents/a1/start", "")
			req.Body = io.NopCloser(&streamReader{strings.NewReader(body)})
			req.ContentLength = -1
			req.GetBody = nil
			return req
		}, false},
		{"chunked body added to a signed bodyless DELETE is refused", func(t *testing.T, base string) *http.Request {
			req := signedRequest(t, http.MethodDelete, base+"/api/v1/agents/a1", "")
			req.Body = io.NopCloser(&streamReader{strings.NewReader(body)})
			req.ContentLength = -1
			req.GetBody = nil
			return req
		}, false},
		{"signed sized body re-sent chunked is refused", func(t *testing.T, base string) *http.Request {
			req := signedRequest(t, http.MethodPost, base+"/api/v1/agents", body)
			req.Body = io.NopCloser(&streamReader{strings.NewReader(body)})
			req.ContentLength = -1
			req.GetBody = nil
			return req
		}, false},
		{"swapped sized body is refused", func(t *testing.T, base string) *http.Request {
			req := signedRequest(t, http.MethodPost, base+"/api/v1/agents", body)
			swapped := strings.Replace(body, "agent-1", "agent-2", 1)
			req.Body = io.NopCloser(strings.NewReader(swapped))
			req.ContentLength = int64(len(swapped))
			req.GetBody = nil
			return req
		}, false},
	}
	for name, s := range bodyCoverageServers(t) {
		for _, c := range cases {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				*s.reached = false
				resp, err := s.srv.Client().Do(c.build(t, s.srv.URL))
				if err != nil {
					t.Fatal(err)
				}
				msg, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if c.wantOK {
					if resp.StatusCode != http.StatusOK || !*s.reached {
						t.Fatalf("status %d reached %v: %s", resp.StatusCode, *s.reached, msg)
					}
					return
				}
				if resp.StatusCode != http.StatusUnauthorized || *s.reached {
					t.Fatalf("status %d reached %v, want 401 and handler not reached: %s", resp.StatusCode, *s.reached, msg)
				}
			})
		}
	}
}

// The framing refusal names its reason.
func TestBrokerAuthMiddlewares_ChunkedSignedBodyMessage(t *testing.T) {
	for name, s := range bodyCoverageServers(t) {
		t.Run(name, func(t *testing.T) {
			req := signedRequest(t, http.MethodPost, s.srv.URL+"/api/v1/agents/a1/start", "")
			req.Body = io.NopCloser(&streamReader{strings.NewReader(`{}`)})
			req.ContentLength = -1
			resp, err := s.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			msg, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(msg), apiclient.ErrUnsizedSignedBody.Error()) {
				t.Fatalf("status %d body %s", resp.StatusCode, msg)
			}
		})
	}
}
