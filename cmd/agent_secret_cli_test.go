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
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	sciontoolhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSecretScope_AgentFlag(t *testing.T) {
	state := saveSecretTestState()
	origAgent := secretAgentScope
	t.Cleanup(func() {
		state.restore()
		secretAgentScope = origAgent
	})

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&secretScope, "scope", "", "")
	cmd.Flags().StringVar(&secretProjectScope, "project", "", "")
	cmd.Flags().StringVar(&secretBrokerScope, "broker", "", "")
	cmd.Flags().StringVar(&secretAgentScope, "agent", "", "")

	require.NoError(t, cmd.Flags().Set("agent", "my-agent"))
	target, err := resolveSecretScope(cmd, &config.Settings{})
	require.NoError(t, err)
	assert.Equal(t, "agent", target.scope)
	assert.Equal(t, "my-agent", target.id)

	// Mutual exclusivity: --agent + --project fails.
	require.NoError(t, cmd.Flags().Set("project", "my-proj"))
	_, err = resolveSecretScope(cmd, &config.Settings{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot specify more than one")
}

func TestHubSecretClearCmd_HasDeleteAlias(t *testing.T) {
	assert.Contains(t, hubSecretClearCmd.Aliases, "delete")
}

func TestAgentSecretGetValue_WithScopeFlag(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{
			"key":    "API_KEY",
			"value":  base64.StdEncoding.EncodeToString([]byte("scoped-secret-val")),
			"type":   "environment",
			"target": "API_KEY",
			"scope":  "agent",
		})
	}))
	t.Cleanup(srv.Close)

	useSecretGetHub(t, srv)
	origScope := agentSecretGetScope
	t.Cleanup(func() { agentSecretGetScope = origScope })

	agentSecretGetValue = true
	agentSecretGetScope = "agent"

	out, err := runSecretGetTest(t, "API_KEY")
	require.NoError(t, err)
	assert.Equal(t, "scoped-secret-val", out)
	assert.Equal(t, "scope=agent", gotQuery)
}

func TestAgentSecretList_InsideAgentShowsScopeColumn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agents/"+secretGetTestAgentID+"/secrets", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"secrets": []map[string]string{
				{"key": "AGENT_KEY", "type": "environment", "target": "AGENT_KEY", "scope": "agent"},
				{"key": "PROJ_KEY", "type": "file", "target": "~/.ssh/id", "scope": "project"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	useSecretGetHub(t, srv)
	origListScope := agentSecretListScope
	t.Cleanup(func() { agentSecretListScope = origListScope })
	agentSecretListScope = ""

	c := &cobra.Command{}
	c.SetContext(context.Background())
	var out bytes.Buffer
	c.SetOut(&out)
	require.NoError(t, runAgentSecretList(c, nil))

	text := out.String()
	assert.Contains(t, text, "SCOPE")
	assert.Contains(t, text, "AGENT_KEY")
	assert.Contains(t, text, "agent")
	assert.Contains(t, text, "PROJ_KEY")
	assert.Contains(t, text, "project")
}

func TestAgentSecretList_InvalidScopeFlag(t *testing.T) {
	origListScope := agentSecretListScope
	t.Cleanup(func() { agentSecretListScope = origListScope })
	agentSecretListScope = "invalid"

	c := &cobra.Command{}
	c.SetContext(context.Background())
	err := runAgentSecretList(c, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --scope")
}

var _ = sciontoolhub.NewClient // keep import explicit
