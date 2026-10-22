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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mcpRedactionServers returns an MCP server map with one stdio server
// (env) and one HTTP server (headers).
func mcpRedactionServers() map[string]api.MCPServerConfig {
	return map[string]api.MCPServerConfig{
		"local-tool": {
			Transport: api.MCPTransportStdio,
			Command:   "local-tool",
			Env:       map[string]string{"TOOL_API_KEY": "stdio-env-value"},
		},
		"remote-tool": {
			Transport: api.MCPTransportStreamableHTTP,
			URL:       "https://mcp.example.com/mcp",
			Headers:   map[string]string{"Authorization": "Bearer header-value"},
		},
	}
}

// mcpServersInResponse is the part of an agent response this test reads:
// the MCP server maps of inlineConfig and createInputs.inlineConfig, as a
// client decodes them.
type mcpServersInResponse struct {
	ID            string `json:"id"`
	AppliedConfig *struct {
		InlineConfig *struct {
			MCPServers map[string]api.MCPServerConfig `json:"mcp_servers"`
		} `json:"inlineConfig"`
		CreateInputs *struct {
			InlineConfig *struct {
				MCPServers map[string]api.MCPServerConfig `json:"mcp_servers"`
			} `json:"inlineConfig"`
		} `json:"createInputs"`
	} `json:"appliedConfig"`
}

// servers returns both MCP server maps, keyed by where they appear.
func (r mcpServersInResponse) servers(t *testing.T) map[string]map[string]api.MCPServerConfig {
	t.Helper()
	require.NotNil(t, r.AppliedConfig, "agent %s: response has no appliedConfig", r.ID)
	require.NotNil(t, r.AppliedConfig.InlineConfig, "agent %s: response has no inlineConfig", r.ID)
	require.NotNil(t, r.AppliedConfig.CreateInputs, "agent %s: response has no createInputs", r.ID)
	require.NotNil(t, r.AppliedConfig.CreateInputs.InlineConfig, "agent %s: response has no createInputs.inlineConfig", r.ID)
	return map[string]map[string]api.MCPServerConfig{
		"inlineConfig":              r.AppliedConfig.InlineConfig.MCPServers,
		"createInputs.inlineConfig": r.AppliedConfig.CreateInputs.InlineConfig.MCPServers,
	}
}

// assertMCPValuesOmitted: both servers are listed, with their env and
// headers omitted (like env for a caller without attach).
func assertMCPValuesOmitted(t *testing.T, r mcpServersInResponse, path string) {
	t.Helper()
	for where, servers := range r.servers(t) {
		require.Len(t, servers, 2, "%s %s: every MCP server stays listed", path, where)
		assert.Nil(t, servers["local-tool"].Env, "%s %s: MCP server env must be omitted without attach", path, where)
		assert.Equal(t, "local-tool", servers["local-tool"].Command, "%s %s", path, where)
		assert.Nil(t, servers["remote-tool"].Headers, "%s %s: MCP server headers must be omitted without attach", path, where)
		assert.Equal(t, "https://mcp.example.com/mcp", servers["remote-tool"].URL, "%s %s", path, where)
	}
}

// assertMCPValuesVisible: env and header values are returned unchanged.
func assertMCPValuesVisible(t *testing.T, r mcpServersInResponse, path string) {
	t.Helper()
	for where, servers := range r.servers(t) {
		assert.Equal(t, map[string]string{"TOOL_API_KEY": "stdio-env-value"}, servers["local-tool"].Env, "%s %s", path, where)
		assert.Equal(t, map[string]string{"Authorization": "Bearer header-value"}, servers["remote-tool"].Headers, "%s %s", path, where)
	}
}

// decodeSingleAgentMCP decodes a response whose body is the agent itself,
// or an envelope carrying it under "agent".
func decodeSingleAgentMCP(t *testing.T, body []byte) mcpServersInResponse {
	t.Helper()
	var envelope struct {
		Agent *mcpServersInResponse `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	if envelope.Agent != nil {
		return *envelope.Agent
	}
	var got mcpServersInResponse
	require.NoError(t, json.Unmarshal(body, &got))
	return got
}

// decodeListedAgentMCP finds agentID in a list response.
func decodeListedAgentMCP(t *testing.T, body []byte, agentID string) mcpServersInResponse {
	t.Helper()
	var resp struct {
		Agents []mcpServersInResponse `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	for _, a := range resp.Agents {
		if a.ID == agentID {
			return a
		}
	}
	t.Fatalf("agent %s not found in list response: %s", agentID, body)
	return mcpServersInResponse{}
}

// TestAgentResponseMCPServerEnvAndHeaders_CrossMember: agent response
// redaction covers every MCP server's env and headers, under the same
// visibility decision as env. A project owner and a project admin acting on
// another member's agent hold agent update and lifecycle but not attach
// (see TestCrossMemberAttach_Matrix), so every response path they reach
// omits those values; the agent's owner, who can attach, sees them
// unchanged. Stored data is never changed by the redaction.
func TestAgentResponseMCPServerEnvAndHeaders_CrossMember(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:        tid("agent-mcp-redaction"),
		Slug:      "agent-mcp-redaction",
		Name:      "MCP Redaction Agent",
		ProjectID: project.ID,
		OwnerID:   alice.ID,
		Phase:     string(state.PhaseStopped),
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{MCPServers: mcpRedactionServers()},
			CreateInputs: &store.AgentCreateInputs{
				InlineConfig: &api.ScionConfig{MCPServers: mcpRedactionServers()},
			},
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	projectOwner := makeProjectMemberUser(t, s, project, tid("user-owner-mcp-redaction"), "Project Owner", store.GroupMemberRoleOwner)
	createTestUserWithProjectRole(t, s, projectOwner.ID, projectOwner.Email, project.ID, store.ProjectRoleOwner)
	projectAdmin := makeProjectMemberUser(t, s, project, tid("user-admin-mcp-redaction"), "Project Admin", store.GroupMemberRoleAdmin)
	createTestUserWithProjectRole(t, s, projectAdmin.ID, projectAdmin.Email, project.ID, store.ProjectRoleAdmin)

	projectAgents := "/api/v1/projects/" + project.ID + "/agents"
	reads := []struct {
		path string
		list bool
	}{
		{"/api/v1/agents/" + agent.ID, false},
		{projectAgents + "/" + agent.ID, false},
		{"/api/v1/agents?projectId=" + project.ID, true},
		{"/api/v1/agents?sort=updated&limit=50", true},
		{projectAgents, true},
		{projectAgents + "?sort=updated&limit=50", true},
	}
	decodeRead := func(t *testing.T, body []byte, list bool) mcpServersInResponse {
		if list {
			return decodeListedAgentMCP(t, body, agent.ID)
		}
		return decodeSingleAgentMCP(t, body)
	}

	for _, caller := range []struct {
		name string
		user *store.User
	}{
		{"project owner", projectOwner},
		{"project admin", projectAdmin},
	} {
		t.Run(caller.name+" on another member's agent", func(t *testing.T) {
			for _, rd := range reads {
				rec := doRequestAsUser(t, srv, caller.user, http.MethodGet, rd.path, nil)
				require.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", rd.path, rec.Body.String())
				assertMCPValuesOmitted(t, decodeRead(t, rec.Body.Bytes(), rd.list), "GET "+rd.path)
			}

			rec := doRequestAsUser(t, srv, caller.user, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
				"taskSummary": "updated by " + caller.name,
			})
			require.Equal(t, http.StatusOK, rec.Code, "PATCH: %s", rec.Body.String())
			assertMCPValuesOmitted(t, decodeSingleAgentMCP(t, rec.Body.Bytes()), "PATCH")

			rec = doRequestAsUser(t, srv, caller.user, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
			require.Equal(t, http.StatusOK, rec.Code, "stop: %s", rec.Body.String())
			assertMCPValuesOmitted(t, decodeSingleAgentMCP(t, rec.Body.Bytes()), "POST stop")
		})
	}

	t.Run("agent owner sees values unchanged", func(t *testing.T) {
		for _, rd := range reads {
			rec := doRequestAsUser(t, srv, alice, http.MethodGet, rd.path, nil)
			require.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", rd.path, rec.Body.String())
			assertMCPValuesVisible(t, decodeRead(t, rec.Body.Bytes(), rd.list), "GET "+rd.path)
		}

		rec := doRequestAsUser(t, srv, alice, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
			"taskSummary": "updated by the agent owner",
		})
		require.Equal(t, http.StatusOK, rec.Code, "PATCH: %s", rec.Body.String())
		assertMCPValuesVisible(t, decodeSingleAgentMCP(t, rec.Body.Bytes()), "PATCH")

		rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
		require.Equal(t, http.StatusOK, rec.Code, "stop: %s", rec.Body.String())
		assertMCPValuesVisible(t, decodeSingleAgentMCP(t, rec.Body.Bytes()), "POST stop")
	})

	t.Run("stored MCP server env and headers are unchanged", func(t *testing.T) {
		stored, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.AppliedConfig)
		require.NotNil(t, stored.AppliedConfig.InlineConfig)
		assert.Equal(t, mcpRedactionServers(), stored.AppliedConfig.InlineConfig.MCPServers)
		require.NotNil(t, stored.AppliedConfig.CreateInputs)
		require.NotNil(t, stored.AppliedConfig.CreateInputs.InlineConfig)
		assert.Equal(t, mcpRedactionServers(), stored.AppliedConfig.CreateInputs.InlineConfig.MCPServers)
	})
}
