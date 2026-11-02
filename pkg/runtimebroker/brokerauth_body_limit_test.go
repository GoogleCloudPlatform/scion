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
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// countingBody yields size bytes of 'x' without allocating them and counts
// how many bytes were read.
type countingBody struct {
	size int64
	read int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.read >= b.size {
		return 0, io.EOF
	}
	n := int64(len(p))
	if rem := b.size - b.read; n > rem {
		n = rem
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'x'
	}
	b.read += n
	return int(n), nil
}

func (b *countingBody) Close() error { return nil }

var bodyLimitSecret = []byte("test-secret-key-32bytes!12345678")

// bodyLimitMiddlewares returns both broker HMAC middlewares, configured with
// bodyLimitSecret, wrapping next.
func bodyLimitMiddlewares(allowUnauth bool) map[string]func(next http.Handler) http.Handler {
	return map[string]func(next http.Handler) http.Handler{
		"single-key": func(next http.Handler) http.Handler {
			return NewBrokerAuthMiddleware(BrokerAuthConfig{
				Enabled:              true,
				MaxClockSkew:         5 * time.Minute,
				SecretKey:            bodyLimitSecret,
				AllowUnauthenticated: allowUnauth,
			}).Middleware(next)
		},
		"multi-key": func(next http.Handler) http.Handler {
			m := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, allowUnauth)
			m.UpdateKeys([]secretKeyEntry{{hubName: "hub-a", secretKey: bodyLimitSecret}})
			return m.Middleware(next)
		},
	}
}

// setUnverifiableHMACHeaders sets well-formed HMAC headers with a current
// timestamp and a signature that does not verify.
func setUnverifiableHMACHeaders(req *http.Request) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set(apiclient.HeaderBrokerID, "some-broker")
	req.Header.Set(apiclient.HeaderTimestamp, ts)
	req.Header.Set(apiclient.HeaderNonce, "nonce-"+ts)
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString([]byte("not-a-valid-signature")))
}

// An unsigned request whose declared body is over the cap is refused with
// 413 before the body is read or the signature checked.
func TestBrokerAuthBodyLimit_OverCapDeclaredRefusedBeforeRead(t *testing.T) {
	for name, wrap := range bodyLimitMiddlewares(false) {
		t.Run(name, func(t *testing.T) {
			nextCalled := false
			h := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
			}))
			body := &countingBody{size: apiclient.MaxSignedRequestBodyBytes + 1}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
			req.Body = body
			req.ContentLength = body.size
			setUnverifiableHMACHeaders(req)

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d, want 413; body %s", rr.Code, rr.Body.String())
			}
			if body.read != 0 {
				t.Errorf("read %d body bytes before refusing, want 0", body.read)
			}
			if nextCalled {
				t.Error("next handler called for an over-cap body")
			}
			if !strings.Contains(rr.Body.String(), `"payload_too_large"`) {
				t.Errorf("error code missing from body %s", rr.Body.String())
			}
		})
	}
}

// A body longer than it declared is read at most to the cap; the signature
// then fails.
func TestBrokerAuthBodyLimit_MisdeclaredBodyBoundedAtCap(t *testing.T) {
	for name, wrap := range bodyLimitMiddlewares(false) {
		t.Run(name, func(t *testing.T) {
			nextCalled := false
			h := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
			}))
			body := &countingBody{size: 4 * apiclient.MaxSignedRequestBodyBytes}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
			req.Body = body
			req.ContentLength = 16
			setUnverifiableHMACHeaders(req)

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body %s", rr.Code, rr.Body.String())
			}
			if body.read > apiclient.MaxSignedRequestBodyBytes+1 {
				t.Errorf("read %d body bytes, want at most %d", body.read, apiclient.MaxSignedRequestBodyBytes+1)
			}
			if nextCalled {
				t.Error("next handler called for an unverified request")
			}
		})
	}
}

// signBodyLimitRequest signs req with bodyLimitSecret.
func signBodyLimitRequest(req *http.Request) {
	signRequest(req, "hub-broker-id", bodyLimitSecret)
}

// A signed body at the cap (the largest legitimate size) verifies and the
// handler reads all of it.
func TestBrokerAuthBodyLimit_SignedBodyAtCapVerifies(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), int(apiclient.MaxSignedRequestBodyBytes))
	for name, wrap := range bodyLimitMiddlewares(false) {
		t.Run(name, func(t *testing.T) {
			var got []byte
			var readErr error
			h := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, readErr = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(payload))
			signBodyLimitRequest(req)

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rr.Code, rr.Body.String())
			}
			if readErr != nil || !bytes.Equal(got, payload) {
				t.Errorf("handler read %d bytes (err %v), want the full %d-byte body", len(got), readErr, len(payload))
			}
		})
	}
}

// A signed request with a small body, and a signed GET with no body, verify.
func TestBrokerAuthBodyLimit_SmallAndEmptySignedRequestsVerify(t *testing.T) {
	for name, wrap := range bodyLimitMiddlewares(false) {
		t.Run(name, func(t *testing.T) {
			h := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			post := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(`{"name":"a"}`))
			signBodyLimitRequest(post)
			get := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
			signBodyLimitRequest(get)
			for _, req := range []*http.Request{post, get} {
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					t.Errorf("%s: status = %d, want 200; body %s", req.Method, rr.Code, rr.Body.String())
				}
			}
		})
	}
}

// A signed request of unknown length (ContentLength < 0) carrying an
// over-cap body is refused before any of it is read and the handler does not
// run. The cap installs its reader for such a body
// (TestLimitSignedRequestBody_ReadsBoundedAtCap in pkg/apiclient covers that
// reader directly); the refusal itself comes from the signed-body framing
// check that follows the cap, so this test guards the composition of the
// two, not the cap alone.
func TestBrokerAuthBodyLimit_UnknownLengthBodyNotRead(t *testing.T) {
	for name, wrap := range bodyLimitMiddlewares(false) {
		t.Run(name, func(t *testing.T) {
			body := &countingBody{size: 4 * apiclient.MaxSignedRequestBodyBytes}
			nextCalled := false
			h := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				_, _ = io.Copy(io.Discard, r.Body)
			}))
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
			req.ContentLength = -1
			signBodyLimitRequest(req)
			req.Body = body

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401; body %s", rr.Code, rr.Body.String())
			}
			if body.read != 0 {
				t.Errorf("read %d body bytes, want 0", body.read)
			}
			if nextCalled {
				t.Error("next handler called for a body of unknown length")
			}
		})
	}
}

// Requests that skip HMAC verification are not capped: health checks and,
// when allowed, requests with no HMAC headers.
func TestBrokerAuthBodyLimit_UnverifiedPathsUnaffected(t *testing.T) {
	for name, wrap := range bodyLimitMiddlewares(true) {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{"/healthz", "/api/v1/info"} {
				body := &countingBody{size: apiclient.MaxSignedRequestBodyBytes + 10}
				var n int64
				h := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n, _ = io.Copy(io.Discard, r.Body)
					w.WriteHeader(http.StatusOK)
				}))
				req := httptest.NewRequest(http.MethodPost, path, nil)
				req.Body = body
				req.ContentLength = body.size

				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)

				if rr.Code != http.StatusOK || n != body.size {
					t.Errorf("%s: status %d, handler read %d of %d bytes; want 200 and the full body", path, rr.Code, n, body.size)
				}
			}
		})
	}
}
