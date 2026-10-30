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

package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetHubAccessToken_OAuthCredentials(t *testing.T) {
	// Set up temp dir for credentials
	tmpDir := t.TempDir()
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string {
		return filepath.Join(tmpDir, "credentials.json")
	})
	defer credentials.SetCredentialsPath(origPath)

	// Clear any dev token env vars
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")

	endpoint := "https://hub.example.com"

	// Store OAuth credentials
	err := credentials.Store(endpoint, &credentials.TokenResponse{
		AccessToken: "oauth-token-123",
		ExpiresIn:   1 * time.Hour,
	})
	require.NoError(t, err)

	// Should return the OAuth token
	token := getHubAccessToken(endpoint)
	assert.Equal(t, "oauth-token-123", token)
}

func TestGetHubAccessToken_DevTokenFallback(t *testing.T) {
	// Set up temp dir for credentials (empty - no OAuth)
	tmpDir := t.TempDir()
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string {
		return filepath.Join(tmpDir, "credentials.json")
	})
	defer credentials.SetCredentialsPath(origPath)

	// Set a dev token via env var; clear SCION_HUB_TOKEN, which outranks it
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test123")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")

	endpoint := "http://127.0.0.1:8080"

	// Should fall back to dev token for a loopback hub
	token := getHubAccessToken(endpoint)
	assert.Equal(t, "scion_dev_test123", token)

	// A dev token is valid only on the local workstation server, so a hub
	// on another address gets none.
	assert.Empty(t, getHubAccessToken("https://hub.example.com"), "a remote hub")
	assert.Empty(t, getHubAccessToken("http://127.0.0.2:8080"), "a non-loopback local address")
	assert.Empty(t, getHubAccessToken("http://10.0.0.5:8080"), "a private address")
}

func TestGetHubAccessToken_DevTokenFileFallback(t *testing.T) {
	// Set up temp dir for credentials (empty - no OAuth)
	tmpDir := t.TempDir()
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string {
		return filepath.Join(tmpDir, "credentials.json")
	})
	defer credentials.SetCredentialsPath(origPath)

	// Clear env-based dev token (including v1 settings env var) and
	// SCION_HUB_TOKEN, which outranks the dev-token file
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_HUB_TOKEN", "")

	// Set up a dev token file
	tokenFile := filepath.Join(tmpDir, "dev-token")
	err := os.WriteFile(tokenFile, []byte("scion_dev_fromfile\n"), 0600)
	require.NoError(t, err)
	t.Setenv("SCION_DEV_TOKEN_FILE", tokenFile)

	endpoint := "http://localhost:8080"

	// Should fall back to dev token from file for a loopback hub
	token := getHubAccessToken(endpoint)
	assert.Equal(t, "scion_dev_fromfile", token)
}

func TestGetHubAccessToken_OAuthTakesPriority(t *testing.T) {
	// Set up temp dir for credentials
	tmpDir := t.TempDir()
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string {
		return filepath.Join(tmpDir, "credentials.json")
	})
	defer credentials.SetCredentialsPath(origPath)

	endpoint := "https://hub.example.com"

	// Store OAuth credentials
	err := credentials.Store(endpoint, &credentials.TokenResponse{
		AccessToken: "oauth-token-priority",
		ExpiresIn:   1 * time.Hour,
	})
	require.NoError(t, err)

	// Also set a dev token
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_shouldnotuse")

	// OAuth should take priority over dev token
	token := getHubAccessToken(endpoint)
	assert.Equal(t, "oauth-token-priority", token)
}

func TestGetHubAccessToken_NoAuth(t *testing.T) {
	// Set up temp dir for credentials (empty)
	tmpDir := t.TempDir()
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string {
		return filepath.Join(tmpDir, "credentials.json")
	})
	defer credentials.SetCredentialsPath(origPath)

	// Clear all dev token sources (including v1 settings env var)
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	// Override HOME to prevent finding ~/.scion/dev-token
	t.Setenv("HOME", tmpDir)

	endpoint := "https://hub.example.com"

	// Should return empty string
	token := getHubAccessToken(endpoint)
	assert.Empty(t, token)
}

func TestGetHubAccessToken_HubTokenEnv(t *testing.T) {
	// No OAuth credentials.
	tmpDir := t.TempDir()
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string {
		return filepath.Join(tmpDir, "credentials.json")
	})
	defer credentials.SetCredentialsPath(origPath)

	// A user access token (PAT) in the env, plus a dev token that must NOT win.
	t.Setenv("SCION_HUB_TOKEN", "scion_pat_env123")
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_shouldnotuse")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")

	endpoint := "https://hub.example.com"

	// SCION_HUB_TOKEN is honored, and beats the dev token — matching the REST
	// hub client. Without this, attach against a dev-auth-off hub driven by
	// SCION_HUB_TOKEN fails with "no access token found for Hub".
	token := getHubAccessToken(endpoint)
	assert.Equal(t, "scion_pat_env123", token)
}

func TestGetHubAccessToken_OAuthTakesPriorityOverHubToken(t *testing.T) {
	tmpDir := t.TempDir()
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string {
		return filepath.Join(tmpDir, "credentials.json")
	})
	defer credentials.SetCredentialsPath(origPath)

	endpoint := "https://hub.example.com"

	err := credentials.Store(endpoint, &credentials.TokenResponse{
		AccessToken: "oauth-wins",
		ExpiresIn:   1 * time.Hour,
	})
	require.NoError(t, err)
	t.Setenv("SCION_HUB_TOKEN", "scion_pat_shouldnotuse")

	// OAuth still takes priority over the SCION_HUB_TOKEN env.
	token := getHubAccessToken(endpoint)
	assert.Equal(t, "oauth-wins", token)
}
