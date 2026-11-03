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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
)

// bodyLimitCountingBody yields size bytes of 'x' without allocating them and
// counts how many bytes were read.
type bodyLimitCountingBody struct {
	size int64
	read int64
}

func (b *bodyLimitCountingBody) Read(p []byte) (int, error) {
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

func (b *bodyLimitCountingBody) Close() error { return nil }

// setupBodyLimitBroker registers a broker with an active secret and returns
// the service, the broker ID, and a signer for requests with a body.
func setupBodyLimitBroker(t *testing.T) (*BrokerAuthService, string, func(method, path string, body []byte) *http.Request) {
	t.Helper()
	svc, s := setupTestBrokerAuthService(t)
	brokerID, _ := setupSignedBrokerRequest(t, svc, s)
	secretKey := []byte("obo-test-secret-key-32-bytes!!!!") // setupSignedBrokerRequest's secret
	n := 0
	sign := func(method, path string, body []byte) *http.Request {
		n++
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := "body-limit-nonce-" + strconv.Itoa(n)
		req.Header.Set(HeaderBrokerID, brokerID)
		req.Header.Set(HeaderTimestamp, ts)
		req.Header.Set(HeaderNonce, nonce)
		h := hmac.New(sha256.New, secretKey)
		h.Write(svc.buildCanonicalString(req, ts, nonce))
		req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(h.Sum(nil)))
		return req
	}
	return svc, brokerID, sign
}

// overCapRequest returns a request from a registered broker, with current
// HMAC headers, whose declared body is one byte over the cap. The signature
// does not cover the body.
func overCapRequest(sign func(method, path string, body []byte) *http.Request) (*http.Request, *bodyLimitCountingBody) {
	req := sign(http.MethodPost, "/api/v1/runtime-brokers/heartbeat", nil)
	body := &bodyLimitCountingBody{size: apiclient.MaxSignedRequestBodyBytes + 1}
	req.Body = body
	req.ContentLength = body.size
	return req, body
}

// An over-cap body is refused with 413 before it is read or the signature is
// checked, by both hub broker middlewares; the audited variant records the
// refusal as malformed with a distinguishing failure reason.
func TestBrokerAuthBodyLimit_HubMiddlewaresRefuseOverCapBeforeRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		audited bool
	}{{name: "plain"}, {name: "audited", audited: true}} {
		t.Run(tc.name, func(t *testing.T) {
			svc, brokerID, sign := setupBodyLimitBroker(t)
			logger := &mockAuditLogger{}
			mw := BrokerAuthMiddleware(svc)
			if tc.audited {
				mw = AuditableBrokerAuthMiddleware(svc, logger)
			}
			nextCalled := false
			h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))
			req, body := overCapRequest(sign)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body %s", rec.Body.String())
			assert.Zero(t, body.read, "body bytes read before refusing")
			assert.False(t, nextCalled, "next handler called")
			assert.Contains(t, rec.Body.String(), `"payload_too_large"`)
			if tc.audited {
				require.Len(t, logger.brokerEvents, 1, "an audit event is emitted for the refusal")
				ev := logger.brokerEvents[0]
				assert.Equal(t, BrokerAuthEventAuthFailure, ev.EventType)
				assert.Equal(t, brokerID, ev.BrokerID)
				assert.Equal(t, auditevent.BrokerFailureMalformed, ev.FailureCode)
				assert.Equal(t, apiclient.SignedBodyTooLargeMessage, ev.FailReason)
			}
		})
	}
}

// A signed body at the cap (the largest legitimate size) verifies through
// both middlewares, and the handler reads all of it.
func TestBrokerAuthBodyLimit_HubSignedBodyAtCapVerifies(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), int(apiclient.MaxSignedRequestBodyBytes))
	for _, audited := range []bool{false, true} {
		svc, _, sign := setupBodyLimitBroker(t)
		mw := BrokerAuthMiddleware(svc)
		if audited {
			mw = AuditableBrokerAuthMiddleware(svc, &mockAuditLogger{})
		}
		var got []byte
		var readErr error
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, readErr = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sign(http.MethodPost, "/api/v1/runtime-brokers/heartbeat", payload))

		assert.Equal(t, http.StatusOK, rec.Code, "audited=%v body %s", audited, rec.Body.String())
		assert.NoError(t, readErr)
		assert.Equal(t, len(payload), len(got), "audited=%v handler body length", audited)
	}
}

// Small-body and empty-body signed requests still verify.
func TestBrokerAuthBodyLimit_HubSmallAndEmptySignedRequestsVerify(t *testing.T) {
	svc, _, sign := setupBodyLimitBroker(t)
	_, err := svc.ValidateBrokerSignature(context.Background(), sign(http.MethodPost, "/api/v1/x", []byte(`{"status":"online"}`)))
	assert.NoError(t, err, "small body")
	_, err = svc.ValidateBrokerSignature(context.Background(), sign(http.MethodGet, "/api/v1/x", nil))
	assert.NoError(t, err, "empty GET")
}

// The rotation-aware verifier applies the same cap.
func TestBrokerAuthBodyLimit_HubRotationVerifier(t *testing.T) {
	svc, _, sign := setupBodyLimitBroker(t)
	req, body := overCapRequest(sign)
	_, err := svc.ValidateBrokerSignatureWithRotation(context.Background(), req)
	assert.True(t, errors.Is(err, errBrokerAuthBodyTooLarge), "err = %v", err)
	assert.Zero(t, body.read, "body bytes read before refusing")

	payload := bytes.Repeat([]byte("b"), int(apiclient.MaxSignedRequestBodyBytes))
	_, err = svc.ValidateBrokerSignatureWithRotation(context.Background(), sign(http.MethodPost, "/api/v1/x", payload))
	assert.NoError(t, err, "signed body at the cap")

	// The cap runs before the secret lookup: with a store whose lookup
	// fails, an over-cap body still gets the body-size refusal.
	faulty := NewBrokerAuthService(DefaultBrokerAuthConfig(), &secretFaultStore{Store: svc.store})
	t.Cleanup(faulty.Close)
	req, body = overCapRequest(sign)
	_, err = faulty.ValidateBrokerSignatureWithRotation(context.Background(), req)
	assert.True(t, errors.Is(err, errBrokerAuthBodyTooLarge), "err = %v", err)
	assert.Zero(t, body.read, "body bytes read before refusing")
}

// A body longer than it declared is read at most to the cap and does not
// verify.
func TestBrokerAuthBodyLimit_HubMisdeclaredBodyBounded(t *testing.T) {
	svc, _, sign := setupBodyLimitBroker(t)
	req := sign(http.MethodPost, "/api/v1/x", nil)
	body := &bodyLimitCountingBody{size: 4 * apiclient.MaxSignedRequestBodyBytes}
	req.Body = body
	req.ContentLength = 16
	_, err := svc.ValidateBrokerSignature(context.Background(), req)
	assert.True(t, errors.Is(err, errBrokerAuthInvalidSignature), "err = %v", err)
	assert.LessOrEqual(t, body.read, apiclient.MaxSignedRequestBodyBytes+1, "body bytes read")
}

// The upgrade path, when no middleware identity is present, answers an
// over-cap body with 413 too.
func TestBrokerAuthBodyLimit_HubUpgradePath(t *testing.T) {
	svc, _, sign := setupBodyLimitBroker(t)
	srv := &Server{brokerAuthService: svc}
	req, body := overCapRequest(sign)
	rec := httptest.NewRecorder()
	_, ok := srv.authenticateBrokerUpgrade(rec, req)
	assert.False(t, ok)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Zero(t, body.read)
}

// Requests without broker headers bypass the broker middleware and are not
// capped by it.
func TestBrokerAuthBodyLimit_HubNonBrokerRequestUnaffected(t *testing.T) {
	svc, _, _ := setupBodyLimitBroker(t)
	var n int64
	h := BrokerAuthMiddleware(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ = io.Copy(io.Discard, r.Body)
	}))
	body := &bodyLimitCountingBody{size: apiclient.MaxSignedRequestBodyBytes + 10}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/x", nil)
	req.Body = body
	req.ContentLength = body.size
	h.ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, body.size, n)
}

// The audit classification of an over-cap body is the existing malformed
// code (the closed failure_code enum is not extended).
func TestBrokerAuthBodyLimit_FailureCodeIsMalformed(t *testing.T) {
	assert.Equal(t, auditevent.BrokerFailureMalformed, brokerAuthFailureCode(errBrokerAuthBodyTooLarge))
	assert.True(t, strings.Contains(errBrokerAuthBodyTooLarge.Error(), "2 MiB"))
}
