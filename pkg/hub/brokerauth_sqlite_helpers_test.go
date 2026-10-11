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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

func setupTestBrokerAuthService(t *testing.T) (*BrokerAuthService, store.Store) {
	t.Helper()

	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	config := DefaultBrokerAuthConfig()
	svc := NewBrokerAuthService(config, s)
	t.Cleanup(svc.Close)

	return svc, s
}

// setupSignedBrokerRequest creates a broker with a secret and returns a helper
// function that produces signed requests with the given method, path, and body.
func setupSignedBrokerRequest(t *testing.T, svc *BrokerAuthService, s store.Store) (brokerID string, signReq func(method, path string, headers map[string]string) *http.Request) {
	t.Helper()
	ctx := context.Background()

	brokerID = uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "obo-test-broker",
		Slug:    "obo-test-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secretKey := []byte("obo-test-secret-key-32-bytes!!!!")
	secret := &store.BrokerSecret{
		BrokerID:  brokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}
	if err := s.CreateBrokerSecret(ctx, secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
	}

	nonceCounter := 0
	signReq = func(method, path string, extraHeaders map[string]string) *http.Request {
		nonceCounter++
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := "obo-nonce-" + strconv.Itoa(nonceCounter)

		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(HeaderBrokerID, brokerID)
		req.Header.Set(HeaderTimestamp, timestamp)
		req.Header.Set(HeaderNonce, nonce)

		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}

		canonicalString := svc.buildCanonicalString(req, timestamp, nonce)
		h := hmac.New(sha256.New, secretKey)
		h.Write(canonicalString)
		signature := base64.StdEncoding.EncodeToString(h.Sum(nil))
		req.Header.Set(HeaderSignature, signature)

		return req
	}

	return brokerID, signReq
}
