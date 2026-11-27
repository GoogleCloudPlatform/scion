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

func TestBuildLabelUpdateRequest(t *testing.T) {
	t.Run("sets and removes disjoint keys", func(t *testing.T) {
		req, err := buildLabelUpdateRequest([]string{"env=prod", "team=core"}, []string{"tmp", "tmp"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"env": "prod", "team": "core"}, req.Set)
		assert.Equal(t, []string{"tmp"}, req.Remove)
	})

	t.Run("requires at least one set or remove", func(t *testing.T) {
		_, err := buildLabelUpdateRequest(nil, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "specify at least one")
	})

	t.Run("rejects malformed label without equals", func(t *testing.T) {
		_, err := buildLabelUpdateRequest([]string{"nope"}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "key=value")
	})

	t.Run("rejects empty remove key", func(t *testing.T) {
		_, err := buildLabelUpdateRequest(nil, []string{""})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not be empty")
	})

	t.Run("rejects overlapping set and remove key", func(t *testing.T) {
		_, err := buildLabelUpdateRequest([]string{"env=prod"}, []string{"env"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both set and removed")
	})
}

func TestLabelCmd_AgentModeExcluded(t *testing.T) {
	assert.False(t, agentAllowed["label"], "scion label must be human-mode only")

	root := buildTestTree()
	applyModeRestrictions(root, ModeAgent)
	names := collectCommandNames(root)
	assert.NotContains(t, names, "label", "scion label must be stripped in agent mode")
}

func TestLabelCmd_WindowsAllowed(t *testing.T) {
	assert.True(t, windowsAllowed["label"], "scion label is a pure Hub API call and must be allowed on Windows")
}

func TestFormatListLabels(t *testing.T) {
	assert.Equal(t, "-", formatListLabels(nil))
	assert.Equal(t, "-", formatListLabels(map[string]string{}))
	assert.Equal(t, "a=1,b=2", formatListLabels(map[string]string{"b": "2", "a": "1"}))

	long := formatListLabels(map[string]string{
		"alpha": "1234567890",
		"beta":  "1234567890",
		"gamma": "1234567890",
	})
	assert.LessOrEqual(t, len([]rune(long)), listLabelsMaxRunes)
	assert.True(t, strings.HasSuffix(long, "..."), "long labels should be truncated: %q", long)
}

func TestDisplayAgents_ShowsLabelsColumn(t *testing.T) {
	origFormat := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = origFormat })

	agents := []api.AgentInfo{
		{
			Name:            "labeled-agent",
			Template:        "default",
			Runtime:         "docker",
			Project:         "proj",
			RuntimeBrokerID: "broker-1",
			Phase:           "running",
			ContainerStatus: "running",
			Labels:          map[string]string{"team": "core", "env": "dev"},
		},
		{
			Name:            "unlabeled-agent",
			Template:        "default",
			Runtime:         "docker",
			Project:         "proj",
			RuntimeBrokerID: "broker-1",
			Phase:           "stopped",
			ContainerStatus: "exited",
		},
	}

	var err error
	out := captureStdout(t, func() {
		err = displayAgents(agents, false)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "LABELS")
	assert.Contains(t, out, "env=dev,team=core")

	// JSON output must preserve the full labels map.
	outputFormat = "json"
	jsonOut := captureStdout(t, func() {
		err = displayAgents(agents, false)
	})
	require.NoError(t, err)
	var decoded []api.AgentInfo
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &decoded))
	require.Len(t, decoded, 2)
	assert.Equal(t, map[string]string{"team": "core", "env": "dev"}, decoded[0].Labels)
	assert.Empty(t, decoded[1].Labels)
}

func TestHubClient_UpdateLabels(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody hubclient.UpdateAgentLabelsRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.Agent{
			ID:     "ag-1",
			Name:   "my-agent",
			Labels: map[string]string{"env": "prod"},
		})
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	got, err := client.ProjectAgents("proj-1").UpdateLabels(context.Background(), "my-agent", &hubclient.UpdateAgentLabelsRequest{
		Set:    map[string]string{"env": "prod"},
		Remove: []string{"old"},
	})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPatch, gotMethod)
	assert.Equal(t, "/api/v1/projects/proj-1/agents/my-agent/labels", gotPath)
	assert.Equal(t, map[string]string{"env": "prod"}, gotBody.Set)
	assert.Equal(t, []string{"old"}, gotBody.Remove)
	assert.Equal(t, map[string]string{"env": "prod"}, got.Labels)
}
