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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const twoTestSecretRequests = `[{"key":"DEPLOY_KEY","description":"SSH key for staging","type":"file","target":"~/.ssh/id_ed25519"},{"key":"GITHUB_TOKEN","description":"PAT for release"}]`

func secretRequestsState(t *testing.T, sender string) {
	t.Helper()
	orig := saveMessageTestState()
	restoreFlags := resetMessageFlags()
	origFormat, origStdin := outputFormat, secretRequestsStdin
	outputFormat = ""
	t.Cleanup(func() {
		restoreFlags()
		orig.restore()
		outputFormat, secretRequestsStdin = origFormat, origStdin
	})
	t.Setenv("SCION_AGENT_NAME", sender)
	t.Setenv("SCION_CLI_MODE", string(ModeAgent))
}

func secretRequestsSentBody() map[string]interface{} {
	return map[string]interface{}{
		"message_id":   "msg-secret-req-1",
		"status":       "sent",
		"recipient":    "user:alice@example.com",
		"recipient_id": "uid-alice",
		"secret_requests": []map[string]interface{}{
			{"key": "DEPLOY_KEY", "version": 1, "row_id": "row-1", "replaced": nil},
			{"key": "GITHUB_TOKEN", "version": 2, "row_id": "row-2", "replaced": map[string]interface{}{"version": 1, "status": "pending"}},
		},
	}
}

func TestValidateSecretRequestFlags(t *testing.T) {
	cases := []struct {
		name      string
		setup     func()
		agent     bool
		group     bool
		noAgent   bool
		human     bool
		wantErr   string
		wantUsage bool
	}{
		{name: "none", setup: func() {}},
		{name: "single key to user", setup: func() { msgSecretRequests = []string{"GITHUB_TOKEN"}; msgSecretDescription = "PAT" }},
		{name: "repeatable keys without description", setup: func() { msgSecretRequests = []string{"K1", "K2"} }},
		{name: "file to user", setup: func() { msgSecretRequestFile = "req.json" }},
		{name: "update with secret-request", setup: func() { msgSecretRequests = []string{"GITHUB_TOKEN"}; msgDecisionsUpdate = true }},
		{name: "description without secret-request", setup: func() { msgSecretDescription = "PAT" }, wantErr: "--secret-description is valid only with --secret-request", wantUsage: true},
		{name: "description with file", setup: func() { msgSecretRequestFile = "req.json"; msgSecretDescription = "PAT" }, wantErr: "--secret-description is valid only with --secret-request, not --secret-request-file", wantUsage: true},
		{name: "description with multiple keys", setup: func() { msgSecretRequests = []string{"K1", "K2"}; msgSecretDescription = "PAT" }, wantErr: "only one --secret-request", wantUsage: true},
		{name: "key and file mutually exclusive", setup: func() { msgSecretRequests = []string{"K1"}; msgSecretRequestFile = "req.json" }, wantErr: "mutually exclusive", wantUsage: true},
		{name: "combined with decisions", setup: func() { msgSecretRequests = []string{"K1"}; msgDecisions = "[]" }, wantErr: "cannot be combined with --decisions", wantUsage: true},
		{name: "two stdin readers", setup: func() { msgSecretRequestFile = "-"; msgBodyFile = "-" }, wantErr: "both read stdin", wantUsage: true},
		{name: "agent recipient", setup: func() { msgSecretRequests = []string{"K1"} }, agent: true, wantErr: "not to an agent", wantUsage: false},
		{name: "group recipient", setup: func() { msgSecretRequests = []string{"K1"} }, group: true, wantErr: "not to an agent or a group", wantUsage: false},
		{name: "scheduled", setup: func() { msgSecretRequests = []string{"K1"}; msgIn = "5m" }, wantErr: "--in or --at", wantUsage: true},
		{name: "attach", setup: func() { msgSecretRequests = []string{"K1"}; msgAttach = []string{"f"} }, wantErr: "--attach", wantUsage: true},
		{name: "artifact", setup: func() { msgSecretRequests = []string{"K1"}; msgArtifacts = []string{"scion://artifact/a"} }, wantErr: "--artifact", wantUsage: true},
		{name: "wake", setup: func() { msgSecretRequests = []string{"K1"}; msgWake = true }, wantErr: "--wake", wantUsage: true},
		{name: "cc", setup: func() { msgSecretRequests = []string{"K1"}; msgCC = []string{"agent:x"} }, wantErr: "--cc", wantUsage: true},
		{name: "channel", setup: func() { msgSecretRequests = []string{"K1"}; msgChannel = "web" }, wantErr: "--channel", wantUsage: true},
		{name: "thread-id", setup: func() { msgSecretRequests = []string{"K1"}; msgThreadID = "t1" }, wantErr: "--thread-id", wantUsage: true},
		{name: "no agent name", setup: func() { msgSecretRequests = []string{"K1"} }, noAgent: true, wantErr: "only by an agent", wantUsage: false},
		{name: "human mode", setup: func() { msgSecretRequests = []string{"K1"} }, human: true, wantErr: "only by an agent", wantUsage: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := "asker"
			if tc.noAgent {
				sender = ""
			}
			secretRequestsState(t, sender)
			if tc.human {
				t.Setenv("SCION_CLI_MODE", string(ModeHuman))
			}
			origBody := msgBodyFile
			t.Cleanup(func() { msgBodyFile = origBody })
			tc.setup()
			err := validateDecisionFlags(tc.agent, tc.group)
			if err == nil {
				err = validateSecretRequestFlags(tc.agent, tc.group)
			}
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, tc.wantUsage, isUsageError(err), "usage error mismatch for %q", err)
		})
	}
}

func TestLoadSecretRequestsPayload(t *testing.T) {
	secretRequestsState(t, "asker")

	// Single key with description.
	msgSecretRequests = []string{"GITHUB_TOKEN"}
	msgSecretDescription = "PAT for release"
	got, err := loadSecretRequestsPayload()
	require.NoError(t, err)
	assert.JSONEq(t, `[{"key":"GITHUB_TOKEN","description":"PAT for release"}]`, string(got))

	// Repeatable keys without description.
	msgSecretRequests = []string{"KEY_A", "KEY_B"}
	msgSecretDescription = ""
	got, err = loadSecretRequestsPayload()
	require.NoError(t, err)
	assert.JSONEq(t, `[{"key":"KEY_A"},{"key":"KEY_B"}]`, string(got))

	// Empty key rejected.
	msgSecretRequests = []string{"   "}
	_, err = loadSecretRequestsPayload()
	require.Error(t, err)
	assert.True(t, isUsageError(err))

	// From JSON file.
	msgSecretRequests = nil
	path := filepath.Join(t.TempDir(), "req.json")
	require.NoError(t, os.WriteFile(path, []byte(" "+twoTestSecretRequests+"\n"), 0o600))
	msgSecretRequestFile = path
	got, err = loadSecretRequestsPayload()
	require.NoError(t, err)
	assert.JSONEq(t, twoTestSecretRequests, string(got))

	// From stdin (-).
	msgSecretRequestFile = "-"
	secretRequestsStdin = strings.NewReader(twoTestSecretRequests)
	got, err = loadSecretRequestsPayload()
	require.NoError(t, err)
	assert.JSONEq(t, twoTestSecretRequests, string(got))

	// Invalid JSON and empty array.
	secretRequestsStdin = strings.NewReader("not json")
	_, err = loadSecretRequestsPayload()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JSON array")

	secretRequestsStdin = strings.NewReader("[]")
	_, err = loadSecretRequestsPayload()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one secret request")
}

func TestSecretRequestSendLine(t *testing.T) {
	assert.Equal(t, "DEPLOY_KEY v1 (new)", secretRequestSendLine(messages.SecretRequestSendResult{
		Key: "DEPLOY_KEY", Version: 1,
	}))
	assert.Equal(t, "GITHUB_TOKEN v2 (replaced v1)", secretRequestSendLine(messages.SecretRequestSendResult{
		Key: "GITHUB_TOKEN", Version: 2, Replaced: &messages.SecretRequestReplaced{Version: 1, Status: "pending"},
	}))
}

func TestSendOutboundSecretRequests_UserRecipient(t *testing.T) {
	secretRequestsState(t, "asker")
	msgSecretRequestsPayload = json.RawMessage(twoTestSecretRequests)
	msgDecisionsUpdate = true
	var got map[string]interface{}
	hubCtx := newDecisionsHub(t, http.StatusOK, secretRequestsSentBody(), &got)

	var err error
	out := captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "Need credentials", false) })
	require.NoError(t, err)

	assert.Equal(t, true, got["secret_requests_update"])
	assert.NotContains(t, got, "type", "the hub sets a secret-request prompt's type")
	assert.NotContains(t, got, "decisions")
	raw, _ := json.Marshal(got["secret_requests"])
	assert.JSONEq(t, twoTestSecretRequests, string(raw))

	assert.Contains(t, out, "Message sent to user:alice@example.com via Hub (message msg-secret-req-1).")
	assert.Contains(t, out, "DEPLOY_KEY v1 (new)\n")
	assert.Equal(t, "GITHUB_TOKEN v2 (replaced v1)", lastLine(out))
}

func TestSendOutboundSecretRequests_ConversationJSON(t *testing.T) {
	secretRequestsState(t, "asker")
	outputFormat = "json"
	msgSecretRequestsPayload = json.RawMessage(twoTestSecretRequests)
	var got map[string]interface{}
	hubCtx := newDecisionsHub(t, http.StatusOK, secretRequestsSentBody(), &got)

	ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "11111111-1111-1111-1111-111111111111", Raw: "conv:11111111-1111-1111-1111-111111111111"}
	var err error
	out := captureStdout(t, func() { err = sendMessageViaConversation(hubCtx, ref, "", false, false, nil) })
	require.NoError(t, err)
	assert.Contains(t, got, "secret_requests")

	var res hubclient.OutboundMessageResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "msg-secret-req-1", res.MessageID)
	require.Len(t, res.SecretRequests, 2)
	assert.Equal(t, "DEPLOY_KEY", res.SecretRequests[0].Key)
	assert.Equal(t, "GITHUB_TOKEN", res.SecretRequests[1].Key)
	require.NotNil(t, res.SecretRequests[1].Replaced)
	assert.Equal(t, 1, res.SecretRequests[1].Replaced.Version)
}

func TestSendOutboundSecretRequests_ConflictPendingHint(t *testing.T) {
	t.Run("hub message includes hint", func(t *testing.T) {
		secretRequestsState(t, "asker")
		msgSecretRequestsPayload = json.RawMessage(`[{"key":"GITHUB_TOKEN"}]`)
		hubCtx := newDecisionsHub(t, http.StatusConflict, map[string]interface{}{
			"error": map[string]interface{}{
				"code":    msgaction.ConflictSecretRequestKeyPending,
				"message": "GITHUB_TOKEN v1 is already pending (message msg-old-1); pass --update to replace it, or scion secret request withdraw GITHUB_TOKEN",
				"details": map[string]interface{}{
					"existing": []map[string]interface{}{
						{"key": "GITHUB_TOKEN", "version": 1, "messageId": "msg-old-1", "status": "pending"},
					},
				},
			},
		}, nil)

		var err error
		_ = captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "", false) })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "GITHUB_TOKEN v1")
		assert.Contains(t, err.Error(), "pass --update to replace it")
		assert.Contains(t, err.Error(), "scion secret request withdraw GITHUB_TOKEN")
	})

	t.Run("structured details.existing fallback", func(t *testing.T) {
		secretRequestsState(t, "asker")
		msgSecretRequestsPayload = json.RawMessage(`[{"key":"GITHUB_TOKEN"}]`)
		hubCtx := newDecisionsHub(t, http.StatusConflict, map[string]interface{}{
			"error": map[string]interface{}{
				"code":    msgaction.ConflictSecretRequestKeyPending,
				"message": "secret request key already pending",
				"details": map[string]interface{}{
					"existing": []map[string]interface{}{
						{"key": "GITHUB_TOKEN", "version": 2, "messageId": "msg-old-2", "status": "pending"},
					},
				},
			},
		}, nil)

		var err error
		_ = captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "", false) })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "GITHUB_TOKEN v2 is already pending (message msg-old-2)")
		assert.Contains(t, err.Error(), "pass --update to replace it, or scion secret request withdraw GITHUB_TOKEN")
	})
}

func TestSendOutboundSecretRequests_ExperimentDisabled(t *testing.T) {
	secretRequestsState(t, "asker")
	msgSecretRequestsPayload = json.RawMessage(`[{"key":"GITHUB_TOKEN"}]`)
	hubCtx := newDecisionsHub(t, http.StatusBadRequest, map[string]interface{}{
		"error": map[string]interface{}{
			"code":    "experiment_disabled",
			"message": "experiment not enabled: hub.agent_secrets is off",
		},
	}, nil)

	var err error
	_ = captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "", false) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "experiment not enabled")
	assert.Contains(t, err.Error(), "hub.agent_secrets")
}

func TestMessageSecretRequest_RootCmdArgsAndBroadcastRefusal(t *testing.T) {
	// Body is optional when --secret-request or --secret-request-file is set.
	t.Run("args accepts recipient only with secret-request", func(t *testing.T) {
		secretRequestsState(t, "asker")
		require.NoError(t, messageCmd.Flags().Set("secret-request", "GITHUB_TOKEN"))
		require.NoError(t, messageCmd.Args(messageCmd, []string{"user:alice@example.com"}))
	})

	t.Run("args accepts recipient only with secret-request-file", func(t *testing.T) {
		secretRequestsState(t, "asker")
		require.NoError(t, messageCmd.Flags().Set("secret-request-file", "req.json"))
		require.NoError(t, messageCmd.Args(messageCmd, []string{"conv:11111111-1111-1111-1111-111111111111"}))
	})

	t.Run("broadcast flag refused with secret-request", func(t *testing.T) {
		secretRequestsState(t, "asker")
		require.NoError(t, messageCmd.Flags().Set("broadcast", "true"))
		require.NoError(t, messageCmd.Flags().Set("secret-request", "GITHUB_TOKEN"))
		err := messageCmd.RunE(messageCmd, []string{"user:alice@example.com"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--broadcast has been removed")
	})

	t.Run("all flag refused with secret-request", func(t *testing.T) {
		secretRequestsState(t, "asker")
		require.NoError(t, messageCmd.Flags().Set("all", "true"))
		require.NoError(t, messageCmd.Flags().Set("secret-request", "GITHUB_TOKEN"))
		err := messageCmd.RunE(messageCmd, []string{"user:alice@example.com"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--all has been removed")
	})
}
