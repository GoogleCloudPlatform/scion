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

func TestOIDCTokenSource_FetchAndCache(t *testing.T) {
	var fetchCount atomic.Int32
	token := makeTestJWT(time.Now().Add(1 * time.Hour))

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Google", r.Header.Get("Metadata-Flavor"))
		assert.Contains(t, r.URL.Query().Get("audience"), "https://hub.example.com")
		assert.Equal(t, "full", r.URL.Query().Get("format"))
		fetchCount.Add(1)
		fmt.Fprint(w, token)
	}))
	defer metaSrv.Close()

	src := &oidcTokenSource{
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

func TestOIDCTokenSource_RefreshExpired(t *testing.T) {
	var fetchCount atomic.Int32
	token1 := makeTestJWT(time.Now().Add(1 * time.Hour))
	token2 := makeTestJWT(time.Now().Add(2 * time.Hour))

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fetchCount.Add(1) == 1 {
			fmt.Fprint(w, token1)
		} else {
			fmt.Fprint(w, token2)
		}
	}))
	defer metaSrv.Close()

	src := &oidcTokenSource{
		audience:        "https://hub.example.com",
		metadataBaseURL: metaSrv.URL,
		httpClient:      &http.Client{Timeout: 2 * time.Second},
	}

	tok, err := src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token1, tok)

	// Simulate expiry by setting expiresAt to the past.
	src.mu.Lock()
	src.expiresAt = time.Now().Add(-1 * time.Minute)
	src.mu.Unlock()

	tok, err = src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token2, tok)
	assert.Equal(t, int32(2), fetchCount.Load())
}

func TestOIDCTokenSource_RefreshWithinMargin(t *testing.T) {
	var fetchCount atomic.Int32
	token1 := makeTestJWT(time.Now().Add(1 * time.Hour))
	token2 := makeTestJWT(time.Now().Add(2 * time.Hour))

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fetchCount.Add(1) == 1 {
			fmt.Fprint(w, token1)
		} else {
			fmt.Fprint(w, token2)
		}
	}))
	defer metaSrv.Close()

	src := &oidcTokenSource{
		audience:        "https://hub.example.com",
		metadataBaseURL: metaSrv.URL,
		httpClient:      &http.Client{Timeout: 2 * time.Second},
	}

	tok, err := src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token1, tok)

	// Set expiry to 3 minutes from now (within 5-minute margin).
	src.mu.Lock()
	src.expiresAt = time.Now().Add(3 * time.Minute)
	src.mu.Unlock()

	tok, err = src.getToken()
	require.NoError(t, err)
	assert.Equal(t, token2, tok, "should re-fetch when within refresh margin")
}

func TestOIDCTransport_InjectsHeader(t *testing.T) {
	var receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, token)
	}))
	defer metaSrv.Close()

	transport := newOIDCTransport(http.DefaultTransport, "https://hub.example.com", metaSrv.URL)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, "Bearer "+token, receivedAuth)
}

func TestOIDCTransport_DoesNotOverrideExistingAuth(t *testing.T) {
	var receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("metadata server should not be called when Authorization is already set")
	}))
	defer metaSrv.Close()

	transport := newOIDCTransport(http.DefaultTransport, "https://hub.example.com", metaSrv.URL)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	req.Header.Set("Authorization", "Bearer existing-token")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, "Bearer existing-token", receivedAuth)
}

func TestOIDCTransport_GracefulDegradation(t *testing.T) {
	var requestReceived bool
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		assert.Empty(t, r.Header.Get("Authorization"), "no auth header when metadata fails")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	// Point at an unreachable metadata server.
	transport := newOIDCTransport(http.DefaultTransport, "https://hub.example.com", "http://127.0.0.1:1")
	transport.source.httpClient.Timeout = 100 * time.Millisecond
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.True(t, requestReceived, "request should proceed even when metadata fetch fails")
}

func TestMaybeConfigureOIDC_NotOnGCP(t *testing.T) {
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.maybeConfigureOIDC()

	assert.Nil(t, c.client.Transport, "transport should not be wrapped when not on GCP")
}

func TestMaybeConfigureOIDC_OnGCP(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.maybeConfigureOIDC()

	require.NotNil(t, c.client.Transport)
	ot, ok := c.client.Transport.(*oidcTransport)
	require.True(t, ok, "transport should be oidcTransport")
	assert.Equal(t, "https://hub.example.com", ot.source.audience)
}

func TestMaybeConfigureOIDC_AudienceOverride(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	origAud := os.Getenv(EnvHubOIDCAudience)
	os.Setenv(EnvHubOIDCAudience, "https://custom-audience.example.com")
	defer os.Setenv(EnvHubOIDCAudience, origAud)

	c := &Client{
		hubURL: "https://hub.example.com",
		client: &http.Client{Timeout: DefaultTimeout},
	}

	c.maybeConfigureOIDC()

	require.NotNil(t, c.client.Transport)
	ot := c.client.Transport.(*oidcTransport)
	assert.Equal(t, "https://custom-audience.example.com", ot.source.audience)
}

func TestOIDC_EndToEnd_BothHeaders(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, token)
	}))
	defer metaSrv.Close()

	var gotAuth, gotAgentToken string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAgentToken = r.Header.Get("X-Scion-Agent-Token")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer hubSrv.Close()

	// Override GCP metadata URL by directly constructing the client with OIDC transport.
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
	c.client.Transport = newOIDCTransport(c.client.Transport, hubSrv.URL, metaSrv.URL)

	err := c.UpdateStatus(context.Background(), StatusUpdate{
		Status:  "running",
		Message: "test",
	})
	require.NoError(t, err)

	assert.Equal(t, "Bearer "+token, gotAuth, "OIDC Authorization header should be set")
	assert.Equal(t, "test-agent-token", gotAgentToken, "X-Scion-Agent-Token should still be set")
}
