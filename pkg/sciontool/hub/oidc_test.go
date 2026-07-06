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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTestJWT builds a minimal JWT with the given expiry for testing.
func makeTestJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]interface{}{"exp": exp.Unix(), "iss": "test"})
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fakesig"))
	return fmt.Sprintf("%s.%s.%s", header, payloadB64, sig)
}

func overrideGCPDetection(val bool) func() {
	orig := isOnGCPFunc
	isOnGCPFunc = func() bool { return val }
	return func() { isOnGCPFunc = orig }
}

// --- injectedTokenSource tests ---

func TestInjectedTokenSource_SetAndGet(t *testing.T) {
	src := &injectedTokenSource{}
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	expiry := time.Now().Add(1 * time.Hour)

	src.setToken(token, expiry)

	got, err := src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token, got)
}

func TestInjectedTokenSource_Empty(t *testing.T) {
	src := &injectedTokenSource{}
	_, err := src.getToken()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no transport token")
}

func TestInjectedTokenSource_NearExpiry(t *testing.T) {
	src := &injectedTokenSource{}
	token := makeTestJWT(time.Now().Add(2 * time.Minute))
	expiry := time.Now().Add(2 * time.Minute) // within 5-min margin

	src.setToken(token, expiry)

	// Should still return the token (with a debug log warning)
	got, err := src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token, got)
}

func TestInjectedTokenSource_UpdateToken(t *testing.T) {
	src := &injectedTokenSource{}
	token1 := makeTestJWT(time.Now().Add(1 * time.Hour))
	token2 := makeTestJWT(time.Now().Add(2 * time.Hour))

	src.setToken(token1, time.Now().Add(1*time.Hour))
	got1, _ := src.getToken()
	assert.Equal(t, token1, got1)

	src.setToken(token2, time.Now().Add(2*time.Hour))
	got2, _ := src.getToken()
	assert.Equal(t, token2, got2)
}

// --- metadataTokenSource tests ---

func TestMetadataTokenSource_FetchAndCache(t *testing.T) {
	var fetchCount atomic.Int32
	token := makeTestJWT(time.Now().Add(1 * time.Hour))

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Google", r.Header.Get("Metadata-Flavor"))
		assert.Contains(t, r.URL.Query().Get("audience"), "https://hub.example.com")
		assert.Equal(t, "full", r.URL.Query().Get("format"))
		fetchCount.Add(1)
		_, _ = fmt.Fprint(w, token)
	}))
	defer metaSrv.Close()

	src := &metadataTokenSource{
		audience:        "https://hub.example.com",
		metadataBaseURL: metaSrv.URL,
		httpClient:      &http.Client{Timeout: 2 * time.Second},
	}

	tok1, err := src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token, tok1)

	tok2, err := src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token, tok2)

	assert.Equal(t, int32(1), fetchCount.Load(), "second call should use cache")
}

func TestMetadataTokenSource_RefreshExpired(t *testing.T) {
	var fetchCount atomic.Int32
	token1 := makeTestJWT(time.Now().Add(1 * time.Hour))
	token2 := makeTestJWT(time.Now().Add(2 * time.Hour))

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fetchCount.Add(1) == 1 {
			_, _ = fmt.Fprint(w, token1)
		} else {
			_, _ = fmt.Fprint(w, token2)
		}
	}))
	defer metaSrv.Close()

	src := &metadataTokenSource{
		audience:        "https://hub.example.com",
		metadataBaseURL: metaSrv.URL,
		httpClient:      &http.Client{Timeout: 2 * time.Second},
	}

	tok, err := src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token1, tok)

	// Simulate expiry
	src.mu.Lock()
	src.expiresAt = time.Now().Add(-1 * time.Minute)
	src.mu.Unlock()

	tok, err = src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token2, tok)
	assert.Equal(t, int32(2), fetchCount.Load())
}

func TestMetadataTokenSource_RefreshWithinMargin(t *testing.T) {
	var fetchCount atomic.Int32
	token1 := makeTestJWT(time.Now().Add(1 * time.Hour))
	token2 := makeTestJWT(time.Now().Add(2 * time.Hour))

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fetchCount.Add(1) == 1 {
			_, _ = fmt.Fprint(w, token1)
		} else {
			_, _ = fmt.Fprint(w, token2)
		}
	}))
	defer metaSrv.Close()

	src := &metadataTokenSource{
		audience:        "https://hub.example.com",
		metadataBaseURL: metaSrv.URL,
		httpClient:      &http.Client{Timeout: 2 * time.Second},
	}

	tok, err := src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token1, tok)

	// Set expiry within the 5-minute refresh margin
	src.mu.Lock()
	src.expiresAt = time.Now().Add(3 * time.Minute)
	src.mu.Unlock()

	tok, err = src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token2, tok, "should re-fetch when within refresh margin")
}

func TestMetadataTokenSource_SetToken(t *testing.T) {
	src := &metadataTokenSource{
		audience:        "https://hub.example.com",
		metadataBaseURL: "http://127.0.0.1:1", // unreachable
		httpClient:      &http.Client{Timeout: 100 * time.Millisecond},
	}

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	expiry := time.Now().Add(1 * time.Hour)
	src.setToken(token, expiry)

	// Should return the set token without hitting metadata server
	got, err := src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token, got)
}

// --- oidcTransport tests ---

func TestOIDCTransport_InjectsHeader(t *testing.T) {
	var receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	source := &injectedTokenSource{}
	source.setToken(token, time.Now().Add(1*time.Hour))

	transport := newOIDCTransport(http.DefaultTransport, source)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer "+token, receivedAuth)
}

func TestOIDCTransport_DoesNotOverrideExistingAuth(t *testing.T) {
	var receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	source := &injectedTokenSource{}
	source.setToken("should-not-be-used", time.Now().Add(1*time.Hour))

	transport := newOIDCTransport(http.DefaultTransport, source)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	req.Header.Set("Authorization", "Bearer existing-token")
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer existing-token", receivedAuth)
}

func TestOIDCTransport_GracefulDegradation(t *testing.T) {
	var requestReceived bool
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		assert.Empty(t, r.Header.Get("Authorization"), "no auth header when source has no token")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	// Source with no token → getToken() returns error
	source := &injectedTokenSource{}
	transport := newOIDCTransport(http.DefaultTransport, source)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.True(t, requestReceived, "request should proceed even when token unavailable")
}

func TestOIDCTransport_WithMetadataSource(t *testing.T) {
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, token)
	}))
	defer metaSrv.Close()

	var receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	source := &metadataTokenSource{
		audience:        "https://hub.example.com",
		metadataBaseURL: metaSrv.URL,
		httpClient:      &http.Client{Timeout: 2 * time.Second},
	}
	transport := newOIDCTransport(http.DefaultTransport, source)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer "+token, receivedAuth)
}

// --- configureOIDCTransport tests ---

func TestConfigureOIDCTransport_InjectedMode(t *testing.T) {
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	_ = os.Setenv(EnvTransportToken, token)
	defer func() { _ = os.Unsetenv(EnvTransportToken) }()

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource)
	_, ok := c.oidcSource.(*injectedTokenSource)
	assert.True(t, ok, "should use injectedTokenSource")
	require.NotNil(t, c.client.Transport)
	_, ok = c.client.Transport.(*oidcTransport)
	assert.True(t, ok, "transport should be oidcTransport")
}

func TestConfigureOIDCTransport_MetadataMode(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	// Ensure no injected token and no scion metadata server
	_ = os.Unsetenv(EnvTransportToken)
	_ = os.Unsetenv("SCION_METADATA_MODE")

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource)
	src, ok := c.oidcSource.(*metadataTokenSource)
	assert.True(t, ok, "should use metadataTokenSource")
	assert.Equal(t, "https://hub.example.com", src.audience)
}

func TestConfigureOIDCTransport_MetadataMode_AudienceOverride(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	_ = os.Unsetenv(EnvTransportToken)
	_ = os.Unsetenv("SCION_METADATA_MODE")
	_ = os.Setenv(EnvHubOIDCAudience, "https://custom-audience.example.com")
	defer func() { _ = os.Unsetenv(EnvHubOIDCAudience) }()

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource)
	src := c.oidcSource.(*metadataTokenSource)
	assert.Equal(t, "https://custom-audience.example.com", src.audience)
}

func TestConfigureOIDCTransport_NotOnGCP(t *testing.T) {
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	_ = os.Unsetenv(EnvTransportToken)

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

	t.Setenv(EnvTransportToken, "")
	t.Setenv("SCION_METADATA_MODE", "assign")

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	assert.Nil(t, c.oidcSource, "should not configure OIDC metadata mode when scion metadata server is active")
}

func TestConfigureOIDCTransport_InjectedPriority(t *testing.T) {
	// When both injected token and GCE are available, injected should win
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	_ = os.Setenv(EnvTransportToken, token)
	defer func() { _ = os.Unsetenv(EnvTransportToken) }()

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.configureOIDCTransport()

	require.NotNil(t, c.oidcSource)
	_, ok := c.oidcSource.(*injectedTokenSource)
	assert.True(t, ok, "injected should take priority over metadata")
}

// --- E2E: both agent + OIDC headers ---

func TestOIDC_EndToEnd_BothHeaders(t *testing.T) {
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	t.Setenv(EnvTransportToken, token)

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
	source := &injectedTokenSource{}
	c := &Client{oidcSource: source}

	newToken := makeTestJWT(time.Now().Add(1 * time.Hour))
	tokens := []RefreshTokenEntry{
		{Layer: "app", Type: "scion_access", Value: "app-token", ExpiresIn: 36000},
		{Layer: "transport", Type: "google_oidc", Value: newToken, ExpiresIn: 3600, Audience: "https://hub.example.com"},
	}

	c.applyRefreshTokens(tokens)

	got, err := source.getToken()
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
	source := &injectedTokenSource{}
	transportExpiry := time.Now().Add(50 * time.Minute) // short-lived
	source.setToken("tok", transportExpiry)

	c := &Client{oidcSource: source}

	// App token would refresh 2h before a 10h expiry (8h from now)
	appRefresh := time.Now().Add(8 * time.Hour)
	adjusted := c.adjustRefreshForTransportTokens(appRefresh)

	// Transport refresh should be ~45 min from now (50min - 5min margin)
	expectedTransportRefresh := transportExpiry.Add(-oidcRefreshMargin)
	assert.WithinDuration(t, expectedTransportRefresh, adjusted, 1*time.Second,
		"should use transport token's earlier refresh time")
}

func TestAdjustRefreshForTransportTokens_LongerTransport(t *testing.T) {
	source := &injectedTokenSource{}
	transportExpiry := time.Now().Add(10 * time.Hour) // long-lived
	source.setToken("tok", transportExpiry)

	c := &Client{oidcSource: source}

	// App token would refresh 30 min from now
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
