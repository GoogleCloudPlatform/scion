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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Runtime Broker accepts the agent credential the hub issued for a
// start or restart from the resolved env only when the hub classified it
// secret-bootstrap (setStartEnvToken). These tests pin that the hub sends
// its env classifications with every start and restart, on both broker
// transports, as it does with a create.

// startCredentialEnvKey is the resolved-env key setStartEnvToken writes
// the issued credential under.
const startCredentialEnvKey = "SCION_AUTH_TOKEN"

func TestApplyStartExtras_EnvClassifications(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	_, ok := payload["envClassifications"]
	assert.False(t, ok, "nil classifications must send nothing (the broker reads that as unavailable)")

	payload = map[string]interface{}{}
	applyStartExtras(payload, StartExtras{EnvClassifications: map[string]api.EnvKind{}})
	got, ok := payload["envClassifications"]
	require.True(t, ok, "an empty non-nil map must still be sent")
	assert.Empty(t, got)

	cls := map[string]api.EnvKind{startCredentialEnvKey: api.EnvKindSecretBootstrap, "SCION_MODEL": api.EnvKindPlain}
	payload = map[string]interface{}{}
	applyStartExtras(payload, StartExtras{EnvClassifications: cls})
	assert.Equal(t, cls, payload["envClassifications"])
}

// wireEnvClassifications decodes the envClassifications key of a broker
// request body; ok is false when the key is absent.
func wireEnvClassifications(t *testing.T, body []byte) (map[string]api.EnvKind, bool) {
	t.Helper()
	var wire struct {
		EnvClassifications *map[string]api.EnvKind `json:"envClassifications"`
	}
	require.NoError(t, json.Unmarshal(body, &wire), "body: %s", body)
	if wire.EnvClassifications == nil {
		return nil, false
	}
	return *wire.EnvClassifications, true
}

func credentialStartExtras() StartExtras {
	return StartExtras{
		RunID: "run-cred",
		EnvClassifications: map[string]api.EnvKind{
			startCredentialEnvKey: api.EnvKindSecretBootstrap,
			"SCION_AGENT_ID":      api.EnvKindPlain,
		},
	}
}

func credentialResolvedEnv() map[string]string {
	// Made-up value; never a real credential.
	return map[string]string{startCredentialEnvKey: "fake.issued.credential", "SCION_AGENT_ID": "agent-1"}
}

func TestBrokerHTTPTransport_StartAndRestartSendEnvClassifications(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			transport := newBrokerHTTPTransport(false, nil)
			extras := credentialStartExtras()
			var err error
			if op == "start" {
				_, err = transport.StartAgent(context.Background(), "broker-1", srv.URL, "agent-1", "project-id-1",
					"", "", "", "", "", "", credentialResolvedEnv(), nil, nil, nil, false, false, extras)
			} else {
				_, err = transport.RestartAgent(context.Background(), "broker-1", srv.URL, "agent-1", "project-id-1",
					credentialResolvedEnv(), extras)
			}
			require.NoError(t, err)

			got, ok := wireEnvClassifications(t, gotBody)
			require.True(t, ok, "%s body carries no envClassifications", op)
			assert.Equal(t, extras.EnvClassifications, got)
		})
	}
}

func TestControlChannelBrokerClient_StartAndRestartSendEnvClassifications(t *testing.T) {
	runOverHubTunnels(t, testControlChannelBrokerClient_StartAndRestartSendEnvClassifications)
}

func testControlChannelBrokerClient_StartAndRestartSendEnvClassifications(t *testing.T, tr hubTunnelTransport) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			tunnel := &mockControlChannelTunnel{connected: true}
			client := &ControlChannelBrokerClient{manager: tr.wrap(t, tunnel), signer: &mockBrokerSigner{}}
			extras := credentialStartExtras()
			var err error
			if op == "start" {
				_, err = client.StartAgent(context.Background(), "broker-1", "unused", "agent-1", "project-id-1",
					"", "", "", "", "", "", credentialResolvedEnv(), nil, nil, nil, false, false, extras)
			} else {
				_, err = client.RestartAgent(context.Background(), "broker-1", "unused", "agent-1", "project-id-1",
					credentialResolvedEnv(), extras)
			}
			require.NoError(t, err)
			require.NotNil(t, tunnel.lastRequest, "expected the tunneled request to be captured")

			got, ok := wireEnvClassifications(t, tunnel.lastRequest.Body)
			require.True(t, ok, "%s body carries no envClassifications", op)
			assert.Equal(t, extras.EnvClassifications, got)
		})
	}
}

// TestDispatchStartAndRestartClassifyIssuedCredential: start, resume and
// restart of an existing agent send the credential the hub issued for the
// run in the resolved env, and send the classifications with it, the
// credential classified secret-bootstrap.
func TestDispatchStartAndRestartClassifyIssuedCredential(t *testing.T) {
	type sentFn func(f *runDispatchFixture) (env map[string]string, extras StartExtras, called bool)
	sentStart := func(f *runDispatchFixture) (map[string]string, StartExtras, bool) {
		return f.client.lastResolvedEnv, f.client.lastStartExtras, f.client.startCalled
	}
	for _, tc := range []struct {
		name     string
		dispatch func(ctx context.Context, f *runDispatchFixture) error
		sent     sentFn
	}{
		{
			name: "start",
			dispatch: func(ctx context.Context, f *runDispatchFixture) error {
				return f.d.DispatchAgentStart(ctx, f.agent, "", false)
			},
			sent: sentStart,
		},
		{
			name: "resume",
			dispatch: func(ctx context.Context, f *runDispatchFixture) error {
				return f.d.DispatchAgentStart(ctx, f.agent, "", true)
			},
			sent: sentStart,
		},
		{
			name:     "restart",
			dispatch: func(ctx context.Context, f *runDispatchFixture) error { return f.d.DispatchAgentRestart(ctx, f.agent) },
			sent: func(f *runDispatchFixture) (map[string]string, StartExtras, bool) {
				return f.client.lastRestartResolvedEnv, f.client.lastRestartExtras, f.client.restartCalled
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRunDispatchFixture(t)
			require.NoError(t, tc.dispatch(ctx, f))

			env, extras, called := tc.sent(f)
			require.True(t, called)
			credential := env[startCredentialEnvKey]
			require.NotEmpty(t, credential, "no issued credential in the resolved env")
			claims, err := f.svc.ValidateAgentToken(credential)
			require.NoError(t, err)
			assert.Equal(t, extras.RunID, claims.RunID, "the credential is the one issued for this run")

			require.NotNil(t, extras.EnvClassifications, "no classifications sent")
			assert.Equal(t, api.EnvKindSecretBootstrap, extras.EnvClassifications[startCredentialEnvKey])
		})
	}
}

// TestSetStartEnvToken_NilClassifications: classifying the credential into
// a nil classifications map allocates the map, so the caller must read the
// map back after setStartEnvToken (as both dispatchers do when they set
// StartExtras.EnvClassifications) rather than keep an earlier nil copy.
func TestSetStartEnvToken_NilClassifications(t *testing.T) {
	env := map[string]string{}
	var cls map[string]api.EnvKind
	setStartEnvToken(env, &cls, "fake.issued.credential")
	require.NotNil(t, cls)
	assert.Equal(t, api.EnvKindSecretBootstrap, cls[startCredentialEnvKey])
	assert.Equal(t, "fake.issued.credential", env[startCredentialEnvKey])

	// No credential issued: nothing is written, and a nil map stays nil.
	env = map[string]string{}
	cls = nil
	setStartEnvToken(env, &cls, "")
	assert.Nil(t, cls)
	assert.Empty(t, env)
}
