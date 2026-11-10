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
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hub-scope secrets seeded by newHubSecretReadFixture, by name.
const (
	hsrEnvAlways   = "HSR_ENV_ALWAYS"
	hsrEnvAsNeeded = "HSR_ENV_AS_NEEDED"
	hsrVariable    = "HSR_VARIABLE"
	hsrFile        = "HSR_FILE"
	hsrInternal    = "HSR_INTERNAL"
	hsrTelemetry   = "scion-telemetry-gcp-credentials"
)

// hsrHidden lists the seeded hub-scope secrets an agent must never see.
var hsrHidden = []string{hsrEnvAsNeeded, hsrVariable, hsrFile, hsrInternal, hsrTelemetry}

type hubSecretReadFixture struct {
	srv        *Server
	agentToken string
}

func newHubSecretReadFixture(t *testing.T) *hubSecretReadFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	for _, sec := range []store.Secret{
		{Key: hsrEnvAlways, SecretType: store.SecretTypeEnvironment, InjectionMode: store.InjectionModeAlways},
		{Key: hsrEnvAsNeeded, SecretType: store.SecretTypeEnvironment, InjectionMode: store.InjectionModeAsNeeded},
		{Key: hsrVariable, SecretType: store.SecretTypeVariable, InjectionMode: store.InjectionModeAlways},
		{Key: hsrFile, SecretType: store.SecretTypeFile, Target: "/etc/hsr/file", InjectionMode: store.InjectionModeAlways},
		{Key: hsrInternal, SecretType: store.SecretTypeInternal, InjectionMode: store.InjectionModeAlways},
		{Key: hsrTelemetry, SecretType: store.SecretTypeFile, Target: "~/.scion/telemetry-gcp-credentials.json", InjectionMode: store.InjectionModeAlways},
	} {
		sec.ID = tid("hsr-" + sec.Key)
		sec.EncryptedValue = "encrypted-" + sec.Key
		if sec.Target == "" {
			sec.Target = sec.Key
		}
		sec.Scope = store.ScopeHub
		sec.ScopeID = "test-hub-id"
		require.NoError(t, s.CreateSecret(ctx, &sec))
	}

	project := &store.Project{
		ID: tid("project_hsr"), Name: "HSR Project", Slug: "hsr-project",
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("agent_hsr"), Slug: "hsr-agent", Name: "HSR Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	token, err := srv.agentTokenService.GenerateAgentToken(agent.ID, project.ID, nil, nil)
	require.NoError(t, err)
	return &hubSecretReadFixture{srv: srv, agentToken: token}
}

func (f *hubSecretReadFixture) agentGet(t *testing.T, path string) (int, string) {
	t.Helper()
	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, path, nil, f.agentToken)
	return rec.Code, rec.Body.String()
}

func (f *hubSecretReadFixture) adminGet(t *testing.T, path string) (int, string) {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodGet, path, nil)
	return rec.Code, rec.Body.String()
}

func listedSecretNames(t *testing.T, body string) []string {
	t.Helper()
	var resp ListSecretsResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp), body)
	names := make([]string, 0, len(resp.Secrets))
	for _, s := range resp.Secrets {
		names = append(names, s.Key)
	}
	sort.Strings(names)
	return names
}

func listedEnvKeys(t *testing.T, body string) []string {
	t.Helper()
	var resp ListEnvVarsResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp), body)
	keys := make([]string, 0, len(resp.EnvVars))
	for _, v := range resp.EnvVars {
		keys = append(keys, v.Key)
	}
	sort.Strings(keys)
	return keys
}

// TestHubSecretAgentRead_ListAndGet: an agent token sees only hub-scope
// secrets delivered to agents as always-mode environment secrets. Every
// other hub-scope secret, including internal ones and the telemetry
// credential file, is absent from the secret and env lists, and a get
// answers exactly as for a key that does not exist.
func TestHubSecretAgentRead_ListAndGet(t *testing.T) {
	f := newHubSecretReadFixture(t)

	t.Run("secret list", func(t *testing.T) {
		code, body := f.agentGet(t, "/api/v1/secrets?scope=hub")
		require.Equal(t, http.StatusOK, code, body)
		assert.Equal(t, []string{hsrEnvAlways}, listedSecretNames(t, body))
		for _, name := range hsrHidden {
			assert.NotContains(t, body, name)
		}
	})

	t.Run("secret get", func(t *testing.T) {
		code, body := f.agentGet(t, "/api/v1/secrets/"+hsrEnvAlways+"?scope=hub")
		require.Equal(t, http.StatusOK, code, body)
		missingCode, missingBody := f.agentGet(t, "/api/v1/secrets/HSR_NO_SUCH_KEY?scope=hub")
		require.Equal(t, http.StatusNotFound, missingCode, missingBody)
		for _, name := range hsrHidden {
			code, body := f.agentGet(t, "/api/v1/secrets/"+name+"?scope=hub")
			assert.Equal(t, missingCode, code, name)
			assert.Equal(t, missingBody, body, name)
		}
	})

	t.Run("env list", func(t *testing.T) {
		code, body := f.agentGet(t, "/api/v1/env?scope=hub")
		require.Equal(t, http.StatusOK, code, body)
		assert.Equal(t, []string{hsrEnvAlways}, listedEnvKeys(t, body))
		for _, name := range hsrHidden {
			assert.NotContains(t, body, name)
		}
	})

	t.Run("env get", func(t *testing.T) {
		code, body := f.agentGet(t, "/api/v1/env/"+hsrEnvAlways+"?scope=hub")
		require.Equal(t, http.StatusOK, code, body)
		missingCode, missingBody := f.agentGet(t, "/api/v1/env/HSR_NO_SUCH_KEY?scope=hub")
		require.Equal(t, http.StatusNotFound, missingCode, missingBody)
		code, body = f.agentGet(t, "/api/v1/env/"+hsrEnvAsNeeded+"?scope=hub")
		assert.Equal(t, missingCode, code)
		assert.Equal(t, missingBody, body)
	})
}

// TestHubSecretAgentRead_AdminUnchanged: an admin session still lists and
// gets every hub-scope secret.
func TestHubSecretAgentRead_AdminUnchanged(t *testing.T) {
	f := newHubSecretReadFixture(t)
	all := append([]string{hsrEnvAlways}, hsrHidden...)
	sort.Strings(all)

	code, body := f.adminGet(t, "/api/v1/secrets?scope=hub")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, all, listedSecretNames(t, body))
	for _, name := range all {
		code, body := f.adminGet(t, "/api/v1/secrets/"+name+"?scope=hub")
		assert.Equal(t, http.StatusOK, code, "%s: %s", name, body)
	}

	code, body = f.adminGet(t, "/api/v1/env?scope=hub")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{hsrEnvAlways, hsrEnvAsNeeded}, listedEnvKeys(t, body))
	code, body = f.adminGet(t, "/api/v1/env/"+hsrEnvAsNeeded+"?scope=hub")
	assert.Equal(t, http.StatusOK, code, body)
}
