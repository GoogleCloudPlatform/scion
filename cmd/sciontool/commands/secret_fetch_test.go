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

package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// splitSecretKeys tests
// =============================================================================

func TestSplitSecretKeys_Normal(t *testing.T) {
	keys := splitSecretKeys("API_KEY,DB_PASSWORD,AUTH_TOKEN")
	assert.Equal(t, []string{"API_KEY", "DB_PASSWORD", "AUTH_TOKEN"}, keys)
}

func TestSplitSecretKeys_Empty(t *testing.T) {
	keys := splitSecretKeys("")
	assert.Empty(t, keys)
}

func TestSplitSecretKeys_Whitespace(t *testing.T) {
	keys := splitSecretKeys(" API_KEY , DB_PASSWORD , ")
	assert.Equal(t, []string{"API_KEY", "DB_PASSWORD"}, keys)
}

func TestSplitSecretKeys_SingleKey(t *testing.T) {
	keys := splitSecretKeys("ONLY_KEY")
	assert.Equal(t, []string{"ONLY_KEY"}, keys)
}

func TestSplitSecretKeys_EmptySegments(t *testing.T) {
	keys := splitSecretKeys("A,,B,,,C")
	assert.Equal(t, []string{"A", "B", "C"}, keys)
}

// =============================================================================
// fetchSecretOverrides tests
// =============================================================================

func TestFetchSecretOverrides_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := hub.SecretFetchResponse{
			Secrets: []hub.SecretFetchResult{
				{Key: "API_KEY", Value: "FAKE-KEY-SENTINEL-not-a-real-credential", Status: hub.SecretStatusOK},
				{Key: "DB_PASS", Value: "FAKE-AUTH-SENTINEL-not-a-real-credential", Status: hub.SecretStatusOK},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := hub.NewClientWithConfig(srv.URL, "test-token", "agent-1")
	overrides := fetchSecretOverrides(client, []string{"API_KEY", "DB_PASS"})

	require.Len(t, overrides, 2)
	assert.Equal(t, "FAKE-KEY-SENTINEL-not-a-real-credential", overrides["API_KEY"])
	assert.Equal(t, "FAKE-AUTH-SENTINEL-not-a-real-credential", overrides["DB_PASS"])
}

func TestFetchSecretOverrides_MixedStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := hub.SecretFetchResponse{
			Secrets: []hub.SecretFetchResult{
				{Key: "GOOD_KEY", Value: "FAKE-KEY-SENTINEL-not-a-real-credential", Status: hub.SecretStatusOK},
				{Key: "MISSING_KEY", Status: hub.SecretStatusNotFound, Error: "secret not found"},
				{Key: "BROKEN_KEY", Status: hub.SecretStatusUnavailable, Error: "cannot read value"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := hub.NewClientWithConfig(srv.URL, "test-token", "agent-1")
	overrides := fetchSecretOverrides(client, []string{"GOOD_KEY", "MISSING_KEY", "BROKEN_KEY"})

	// Only the ok key should be in overrides.
	require.Len(t, overrides, 1)
	assert.Equal(t, "FAKE-KEY-SENTINEL-not-a-real-credential", overrides["GOOD_KEY"])

	// Missing and broken keys are logged but not in overrides — no crash.
	_, hasMissing := overrides["MISSING_KEY"]
	_, hasBroken := overrides["BROKEN_KEY"]
	assert.False(t, hasMissing)
	assert.False(t, hasBroken)
}

func TestFetchSecretOverrides_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()

	client := hub.NewClientWithConfig(srv.URL, "test-token", "agent-1")
	overrides := fetchSecretOverrides(client, []string{"ANY_KEY"})

	// HTTP error → nil overrides, no crash.
	assert.Nil(t, overrides)
}

// TestFetchSecretOverrides_UnknownStatusExcluded verifies that a per-key
// result with an unrecognised status is NOT included in the returned map.
// This guards against the hub adding a fifth status that the client
// silently treats as success — the default branch in fetchSecretOverrides
// must exclude the key and log, not include it.
//
// Mutation: remove the default branch (or change it to set overrides[s.Key]).
// The test must go red.
func TestFetchSecretOverrides_UnknownStatusExcluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := hub.SecretFetchResponse{
			Secrets: []hub.SecretFetchResult{
				{Key: "KNOWN", Value: "FAKE-KEY-SENTINEL-not-a-real-credential", Status: hub.SecretStatusOK},
				{Key: "FUTURE", Value: "some-value", Status: "some_future_status", Error: "not a known status"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := hub.NewClientWithConfig(srv.URL, "test-token", "agent-1")
	overrides := fetchSecretOverrides(client, []string{"KNOWN", "FUTURE"})

	// The known-ok key must be present.
	require.NotNil(t, overrides)
	assert.Equal(t, "FAKE-KEY-SENTINEL-not-a-real-credential", overrides["KNOWN"])

	// The unknown-status key must NOT be present — fail-closed.
	_, hasFuture := overrides["FUTURE"]
	assert.False(t, hasFuture,
		"a result with an unknown status must be excluded from overrides — "+
			"if this fails, the default branch is leaking unknown statuses into the env")
}

func TestFetchSecretOverrides_AllNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := hub.SecretFetchResponse{
			Secrets: []hub.SecretFetchResult{
				{Key: "A", Status: hub.SecretStatusNotFound, Error: "not found"},
				{Key: "B", Status: hub.SecretStatusAccessWithdrawn, Error: "withdrawn"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := hub.NewClientWithConfig(srv.URL, "test-token", "agent-1")
	overrides := fetchSecretOverrides(client, []string{"A", "B"})

	// No ok keys → empty map, not nil (allocated but empty).
	require.NotNil(t, overrides)
	assert.Len(t, overrides, 0)
}
