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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestValidateCommandRequestFlags(t *testing.T) {
	t.Cleanup(resetCommandRequestFlags)
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_AGENT_NAME", "builder")

	t.Run("auxiliary flags without --command-request refused", func(t *testing.T) {
		resetCommandRequestFlags()
		msgCommandInline = "echo hi"
		err := validateCommandRequestFlags(false, false, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--command-request")
	})

	t.Run("requires --command or --command-file", func(t *testing.T) {
		resetCommandRequestFlags()
		msgCommandRequestKey = "deploy"
		err := validateCommandRequestFlags(false, false, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--command or --command-file")
	})

	t.Run("mutual exclusion --command and --command-file", func(t *testing.T) {
		resetCommandRequestFlags()
		msgCommandRequestKey = "deploy"
		msgCommandInline = "echo hi"
		msgCommandFile = "script.sh"
		err := validateCommandRequestFlags(false, false, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("refuses conversation target in P1", func(t *testing.T) {
		resetCommandRequestFlags()
		msgCommandRequestKey = "deploy"
		msgCommandInline = "echo hi"
		err := validateCommandRequestFlags(false, false, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "direct message to a user")
	})

	t.Run("valid DM command request loads payload", func(t *testing.T) {
		resetCommandRequestFlags()
		msgCommandRequestKey = "deploy-preview"
		msgCommandInline = "gcloud config list"
		msgCommandDescription = "List gcloud config"
		msgCommandTimeout = "5m"
		require.NoError(t, validateCommandRequestFlags(false, false, false))
		raw, err := loadCommandRequestPayload()
		require.NoError(t, err)
		var decoded messages.CommandRequestInput
		require.NoError(t, json.Unmarshal(raw, &decoded))
		assert.Equal(t, "deploy-preview", decoded.Key)
		assert.Equal(t, "gcloud config list", decoded.Script)
		assert.Equal(t, 300, decoded.TimeoutS)
	})

	t.Run("send line prints key, version, and row id", func(t *testing.T) {
		line := commandRequestSendLine(&messages.CommandRequestSendResult{
			Key:     "deploy",
			Version: 2,
			RowID:   "row-uuid-2",
			Replaced: &messages.CommandRequestReplaced{
				Version: 1,
				Status:  "replaced",
			},
		})
		assert.Contains(t, line, "deploy")
		assert.Contains(t, line, "v2")
		assert.Contains(t, line, "row-uuid-2")
		assert.Contains(t, line, "replaced v1")
	})
}

func TestCommandRequestCLI_ListAndApprove(t *testing.T) {
	hash := messages.CommandRequestCanonicalHash("gcloud config list", "", 600)
	now := time.Now().UTC()
	var approvedReq messages.ApproveCommandRequestBody

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/command-requests":
			_ = json.NewEncoder(w).Encode(messages.AgentCommandRequestListResponse{
				Items: []messages.CommandRequestView{
					{
						RowID:         "req-1",
						Key:           "gcloud-list",
						Version:       1,
						Status:        "approved",
						CommandSHA256: hash,
						CreatedAt:     now,
						Run: &messages.CommandRequestRunView{
							RunID:  "run-1",
							Status: "succeeded",
						},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/command-requests/req-1":
			_ = json.NewEncoder(w).Encode(messages.CommandRequestDetailResponse{
				Request: messages.CommandRequestView{
					RowID:         "req-1",
					AgentName:     "builder",
					Key:           "gcloud-list",
					Version:       1,
					Description:   "Check active config",
					Script:        "gcloud config list",
					TimeoutS:      600,
					CommandSHA256: hash,
					Status:        "pending",
					CreatedAt:     now,
					ExpiresAt:     now.Add(24 * time.Hour),
				},
				ExecContainer: &messages.CommandRequestExecContainerView{
					State:       "ready",
					AgentName:   "my-shell",
					ProjectName: "ops",
				},
				CanApprove: true,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/command-requests/req-1/approve":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&approvedReq))
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(messages.ApproveCommandRequestResponse{
				Request: messages.CommandRequestView{
					RowID:   "req-1",
					Key:     "gcloud-list",
					Version: 1,
					Status:  "approved",
				},
				Run: messages.CommandRequestRunView{
					RunID:     "run-1",
					RequestID: "req-1",
					Status:    "queued",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	prevFactory := commandRequestsClientFactory
	commandRequestsClientFactory = func() (hubclient.Client, error) {
		return hubclient.New(ts.URL)
	}
	t.Cleanup(func() { commandRequestsClientFactory = prevFactory })

	t.Run("agent list works in agent mode", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "agent")
		var out bytes.Buffer
		cmd := newCommandRequestListCmd()
		cmd.SetOut(&out)
		require.NoError(t, runCommandRequestList(cmd, nil))
		assert.Contains(t, out.String(), "gcloud-list")
		assert.Contains(t, out.String(), "succeeded")
	})

	t.Run("approve refused in agent mode (S1)", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "agent")
		cmd := newCommandRequestApproveCmd()
		err := runCommandRequestApprove(cmd, []string{"req-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "human users")
	})

	t.Run("agentAllowed excludes command.request.approve and windowsAllowed excludes command", func(t *testing.T) {
		assert.True(t, agentAllowed["command"])
		assert.True(t, agentAllowed["command.request"])
		assert.True(t, agentAllowed["command.request.list"])
		assert.False(t, agentAllowed["command.request.approve"])
		assert.False(t, windowsAllowed["command"])
		assert.False(t, windowsAllowed["command.request.approve"])
	})

	t.Run("non-TTY approve requires matching --confirm-sha256", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "human")
		prevTTY := commandApproveIsTerminal
		commandApproveIsTerminal = func() bool { return false }
		t.Cleanup(func() { commandApproveIsTerminal = prevTTY })

		cmd := newCommandRequestApproveCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		err := runCommandRequestApprove(cmd, []string{"req-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--confirm-sha256")

		require.NoError(t, cmd.Flags().Set("confirm-sha256", "0000000000000000000000000000000000000000000000000000000000000000"))
		err = runCommandRequestApprove(cmd, []string{"req-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match")

		require.NoError(t, cmd.Flags().Set("confirm-sha256", hash))
		require.NoError(t, runCommandRequestApprove(cmd, []string{"req-1"}))
		assert.Equal(t, hash, approvedReq.CommandSHA256)
		assert.Equal(t, "cli", approvedReq.Surface)
		assert.Contains(t, out.String(), "Approved gcloud-list v1")
	})

	t.Run("TTY approve prompts [y/N]", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "human")
		prevTTY := commandApproveIsTerminal
		prevStdin := commandApproveStdin
		commandApproveIsTerminal = func() bool { return true }
		t.Cleanup(func() {
			commandApproveIsTerminal = prevTTY
			commandApproveStdin = prevStdin
		})

		commandApproveStdin = strings.NewReader("n\n")
		cmd := newCommandRequestApproveCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		err := runCommandRequestApprove(cmd, []string{"req-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cancelled")

		commandApproveStdin = strings.NewReader("y\n")
		out.Reset()
		require.NoError(t, runCommandRequestApprove(cmd, []string{"req-1"}))
		assert.Contains(t, out.String(), "Run this in my-shell (ops)? [y/N]")
		assert.Contains(t, out.String(), "Approved gcloud-list v1")
	})
}
