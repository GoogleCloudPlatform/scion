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
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for join tokens minted through POST /api/v1/brokers with
// joinTokenTtlSeconds / preserveSettings (the request 'scion hub brokers
// join-token create' sends) and redeemed at POST /api/v1/brokers/join.

// mintJoinToken sends the request 'hub brokers join-token create' sends.
func mintJoinToken(t *testing.T, srv *Server, user *store.User, name string, ttlSeconds int) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:                name,
		JoinTokenTTLSeconds: ttlSeconds,
		PreserveSettings:    true,
		Labels:              map[string]string{"scion.io/broker-role": "remote"},
	})
}

func decodeRegistration(t *testing.T, rec *httptest.ResponseRecorder) CreateBrokerRegistrationResponse {
	t.Helper()
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

// joinWithToken redeems a join token with no Authorization header.
func joinWithToken(t *testing.T, srv *Server, brokerID, token string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/brokers/join", BrokerJoinRequest{
		BrokerID:  brokerID,
		JoinToken: token,
		Hostname:  "headless-host",
		Version:   "test",
	})
}

func TestBrokerJoinToken_TTLBounds(t *testing.T) {
	srv, s := testServer(t)
	minter := newHubMemberUser(t, s, "jt-ttl-minter")

	cases := []struct {
		name       string
		ttl        int
		wantStatus int
		wantTTL    time.Duration
	}{
		{"below minimum", 299, http.StatusBadRequest, 0},
		{"minimum", 300, http.StatusCreated, 300 * time.Second},
		{"maximum", 86400, http.StatusCreated, 24 * time.Hour},
		{"above maximum", 86401, http.StatusBadRequest, 0},
		{"negative", -1, http.StatusBadRequest, 0},
		{"omitted uses the default", 0, http.StatusCreated, time.Hour},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "jt-ttl-broker-" + string(rune('a'+i))
			before := time.Now()
			rec := mintJoinToken(t, srv, minter, name, tc.ttl)
			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())

			if tc.wantStatus == http.StatusBadRequest {
				assert.Equal(t, ErrCodeValidationError, errorCode(t, rec))
				assert.Contains(t, rec.Body.String(), "joinTokenTtlSeconds must be between 300 and 86400")
				assert.Contains(t, rec.Body.String(), `"field":"joinTokenTtlSeconds"`)
				_, err := s.GetRuntimeBrokerByName(context.Background(), name)
				assert.ErrorIs(t, err, store.ErrNotFound, "a rejected request must not create a broker")
				return
			}

			resp := decodeRegistration(t, rec)
			assert.WithinDuration(t, before.Add(tc.wantTTL), resp.ExpiresAt, 5*time.Second)
			stored, err := s.GetJoinTokenByBrokerID(context.Background(), resp.BrokerID)
			require.NoError(t, err)
			assert.WithinDuration(t, resp.ExpiresAt, stored.ExpiresAt, time.Second)
		})
	}
}

// TestBrokerJoinToken_RemintBeforeJoin: a second mint for the same broker
// succeeds (it used to fail with 500 on the join token primary key),
// reports reissued, and only the newest token works.
func TestBrokerJoinToken_RemintBeforeJoin(t *testing.T) {
	srv, s := testServer(t)
	minter := newHubMemberUser(t, s, "jt-remint-minter")

	rec1 := mintJoinToken(t, srv, minter, "jt-remint-broker", 0)
	require.Equal(t, http.StatusCreated, rec1.Code, "body: %s", rec1.Body.String())
	first := decodeRegistration(t, rec1)
	assert.False(t, first.Reissued, "the first token replaces nothing")

	rec2 := mintJoinToken(t, srv, minter, "jt-remint-broker", 0)
	require.Equal(t, http.StatusCreated, rec2.Code, "a re-mint before join must succeed; body: %s", rec2.Body.String())
	second := decodeRegistration(t, rec2)
	assert.True(t, second.Reissued)
	assert.True(t, second.Reregistered)
	assert.Equal(t, first.BrokerID, second.BrokerID)
	assert.NotEqual(t, first.JoinToken, second.JoinToken)

	recOld := joinWithToken(t, srv, first.BrokerID, first.JoinToken)
	assert.Equal(t, http.StatusUnauthorized, recOld.Code, "body: %s", recOld.Body.String())
	assert.Equal(t, ErrCodeInvalidJoinToken, errorCode(t, recOld))

	recNew := joinWithToken(t, srv, second.BrokerID, second.JoinToken)
	assert.Equal(t, http.StatusOK, recNew.Code, "body: %s", recNew.Body.String())
}

// TestBrokerJoinToken_MintThenJoinWithoutUserAuth: a token minted by user A
// is redeemed with no Authorization header; the broker is owned by A and
// the returned secret authenticates the broker over HMAC.
func TestBrokerJoinToken_MintThenJoinWithoutUserAuth(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	minter := newHubMemberUser(t, s, "jt-e2e-minter")

	rec := mintJoinToken(t, srv, minter, "jt-e2e-broker", 600)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	minted := decodeRegistration(t, rec)
	require.True(t, len(minted.JoinToken) > len(JoinTokenPrefix))
	assert.Equal(t, JoinTokenPrefix, minted.JoinToken[:len(JoinTokenPrefix)])

	joinRec := joinWithToken(t, srv, minted.BrokerID, minted.JoinToken)
	require.Equal(t, http.StatusOK, joinRec.Code, "body: %s", joinRec.Body.String())
	var joined BrokerJoinResponse
	require.NoError(t, json.NewDecoder(joinRec.Body).Decode(&joined))
	assert.Equal(t, minted.BrokerID, joined.BrokerID)

	broker, err := s.GetRuntimeBroker(ctx, minted.BrokerID)
	require.NoError(t, err)
	assert.Equal(t, minter.ID, broker.CreatedBy, "the minter owns the broker")
	assert.False(t, broker.AutoProvide, "a minted broker never auto-provides")
	assert.Equal(t, store.BrokerStatusOnline, broker.Status)

	// The token is single use.
	again := joinWithToken(t, srv, minted.BrokerID, minted.JoinToken)
	assert.Equal(t, http.StatusUnauthorized, again.Code)

	// The secret authenticates the broker.
	key, err := base64.StdEncoding.DecodeString(joined.SecretKey)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{"status": "online"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+minted.BrokerID+"/heartbeat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	require.NoError(t, srv.brokerAuthService.SignRequest(req, minted.BrokerID, key))
	hb := httptest.NewRecorder()
	srv.Handler().ServeHTTP(hb, req)
	assert.Equal(t, http.StatusOK, hb.Code, "HMAC with the joined secret should authenticate; body: %s", hb.Body.String())
}

// TestBrokerJoinToken_PreserveSettingsLeavesBrokerUnchanged: a re-mint with
// preserveSettings changes nothing on the broker record, even when the
// request carries values that a plain re-registration would apply.
func TestBrokerJoinToken_PreserveSettingsLeavesBrokerUnchanged(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "jt-preserve-owner")

	broker := &store.RuntimeBroker{
		ID:                         tid("jt-preserve-broker"),
		Name:                       "jt-preserve-broker",
		Slug:                       "jt-preserve-broker",
		Status:                     store.BrokerStatusOffline,
		AutoProvide:                true,
		Labels:                     map[string]string{"env": "baseline"},
		GCPHostServiceAccountEmail: "host@proj.iam.gserviceaccount.com",
		GCPHostProjectID:           "proj",
		Created:                    time.Now(),
		Updated:                    time.Now(),
		CreatedBy:                  owner.ID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:             broker.Name,
		PreserveSettings: true,
		AutoProvide:      false,
		Labels:           map[string]string{"env": "requested", "scion.io/broker-role": "remote"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	resp := decodeRegistration(t, rec)
	assert.Equal(t, broker.ID, resp.BrokerID)
	assert.NotEmpty(t, resp.JoinToken)

	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, got.AutoProvide, "AutoProvide must be unchanged")
	assert.Equal(t, map[string]string{"env": "baseline"}, got.Labels, "labels must be unchanged")
	assert.Equal(t, "host@proj.iam.gserviceaccount.com", got.GCPHostServiceAccountEmail)
	assert.Equal(t, "proj", got.GCPHostProjectID)

	// Without preserveSettings, re-registration still applies the request.
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: false,
		Labels:      map[string]string{"env": "requested"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	got, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.False(t, got.AutoProvide)
	assert.Equal(t, "requested", got.Labels["env"])
	assert.Empty(t, got.GCPHostServiceAccountEmail)
}

// TestBrokerJoinToken_PreserveSettingsNewBrokerIgnoresSettings: for a new
// broker, preserveSettings creates it with AutoProvide off and no GCP host
// identity, whatever the request says.
func TestBrokerJoinToken_PreserveSettingsNewBrokerIgnoresSettings(t *testing.T) {
	srv, s := testServer(t)
	minter := newHubMemberUser(t, s, "jt-preserve-new-minter")

	rec := doRequestAsUser(t, srv, minter, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:                       "jt-preserve-new-broker",
		PreserveSettings:           true,
		AutoProvide:                true,
		GCPHostServiceAccountEmail: "host@proj.iam.gserviceaccount.com",
		GCPHostProjectID:           "proj",
		Labels:                     map[string]string{"scion.io/broker-role": "remote"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	resp := decodeRegistration(t, rec)

	got, err := s.GetRuntimeBroker(context.Background(), resp.BrokerID)
	require.NoError(t, err)
	assert.False(t, got.AutoProvide)
	assert.Empty(t, got.GCPHostServiceAccountEmail)
	assert.Empty(t, got.GCPHostProjectID)
	assert.Equal(t, "remote", got.Labels["scion.io/broker-role"])
	assert.Equal(t, minter.ID, got.CreatedBy)
}

// TestBrokerJoinToken_ConcurrentJoins: of eight concurrent joins with one
// token, exactly one succeeds and the broker ends up with one secret.
func TestBrokerJoinToken_ConcurrentJoins(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	minter := newHubMemberUser(t, s, "jt-concurrent-minter")

	rec := mintJoinToken(t, srv, minter, "jt-concurrent-broker", 0)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	minted := decodeRegistration(t, rec)

	const joiners = 8
	codes := make(chan int, joiners)
	keys := make(chan string, joiners)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < joiners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := joinWithToken(t, srv, minted.BrokerID, minted.JoinToken)
			codes <- r.Code
			if r.Code == http.StatusOK {
				var resp BrokerJoinResponse
				_ = json.NewDecoder(r.Body).Decode(&resp)
				keys <- resp.SecretKey
			}
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	close(keys)

	ok, unauthorized := 0, 0
	for c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusUnauthorized:
			unauthorized++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	assert.Equal(t, 1, ok, "exactly one join succeeds")
	assert.Equal(t, joiners-1, unauthorized)

	active, err := s.GetActiveSecrets(ctx, minted.BrokerID)
	require.NoError(t, err)
	require.Len(t, active, 1, "the broker has exactly one active secret")
	winner := <-keys
	assert.Equal(t, winner, base64.StdEncoding.EncodeToString(active[0].SecretKey), "the stored secret is the one returned to the winner")
}

// seedJoinToken stores a join token for brokerID directly and returns the
// plaintext token.
func seedJoinToken(t *testing.T, s store.Store, brokerID string, expiresAt time.Time) string {
	t.Helper()
	token := JoinTokenPrefix + "seeded-" + brokerID
	_, err := s.UpsertJoinToken(context.Background(), &store.BrokerJoinToken{
		BrokerID:  brokerID,
		TokenHash: sha256Hash(token),
		ExpiresAt: expiresAt,
		CreatedBy: "test",
	})
	require.NoError(t, err)
	return token
}

func TestBrokerJoinToken_ExpiredToken(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "jt-expired-owner")
	broker := createReregistrationTestBroker(t, s, "jt-expired-broker", owner.ID)
	token := seedJoinToken(t, s, broker.ID, time.Now().Add(-time.Minute))

	rec := joinWithToken(t, srv, broker.ID, token)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeExpiredJoinToken, errorCode(t, rec))
	assert.Contains(t, rec.Body.String(), "join token has expired")

	_, err := s.GetJoinTokenByBrokerID(ctx, broker.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the expired token is removed")
	_, err = s.GetBrokerSecret(ctx, broker.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no secret is issued")
}

func TestBrokerJoinToken_WrongBrokerAndUnknownToken(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "jt-mismatch-owner")
	broker := createReregistrationTestBroker(t, s, "jt-mismatch-broker", owner.ID)
	other := createReregistrationTestBroker(t, s, "jt-mismatch-other", owner.ID)
	token := seedJoinToken(t, s, broker.ID, time.Now().Add(time.Hour))

	rec := joinWithToken(t, srv, other.ID, token)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeInvalidJoinToken, errorCode(t, rec))
	assert.Contains(t, rec.Body.String(), "join token does not match broker")

	rec = joinWithToken(t, srv, broker.ID, JoinTokenPrefix+"unknown")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, ErrCodeInvalidJoinToken, errorCode(t, rec))
	assert.Contains(t, rec.Body.String(), "invalid join token")

	_, err := s.GetJoinTokenByBrokerID(ctx, broker.ID)
	require.NoError(t, err, "failed joins leave the token usable")
	rec = joinWithToken(t, srv, broker.ID, token)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// failingBrokerUpdateStore makes UpdateRuntimeBroker fail while fail is
// set, including on the transactional store WithTx hands out.
type failingBrokerUpdateStore struct {
	store.Store
	fail *atomic.Bool
}

func (f *failingBrokerUpdateStore) UpdateRuntimeBroker(ctx context.Context, b *store.RuntimeBroker) error {
	if f.fail.Load() {
		return errors.New("injected broker update failure")
	}
	return f.Store.UpdateRuntimeBroker(ctx, b)
}

func (f *failingBrokerUpdateStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&failingBrokerUpdateStore{Store: tx, fail: f.fail})
	})
}

// TestBrokerJoinToken_FailedJoinRollsBack: when a step after the token is
// consumed fails, the join returns 500, nothing is kept, and the same token
// works on retry.
func TestBrokerJoinToken_FailedJoinRollsBack(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	minter := newHubMemberUser(t, s, "jt-rollback-minter")

	rec := mintJoinToken(t, srv, minter, "jt-rollback-broker", 0)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	minted := decodeRegistration(t, rec)

	fail := &atomic.Bool{}
	fail.Store(true)
	srv.brokerAuthService.store = &failingBrokerUpdateStore{Store: s, fail: fail}

	rec = joinWithToken(t, srv, minted.BrokerID, minted.JoinToken)
	require.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
	_, err := s.GetJoinTokenByBrokerID(ctx, minted.BrokerID)
	require.NoError(t, err, "the token survives a failed join")
	_, err = s.GetBrokerSecret(ctx, minted.BrokerID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the secret write is rolled back")

	fail.Store(false)
	rec = joinWithToken(t, srv, minted.BrokerID, minted.JoinToken)
	require.Equal(t, http.StatusOK, rec.Code, "a retry with the same token succeeds; body: %s", rec.Body.String())
	_, err = s.GetJoinTokenByBrokerID(ctx, minted.BrokerID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCompleteBrokerJoin_SentinelErrors(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	owner := newPlainUser(t, s, "jt-sentinel-owner")
	broker := createReregistrationTestBroker(t, s, "jt-sentinel-broker", owner.ID)
	svc := NewBrokerAuthService(DefaultBrokerAuthConfig(), s)
	t.Cleanup(svc.Close)

	_, err := svc.CompleteBrokerJoin(ctx, BrokerJoinRequest{BrokerID: broker.ID, JoinToken: JoinTokenPrefix + "nope"}, "")
	assert.ErrorIs(t, err, ErrJoinTokenInvalid)
	assert.EqualError(t, err, "invalid join token")

	token := seedJoinToken(t, s, broker.ID, time.Now().Add(time.Hour))
	_, err = svc.CompleteBrokerJoin(ctx, BrokerJoinRequest{BrokerID: tid("jt-sentinel-other"), JoinToken: token}, "")
	assert.ErrorIs(t, err, ErrJoinTokenBrokerMismatch)
	assert.EqualError(t, err, "join token does not match broker")

	expired := seedJoinToken(t, s, broker.ID, time.Now().Add(-time.Second))
	_, err = svc.CompleteBrokerJoin(ctx, BrokerJoinRequest{BrokerID: broker.ID, JoinToken: expired}, "")
	assert.ErrorIs(t, err, ErrJoinTokenExpired)
	assert.EqualError(t, err, "join token has expired")
}
