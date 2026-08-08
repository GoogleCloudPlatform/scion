/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityTokenCommand_MissingAudience(t *testing.T) {
	scrubScionEnv(t)

	// Reset flags to defaults before each test (cobra retains flag state).
	identityTokenAudience = ""
	identityTokenFormat = "token"

	var stderr bytes.Buffer
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs([]string{"identity-token"})
	err := rootCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--audience is required")
}

func TestIdentityTokenCommand_InvalidFormat(t *testing.T) {
	scrubScionEnv(t)

	identityTokenAudience = ""
	identityTokenFormat = "token"

	var stderr bytes.Buffer
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs([]string{"identity-token", "--audience=test", "--format=xml"})
	err := rootCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--format must be")
}

func TestIdentityTokenCommand_DefaultFormat(t *testing.T) {
	scrubScionEnv(t)
	// Redirect token file reads so NewClient() uses the env var, not the real container token.
	t.Cleanup(hub.SetTokenHome(t.TempDir()))

	wantToken := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhZ2VudC0xMjMifQ.signature"
	wantExpiry := time.Now().Add(15 * time.Minute).Truncate(time.Second).UTC()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/agent/identity-token", r.URL.Path)
		assert.Equal(t, "test-token", r.Header.Get("X-Scion-Agent-Token"))

		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "https://vault.example.com", body["audience"])

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"token":      wantToken,
			"expires_at": wantExpiry.Format(time.RFC3339),
		}
		require.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	// Point the hub client at our test server.
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "agent-123")

	identityTokenAudience = ""
	identityTokenFormat = "token"

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetArgs([]string{"identity-token", "--audience=https://vault.example.com"})
	err := rootCmd.Execute()
	require.NoError(t, err)

	// Default format should output raw JWT only — no trailing newline.
	assert.Equal(t, wantToken, stdout.String())
}

func TestIdentityTokenCommand_JSONFormat(t *testing.T) {
	scrubScionEnv(t)
	t.Cleanup(hub.SetTokenHome(t.TempDir()))

	wantToken := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhZ2VudC0xMjMifQ.signature"
	wantExpiry := time.Now().Add(15 * time.Minute).Truncate(time.Second).UTC()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"token":      wantToken,
			"expires_at": wantExpiry.Format(time.RFC3339),
		}
		require.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "agent-123")

	identityTokenAudience = ""
	identityTokenFormat = "token"

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetArgs([]string{"identity-token", "--audience=https://vault.example.com", "--format=json"})
	err := rootCmd.Execute()
	require.NoError(t, err)

	// JSON format should output valid JSON with token and expires_at.
	var result hub.IdentityTokenResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	assert.Equal(t, wantToken, result.Token)
	assert.True(t, result.ExpiresAt.Equal(wantExpiry),
		"ExpiresAt mismatch: got %v, want %v", result.ExpiresAt, wantExpiry)
}

func TestIdentityTokenCommand_HubError(t *testing.T) {
	scrubScionEnv(t)
	t.Cleanup(hub.SetTokenHome(t.TempDir()))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"agent not authorized to request identity tokens"}`))
	}))
	defer srv.Close()

	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "agent-123")

	identityTokenAudience = ""
	identityTokenFormat = "token"

	rootCmd.SetArgs([]string{"identity-token", "--audience=https://vault.example.com"})
	err := rootCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestIdentityTokenCommand_NoHubConfigured(t *testing.T) {
	scrubScionEnv(t)

	identityTokenAudience = ""
	identityTokenFormat = "token"

	rootCmd.SetArgs([]string{"identity-token", "--audience=test"})
	err := rootCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hub client not configured")
}
