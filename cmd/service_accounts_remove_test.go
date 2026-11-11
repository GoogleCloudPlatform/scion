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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `remove` prints the Hub's impact report on refusal and accepts --force
// (ptone/scion#4022).

const saInUseBody = `{"error":{"code":"sa_in_use","message":"in use","details":{"impact":{
  "serviceAccountId":"sa-1","agentCount":1,"visibleAgentCount":1,"hiddenAgentCounts":[],
  "agents":[{"id":"agent-1","name":"worker","projectId":"proj-1"}],
  "defaults":[{"tier":"project","projectId":"proj-1","clearable":true},
              {"tier":"profile","projectId":"proj-1","profile":"k8s","clearable":true}],
  "brokerMappings":[{"brokerId":"b-1","brokerName":"gke-broker","profile":"k8s"}],
  "managed":false,"manualCleanup":["step"]}}}}`

const saRemovedBody = `{"deleted":true,
  "clearedDefaults":[{"tier":"project","projectId":"proj-1","clearable":true}],
  "impact":{"serviceAccountId":"sa-1","agentCount":0,"agents":[],"defaults":[],
  "brokerMappings":[],"managed":true,
  "manualCleanup":["Broker mapping: remove it","GCP account: retained in GCP"]}}`

func saRemoveFakeHub(t *testing.T) *[]string {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("force") == "true" {
			_, _ = w.Write([]byte(saRemovedBody))
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(saInUseBody))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	prevResolve, prevFormat, prevForce := resolveProjectForSA, outputFormat, saRemoveForce
	t.Cleanup(func() { resolveProjectForSA, outputFormat, saRemoveForce = prevResolve, prevFormat, prevForce })
	resolveProjectForSA = func() (hubclient.Client, string, error) { return client, "proj-1", nil }
	outputFormat = ""
	return &queries
}

func TestProjectSARemove_RefusalPrintsImpact(t *testing.T) {
	queries := saRemoveFakeHub(t)
	saRemoveForce = false

	var runErr error
	stdout, _ := captureStdoutStderr(t, func() { runErr = runSARemove(nil, []string{"sa-1"}) })
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "--force")
	assert.Equal(t, []string{""}, *queries, "force is not sent unless asked for")

	assert.Contains(t, stdout, "was not removed")
	assert.Contains(t, stdout, "project proj-1 default")
	assert.Contains(t, stdout, `project proj-1, profile "k8s" default`)
	assert.Contains(t, stdout, "worker (agent-1) in project proj-1")
	assert.Contains(t, stdout, "gke-broker/k8s")
	assert.Contains(t, stdout, "Re-run with --force")
	assert.Contains(t, stdout, "becomes 'block'")
}

func TestProjectSARemove_ForcePrintsClearedDefaultsAndCleanup(t *testing.T) {
	queries := saRemoveFakeHub(t)
	saRemoveForce = true

	var runErr error
	stdout, _ := captureStdoutStderr(t, func() { runErr = runSARemove(nil, []string{"sa-1"}) })
	require.NoError(t, runErr)
	assert.Equal(t, []string{"force=true"}, *queries)

	assert.Contains(t, stdout, "Removed service account sa-1")
	assert.Contains(t, stdout, "Cleared defaults")
	assert.Contains(t, stdout, "project proj-1 default")
	assert.Contains(t, stdout, "Cleanup the hub cannot do:")
	assert.Contains(t, stdout, "retained in GCP")
}

func TestPrintSAInUse_HubDefaultNamesAdminStep(t *testing.T) {
	impact := &hubclient.GCPServiceAccountImpact{
		Defaults: []hubclient.GCPServiceAccountImpactDefault{{Tier: "hub", Clearable: false}},
	}
	var buf bytes.Buffer
	printSAInUse(&buf, "sa-1", impact, true)
	stdout := buf.String()
	assert.Contains(t, stdout, "hub default (not cleared by --force")
	assert.Contains(t, stdout, "Ask a hub admin")
	assert.NotContains(t, stdout, "Re-run with --force")
}

func TestPrintSAInUse_RedactedDefaultsAndHiddenAgents(t *testing.T) {
	impact := &hubclient.GCPServiceAccountImpact{
		AgentCount:         3,
		HiddenAgentCounts:  []int{2, 1},
		HiddenDefaultCount: 1,
		Defaults: []hubclient.GCPServiceAccountImpactDefault{
			{Tier: "profile", Clearable: false, Redacted: true},
		},
	}
	var buf bytes.Buffer
	printSAInUse(&buf, "sa-1", impact, true)
	out := buf.String()
	assert.Contains(t, out, "a per-profile default in another project (not cleared by --force; an admin of that project must change it)")
	assert.Contains(t, out, "3 agent(s) you cannot see, in 2 project(s)")
	assert.Contains(t, out, "Ask an admin of the other projects")
	assert.NotContains(t, out, "Re-run with --force")
}
