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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSAEmail = "worker@example.com"

func TestFormatGCPIdentity(t *testing.T) {
	tests := []struct {
		name string
		id   *api.AgentGCPIdentity
		want string
	}{
		{name: "nil", id: nil, want: "none"},
		{name: "empty mode", id: &api.AgentGCPIdentity{}, want: "none"},
		{name: "block", id: &api.AgentGCPIdentity{Mode: "block"}, want: "block"},
		{name: "passthrough", id: &api.AgentGCPIdentity{Mode: "passthrough"}, want: "passthrough"},
		{
			name: "assign prefers display name",
			id:   &api.AgentGCPIdentity{Mode: "assign", ServiceAccountEmail: testSAEmail, DisplayName: "Build worker"},
			want: `assign as "Build worker"`,
		},
		{
			name: "assign falls back to email",
			id:   &api.AgentGCPIdentity{Mode: "assign", ServiceAccountEmail: testSAEmail},
			want: `assign as "` + testSAEmail + `"`,
		},
		{name: "assign without account", id: &api.AgentGCPIdentity{Mode: "assign"}, want: "assign (unknown account)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatGCPIdentity(tt.id))
		})
	}
}

func TestLookIdentityHeader(t *testing.T) {
	assert.Equal(t, "", lookIdentityHeader(nil, "default"), "no identity recorded prints no header")
	assert.Equal(t, "GCP identity: block", lookIdentityHeader(&api.AgentGCPIdentity{Mode: "block"}, ""))
	assert.Equal(t, `GCP identity: assign as "Build worker" (profile: gke)`,
		lookIdentityHeader(&api.AgentGCPIdentity{Mode: "assign", ServiceAccountEmail: testSAEmail, DisplayName: "Build worker"}, "gke"))
}

func TestHubAgentToAgentInfo_GCPIdentity(t *testing.T) {
	var a hubclient.Agent
	require.NoError(t, json.Unmarshal([]byte(`{
		"id": "a1", "slug": "a1",
		"appliedConfig": {"gcpIdentity": {"metadataMode": "assign", "serviceAccountId": "sa-1", "serviceAccountEmail": "`+testSAEmail+`"}}
	}`), &a))

	info := hubAgentToAgentInfo(a)
	require.NotNil(t, info.GCPIdentity)
	assert.Equal(t, api.AgentGCPIdentity{Mode: "assign", ServiceAccountID: "sa-1", ServiceAccountEmail: testSAEmail}, *info.GCPIdentity)

	assert.Nil(t, hubAgentToAgentInfo(hubclient.Agent{ID: "a2"}).GCPIdentity, "no applied identity stays nil")
}

// scion list --format json carries the applied identity, with the display
// name resolved from the project's service account registrations.
func TestListAgentsViaHub_JSONIncludesGCPIdentity(t *testing.T) {
	var saQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/gcp-service-accounts":
			saQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"items": []map[string]interface{}{{"id": "sa-1", "email": testSAEmail, "displayName": "Build worker"}},
			})
		case strings.HasSuffix(r.URL.Path, "/agents"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{"id": "a1", "slug": "assigned", "projectId": "p1", "phase": "running",
						"appliedConfig": map[string]interface{}{"gcpIdentity": map[string]interface{}{
							"metadataMode": "assign", "serviceAccountId": "sa-1", "serviceAccountEmail": testSAEmail}}},
					{"id": "a2", "slug": "blocked", "projectId": "p1", "phase": "running",
						"appliedConfig": map[string]interface{}{"gcpIdentity": map[string]interface{}{"metadataMode": "block"}}},
					{"id": "a3", "slug": "unset", "projectId": "p1", "phase": "running"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	oldListAll, oldOutputFormat := listAll, outputFormat
	listAll, outputFormat = true, "json"
	defer func() { listAll, outputFormat = oldListAll, oldOutputFormat }()

	stdout, _ := captureStdoutStderr(t, func() {
		require.NoError(t, listAgentsViaHub(hubCtx))
	})

	var got []api.AgentInfo
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
	require.Len(t, got, 3)
	byName := map[string]api.AgentInfo{}
	for _, a := range got {
		byName[a.Name] = a
	}
	require.NotNil(t, byName["assigned"].GCPIdentity)
	assert.Equal(t, api.AgentGCPIdentity{Mode: "assign", ServiceAccountID: "sa-1", ServiceAccountEmail: testSAEmail, DisplayName: "Build worker"},
		*byName["assigned"].GCPIdentity)
	require.NotNil(t, byName["blocked"].GCPIdentity)
	assert.Equal(t, "block", byName["blocked"].GCPIdentity.Mode)
	assert.Nil(t, byName["unset"].GCPIdentity)
	assert.Contains(t, saQuery, "scopeId=p1")
	assert.Contains(t, stdout, `"gcpIdentity"`)
}

// The table output does not look up service accounts: it has no identity
// column, so the lookups would be wasted calls.
func TestListAgentsViaHub_TableSkipsServiceAccountLookup(t *testing.T) {
	saCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/gcp-service-accounts" {
			saCalls++
			_, _ = w.Write([]byte(`{"items": []}`))
			return
		}
		_, _ = w.Write([]byte(`{"agents": [{"id": "a1", "slug": "assigned", "projectId": "p1", "phase": "running",
			"appliedConfig": {"gcpIdentity": {"metadataMode": "assign", "serviceAccountId": "sa-1"}}}]}`))
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	oldListAll, oldOutputFormat := listAll, outputFormat
	listAll, outputFormat = true, ""
	defer func() { listAll, outputFormat = oldListAll, oldOutputFormat }()

	_, _ = captureStdoutStderr(t, func() {
		require.NoError(t, listAgentsViaHub(&HubContext{Client: client, Endpoint: server.URL}))
	})
	assert.Equal(t, 0, saCalls)
}

func lookIdentityServer(t *testing.T, agentJSON string, saStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/gcp-service-accounts":
			if saStatus != http.StatusOK {
				w.WriteHeader(saStatus)
				_, _ = w.Write([]byte(`{"error": {"code": "forbidden", "message": "denied"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"items": [{"id": "sa-1", "email": "` + testSAEmail + `", "displayName": "Build worker"}]}`))
		case "/api/v1/projects/p1/agents/worker":
			_, _ = w.Write([]byte(agentJSON))
		default:
			http.NotFound(w, r)
		}
	}))
}

// scion look prints the identity header on stderr, leaving stdout to the
// captured terminal output.
func TestPrintLookIdentityHeader(t *testing.T) {
	const assigned = `{"id": "a1", "slug": "worker", "projectId": "p1",
		"appliedConfig": {"profile": "gke", "gcpIdentity": {"metadataMode": "assign", "serviceAccountId": "sa-1", "serviceAccountEmail": "` + testSAEmail + `"}}}`

	tests := []struct {
		name      string
		agentJSON string
		saStatus  int
		want      string
	}{
		{name: "display name", agentJSON: assigned, saStatus: http.StatusOK,
			want: "GCP identity: assign as \"Build worker\" (profile: gke)\n"},
		{name: "email when registrations are unreadable", agentJSON: assigned, saStatus: http.StatusForbidden,
			want: "GCP identity: assign as \"" + testSAEmail + "\" (profile: gke)\n"},
		{name: "block", agentJSON: `{"id": "a1", "slug": "worker", "appliedConfig": {"gcpIdentity": {"metadataMode": "block"}}}`,
			saStatus: http.StatusOK, want: "GCP identity: block\n"},
		{name: "no identity recorded", agentJSON: `{"id": "a1", "slug": "worker", "appliedConfig": {"profile": "gke"}}`,
			saStatus: http.StatusOK, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := lookIdentityServer(t, tt.agentJSON, tt.saStatus)
			defer server.Close()
			client, err := hubclient.New(server.URL)
			require.NoError(t, err)

			stdout, stderr := captureStdoutStderr(t, func() {
				printLookIdentityHeader(client, client.ProjectAgents("p1"), "p1", "worker")
			})
			assert.Empty(t, stdout)
			assert.Equal(t, tt.want, stderr)
		})
	}
}

// A failed agent read prints no header rather than an error: the header is
// advisory and must not block viewing the terminal.
func TestPrintLookIdentityHeader_AgentUnreadable(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	stdout, stderr := captureStdoutStderr(t, func() {
		printLookIdentityHeader(client, client.ProjectAgents("p1"), "p1", "worker")
	})
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
}
