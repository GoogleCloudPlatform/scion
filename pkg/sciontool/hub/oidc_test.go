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
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	_ = os.Setenv(transportauth.EnvTransportToken, token)
	defer func() { _ = os.Unsetenv(transportauth.EnvTransportToken) }()

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource)
	_, ok := c.oidcSource.(*transportauth.InjectedSource)
	assert.True(t, ok, "should use InjectedSource")
	require.NotNil(t, c.client.Transport)
}

func TestConfigureOIDCTransport_MetadataMode(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	_ = os.Unsetenv(transportauth.EnvTransportToken)
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
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	_ = os.Unsetenv(transportauth.EnvTransportToken)
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
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	_ = os.Unsetenv(transportauth.EnvTransportToken)

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	assert.Nil(t, c.oidcSource, "should not configure OIDC when not on GCP and no injected token")
	assert.Nil(t, c.client.Transport, "transport should not be wrapped")
}

func TestConfigureOIDCTransport_SkipsMetadataWhenScionMetadataActive(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvMetadataMode, "assign")

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	assert.Nil(t, c.oidcSource, "should not configure OIDC metadata mode when scion metadata server is active")
}

func TestConfigureOIDCTransport_InjectedPriority(t *testing.T) {
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
	_, ok := c.oidcSource.(*transportauth.InjectedSource)
	assert.True(t, ok, "injected should take priority over metadata")
}

// --- E2E: both agent + OIDC headers ---

func TestOIDC_EndToEnd_BothHeaders(t *testing.T) {
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
	source := transportauth.NewInjectedSource()
	c := &Client{oidcSource: source}

	newToken := makeTestJWT(time.Now().Add(1 * time.Hour))
	tokens := []RefreshTokenEntry{
		{Layer: "app", Type: "scion_access", Value: "app-token", ExpiresIn: 36000},
		{Layer: "transport", Type: "google_oidc", Value: newToken, ExpiresIn: 3600, Audience: "https://hub.example.com"},
	}

	c.applyRefreshTokens(tokens)

	got, err := source.Token()
	require.NoError(t, err)
	assert.Equal(t, newToken, got)
}

func TestApplyRefreshTokens_NoOIDCSource(t *testing.T) {
	c := &Client{} // no oidcSource

	tokens := []RefreshTokenEntry{
		{Layer: "transport", Type: "google_oidc", Value: "token", ExpiresIn: 3600},
	}

	// Should not panic
	c.applyRefreshTokens(tokens)
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
