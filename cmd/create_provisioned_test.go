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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newProvisionCreateHub serves a create that answers with an agent in
// phase created and the given provisionedOnly value.
func newProvisionCreateHub(t *testing.T, provisionedOnly bool) *HubContext {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/agents") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"agent": map[string]interface{}{
				"id":              "agent-1",
				"slug":            "po-agent",
				"name":            "po-agent",
				"phase":           "created",
				"provisionedOnly": provisionedOnly,
				"created":         time.Now().UTC().Format(time.RFC3339),
			},
		})
	}))
	t.Cleanup(server.Close)
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	return &HubContext{
		Client:      client,
		Endpoint:    server.URL,
		ProjectID:   "project-1",
		ProjectPath: t.TempDir(),
	}
}

func TestCreateViaHub_ProvisionedOnlyPrintsStartHint(t *testing.T) {
	prev := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prev })

	hubCtx := newProvisionCreateHub(t, true)
	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = createAgentViaHub(hubCtx, "po-agent", "") })
	})
	require.NoError(t, err)
	assert.Contains(t, stderr, "Phase: created (not started)")
	assert.Contains(t, stderr, "provisioned but not started")
	assert.Contains(t, stderr, "scion start po-agent")
}

func TestCreateViaHub_NotProvisionedOnlyHasNoHint(t *testing.T) {
	prev := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prev })

	hubCtx := newProvisionCreateHub(t, false)
	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = createAgentViaHub(hubCtx, "po-agent", "") })
	})
	require.NoError(t, err)
	assert.Contains(t, stderr, "Phase: created\n")
	assert.NotContains(t, stderr, "scion start")
}

func TestCreateViaHub_ProvisionedOnlyJSON(t *testing.T) {
	prev := outputFormat
	outputFormat = "json"
	t.Cleanup(func() { outputFormat = prev })

	hubCtx := newProvisionCreateHub(t, true)
	var err error
	stdout := captureStdout(t, func() {
		_ = captureStderr(t, func() { err = createAgentViaHub(hubCtx, "po-agent", "") })
	})
	require.NoError(t, err)
	var result ActionResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &result), stdout)
	assert.Equal(t, false, result.Details["started"])
	assert.Contains(t, result.Details["hint"], "scion start po-agent")
}

func TestHubAgentToAgentInfo_ProvisionedOnly(t *testing.T) {
	info := hubAgentToAgentInfo(hubclient.Agent{Name: "a", Phase: "created", ProvisionedOnly: true})
	assert.True(t, info.ProvisionedOnly)
}

func TestDisplayAgents_ProvisionedOnlyLabel(t *testing.T) {
	prev := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prev })

	agents := []api.AgentInfo{
		{Name: "po-agent", Template: "default", Phase: "created", ProvisionedOnly: true},
		{Name: "full-agent", Template: "default", Phase: "created"},
	}
	var err error
	out := captureStdout(t, func() { err = displayAgents(agents, false, true) })
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3, out)
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "po-agent") {
			assert.Contains(t, l, "created (not started)")
		} else {
			assert.NotContains(t, l, "not started")
		}
	}
}
