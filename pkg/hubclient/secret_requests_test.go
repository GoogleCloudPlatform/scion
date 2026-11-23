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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutboundMessageRequest_SecretRequestsJSON(t *testing.T) {
	plain, err := json.Marshal(OutboundMessageRequest{Recipient: "user:a@example.com", Msg: "hi"})
	require.NoError(t, err)
	var plainMap map[string]any
	require.NoError(t, json.Unmarshal(plain, &plainMap))
	assert.NotContains(t, plainMap, "secret_requests")
	assert.NotContains(t, plainMap, "secret_requests_update")

	raw := json.RawMessage(`[{"key":"GITHUB_TOKEN","description":"PAT for release","type":"environment"}]`)
	withReqs, err := json.Marshal(OutboundMessageRequest{
		Recipient:            "user:a@example.com",
		Msg:                  "Need a token",
		SecretRequests:       raw,
		SecretRequestsUpdate: true,
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(withReqs, &m))
	assert.Equal(t, true, m["secret_requests_update"])
	gotRaw, err := json.Marshal(m["secret_requests"])
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(gotRaw))
}

func TestOutboundMessageResult_SecretRequestsJSON(t *testing.T) {
	body := `{
		"message_id": "msg-1",
		"status": "sent",
		"recipient": "user:alice@example.com",
		"recipient_id": "uid-1",
		"secret_requests": [
			{"key": "GITHUB_TOKEN", "version": 2, "row_id": "row-2", "replaced": {"version": 1, "status": "pending"}},
			{"key": "DEPLOY_KEY", "version": 1, "row_id": "row-3", "replaced": null}
		]
	}`
	var res OutboundMessageResult
	require.NoError(t, json.Unmarshal([]byte(body), &res))
	require.Len(t, res.SecretRequests, 2)
	assert.Equal(t, "GITHUB_TOKEN", res.SecretRequests[0].Key)
	assert.Equal(t, 2, res.SecretRequests[0].Version)
	assert.Equal(t, "row-2", res.SecretRequests[0].RowID)
	require.NotNil(t, res.SecretRequests[0].Replaced)
	assert.Equal(t, 1, res.SecretRequests[0].Replaced.Version)
	assert.Equal(t, "pending", res.SecretRequests[0].Replaced.Status)
	assert.Equal(t, "DEPLOY_KEY", res.SecretRequests[1].Key)
	assert.Nil(t, res.SecretRequests[1].Replaced)
}

func TestSecretService_ListRequests(t *testing.T) {
	now := time.Date(2026, 11, 22, 23, 50, 0, 0, time.UTC)
	var gotQueries []string
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/agent/secret-requests", r.URL.Path)
		gotAuth = r.Header.Get("X-Scion-Agent-Token")
		gotQueries = append(gotQueries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messages.AgentSecretRequestListResponse{
			Items: []messages.SecretRequestView{
				{
					RowID:      "row-1",
					MessageID:  "msg-1",
					ProjectID:  "proj-1",
					AgentID:    "agent-1",
					AgentName:  "builder-1",
					Key:        "GITHUB_TOKEN",
					Version:    2,
					SecretType: messages.SecretRequestTypeEnvironment,
					Status:     "fulfilled",
					CreatedAt:  now,
					ExpiresAt:  now.Add(7 * 24 * time.Hour),
					Fulfilled: &messages.SecretRequestFulfilled{
						At:            now.Add(5 * time.Minute),
						ByName:        "Alice",
						ByEmail:       "alice@example.com",
						Surface:       "standalone_page",
						SecretVersion: 1,
					},
				},
			},
			NextCursor: "cur-2",
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithAgentToken("agent-tok"))
	require.NoError(t, err)

	resp, err := c.Secrets().ListRequests(context.Background(), &ListAgentSecretRequestsOptions{
		State:  "all",
		Cursor: "cur-1",
		Limit:  50,
	})
	require.NoError(t, err)
	assert.Equal(t, "agent-tok", gotAuth)
	assert.Equal(t, []string{"cursor=cur-1&limit=50&state=all"}, gotQueries)
	require.Len(t, resp.Items, 1)
	assert.Equal(t, "GITHUB_TOKEN", resp.Items[0].Key)
	assert.Equal(t, 2, resp.Items[0].Version)
	require.NotNil(t, resp.Items[0].Fulfilled)
	assert.Equal(t, "Alice", resp.Items[0].Fulfilled.ByName)
	assert.Equal(t, "cur-2", resp.NextCursor)

	// Nil options sends no query params.
	_, err = c.Secrets().ListRequests(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "", gotQueries[1])
}

func TestSecretService_WithdrawRequests(t *testing.T) {
	now := time.Date(2026, 11, 22, 23, 55, 0, 0, time.UTC)
	var gotBody WithdrawSecretRequestsRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/agent/secret-requests/withdraw", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messages.WithdrawSecretRequestsResponse{
			Withdrawn: []messages.SecretRequestView{
				{
					RowID:   "row-1",
					Key:     "GITHUB_TOKEN",
					Version: 1,
					Status:  "withdrawn",
					Withdrawn: &messages.SecretRequestWithdrawn{
						At:     now,
						Reason: "no longer needed",
						By:     "agent",
						ByName: "builder-1",
					},
				},
			},
			AlreadyWithdrawn: []messages.SecretRequestView{
				{
					RowID:   "row-2",
					Key:     "OLD_KEY",
					Version: 1,
					Status:  "withdrawn",
				},
			},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithAgentToken("agent-tok"))
	require.NoError(t, err)

	resp, err := c.Secrets().WithdrawRequests(context.Background(), &WithdrawSecretRequestsRequest{
		Keys:     []string{"GITHUB_TOKEN", "OLD_KEY"},
		WholeSet: true,
		Reason:   "no longer needed",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"GITHUB_TOKEN", "OLD_KEY"}, gotBody.Keys)
	assert.True(t, gotBody.WholeSet)
	assert.Equal(t, "no longer needed", gotBody.Reason)
	require.Len(t, resp.Withdrawn, 1)
	assert.Equal(t, "GITHUB_TOKEN", resp.Withdrawn[0].Key)
	require.Len(t, resp.AlreadyWithdrawn, 1)
	assert.Equal(t, "OLD_KEY", resp.AlreadyWithdrawn[0].Key)
}

func TestSecretService_RequestsErrors(t *testing.T) {
	t.Run("withdraw 409 conflict with failures", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"withdraw_failed","message":"one or more keys cannot be withdrawn","details":{"failures":[{"key":"GITHUB_TOKEN","reason":"already_fulfilled","message":"GITHUB_TOKEN: already fulfilled by Alice"}]}}}`))
		}))
		defer srv.Close()

		c, err := New(srv.URL, WithAgentToken("agent-tok"))
		require.NoError(t, err)
		_, err = c.Secrets().WithdrawRequests(context.Background(), &WithdrawSecretRequestsRequest{Keys: []string{"GITHUB_TOKEN"}})
		require.Error(t, err)
		var apiErr *apiclient.APIError
		require.True(t, errors.As(err, &apiErr))
		assert.Equal(t, http.StatusConflict, apiErr.StatusCode)
		assert.Equal(t, "withdraw_failed", apiErr.Code)
		assert.Contains(t, apiErr.Details, "failures")
	})

	t.Run("list 404 experiment off", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"route not found"}}`))
		}))
		defer srv.Close()

		c, err := New(srv.URL, WithAgentToken("agent-tok"))
		require.NoError(t, err)
		_, err = c.Secrets().ListRequests(context.Background(), nil)
		require.Error(t, err)
		var apiErr *apiclient.APIError
		require.True(t, errors.As(err, &apiErr))
		assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
	})

	t.Run("list 204 fails DecodeRequired", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		c, err := New(srv.URL, WithAgentToken("agent-tok"))
		require.NoError(t, err)
		_, err = c.Secrets().ListRequests(context.Background(), nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, apiclient.ErrNoContent)
	})
}
