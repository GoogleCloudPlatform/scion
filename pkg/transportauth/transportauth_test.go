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

package transportauth

import (
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
	orig := IsOnGCEFunc
	IsOnGCEFunc = func() bool { return val }
	return func() { IsOnGCEFunc = orig }
}

// --- ParseTokenExpiry tests ---

func TestParseTokenExpiry_Valid(t *testing.T) {
	token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZ2VudC0xMjMiLCJleHAiOjE4OTM0NTYwMDB9.invalid-sig"
	expiry, err := ParseTokenExpiry(token)
	require.NoError(t, err)
	assert.Equal(t, time.Unix(1893456000, 0), expiry)
}

func TestParseTokenExpiry_InvalidFormat(t *testing.T) {
	_, err := ParseTokenExpiry("not-a-jwt")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid JWT format")
}

func TestParseTokenExpiry_Empty(t *testing.T) {
	_, err := ParseTokenExpiry("")
	assert.Error(t, err)
}

func TestParseTokenExpiry_NoExpClaim(t *testing.T) {
	token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZ2VudC0xMjMifQ.invalid-sig"
	_, err := ParseTokenExpiry(token)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no expiry claim")
}

// --- InjectedSource tests ---

func TestInjectedSource_SetAndGet(t *testing.T) {
	src := NewInjectedSource()
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	expiry := time.Now().Add(1 * time.Hour)

	src.SetToken(token, expiry)

	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token, got)
}

func TestInjectedSource_Empty(t *testing.T) {
	src := NewInjectedSource()
	_, err := src.Token()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no transport token")
}

func TestInjectedSource_UpdateToken(t *testing.T) {
	src := NewInjectedSource()
	token1 := makeTestJWT(time.Now().Add(1 * time.Hour))
	token2 := makeTestJWT(time.Now().Add(2 * time.Hour))

	src.SetToken(token1, time.Now().Add(1*time.Hour))
	got1, _ := src.Token()
	assert.Equal(t, token1, got1)

	src.SetToken(token2, time.Now().Add(2*time.Hour))
	got2, _ := src.Token()
	assert.Equal(t, token2, got2)
}

func TestInjectedSource_Expiry(t *testing.T) {
	src := NewInjectedSource()
	assert.True(t, src.Expiry().IsZero())

	expiry := time.Now().Add(1 * time.Hour)
	src.SetToken("tok", expiry)
	assert.WithinDuration(t, expiry, src.Expiry(), time.Second)
}

// --- MetadataSource tests ---

func TestMetadataSource_FetchAndCache(t *testing.T) {
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

	src := NewMetadataSourceWithURL("https://hub.example.com", metaSrv.URL)

	tok1, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token, tok1)

	tok2, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token, tok2)

	assert.Equal(t, int32(1), fetchCount.Load(), "second call should use cache")
}

func TestMetadataSource_RefreshExpired(t *testing.T) {
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

	src := NewMetadataSourceWithURL("https://hub.example.com", metaSrv.URL)

	tok, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token1, tok)

	// Simulate expiry
	src.mu.Lock()
	src.expiresAt = time.Now().Add(-1 * time.Minute)
	src.mu.Unlock()

	tok, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, token2, tok)
	assert.Equal(t, int32(2), fetchCount.Load())
}

func TestMetadataSource_RefreshWithinMargin(t *testing.T) {
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

	src := NewMetadataSourceWithURL("https://hub.example.com", metaSrv.URL)

	tok, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token1, tok)

	// Set expiry within the 5-minute refresh margin
	src.mu.Lock()
	src.expiresAt = time.Now().Add(3 * time.Minute)
	src.mu.Unlock()

	tok, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, token2, tok, "should re-fetch when within refresh margin")
}

func TestMetadataSource_SetToken(t *testing.T) {
	src := NewMetadataSourceWithURL("https://hub.example.com", "http://127.0.0.1:1")

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	expiry := time.Now().Add(1 * time.Hour)
	src.SetToken(token, expiry)

	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token, got)
}

func TestMetadataSource_Expiry(t *testing.T) {
	src := NewMetadataSourceWithURL("https://hub.example.com", "http://127.0.0.1:1")
	assert.True(t, src.Expiry().IsZero())

	expiry := time.Now().Add(1 * time.Hour)
	src.SetToken("tok", expiry)
	assert.WithinDuration(t, expiry, src.Expiry(), time.Second)
}

// --- RoundTripper (Wrap) tests ---

func TestWrap_InjectsHeader(t *testing.T) {
	var receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	source := NewInjectedSource()
	source.SetToken(token, time.Now().Add(1*time.Hour))

	transport := Wrap(http.DefaultTransport, source, HeaderAuthorization)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer "+token, receivedAuth)
}

func TestWrap_DoesNotOverrideExistingAuth(t *testing.T) {
	var receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	source := NewInjectedSource()
	source.SetToken("should-not-be-used", time.Now().Add(1*time.Hour))

	transport := Wrap(http.DefaultTransport, source, HeaderAuthorization)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	req.Header.Set("Authorization", "Bearer existing-token")
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer existing-token", receivedAuth)
}

func TestWrap_GracefulDegradation(t *testing.T) {
	var requestReceived bool
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		assert.Empty(t, r.Header.Get("Authorization"), "no auth header when source has no token")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	source := NewInjectedSource() // empty — Token() returns error
	transport := Wrap(http.DefaultTransport, source, HeaderAuthorization)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.True(t, requestReceived, "request should proceed even when token unavailable")
}

func TestWrap_WithMetadataSource(t *testing.T) {
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

	source := NewMetadataSourceWithURL("https://hub.example.com", metaSrv.URL)
	transport := Wrap(http.DefaultTransport, source, HeaderAuthorization)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer "+token, receivedAuth)
}

func TestWrap_NilBase(t *testing.T) {
	source := NewInjectedSource()
	source.SetToken("tok", time.Now().Add(1*time.Hour))

	rt := Wrap(nil, source, HeaderAuthorization)
	require.NotNil(t, rt, "Wrap(nil, ...) should use http.DefaultTransport")
}

// --- HeaderMode tests ---

func TestHeaderMode_ProxyAuthorization(t *testing.T) {
	var receivedProxy, receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedProxy = r.Header.Get("Proxy-Authorization")
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	source := NewInjectedSource()
	source.SetToken(token, time.Now().Add(1*time.Hour))

	transport := Wrap(http.DefaultTransport, source, HeaderProxyAuthorization)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer "+token, receivedProxy, "should set Proxy-Authorization")
	assert.Empty(t, receivedAuth, "should not set Authorization in proxy mode")
}

func TestHeaderMode_ServerlessAuthorization(t *testing.T) {
	var receivedServerless, receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedServerless = r.Header.Get("X-Serverless-Authorization")
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	source := NewInjectedSource()
	source.SetToken(token, time.Now().Add(1*time.Hour))

	transport := Wrap(http.DefaultTransport, source, HeaderServerlessAuthorization)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer "+token, receivedServerless, "should set X-Serverless-Authorization")
	assert.Empty(t, receivedAuth, "should not set Authorization in serverless mode")
}

func TestHeaderMode_ProxyDoesNotSkipExistingAuth(t *testing.T) {
	var receivedProxy, receivedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedProxy = r.Header.Get("Proxy-Authorization")
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	source := NewInjectedSource()
	source.SetToken(token, time.Now().Add(1*time.Hour))

	transport := Wrap(http.DefaultTransport, source, HeaderProxyAuthorization)
	client := &http.Client{Transport: transport}

	req, _ := http.NewRequest("GET", hubSrv.URL+"/test", nil)
	req.Header.Set("Authorization", "Bearer user-token")
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer "+token, receivedProxy, "Proxy-Authorization set regardless of existing Auth")
	assert.Equal(t, "Bearer user-token", receivedAuth, "existing Authorization preserved")
}

// --- ApplyHeaders tests ---

func TestApplyHeaders_Authorization(t *testing.T) {
	source := NewInjectedSource()
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	source.SetToken(token, time.Now().Add(1*time.Hour))

	h := http.Header{}
	err := ApplyHeaders(h, source, HeaderAuthorization)
	require.NoError(t, err)
	assert.Equal(t, "Bearer "+token, h.Get("Authorization"))
}

func TestApplyHeaders_ProxyAuthorization(t *testing.T) {
	source := NewInjectedSource()
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	source.SetToken(token, time.Now().Add(1*time.Hour))

	h := http.Header{}
	err := ApplyHeaders(h, source, HeaderProxyAuthorization)
	require.NoError(t, err)
	assert.Equal(t, "Bearer "+token, h.Get("Proxy-Authorization"))
	assert.Empty(t, h.Get("Authorization"))
}

func TestApplyHeaders_Error(t *testing.T) {
	source := NewInjectedSource() // empty
	h := http.Header{}
	err := ApplyHeaders(h, source, HeaderAuthorization)
	assert.Error(t, err)
}

func TestApplyHeaders_AuthorizationSkipsExisting(t *testing.T) {
	source := NewInjectedSource()
	source.SetToken("new-tok", time.Now().Add(1*time.Hour))

	h := http.Header{}
	h.Set("Authorization", "Bearer existing")
	err := ApplyHeaders(h, source, HeaderAuthorization)
	require.NoError(t, err)
	assert.Equal(t, "Bearer existing", h.Get("Authorization"), "should not override existing")
}

// --- ModeFromEnv tests ---

func TestModeFromEnv_Unset(t *testing.T) {
	_ = os.Unsetenv(EnvTransportMode)
	assert.Equal(t, HeaderAuthorization, ModeFromEnv())
}

func TestModeFromEnv_IAP(t *testing.T) {
	t.Setenv(EnvTransportMode, "iap")
	assert.Equal(t, HeaderProxyAuthorization, ModeFromEnv())
}

func TestModeFromEnv_CloudRunInvoker(t *testing.T) {
	t.Setenv(EnvTransportMode, "cloudrun_invoker")
	assert.Equal(t, HeaderServerlessAuthorization, ModeFromEnv())
}

func TestModeFromEnv_Unknown(t *testing.T) {
	t.Setenv(EnvTransportMode, "something_else")
	assert.Equal(t, HeaderAuthorization, ModeFromEnv())
}

// --- FromEnv tests ---

func TestFromEnv_InjectedToken(t *testing.T) {
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	t.Setenv(EnvTransportToken, token)

	src, err := FromEnv()
	require.NoError(t, err)
	require.NotNil(t, src)

	_, ok := src.(*InjectedSource)
	assert.True(t, ok, "should return InjectedSource")

	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token, got)
}

func TestFromEnv_MetadataMode(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(EnvTransportToken, "")
	_ = os.Unsetenv(EnvTransportToken)
	_ = os.Unsetenv(EnvMetadataMode)
	t.Setenv(EnvTransportAudience, "https://custom-audience.example.com")

	src, err := FromEnv()
	require.NoError(t, err)
	require.NotNil(t, src)
	_, ok := src.(*MetadataSource)
	assert.True(t, ok, "should return MetadataSource")
}

func TestFromEnv_MetadataMode_HubOIDCAudienceFallback(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	_ = os.Unsetenv(EnvTransportToken)
	_ = os.Unsetenv(EnvMetadataMode)
	_ = os.Unsetenv(EnvTransportAudience)
	t.Setenv(EnvHubOIDCAudience, "https://fallback-audience.example.com")

	src, err := FromEnv()
	require.NoError(t, err)
	require.NotNil(t, src, "should use SCION_HUB_OIDC_AUDIENCE as fallback")
}

func TestFromEnv_MetadataMode_NoAudience(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	_ = os.Unsetenv(EnvTransportToken)
	_ = os.Unsetenv(EnvMetadataMode)
	_ = os.Unsetenv(EnvTransportAudience)
	_ = os.Unsetenv(EnvHubOIDCAudience)

	src, err := FromEnv()
	require.NoError(t, err)
	assert.Nil(t, src, "metadata mode requires explicit audience in FromEnv()")
}

func TestFromEnv_NotOnGCP(t *testing.T) {
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	_ = os.Unsetenv(EnvTransportToken)

	src, err := FromEnv()
	require.NoError(t, err)
	assert.Nil(t, src, "should return nil when not on GCP and no injected token")
}

func TestFromEnv_MetadataBlocked(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	_ = os.Unsetenv(EnvTransportToken)
	t.Setenv(EnvMetadataMode, "assign")
	t.Setenv(EnvTransportAudience, "https://audience.example.com")

	src, err := FromEnv()
	require.NoError(t, err)
	assert.Nil(t, src, "should return nil when SCION_METADATA_MODE is set")
}

func TestFromEnv_InjectedPriority(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	t.Setenv(EnvTransportToken, token)
	t.Setenv(EnvTransportAudience, "https://audience.example.com")

	src, err := FromEnv()
	require.NoError(t, err)
	require.NotNil(t, src)
	_, ok := src.(*InjectedSource)
	assert.True(t, ok, "injected should take priority over metadata")
}

func TestFromEnv_NothingConfigured(t *testing.T) {
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	_ = os.Unsetenv(EnvTransportToken)
	_ = os.Unsetenv(EnvTransportAudience)
	_ = os.Unsetenv(EnvHubOIDCAudience)
	_ = os.Unsetenv(EnvMetadataMode)

	src, err := FromEnv()
	require.NoError(t, err)
	assert.Nil(t, src)
}
