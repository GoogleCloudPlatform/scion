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
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartHub400OnCreate_NoUsage is the regression test for a ptone/scion#2859
// repro: `scion start <name>` printed the start command's Usage block after
// the hub rejected the create with a 400. A hub rejection is a runtime
// failure, so Execute must print the hub's error and no usage.
func TestStartHub400OnCreate_NoUsage(t *testing.T) {
	saveCLIModeState(t)
	restoreAllSilenceUsage(t)
	origProjectPath, origNoHub := projectPath, noHub
	t.Cleanup(func() {
		projectPath, noHub = origProjectPath, origNoHub
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	var mu sync.Mutex
	var creates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		t.Logf("hub request: %s %s", r.Method, r.URL.Path)
		switch {
		case r.URL.Path == "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agents"):
			mu.Lock()
			creates++
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{
				"code":    "validation_error",
				"message": "invalid agent configuration: harness is required",
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{
				"code": "not_found", "message": "not found",
			}})
		}
	}))
	t.Cleanup(server.Close)

	hermeticCLIEnv(t)
	tmpHome := os.Getenv("HOME")
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	resetCommandFlags(t, startCmd)
	t.Cleanup(func() { resetCommandFlags(t, startCmd) })
	projectPath = setupEnvProject(t, tmpHome, server.URL)
	noHub = false

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"start", "a1"})
	var cmd *cobra.Command
	var err error
	_, _ = captureStdIO(t, func() { cmd, err = rootCmd.ExecuteC() })

	require.Error(t, err)
	assert.Equal(t, startCmd, cmd)
	mu.Lock()
	assert.Equal(t, 1, creates, "the error must come from the hub's create response")
	mu.Unlock()
	assert.Contains(t, err.Error(), "invalid agent configuration: harness is required", "the hub's error is reported")
	assert.False(t, isUsageError(err), "a hub rejection must not be marked as a usage error")
	assert.False(t, showUsageForError(cmd, err, true), "no Usage block after a hub 400")
}
