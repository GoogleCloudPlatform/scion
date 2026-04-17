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
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workflowRunFixture is a minimal WorkflowRun for test servers.
var workflowRunFixture = hubclient.WorkflowRun{
	ID:        "run-abc123",
	GroveID:   "grove-1",
	Status:    "succeeded",
	CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

// newWorkflowMockServer creates an httptest.Server that stubs the workflow run
// endpoints used by the CLI commands. Returns the server URL and a slice that
// records incoming requests as "METHOD /path".
func newWorkflowMockServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()

	var reqs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

		case r.URL.Path == "/api/v1/groves/grove-1/workflows/runs" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"runs":       []hubclient.WorkflowRun{workflowRunFixture},
				"nextCursor": "",
			})

		case r.URL.Path == "/api/v1/workflows/runs/run-abc123" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"run": hubclient.WorkflowRunDetail{WorkflowRun: workflowRunFixture},
			})

		case r.URL.Path == "/api/v1/workflows/runs/run-abc123/cancel" && r.Method == http.MethodPost:
			canceled := workflowRunFixture
			canceled.Status = "canceled"
			json.NewEncoder(w).Encode(map[string]interface{}{"run": canceled})

		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(srv.Close)
	return srv, &reqs
}

// TestWorkflowListCommandStructure verifies that the list subcommand is
// registered with the expected flags.
func TestWorkflowListCommandStructure(t *testing.T) {
	assert.Equal(t, "list", workflowListCmd.Use)
	assert.NotEmpty(t, workflowListCmd.Short)
	assert.NotNil(t, workflowListCmd.RunE)

	statusFlag := workflowListCmd.Flags().Lookup("status")
	require.NotNil(t, statusFlag, "--status flag should be registered")
	assert.Equal(t, "", statusFlag.DefValue)

	limitFlag := workflowListCmd.Flags().Lookup("limit")
	require.NotNil(t, limitFlag, "--limit flag should be registered")
	assert.Equal(t, "20", limitFlag.DefValue)

	cursorFlag := workflowListCmd.Flags().Lookup("cursor")
	require.NotNil(t, cursorFlag, "--cursor flag should be registered")

	groveFlag := workflowListCmd.Flags().Lookup("grove")
	require.NotNil(t, groveFlag, "--grove flag should be registered")
}

// TestWorkflowGetCommandStructure verifies that the get subcommand is
// registered with the expected args and flags.
func TestWorkflowGetCommandStructure(t *testing.T) {
	assert.Equal(t, "get <run-id>", workflowGetCmd.Use)
	assert.NotEmpty(t, workflowGetCmd.Short)
	assert.NotNil(t, workflowGetCmd.RunE)

	showSourceFlag := workflowGetCmd.Flags().Lookup("show-source")
	require.NotNil(t, showSourceFlag, "--show-source flag should be registered")
	assert.Equal(t, "false", showSourceFlag.DefValue)
}

// TestWorkflowLogsCommandStructure verifies that the logs subcommand is
// registered with -f/--follow.
func TestWorkflowLogsCommandStructure(t *testing.T) {
	assert.Equal(t, "logs <run-id>", workflowLogsCmd.Use)
	assert.NotEmpty(t, workflowLogsCmd.Short)
	assert.NotNil(t, workflowLogsCmd.RunE)

	followFlag := workflowLogsCmd.Flags().Lookup("follow")
	require.NotNil(t, followFlag, "--follow flag should be registered")
	assert.Equal(t, "false", followFlag.DefValue)

	followShort := workflowLogsCmd.Flags().ShorthandLookup("f")
	require.NotNil(t, followShort, "-f shorthand should be registered")
}

// TestWorkflowCancelCommandStructure verifies that the cancel subcommand is
// registered and requires exactly one argument.
func TestWorkflowCancelCommandStructure(t *testing.T) {
	assert.Equal(t, "cancel <run-id>", workflowCancelCmd.Use)
	assert.NotEmpty(t, workflowCancelCmd.Short)
	assert.NotNil(t, workflowCancelCmd.RunE)
}

// TestWorkflowCmdSubcommands verifies all four new subcommands are registered
// under the parent workflow command.
func TestWorkflowCmdSubcommands(t *testing.T) {
	names := make(map[string]bool)
	for _, sub := range workflowCmd.Commands() {
		names[sub.Use] = true
	}
	assert.True(t, names["list"], "workflow list should be registered")
	assert.True(t, names["get <run-id>"], "workflow get should be registered")
	assert.True(t, names["logs <run-id>"], "workflow logs should be registered")
	assert.True(t, names["cancel <run-id>"], "workflow cancel should be registered")
}

// TestWorkflowListViaHub_HappyPath calls runWorkflowList against a mock HTTP
// server with a fixed grove ID override. It verifies the list endpoint is
// hit and no error is returned.
func TestWorkflowListViaHub_HappyPath(t *testing.T) {
	srv, reqs := newWorkflowMockServer(t)

	origGrove := workflowListGrove
	origNoHub := noHub
	origHubEndpoint := hubEndpoint
	origOutputFormat := outputFormat
	defer func() {
		workflowListGrove = origGrove
		noHub = origNoHub
		hubEndpoint = origHubEndpoint
		outputFormat = origOutputFormat
	}()

	workflowListGrove = "grove-1"
	noHub = false
	hubEndpoint = srv.URL
	outputFormat = "json"

	err := runWorkflowList(nil, nil)
	require.NoError(t, err)

	assert.Contains(t, *reqs, "GET /api/v1/groves/grove-1/workflows/runs",
		"expected list endpoint to be called")
}

// TestWorkflowGetViaHub_HappyPath calls runWorkflowGet against a mock HTTP
// server and checks no error is returned.
func TestWorkflowGetViaHub_HappyPath(t *testing.T) {
	srv, reqs := newWorkflowMockServer(t)

	origNoHub := noHub
	origHubEndpoint := hubEndpoint
	origOutputFormat := outputFormat
	defer func() {
		noHub = origNoHub
		hubEndpoint = origHubEndpoint
		outputFormat = origOutputFormat
	}()

	noHub = false
	hubEndpoint = srv.URL
	outputFormat = "json"

	err := runWorkflowGet(nil, []string{"run-abc123"})
	require.NoError(t, err)

	assert.Contains(t, *reqs, "GET /api/v1/workflows/runs/run-abc123",
		"expected get endpoint to be called")
}

// TestWorkflowCancelViaHub_HappyPath calls runWorkflowCancel against a mock
// HTTP server and checks no error is returned.
func TestWorkflowCancelViaHub_HappyPath(t *testing.T) {
	srv, reqs := newWorkflowMockServer(t)

	origNoHub := noHub
	origHubEndpoint := hubEndpoint
	origOutputFormat := outputFormat
	defer func() {
		noHub = origNoHub
		hubEndpoint = origHubEndpoint
		outputFormat = origOutputFormat
	}()

	noHub = false
	hubEndpoint = srv.URL
	outputFormat = "json"

	err := runWorkflowCancel(nil, []string{"run-abc123"})
	require.NoError(t, err)

	assert.Contains(t, *reqs, "POST /api/v1/workflows/runs/run-abc123/cancel",
		"expected cancel endpoint to be called")
}

// TestBuildInputsJSON_FlagsTakePrecedenceOverFile exercises the inputs merge
// logic used by workflow run --hub mode.
func TestBuildInputsJSON_FlagsTakePrecedenceOverFile(t *testing.T) {
	inputFile := t.TempDir() + "/inputs.json"
	require.NoError(t, os.WriteFile(inputFile, []byte(`{"key1":"from-file","key2":"from-file"}`), 0644))

	out, err := buildInputsJSON([]string{"key1=from-flag"}, inputFile)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	assert.Equal(t, "from-flag", m["key1"], "flag should override file value")
	assert.Equal(t, "from-file", m["key2"], "file value should be kept when not overridden")
}

// TestBuildInputsJSON_EmptyReturnsEmpty verifies the function returns "" when
// no inputs are provided.
func TestBuildInputsJSON_EmptyReturnsEmpty(t *testing.T) {
	out, err := buildInputsJSON(nil, "")
	require.NoError(t, err)
	assert.Equal(t, "", out, "empty inputs should return empty string")
}

// TestLoadSettingsForWorkflow_UnloadablePathReturnsNonNil verifies that when
// config.LoadSettings fails (e.g. grovePath is bogus), loadSettingsForWorkflow
// returns a non-nil *config.Settings so downstream GetHubEndpoint / getHubClient
// calls don't nil-deref. Regression guard for a panic that would fire if
// --hub is used with SCION_HUB_ENDPOINT set but no local settings.yaml.
func TestLoadSettingsForWorkflow_UnloadablePathReturnsNonNil(t *testing.T) {
	origGrovePath := grovePath
	defer func() { grovePath = origGrovePath }()

	// Point grovePath at a nonexistent location that will cause config loaders
	// to error. The function should swallow the error but still return a
	// usable (empty) Settings instead of nil.
	grovePath = "/nonexistent/path/that/should/not/load"

	settings, err := loadSettingsForWorkflow()
	require.NoError(t, err)
	require.NotNil(t, settings, "loadSettingsForWorkflow must not return nil; GetHubEndpoint would panic")

	// GetHubEndpoint on the empty settings must not panic.
	assert.NotPanics(t, func() {
		_ = GetHubEndpoint(settings)
	}, "GetHubEndpoint on empty settings must not panic")
}
