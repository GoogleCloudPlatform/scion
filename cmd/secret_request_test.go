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

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretRequestCLIModesAndWindowsGate(t *testing.T) {
	for _, path := range []string{"secret.request", "secret.request.list", "secret.request.withdraw"} {
		assert.True(t, agentAllowed[path], "agentAllowed[%q]", path)
		assert.False(t, windowsAllowed[path], "windowsAllowed[%q] should stay false (agent-only command)", path)
	}

	for _, sub := range []string{"list", "withdraw"} {
		_, _, err := rootCmd.Find([]string{"secret", "request", sub})
		assert.NoError(t, err, "secret request %s is registered on rootCmd", sub)
	}

	// Verify agent mode keeps secret.request.{list,withdraw} and removes secret.set.
	root := cloneCommandForModeExec(rootCmd)
	applyModeRestrictions(root, ModeAgent)
	names := collectCommandNames(root)
	assert.Contains(t, names, "secret.request")
	assert.Contains(t, names, "secret.request.list")
	assert.Contains(t, names, "secret.request.withdraw")
	assert.NotContains(t, names, "secret.set")
}

func withMockSecretRequestsServer(t *testing.T, handler http.Handler) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	origFactory := secretRequestsClientFactory
	secretRequestsClientFactory = func() (hubclient.Client, error) {
		return hubclient.New(srv.URL, hubclient.WithAgentToken("test-agent-token"))
	}
	t.Cleanup(func() {
		secretRequestsClientFactory = origFactory
	})
}

func runSecretRequestTestCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	prevFormat := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prevFormat })

	root := &cobra.Command{Use: "scion", SilenceUsage: true, SilenceErrors: true}
	sec := &cobra.Command{Use: "secret"}
	sec.AddCommand(newSecretRequestCmd())
	root.AddCommand(sec)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestSecretRequestList_FiltersPaginationAndTable(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", string(ModeAgent))
	fixedNow := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	var seenStates []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/secret-requests", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		seenStates = append(seenStates, state)
		w.Header().Set("Content-Type", "application/json")
		if state == "expired" {
			_ = json.NewEncoder(w).Encode(messages.AgentSecretRequestListResponse{Items: []messages.SecretRequestView{}})
			return
		}
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(messages.AgentSecretRequestListResponse{
				Items: []messages.SecretRequestView{
					{
						RowID:      "row-1",
						MessageID:  "msg-1",
						Key:        "GITHUB_TOKEN",
						Version:    2,
						SecretType: messages.SecretRequestTypeEnvironment,
						Status:     "fulfilled",
						CreatedAt:  fixedNow.Add(-20 * time.Minute),
						Fulfilled: &messages.SecretRequestFulfilled{
							At:            fixedNow.Add(-10 * time.Minute),
							ByName:        "Alice",
							SecretVersion: 1,
						},
					},
					{
						RowID:      "row-2",
						MessageID:  "msg-2",
						Key:        "DEPLOY_KEY",
						Version:    1,
						SecretType: messages.SecretRequestTypeFile,
						Status:     "dismissed",
						CreatedAt:  fixedNow.Add(-15 * time.Minute),
						Dismissed: &messages.SecretRequestDismissed{
							At:     fixedNow.Add(-5 * time.Minute),
							ByName: "Bob",
							Reason: "use the shared bot token",
						},
					},
				},
				NextCursor: "page-2",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(messages.AgentSecretRequestListResponse{
			Items: []messages.SecretRequestView{
				{
					RowID:       "row-3",
					MessageID:   "msg-3",
					Key:         "NPM_TOKEN",
					Version:     1,
					SecretType:  messages.SecretRequestTypeEnvironment,
					Status:      "pending",
					Description: "Token for publishing",
					CreatedAt:   fixedNow.Add(-2 * time.Minute),
				},
			},
		})
	})
	withMockSecretRequestsServer(t, mux)

	// Default is pending, paginates across 2 pages.
	out, err := runSecretRequestTestCmd(t, "secret", "request", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "GITHUB_TOKEN")
	assert.Contains(t, out, "Alice")
	assert.Contains(t, out, "DEPLOY_KEY")
	assert.Contains(t, out, "Bob")
	assert.Contains(t, out, "use the shared bot token")
	assert.Contains(t, out, "NPM_TOKEN")
	assert.Contains(t, out, "msg-1")

	// Empty state message.
	outEmpty, err := runSecretRequestTestCmd(t, "secret", "request", "list", "--expired")
	require.NoError(t, err)
	assert.Equal(t, "No expired secret requests.\n", outEmpty)

	// JSON output.
	outJSON, err := runSecretRequestTestCmd(t, "secret", "request", "list", "--all", "--json")
	require.NoError(t, err)
	var resp messages.AgentSecretRequestListResponse
	require.NoError(t, json.Unmarshal([]byte(outJSON), &resp))
	require.Len(t, resp.Items, 3)

	// Every individual filter flag passes the expected state query param.
	for _, tc := range []struct {
		flag  string
		state string
	}{
		{"--pending", "pending"},
		{"--fulfilled", "fulfilled"},
		{"--withdrawn", "withdrawn"},
		{"--dismissed", "dismissed"},
		{"--expired", "expired"},
		{"--all", "all"},
	} {
		seenStates = nil
		_, err := runSecretRequestTestCmd(t, "secret", "request", "list", tc.flag)
		require.NoError(t, err, tc.flag)
		require.NotEmpty(t, seenStates)
		assert.Equal(t, tc.state, seenStates[0], tc.flag)
	}

	// Conflicting filter flags rejected client-side.
	_, err = runSecretRequestTestCmd(t, "secret", "request", "list", "--pending", "--all")
	require.Error(t, err)
	assert.True(t, isUsageError(err))
}

func TestSecretRequestList_HumanModeRefusedAndExperimentOff(t *testing.T) {
	t.Run("human mode refused", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", string(ModeHuman))
		_, err := runSecretRequestTestCmd(t, "secret", "request", "list")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent mode")
	})

	t.Run("experiment off 404", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", string(ModeAgent))
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/agent/secret-requests", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"route not found"}}`))
		})
		withMockSecretRequestsServer(t, mux)

		_, err := runSecretRequestTestCmd(t, "secret", "request", "list")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hub.agent_secrets")
	})
}

func TestSecretRequestWithdraw_SuccessAlreadyWithdrawnAndFailures(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", string(ModeAgent))

	var lastReq messages.WithdrawSecretRequestsRequest
	mode := "ok"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/secret-requests/withdraw", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&lastReq))
		w.Header().Set("Content-Type", "application/json")
		switch mode {
		case "ok":
			_ = json.NewEncoder(w).Encode(messages.WithdrawSecretRequestsResponse{
				Withdrawn: []messages.SecretRequestView{
					{Key: "GITHUB_TOKEN", Version: 2, Status: "withdrawn"},
					{Key: "SIBLING_KEY", Version: 1, Status: "withdrawn"},
				},
				AlreadyWithdrawn: []messages.SecretRequestView{
					{Key: "OLD_KEY", Version: 1, Status: "withdrawn"},
				},
			})
		case "conflict_messages":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"withdraw_failed","message":"cannot withdraw","details":{"failures":[` +
				`{"key":"GITHUB_TOKEN","reason":"already_fulfilled","message":"GITHUB_TOKEN: already fulfilled by Alice"},` +
				`{"key":"MISSING_KEY","reason":"not_found","message":"MISSING_KEY: secret request not found"}` +
				`]}}}`))
		case "conflict_structured":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"withdraw_failed","message":"cannot withdraw","details":{"failures":[` +
				`{"key":"K1","reason":"already_fulfilled","secretRequest":{"fulfilled":{"byName":"Alice"}}},` +
				`{"key":"K2","reason":"dismissed","secretRequest":{"dismissed":{"byName":"Bob"}}},` +
				`{"key":"K3","reason":"expired"},` +
				`{"key":"K4","reason":"submitting"},` +
				`{"key":"K5","reason":"not_found"}` +
				`]}}}`))
		case "exp_off":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"route not found"}}`))
		}
	})
	withMockSecretRequestsServer(t, mux)

	// Success with --set and --reason, including alreadyWithdrawn + sibling ordering.
	out, err := runSecretRequestTestCmd(t, "secret", "request", "withdraw", "GITHUB_TOKEN", "OLD_KEY", "--set", "--reason", "no longer needed")
	require.NoError(t, err)
	assert.Equal(t, []string{"GITHUB_TOKEN", "OLD_KEY"}, lastReq.Keys)
	assert.True(t, lastReq.WholeSet)
	assert.Equal(t, "no longer needed", lastReq.Reason)
	assert.Equal(t, "GITHUB_TOKEN v2 withdrawn\nOLD_KEY v1 already withdrawn\nSIBLING_KEY v1 withdrawn\n", out)

	// JSON output.
	outJSON, err := runSecretRequestTestCmd(t, "secret", "request", "withdraw", "GITHUB_TOKEN", "--json")
	require.NoError(t, err)
	var resp messages.WithdrawSecretRequestsResponse
	require.NoError(t, json.Unmarshal([]byte(outJSON), &resp))
	require.Len(t, resp.Withdrawn, 2)
	require.Len(t, resp.AlreadyWithdrawn, 1)

	// 409 withdraw_failed with per-key messages.
	mode = "conflict_messages"
	_, err = runSecretRequestTestCmd(t, "secret", "request", "withdraw", "GITHUB_TOKEN", "MISSING_KEY")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GITHUB_TOKEN: already fulfilled by Alice")
	assert.Contains(t, err.Error(), "MISSING_KEY: secret request not found")

	// 409 withdraw_failed with structured reason fallback (all 5 reasons).
	mode = "conflict_structured"
	_, err = runSecretRequestTestCmd(t, "secret", "request", "withdraw", "K1", "K2", "K3", "K4", "K5")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "K1: already fulfilled by Alice")
	assert.Contains(t, err.Error(), "K2: dismissed by Bob")
	assert.Contains(t, err.Error(), "K3: expired")
	assert.Contains(t, err.Error(), "K4: currently being fulfilled")
	assert.Contains(t, err.Error(), "K5: secret request not found")

	// Experiment off 404.
	mode = "exp_off"
	_, err = runSecretRequestTestCmd(t, "secret", "request", "withdraw", "GITHUB_TOKEN")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hub.agent_secrets")

	// Client-side validation: missing args, duplicate keys (case-insensitive), too many keys, long reason, human mode.
	_, err = runSecretRequestTestCmd(t, "secret", "request", "withdraw")
	require.Error(t, err)
	assert.True(t, isUsageError(err))

	_, err = runSecretRequestTestCmd(t, "secret", "request", "withdraw", "GITHUB_TOKEN", "github_token")
	require.Error(t, err)
	assert.True(t, isUsageError(err))

	_, err = runSecretRequestTestCmd(t, "secret", "request", "withdraw", "GITHUB_TOKEN", "--reason", strings.Repeat("a", msgaction.MaxReasonRunes+1))
	require.Error(t, err)
	assert.True(t, isUsageError(err))

	t.Setenv("SCION_CLI_MODE", string(ModeHuman))
	_, err = runSecretRequestTestCmd(t, "secret", "request", "withdraw", "GITHUB_TOKEN")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent mode")
}
