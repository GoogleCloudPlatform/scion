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
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessagesArgsValidators(t *testing.T) {
	noArgCmds := []*cobra.Command{
		messagesCmd,
	}
	for _, cmd := range noArgCmds {
		t.Run(cmd.Name()+"_no_args", func(t *testing.T) {
			require.NotNil(t, cmd.Args, "%s must set Args to reject stray positional arguments", cmd.Name())
			assert.NoError(t, cmd.Args(cmd, []string{}))
			assert.Error(t, cmd.Args(cmd, []string{"bogus"}))
		})
	}

	maxOneArgCmds := []*cobra.Command{
		messagesReadCmd,
	}
	for _, cmd := range maxOneArgCmds {
		t.Run(cmd.Name()+"_max_one_arg", func(t *testing.T) {
			require.NotNil(t, cmd.Args, "%s must set Args to reject extra positional arguments", cmd.Name())
			assert.NoError(t, cmd.Args(cmd, []string{}))
			assert.NoError(t, cmd.Args(cmd, []string{"msg-1"}))
			assert.Error(t, cmd.Args(cmd, []string{"msg-1", "extra"}))
		})
	}
}

// TestMessagesBareAndUnknownSubcommand covers ptone/scion#8847 through
// rootCmd.Execute(): bare "scion messages" (and its aliases) still lists
// messages, valid "scion messages read" invocations still succeed, and unknown
// subcommands or stray positional arguments fail fast without hitting the hub.
func TestMessagesBareAndUnknownSubcommand(t *testing.T) {
	restoreAllSilenceUsage(t)
	origProjectPath := projectPath
	origJSON := messagesJSON
	origShowAll := messagesShowAll
	origReadAll := messagesReadAll
	origAgent := messagesAgent
	origOutputFormat := outputFormat
	defer func() {
		projectPath = origProjectPath
		messagesJSON = origJSON
		messagesShowAll = origShowAll
		messagesReadAll = origReadAll
		messagesAgent = origAgent
		outputFormat = origOutputFormat
	}()
	clearHubContextEnv(t)

	var sawRequest atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest.Store(true)
		switch {
		case r.URL.Path == "/api/v1/messages" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{Items: []store.Message{}})
		case r.URL.Path == "/api/v1/messages/msg-1/read" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/messages/read-all" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_PROJECT_ID", "agent-project-id")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectDir := filepath.Join(tmpHome, "agent-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	projectPath = projectDir
	messagesJSON = false
	messagesShowAll = false
	messagesReadAll = false
	messagesAgent = ""
	outputFormat = ""

	execArgs := func(t *testing.T, args []string) (string, error) {
		t.Helper()
		t.Cleanup(func() {
			resetCommandFlags(t, messagesCmd)
			resetCommandFlags(t, messagesReadCmd)
			messagesJSON = false
			messagesShowAll = false
			messagesReadAll = false
			messagesAgent = ""
			outputFormat = ""
		})
		var buf bytes.Buffer
		rootCmd.SetArgs(args)
		rootCmd.SetOut(&buf)
		rootCmd.SetErr(&buf)
		defer func() {
			rootCmd.SetArgs(nil)
			rootCmd.SetOut(nil)
			rootCmd.SetErr(nil)
		}()
		var execErr error
		stdout := captureStdout(t, func() {
			execErr = rootCmd.Execute()
		})
		return stdout + buf.String(), execErr
	}

	for _, bare := range [][]string{{"messages"}, {"msgs"}} {
		t.Run("bare_"+bare[0], func(t *testing.T) {
			sawRequest.Store(false)
			out, err := execArgs(t, bare)
			require.NoError(t, err)
			assert.True(t, sawRequest.Load(), "bare %v must call the hub list endpoint", bare)
			assert.Contains(t, out, "No messages")
		})
	}

	for _, tc := range []struct {
		name    string
		args    []string
		wantOut string
	}{
		{
			name:    "read_single_id",
			args:    []string{"messages", "read", "msg-1"},
			wantOut: "Message msg-1 marked as read.",
		},
		{
			name:    "read_all",
			args:    []string{"messages", "read", "--all"},
			wantOut: "All messages marked as read.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sawRequest.Store(false)
			out, err := execArgs(t, tc.args)
			require.NoError(t, err)
			assert.True(t, sawRequest.Load(), "%v must call the hub read endpoint", tc.args)
			assert.Contains(t, out, tc.wantOut)
		})
	}

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "unknown_subcommand",
			args:    []string{"messages", "bogus"},
			wantErr: `unknown command "bogus" for "scion messages"`,
		},
		{
			name:    "msgs_alias_unknown_subcommand",
			args:    []string{"msgs", "bogus"},
			wantErr: `unknown command "bogus" for "scion messages"`,
		},
		{
			name:    "read_extra_arg",
			args:    []string{"messages", "read", "msg-1", "extra"},
			wantErr: `accepts at most 1 arg(s), received 2`,
		},
		{
			name:    "read_all_extra_args",
			args:    []string{"messages", "read", "--all", "msg-1", "extra"},
			wantErr: `accepts at most 1 arg(s), received 2`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sawRequest.Store(false)
			_, err := execArgs(t, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.False(t, sawRequest.Load(), "unknown subcommand/arg must fail fast before calling the hub")
		})
	}
}
