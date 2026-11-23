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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSecretsFile_MapShorthand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.yaml")
	require.NoError(t, os.WriteFile(path, []byte("GITHUB_TOKEN: ghp_abc123\nAPI_KEY: sk-xyz\n"), 0o600))

	var warn bytes.Buffer
	got, err := loadSecretsFile(path, &warn)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Sorted deterministically by key.
	assert.Equal(t, hubclient.AgentSecretInput{
		Key:      "API_KEY",
		Value:    "sk-xyz",
		Encoding: "raw",
	}, got[0])
	assert.Equal(t, hubclient.AgentSecretInput{
		Key:      "GITHUB_TOKEN",
		Value:    "ghp_abc123",
		Encoding: "raw",
	}, got[1])
}

func TestLoadSecretsFile_FullForm(t *testing.T) {
	dir := t.TempDir()
	keyContent := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nabc123\n-----END OPENSSH PRIVATE KEY-----\n")
	keyFile := filepath.Join(dir, "deploy_key")
	require.NoError(t, os.WriteFile(keyFile, keyContent, 0o600))

	t.Setenv("TEST_OPENAI_SECRET", "sk-from-env-999")

	yamlBody := `secrets:
  - key: DEPLOY_KEY
    valueFile: deploy_key
    type: file
    target: ~/.ssh/id_ed25519
    description: Deploy SSH key
  - key: OPENAI_API_KEY
    fromEnv: TEST_OPENAI_SECRET
    injectionMode: always
  - key: LITERAL_VAR
    value: "hello-world"
    type: variable
    target: literal_var
`
	path := filepath.Join(dir, "secrets.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlBody), 0o600))

	got, err := loadSecretsFile(path, nil)
	require.NoError(t, err)
	require.Len(t, got, 3)

	assert.Equal(t, hubclient.AgentSecretInput{
		Key:         "DEPLOY_KEY",
		Value:       base64.StdEncoding.EncodeToString(keyContent),
		Encoding:    "base64",
		Type:        "file",
		Target:      "~/.ssh/id_ed25519",
		Description: "Deploy SSH key",
	}, got[0])
	assert.Equal(t, hubclient.AgentSecretInput{
		Key:           "OPENAI_API_KEY",
		Value:         "sk-from-env-999",
		Encoding:      "raw",
		InjectionMode: "always",
	}, got[1])
	assert.Equal(t, hubclient.AgentSecretInput{
		Key:      "LITERAL_VAR",
		Value:    "hello-world",
		Encoding: "raw",
		Type:     "variable",
		Target:   "literal_var",
	}, got[2])
}

func TestLoadSecretsFile_Errors(t *testing.T) {
	dir := t.TempDir()

	writeFile := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
		return p
	}

	t.Run("empty file", func(t *testing.T) {
		_, err := loadSecretsFile(writeFile("empty.yaml", "   \n"), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is empty")
	})

	t.Run("missing source", func(t *testing.T) {
		_, err := loadSecretsFile(writeFile("nosrc.yaml", "secrets:\n  - key: FOO\n"), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "one of value, valueFile, or fromEnv is required")
	})

	t.Run("multiple sources", func(t *testing.T) {
		_, err := loadSecretsFile(writeFile("multisrc.yaml", "secrets:\n  - key: FOO\n    value: bar\n    fromEnv: PATH\n"), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only one of value, valueFile, or fromEnv")
	})

	t.Run("unset fromEnv", func(t *testing.T) {
		_, err := loadSecretsFile(writeFile("unsetenv.yaml", "secrets:\n  - key: FOO\n    fromEnv: DEFINITELY_UNSET_SCION_TEST_VAR_XYZ\n"), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not set or empty")
	})

	t.Run("missing valueFile", func(t *testing.T) {
		_, err := loadSecretsFile(writeFile("missingvf.yaml", "secrets:\n  - key: FOO\n    valueFile: does-not-exist\n"), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read valueFile")
	})

	t.Run("exceeds 64KB", func(t *testing.T) {
		big := strings.Repeat("a", 64*1024+1)
		_, err := loadSecretsFile(writeFile("big.yaml", "BIG_KEY: "+big+"\n"), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds 64KB limit")
	})
}

func TestSecretsFileFlags_CreateAndStart(t *testing.T) {
	// --secrets-file is registered on both create and start.
	assert.NotNil(t, createCmd.Flags().Lookup(secretsFileFlagName))
	assert.NotNil(t, startCmd.Flags().Lookup(secretsFileFlagName))

	// Deliberately NO --secret flag on create or start (design §4.5).
	assert.Nil(t, createCmd.Flags().Lookup("secret"))
	assert.Nil(t, startCmd.Flags().Lookup("secret"))

	// --secrets-file IS applied when starting an existing agent, so it must not
	// be listed in configFlagsNotAppliedToExistingAgent.
	assert.NotContains(t, configFlagsNotAppliedToExistingAgent, secretsFileFlagName)
}

func TestCreateAgentViaHub_SendsSecretsFromFile(t *testing.T) {
	dir := t.TempDir()
	secPath := filepath.Join(dir, "secrets.yaml")
	require.NoError(t, os.WriteFile(secPath, []byte("MY_TOKEN: secret-val-123\n"), 0o600))

	var gotReq hubclient.CreateAgentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agents") {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.CreateAgentResponse{
				Agent: &hubclient.Agent{
					ID:    "agent-1",
					Slug:  gotReq.Name,
					Name:  gotReq.Name,
					Phase: "created",
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origSecretsFile := secretsFilePath
	origTemplate := templateName
	origOutput := outputFormat
	t.Cleanup(func() {
		secretsFilePath = origSecretsFile
		templateName = origTemplate
		outputFormat = origOutput
	})
	secretsFilePath = secPath
	templateName = ""
	outputFormat = ""

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: "proj-1",
	}
	err = createAgentViaHub(hubCtx, "sec-agent", "", nil)
	require.NoError(t, err)

	require.Len(t, gotReq.Secrets, 1)
	assert.Equal(t, "MY_TOKEN", gotReq.Secrets[0].Key)
	assert.Equal(t, "secret-val-123", gotReq.Secrets[0].Value)
	assert.Equal(t, "raw", gotReq.Secrets[0].Encoding)
}
