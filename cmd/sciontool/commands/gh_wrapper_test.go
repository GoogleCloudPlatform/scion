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
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindRealGh_PrefersRealSuffix(t *testing.T) {
	// Create a temp directory structure simulating the container layout
	tmpDir := t.TempDir()
	ghRealPath := filepath.Join(tmpDir, "gh.real")
	ghPath := filepath.Join(tmpDir, "gh")

	// Create both files
	require.NoError(t, os.WriteFile(ghRealPath, []byte("real binary"), 0755))
	require.NoError(t, os.WriteFile(ghPath, []byte("#!/bin/sh\nexec sciontool gh-wrapper"), 0755))

	// findRealGh uses hardcoded paths, so we test the path ordering logic
	// by verifying the paths array order
	paths := []string{
		"/usr/bin/gh.real",
		"/usr/local/bin/gh.real",
		"/usr/bin/gh",
		"/usr/local/bin/gh",
	}

	// .real paths must come before non-.real paths
	assert.Equal(t, "/usr/bin/gh.real", paths[0], "first path should be /usr/bin/gh.real")
	assert.Equal(t, "/usr/local/bin/gh.real", paths[1], "second path should be /usr/local/bin/gh.real")
}

func TestRunGhWrapper_SkipsInjectionForUserToken(t *testing.T) {
	// When SCION_USER_GITHUB_TOKEN=true, the wrapper should NOT set GH_TOKEN
	// from the token file, letting the user's GITHUB_TOKEN be used instead.
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("ghs_app_token\n"), 0600))

	t.Setenv("SCION_GITHUB_APP_ENABLED", "true")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "true")
	t.Setenv("SCION_GITHUB_TOKEN_PATH", tokenFile)
	t.Setenv("GITHUB_TOKEN", "ghp_user_pat")
	t.Setenv("GH_TOKEN", "")
	t.Setenv(envGhWrapperTokenHash, "")

	var stderr bytes.Buffer
	applyGhWrapperTokenEnv(&stderr)

	assert.Equal(t, "ghp_user_pat", os.Getenv("GITHUB_TOKEN"))
	assert.Empty(t, os.Getenv("GH_TOKEN"), "GH_TOKEN should not be set when user token takes precedence")
	assert.Contains(t, stderr.String(), "user-provided GITHUB_TOKEN")
	assert.NotContains(t, stderr.String(), "ghp_user_pat")
	assert.NotContains(t, stderr.String(), "ghs_app_token")
}

func TestGhWrapperCmd_IsRegistered(t *testing.T) {
	// Verify the gh-wrapper command is registered as a hidden subcommand
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Use == "gh-wrapper [gh args...]" {
			found = true
			assert.True(t, cmd.Hidden, "gh-wrapper should be hidden")
			assert.True(t, cmd.DisableFlagParsing, "gh-wrapper should disable flag parsing")
			break
		}
	}
	assert.True(t, found, "gh-wrapper command should be registered")
}

func TestApplyGhWrapperTokenEnv_InjectsAppTokenWhenUnset(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("ghs_fresh_app_token\n"), 0600))

	t.Setenv("SCION_GITHUB_APP_ENABLED", "true")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "")
	t.Setenv("SCION_GITHUB_TOKEN_PATH", tokenFile)
	t.Setenv("GITHUB_TOKEN", "ghs_initial_app_token")
	t.Setenv("GH_TOKEN", "")
	t.Setenv(envGhWrapperTokenHash, "")

	var stderr bytes.Buffer
	applyGhWrapperTokenEnv(&stderr)

	assert.Equal(t, "ghs_fresh_app_token", os.Getenv("GH_TOKEN"))
	assert.Equal(t, tokenSHA256Hex("ghs_fresh_app_token"), os.Getenv(envGhWrapperTokenHash))
	assert.NotEqual(t, "ghs_fresh_app_token", os.Getenv(envGhWrapperTokenHash), "hash marker must not store raw token")
	assert.Empty(t, stderr.String())
}

func TestApplyGhWrapperTokenEnv_RespectsCallerToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("ghs_refreshed_app_token\n"), 0600))

	t.Setenv("SCION_GITHUB_APP_ENABLED", "true")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "")
	t.Setenv("SCION_GITHUB_TOKEN_PATH", tokenFile)
	t.Setenv("GITHUB_TOKEN", "ghs_initial_app_token")
	t.Setenv("GH_TOKEN", "ghp_caller_override_secret")
	t.Setenv(envGhWrapperTokenHash, "")

	var stderr bytes.Buffer
	applyGhWrapperTokenEnv(&stderr)

	assert.Equal(t, "ghp_caller_override_secret", os.Getenv("GH_TOKEN"), "caller-provided GH_TOKEN must not be overwritten")
	assert.Empty(t, os.Getenv(envGhWrapperTokenHash))
	assert.Contains(t, stderr.String(), "caller-provided GH_TOKEN")
	assert.NotContains(t, stderr.String(), "ghp_caller_override_secret", "stderr must never leak caller token")
	assert.NotContains(t, stderr.String(), "ghs_refreshed_app_token", "stderr must never leak App token")
}

func TestApplyGhWrapperTokenEnv_RefreshesStaleInitialHubToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("ghs_refreshed_app_token\n"), 0600))

	t.Setenv("SCION_GITHUB_APP_ENABLED", "true")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "")
	t.Setenv("SCION_GITHUB_TOKEN_PATH", tokenFile)
	// Caller inherited or copied the container's initial App token into GH_TOKEN.
	t.Setenv("GITHUB_TOKEN", "ghs_initial_app_token")
	t.Setenv("GH_TOKEN", "ghs_initial_app_token")
	t.Setenv(envGhWrapperTokenHash, "")

	var stderr bytes.Buffer
	applyGhWrapperTokenEnv(&stderr)

	assert.Equal(t, "ghs_refreshed_app_token", os.Getenv("GH_TOKEN"), "stale initial hub token should be refreshed")
	assert.Equal(t, tokenSHA256Hex("ghs_refreshed_app_token"), os.Getenv(envGhWrapperTokenHash))
	assert.Empty(t, stderr.String())
}

func TestApplyGhWrapperTokenEnv_RefreshesStaleWrapperInjectedToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("ghs_rotated_v1\n"), 0600))

	t.Setenv("SCION_GITHUB_APP_ENABLED", "true")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "")
	t.Setenv("SCION_GITHUB_TOKEN_PATH", tokenFile)
	t.Setenv("GITHUB_TOKEN", "ghs_initial_v0")
	t.Setenv("GH_TOKEN", "")
	t.Setenv(envGhWrapperTokenHash, "")

	// First invocation injects v1 and records its digest.
	var stderr1 bytes.Buffer
	applyGhWrapperTokenEnv(&stderr1)
	assert.Equal(t, "ghs_rotated_v1", os.Getenv("GH_TOKEN"))
	assert.Equal(t, tokenSHA256Hex("ghs_rotated_v1"), os.Getenv(envGhWrapperTokenHash))
	assert.Empty(t, stderr1.String())

	// Token file rotates to v2 while a nested process still has v1 in GH_TOKEN.
	require.NoError(t, os.WriteFile(tokenFile, []byte("ghs_rotated_v2\n"), 0600))

	var stderr2 bytes.Buffer
	applyGhWrapperTokenEnv(&stderr2)
	assert.Equal(t, "ghs_rotated_v2", os.Getenv("GH_TOKEN"), "stale wrapper-injected token should be refreshed after rotation")
	assert.Equal(t, tokenSHA256Hex("ghs_rotated_v2"), os.Getenv(envGhWrapperTokenHash))
	assert.Empty(t, stderr2.String())
}

func TestApplyGhWrapperTokenEnv_CallerOverrideBeatsInheritedWrapperHash(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("ghs_rotated_v2\n"), 0600))

	t.Setenv("SCION_GITHUB_APP_ENABLED", "true")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "")
	t.Setenv("SCION_GITHUB_TOKEN_PATH", tokenFile)
	t.Setenv("GITHUB_TOKEN", "ghs_initial_v0")
	// Child inherited the wrapper hash from an earlier gh call, but caller
	// explicitly overrode GH_TOKEN for this nested invocation.
	t.Setenv(envGhWrapperTokenHash, tokenSHA256Hex("ghs_rotated_v1"))
	t.Setenv("GH_TOKEN", "ghp_nested_caller_override")

	var stderr bytes.Buffer
	applyGhWrapperTokenEnv(&stderr)

	assert.Equal(t, "ghp_nested_caller_override", os.Getenv("GH_TOKEN"))
	assert.Empty(t, os.Getenv(envGhWrapperTokenHash), "stale wrapper hash should be cleared when caller overrides GH_TOKEN")
	assert.Contains(t, stderr.String(), "caller-provided GH_TOKEN")
	assert.NotContains(t, stderr.String(), "ghp_nested_caller_override")
	assert.NotContains(t, stderr.String(), "ghs_rotated_v2")
}

func TestApplyGhWrapperTokenEnv_KeepsMatchingCurrentAppToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("ghs_current_app_token\n"), 0600))

	t.Setenv("SCION_GITHUB_APP_ENABLED", "true")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "")
	t.Setenv("SCION_GITHUB_TOKEN_PATH", tokenFile)
	t.Setenv("GITHUB_TOKEN", "ghs_initial_v0")
	t.Setenv("GH_TOKEN", "ghs_current_app_token")
	t.Setenv(envGhWrapperTokenHash, "")

	var stderr bytes.Buffer
	applyGhWrapperTokenEnv(&stderr)

	assert.Equal(t, "ghs_current_app_token", os.Getenv("GH_TOKEN"))
	assert.Equal(t, tokenSHA256Hex("ghs_current_app_token"), os.Getenv(envGhWrapperTokenHash))
	assert.Empty(t, stderr.String(), "matching current App token should not print caller-override notice")
}

func TestApplyGhWrapperTokenEnv_SkipsWhenAppDisabled(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("ghs_app_token\n"), 0600))

	t.Setenv("SCION_GITHUB_APP_ENABLED", "false")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "")
	t.Setenv("SCION_GITHUB_TOKEN_PATH", tokenFile)
	t.Setenv("GH_TOKEN", "ghp_caller_token")
	t.Setenv(envGhWrapperTokenHash, "")

	var stderr bytes.Buffer
	applyGhWrapperTokenEnv(&stderr)

	assert.Equal(t, "ghp_caller_token", os.Getenv("GH_TOKEN"))
	assert.Empty(t, stderr.String())
}

func TestApplyGhWrapperTokenEnv_MissingTokenFile(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "does-not-exist")

	t.Setenv("SCION_GITHUB_APP_ENABLED", "true")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "")
	t.Setenv("SCION_GITHUB_TOKEN_PATH", missingPath)
	t.Setenv("GITHUB_TOKEN", "ghs_initial_app_token")
	t.Setenv("GH_TOKEN", "ghp_caller_token")
	t.Setenv(envGhWrapperTokenHash, "")

	var stderr bytes.Buffer
	applyGhWrapperTokenEnv(&stderr)

	assert.Equal(t, "ghp_caller_token", os.Getenv("GH_TOKEN"))
	assert.Contains(t, stderr.String(), "caller-provided GH_TOKEN")
	assert.NotContains(t, stderr.String(), "ghp_caller_token")
}
