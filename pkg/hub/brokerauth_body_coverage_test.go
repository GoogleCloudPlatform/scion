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

//go:build !no_sqlite

package hub

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// streamBody hides its length from net/http, so a request built on it is
// sent with chunked framing.
type streamBody struct{ r io.Reader }

func (s *streamBody) Read(p []byte) (int, error) { return s.r.Read(p) }

// seedBodyCoverageBroker creates a broker with an active secret and returns
// a signer for it.
func seedBodyCoverageBroker(t *testing.T, s store.Store) *apiclient.HMACAuth {
	t.Helper()
	ctx := context.Background()
	brokerID := uuid.New().String()
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "bc-" + brokerID[:8], Slug: "bc-" + brokerID[:8], Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	secret := []byte("body-coverage-secret-32-bytes!!!")
	if err := s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID: brokerID, SecretKey: secret,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256, Status: store.BrokerSecretStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	return &apiclient.HMACAuth{BrokerID: brokerID, SecretKey: secret}
}

func signBodyCoverage(t *testing.T, auth *apiclient.HMACAuth, method, url, body string) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.ApplyAuth(req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestBrokerAuthMiddleware_SignedBodyCoverage(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	auth := seedBodyCoverageBroker(t, s)
	const body = `{"status":"online"}`

	reached := false
	srv := httptest.NewServer(BrokerAuthMiddleware(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})))
	t.Cleanup(srv.Close)

	stream := func(req *http.Request, b string) *http.Request {
		req.Body = io.NopCloser(&streamBody{strings.NewReader(b)})
		req.ContentLength = -1
		req.GetBody = nil
		return req
	}
	cases := []struct {
		name   string
		build  func() *http.Request
		wantOK bool
	}{
		{"sized POST verifies", func() *http.Request {
			return signBodyCoverage(t, auth, http.MethodPost, srv.URL+"/api/v1/runtime-brokers/x/heartbeat", body)
		}, true},
		{"bodyless GET verifies", func() *http.Request {
			return signBodyCoverage(t, auth, http.MethodGet, srv.URL+"/api/v1/runtime-brokers", "")
		}, true},
		{"bodyless DELETE verifies", func() *http.Request {
			return signBodyCoverage(t, auth, http.MethodDelete, srv.URL+"/api/v1/runtime-brokers/x", "")
		}, true},
		{"chunked body added to a signed bodyless DELETE is refused", func() *http.Request {
			return stream(signBodyCoverage(t, auth, http.MethodDelete, srv.URL+"/api/v1/runtime-brokers/x", ""), body)
		}, false},
		{"chunked body added to a signed bodyless GET is refused", func() *http.Request {
			return stream(signBodyCoverage(t, auth, http.MethodGet, srv.URL+"/api/v1/runtime-brokers", ""), body)
		}, false},
		{"signed sized body re-sent chunked is refused", func() *http.Request {
			return stream(signBodyCoverage(t, auth, http.MethodPost, srv.URL+"/api/v1/runtime-brokers/x/heartbeat", body), body)
		}, false},
		{"swapped sized body is refused", func() *http.Request {
			req := signBodyCoverage(t, auth, http.MethodPost, srv.URL+"/api/v1/runtime-brokers/x/heartbeat", body)
			swapped := strings.Replace(body, "online", "remove", 1)
			req.Body = io.NopCloser(strings.NewReader(swapped))
			req.ContentLength = int64(len(swapped))
			req.GetBody = nil
			return req
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached = false
			resp, err := srv.Client().Do(c.build())
			if err != nil {
				t.Fatal(err)
			}
			msg, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if c.wantOK {
				if resp.StatusCode != http.StatusOK || !reached {
					t.Fatalf("status %d reached %v: %s", resp.StatusCode, reached, msg)
				}
				return
			}
			if resp.StatusCode != http.StatusUnauthorized || reached {
				t.Fatalf("status %d reached %v, want 401 and handler not reached: %s", resp.StatusCode, reached, msg)
			}
		})
	}
}

// The body hash alone (without the framing check) defeats a body added to
// a signed bodyless request: a synthetic request that declares
// ContentLength 0 but carries bytes passes the framing check and must
// still fail the signature.
func TestValidateBrokerSignature_AddedBodyFailsWithoutFramingCheck(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	auth := seedBodyCoverageBroker(t, s)

	signed := signBodyCoverage(t, auth, http.MethodDelete, "http://hub.test/api/v1/runtime-brokers/x", "")
	req := httptest.NewRequest(http.MethodDelete, "http://hub.test/api/v1/runtime-brokers/x", nil)
	req.Header = signed.Header.Clone()
	req.Body = io.NopCloser(strings.NewReader(`{"injected":true}`))
	req.ContentLength = 0
	if err := apiclient.CheckSignedBodyFraming(req); err != nil {
		t.Fatalf("precondition: framing check refused: %v", err)
	}
	_, err := svc.ValidateBrokerSignature(context.Background(), req)
	if !errors.Is(err, errBrokerAuthInvalidSignature) {
		t.Fatalf("err = %v, want invalid signature", err)
	}
}

func TestValidateBrokerSignature_RefusesUnsizedBody(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	auth := seedBodyCoverageBroker(t, s)
	validators := map[string]func(context.Context, *http.Request) (BrokerIdentity, error){
		"ValidateBrokerSignature":             svc.ValidateBrokerSignature,
		"ValidateBrokerSignatureWithRotation": svc.ValidateBrokerSignatureWithRotation,
	}
	for name, validate := range validators {
		t.Run(name, func(t *testing.T) {
			signed := signBodyCoverage(t, auth, http.MethodPost, "http://hub.test/api/v1/runtime-brokers/x/heartbeat", "")
			for _, te := range [][]string{nil, {"chunked"}} {
				req := httptest.NewRequest(http.MethodPost, "http://hub.test/api/v1/runtime-brokers/x/heartbeat", nil)
				req.Header = signed.Header.Clone()
				req.Body = io.NopCloser(strings.NewReader(`{}`))
				req.ContentLength = -1
				req.TransferEncoding = te
				if _, err := validate(context.Background(), req); !errors.Is(err, apiclient.ErrUnsizedSignedBody) {
					t.Fatalf("TransferEncoding %v: err = %v, want ErrUnsizedSignedBody", te, err)
				}
			}
			// The sized form of the same signer still verifies.
			ok := signBodyCoverage(t, auth, http.MethodPost, "http://hub.test/api/v1/runtime-brokers/x/heartbeat", `{"a":1}`)
			if _, err := validate(context.Background(), ok); err != nil {
				t.Fatalf("sized request: %v", err)
			}
		})
	}
}
