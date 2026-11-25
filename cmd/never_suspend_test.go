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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNeverSuspendFlag_RegisteredOnCreateAndStart(t *testing.T) {
	assert.NotNil(t, createCmd.Flags().Lookup("never-suspend"), "create must register --never-suspend")
	assert.NotNil(t, startCmd.Flags().Lookup("never-suspend"), "start must register --never-suspend")
}

func TestList_NeverSuspendPhaseLabel(t *testing.T) {
	info := hubAgentToAgentInfo(hubclient.Agent{
		Slug:          "lead",
		Phase:         "running",
		SuspendPolicy: "never",
	})
	assert.Equal(t, "never", info.SuspendPolicy)
	assert.Equal(t, "running ⏻", neverSuspendPhaseLabel("running", info.SuspendPolicy))
	assert.Equal(t, "running", neverSuspendPhaseLabel("running", "auto"))
	assert.Equal(t, "running", neverSuspendPhaseLabel("running", ""))
}

func TestFetchLookIdentityHeader_SuspendPolicyNever(t *testing.T) {
	tests := []struct {
		name      string
		agentJSON string
		want      string
	}{
		{
			name:      "never suspend without GCP identity",
			agentJSON: `{"id": "a1", "slug": "worker", "suspendPolicy": "never"}`,
			want:      "Suspend policy: never",
		},
		{
			name:      "auto suspend omits line",
			agentJSON: `{"id": "a1", "slug": "worker", "suspendPolicy": "auto"}`,
			want:      "",
		},
		{
			name:      "never suspend combined with GCP identity",
			agentJSON: `{"id": "a1", "slug": "worker", "suspendPolicy": "never", "appliedConfig": {"gcpIdentity": {"metadataMode": "block"}}}`,
			want:      "GCP identity: block\nSuspend policy: never",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := lookIdentityServer(t, tt.agentJSON, http.StatusOK)
			defer server.Close()
			client, err := hubclient.New(server.URL)
			require.NoError(t, err)

			got := fetchLookIdentityHeader(context.Background(), client, client.ProjectAgents("p1"), "p1", "worker", true)
			assert.Equal(t, tt.want, got)
		})
	}
}
