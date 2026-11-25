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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
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

func TestSuspendForceFlag_HumanModeOnly(t *testing.T) {
	f := suspendCmd.Flags().Lookup("force")
	require.NotNil(t, f, "suspend must register --force")
	assert.Equal(t, ModeHuman, flagModeOf(f), "--force must be marked human-mode only")

	// Verify hideOtherModeFlags hides --force in agent mode on a cloned command.
	testRoot := &cobra.Command{Use: "scion"}
	testSuspend := &cobra.Command{Use: "suspend"}
	testSuspend.Flags().Bool("force", false, "Force-suspend agents marked never-suspend")
	markFlagsForMode(testSuspend, ModeHuman, "force")
	testRoot.AddCommand(testSuspend)

	hideOtherModeFlags(testRoot, ModeAgent)
	assert.True(t, testSuspend.Flags().Lookup("force").Hidden, "--force must be hidden in agent mode")

	// Verify checkFlagsForMode refuses --force in agent mode when set.
	require.NoError(t, testSuspend.Flags().Set("force", "true"))
	err := checkFlagsForMode(testSuspend, ModeAgent)
	require.Error(t, err)
	assert.NoError(t, checkFlagsForMode(testSuspend, ModeHuman))
}

func TestSuspendAll_SkipsNeverSuspendAndForceIncludes(t *testing.T) {
	agentsJSON := `{"agents":[
		{"id":"a1","name":"lead","slug":"lead","phase":"running","suspendPolicy":"never"},
		{"id":"a2","name":"worker","slug":"worker","phase":"running","suspendPolicy":"auto"},
		{"id":"a3","name":"idle-stopped","slug":"idle-stopped","phase":"stopped","suspendPolicy":"auto"}
	]}`

	t.Run("without --force skips never-suspend and suspends auto", func(t *testing.T) {
		var mu sync.Mutex
		var suspended []string
		var forceSeen []bool

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
				_, _ = w.Write([]byte(agentsJSON))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/suspend"):
				parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/suspend"), "/")
				name := parts[len(parts)-1]
				body, _ := io.ReadAll(r.Body)
				var req map[string]any
				_ = json.Unmarshal(body, &req)
				f, _ := req["force"].(bool)
				mu.Lock()
				suspended = append(suspended, name)
				forceSeen = append(forceSeen, f)
				mu.Unlock()
				_, _ = w.Write([]byte(`{"id":"` + name + `","name":"` + name + `","phase":"suspended"}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)

		origForce := suspendForce
		origJSON := outputFormat
		suspendForce = false
		outputFormat = ""
		t.Cleanup(func() {
			suspendForce = origForce
			outputFormat = origJSON
		})

		// Capture stderr for statusf output.
		origStderr := os.Stderr
		rPipe, wPipe, err := os.Pipe()
		require.NoError(t, err)
		os.Stderr = wPipe
		t.Cleanup(func() { os.Stderr = origStderr })

		hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "p1"}
		runErr := suspendAllAgentsViaHub(hubCtx)

		_ = wPipe.Close()
		os.Stderr = origStderr
		var out bytes.Buffer
		_, _ = io.Copy(&out, rPipe)
		_ = rPipe.Close()

		require.NoError(t, runErr)
		assert.Equal(t, []string{"worker"}, suspended, "only auto agent should be sent /suspend")
		assert.Equal(t, []bool{false}, forceSeen)
		assert.Contains(t, out.String(), "Agent 'lead': skipped (never-suspend)")
		assert.Contains(t, out.String(), "Agent 'worker' suspended via Hub.")
	})

	t.Run("with --force includes never-suspend and sends force=true", func(t *testing.T) {
		var mu sync.Mutex
		suspended := map[string]bool{}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
				_, _ = w.Write([]byte(agentsJSON))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/suspend"):
				parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/suspend"), "/")
				name := parts[len(parts)-1]
				body, _ := io.ReadAll(r.Body)
				var req map[string]any
				_ = json.Unmarshal(body, &req)
				f, _ := req["force"].(bool)
				mu.Lock()
				suspended[name] = f
				mu.Unlock()
				_, _ = w.Write([]byte(`{"id":"` + name + `","name":"` + name + `","phase":"suspended"}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)

		origForce := suspendForce
		origJSON := outputFormat
		suspendForce = true
		outputFormat = "json"
		t.Cleanup(func() {
			suspendForce = origForce
			outputFormat = origJSON
		})

		origStdout := os.Stdout
		rPipe, wPipe, err := os.Pipe()
		require.NoError(t, err)
		os.Stdout = wPipe
		t.Cleanup(func() { os.Stdout = origStdout })

		hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "p1"}
		runErr := suspendAllAgentsViaHub(hubCtx)

		_ = wPipe.Close()
		os.Stdout = origStdout
		var out bytes.Buffer
		_, _ = io.Copy(&out, rPipe)
		_ = rPipe.Close()

		require.NoError(t, runErr)
		assert.Equal(t, map[string]bool{"lead": true, "worker": true}, suspended)

		var payload struct {
			Status  string                   `json:"status"`
			Command string                   `json:"command"`
			Results []map[string]interface{} `json:"results"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
		assert.Equal(t, "success", payload.Status)
		assert.Len(t, payload.Results, 2)
	})
}

func TestFormatSuspendError_TypedErrors(t *testing.T) {
	neverErr := &apiclient.APIError{
		StatusCode: http.StatusConflict,
		Code:       apiclient.ErrCodeAgentNeverSuspend,
		Message:    `agent "lead" has suspend policy "never"`,
	}

	t.Setenv("SCION_CLI_MODE", "human")
	humanMsg := formatSuspendError("lead", neverErr).Error()
	assert.Contains(t, humanMsg, "never-suspend")
	assert.Contains(t, humanMsg, "--force")

	t.Setenv("SCION_CLI_MODE", "agent")
	agentMsg := formatSuspendError("lead", neverErr).Error()
	assert.Contains(t, agentMsg, "never-suspend")
	assert.NotContains(t, agentMsg, "--force")

	forbidErr := &apiclient.APIError{
		StatusCode: http.StatusForbidden,
		Code:       apiclient.ErrCodeForceSuspendForbidden,
		Message:    "only human principals may force-suspend a never-suspend agent",
	}
	assert.Contains(t, formatSuspendError("lead", forbidErr).Error(), "only human principals")
}
