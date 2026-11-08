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
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The broker signature validators check, in this order: the body bound, the
// signature, the broker row, then the nonce (unless deferred). These tests
// pin that order through what each step leaves behind: how often the broker
// row is read, and whether the request's nonce was consumed.

// rowCountingStore counts runtime broker reads and can fail them.
type rowCountingStore struct {
	store.Store
	reads atomic.Int32
	fail  atomic.Bool
}

func (c *rowCountingStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	c.reads.Add(1)
	if c.fail.Load() {
		return nil, errors.New("db down")
	}
	return c.Store.GetRuntimeBroker(ctx, id)
}

type composedOrderFixture struct {
	svc     *BrokerAuthService
	s       store.Store
	rows    *rowCountingStore
	broker  string
	signReq func(method, path string, headers map[string]string) *http.Request
}

func newComposedOrderFixture(t *testing.T) *composedOrderFixture {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	rows := &rowCountingStore{Store: s}
	svc := NewBrokerAuthService(DefaultBrokerAuthConfig(), rows)
	t.Cleanup(svc.Close)
	broker, signReq := setupSignedBrokerRequest(t, svc, s)
	return &composedOrderFixture{svc: svc, s: s, rows: rows, broker: broker, signReq: signReq}
}

// deleteRowKeepSecret deletes the broker row and stores its secret again, so
// a credential remains for an ID that has no row.
func (f *composedOrderFixture) deleteRowKeepSecret(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.s.DeleteRuntimeBroker(ctx, f.broker))
	require.NoError(t, f.s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID:  f.broker,
		SecretKey: []byte("obo-test-secret-key-32-bytes!!!!"), // setupSignedBrokerRequest's secret
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}))
}

// restoreRow creates the broker row again with the same ID (plain create,
// so the stored secret is kept).
func (f *composedOrderFixture) restoreRow(t *testing.T) {
	t.Helper()
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: f.broker, Name: "obo-test-broker", Slug: "obo-test-broker", Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}))
}

type brokerValidator func(*BrokerAuthService, context.Context, *http.Request) (BrokerIdentity, error)

var composedValidators = map[string]brokerValidator{
	"ValidateBrokerSignature":             (*BrokerAuthService).ValidateBrokerSignature,
	"ValidateBrokerSignatureWithRotation": (*BrokerAuthService).ValidateBrokerSignatureWithRotation,
}

// T1: an over-cap body is refused before the broker row is read.
func TestBrokerSignatureOrder_BodyBoundBeforeRowCheck(t *testing.T) {
	for name, validate := range composedValidators {
		t.Run(name, func(t *testing.T) {
			f := newComposedOrderFixture(t)
			f.deleteRowKeepSecret(t)
			req := f.signReq(http.MethodPost, "/api/v1/runtime-brokers/heartbeat", nil)
			req.Body = io.NopCloser(strings.NewReader("x"))
			req.ContentLength = apiclient.MaxSignedRequestBodyBytes + 1

			_, err := validate(f.svc, context.Background(), req)
			require.Error(t, err)
			assert.ErrorIs(t, err, errBrokerAuthBodyTooLarge)
			assert.Zero(t, f.rows.reads.Load(), "the broker row is not read for an over-cap body")
		})
	}
}

// T2 (and T7 for the rotation validator): a verified signature for an ID
// without a broker row is refused as an unknown broker before the nonce is
// recorded; the same request then succeeds once the row exists again.
func TestBrokerSignatureOrder_RowCheckBeforeNonce(t *testing.T) {
	for name, validate := range composedValidators {
		t.Run(name, func(t *testing.T) {
			f := newComposedOrderFixture(t)
			f.deleteRowKeepSecret(t)
			ctx := context.Background()
			req := f.signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)

			_, err := validate(f.svc, ctx, req)
			require.Error(t, err)
			assert.ErrorIs(t, err, errBrokerAuthUnknownBroker)
			assert.Equal(t, auditevent.BrokerFailureUnknownBroker, brokerAuthFailureCode(err))
			assert.EqualValues(t, 1, f.rows.reads.Load())

			f.restoreRow(t)
			identity, err := validate(f.svc, ctx, req)
			require.NoError(t, err, "the refused request left its nonce unused")
			assert.Equal(t, f.broker, identity.ID())
		})
	}
}

// T3: a live broker row records the nonce once; a replay is refused. The row
// is read once per request.
func TestBrokerSignatureOrder_LiveRowRecordsNonceOnce(t *testing.T) {
	for name, validate := range composedValidators {
		t.Run(name, func(t *testing.T) {
			f := newComposedOrderFixture(t)
			ctx := context.Background()
			req := f.signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)

			_, err := validate(f.svc, ctx, req)
			require.NoError(t, err)
			assert.EqualValues(t, 1, f.rows.reads.Load())

			_, err = validate(f.svc, ctx, req)
			require.Error(t, err)
			assert.ErrorIs(t, err, errBrokerAuthReplay)
			assert.EqualValues(t, 2, f.rows.reads.Load(), "one broker row read per request")
		})
	}
}

// T4: a forged signature never reaches the broker row check, and leaves the
// legitimate request's nonce unused.
func TestBrokerSignatureOrder_ForgedSignatureSkipsRowCheck(t *testing.T) {
	for name, validate := range composedValidators {
		t.Run(name, func(t *testing.T) {
			f := newComposedOrderFixture(t)
			f.deleteRowKeepSecret(t)
			ctx := context.Background()
			legit := f.signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)
			forged := legit.Clone(ctx)
			forged.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(make([]byte, 32)))

			_, err := validate(f.svc, ctx, forged)
			require.Error(t, err)
			assert.ErrorIs(t, err, errBrokerAuthInvalidSignature)
			assert.Zero(t, f.rows.reads.Load(), "no broker row read for a forged signature")

			f.restoreRow(t)
			_, err = validate(f.svc, ctx, legit)
			require.NoError(t, err, "the forged request left the nonce unused")
		})
	}
}

// T5: a failed broker row read refuses the request (fail closed) without
// recording the nonce.
func TestBrokerSignatureOrder_RowLookupErrorFailsClosed(t *testing.T) {
	for name, validate := range composedValidators {
		t.Run(name, func(t *testing.T) {
			f := newComposedOrderFixture(t)
			ctx := context.Background()
			req := f.signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)

			f.rows.fail.Store(true)
			identity, err := validate(f.svc, ctx, req)
			require.Error(t, err)
			assert.Nil(t, identity)
			assert.ErrorIs(t, err, errBrokerAuthBrokerLookup)
			assert.Equal(t, "failed to get runtime broker: db down", err.Error())
			assert.Equal(t, auditevent.BrokerFailureInternal, brokerAuthFailureCode(err))

			f.rows.fail.Store(false)
			_, err = validate(f.svc, ctx, req)
			require.NoError(t, err, "the refused request left its nonce unused")
		})
	}
}

// T6: a request whose nonce is deferred to the admitting hop still needs a
// live broker row; its nonce is not recorded here either way.
func TestBrokerSignatureOrder_DeferredNonceStillNeedsRow(t *testing.T) {
	f := newComposedOrderFixture(t)
	f.svc.deferNonce = func(*http.Request) bool { return true }
	f.deleteRowKeepSecret(t)
	ctx := context.Background()
	req := f.signReq(http.MethodGet, "/api/v1/conduit", nil)

	_, err := f.svc.ValidateBrokerSignature(ctx, req)
	require.Error(t, err)
	assert.ErrorIs(t, err, errBrokerAuthUnknownBroker)
	assert.EqualValues(t, 1, f.rows.reads.Load())

	f.restoreRow(t)
	for i := 0; i < 2; i++ {
		_, err = f.svc.ValidateBrokerSignature(ctx, req)
		require.NoError(t, err, "a deferred request is verified, not recorded")
	}
}

// twoSecretStore serves two active secrets for the broker: the stored one
// and one with another key, in the order given. A broker can store only one
// secret, so this is the only way to reach the rotation validator's loop
// over several secrets. served counts the calls that returned two secrets;
// a call that did not find exactly one stored secret passes through
// uncounted, so a test asserting served == 1 notices it.
type twoSecretStore struct {
	*rowCountingStore
	wrongFirst bool
	served     atomic.Int32
}

func (w *twoSecretStore) GetActiveSecrets(ctx context.Context, brokerID string) ([]*store.BrokerSecret, error) {
	got, err := w.rowCountingStore.GetActiveSecrets(ctx, brokerID)
	if err != nil || len(got) != 1 {
		return got, err
	}
	w.served.Add(1)
	wrong := *got[0]
	wrong.SecretKey = []byte("composed-order-other-key-32-byte")
	if w.wrongFirst {
		return []*store.BrokerSecret{&wrong, got[0]}, nil
	}
	return []*store.BrokerSecret{got[0], &wrong}, nil
}

// The rotation validator refuses at once when a secret verifies but the
// broker row is missing or cannot be read: it does not go on to the next
// secret, so the refusal stays "unknown broker" (or the lookup error) and is
// not masked by a later secret's "invalid signature".
func TestBrokerSignatureRotation_RowRefusalNotMaskedByNextSecret(t *testing.T) {
	cases := []struct {
		name       string
		wrongFirst bool
		rowFails   bool
		want       error
	}{
		{name: "verifying secret first", want: errBrokerAuthUnknownBroker},
		{name: "verifying secret second", wrongFirst: true, want: errBrokerAuthUnknownBroker},
		{name: "verifying secret first, row lookup fails", rowFails: true, want: errBrokerAuthBrokerLookup},
		{name: "verifying secret second, row lookup fails", wrongFirst: true, rowFails: true, want: errBrokerAuthBrokerLookup},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newComposedOrderFixture(t)
			two := &twoSecretStore{rowCountingStore: f.rows, wrongFirst: tc.wrongFirst}
			svc := NewBrokerAuthService(DefaultBrokerAuthConfig(), two)
			t.Cleanup(svc.Close)
			if tc.rowFails {
				f.rows.fail.Store(true)
			} else {
				f.deleteRowKeepSecret(t)
			}
			req := f.signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)

			_, err := svc.ValidateBrokerSignatureWithRotation(context.Background(), req)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)
			assert.NotErrorIs(t, err, errBrokerAuthInvalidSignature)
			assert.EqualValues(t, 1, two.served.Load(), "the validator was served two secrets")
			assert.EqualValues(t, 1, f.rows.reads.Load(), "the broker row is read once, for the verifying secret")
		})
	}
}
