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
	"net/http"
	"net/http/httptest"
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
