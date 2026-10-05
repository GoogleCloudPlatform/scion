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
	_, err := ReadTransportTokenFileGuarded(TransportTokenFilePath())
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

// TestRefreshToken_RecordsTransportOutcome covers the transport refresh
// status file that sciontool doctor reads: refreshed, failed (hub reported
// a mint failure) and absent (hub returned no transport token).
func TestRefreshToken_RecordsTransportOutcome(t *testing.T) {
	cases := []struct {
		name        string
		tokens      []map[string]interface{}
		transportEr string
		wantOutcome string
		wantError   string
	}{
		{
			name: "refreshed",
			tokens: []map[string]interface{}{
				{"layer": "transport", "type": "google_oidc", "value": makeTestJWT(time.Now().Add(time.Hour)), "expiresIn": 3600},
			},
			wantOutcome: TransportRefreshOutcomeRefreshed,
		},
		{
			name:        "failed",
			transportEr: "hub could not mint a transport token",
			wantOutcome: TransportRefreshOutcomeFailed,
			wantError:   "hub could not mint a transport token",
		},
		{
			name:        "absent",
			wantOutcome: TransportRefreshOutcomeAbsent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(SetTokenHome(t.TempDir()))
			cleanup := overrideGCPDetection(false)
			defer cleanup()
			t.Setenv(transportauth.EnvTransportToken, makeTestJWT(time.Now().Add(30*time.Minute)))
			t.Setenv(transportauth.EnvTransportTokenFile, "")
			t.Setenv(transportauth.EnvTransportMode, "")

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := map[string]interface{}{
					"token":      "app-credential-2",
					"expires_at": time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339),
					"tokens": append([]map[string]interface{}{
						{"layer": "app", "type": "scion_access", "value": "app-credential-2", "expiresIn": 36000},
					}, tc.tokens...),
				}
				if tc.transportEr != "" {
					body["transportError"] = tc.transportEr
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()

			c := NewClientWithConfig(srv.URL, "app-credential", "agent-1")
			c.configureOIDCTransport()
			_, _, err := c.RefreshToken(context.Background())
			require.NoError(t, err)

			st, ok := ReadTransportRefreshStatus()
			require.True(t, ok, "transport refresh status must be recorded")
			assert.Equal(t, tc.wantOutcome, st.Outcome)
			assert.Equal(t, tc.wantError, st.Error)
			assert.WithinDuration(t, time.Now(), st.At, time.Minute)

			fi, err := os.Stat(TransportRefreshStatusPath())
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0600), fi.Mode().Perm())
		})
	}
}

// TestRefreshToken_NoTransportNoStatus: agents that do not use a
// hub-provided transport token record nothing.
func TestRefreshToken_NoTransportNoStatus(t *testing.T) {
	// A hub with a transport minter sends a transport entry, or a
	// transportError, to every agent. An agent without a hub-provided
	// transport source must record nothing either way.
	cases := []struct {
		name           string
		transportEntry bool
		transportError string
	}{
		{name: "no transport in response"},
		{name: "transport entry", transportEntry: true},
		{name: "transport error", transportError: "hub could not mint a transport token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(SetTokenHome(t.TempDir()))
			cleanup := overrideGCPDetection(false)
			defer cleanup()
			t.Setenv(transportauth.EnvTransportToken, "")
			t.Setenv(transportauth.EnvTransportTokenFile, "")
			t.Setenv(transportauth.EnvTransportMode, "")

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := map[string]interface{}{
					"token":      "app-credential-2",
					"expires_at": time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339),
				}
				if tc.transportEntry {
					body["tokens"] = []map[string]interface{}{
						{"layer": "app", "type": "scion_access", "value": "app-credential-2", "expiresIn": 36000},
						{"layer": "transport", "type": "google_oidc", "value": makeTestJWT(time.Now().Add(time.Hour)), "expiresIn": 3600},
					}
				}
				if tc.transportError != "" {
					body["transportError"] = tc.transportError
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()

			c := NewClientWithConfig(srv.URL, "app-credential", "agent-1")
			c.configureOIDCTransport()
			require.False(t, c.hasHubProvidedTransport())
			_, _, err := c.RefreshToken(context.Background())
			require.NoError(t, err)
			_, ok := ReadTransportRefreshStatus()
			assert.False(t, ok, "status recorded without a hub-provided transport source")
			_, err = os.Lstat(TransportTokenFilePath())
			assert.True(t, os.IsNotExist(err), "token file written without a hub-provided transport source")
		})
	}
}

// TestAdoptTransportTokenFile covers reset-auth: a transport token written
// into the file from outside replaces the expired one in use, and the file
// is rewritten 0600.
func TestAdoptTransportTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	cleanup := overrideGCPDetection(false)
	defer cleanup()
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	t.Setenv(transportauth.EnvTransportToken, makeTestJWT(time.Now().Add(-5*time.Minute)))

	c := NewClientWithConfig("https://hub.example.com", "app", "agent-1")
	c.configureOIDCTransport()
	require.NotNil(t, c.oidcSource)

	// No file yet: nothing adopted.
	adopted, err := c.AdoptTransportTokenFile(0, 0)
	require.NoError(t, err)
	assert.False(t, adopted)

	// Simulate the broker's write (broader mode than we want).
	fresh := makeTestJWT(time.Now().Add(time.Hour))
	path := filepath.Join(home, ".scion", transportauth.TransportTokenFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(fresh+"\n"), 0644))

	adopted, err = c.AdoptTransportTokenFile(0, 0)
	require.NoError(t, err)
	assert.True(t, adopted)

	got, err := c.oidcSource.Token()
	require.NoError(t, err)
	assert.Equal(t, fresh, got)
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm())

	// The reset is recorded, so doctor does not show an earlier failure.
	st, ok := ReadTransportRefreshStatus()
	require.True(t, ok)
	assert.Equal(t, TransportRefreshOutcomeReset, st.Outcome)
}

// TestAdoptTransportTokenFile_UnparseableKeepsRefreshed models the
// reset-auth case: hours after dispatch the bootstrap value has expired and
// the valid credential is the in-memory refreshed one. An unparseable value
// from reset-auth must not replace it, the file is restored from it, and
// the reset is recorded as failed.
func TestAdoptTransportTokenFile_UnparseableKeepsRefreshed(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	cleanup := overrideGCPDetection(false)
	defer cleanup()
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	t.Setenv(transportauth.EnvTransportToken, makeTestJWT(time.Now().Add(-2*time.Hour)))

	c := NewClientWithConfig("https://hub.example.com", "app", "agent-1")
	c.configureOIDCTransport()
	require.NotNil(t, c.oidcSource)

	refreshed := makeTestJWT(time.Now().Add(40 * time.Minute))
	exp, err := transportauth.ParseTokenExpiry(refreshed)
	require.NoError(t, err)
	c.oidcSource.SetToken(refreshed, exp)

	// The broker overwrites the file with a value that does not parse.
	require.NoError(t, WriteTransportTokenFile("not-a-jwt", 0, 0))
	adopted, err := c.AdoptTransportTokenFile(0, 0)
	require.Error(t, err)
	assert.False(t, adopted)

	got, err := c.oidcSource.Token()
	require.NoError(t, err)
	assert.Equal(t, refreshed, got, "unparseable reset-auth value replaced the valid refreshed credential")

	data, err := os.ReadFile(TransportTokenFilePath())
	require.NoError(t, err)
	assert.Equal(t, refreshed, strings.TrimSpace(string(data)), "file not restored from the current credential")

	st, ok := ReadTransportRefreshStatus()
	require.True(t, ok)
	assert.Equal(t, TransportRefreshOutcomeFailed, st.Outcome)
	assert.NotContains(t, st.Error, "not-a-jwt")
}

// TestRemoveTransportTokenFile_RemovesStatus verifies the refresh status
// file is removed together with the token file.
func TestRemoveTransportTokenFile_RemovesStatus(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	require.NoError(t, WriteTransportTokenFile(makeTestJWT(time.Now().Add(time.Hour)), 0, 0))
	require.NoError(t, WriteTransportRefreshStatus(TransportRefreshStatus{At: time.Now(), Outcome: TransportRefreshOutcomeFailed}, 0, 0))

	removed, err := RemoveTransportTokenFile()
	require.NoError(t, err)
	assert.True(t, removed)
	_, ok := ReadTransportRefreshStatus()
	assert.False(t, ok, "status file left behind")

	// Status alone (no token file) is still cleaned up.
	require.NoError(t, WriteTransportRefreshStatus(TransportRefreshStatus{At: time.Now(), Outcome: TransportRefreshOutcomeFailed}, 0, 0))
	removed, err = RemoveTransportTokenFile()
	require.NoError(t, err)
	assert.False(t, removed)
	_, err = os.Lstat(TransportRefreshStatusPath())
	assert.True(t, os.IsNotExist(err))
}

// TestAdoptTransportTokenFile_NoHubProvidedTransport verifies a file is not
// adopted by a client that does not use a hub-provided transport token.
func TestAdoptTransportTokenFile_NoHubProvidedTransport(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	require.NoError(t, WriteTransportTokenFile(makeTestJWT(time.Now().Add(time.Hour)), 0, 0))
	c := NewClientWithConfig("https://hub.example.com", "app", "agent-1")
	adopted, err := c.AdoptTransportTokenFile(0, 0)
	require.NoError(t, err)
	assert.False(t, adopted)
}

// --- proxy mode without a dispatch-time transport token ---

// isolateLateTransport models an agent in proxy mode that started without
// a hub-provided transport token: no SCION_TRANSPORT_TOKEN(_FILE), not on
// GCP, HOME and the token home pointing at the same temp dir. It returns
// the home dir.
func isolateLateTransport(t *testing.T, mode string) string {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	t.Setenv("HOME", home)
	t.Cleanup(overrideGCPDetection(false))
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	t.Setenv(transportauth.EnvTransportAudience, "")
	t.Setenv(transportauth.EnvHubOIDCAudience, "")
	t.Setenv(transportauth.EnvMetadataMode, "")
	t.Setenv(transportauth.EnvTransportMode, mode)
	return home
}

// lateTransportHub serves token refreshes (with transportValue as the
// transport entry when non-empty) and records the last Proxy-Authorization
// header it saw on any other request.
func lateTransportHub(t *testing.T, transportValue string, lastProxyAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			body := map[string]interface{}{
				"token":      "app-credential-2",
				"expires_at": time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339),
			}
			if transportValue != "" {
				body["tokens"] = []map[string]interface{}{
					{"layer": "transport", "type": "google_oidc", "value": transportValue, "expiresIn": 3300},
				}
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		*lastProxyAuth = r.Header.Get("Proxy-Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertLateTransportInUse checks that the transport token file holds want
// with mode 0600, and that a newly built client and transportauth.FromEnv
// (the in-agent scion CLI) both use it.
func assertLateTransportInUse(t *testing.T, srvURL, want string, lastProxyAuth *string) {
	t.Helper()
	path := TransportTokenFilePath()
	fi, err := os.Stat(path)
	require.NoError(t, err, "transport token must be persisted")
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, strings.TrimSpace(string(data)))

	fresh := NewClientWithConfig(srvURL, "app-credential-2", "agent-1")
	fresh.configureOIDCTransport()
	require.NoError(t, fresh.UpdateStatus(context.Background(), StatusUpdate{Status: "running"}))
	assert.Equal(t, "Bearer "+want, *lastProxyAuth, "new client must send the recovered token in the iap header")
	st, ok := fresh.TransportSourceStatus()
	require.True(t, ok)
	assert.Equal(t, transportauth.SourceLabelFile, st.InUse)
	assert.False(t, st.Expiry.IsZero())

	src, err := transportauth.FromEnv()
	require.NoError(t, err)
	fs, ok := src.(*transportauth.FileSource)
	require.True(t, ok, "FromEnv must return a file-backed source, got %T", src)
	assert.Equal(t, path, fs.Path())
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestLateTransport_RefreshBootstrapsFileSource: the agent started in proxy
// mode without a transport token; a later refresh delivers one, which is
// persisted and used by the long-lived client, new clients and FromEnv.
func TestLateTransport_RefreshBootstrapsFileSource(t *testing.T) {
	isolateLateTransport(t, "iap")
	recovered := makeTestJWT(time.Now().Add(55 * time.Minute))
	var lastProxyAuth string
	srv := lateTransportHub(t, recovered, &lastProxyAuth)

	pid1 := NewClientWithConfig(srv.URL, "app-credential", "agent-1")
	pid1.configureOIDCTransport()
	require.True(t, pid1.hasHubProvidedTransport())

	// Before any token arrives: no transport header, and FromEnv finds nothing.
	require.NoError(t, pid1.UpdateStatus(context.Background(), StatusUpdate{Status: "running"}))
	assert.Empty(t, lastProxyAuth)
	src, err := transportauth.FromEnv()
	require.NoError(t, err)
	assert.Nil(t, src)

	_, _, err = pid1.RefreshToken(context.Background())
	require.NoError(t, err)

	require.NoError(t, pid1.UpdateStatus(context.Background(), StatusUpdate{Status: "running"}))
	assert.Equal(t, "Bearer "+recovered, lastProxyAuth, "long-lived client must use the recovered token")
	assertLateTransportInUse(t, srv.URL, recovered, &lastProxyAuth)

	st, ok := ReadTransportRefreshStatus()
	require.True(t, ok)
	assert.Equal(t, TransportRefreshOutcomeRefreshed, st.Outcome)

	// The refresh schedule now follows the transport token's expiry.
	far := time.Now().Add(5 * time.Hour)
	assert.True(t, pid1.adjustRefreshForTransportTokens(far).Before(far))
}

// TestLateTransport_ResetAuthBootstrapsFileSource: same, with the token
// delivered by reset-auth (written into the file from outside, then adopted).
func TestLateTransport_ResetAuthBootstrapsFileSource(t *testing.T) {
	home := isolateLateTransport(t, "iap")
	var lastProxyAuth string
	srv := lateTransportHub(t, "", &lastProxyAuth)

	pid1 := NewClientWithConfig(srv.URL, "app-credential", "agent-1")
	pid1.configureOIDCTransport()

	// Simulate the broker's write (broader mode than we want).
	recovered := makeTestJWT(time.Now().Add(time.Hour))
	path := filepath.Join(home, ".scion", transportauth.TransportTokenFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(recovered+"\n"), 0644))

	adopted, err := pid1.AdoptTransportTokenFile(0, 0)
	require.NoError(t, err)
	assert.True(t, adopted)
	got, err := pid1.oidcSource.Token()
	require.NoError(t, err)
	assert.Equal(t, recovered, got)

	assertLateTransportInUse(t, srv.URL, recovered, &lastProxyAuth)

	st, ok := ReadTransportRefreshStatus()
	require.True(t, ok)
	assert.Equal(t, TransportRefreshOutcomeReset, st.Outcome)
}

// TestLateTransport_ResetAuthUnparseableRemoved: with no credential to fall
// back on, an unparseable reset-auth value is not adopted and the file is
// removed so other processes do not use it.
func TestLateTransport_ResetAuthUnparseableRemoved(t *testing.T) {
	isolateLateTransport(t, "iap")
	pid1 := NewClientWithConfig("https://hub.example.com", "app", "agent-1")
	pid1.configureOIDCTransport()

	require.NoError(t, WriteTransportTokenFile("placeholder-not-a-jwt", 0, 0))
	adopted, err := pid1.AdoptTransportTokenFile(0, 0)
	require.Error(t, err)
	assert.False(t, adopted)
	_, err = os.Lstat(TransportTokenFilePath())
	assert.True(t, os.IsNotExist(err), "unparseable transport token file left in place")
	st, ok := ReadTransportRefreshStatus()
	require.True(t, ok)
	assert.Equal(t, TransportRefreshOutcomeFailed, st.Outcome)
}

// TestLateTransport_NonProxyModeNoSource: without a proxy mode nothing
// changes: no source is created, a refresh's transport entry is not
// persisted, and a file present on disk is ignored.
func TestLateTransport_NonProxyModeNoSource(t *testing.T) {
	for _, mode := range []string{"", "something-else"} {
		t.Run("mode="+mode, func(t *testing.T) {
			isolateLateTransport(t, mode)
			var lastProxyAuth string
			srv := lateTransportHub(t, makeTestJWT(time.Now().Add(time.Hour)), &lastProxyAuth)

			c := NewClientWithConfig(srv.URL, "app-credential", "agent-1")
			c.configureOIDCTransport()
			assert.Nil(t, c.oidcSource)
			_, _, err := c.RefreshToken(context.Background())
			require.NoError(t, err)
			_, err = os.Lstat(TransportTokenFilePath())
			assert.True(t, os.IsNotExist(err), "transport token persisted without a proxy mode")

			require.NoError(t, WriteTransportTokenFile(makeTestJWT(time.Now().Add(time.Hour)), 0, 0))
			src, err := transportauth.FromEnv()
			require.NoError(t, err)
			assert.Nil(t, src, "FromEnv used the file without a proxy mode")
			c2 := NewClientWithConfig(srv.URL, "app-credential", "agent-1")
			c2.configureOIDCTransport()
			assert.Nil(t, c2.oidcSource)
		})
	}
}

// TestLateTransport_ExistingSourceUnchanged: in proxy mode an existing
// source is kept: a hub-provided bootstrap value keeps the normal
// file-backed source (and refresh still works), and on GCP with the real
// metadata server the metadata source is kept.
func TestLateTransport_ExistingSourceUnchanged(t *testing.T) {
	t.Run("hub-provided", func(t *testing.T) {
		isolateLateTransport(t, "iap")
		boot := makeTestJWT(time.Now().Add(-5 * time.Minute))
		t.Setenv(transportauth.EnvTransportToken, boot)
		refreshed := makeTestJWT(time.Now().Add(55 * time.Minute))
		var lastProxyAuth string
		srv := lateTransportHub(t, refreshed, &lastProxyAuth)

		pid1 := NewClientWithConfig(srv.URL, "app-credential", "agent-1")
		pid1.configureOIDCTransport()
		assert.False(t, pid1.oidcLate)
		st, ok := pid1.TransportSourceStatus()
		require.True(t, ok)
		assert.True(t, st.EnvPresent)

		_, _, err := pid1.RefreshToken(context.Background())
		require.NoError(t, err)
		assertLateTransportInUse(t, srv.URL, refreshed, &lastProxyAuth)
	})
	t.Run("metadata", func(t *testing.T) {
		isolateLateTransport(t, "iap")
		t.Cleanup(overrideGCPDetection(true))
		t.Setenv(transportauth.EnvMetadataMode, "passthrough")
		require.NoError(t, WriteTransportTokenFile(makeTestJWT(time.Now().Add(time.Hour)), 0, 0))

		c := NewClientWithConfig("https://hub.example.com", "app", "agent-1")
		c.configureOIDCTransport()
		_, ok := c.oidcSource.(*transportauth.MetadataSource)
		assert.True(t, ok, "metadata source replaced, got %T", c.oidcSource)
		assert.False(t, c.oidcLate)
	})
}
