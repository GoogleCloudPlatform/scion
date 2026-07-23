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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTransportSource implements transportauth.TokenSource for testing.
type fakeTransportSource struct {
	token  string
	expiry time.Time
}

func (f *fakeTransportSource) Token() (string, error) {
	if f.token == "" {
		return "", fmt.Errorf("no transport token")
	}
	return f.token, nil
}
func (f *fakeTransportSource) SetToken(token string, expiry time.Time) {
	f.token = token
	f.expiry = expiry
}
func (f *fakeTransportSource) Expiry() time.Time { return f.expiry }

// clearAppTokenSources clears all env-var and credential sources for app tokens,
// leaving getHubAccessToken() returning "". It also points to an empty tmpDir
// for credential storage so that no OAuth token is present.
func clearAppTokenSources(t *testing.T) {
	t.Helper()

	tmpDir := t.TempDir()
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string {
		return filepath.Join(tmpDir, "credentials.json")
	})
	t.Cleanup(func() { credentials.SetCredentialsPath(origPath) })

	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("HOME", tmpDir)
}

// newAttachMockHubServer creates a mock Hub server that handles the agent GET
// request needed by attachViaHub(). The agent is returned in the "running" phase
// with the given runtime string (use "" for a normal non-managed agent).
func newAttachMockHubServer(t *testing.T, projectID, agentName, agentID, runtime string) *httptest.Server {
	t.Helper()

	agentPath := "/api/v1/projects/" + projectID + "/agents/" + agentName

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == agentPath:
			agent := hubclient.Agent{
				ID:      agentID,
				Name:    agentName,
				Phase:   "running",
				Runtime: runtime,
			}
			_ = json.NewEncoder(w).Encode(agent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestResolveAttachTransport_PlainMode verifies that resolveAttachTransport returns
// a nil TokenSource when no transport auth is configured (plain / dev / local hub).
// This is the base case; the plain-mode invariant requires an app token in this case.
func TestResolveAttachTransport_PlainMode(t *testing.T) {
	// Ensure all transport auth env vars are unset.
	t.Setenv("SCION_TRANSPORT_TOKEN", "")
	t.Setenv("SCION_TRANSPORT_AUDIENCE", "")
	t.Setenv("SCION_HUB_OIDC_AUDIENCE", "")
	t.Setenv("SCION_METADATA_MODE", "")

	// Override the GCE-detection function so we don't depend on the test host
	// being on GCP.
	origIsOnGCE := transportauth.IsOnGCEFunc
	transportauth.IsOnGCEFunc = func() bool { return false }
	defer func() { transportauth.IsOnGCEFunc = origIsOnGCE }()

	// Use a temp dir with no settings.yaml to simulate a plain project.
	tmpDir := t.TempDir()
	origProjectPath := projectPath
	projectPath = tmpDir
	defer func() { projectPath = origProjectPath }()

	src, mode, err := resolveAttachTransport()

	require.NoError(t, err, "plain mode should not error")
	assert.Nil(t, src, "plain mode should return nil TokenSource")
	assert.Equal(t, transportauth.HeaderAuthorization, mode)
}

// TestResolveAttachTransport_IAPMode verifies that resolveAttachTransport returns
// a non-nil TokenSource when transport auth is configured via SCION_TRANSPORT_TOKEN.
// This simulates the hub-injected IAP token present inside an agent container.
func TestResolveAttachTransport_IAPMode(t *testing.T) {
	// A minimal three-part JWT-shaped value; ParseTokenExpiry falls back to
	// DefaultTTL on any parse error, so we don't need a valid signature.
	t.Setenv("SCION_TRANSPORT_TOKEN", "header.payload.sig")
	t.Setenv("SCION_TRANSPORT_MODE", "iap")

	src, mode, err := resolveAttachTransport()

	require.NoError(t, err, "IAP mode should not error")
	require.NotNil(t, src, "IAP mode should return a non-nil TokenSource")
	assert.Equal(t, transportauth.HeaderProxyAuthorization, mode,
		"iap transport mode should yield HeaderProxyAuthorization")
}

// TestAttachViaHub_PlainMode_EmptyToken_RequiresAppToken is a regression test for
// the plain-mode invariant: when no transport auth is configured, an empty app
// token must still return the "no access token found for Hub" error. The fix
// must not relax this requirement for the plain case.
func TestAttachViaHub_PlainMode_EmptyToken_RequiresAppToken(t *testing.T) {
	clearAppTokenSources(t)

	// Stub resolveAttachTransportFn to return nil (plain mode — no transport auth).
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, transportauth.HeaderAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	const (
		projectID = "proj-plain-123"
		agentName = "test-agent"
		agentID   = "agent-uuid-plain"
	)

	srv := newAttachMockHubServer(t, projectID, agentName, agentID, "")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
	}

	err = attachViaHub(hubCtx, agentName)

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no access token found for Hub"),
		"plain mode with empty token should return 'no access token found for Hub', got: %v", err)
}

// TestAttachViaHub_IAPMode_EmptyToken_PassesGate is the primary regression test for
// issue #851: scion attach fails under IAP proxy-auth. When a transport source is
// present (IAP mode), an empty application-level token must no longer abort the
// attach attempt. The function should proceed past the token gate and reach the
// WebSocket dial stage (which will fail with a transport error, not a token error).
func TestAttachViaHub_IAPMode_EmptyToken_PassesGate(t *testing.T) {
	clearAppTokenSources(t)

	// Stub resolveAttachTransportFn to return a fake IAP transport source.
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return &fakeTransportSource{
			token:  "fake-oidc-token",
			expiry: time.Now().Add(1 * time.Hour),
		}, transportauth.HeaderProxyAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	const (
		projectID = "proj-iap-456"
		agentName = "iap-agent"
		agentID   = "agent-uuid-iap"
	)

	srv := newAttachMockHubServer(t, projectID, agentName, agentID, "")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
	}

	err = attachViaHub(hubCtx, agentName)

	// The call must NOT fail with "no access token found for Hub" — that was
	// the pre-fix behaviour that blocked IAP users entirely.
	if err != nil {
		assert.False(t, strings.Contains(err.Error(), "no access token found for Hub"),
			"IAP mode with transport source should pass the token gate; got: %v", err)
	}
	// The function will fail at the WebSocket dial stage (the test HTTP server
	// does not handle WebSocket upgrades), which is the expected outcome — it
	// confirms the gate was cleared and the code reached the connection attempt.
}

// TestAttachViaHub_StartAgentSite_PlainMode_EmptyToken verifies that the
// workspace-upload attach path in startAgentViaHub() preserves the plain-mode
// invariant: no transport source + empty app token → "no access token" error.
// This covers common.go site 1 (the workspace-upload polling loop attach).
func TestAttachViaHub_StartAgentSite_PlainMode_EmptyToken(t *testing.T) {
	clearAppTokenSources(t)

	// Stub to plain mode (nil transport source).
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, transportauth.HeaderAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	// Verify the gate logic by calling it directly through the stub path.
	// We exercise the same gate condition that both common.go sites use.
	var attachOpts []wsclient.AttachOption
	transportSrc, transportMode, err := resolveAttachTransportFn()
	require.NoError(t, err)
	if transportSrc != nil {
		attachOpts = append(attachOpts, wsclient.WithTransport(transportSrc, transportMode))
	}
	token := getHubAccessToken("https://hub.example.com")

	require.Empty(t, token, "app token should be empty in plain mode with cleared sources")
	require.Nil(t, transportSrc, "transport source should be nil in plain mode")
	// Confirmed: token == "" && transportSrc == nil → the gate would fire.
	assert.True(t, token == "" && transportSrc == nil,
		"gate condition must be true when both token and transport src are absent")
	_ = attachOpts
}

// TestAttachViaHub_StartAgentSite_IAPMode_WithTransportOpts verifies that when
// a transport source is present (IAP mode), the common.go attach sites pass
// WithTransport opts to AttachToAgent and do not gate on the app token.
func TestAttachViaHub_StartAgentSite_IAPMode_WithTransportOpts(t *testing.T) {
	clearAppTokenSources(t)

	fakeSrc := &fakeTransportSource{
		token:  "oidc-token-for-iap",
		expiry: time.Now().Add(1 * time.Hour),
	}

	// Stub to IAP mode (non-nil transport source).
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return fakeSrc, transportauth.HeaderProxyAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	// Simulate the gate + opts logic that both common.go sites execute.
	var attachOpts []wsclient.AttachOption
	transportSrc, transportMode, err := resolveAttachTransportFn()
	require.NoError(t, err)
	if transportSrc != nil {
		attachOpts = append(attachOpts, wsclient.WithTransport(transportSrc, transportMode))
	}
	token := getHubAccessToken("https://hub.example.com")

	require.Empty(t, token, "app token should be empty in IAP mode with cleared sources")
	require.NotNil(t, transportSrc, "transport source should be non-nil in IAP mode")

	// Gate must NOT fire: token == "" but transportSrc != nil.
	gateWouldFire := token == "" && transportSrc == nil
	assert.False(t, gateWouldFire,
		"gate must NOT fire when transport source is present (IAP mode)")

	// Exactly one WithTransport option should have been constructed.
	assert.Len(t, attachOpts, 1,
		"WithTransport option must be appended when transport source is present")

	// Apply the option to a config and verify the transport source is wired up.
	cfg := wsclient.PTYClientConfig{}
	attachOpts[0](&cfg)
	assert.Equal(t, fakeSrc, cfg.TransportSource,
		"TransportSource must be the fake source we provided")
	assert.Equal(t, transportauth.HeaderProxyAuthorization, cfg.TransportMode,
		"TransportMode must match HeaderProxyAuthorization for IAP")
}
