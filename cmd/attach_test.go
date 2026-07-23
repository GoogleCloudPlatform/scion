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

	// The function is expected to fail at the WebSocket dial step (the mock HTTP
	// server does not handle WebSocket upgrades) — that confirms the gate was
	// cleared and the connection attempt was reached. Use require.Error so the
	// assert.NotContains below is not silently skipped when err is nil.
	require.Error(t, err, "expected a WS dial error — gate should have been cleared in IAP mode")
	assert.NotContains(t, err.Error(), "no access token found for Hub",
		"IAP mode with transport source should pass the token gate; got: %v", err)
}

// newStartAgentMockHubServer creates a minimal mock Hub server sufficient for
// exercising the attach path of startAgentViaHub() (site 2, the ready: label).
// It handles the suspend-check GET, project GET (git-remote display), agent
// CREATE POST, and the polling GET — all of which are reached before the token
// gate in the non-workspace-upload code path.
func newStartAgentMockHubServer(t *testing.T, projectID, agentName, agentID string) *httptest.Server {
	t.Helper()
	agentPath := "/api/v1/projects/" + projectID + "/agents/" + agentName
	agentsPath := "/api/v1/projects/" + projectID + "/agents"
	projectGetPath := "/api/v1/projects/" + projectID

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodGet && r.URL.Path == projectGetPath:
			// Git-remote display: return a project with no GitRemote to suppress output.
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "test"})

		case r.Method == http.MethodGet && r.URL.Path == agentPath:
			// Suspend check (pre-create) and polling (post-create): return running.
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID:    agentID,
				Name:  agentName,
				Phase: "running",
			})

		case r.Method == http.MethodPost && r.URL.Path == agentsPath:
			// Create: return a minimal response with no UploadURLs and no EnvGather
			// so the workspace-upload branch (site 1) is skipped.
			_ = json.NewEncoder(w).Encode(hubclient.CreateAgentResponse{
				Agent: &hubclient.Agent{
					ID:   agentID,
					Name: agentName,
					// Created is zero-value → hubsync watermark update is skipped.
				},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// saveAttachTestState saves the package-level variables that startAgentViaHub
// reads, and returns a function that restores them.
func saveAttachTestState() func() {
	origAttach := attach
	origTemplate := templateName
	origBranch := branch
	origWorkspace := workspace
	origBroker := runtimeBrokerID
	origHConfig := harnessConfigFlag
	origHAuth := harnessAuthFlag
	origNoNotify := startNoNotify
	origLabels := labelFlags
	return func() {
		attach = origAttach
		templateName = origTemplate
		branch = origBranch
		workspace = origWorkspace
		runtimeBrokerID = origBroker
		harnessConfigFlag = origHConfig
		harnessAuthFlag = origHAuth
		startNoNotify = origNoNotify
		labelFlags = origLabels
	}
}

// TestStartAgentViaHub_Site2_PlainMode_EmptyToken_RequiresAppToken directly
// calls startAgentViaHub() with attach=true and exercises site 2 (the ready:
// label in the main polling path). In plain mode (nil transport source) with an
// empty app token the function must return "no access token found for Hub",
// confirming the invariant is preserved in the real function path.
//
// Site 1 (workspace-upload path, ~common.go:1013) is not directly exercised
// here because it requires Hub-supplied UploadURLs and a Workspace.FinalizeSyncTo
// response — complexity that exceeds the scope of a unit test. The gate logic at
// site 1 is structurally identical to site 2 and was verified by code review.
func TestStartAgentViaHub_Site2_PlainMode_EmptyToken_RequiresAppToken(t *testing.T) {
	clearAppTokenSources(t)

	restore := saveAttachTestState()
	defer restore()
	attach = true
	templateName = ""
	labelFlags = nil
	runtimeBrokerID = ""
	harnessConfigFlag = ""
	harnessAuthFlag = ""

	// Stub resolveAttachTransportFn to return nil (plain mode — no transport auth).
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, transportauth.HeaderAuthorization, nil
	}
	defer func() { resolveAttachTransportFn = orig }()

	const (
		projectID = "proj-start-plain-789"
		agentName = "start-plain-agent"
		agentID   = "start-plain-uuid"
	)

	srv := newStartAgentMockHubServer(t, projectID, agentName, agentID)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
		// ProjectPath is empty → workspace scan and hubsync calls are skipped.
	}

	err = startAgentViaHub(hubCtx, agentName, "", false, nil)

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no access token found for Hub"),
		"plain mode with empty token should reach 'no access token found for Hub' in startAgentViaHub site 2; got: %v", err)
}

// TestStartAgentViaHub_Site2_IAPMode_EmptyToken_PassesGate directly calls
// startAgentViaHub() with attach=true and exercises site 2 (the ready: label in
// the main polling path). With a transport source present (IAP mode) and an empty
// app token the function must NOT return "no access token found for Hub" — it
// should clear the gate and fail at the WebSocket dial, confirming that issue #851
// is fixed in the startAgentViaHub() path too.
//
// Site 1 (workspace-upload path) is not directly exercised here — see the comment
// on TestStartAgentViaHub_Site2_PlainMode_EmptyToken_RequiresAppToken for why.
func TestStartAgentViaHub_Site2_IAPMode_EmptyToken_PassesGate(t *testing.T) {
	clearAppTokenSources(t)

	restore := saveAttachTestState()
	defer restore()
	attach = true
	templateName = ""
	labelFlags = nil
	runtimeBrokerID = ""
	harnessConfigFlag = ""
	harnessAuthFlag = ""

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
		projectID = "proj-start-iap-abc"
		agentName = "start-iap-agent"
		agentID   = "start-iap-uuid"
	)

	srv := newStartAgentMockHubServer(t, projectID, agentName, agentID)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: projectID,
		// ProjectPath is empty → workspace scan and hubsync calls are skipped.
	}

	err = startAgentViaHub(hubCtx, agentName, "", false, nil)

	// The function is expected to fail at the WebSocket dial step (the mock HTTP
	// server does not handle WebSocket upgrades) — that confirms the gate was
	// cleared and the connection attempt was reached. Use require.Error so the
	// assert.NotContains below is not silently skipped when err is nil.
	require.Error(t, err, "expected a WS dial error — gate should have been cleared in IAP mode")
	assert.NotContains(t, err.Error(), "no access token found for Hub",
		"IAP mode with transport source should pass the token gate in startAgentViaHub site 2; got: %v", err)
}
