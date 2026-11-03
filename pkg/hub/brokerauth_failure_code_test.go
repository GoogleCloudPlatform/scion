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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Q3 pins (remaining-audit P4 slice 1): brokerauth.go's errors became
// unexported sentinels wrapped with %w, so auth_failure can be classified
// without text matching. writeBrokerAuthError sends err.Error() to the
// client, so the response bodies must be byte-for-byte what they were.
// Every message below is the literal text at fce92df9.

// secretFaultStore fails the broker secret lookups.
type secretFaultStore struct {
	store.Store
}

func (f *secretFaultStore) GetBrokerSecret(context.Context, string) (*store.BrokerSecret, error) {
	return nil, errors.New("db down")
}

func (f *secretFaultStore) GetActiveSecrets(context.Context, string) ([]*store.BrokerSecret, error) {
	return nil, errors.New("db down")
}

// seedBrokerWithSecretState creates a broker whose only secret has the
// given status and expiry.
func seedBrokerWithSecretState(t *testing.T, s store.Store, status string, expiresAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	brokerID := uuid.New().String()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "q3-" + brokerID[:8], Slug: "q3-" + brokerID[:8], Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}))
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID: brokerID, SecretKey: []byte("q3-secret-key-32-bytes-long!!!!!"),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256, Status: status, ExpiresAt: expiresAt,
	}))
	return brokerID
}

// unsignedBrokerRequest carries every broker header with a dummy signature.
func unsignedBrokerRequest(brokerID, timestamp, nonce string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers", nil)
	req.Header.Set(HeaderBrokerID, brokerID)
	if timestamp != "" {
		req.Header.Set(HeaderTimestamp, timestamp)
	}
	req.Header.Set(HeaderSignature, "not-a-signature")
	if nonce != "" {
		req.Header.Set(HeaderNonce, nonce)
	}
	return req
}

func TestBrokerAuthFailure_Q3_ResponseBodiesUnchangedAndClassified(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	brokerID, signReq := setupSignedBrokerRequest(t, svc, s)
	now := strconv.FormatInt(time.Now().Unix(), 10)
	revokedID := seedBrokerWithSecretState(t, s, store.BrokerSecretStatusRevoked, time.Time{})
	expiredID := seedBrokerWithSecretState(t, s, store.BrokerSecretStatusActive, time.Now().Add(-time.Hour))
	unknownID := uuid.New().String()

	faultSvc := NewBrokerAuthService(DefaultBrokerAuthConfig(), &secretFaultStore{Store: s})
	t.Cleanup(faultSvc.Close)
	refusingSvc := NewBrokerAuthService(DefaultBrokerAuthConfig(), s)
	t.Cleanup(refusingSvc.Close)
	refusingSvc.brokerAdmission = func(context.Context, string) error { return ErrTestAdminRemoteBrokerRefused }

	replayed := signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)
	replayFirst := replayed.Clone(context.Background())

	forged := signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)
	forged.Header.Set(HeaderSignature, strings.Repeat("0", 44))

	cases := []struct {
		name        string
		svc         *BrokerAuthService
		before      func(t *testing.T)
		req         *http.Request
		wantMessage string // exact, unless wantPrefix is set
		wantPrefix  string
		wantCode    auditevent.BrokerFailureCode
	}{
		{name: "missing timestamp", req: unsignedBrokerRequest(brokerID, "", "n1"),
			wantMessage: "missing X-Scion-Timestamp header", wantCode: auditevent.BrokerFailureMissingCredentials},
		{name: "missing signature", req: func() *http.Request {
			r := unsignedBrokerRequest(brokerID, now, "n2")
			r.Header.Del(HeaderSignature)
			return r
		}(), wantMessage: "missing X-Scion-Signature header", wantCode: auditevent.BrokerFailureMissingCredentials},
		{name: "missing nonce", req: unsignedBrokerRequest(brokerID, now, ""),
			wantMessage: "missing X-Scion-Nonce header", wantCode: auditevent.BrokerFailureMissingCredentials},
		{name: "malformed timestamp", req: unsignedBrokerRequest(brokerID, "abc", "n3"),
			wantMessage: `invalid timestamp format: strconv.ParseInt: parsing "abc": invalid syntax`, wantCode: auditevent.BrokerFailureMalformed},
		{name: "unsized signed body", req: func() *http.Request {
			r := unsignedBrokerRequest(brokerID, now, "n11")
			r.Method = http.MethodPost
			r.Body = io.NopCloser(strings.NewReader("{}"))
			r.ContentLength = -1
			r.TransferEncoding = []string{"chunked"}
			return r
		}(), wantMessage: "signed request body must declare its length", wantCode: auditevent.BrokerFailureMalformed},
		{name: "clock skew", req: unsignedBrokerRequest(brokerID, strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10), "n4"),
			wantPrefix: "timestamp outside acceptable range (skew: ", wantCode: auditevent.BrokerFailureClockSkew},
		{name: "replay", before: func(t *testing.T) {
			rec := httptest.NewRecorder()
			AuditableBrokerAuthMiddleware(svc, &mockAuditLogger{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, replayFirst)
			require.Equal(t, http.StatusOK, rec.Code, "the first use of the nonce must pass: %s", rec.Body.String())
		}, req: replayed,
			wantMessage: "nonce already used (possible replay attack)", wantCode: auditevent.BrokerFailureReplay},
		{name: "unknown broker", req: unsignedBrokerRequest(unknownID, now, "n5"),
			wantMessage: "unknown broker: " + unknownID, wantCode: auditevent.BrokerFailureUnknownBroker},
		{name: "non-UUID claimed broker", req: unsignedBrokerRequest("probe-1", now, "n10"),
			wantMessage: `failed to get broker secret: invalid input: invalid UUID "probe-1"`, wantCode: auditevent.BrokerFailureUnknownBroker},
		{name: "secret lookup failure", svc: faultSvc, req: unsignedBrokerRequest(brokerID, now, "n6"),
			wantMessage: "failed to get broker secret: db down", wantCode: auditevent.BrokerFailureInternal},
		{name: "revoked secret", req: unsignedBrokerRequest(revokedID, now, "n7"),
			wantMessage: "broker secret is revoked", wantCode: auditevent.BrokerFailureRevokedOrDisabled},
		{name: "expired secret", req: unsignedBrokerRequest(expiredID, now, "n8"),
			wantMessage: "broker secret has expired", wantCode: auditevent.BrokerFailureExpired},
		{name: "invalid signature", req: forged,
			wantMessage: "invalid signature", wantCode: auditevent.BrokerFailureInvalidSignature},
		{name: "test admin refusal", svc: refusingSvc, req: unsignedBrokerRequest(brokerID, now, "n9"),
			wantMessage: ErrTestAdminRemoteBrokerRefused.Error(), wantCode: auditevent.BrokerFailureRefused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.before != nil {
				c.before(t)
			}
			use := c.svc
			if use == nil {
				use = svc
			}
			logger := &mockAuditLogger{}
			called := false
			rec := httptest.NewRecorder()
			AuditableBrokerAuthMiddleware(use, logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(rec, c.req)

			require.False(t, called)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			var body map[string]map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			require.Len(t, body, 1)
			require.Len(t, body["error"], 2, "only code and message: %s", rec.Body.String())
			assert.Equal(t, ErrCodeBrokerAuthFailed, body["error"]["code"])
			message, _ := body["error"]["message"].(string)
			if c.wantPrefix != "" {
				assert.True(t, strings.HasPrefix(message, c.wantPrefix) && strings.HasSuffix(message, ")"), "message %q", message)
			} else {
				assert.Equal(t, c.wantMessage, message)
			}

			require.Len(t, logger.brokerEvents, 1, "count parity: one auth_failure event")
			event := logger.brokerEvents[0]
			assert.Equal(t, BrokerAuthEventAuthFailure, event.EventType)
			assert.Equal(t, message, event.FailReason, "the legacy FailReason is unchanged")
			assert.Equal(t, c.wantCode, event.FailureCode)
		})
	}
}

// The non-HTTP texts: the broker-ID branch (unreachable through the
// middleware, which skips requests without the header), the rotation
// variant, and the nonce store failure.
func TestBrokerAuthFailure_Q3_ErrorTextUnchangedOutsideMiddleware(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	faultSvc := NewBrokerAuthService(DefaultBrokerAuthConfig(), &secretFaultStore{Store: s})
	t.Cleanup(faultSvc.Close)
	ctx := context.Background()

	noID := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers", nil)
	_, err := svc.ValidateBrokerSignature(ctx, noID)
	require.Error(t, err)
	assert.Equal(t, "missing X-Scion-Broker-ID header", err.Error())
	assert.Equal(t, auditevent.BrokerFailureMissingCredentials, brokerAuthFailureCode(err))

	_, err = svc.ValidateBrokerSignatureWithRotation(ctx, noID)
	require.Error(t, err)
	assert.Equal(t, "missing X-Scion-Broker-ID header", err.Error())

	unknownID := uuid.New().String()
	_, err = svc.ValidateBrokerSignatureWithRotation(ctx, unsignedBrokerRequest(unknownID, "1", "n"))
	require.Error(t, err)
	assert.Equal(t, "unknown broker: "+unknownID, err.Error())
	assert.Equal(t, auditevent.BrokerFailureUnknownBroker, brokerAuthFailureCode(err))

	_, err = svc.ValidateBrokerSignatureWithRotation(ctx, unsignedBrokerRequest("probe-1", "1", "n"))
	require.Error(t, err)
	assert.Equal(t, `failed to get broker secrets: invalid input: invalid UUID "probe-1"`, err.Error())
	assert.Equal(t, auditevent.BrokerFailureUnknownBroker, brokerAuthFailureCode(err), "a non-UUID claimed ID is an unknown broker")

	_, err = faultSvc.ValidateBrokerSignatureWithRotation(ctx, unsignedBrokerRequest(unknownID, "1", "n"))
	require.Error(t, err)
	assert.Equal(t, "failed to get broker secrets: db down", err.Error())
	assert.Equal(t, auditevent.BrokerFailureInternal, brokerAuthFailureCode(err))

	_, signReq := setupSignedBrokerRequest(t, svc, s)
	forged := signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)
	forged.Header.Set(HeaderSignature, "forged")
	_, err = svc.ValidateBrokerSignatureWithRotation(ctx, forged)
	require.Error(t, err)
	assert.Equal(t, "invalid signature", err.Error())
	assert.Equal(t, auditevent.BrokerFailureInvalidSignature, brokerAuthFailureCode(err))

	// Wrapped forms keep the old text and their classification.
	nonceErr := fmt.Errorf("%w: %w", errBrokerAuthNonceCheck, errors.New("db down"))
	assert.Equal(t, "nonce cache check failed: db down", nonceErr.Error())
	assert.Equal(t, auditevent.BrokerFailureInternal, brokerAuthFailureCode(nonceErr))
	assert.Equal(t, "no valid secrets found", errBrokerAuthNoValidSecrets.Error())
	assert.Equal(t, auditevent.BrokerFailureUnknownBroker, brokerAuthFailureCode(errBrokerAuthNoValidSecrets))
	assert.Equal(t, auditevent.BrokerFailureOther, brokerAuthFailureCode(errors.New("something new")))
}
