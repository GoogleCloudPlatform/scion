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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for `scion reincarnate --harness` (ptone/scion#7757).

// setReincarnateHarnessFlag sets --harness and restores it on cleanup.
func setReincarnateHarnessFlag(t *testing.T, v string) {
	t.Helper()
	prev := reincarnateHarness
	t.Cleanup(func() { reincarnateHarness = prev })
	reincarnateHarness = v
}

func TestValidateReincarnateHarnessFlags(t *testing.T) {
	cases := []struct {
		name, harness, auth, broker, wantErr string
	}{
		{name: "unset"},
		{name: "valid", harness: "codex"},
		{name: "valid with none", harness: "codex", auth: "none"},
		{name: "path", harness: "a/b", wantErr: "single harness-config name"},
		{name: "dot dot", harness: "..", wantErr: "single harness-config name"},
		{name: "with broker", harness: "codex", broker: "b2", wantErr: "--broker"},
		{name: "none without harness", auth: "none", wantErr: "only accepted with --harness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReincarnateHarnessFlags(tc.harness, tc.auth, tc.broker)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	// Through validateReincarnatePatchFlags: usage errors, and "none" only
	// with --harness.
	setReincarnatePatchFlags(t, "", "", "", -1, "none", "")
	setReincarnateHarnessFlag(t, "codex")
	assert.NoError(t, validateReincarnatePatchFlags())
	setReincarnateHarnessFlag(t, "")
	err := validateReincarnatePatchFlags()
	require.Error(t, err)
	assert.True(t, isUsageError(err))
}

func TestApplyReincarnatePatchFlags_Harness(t *testing.T) {
	setReincarnatePatchFlags(t, "", "", "", -1, "api-key", "")
	setReincarnateHarnessFlag(t, "codex")
	req := &hubclient.ReincarnateAgentRequest{}
	applyReincarnatePatchFlags(req)
	assert.Equal(t, "codex", req.HarnessConfig)
	assert.Equal(t, []string{"harness", "harnessAuth"}, requestedPatchFields(req))
}

// harnessHub is a fake hub reincarnate endpoint answering a --harness
// request with a home-resetting plan, or with oldHub's 400.
type harnessHub struct {
	mu       sync.Mutex
	requests []hubclient.ReincarnateAgentRequest
	oldHub   bool
}

func (h *harnessHub) serve(t *testing.T) *HubContext {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req hubclient.ReincarnateAgentRequest
		_ = json.Unmarshal(body, &req)
		h.mu.Lock()
		h.requests = append(h.requests, req)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if h.oldHub {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"validation_error","message":"config overrides are not yet supported for scion reincarnate"}}`))
			return
		}
		state, code := "planned", http.StatusOK
		if !req.DryRun {
			state, code = "pending", http.StatusAccepted
		}
		_ = json.NewEncoder(w).Encode(hubclient.ReincarnateAgentResponse{AgentID: "agent-1", Generation: 2, State: state,
			Plan: hubclient.ReincarnationPlan{
				Patched:   []string{"harness"},
				HomeReset: true,
				Harness:   &hubclient.FieldChange{Old: "claude", New: "codex"},
				Warnings:  []string{"HOME RESET: the agent's home directory will be deleted and rebuilt for harness config codex.", "explicit model opus dropped"},
			}})
		_ = code
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-1"}
}

// An old hub answers --harness with its "not yet supported" 400: the CLI
// says to upgrade the hub, and only the dry-run probe was sent.
func TestReincarnateHarness_OldHubFailsClosed(t *testing.T) {
	hub := &harnessHub{oldHub: true}
	hubCtx := hub.serve(t)
	setReincarnatePatchFlags(t, "", "", "", -1, "", "")
	setReincarnateHarnessFlag(t, "codex")
	reincarnateBroker, reincarnateDryRun = "", false

	err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this hub does not support --harness; upgrade the hub")
	require.Len(t, hub.requests, 1)
	assert.True(t, hub.requests[0].DryRun)
	assert.Equal(t, "codex", hub.requests[0].HarnessConfig)
}

// The dry run and the real command both print the Harness line and the
// HOME RESET block; the real command sends no acknowledgment and does not
// prompt.
func TestReincarnateHarness_PrintsHomeResetWarning(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		hub := &harnessHub{}
		hubCtx := hub.serve(t)
		setReincarnatePatchFlags(t, "", "", "", -1, "", "")
		setReincarnateHarnessFlag(t, "codex")
		reincarnateBroker, reincarnateDryRun = "", dryRun

		var runErr error
		var stdout string
		stderr := captureStderr(t, func() {
			stdout = captureStdout(t, func() {
				runErr = reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
			})
		})
		require.NoError(t, runErr, "dryRun=%v", dryRun)
		assert.Contains(t, stdout, "Harness:")
		assert.Contains(t, stdout, "claude -> codex")
		assert.Contains(t, stderr, "!! HOME RESET:")
		assert.Contains(t, stderr, "Warning: explicit model opus dropped")
		assert.NotContains(t, stderr, "Warning: HOME RESET", "the home-reset warning is the emphasized block, not a plain warning")
		last := hub.requests[len(hub.requests)-1]
		assert.Equal(t, dryRun, last.DryRun)
		assert.Equal(t, "codex", last.HarnessConfig)
	}
}

func TestPrintHomeResetWarning_Wraps(t *testing.T) {
	var buf bytes.Buffer
	printHomeResetWarning(&buf, strings.Repeat("word ", 60))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Greater(t, len(lines), 1)
	assert.True(t, strings.HasPrefix(lines[0], "!! "))
	for _, l := range lines[1:] {
		assert.True(t, strings.HasPrefix(l, "   "), "continuation lines are indented: %q", l)
	}
}

func TestReincarnateHelp_RecommendsDryRunForHarness(t *testing.T) {
	assert.Contains(t, reincarnateCmd.Long, "--harness <harness-config>")
	assert.Contains(t, reincarnateCmd.Long, "Run with\n--dry-run first")
	long := strings.Join(strings.Fields(reincarnateCmd.Long), " ")
	assert.NotContains(t, long, "docker, podman", "CLI help must not hard-code the Phase 1 local runtime list")
	assert.Contains(t, long, "runtime broker advertises")
	require.NotNil(t, reincarnateCmd.Flags().Lookup("harness"))
}
