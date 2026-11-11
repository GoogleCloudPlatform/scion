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

package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// IncludeAuthority sends include=authority on both list paths, and the
// authority object decodes onto each agent. Without it nothing is sent.
func TestAgentService_List_IncludeAuthority(t *testing.T) {
	var gotInclude []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotInclude = append(gotInclude, r.URL.Query().Get("include"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agents":[{"id":"a1","slug":"a1","authority":{"state":"orphaned","cause":"delegator_deleted","principal":{"kind":"user","deleted":true}}}]}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	for _, svc := range []AgentService{client.Agents(), client.ProjectAgents("project-1")} {
		gotInclude = nil
		resp, err := svc.List(context.Background(), &ListAgentsOptions{IncludeAuthority: true})
		require.NoError(t, err)
		assert.Equal(t, []string{"authority"}, gotInclude)
		require.Len(t, resp.Agents, 1)
		assert.Equal(t, &api.AgentAuthority{
			State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseDelegatorDeleted,
			Principal: &api.AgentAuthorityPrincipal{Kind: "user", Deleted: true},
		}, resp.Agents[0].Authority)

		gotInclude = nil
		_, err = svc.List(context.Background(), &ListAgentsOptions{})
		require.NoError(t, err)
		assert.Equal(t, []string{""}, gotInclude)
	}
}
