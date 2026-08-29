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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contractFixturePath returns the absolute path to the contract fixture
// at pkg/sciontool/hub/testdata/agent_secret_fetch_response.json.
func contractFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "agent_secret_fetch_response.json")
}

// TestSecretFetchContract_FixtureUnmarshal verifies that the client-side
// types can unmarshal the contract fixture without data loss. If the hub
// side renames a field or adds a status, this test catches the drift.
//
// The hub-side half of this contract test (marshal → compare) is tracked
// as a separate issue until the #127 stack lands on main.
func TestSecretFetchContract_FixtureUnmarshal(t *testing.T) {
	data, err := os.ReadFile(contractFixturePath(t))
	require.NoError(t, err, "contract fixture must exist at pkg/sciontool/hub/testdata/agent_secret_fetch_response.json")

	var resp SecretFetchResponse
	require.NoError(t, json.Unmarshal(data, &resp))

	// The fixture must have one entry per per-key status.
	require.Len(t, resp.Secrets, 4, "fixture must have exactly 4 per-key statuses")

	// Build a map by status for assertions.
	byStatus := make(map[string]SecretFetchResult, len(resp.Secrets))
	for _, s := range resp.Secrets {
		byStatus[s.Status] = s
	}

	// All four per-key statuses must be present.
	knownStatuses := []string{
		SecretStatusOK,
		SecretStatusUnavailable,
		SecretStatusAccessWithdrawn,
		SecretStatusNotFound,
	}
	for _, status := range knownStatuses {
		_, ok := byStatus[status]
		assert.True(t, ok, "fixture missing status %q", status)
	}

	// No unknown statuses.
	for status := range byStatus {
		found := false
		for _, known := range knownStatuses {
			if status == known {
				found = true
				break
			}
		}
		assert.True(t, found, "fixture contains unknown status %q — add it to the client constants", status)
	}

	// Row 1 (ok): all fields populated.
	ok1 := byStatus[SecretStatusOK]
	assert.NotEmpty(t, ok1.Key)
	assert.NotEmpty(t, ok1.Value)
	assert.Equal(t, SecretStatusOK, ok1.Status)

	// Row 2 (unavailable): key and error populated, no value.
	unavail := byStatus[SecretStatusUnavailable]
	assert.NotEmpty(t, unavail.Key)
	assert.Empty(t, unavail.Value)
	assert.NotEmpty(t, unavail.Error)

	// Row 3 (withdrawn): key and error populated, no value.
	withdrawn := byStatus[SecretStatusAccessWithdrawn]
	assert.NotEmpty(t, withdrawn.Key)
	assert.Empty(t, withdrawn.Value)
	assert.NotEmpty(t, withdrawn.Error)

	// Row 4 (not found): key and error populated, no value.
	notFound := byStatus[SecretStatusNotFound]
	assert.NotEmpty(t, notFound.Key)
	assert.Empty(t, notFound.Value)
	assert.NotEmpty(t, notFound.Error)
}

// TestFetchSecrets_HappyPath tests that FetchSecrets correctly calls the
// endpoint and parses the response.
func TestFetchSecrets_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/agent/secrets", r.URL.Path)
		assert.Equal(t, "test-token", r.Header.Get("X-Scion-Agent-Token"))

		var req SecretFetchRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, []string{"API_KEY"}, req.Keys)

		resp := SecretFetchResponse{
			Secrets: []SecretFetchResult{
				{
					Key:    "API_KEY",
					Value:  "FAKE-KEY-SENTINEL-not-a-real-credential",
					Status: SecretStatusOK,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClientWithConfig(srv.URL, "test-token", "agent-1")
	resp, err := client.FetchSecrets(context.Background(), []string{"API_KEY"})
	require.NoError(t, err)
	require.Len(t, resp.Secrets, 1)
	assert.Equal(t, "API_KEY", resp.Secrets[0].Key)
	assert.Equal(t, SecretStatusOK, resp.Secrets[0].Status)
	assert.Equal(t, "FAKE-KEY-SENTINEL-not-a-real-credential", resp.Secrets[0].Value)
}

// TestFetchSecrets_NonOKStatus tests that non-ok per-key statuses are
// returned without error — the caller inspects each result.
func TestFetchSecrets_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := SecretFetchResponse{
			Secrets: []SecretFetchResult{
				{Key: "GOOD", Value: "FAKE-KEY-SENTINEL-not-a-real-credential", Status: SecretStatusOK},
				{Key: "BAD", Status: SecretStatusNotFound, Error: "secret not found"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClientWithConfig(srv.URL, "test-token", "agent-1")
	resp, err := client.FetchSecrets(context.Background(), []string{"GOOD", "BAD"})
	require.NoError(t, err)
	require.Len(t, resp.Secrets, 2)

	results := make(map[string]SecretFetchResult)
	for _, s := range resp.Secrets {
		results[s.Key] = s
	}
	assert.Equal(t, SecretStatusOK, results["GOOD"].Status)
	assert.Equal(t, SecretStatusNotFound, results["BAD"].Status)
	assert.NotEmpty(t, results["BAD"].Error)
}

// TestFetchSecrets_HTTPError tests that HTTP-level errors (403, 500) are
// returned as Go errors, not as per-key results.
func TestFetchSecrets_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"token predates entitlement recording"}`))
	}))
	defer srv.Close()

	client := NewClientWithConfig(srv.URL, "test-token", "agent-1")
	resp, err := client.FetchSecrets(context.Background(), []string{"ANY"})
	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "403")
}

// TestFetchSecrets_NotConfigured tests that calling FetchSecrets on an
// unconfigured client returns an error immediately.
func TestFetchSecrets_NotConfigured(t *testing.T) {
	client := &Client{} // unconfigured
	resp, err := client.FetchSecrets(context.Background(), []string{"KEY"})
	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "not configured")
}
