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
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allowEnvSend sets --send-env for one test, as a non-interactive caller
// that opts in to sending local environment values would.
func allowEnvSend(t *testing.T) {
	t.Helper()
	orig := sendEnvFlag
	t.Cleanup(func() { sendEnvFlag = orig })
	sendEnvFlag = true
}

// Made-up values: none of them may appear in any error or output.
const (
	optInValueA = "optin-made-up-value-7f3a"
	optInValueB = "optin-made-up-value-c41e"
)

// optInErrAB is the whole opt-in error for OPTIN_KEY_A and OPTIN_KEY_B.
const optInErrAB = "the Hub asked for 2 environment value(s) found in the local environment (OPTIN_KEY_A, OPTIN_KEY_B); a non-interactive start or resume sends local environment values only with --send-env: rerun with --send-env, or store them on the Hub with 'scion hub secret set' or 'scion hub env set'"

func TestGatherAndSubmitEnv_SendEnvOptIn(t *testing.T) {
	cases := []struct {
		name           string
		terminal       bool
		nonInteractive bool // --non-interactive, cli.interactive_disabled or agent mode
		autoConfirm    bool // --yes (implied by nonInteractive)
		sendEnv        bool
		stdin          string // answer to the interactive confirmation
		wantSent       bool
		wantErr        string
	}{
		{name: "no terminal without flag fails", wantErr: optInErrAB},
		{name: "no terminal with --yes without flag fails", autoConfirm: true, wantErr: optInErrAB},
		{name: "non-interactive without flag fails", nonInteractive: true, autoConfirm: true, wantErr: optInErrAB},
		{name: "non-interactive on a terminal without flag fails", terminal: true, nonInteractive: true, autoConfirm: true, wantErr: optInErrAB},
		{name: "no terminal with flag sends", sendEnv: true, wantSent: true},
		{name: "non-interactive with flag sends", nonInteractive: true, autoConfirm: true, sendEnv: true, wantSent: true},
		{name: "interactive confirmed sends", terminal: true, stdin: "y\n", wantSent: true},
		{name: "interactive default answer sends", terminal: true, stdin: "\n", wantSent: true},
		{name: "interactive declined sends nothing", terminal: true, stdin: "n\n", wantErr: "env-gather cancelled by user"},
		{name: "interactive with --yes sends", terminal: true, autoConfirm: true, wantSent: true},
		{name: "interactive with flag confirms", terminal: true, sendEnv: true, stdin: "n\n", wantErr: "env-gather cancelled by user"},
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
			if tc.stdin != "" {
				withStdin(t, tc.stdin)
			}
			t.Setenv("OPTIN_KEY_B", optInValueB)
			t.Setenv("OPTIN_KEY_A", optInValueA)

			projectID := tid("project-optin")
			server, captured := newEnvGatherMockHubServer(t, projectID)
			defer server.Close()
			client, err := hubclient.New(server.URL)
			require.NoError(t, err)
			hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}
			resp := &hubclient.CreateAgentResponse{
				Agent: &hubclient.Agent{ID: tid("agent-optin")},
				EnvGather: &hubclient.EnvGatherResponse{
					AgentID:  tid("agent-optin"),
					Required: []string{"OPTIN_KEY_B", "OPTIN_KEY_A"},
					Needs:    []string{"OPTIN_KEY_B", "OPTIN_KEY_A"},
				},
			}

			var result *hubclient.CreateAgentResponse
			stdout, stderr := captureStdoutStderr(t, func() {
				result, err = gatherAndSubmitEnv(context.Background(), hubCtx, projectID, resp)
			})

			for _, out := range []string{stdout, stderr} {
				assert.NotContains(t, out, optInValueA)
				assert.NotContains(t, out, optInValueB)
			}
			if tc.wantSent {
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, map[string]string{"OPTIN_KEY_A": optInValueA, "OPTIN_KEY_B": optInValueB}, *captured)
				return
			}
			require.EqualError(t, err, tc.wantErr)
			assert.NotContains(t, err.Error(), optInValueA)
			assert.NotContains(t, err.Error(), optInValueB)
			assert.Empty(t, *captured, "nothing may be sent")
		})
	}
}

// The opt-in error names the keys, sorted, and the flag, and never a value.
func TestEnvSendOptInError(t *testing.T) {
	err := envSendOptInError(map[string]string{"OPTIN_KEY_B": optInValueB, "OPTIN_KEY_A": optInValueA})
	require.EqualError(t, err, optInErrAB)
	assert.NotContains(t, err.Error(), optInValueA)
	assert.NotContains(t, err.Error(), optInValueB)
}

// A non-interactive scion start without --send-env sends nothing, removes
// the provisioning agent and names the flag; with it, the value reaches
// the Hub's env submit.
func TestStartAgentViaHub_NonInteractiveSendEnvOptIn(t *testing.T) {
	for _, sendEnv := range []bool{false, true} {
		name := "without flag"
		if sendEnv {
			name = "with flag"
		}
		t.Run(name, func(t *testing.T) {
			var submits atomic.Int32
			var submitted string
			h := &startOutcomeHub{projectID: "p-optin", agentID: "agent-optin", agentAfter: launchingAgent}
			h.submitHandler = func(w http.ResponseWriter, r *http.Request) {
				submits.Add(1)
				var body hubclient.SubmitEnvRequest
				_ = json.NewDecoder(r.Body).Decode(&body)
				submitted = body.Env["START_OUTCOME_KEY"]
				// A definite refusal ends the start once the value has
				// reached the Hub; the launch wait is out of scope here.
				writeAPIError(w, http.StatusBadRequest, "refused")
			}
			server := h.serve(t)
			defer server.Close()
			hubCtx := setupStartOutcomeTest(t, server.URL, h.projectID)
			sendEnvFlag = sendEnv
			t.Setenv("START_OUTCOME_KEY", optInValueA)

			var err error
			stdout, stderr := captureStdoutStderr(t, func() {
				err = startAgentViaHub(nil, hubCtx, "outcome-agent", "", false, nil)
			})
			for _, out := range []string{stdout, stderr} {
				assert.NotContains(t, out, optInValueA)
			}
			require.Error(t, err)
			assert.NotContains(t, err.Error(), optInValueA)
			if sendEnv {
				assert.Equal(t, int32(1), submits.Load())
				assert.Equal(t, optInValueA, submitted)
				assert.NotContains(t, err.Error(), "--send-env")
				return
			}
			assert.EqualError(t, err, "env-gather failed: the Hub asked for 1 environment value(s) found in the local environment (START_OUTCOME_KEY); a non-interactive start or resume sends local environment values only with --send-env: rerun with --send-env, or store them on the Hub with 'scion hub secret set' or 'scion hub env set'")
			assert.Equal(t, int32(0), submits.Load(), "nothing may be sent")
			assert.Equal(t, int32(1), h.deletes.Load(), "the provisioning agent is cleaned up")
		})
	}
}

// scion start and scion resume both reach the env submit, so both take
// the opt-in flag.
func TestSendEnvFlagRegistered(t *testing.T) {
	for _, c := range []struct {
		name string
		f    func(string) bool
	}{
		{"start", func(n string) bool { return startCmd.Flags().Lookup(n) != nil }},
		{"resume", func(n string) bool { return resumeCmd.Flags().Lookup(n) != nil }},
	} {
		assert.True(t, c.f(sendEnvFlagName), "%s has --%s", c.name, sendEnvFlagName)
	}
}
