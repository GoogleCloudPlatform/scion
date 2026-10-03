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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

func makeTestJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]interface{}{"exp": exp.Unix(), "iss": "test"})
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fakesig"))
	return fmt.Sprintf("%s.%s.%s", header, payloadB64, sig)
}

func overrideGCPDetection(val bool) func() {
	orig := transportauth.IsOnGCEFunc
	transportauth.IsOnGCEFunc = func() bool { return val }
	return func() { transportauth.IsOnGCEFunc = orig }
}

// --- configureOIDCTransport tests ---

func TestConfigureOIDCTransport_InjectedMode(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	_ = os.Setenv(transportauth.EnvTransportToken, token)
	defer func() { _ = os.Unsetenv(transportauth.EnvTransportToken) }()

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource)
	_, ok := c.oidcSource.(*transportauth.FileSource)
	assert.True(t, ok, "should use the file-backed source")
	require.NotNil(t, c.client.Transport)
}

func TestConfigureOIDCTransport_MetadataMode(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	_ = os.Unsetenv(transportauth.EnvTransportToken)
	_ = os.Unsetenv(transportauth.EnvTransportTokenFile)
	_ = os.Unsetenv(transportauth.EnvMetadataMode)

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource)
	src, ok := c.oidcSource.(*transportauth.MetadataSource)
	assert.True(t, ok, "should use MetadataSource")
	assert.Equal(t, "https://hub.example.com", src.Audience())
}

func TestConfigureOIDCTransport_MetadataMode_AudienceOverride(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	_ = os.Unsetenv(transportauth.EnvTransportToken)
	_ = os.Unsetenv(transportauth.EnvTransportTokenFile)
	_ = os.Unsetenv(transportauth.EnvMetadataMode)
	_ = os.Setenv(transportauth.EnvHubOIDCAudience, "https://custom-audience.example.com")
	defer func() { _ = os.Unsetenv(transportauth.EnvHubOIDCAudience) }()

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource)
	src, ok := c.oidcSource.(*transportauth.MetadataSource)
	assert.True(t, ok, "should use MetadataSource")
	assert.Equal(t, "https://custom-audience.example.com", src.Audience())
}

func TestConfigureOIDCTransport_NotOnGCP(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	_ = os.Unsetenv(transportauth.EnvTransportToken)
	_ = os.Unsetenv(transportauth.EnvTransportTokenFile)

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	assert.Nil(t, c.oidcSource, "should not configure OIDC when not on GCP and no injected token")
	assert.Nil(t, c.client.Transport, "transport should not be wrapped")
}

func TestConfigureOIDCTransport_SkipsMetadataWhenScionMetadataActive(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	t.Setenv(transportauth.EnvMetadataMode, "assign")

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	assert.Nil(t, c.oidcSource, "should not configure OIDC metadata mode when scion metadata server is active")
}

// TestConfigureOIDCTransport_PassthroughStillUsesMetadata is the regression
// guard for ptone/scion#1882: SCION_METADATA_MODE=passthrough does not
// redirect the real GCE metadata server (unlike assign/block), so ambient-SA
// OIDC via MetadataSource must still be configured for it.
func TestConfigureOIDCTransport_PassthroughStillUsesMetadata(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	t.Setenv(transportauth.EnvMetadataMode, "passthrough")

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource, "passthrough must not disable ambient-SA OIDC transport")
	_, ok := c.oidcSource.(*transportauth.MetadataSource)
	assert.True(t, ok, "should use MetadataSource")
}

func TestConfigureOIDCTransport_InjectedPriority(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	_ = os.Setenv(transportauth.EnvTransportToken, token)
	defer func() { _ = os.Unsetenv(transportauth.EnvTransportToken) }()

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource)
	_, ok := c.oidcSource.(*transportauth.FileSource)
	assert.True(t, ok, "injected should take priority over metadata")
}

// --- E2E: both agent + OIDC headers ---

func TestOIDC_EndToEnd_BothHeaders(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	t.Setenv(transportauth.EnvTransportMode, "")
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	t.Setenv(transportauth.EnvTransportToken, token)

	var gotAuth, gotAgentToken string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAgentToken = r.Header.Get("X-Scion-Agent-Token")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer hubSrv.Close()

	c := &Client{
		hubURL:         hubSrv.URL,
		token:          "test-agent-token",
		agentID:        "test-agent-123",
		maxRetries:     1,
		retryBaseDelay: 10 * time.Millisecond,
		retryMaxDelay:  10 * time.Millisecond,
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
	c.configureOIDCTransport()

	err := c.UpdateStatus(context.Background(), StatusUpdate{
		Status:  "running",
		Message: "test",
	})
	require.NoError(t, err)

	assert.Equal(t, "Bearer "+token, gotAuth, "OIDC Authorization header should be set")
	assert.Equal(t, "test-agent-token", gotAgentToken, "X-Scion-Agent-Token should still be set")
}

// --- applyRefreshTokens tests ---

func TestApplyRefreshTokens_TransportToken(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	source := transportauth.NewInjectedSource()
	c := &Client{oidcSource: source}

	newToken := makeTestJWT(time.Now().Add(1 * time.Hour))
	tokens := []RefreshTokenEntry{
		{Layer: "app", Type: "scion_access", Value: "app-token", ExpiresIn: 36000},
		{Layer: "transport", Type: "google_oidc", Value: newToken, ExpiresIn: 3600, Audience: "https://hub.example.com"},
	}

	c.applyRefreshTokens(tokens, 0, 0)

	got, err := source.Token()
	require.NoError(t, err)
	assert.Equal(t, newToken, got)
}

func TestApplyRefreshTokens_NoOIDCSource(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	c := &Client{} // no oidcSource

	tokens := []RefreshTokenEntry{
		{Layer: "transport", Type: "google_oidc", Value: "token", ExpiresIn: 3600},
	}

	// Should not panic
	c.applyRefreshTokens(tokens, 0, 0)
}

// --- adjustRefreshForTransportTokens tests ---

func TestAdjustRefreshForTransportTokens_ShorterTransport(t *testing.T) {
	source := transportauth.NewInjectedSource()
	transportExpiry := time.Now().Add(50 * time.Minute)
	source.SetToken("tok", transportExpiry)

	c := &Client{oidcSource: source}

	appRefresh := time.Now().Add(8 * time.Hour)
	adjusted := c.adjustRefreshForTransportTokens(appRefresh)

	expectedTransportRefresh := transportExpiry.Add(-transportauth.RefreshMargin)
	assert.WithinDuration(t, expectedTransportRefresh, adjusted, 1*time.Second,
		"should use transport token's earlier refresh time")
}

func TestAdjustRefreshForTransportTokens_LongerTransport(t *testing.T) {
	source := transportauth.NewInjectedSource()
	transportExpiry := time.Now().Add(10 * time.Hour)
	source.SetToken("tok", transportExpiry)

	c := &Client{oidcSource: source}

	appRefresh := time.Now().Add(30 * time.Minute)
	adjusted := c.adjustRefreshForTransportTokens(appRefresh)

	assert.WithinDuration(t, appRefresh, adjusted, 1*time.Second,
		"should keep app token's earlier refresh time")
}

func TestAdjustRefreshForTransportTokens_NoSource(t *testing.T) {
	c := &Client{} // no oidcSource
	proposed := time.Now().Add(8 * time.Hour)
	adjusted := c.adjustRefreshForTransportTokens(proposed)
	assert.Equal(t, proposed, adjusted)
}

func TestAdjustRefreshForTransportTokens_MetadataSourceNoAdjust(t *testing.T) {
	source := transportauth.NewMetadataSourceWithURL("https://hub.example.com", "http://127.0.0.1:1")
	source.SetToken("tok", time.Now().Add(10*time.Minute))

	c := &Client{oidcSource: source}

	appRefresh := time.Now().Add(8 * time.Hour)
	adjusted := c.adjustRefreshForTransportTokens(appRefresh)

	assert.WithinDuration(t, appRefresh, adjusted, 1*time.Second,
		"metadata source self-refreshes; should not adjust app refresh time")
}

// --- refreshed transport credential shared through the file ---

// TestRefreshToken_PersistsTransportTokenForNewClients drives a real
// refresh: the hub returns a new transport credential in tokens[], the
// long-lived client persists it (mode 0600), and a client built afterwards
// (as hooks, sciontool subcommands and the scion CLI do) sends it even
// though the bootstrap env value has expired.
func TestRefreshToken_PersistsTransportTokenForNewClients(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	expired := makeTestJWT(time.Now().Add(-10 * time.Minute))
	refreshed := makeTestJWT(time.Now().Add(55 * time.Minute))
	t.Setenv(transportauth.EnvTransportToken, expired)
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	t.Setenv(transportauth.EnvTransportMode, "")

	var lastAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "app-credential-2",
				"expires_at": time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339),
				"tokens": []map[string]interface{}{
					{"layer": "transport", "type": "google_oidc", "value": refreshed, "expiresIn": 3300},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	pid1 := NewClientWithConfig(srv.URL, "app-credential", "agent-1")
	pid1.configureOIDCTransport()
	_, _, err := pid1.RefreshToken(context.Background())
	require.NoError(t, err)

	path := filepath.Join(home, ".scion", transportauth.TransportTokenFileName)
	fi, err := os.Stat(path)
	require.NoError(t, err, "refreshed transport credential must be persisted")
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, refreshed, string(data))

	// A freshly built client (still seeing the expired env value) uses the
	// refreshed file value.
	fresh := NewClientWithConfig(srv.URL, "app-credential-2", "agent-1")
	fresh.configureOIDCTransport()
	require.NoError(t, fresh.UpdateStatus(context.Background(), StatusUpdate{Status: "running"}))
	assert.Equal(t, "Bearer "+refreshed, lastAuth)

	st, ok := fresh.TransportSourceStatus()
	require.True(t, ok)
	assert.Equal(t, transportauth.SourceLabelFile, st.InUse)

	// transportauth.FromEnv (hubclient, the in-agent scion CLI) agrees.
	t.Setenv(transportauth.EnvTransportTokenFile, path)
	src, err := transportauth.FromEnv()
	require.NoError(t, err)
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, refreshed, got)
}

// TestConfigureOIDCTransport_FileWithoutEnv covers child processes after
// sciontool init removed the bootstrap env value: the file alone is enough.
func TestConfigureOIDCTransport_FileWithoutEnv(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	t.Setenv(transportauth.EnvTransportToken, "")
	tok := makeTestJWT(time.Now().Add(time.Hour))
	require.NoError(t, WriteTransportTokenFile(tok, 0, 0))
	t.Setenv(transportauth.EnvTransportTokenFile, TransportTokenFilePath())

	c := NewClientWithConfig("https://hub.example.com", "app", "agent-1")
	c.configureOIDCTransport()
	require.NotNil(t, c.oidcSource)
	got, err := c.oidcSource.Token()
	require.NoError(t, err)
	assert.Equal(t, tok, got)
}

// TestConfigureOIDCTransport_FileIgnoredWithoutEnv verifies a transport
// token file is not used unless the agent was given a transport token
// (SCION_TRANSPORT_TOKEN or SCION_TRANSPORT_TOKEN_FILE), matching
// transportauth.FromEnv.
func TestConfigureOIDCTransport_FileIgnoredWithoutEnv(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	cleanup := overrideGCPDetection(false)
	defer cleanup()
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	require.NoError(t, WriteTransportTokenFile(makeTestJWT(time.Now().Add(time.Hour)), 0, 0))

	assert.Nil(t, newTransportFileSource())
	c := NewClientWithConfig("https://hub.example.com", "app", "agent-1")
	c.configureOIDCTransport()
	if fs, ok := c.oidcSource.(*transportauth.FileSource); ok && fs != nil {
		t.Error("client uses the transport token file without a transport env var")
	}
}

// TestReadTransportTokenFile_TestGuard verifies tests cannot read the real
// default transport token file without SetTokenHome.
func TestReadTransportTokenFile_TestGuard(t *testing.T) {
	if tokenHomeOverridden {
		t.Skip("token home already overridden")
	}
	_, err := readTransportTokenFile(TransportTokenFilePath())
	require.Error(t, err)
}

// TestConfigureOIDCTransport_HonoursTransportMode verifies the sciontool
// client uses the same header as hubclient for SCION_TRANSPORT_MODE=iap.
func TestConfigureOIDCTransport_HonoursTransportMode(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	cleanup := overrideGCPDetection(false)
	defer cleanup()
	tok := makeTestJWT(time.Now().Add(time.Hour))
	t.Setenv(transportauth.EnvTransportToken, tok)
	t.Setenv(transportauth.EnvTransportMode, "iap")

	var gotProxy, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProxy = r.Header.Get("Proxy-Authorization")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	c := NewClientWithConfig(srv.URL, "app", "agent-1")
	c.configureOIDCTransport()
	require.NoError(t, c.UpdateStatus(context.Background(), StatusUpdate{Status: "running"}))
	assert.Equal(t, "Bearer "+tok, gotProxy)
	assert.Empty(t, gotAuth)

	h := http.Header{}
	require.NoError(t, c.ApplyTransportHeaders(h))
	assert.Equal(t, "Bearer "+tok, h.Get("Proxy-Authorization"))
}

func TestAdjustRefreshForTransportTokens_FileSource(t *testing.T) {
	src := transportauth.NewFileSource(filepath.Join(t.TempDir(), "missing"), nil)
	transportExpiry := time.Now().Add(50 * time.Minute)
	src.SetToken("tok", transportExpiry)
	c := &Client{oidcSource: src}

	adjusted := c.adjustRefreshForTransportTokens(time.Now().Add(8 * time.Hour))
	assert.WithinDuration(t, transportExpiry.Add(-transportauth.RefreshMargin), adjusted, time.Second)
}
