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
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// namesLineAB is the stderr line that names OPTIN_KEY_A and OPTIN_KEY_B.
const namesLineAB = "Sending 2 environment value(s) to the Hub: OPTIN_KEY_A, OPTIN_KEY_B\n"

// foundListAB is the text-mode list of the keys found locally, with its
// key lines sorted (they print in map order; see sortFoundList).
const foundListAB = "\nRequired variables for this agent were not provided in the template, hub, or broker, but are available in our current environment:\n" +
	"  OPTIN_KEY_A — found in local environment\n" +
	"  OPTIN_KEY_B — found in local environment\n"

const sendPromptAB = "\nSend 2 environment value(s) to use in provisioning this agent? (will not be stored) [Y/n]: "

// sortFoundList sorts each run of "found in local environment" lines,
// which print in map order, and leaves every other line in place, so the
// order of the list, the prompt, the names line and the "Submitting"
// line is part of the comparison.
func sortFoundList(s string) string {
	lines := strings.Split(s, "\n")
	isFound := func(l string) bool { return strings.HasSuffix(l, " — found in local environment") }
	for i := 0; i < len(lines); {
		if !isFound(lines[i]) {
			i++
			continue
		}
		j := i
		for j < len(lines) && isFound(lines[j]) {
			j++
		}
		slices.Sort(lines[i:j])
		i = j
	}
	return strings.Join(lines, "\n")
}

// Whenever the confirmation prompt does not show the keys (--yes,
// --format json, a non-interactive send with --send-env), the CLI names
// them on stderr, sorted, and never prints a value. The text-mode prompt
// shows them once, in the list above it.
func TestGatherAndSubmitEnv_NamesSentKeys(t *testing.T) {
	cases := []struct {
		name           string
		terminal       bool
		nonInteractive bool
		autoConfirm    bool
		sendEnv        bool
		json           bool
		stdin          string
		noEnv          bool // the Hub has every key: nothing is gathered
		wantStderr     string
	}{
		{
			name: "interactive prompt shows the list once", terminal: true, stdin: "y\n",
			wantStderr: foundListAB + sendPromptAB + "Submitting 2 gathered environment value(s)...\n",
		},
		{
			name: "--yes on a terminal names the keys", terminal: true, autoConfirm: true,
			wantStderr: foundListAB + namesLineAB,
		},
		{
			name: "--yes with --format json names the keys", terminal: true, autoConfirm: true, json: true,
			wantStderr: namesLineAB,
		},
		{
			name: "--format json prompt names the keys", terminal: true, json: true, stdin: "y\n",
			wantStderr: sendPromptAB + namesLineAB,
		},
		{
			name: "non-interactive with --send-env names the keys", nonInteractive: true, autoConfirm: true, sendEnv: true,
			wantStderr: foundListAB + namesLineAB,
		},
		{
			name: "no terminal with --send-env names the keys", sendEnv: true,
			wantStderr: foundListAB + namesLineAB,
		},
		{
			name: "non-interactive --send-env with --format json names the keys", nonInteractive: true, autoConfirm: true, sendEnv: true, json: true,
			wantStderr: namesLineAB,
		},
		{
			name: "no env gathered prints no names", terminal: true, autoConfirm: true, noEnv: true,
			wantStderr: "\nEnvironment variables already provided:\n  OPTIN_KEY_A — provided by Hub (project)\n",
		},
		{
			name: "no env gathered with --format json prints nothing", nonInteractive: true, autoConfirm: true, sendEnv: true, json: true, noEnv: true,
			wantStderr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origNonInteractive, origAutoConfirm, origSendEnv := nonInteractive, autoConfirm, sendEnvFlag
			origIsTerminal, origOutputFormat := isInteractiveTerminal, outputFormat
			t.Cleanup(func() {
				nonInteractive, autoConfirm, sendEnvFlag = origNonInteractive, origAutoConfirm, origSendEnv
				isInteractiveTerminal, outputFormat = origIsTerminal, origOutputFormat
			})
			nonInteractive, autoConfirm, sendEnvFlag = tc.nonInteractive, tc.autoConfirm, tc.sendEnv
			terminal := tc.terminal
			isInteractiveTerminal = func() bool { return terminal }
			outputFormat = ""
			if tc.json {
				outputFormat = "json"
			}
			if tc.stdin != "" {
				withStdin(t, tc.stdin)
			}
			t.Setenv("OPTIN_KEY_B", optInValueB)
			t.Setenv("OPTIN_KEY_A", optInValueA)

			projectID := tid("project-names")
			server, captured := newEnvGatherMockHubServer(t, projectID)
			defer server.Close()
			client, err := hubclient.New(server.URL)
			require.NoError(t, err)
			hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}
			gather := &hubclient.EnvGatherResponse{
				AgentID:  tid("agent-names"),
				Required: []string{"OPTIN_KEY_B", "OPTIN_KEY_A"},
				Needs:    []string{"OPTIN_KEY_B", "OPTIN_KEY_A"},
			}
			if tc.noEnv {
				gather.Required = []string{"OPTIN_KEY_A"}
				gather.Needs = nil
				gather.HubHas = []hubclient.EnvSource{{Key: "OPTIN_KEY_A", Scope: "project"}}
			}
			resp := &hubclient.CreateAgentResponse{Agent: &hubclient.Agent{ID: tid("agent-names")}, EnvGather: gather}

			stdout, stderr := captureStdoutStderr(t, func() {
				_, err = gatherAndSubmitEnv(context.Background(), hubCtx, projectID, resp)
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantStderr, sortFoundList(stderr))
			assert.Empty(t, stdout, "the names go to stderr only")
			for _, out := range []string{stdout, stderr} {
				assert.NotContains(t, out, optInValueA)
				assert.NotContains(t, out, optInValueB)
			}
			if tc.noEnv {
				assert.Empty(t, *captured)
				return
			}
			assert.Equal(t, map[string]string{"OPTIN_KEY_A": optInValueA, "OPTIN_KEY_B": optInValueB}, *captured)
		})
	}
}

// envNamesStartJSON is the whole JSON document scion start prints for the
// synchronous start below.
const envNamesStartJSON = `{
  "status": "success",
  "command": "start",
  "agent": "outcome-agent",
  "message": "Agent 'outcome-agent' started via Hub.",
  "details": {
    "phase": "running",
    "slug": "outcome-agent"
  }
}
`

// A non-interactive scion start with --send-env and --format json names
// the sent key on stderr; the JSON document on stdout is exactly the
// start result, and the value appears nowhere.
func TestStartAgentViaHub_JSONNamesSentKeysOnStderr(t *testing.T) {
	h := &startOutcomeHub{projectID: "p-names", agentID: "agent-outcome"}
	h.submitHandler = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"agent": map[string]interface{}{"id": "agent-outcome", "name": "outcome-agent", "slug": "outcome-agent", "phase": "running"},
		})
	}
	server := h.serve(t)
	defer server.Close()
	hubCtx := setupStartOutcomeTest(t, server.URL, h.projectID)
	t.Setenv("START_OUTCOME_KEY", optInValueA)

	var err error
	stdout, stderr := captureStdoutStderr(t, func() {
		err = startAgentViaHub(nil, hubCtx, "outcome-agent", "", false, nil)
	})
	require.NoError(t, err)
	assert.Equal(t, envNamesStartJSON, stdout)
	assert.Equal(t, "Sending 1 environment value(s) to the Hub: START_OUTCOME_KEY\n", stderr)
	for _, out := range []string{stdout, stderr} {
		assert.NotContains(t, out, optInValueA)
	}
}

// The names line lists the keys sorted and never a value.
func TestEnvSendNamesLine(t *testing.T) {
	line := envSendNamesLine(map[string]string{"OPTIN_KEY_B": optInValueB, "OPTIN_KEY_A": optInValueA})
	assert.Equal(t, strings.TrimSuffix(namesLineAB, "\n"), line)
	assert.NotContains(t, line, optInValueA)
	assert.NotContains(t, line, optInValueB)
}

// sortFoundList sorts only the found-list lines; the lines around them
// stay in their printed order.
func TestSortFoundList(t *testing.T) {
	in := "head\n  K_B — found in local environment\n  K_A — found in local environment\nz-last\na-after\n"
	want := "head\n  K_A — found in local environment\n  K_B — found in local environment\nz-last\na-after\n"
	assert.Equal(t, want, sortFoundList(in))
}
