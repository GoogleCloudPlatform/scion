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
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const orphanSweepTestKey = "orphan-sweep-test-secret-32-byte"

// orphanSweepStore returns a fresh store holding a live broker with a secret
// and a join token, and an orphan secret and join token (no broker row).
func orphanSweepStore(t *testing.T) (s store.Store, live, orphan string) {
	t.Helper()
	ctx := context.Background()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	live, orphan = uuid.NewString(), uuid.NewString()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: live, Name: "orphan-sweep-live", Slug: "orphan-sweep-live", Status: store.BrokerStatusOnline,
		Created: time.Now(), Updated: time.Now(),
	}))
	for _, id := range []string{live, orphan} {
		require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
			BrokerID: id, SecretKey: []byte(orphanSweepTestKey),
			Algorithm: store.BrokerSecretAlgorithmHMACSHA256, Status: store.BrokerSecretStatusActive,
		}))
		require.NoError(t, s.CreateJoinToken(ctx, &store.BrokerJoinToken{
			BrokerID: id, TokenHash: "orphan-sweep-hash-" + id, ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "dev",
		}))
	}
	return s, live, orphan
}

// orphanSweepLogs sends the default logger to a buffer for the test. The
// buffer is mutex-guarded (syncBuffer): New starts background goroutines
// that may still log while the test reads it.
func orphanSweepLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

func assertCredentials(t *testing.T, s store.Store, id string, present bool, msg string) {
	t.Helper()
	ctx := context.Background()
	_, errSecret := s.GetBrokerSecret(ctx, id)
	_, errToken := s.GetJoinTokenByBrokerID(ctx, id)
	if present {
		assert.NoError(t, errSecret, msg+": secret")
		assert.NoError(t, errToken, msg+": join token")
	} else {
		assert.ErrorIs(t, errSecret, store.ErrNotFound, msg+": secret")
		assert.ErrorIs(t, errToken, store.ErrNotFound, msg+": join token")
	}
}

// Hub startup removes the credentials that have no broker row and keeps a
// live broker's.
func TestNew_SweepsOrphanBrokerCredentials(t *testing.T) {
	s, live, orphan := orphanSweepStore(t)
	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()
	_, err := newTestHubServer(t, cfg, s)
	require.NoError(t, err)

	assertCredentials(t, s, orphan, false, "orphan removed at startup")
	assertCredentials(t, s, live, true, "live broker kept")
}

// The sweep logs only counts, never key material or IDs, and a second run
// removes nothing.
func TestSweepOrphanBrokerCredentials_LogsCountsAndIsIdempotent(t *testing.T) {
	s, live, orphan := orphanSweepStore(t)
	logs := orphanSweepLogs(t)

	secrets, tokens, err := sweepOrphanBrokerCredentials(context.Background(), s)
	require.NoError(t, err)
	assert.Equal(t, 1, secrets)
	assert.Equal(t, 1, tokens)
	out := logs.String()
	assert.Contains(t, out, "secrets_removed=1")
	assert.Contains(t, out, "join_tokens_removed=1")
	for _, leak := range []string{orphanSweepTestKey, base64.StdEncoding.EncodeToString([]byte(orphanSweepTestKey)), orphan, live} {
		assert.NotContains(t, out, leak, "the sweep log carries counts only")
	}

	mark := len(logs.String())
	secrets, tokens, err = sweepOrphanBrokerCredentials(context.Background(), s)
	require.NoError(t, err)
	assert.Equal(t, 0, secrets)
	assert.Equal(t, 0, tokens)
	assert.NotContains(t, logs.String()[mark:], "Swept broker credentials", "a second run removes and logs nothing")
	assertCredentials(t, s, live, true, "live broker kept")
}

// orphanSweepFaultStore fails the orphan sweep and counts the calls.
type orphanSweepFaultStore struct {
	store.Store
	sweeps atomic.Int32
}

func (f *orphanSweepFaultStore) DeleteOrphanBrokerCredentials(context.Context) (int, int, error) {
	f.sweeps.Add(1)
	return 0, 0, errors.New("db down")
}

// DB forwards to the wrapped store's raw *sql.DB so New()'s membership
// migration runs against the real store and New reaches the sweep.
func (f *orphanSweepFaultStore) DB() *sql.DB {
	if p, ok := f.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

// A failed sweep is returned and logged as an error, and does not stop
// startup: the orphan stays, and broker authentication still refuses it
// (no broker row).
func TestSweepOrphanBrokerCredentials_FailureLoggedStartupContinues(t *testing.T) {
	s, _, orphan := orphanSweepStore(t)
	fault := &orphanSweepFaultStore{Store: s}
	logs := orphanSweepLogs(t)

	_, _, err := sweepOrphanBrokerCredentials(context.Background(), fault)
	require.Error(t, err)
	assert.Contains(t, logs.String(), "level=ERROR")
	assert.Contains(t, logs.String(), "Failed to sweep broker credentials that have no broker row")
	assert.NotContains(t, logs.String(), orphanSweepTestKey)

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()
	mark := len(logs.String())
	before := fault.sweeps.Load()
	srv, err := newTestHubServer(t, cfg, fault)
	require.NoError(t, err, "a failed sweep does not stop startup")
	require.NotNil(t, srv)
	assert.Equal(t, before+1, fault.sweeps.Load(), "New reached the sweep, which failed")
	assert.Contains(t, logs.String()[mark:], "Failed to sweep broker credentials that have no broker row", "New logged the failed sweep")
	assertCredentials(t, s, orphan, true, "the failed sweep removed nothing")
}

// seedOrphanFor stores a secret (orphanSweepTestKey) and a join token for
// id, which has no broker row, and returns the token hash.
func seedOrphanFor(t *testing.T, s store.Store, id string) string {
	t.Helper()
	ctx := context.Background()
	hash := "orphan-recreate-hash-" + id
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID: id, SecretKey: []byte(orphanSweepTestKey),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256, Status: store.BrokerSecretStatusActive,
	}))
	require.NoError(t, s.CreateJoinToken(ctx, &store.BrokerJoinToken{
		BrokerID: id, TokenHash: hash, ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "dev",
	}))
	return hash
}

// assertOrphanCleared checks that id's seeded secret and join token are
// gone: no secret, or one with another key; no token, or one with another
// hash.
func assertOrphanCleared(t *testing.T, s store.Store, id, seededHash string) {
	t.Helper()
	ctx := context.Background()
	if sec, err := s.GetBrokerSecret(ctx, id); err == nil {
		assert.NotEqual(t, []byte(orphanSweepTestKey), sec.SecretKey, "the re-created row does not inherit the old secret")
	} else {
		assert.ErrorIs(t, err, store.ErrNotFound)
	}
	if tok, err := s.GetJoinTokenByBrokerID(ctx, id); err == nil {
		assert.NotEqual(t, seededHash, tok.TokenHash, "the re-created row does not inherit the old join token")
	} else {
		assert.ErrorIs(t, err, store.ErrNotFound)
	}
}

// A plugin broker row re-created with its fixed (UUIDv5) ID after the
// startup sweep failed does not inherit the credential left for that ID:
// the create clears it, so the old key no longer authenticates.
func TestPluginBrokerRecreate_ClearsOrphanWhenSweepFails(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	const name = "orphan-recreate-plugin"
	id := uuid.NewSHA1(pluginBrokerNS, []byte("plugin-broker-"+name)).String()
	hash := seedOrphanFor(t, s, id)

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()
	fault := &orphanSweepFaultStore{Store: s}
	srv, err := newTestHubServer(t, cfg, fault)
	require.NoError(t, err)
	require.Equal(t, int32(1), fault.sweeps.Load(), "New reached the sweep, which failed")
	_, err = s.GetBrokerSecret(context.Background(), id)
	require.NoError(t, err, "the failed sweep left the orphan")

	creds := srv.getPluginHubCreds(context.Background(), name)
	require.Equal(t, id, creds["broker_id"])
	assert.NotEqual(t, base64.StdEncoding.EncodeToString([]byte(orphanSweepTestKey)), creds["hmac_key"], "a new secret is minted, not the old one returned")
	_, err = s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, err, "the plugin row is re-created")
	assertOrphanCleared(t, s, id, hash)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers", nil)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: id, SecretKey: []byte(orphanSweepTestKey)}).ApplyAuth(req))
	_, err = srv.GetBrokerAuthService().ValidateBrokerSignature(context.Background(), req)
	assert.ErrorIs(t, err, errBrokerAuthInvalidSignature, "the old key does not authenticate as the re-created row")
}

// A registration that supplies the ID of a deleted broker creates a row
// that does not inherit a credential left for that ID.
func TestBrokerRegistration_SuppliedIDClearsOrphanCredential(t *testing.T) {
	srv, s := testServerWithBrokerAuth(t)
	id := uuid.NewString()
	hash := seedOrphanFor(t, s, id)

	resp, err := srv.brokerAuthService.CreateBrokerRegistration(context.Background(), CreateBrokerRegistrationRequest{BrokerID: id, Name: "orphan-recreate-registered"}, "dev")
	require.NoError(t, err)
	require.Equal(t, id, resp.BrokerID)
	assert.False(t, resp.Reregistered)
	assertOrphanCleared(t, s, id, hash)
	_, err = s.GetJoinTokenByBrokerID(context.Background(), id)
	assert.NoError(t, err, "the registration's own new join token is stored")
}

// A flat registration that creates a row with the ID of a deleted broker
// does not inherit a credential left for that ID.
func TestFlatRegistration_ClearsOrphanCredential(t *testing.T) {
	srv, s := testServer(t)
	setFlatExperiment(t, srv, true)
	id := uuid.NewString()
	hash := seedOrphanFor(t, s, id)
	target := api.RuntimeTargetDescriptor{ID: uuid.NewString(), Type: "docker", DisplayName: "Local Docker"}

	_, created, err := srv.registerFlatRuntimeBroker(context.Background(), flatRegistration{BrokerID: id, Name: "orphan-recreate-flat", Target: target, CreatedBy: "test"})
	require.NoError(t, err)
	require.True(t, created)
	assertOrphanCleared(t, s, id, hash)
}
