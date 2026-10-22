// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mcpServersFixture returns an MCP server map with one stdio server (env)
// and one HTTP server (headers).
func mcpServersFixture() map[string]api.MCPServerConfig {
	return map[string]api.MCPServerConfig{
		"local-tool": {
			Transport: api.MCPTransportStdio,
			Command:   "local-tool",
			Args:      []string{"--serve"},
			Env:       map[string]string{"TOOL_API_KEY": "stdio-env-value"},
			Scope:     api.MCPScopeProject,
		},
		"remote-tool": {
			Transport: api.MCPTransportStreamableHTTP,
			URL:       "https://mcp.example.com/mcp",
			Headers:   map[string]string{"Authorization": "Bearer header-value"},
		},
	}
}

func mcpServersResponseConfig() *AgentAppliedConfig {
	return &AgentAppliedConfig{
		InlineConfig: &api.ScionConfig{MCPServers: mcpServersFixture()},
		CreateInputs: &AgentCreateInputs{
			InlineConfig: &api.ScionConfig{MCPServers: mcpServersFixture()},
		},
	}
}

type mcpServerView struct {
	Transport string            `json:"transport"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Scope     string            `json:"scope"`
}

// decodeMCPServers marshals view and returns the mcp_servers maps of
// inlineConfig and createInputs.inlineConfig, as a client would read them.
func decodeMCPServers(t *testing.T, view *AgentAppliedConfig) (inline, createInputs map[string]mcpServerView) {
	t.Helper()
	data, err := json.Marshal(view)
	require.NoError(t, err)
	var decoded struct {
		InlineConfig struct {
			MCPServers map[string]mcpServerView `json:"mcp_servers"`
		} `json:"inlineConfig"`
		CreateInputs struct {
			InlineConfig struct {
				MCPServers map[string]mcpServerView `json:"mcp_servers"`
			} `json:"inlineConfig"`
		} `json:"createInputs"`
	}
	require.NoError(t, json.Unmarshal(data, &decoded))
	return decoded.InlineConfig.MCPServers, decoded.CreateInputs.InlineConfig.MCPServers
}

// TestResponseViewMCPServers_NoAttachOmitsEnvAndHeaders: without attach,
// every MCP server's env and headers are omitted from the response, in
// InlineConfig and in CreateInputs.InlineConfig, like InlineConfig.Env. The
// entries and their other fields stay visible.
func TestResponseViewMCPServers_NoAttachOmitsEnvAndHeaders(t *testing.T) {
	inline, createInputs := decodeMCPServers(t, mcpServersResponseConfig().ResponseView(false))
	for where, servers := range map[string]map[string]mcpServerView{"inlineConfig": inline, "createInputs.inlineConfig": createInputs} {
		require.Len(t, servers, 2, where)

		local := servers["local-tool"]
		assert.Nil(t, local.Env, "%s: stdio env must be omitted without attach", where)
		assert.Equal(t, "stdio", local.Transport, where)
		assert.Equal(t, "local-tool", local.Command, where)
		assert.Equal(t, []string{"--serve"}, local.Args, where)
		assert.Equal(t, "project", local.Scope, where)

		remote := servers["remote-tool"]
		assert.Nil(t, remote.Headers, "%s: headers must be omitted without attach", where)
		assert.Equal(t, "streamable-http", remote.Transport, where)
		assert.Equal(t, "https://mcp.example.com/mcp", remote.URL, where)
	}
}

// TestResponseViewMCPServers_AttachKeepsValues: with attach, env and header
// values are returned unchanged.
func TestResponseViewMCPServers_AttachKeepsValues(t *testing.T) {
	inline, createInputs := decodeMCPServers(t, mcpServersResponseConfig().ResponseView(true))
	for where, servers := range map[string]map[string]mcpServerView{"inlineConfig": inline, "createInputs.inlineConfig": createInputs} {
		assert.Equal(t, map[string]string{"TOOL_API_KEY": "stdio-env-value"}, servers["local-tool"].Env, where)
		assert.Equal(t, map[string]string{"Authorization": "Bearer header-value"}, servers["remote-tool"].Headers, where)
	}
}

// TestResponseViewMCPServers_InputUnchanged: the redaction is response-only.
// Neither view mutates the original config, and changing the view's maps
// does not reach the original.
func TestResponseViewMCPServers_InputUnchanged(t *testing.T) {
	ac := mcpServersResponseConfig()
	want := mcpServersFixture()

	_ = ac.ResponseView(false)
	assert.Equal(t, want, ac.InlineConfig.MCPServers)
	assert.Equal(t, want, ac.CreateInputs.InlineConfig.MCPServers)

	view := ac.ResponseView(true)
	view.InlineConfig.MCPServers["local-tool"].Env["TOOL_API_KEY"] = "changed"
	view.InlineConfig.MCPServers["local-tool"].Args[0] = "changed"
	view.InlineConfig.MCPServers["remote-tool"].Headers["Authorization"] = "changed"
	view.InlineConfig.MCPServers["added"] = api.MCPServerConfig{}
	view.CreateInputs.InlineConfig.MCPServers["local-tool"].Env["TOOL_API_KEY"] = "changed"
	assert.Equal(t, want, ac.InlineConfig.MCPServers)
	assert.Equal(t, want, ac.CreateInputs.InlineConfig.MCPServers)
}
