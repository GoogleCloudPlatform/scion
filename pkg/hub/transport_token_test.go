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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTestJWTWithExpiry builds a minimal JWT with the given expiry for testing.
func makeTestJWTWithExpiry(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]interface{}{"exp": exp.Unix(), "iss": "test"})
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fakesig"))
	return fmt.Sprintf("%s.%s.%s", header, payloadB64, sig)
}

func TestNoopTransportMinter_ReturnsError(t *testing.T) {
	m := &noopTransportMinter{}
	token, expiry, err := m.MintIDToken(context.Background(), "https://example.com")
	assert.Empty(t, token)
	assert.True(t, expiry.IsZero())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "disabled")
}

func TestFakeTransportMinter(t *testing.T) {
	testToken := makeTestJWTWithExpiry(time.Now().Add(1 * time.Hour))
	testExpiry := time.Now().Add(1 * time.Hour)

	m := &FakeTransportMinter{
		Token:  testToken,
		Expiry: testExpiry,
	}

	token, expiry, err := m.MintIDToken(context.Background(), "https://example.com")
	require.NoError(t, err)
	assert.Equal(t, testToken, token)
	assert.Equal(t, testExpiry, expiry)
	assert.Equal(t, 1, m.CallCount)
}

func TestFakeTransportMinter_Error(t *testing.T) {
	m := &FakeTransportMinter{
		Err: fmt.Errorf("test error"),
	}

	_, _, err := m.MintIDToken(context.Background(), "https://example.com")
	assert.Error(t, err)
	assert.Equal(t, "test error", err.Error())
}

func TestGCPTransportMinter_MintIDToken(t *testing.T) {
	testExpiry := time.Now().Add(1 * time.Hour).Truncate(time.Second)
	testToken := makeTestJWTWithExpiry(testExpiry)
	testSA := "transport-auth@project.iam.gserviceaccount.com"

	// Fake IAM Credentials API server
	iamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request path matches the expected SA
		assert.Contains(t, r.URL.Path, testSA)
		assert.Equal(t, "POST", r.Method)

		// Parse request body
		var req struct {
			Audience     string `json:"audience"`
			IncludeEmail bool   `json:"includeEmail"`
		}
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)
		assert.Equal(t, "https://hub.example.com", req.Audience)
		assert.True(t, req.IncludeEmail)

		// Return a valid response
		resp := map[string]string{"token": testToken}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer iamServer.Close()

	m := NewGCPTransportMinter(testSA, iamServer.URL)

	token, expiry, err := m.MintIDToken(context.Background(), "https://hub.example.com")
	require.NoError(t, err)
	assert.Equal(t, testToken, token)
	assert.Equal(t, testExpiry, expiry)
}

func TestGCPTransportMinter_EmptySA(t *testing.T) {
	m := NewGCPTransportMinter("", "")

	_, _, err := m.MintIDToken(context.Background(), "https://hub.example.com")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "service account email not configured")
}

func TestGCPTransportMinter_APIError(t *testing.T) {
	iamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintln(w, `{"error": {"code": 403, "message": "Permission denied"}}`)
	}))
	defer iamServer.Close()

	m := NewGCPTransportMinter("sa@project.iam.gserviceaccount.com", iamServer.URL)

	_, _, err := m.MintIDToken(context.Background(), "https://hub.example.com")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "generateIdToken failed")
}

func TestParseJWTExpiry(t *testing.T) {
	expected := time.Now().Add(1 * time.Hour).Truncate(time.Second)
	token := makeTestJWTWithExpiry(expected)

	expiry, err := parseJWTExpiry(token)
	require.NoError(t, err)
	assert.Equal(t, expected, expiry)
}

func TestParseJWTExpiry_InvalidFormat(t *testing.T) {
	_, err := parseJWTExpiry("not-a-jwt")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expected 3 parts")
}

func TestParseJWTExpiry_NoExpClaim(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"test"}`))
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	token := fmt.Sprintf("%s.%s.%s", header, payload, sig)

	_, err := parseJWTExpiry(token)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no expiry claim")
}

func TestRefreshTokenEntry_JSON(t *testing.T) {
	entry := RefreshTokenEntry{
		Layer:     "transport",
		Type:      "google_oidc",
		Value:     "token-value",
		ExpiresIn: 3600,
		Audience:  "https://hub.example.com",
	}

	data, err := json.Marshal(entry)
	require.NoError(t, err)

	var parsed RefreshTokenEntry
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)
	assert.Equal(t, entry, parsed)
}

func TestRefreshTokenEntry_JSON_OmitAudience(t *testing.T) {
	entry := RefreshTokenEntry{
		Layer:     "app",
		Type:      "scion_access",
		Value:     "token-value",
		ExpiresIn: 36000,
	}

	data, err := json.Marshal(entry)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "audience")
}
