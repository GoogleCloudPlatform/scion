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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testIntakeServer creates a test server with a local secret backend configured
// so that store operations actually persist the secret.
func testIntakeServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	// Wire up a local secret backend so store can persist secrets.
	srv.SetSecretBackend(secret.NewLocalBackend(s.(store.SecretStore), "test-hub-id"))
	return srv, s
}

// createIntake is a helper that calls the create intake endpoint and returns
// the response body and the HTTP status.
func createIntake(t *testing.T, srv *Server, body map[string]interface{}) (map[string]interface{}, int) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/secret-intake", body)
	var resp map[string]interface{}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	return resp, rec.Code
}

// storeViaIntake is a helper that calls the store endpoint (authenticated).
func storeViaIntake(t *testing.T, srv *Server, intakeID string, body map[string]interface{}) (map[string]interface{}, int) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/secret-intake/"+intakeID+"/store", body)
	var resp map[string]interface{}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	return resp, rec.Code
}

// storeViaIntakeNoAuth is a helper that calls the store endpoint without auth.
func storeViaIntakeNoAuth(t *testing.T, srv *Server, intakeID string, body map[string]interface{}) (map[string]interface{}, int) {
	t.Helper()
	rec := doRequestNoAuth(t, srv, http.MethodPost,
		"/api/v1/secret-intake/"+intakeID+"/store", body)
	var resp map[string]interface{}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	return resp, rec.Code
}

// extractToken extracts the JWT token from an intake URL (everything after #).
func extractToken(t *testing.T, intakeURL string) string {
	t.Helper()
	for i, c := range intakeURL {
		if c == '#' {
			return intakeURL[i+1:]
		}
	}
	t.Fatal("no # found in intake URL")
	return ""
}

// ============================================================================
// Test: Create intake (happy path)
// ============================================================================

func TestSecretIntake_Create(t *testing.T) {
	srv, _ := testIntakeServer(t)

	resp, status := createIntake(t, srv, map[string]interface{}{
		"key":         "GITHUB_TOKEN",
		"scope":       "user",
		"type":        "environment",
		"description": "GitHub PAT for repo access",
	})

	assert.Equal(t, http.StatusCreated, status)
	assert.NotEmpty(t, resp["url"])
	assert.NotEmpty(t, resp["expires_at"])
	assert.NotEmpty(t, resp["intake_id"])

	// URL should contain /intake# fragment
	url, ok := resp["url"].(string)
	require.True(t, ok)
	assert.Contains(t, url, "/intake#")
}

// ============================================================================
// Test: Store via authenticated user (happy path — secret stored)
// ============================================================================

func TestSecretIntake_StoreAuthenticated(t *testing.T) {
	srv, s := testIntakeServer(t)

	// Create intake
	createResp, status := createIntake(t, srv, map[string]interface{}{
		"key":         "GITHUB_TOKEN",
		"scope":       "user",
		"type":        "environment",
		"description": "GitHub PAT for repo access",
	})
	require.Equal(t, http.StatusCreated, status)

	intakeID := createResp["intake_id"].(string)
	token := extractToken(t, createResp["url"].(string))

	// Store via authenticated endpoint
	storeResp, storeStatus := storeViaIntake(t, srv, intakeID, map[string]interface{}{
		"token": token,
		"value": "ghp_abc123secretvalue",
	})

	assert.Equal(t, http.StatusOK, storeStatus)
	assert.Equal(t, "stored", storeResp["status"])
	assert.Equal(t, "GITHUB_TOKEN", storeResp["key"])

	// Verify the intake is marked consumed
	intake := srv.secretIntakeService.GetIntake(intakeID)
	require.NotNil(t, intake)
	assert.True(t, intake.Consumed)
	assert.NotNil(t, intake.ConsumedAt)

	// Verify the secret was actually persisted in the store
	ctx := context.Background()
	val, err := s.GetSecretValue(ctx, "GITHUB_TOKEN", "user", intake.UserID)
	assert.NoError(t, err)
	assert.NotEmpty(t, val)
}

// ============================================================================
// Test: Store without login (401)
// ============================================================================

func TestSecretIntake_StoreWithoutAuth(t *testing.T) {
	srv, _ := testIntakeServer(t)

	// Create intake
	createResp, status := createIntake(t, srv, map[string]interface{}{
		"key": "NO_AUTH_SECRET",
	})
	require.Equal(t, http.StatusCreated, status)

	intakeID := createResp["intake_id"].(string)
	token := extractToken(t, createResp["url"].(string))

	// Try store without auth — should get 401
	_, storeStatus := storeViaIntakeNoAuth(t, srv, intakeID, map[string]interface{}{
		"token": token,
		"value": "secret-value",
	})
	assert.Equal(t, http.StatusUnauthorized, storeStatus)
}

// ============================================================================
// Test: Store with expired intake (404)
// ============================================================================

func TestSecretIntake_StoreExpired(t *testing.T) {
	srv, _ := testIntakeServer(t)

	// Create intake
	createResp, _ := createIntake(t, srv, map[string]interface{}{
		"key":         "EXPIRES_FAST",
		"ttl_seconds": 1,
	})
	intakeID := createResp["intake_id"].(string)
	token := extractToken(t, createResp["url"].(string))

	// Manually expire the intake
	intake := srv.secretIntakeService.GetIntake(intakeID)
	require.NotNil(t, intake)
	intake.ExpiresAt = intake.CreatedAt.Add(-1) // force expiry
	srv.secretIntakeService.mu.Lock()
	srv.secretIntakeService.intakes[intakeID] = intake
	srv.secretIntakeService.mu.Unlock()

	// Try to store — should get 400 (JWT expired) or 404 (intake expired)
	_, storeStatus := storeViaIntake(t, srv, intakeID, map[string]interface{}{
		"token": token,
		"value": "too-late",
	})

	// The JWT itself is also expired, so we get 400 for invalid token
	assert.True(t, storeStatus == http.StatusNotFound || storeStatus == http.StatusBadRequest,
		"expected 404 or 400, got %d", storeStatus)
}

// ============================================================================
// Test: Store with consumed intake (410)
// ============================================================================

func TestSecretIntake_StoreAlreadyConsumed(t *testing.T) {
	srv, _ := testIntakeServer(t)

	// Create intake
	createResp, _ := createIntake(t, srv, map[string]interface{}{
		"key": "CONSUMED_SECRET",
	})
	intakeID := createResp["intake_id"].(string)
	token := extractToken(t, createResp["url"].(string))

	// Store once (succeeds)
	_, storeStatus := storeViaIntake(t, srv, intakeID, map[string]interface{}{
		"token": token,
		"value": "first-value",
	})
	require.Equal(t, http.StatusOK, storeStatus)

	// Try to store again — should be rejected as already consumed
	_, status := storeViaIntake(t, srv, intakeID, map[string]interface{}{
		"token": token,
		"value": "second-value",
	})
	assert.Equal(t, http.StatusGone, status)
}

// ============================================================================
// Test: Store with tampered JWT (400)
// ============================================================================

func TestSecretIntake_StoreWithTamperedToken(t *testing.T) {
	srv, _ := testIntakeServer(t)

	createResp, _ := createIntake(t, srv, map[string]interface{}{
		"key": "TAMPERED",
	})
	intakeID := createResp["intake_id"].(string)

	// Use a completely invalid token
	_, status := storeViaIntake(t, srv, intakeID, map[string]interface{}{
		"token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"value": "hacked",
	})
	assert.Equal(t, http.StatusBadRequest, status)
}

// ============================================================================
// Test: Token/intake ID mismatch (security — cross-intake attack)
// ============================================================================

func TestSecretIntake_TokenMismatch(t *testing.T) {
	srv, _ := testIntakeServer(t)

	// Create two separate intakes.
	resp1, status1 := createIntake(t, srv, map[string]interface{}{
		"key": "INTAKE_ONE",
	})
	require.Equal(t, http.StatusCreated, status1)

	resp2, status2 := createIntake(t, srv, map[string]interface{}{
		"key": "INTAKE_TWO",
	})
	require.Equal(t, http.StatusCreated, status2)

	intakeID2 := resp2["intake_id"].(string)
	token1 := extractToken(t, resp1["url"].(string))

	// Submit token from intake 1 against intake 2's URL — must fail.
	_, status := storeViaIntake(t, srv, intakeID2, map[string]interface{}{
		"token": token1,
		"value": "cross-intake-attack",
	})
	assert.Equal(t, http.StatusBadRequest, status)
}

// ============================================================================
// Test: Create intake without auth fails
// ============================================================================

func TestSecretIntake_CreateRequiresAuth(t *testing.T) {
	srv, _ := testIntakeServer(t)

	bodyBytes, _ := json.Marshal(map[string]interface{}{
		"key": "NO_AUTH",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/secret-intake",
		bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	// No auth header

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// ============================================================================
// Test: Missing required fields
// ============================================================================

func TestSecretIntake_MissingKey(t *testing.T) {
	srv, _ := testIntakeServer(t)

	_, status := createIntake(t, srv, map[string]interface{}{
		"description": "no key",
	})
	assert.Equal(t, http.StatusBadRequest, status)
}

// ============================================================================
// Test: Method not allowed
// ============================================================================

func TestSecretIntake_MethodNotAllowed(t *testing.T) {
	srv, _ := testIntakeServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/secret-intake", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ============================================================================
// Test: Store with missing fields
// ============================================================================

func TestSecretIntake_StoreMissingFields(t *testing.T) {
	srv, _ := testIntakeServer(t)

	createResp, _ := createIntake(t, srv, map[string]interface{}{
		"key": "MISSING_FIELDS",
	})
	intakeID := createResp["intake_id"].(string)
	token := extractToken(t, createResp["url"].(string))

	// Missing token.
	_, status1 := storeViaIntake(t, srv, intakeID, map[string]interface{}{
		"value": "some-value",
	})
	assert.Equal(t, http.StatusBadRequest, status1)

	// Missing value.
	_, status2 := storeViaIntake(t, srv, intakeID, map[string]interface{}{
		"token": token,
	})
	assert.Equal(t, http.StatusBadRequest, status2)
}

// ============================================================================
// Test: TTL capping (> 1 hour capped to 1 hour)
// ============================================================================

func TestSecretIntake_TTLCapping(t *testing.T) {
	srv, _ := testIntakeServer(t)

	resp, status := createIntake(t, srv, map[string]interface{}{
		"key":         "TTL_CAP",
		"ttl_seconds": 7200, // 2 hours
	})
	require.Equal(t, http.StatusCreated, status)

	intakeID := resp["intake_id"].(string)
	intake := srv.secretIntakeService.GetIntake(intakeID)
	require.NotNil(t, intake)

	actualTTL := intake.ExpiresAt.Sub(intake.CreatedAt)
	assert.InDelta(t, maxIntakeTTL.Seconds(), actualTTL.Seconds(), 1.0,
		"TTL should be capped at maxIntakeTTL")
}
