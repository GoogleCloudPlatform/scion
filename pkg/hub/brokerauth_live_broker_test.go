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
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// liveBrokerValidators are the two hub-side broker signature validators;
// both accept a verified signature only for a live broker row.
func liveBrokerValidators(svc *BrokerAuthService) map[string]func(context.Context, *http.Request) (BrokerIdentity, error) {
	return map[string]func(context.Context, *http.Request) (BrokerIdentity, error){
		"ValidateBrokerSignature":             svc.ValidateBrokerSignature,
		"ValidateBrokerSignatureWithRotation": svc.ValidateBrokerSignatureWithRotation,
	}
}

// restoreSurvivingSecret stores the secret setupSignedBrokerRequest signs
// with for brokerID, standing in for a credential left behind for an ID
// whose row is gone (for example one written before deregistration deleted
// credentials with the row).
func restoreSurvivingSecret(t *testing.T, s store.Store, brokerID string) {
	t.Helper()
	require.NoError(t, s.CreateBrokerSecret(context.Background(), &store.BrokerSecret{
		BrokerID:  brokerID,
		SecretKey: []byte("obo-test-secret-key-32-bytes!!!!"),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}))
}

// A live broker row with its secret still authenticates through both
// validators.
func TestBrokerSignature_LiveBrokerRowAuthenticates(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	brokerID, signReq := setupSignedBrokerRequest(t, svc, s)
	for name, validate := range liveBrokerValidators(svc) {
		t.Run(name, func(t *testing.T) {
			identity, err := validate(context.Background(), signReq(http.MethodGet, "/api/v1/runtime-brokers", nil))
			require.NoError(t, err)
			assert.Equal(t, brokerID, identity.ID())
		})
	}
}

// A secret whose broker row no longer exists does not authenticate, even
// though the signature verifies: it is refused as an unknown broker.
func TestBrokerSignature_SecretWithoutBrokerRowRefused(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	brokerID, signReq := setupSignedBrokerRequest(t, svc, s)
	require.NoError(t, s.DeleteRuntimeBroker(context.Background(), brokerID))
	restoreSurvivingSecret(t, s, brokerID)

	for name, validate := range liveBrokerValidators(svc) {
		t.Run(name, func(t *testing.T) {
			identity, err := validate(context.Background(), signReq(http.MethodGet, "/api/v1/runtime-brokers", nil))
			require.Error(t, err)
			assert.Nil(t, identity)
			assert.ErrorIs(t, err, errBrokerAuthUnknownBroker)
			assert.Equal(t, "unknown broker: "+brokerID, err.Error())
			assert.Equal(t, auditevent.BrokerFailureUnknownBroker, brokerAuthFailureCode(err))
		})
	}
}

// The row check runs only after the signature verifies: a forged signature
// for an ID with a surviving secret but no row is still answered as an
// invalid signature, so the answer says nothing about the row.
func TestBrokerSignature_RowCheckAfterSignature(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	brokerID, signReq := setupSignedBrokerRequest(t, svc, s)
	require.NoError(t, s.DeleteRuntimeBroker(context.Background(), brokerID))
	restoreSurvivingSecret(t, s, brokerID)

	for name, validate := range liveBrokerValidators(svc) {
		t.Run(name, func(t *testing.T) {
			forged := signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)
			forged.Header.Set(HeaderSignature, strings.Repeat("0", 44))
			_, err := validate(context.Background(), forged)
			require.Error(t, err)
			assert.ErrorIs(t, err, errBrokerAuthInvalidSignature)
		})
	}
}

// brokerRowFaultStore fails the runtime broker lookup.
type brokerRowFaultStore struct {
	store.Store
}

func (f *brokerRowFaultStore) GetRuntimeBroker(context.Context, string) (*store.RuntimeBroker, error) {
	return nil, errors.New("db down")
}

// A failed broker row lookup refuses the request (fail closed) and is
// classified as an internal failure.
func TestBrokerSignature_BrokerRowLookupFailureRefused(t *testing.T) {
	_, s := setupTestBrokerAuthService(t)
	svc := NewBrokerAuthService(DefaultBrokerAuthConfig(), &brokerRowFaultStore{Store: s})
	t.Cleanup(svc.Close)
	_, signReq := setupSignedBrokerRequest(t, svc, s)

	for name, validate := range liveBrokerValidators(svc) {
		t.Run(name, func(t *testing.T) {
			identity, err := validate(context.Background(), signReq(http.MethodGet, "/api/v1/runtime-brokers", nil))
			require.Error(t, err)
			assert.Nil(t, identity)
			assert.ErrorIs(t, err, errBrokerAuthBrokerLookup)
			assert.Equal(t, "failed to get runtime broker: db down", err.Error())
			assert.Equal(t, auditevent.BrokerFailureInternal, brokerAuthFailureCode(err))
		})
	}
}

// Deleting a broker and re-creating a row with the same ID leaves the old
// secret unable to authenticate as the new row: the delete removed it.
func TestBrokerSignature_SecretFromDeletedRowRefusedForRecreatedRow(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()
	brokerID, signReq := setupSignedBrokerRequest(t, svc, s)
	require.NoError(t, s.DeleteRuntimeBroker(ctx, brokerID))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "obo-test-broker", Slug: "obo-test-broker", Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}))

	for name, validate := range liveBrokerValidators(svc) {
		t.Run(name, func(t *testing.T) {
			identity, err := validate(ctx, signReq(http.MethodGet, "/api/v1/runtime-brokers", nil))
			require.Error(t, err)
			assert.Nil(t, identity)
			assert.ErrorIs(t, err, errBrokerAuthUnknownBroker)
		})
	}
}

// Deregistering a broker through the API deletes its secret and any
// unconsumed join token with the row, and the old secret no longer
// authenticates.
func TestDeleteRuntimeBroker_RemovesCredentials(t *testing.T) {
	srv, s := testServerWithBrokerAuth(t)
	ctx := context.Background()
	id := tid("dereg-creds-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: id, Name: "dereg-creds-broker", Slug: "dereg-creds-broker", Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}))
	keyB64, err := srv.brokerAuthService.GenerateAndStoreSecret(ctx, id)
	require.NoError(t, err)
	key, err := base64.StdEncoding.DecodeString(keyB64)
	require.NoError(t, err)
	require.NoError(t, s.CreateJoinToken(ctx, &store.BrokerJoinToken{
		BrokerID: id, TokenHash: "dereg-creds-token-hash", ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "dev",
	}))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/runtime-brokers/"+id, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err = s.GetBrokerSecret(ctx, id)
	assert.ErrorIs(t, err, store.ErrNotFound, "deregistration deletes the broker secret")
	_, err = s.GetJoinTokenByBrokerID(ctx, id)
	assert.ErrorIs(t, err, store.ErrNotFound, "deregistration deletes the join token")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers", nil)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: id, SecretKey: key}).ApplyAuth(req))
	_, err = srv.brokerAuthService.ValidateBrokerSignature(ctx, req)
	assert.ErrorIs(t, err, errBrokerAuthUnknownBroker, "the deregistered broker's secret no longer authenticates")
}

// Plugin credentials obtained without a restart (activation, reconfigure)
// come with the plugin's broker row, so the plugin's secret authenticates.
func TestPluginHubCreds_EnsuresBrokerRowSoSecretAuthenticates(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	creds := srv.getPluginHubCreds(ctx, "live-row-plugin")
	brokerID := creds["broker_id"]
	require.NotEmpty(t, brokerID)
	require.NotEmpty(t, creds["hmac_key"])

	row, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err, "the plugin's broker row is created with its credentials")
	assert.Equal(t, "plugin-live-row-plugin", row.Name)
	assert.Equal(t, "live-row-plugin", row.Labels[pluginBrokerLabel])

	key, err := base64.StdEncoding.DecodeString(creds["hmac_key"])
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers", nil)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: key}).ApplyAuth(req))
	identity, err := srv.GetBrokerAuthService().ValidateBrokerSignature(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, brokerID, identity.ID())
}

// An existing plugin broker row is left as it is.
func TestPluginHubCreds_KeepsExistingBrokerRow(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	brokerID := srv.getPluginHubCreds(ctx, "kept-row-plugin")["broker_id"]
	row, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	row.Name = "renamed-plugin-broker"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, row))

	again := srv.getPluginHubCreds(ctx, "kept-row-plugin")
	assert.Equal(t, brokerID, again["broker_id"])
	got, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, "renamed-plugin-broker", got.Name, "an existing row is not replaced")
}
