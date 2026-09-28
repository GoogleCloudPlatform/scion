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
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAuthInfo_NoAuth(t *testing.T) {
	// Clear all dev token sources so getAuthInfo doesn't find dev auth
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("HOME", t.TempDir())

	settings := &config.Settings{}
	info := getAuthInfo(settings, "https://hub.example.com")
	assert.Equal(t, "none", info.MethodType)
	assert.Equal(t, "none", info.Method)
}

func TestGetAuthInfo_DeprecatedTokenIgnored(t *testing.T) {
	// Clear higher-priority token sources
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("HOME", t.TempDir())

	// hub.token is deprecated and should no longer be used for auth
	settings := &config.Settings{
		Hub: &config.HubClientConfig{
			Token: "test-token",
		},
	}
	info := getAuthInfo(settings, "https://hub.example.com")
	// Should NOT return bearer — token is deprecated
	assert.NotEqual(t, "bearer", info.MethodType)
}

func TestGetAuthInfo_DeprecatedAPIKeyIgnored(t *testing.T) {
	// Clear higher-priority token sources
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("HOME", t.TempDir())

	// hub.apiKey is deprecated and should no longer be used for auth
	settings := &config.Settings{
		Hub: &config.HubClientConfig{
			APIKey: "test-api-key",
		},
	}
	info := getAuthInfo(settings, "https://hub.example.com")
	// Should NOT return apikey — apiKey is deprecated
	assert.NotEqual(t, "apikey", info.MethodType)
}

func TestGetAuthInfo_EnvTokenTakesPriority(t *testing.T) {
	// Clear higher-priority token sources so SCION_HUB_TOKEN is reached
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("HOME", t.TempDir())

	// SCION_HUB_TOKEN env var should work for bearer auth
	settings := &config.Settings{}
	t.Setenv("SCION_HUB_TOKEN", "env-token")
	info := getAuthInfo(settings, "https://hub.example.com")
	assert.Equal(t, "bearer", info.MethodType)
	assert.Equal(t, "SCION_HUB_TOKEN env", info.Source)
}

func TestGetAuthInfo_NilHub(t *testing.T) {
	// Clear all dev token sources so getAuthInfo doesn't find dev auth
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("HOME", t.TempDir())

	settings := &config.Settings{
		Hub: nil,
	}
	info := getAuthInfo(settings, "")
	assert.Equal(t, "none", info.MethodType)
}

func TestGetAuthInfo_DevAuthPreferredOverStaleAgentTokenOnLocalhost(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("SCION_AGENT_ID", "")

	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a non-dev agent token (stale JWT from a previous remote hub)
	if err := os.WriteFile(filepath.Join(scionDir, "scion-token"), []byte("eyJhbGciOiJIUzI1NiJ9.stale-jwt"), 0600); err != nil {
		t.Fatal(err)
	}

	// Write a dev token (from the currently running local server)
	if err := os.WriteFile(filepath.Join(scionDir, "dev-token"), []byte("scion_dev_abc123"), 0600); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{}
	info := getAuthInfo(settings, "http://localhost:8080")
	assert.Equal(t, "devauth", info.MethodType)
	assert.Equal(t, "Dev auth", info.Method)
	assert.True(t, info.IsDevAuth)
}

// TestGetAuthInfo_HubManagedAgentUsesRealTokenOnLocalhost is the counterpart
// to TestGetAuthInfo_DevAuthPreferredOverStaleAgentTokenOnLocalhost: inside a
// container the Runtime Broker started (SCION_AGENT_ID set), the scion-token
// file is freshly minted for *this* Hub, not stale — so it must NOT be
// overridden by dev auth even when the endpoint is localhost, since some Hub
// endpoints (e.g. an agent's own outbound message to a user) require the
// caller's authenticated identity to be that specific agent, which dev auth
// does not provide.
func TestGetAuthInfo_HubManagedAgentUsesRealTokenOnLocalhost(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("SCION_AGENT_ID", "agent-uuid-123") // Hub-managed agent context.

	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Real per-agent token, freshly issued by the broker for this Hub.
	if err := os.WriteFile(filepath.Join(scionDir, "scion-token"), []byte("eyJhbGciOiJIUzI1NiJ9.fresh-agent-jwt"), 0600); err != nil {
		t.Fatal(err)
	}

	// A dev token also happens to be present (this IS a dev-mode Hub) — must
	// not be preferred over the agent's own token in this context.
	if err := os.WriteFile(filepath.Join(scionDir, "dev-token"), []byte("scion_dev_abc123"), 0600); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{}
	info := getAuthInfo(settings, "http://localhost:8080")
	assert.Equal(t, "agent_token", info.MethodType)
	assert.Equal(t, "Agent token", info.Method)
	assert.Equal(t, "scion-token file", info.Source)
	assert.False(t, info.IsDevAuth)
}

func TestGetAuthInfo_AgentTokenUsedOnRemoteEndpoint(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")

	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a non-dev agent token
	if err := os.WriteFile(filepath.Join(scionDir, "scion-token"), []byte("eyJhbGciOiJIUzI1NiJ9.valid-jwt"), 0600); err != nil {
		t.Fatal(err)
	}

	// Write a dev token (leftover from a previous local server)
	if err := os.WriteFile(filepath.Join(scionDir, "dev-token"), []byte("scion_dev_abc123"), 0600); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{}
	info := getAuthInfo(settings, "https://hub.example.com")
	assert.Equal(t, "agent_token", info.MethodType)
	assert.Equal(t, "Agent token", info.Method)
	assert.Equal(t, "scion-token file", info.Source)
}

func TestGetAuthInfo_DevAgentTokenUsedDirectly(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")

	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a dev token in the scion-token file (agent launched by dev server)
	if err := os.WriteFile(filepath.Join(scionDir, "scion-token"), []byte("scion_dev_abc123"), 0600); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{}
	info := getAuthInfo(settings, "http://localhost:8080")
	assert.Equal(t, "agent_token", info.MethodType)
	assert.Equal(t, "Agent token (dev)", info.Method)
	assert.True(t, info.IsDevAuth)
}

func TestIsLocalhostEndpoint(t *testing.T) {
	assert.True(t, isLocalhostEndpoint("http://localhost:8080"))
	assert.True(t, isLocalhostEndpoint("https://localhost:443"))
	assert.True(t, isLocalhostEndpoint("http://127.0.0.1:8080"))
	assert.True(t, isLocalhostEndpoint("http://[::1]:8080"))
	assert.False(t, isLocalhostEndpoint("https://hub.example.com"))
	assert.False(t, isLocalhostEndpoint("http://192.168.1.100:8080"))
	assert.False(t, isLocalhostEndpoint(""))
}

func TestGetHubEnabledScope_GlobalScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_HUB_ENDPOINT", "")

	enabled := true
	settings := &config.Settings{
		Hub: &config.HubClientConfig{Enabled: &enabled},
	}

	scope := getHubEnabledScope("/some/path", true, settings)
	assert.Equal(t, "global", scope.Scope)
	assert.False(t, scope.Inherited)
	assert.True(t, scope.Enabled)
}

func TestGetHubEnabledScope_ProjectHasOwnSetting(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_HUB_ENDPOINT", "")

	// Create project settings with hub.enabled
	projectDir := filepath.Join(tmpDir, "project-scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
		[]byte("hub:\n  enabled: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	enabled := true
	settings := &config.Settings{
		Hub: &config.HubClientConfig{Enabled: &enabled},
	}

	scope := getHubEnabledScope(projectDir, false, settings)
	assert.Equal(t, "project", scope.Scope)
	assert.False(t, scope.Inherited)
	assert.True(t, scope.Enabled)
}

func TestGetHubEnabledScope_InheritedFromGlobal(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_HUB_ENDPOINT", "")

	// Create global settings with hub.enabled
	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("hub:\n  enabled: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create project settings WITHOUT hub.enabled
	projectDir := filepath.Join(tmpDir, "project-scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
		[]byte("runtime: docker\n"), 0644); err != nil {
		t.Fatal(err)
	}

	enabled := true
	settings := &config.Settings{
		Hub: &config.HubClientConfig{Enabled: &enabled},
	}

	scope := getHubEnabledScope(projectDir, false, settings)
	assert.Equal(t, "global", scope.Scope)
	assert.True(t, scope.Inherited)
	assert.True(t, scope.Enabled)
}

func TestGetHubEnabledScope_DefaultWhenNothingSet(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_HUB_ENDPOINT", "")

	// Create empty global dir
	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create project settings WITHOUT hub.enabled
	projectDir := filepath.Join(tmpDir, "project-scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{}

	scope := getHubEnabledScope(projectDir, false, settings)
	assert.Equal(t, "default", scope.Scope)
	assert.False(t, scope.Inherited)
	assert.False(t, scope.Enabled)
}

func TestGetHubEndpointScope_FromProject(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_HUB_ENDPOINT", "")

	// Save original hubEndpoint and restore after test
	origHubEndpoint := hubEndpoint
	hubEndpoint = ""
	defer func() { hubEndpoint = origHubEndpoint }()

	// Create project settings with hub.endpoint
	projectDir := filepath.Join(tmpDir, "project-scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
		[]byte("hub:\n  endpoint: https://project-hub.example.com\n"), 0644); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{
		Hub: &config.HubClientConfig{Endpoint: "https://project-hub.example.com"},
	}

	scope := getHubEndpointScope(projectDir, false, settings)
	assert.Equal(t, "project", scope.Source)
	assert.False(t, scope.Inherited)
	assert.Equal(t, "https://project-hub.example.com", scope.Endpoint)
}

func TestGetHubEndpointScope_InheritedFromGlobal(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_HUB_ENDPOINT", "")

	origHubEndpoint := hubEndpoint
	hubEndpoint = ""
	defer func() { hubEndpoint = origHubEndpoint }()

	// Create global settings with hub.endpoint
	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("hub:\n  endpoint: https://global-hub.example.com\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create project settings WITHOUT hub.endpoint
	projectDir := filepath.Join(tmpDir, "project-scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
		[]byte("runtime: docker\n"), 0644); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{
		Hub: &config.HubClientConfig{Endpoint: "https://global-hub.example.com"},
	}

	scope := getHubEndpointScope(projectDir, false, settings)
	assert.Equal(t, "global", scope.Source)
	assert.True(t, scope.Inherited)
	assert.Equal(t, "https://global-hub.example.com", scope.Endpoint)
}

func TestGetHubEndpointScope_FromEnv(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SCION_HUB_ENDPOINT", "https://env-hub.example.com")

	origHubEndpoint := hubEndpoint
	hubEndpoint = ""
	defer func() { hubEndpoint = origHubEndpoint }()

	// Create empty global dir
	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create project settings WITHOUT hub.endpoint
	projectDir := filepath.Join(tmpDir, "project-scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := &config.Settings{}

	scope := getHubEndpointScope(projectDir, false, settings)
	assert.Equal(t, "env", scope.Source)
	assert.True(t, scope.Inherited)
}

func TestGetHubEndpointScope_FromFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_HUB_ENDPOINT", "")

	origHubEndpoint := hubEndpoint
	hubEndpoint = "https://flag-hub.example.com"
	defer func() { hubEndpoint = origHubEndpoint }()

	settings := &config.Settings{}

	scope := getHubEndpointScope("/some/path", false, settings)
	assert.Equal(t, "flag", scope.Source)
	assert.False(t, scope.Inherited)
	assert.Equal(t, "https://flag-hub.example.com", scope.Endpoint)
}

func TestParseJWTExpiry_ValidToken(t *testing.T) {
	// Build a minimal JWT with exp claim (header.payload.signature)
	// Header: {"alg":"HS256","typ":"JWT"}
	header := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
	// Payload: {"exp":1700000000} -> 2023-11-14T22:13:20Z
	payload := "eyJleHAiOjE3MDAwMDAwMDB9"
	token := header + "." + payload + ".fakesig"

	expiry := parseJWTExpiry(token)
	assert.NotNil(t, expiry)
	assert.Equal(t, int64(1700000000), expiry.Unix())
}

func TestParseJWTExpiry_NoExpClaim(t *testing.T) {
	header := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
	// Payload: {"sub":"test"}
	payload := "eyJzdWIiOiJ0ZXN0In0"
	token := header + "." + payload + ".fakesig"

	expiry := parseJWTExpiry(token)
	assert.Nil(t, expiry)
}

func TestParseJWTExpiry_InvalidToken(t *testing.T) {
	assert.Nil(t, parseJWTExpiry("not-a-jwt"))
	assert.Nil(t, parseJWTExpiry(""))
	assert.Nil(t, parseJWTExpiry("a.!!!invalid-base64!!!.c"))
}

func TestParseDefaultBranch_ParsesSymref(t *testing.T) {
	// Real output from `git ls-remote --symref <url> HEAD`
	output := "ref: refs/heads/main\tHEAD\n5f3c6e72abc123def456 HEAD\n"
	result := parseDefaultBranch(output)
	assert.Equal(t, "main", result)
}

func TestParseDefaultBranch_NonMainBranch(t *testing.T) {
	output := "ref: refs/heads/develop\tHEAD\nabc123 HEAD\n"
	result := parseDefaultBranch(output)
	assert.Equal(t, "develop", result)
}

func TestParseDefaultBranch_NoMatch(t *testing.T) {
	// Output that doesn't contain the expected symref line
	output := "abc123def456 HEAD\n"
	result := parseDefaultBranch(output)
	assert.Equal(t, "", result)
}

func TestParseDefaultBranch_EmptyOutput(t *testing.T) {
	result := parseDefaultBranch("")
	assert.Equal(t, "", result)
}

// TestHubUnknownSubcommand_RejectsRemovedGroveAlias is a regression test for
// the removed "hub groves"/"hub grove" alias. hubCmd has no subcommand
// named "groves", but before hubCmd was made Runnable, cobra silently fell
// through to hub's own help with exit status 0 for any unrecognized hub
// subcommand — including this one — instead of reporting an error. That
// made a removed command indistinguishable from a typo and let old
// scripts' error checks pass silently. This executes the real rootCmd,
// since the behavior depends on cobra's command-resolution path through
// the actual tree, not a synthetic one.
func TestHubUnknownSubcommand_RejectsRemovedGroveAlias(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetArgs([]string{"hub", "groves", "list"})
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	defer func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	}()

	err := rootCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown command "groves" for "scion hub"`)
}

// TestHubBareInvocation_PrintsHelpOutsideProject is a regression test for a
// side effect of making hubCmd Runnable: once ValidateArgs runs for "hub",
// execution would otherwise continue into root's PersistentPreRunE, which
// requires an active scion project for "hub" (it is not in that hook's
// exempt command list). A bare "scion hub" run outside any project would
// then fail with "not in a scion project" instead of printing hub's help,
// even though a plain "scion hub" never did anything project-specific
// before. hubCmd's Args validator returns pflag.ErrHelp for zero args (and
// for a leading "help" argument, since cobra only auto-registers a real
// "help" subcommand on the root command) specifically to make cobra print
// help and stop *before* that hook runs. This test runs both cases from a
// temp directory that is not a scion project, with a clean HOME, so it
// fails loudly if that short-circuit regresses for either one — a mutation
// that only special-cases zero args (dropping the "help" branch) passes
// unless the "help" sub-case below is present.
func TestHubBareInvocation_PrintsHelpOutsideProject(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "bare", args: []string{"hub"}},
		{name: "help", args: []string{"hub", "help"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Chdir(t.TempDir())

			var buf bytes.Buffer
			rootCmd.SetArgs(tc.args)
			rootCmd.SetOut(&buf)
			rootCmd.SetErr(&buf)
			defer func() {
				rootCmd.SetArgs(nil)
				rootCmd.SetOut(nil)
				rootCmd.SetErr(nil)
			}()

			err := rootCmd.Execute()
			require.NoError(t, err)
			assert.Contains(t, buf.String(), "Commands for interacting with a remote Scion Hub")
		})
	}
}
