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
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headerRecorder answers every request with a healthy /healthz body and
// keeps the last request's credential headers.
type headerRecorder struct {
	bearer     string
	agentToken string
}

func (h *headerRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	h.bearer = req.Header.Get("Authorization")
	h.agentToken = req.Header.Get("X-Scion-Agent-Token")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"status":"healthy"}`)),
		Request:    req,
	}, nil
}

// TestHubClientAuthSource pins the Hub credential priority order (stored
// login, agent token file, SCION_AUTH_TOKEN, SCION_HUB_TOKEN, development
// token) with its localhost exception, and the source reported for each
// case. Reporting the source must not change which credential is sent.
func TestHubClientAuthSource(t *testing.T) {
	const remote = "https://hub.example.test"
	const local = "http://localhost:8080"

	type setup struct {
		storedLogin, tokenFile, envAuth, envHub, dev, agentID bool
	}
	all := setup{storedLogin: true, tokenFile: true, envAuth: true, envHub: true, dev: true}
	tests := []struct {
		name       string
		endpoint   string
		setup      setup
		wantSource string
		wantBearer string
		wantAgent  string
	}{
		{"stored login wins over everything", remote, all, hubAuthSourceStoredLogin, "Bearer login-tok", ""},
		{"agent token file next", remote, setup{tokenFile: true, envAuth: true, envHub: true, dev: true}, hubAuthSourceAgentTokenFile, "", "file-tok"},
		{"agent token file wins over SCION_HUB_TOKEN", remote, setup{tokenFile: true, envHub: true}, hubAuthSourceAgentTokenFile, "", "file-tok"},
		{"SCION_AUTH_TOKEN next", remote, setup{envAuth: true, envHub: true, dev: true}, hubAuthSourceEnvAuthToken, "", "env-auth-tok"},
		{"SCION_HUB_TOKEN next", remote, setup{envHub: true, dev: true}, hubAuthSourceEnvHubToken, "Bearer env-hub-tok", ""},
		{"development token last", remote, setup{dev: true}, hubAuthSourceDev, "Bearer dev-tok", ""},
		{"no credential", remote, setup{}, hubAuthSourceNone, "", ""},
		{"localhost prefers the development token over the agent token file", local, setup{tokenFile: true, dev: true}, hubAuthSourceDev, "Bearer dev-tok", ""},
		{"localhost without a development token keeps the agent token file", local, setup{tokenFile: true}, hubAuthSourceAgentTokenFile, "", "file-tok"},
		{"localhost hub-managed agent keeps the agent token file", local, setup{tokenFile: true, dev: true, agentID: true}, hubAuthSourceAgentTokenFile, "", "file-tok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			for _, v := range []string{
				"SCION_AUTH_TOKEN", "SCION_HUB_TOKEN", "SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE",
				"SCION_AGENT_ID", "SCION_LAUNCH_ID", hubprofile.EnvHubProfile,
				"SCION_HUB_ENDPOINT", "SCION_HUB_URL",
				// Transport auth would wrap the client and add its own credential.
				transportauth.EnvTransportMode, transportauth.EnvTransportToken, transportauth.EnvTransportTokenFile,
				transportauth.EnvTransportAudience, transportauth.EnvHubOIDCAudience, transportauth.EnvMetadataMode,
			} {
				t.Setenv(v, "")
			}
			if tt.setup.storedLogin {
				require.NoError(t, credentials.Store(tt.endpoint, &credentials.TokenResponse{AccessToken: "login-tok", ExpiresIn: time.Hour}))
			}
			if tt.setup.tokenFile {
				require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte("file-tok\n"), 0o600))
			}
			if tt.setup.envAuth {
				t.Setenv("SCION_AUTH_TOKEN", "env-auth-tok")
			}
			if tt.setup.envHub {
				t.Setenv("SCION_HUB_TOKEN", "env-hub-tok")
			}
			if tt.setup.dev {
				t.Setenv("SCION_DEV_TOKEN", "dev-tok")
			}
			if tt.setup.agentID {
				t.Setenv("SCION_AGENT_ID", "agent-1")
			}

			rec := &headerRecorder{}
			orig := http.DefaultTransport
			http.DefaultTransport = rec
			t.Cleanup(func() { http.DefaultTransport = orig })

			c, source, err := hubClientAndAuthSourceForEndpoint(nil, tt.endpoint)
			require.NoError(t, err)
			assert.Equal(t, tt.wantSource, source)
			_, err = c.Health(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.wantBearer, rec.bearer, "Authorization header")
			assert.Equal(t, tt.wantAgent, rec.agentToken, "agent token header")

			// hubClientForEndpoint sends the same credential.
			rec.bearer, rec.agentToken = "", ""
			c2, err := hubClientForEndpoint(nil, tt.endpoint)
			require.NoError(t, err)
			_, err = c2.Health(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.wantBearer, rec.bearer, "Authorization header (hubClientForEndpoint)")
			assert.Equal(t, tt.wantAgent, rec.agentToken, "agent token header (hubClientForEndpoint)")

			// Through the invocation's hub (SCION_HUB_ENDPOINT, no profile):
			// getHubClientWithAuthSource reports the same source, and
			// getHubClient, its wrapper, sends the same credential.
			origEP, origSrc, origProfile := hubEndpoint, hubSource, hubProfileFlag
			hubEndpoint, hubProfileFlag = "", ""
			t.Cleanup(func() { hubEndpoint, hubSource, hubProfileFlag = origEP, origSrc, origProfile })
			t.Setenv("SCION_HUB_ENDPOINT", tt.endpoint)
			rec.bearer, rec.agentToken = "", ""
			c3, source3, err := getHubClientWithAuthSource(nil)
			require.NoError(t, err)
			assert.Equal(t, tt.wantSource, source3, "getHubClientWithAuthSource")
			_, err = c3.Health(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.wantBearer, rec.bearer, "Authorization header (getHubClientWithAuthSource)")
			assert.Equal(t, tt.wantAgent, rec.agentToken, "agent token header (getHubClientWithAuthSource)")
			rec.bearer, rec.agentToken = "", ""
			c4, err := getHubClient(nil)
			require.NoError(t, err)
			_, err = c4.Health(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.wantBearer, rec.bearer, "Authorization header (getHubClient)")
			assert.Equal(t, tt.wantAgent, rec.agentToken, "agent token header (getHubClient)")
		})
	}
}

func TestDoctorHubCredentialCheck_NamesOnlyTheSource(t *testing.T) {
	for _, src := range []string{hubAuthSourceHubProfile, hubAuthSourceStoredLogin, hubAuthSourceAgentTokenFile, hubAuthSourceEnvAuthToken, hubAuthSourceEnvHubToken, hubAuthSourceDev, hubAuthSourceNone} {
		c := doctorHubCredentialCheck(src)
		assert.Equal(t, "hub-credential", c.Name, src)
		assert.Equal(t, "info", c.Status, src)
		assert.NotContains(t, c.Message, "Credential source:", "%s has its own message", src)
		assert.NotContains(t, c.Message, "/", "%s message names no path", src)
	}
	assert.Contains(t, doctorHubCredentialCheck(hubAuthSourceAgentTokenFile).Message, "precedence over SCION_HUB_TOKEN")
}
