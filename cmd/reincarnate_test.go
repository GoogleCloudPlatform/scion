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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveReincarnateTarget is the design §3.4 Amendment A3.11
// table test for the self/handoff-required rule: self-migration (no
// argument, or an explicit argument matching $SCION_AGENT_NAME) requires
// --handoff-file, unless the request is a dry run (which migrates nothing).
// Migrating another agent never requires a handoff.
func TestResolveReincarnateTarget(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		selfName       string
		hasHandoffFile bool
		dryRun         bool
		wantAgentName  string
		wantIsSelf     bool
		wantErrSubstr  string // "" = no error
	}{
		{
			name:          "explicit other agent, no self context: no handoff needed",
			args:          []string{"other-agent"},
			selfName:      "",
			wantAgentName: "other-agent",
			wantIsSelf:    false,
		},
		{
			name:          "explicit other agent, inside an agent container: still not self",
			args:          []string{"other-agent"},
			selfName:      "me",
			wantAgentName: "other-agent",
			wantIsSelf:    false,
		},
		{
			name:          "no argument, no self context: error, not a handoff error",
			args:          nil,
			selfName:      "",
			wantErrSubstr: "specify an agent name",
		},
		{
			name:          "no argument, inside an agent container, no handoff: self-migration requires one",
			args:          nil,
			selfName:      "me",
			wantErrSubstr: "self-migration requires --handoff-file",
		},
		{
			name:           "no argument, inside an agent container, with handoff: allowed",
			args:           nil,
			selfName:       "me",
			hasHandoffFile: true,
			wantAgentName:  "me",
			wantIsSelf:     true,
		},
		{
			name:          "no argument, inside an agent container, dry-run, no handoff: allowed",
			args:          nil,
			selfName:      "me",
			dryRun:        true,
			wantAgentName: "me",
			wantIsSelf:    true,
		},
		{
			name:          "explicit argument equal to self, no handoff: self-migration requires one",
			args:          []string{"me"},
			selfName:      "me",
			wantErrSubstr: "self-migration requires --handoff-file",
		},
		{
			name:           "explicit argument equal to self, with handoff: allowed",
			args:           []string{"me"},
			selfName:       "me",
			hasHandoffFile: true,
			wantAgentName:  "me",
			wantIsSelf:     true,
		},
		{
			name:          "explicit argument equal to self, dry-run, no handoff: allowed",
			args:          []string{"me"},
			selfName:      "me",
			dryRun:        true,
			wantAgentName: "me",
			wantIsSelf:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentName, isSelf, err := resolveReincarnateTarget(tc.args, tc.selfName, tc.hasHandoffFile, tc.dryRun)

			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (agentName=%q isSelf=%v)", tc.wantErrSubstr, agentName, isSelf)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if agentName != tc.wantAgentName {
				t.Errorf("agentName = %q, want %q", agentName, tc.wantAgentName)
			}
			if isSelf != tc.wantIsSelf {
				t.Errorf("isSelf = %v, want %v", isSelf, tc.wantIsSelf)
			}
		})
	}
}

// TestResolveReincarnateTarget_SelfErrorPointsAtHandoffTemplate is the
// Phase 2b (design §3.9 / Amendment A25's "2b" bullet) requirement that the
// self-mode-without-handoff error names --handoff-template as the way out,
// not just --handoff-file.
func TestResolveReincarnateTarget_SelfErrorPointsAtHandoffTemplate(t *testing.T) {
	_, _, err := resolveReincarnateTarget(nil, "me", false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--handoff-template",
		"the self-migration-without-a-handoff error must point at --handoff-template, not just say --handoff-file is required")
	assert.Contains(t, err.Error(), "--handoff-file")
}

// TestReincarnateHelp_FiveLineContractVerbatim pins design §3.9's "the help
// text states the contract in five lines" requirement: reincarnateCmd's Long
// text must contain the five lines byte for byte, not a paraphrase.
func TestReincarnateHelp_FiveLineContractVerbatim(t *testing.T) {
	const wantContract = "1. Commit and push your branch.\n" +
		"2. Write a handoff file.\n" +
		"3. Run `scion reincarnate --dry-run` to see what changes.\n" +
		"4. Run `scion reincarnate --handoff-file <f>`.\n" +
		"5. Do nothing after that call; your container will be stopped.\n"

	assert.Equal(t, wantContract, reincarnateFiveLineContract,
		"the contract constant itself must match §3.9's five lines verbatim")
	assert.Contains(t, reincarnateCmd.Long, wantContract,
		"--help (Long) must state the five-line contract verbatim from §3.9")
	assert.Contains(t, reincarnateCmd.Long, "--handoff-template",
		"--help must also point at --handoff-template for the handoff's expected sections")
}

// TestReincarnateHandoffTemplate_Golden is the golden test for
// `scion reincarnate --handoff-template` (design §3.9: "prints the handoff
// template to stdout... embedded in the CLI, not stored as a skill file").
// It pins both the exact text and that every §3.9 "Handoff template
// sections" heading is present, in order, and that the command needs
// neither a resolved target nor a Hub connection to produce it.
func TestReincarnateHandoffTemplate_Golden(t *testing.T) {
	origTemplate := reincarnateHandoffTemplate
	t.Cleanup(func() {
		reincarnateHandoffTemplate = origTemplate
		reincarnateCmd.SetOut(nil)
	})
	reincarnateHandoffTemplate = true

	var buf bytes.Buffer
	reincarnateCmd.SetOut(&buf)

	// No agent argument, no $SCION_AGENT_NAME, no --handoff-file, no Hub
	// context configured: --handoff-template must still succeed, proving it
	// short-circuits every other requirement of the command.
	t.Setenv("SCION_AGENT_NAME", "")
	err := reincarnateCmd.RunE(reincarnateCmd, nil)
	require.NoError(t, err)

	assert.Equal(t, reincarnateHandoffTemplateText, buf.String(),
		"the printed template must match the embedded constant exactly")

	wantSections := []string{
		"## Role charter",
		"## Immediate active work (status, next action)",
		"## Canonical files and artifacts",
		"## Authority and ownership (who to ask, who can approve)",
		"## Live conversations (conv ids) and counterparties",
		"## Children agents and their state",
		"## Pending waits and scheduled events",
		"## Open questions to humans (already asked and not yet asked)",
		"## Operating constraints and lessons learned",
		"## Do not redo",
	}
	out := buf.String()
	lastIdx := -1
	for _, section := range wantSections {
		idx := strings.Index(out, section)
		if idx < 0 {
			t.Fatalf("handoff template missing section %q", section)
		}
		if idx <= lastIdx {
			t.Fatalf("section %q is out of order (design §3.9's section order must be preserved)", section)
		}
		lastIdx = idx
	}
}

// TestReincarnateSetBlockedStatus_SciontoolAbsent is the design §3.9 "fail
// soft" requirement: if sciontool is not on PATH (e.g. a harness image that
// predates it, ptone/scion#1910), reincarnateSetBlockedStatus must warn, not
// panic or otherwise fail the already-succeeded reincarnate call.
func TestReincarnateSetBlockedStatus_SciontoolAbsent(t *testing.T) {
	// An empty, otherwise-real temp dir on PATH guarantees "sciontool" is
	// not found, without disturbing anything else the test process needs.
	t.Setenv("PATH", t.TempDir())

	stderr := captureStderr(t, func() {
		reincarnateSetBlockedStatus(7)
	})

	assert.Contains(t, stderr, "sciontool not found")
	assert.Contains(t, stderr, "migrating to generation 7",
		"the warning must still name the message that could not be delivered")
}

// TestReincarnateSetBlockedStatus_InvokesSciontool proves the success path:
// when sciontool is on PATH, reincarnateSetBlockedStatus must invoke it as
// `sciontool status blocked "migrating to generation N+1"`, with N+1 taken
// from the argument (the Hub's returned target generation).
func TestReincarnateSetBlockedStatus_InvokesSciontool(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "invoked-args.txt")

	scriptPath := filepath.Join(dir, "sciontool")
	script := "#!/bin/sh\necho \"$@\" > " + recordPath + "\n"
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))

	t.Setenv("PATH", dir)

	stderr := captureStderr(t, func() {
		reincarnateSetBlockedStatus(3)
	})
	assert.Empty(t, stderr, "no warning should be printed when sciontool succeeds")

	got, err := os.ReadFile(recordPath)
	require.NoError(t, err, "the fake sciontool script must have been invoked")
	assert.Equal(t, "status blocked migrating to generation 3\n", string(got))
}

// TestReincarnateSetBlockedStatus_TimesOut is the design Amendment A26.2 O3
// test: a hung sciontool call must not hang the CLI forever — it warns and
// returns once reincarnateBlockedStatusTimeout elapses. Overrides the
// package var to a few milliseconds so the test doesn't actually wait out
// the real 10s production timeout.
func TestReincarnateSetBlockedStatus_TimesOut(t *testing.T) {
	origTimeout := reincarnateBlockedStatusTimeout
	t.Cleanup(func() { reincarnateBlockedStatusTimeout = origTimeout })
	reincarnateBlockedStatusTimeout = 50 * time.Millisecond

	// Resolve `sleep` on the real PATH before overriding it below, so the
	// fake sciontool script can exec it by absolute path.
	sleepPath, err := exec.LookPath("sleep")
	require.NoError(t, err, "this test needs a real `sleep` binary")

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "sciontool")
	// Sleeps far longer than the shortened timeout above; `exec` (not a
	// shell builtin) so CommandContext's context cancellation actually
	// kills it rather than killing an intermediate shell.
	script := "#!/bin/sh\nexec " + sleepPath + " 5\n"
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	t.Setenv("PATH", dir)

	stderr := captureStderr(t, func() {
		reincarnateSetBlockedStatus(9)
	})
	assert.Contains(t, stderr, "timed out")
}

// TestShouldSetBlockedStatus is the design Amendment A26.2 R4 table test:
// the blocked-status call must fire only in self-mode and only on a real
// (non-dry-run) migration, extracted as a pure decision so it needs no
// process execution at all. TestReincarnateAgentViaHub_SetBlockedStatusCallSite
// below additionally pins that reincarnateAgentViaHub's call site actually
// uses this decision, end to end against a fake Hub.
func TestShouldSetBlockedStatus(t *testing.T) {
	cases := []struct {
		name   string
		isSelf bool
		dryRun bool
		want   bool
	}{
		{"self, real migration: fires", true, false, true},
		{"self, dry run: does not fire", true, true, false},
		{"other agent, real migration: does not fire", false, false, false},
		{"other agent, dry run: does not fire", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldSetBlockedStatus(tc.isSelf, tc.dryRun))
		})
	}
}

// TestReincarnateHandoffTemplate_WorksAnywhere is the design Amendment A26.2
// O1 / A26.3 R1 test: `scion reincarnate --handoff-template` is a pure local
// print and must work through the real command dispatch path
// (rootCmd.ExecuteC, which runs the full PersistentPreRunE chain), even
// inside a simulated agent container with no reachable Hub endpoint, and
// even outside any scion project — both of which reject ordinary commands in
// PersistentPreRunE, before RunE ever runs. The negative subtests pin the
// other half: an ordinary `reincarnate` invocation (no --handoff-template)
// must still hit both gates in the same two environments, so the exemption
// cannot silently widen to the whole command.
func TestReincarnateHandoffTemplate_WorksAnywhere(t *testing.T) {
	origProjectPath := projectPath
	origHubEndpoint := hubEndpoint
	origNoHub := noHub
	t.Cleanup(func() {
		projectPath = origProjectPath
		hubEndpoint = origHubEndpoint
		noHub = origNoHub
		reincarnateHandoffTemplate = false
		reincarnateDryRun = false
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	// run resets the two flag vars cobra does not reset between ExecuteC
	// calls on the same command tree, then executes args through the real
	// dispatch path (PersistentPreRunE included).
	run := func(t *testing.T, args []string) (string, error) {
		t.Helper()
		reincarnateHandoffTemplate = false
		reincarnateDryRun = false
		var buf bytes.Buffer
		rootCmd.SetOut(&buf)
		rootCmd.SetErr(&buf)
		rootCmd.SetArgs(args)
		_, err := rootCmd.ExecuteC()
		return buf.String(), err
	}

	t.Run("inside an agent container with no reachable Hub endpoint", func(t *testing.T) {
		t.Setenv("SCION_HOST_UID", "1000")
		t.Setenv("SCION_HUB_ENDPOINT", "")
		t.Setenv("SCION_HUB_URL", "")
		t.Setenv("SCION_NETWORK_MODE", "")
		hubEndpoint = ""
		projectPath = t.TempDir()

		t.Run("--handoff-template works", func(t *testing.T) {
			out, err := run(t, []string{"reincarnate", "--handoff-template"})
			require.NoError(t, err)
			assert.Equal(t, reincarnateHandoffTemplateText, out)
		})

		t.Run("an ordinary --dry-run is still gated", func(t *testing.T) {
			out, err := run(t, []string{"reincarnate", "--dry-run"})
			require.Error(t, err, "the agent-container gate must still reject an ordinary reincarnate call")
			assert.Contains(t, err.Error(), "agent container")
			assert.NotContains(t, out, reincarnateHandoffTemplateText)
		})

		t.Run("a plain, non-dry-run reincarnate is still gated", func(t *testing.T) {
			out, err := run(t, []string{"reincarnate", "some-agent"})
			require.Error(t, err, "the agent-container gate must still reject an ordinary reincarnate call")
			assert.Contains(t, err.Error(), "agent container")
			assert.NotContains(t, out, reincarnateHandoffTemplateText)
		})

		t.Run("--handoff-template=false is still gated", func(t *testing.T) {
			out, err := run(t, []string{"reincarnate", "--handoff-template=false"})
			require.Error(t, err, "an explicit false must not be treated as the exemption")
			assert.NotContains(t, out, reincarnateHandoffTemplateText)
		})
	})

	t.Run("outside any scion project", func(t *testing.T) {
		t.Setenv("SCION_HOST_UID", "")
		t.Setenv("HOME", t.TempDir())
		t.Chdir(t.TempDir())
		projectPath = ""

		t.Run("--handoff-template works", func(t *testing.T) {
			out, err := run(t, []string{"reincarnate", "--handoff-template"})
			require.NoError(t, err)
			assert.Equal(t, reincarnateHandoffTemplateText, out)
		})

		t.Run("an ordinary --dry-run still hits the requires-project error", func(t *testing.T) {
			_, err := run(t, []string{"reincarnate", "--dry-run", "some-agent"})
			require.Error(t, err, "the requires-project gate must still reject an ordinary reincarnate call")
			assert.Contains(t, err.Error(), "not in a scion project")
		})

		t.Run("a plain, non-dry-run reincarnate still hits the requires-project error", func(t *testing.T) {
			_, err := run(t, []string{"reincarnate", "some-agent"})
			require.Error(t, err, "the requires-project gate must still reject an ordinary reincarnate call")
			assert.Contains(t, err.Error(), "not in a scion project")
		})
	})
}

// TestReincarnateAgentViaHub_SetBlockedStatusCallSite is the design
// Amendment A26.3 O1 test (A26.2 R4's second option): drives
// reincarnateAgentViaHub end to end against a fake Hub returning 202 with a
// given generation, and asserts the injectable setBlockedStatus var is
// called exactly once with that generation for a real self-migration, and
// not at all for a self dry-run or a real migration of another agent. This
// pins the call site itself — shouldSetBlockedStatus's own decision is
// covered in isolation by TestShouldSetBlockedStatus above — and also pins
// "N+1 = the Hub's returned generation" end to end.
func TestReincarnateAgentViaHub_SetBlockedStatusCallSite(t *testing.T) {
	const projectID = "proj-1"
	const targetGeneration = 7

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agentId":    "agent-1",
			"generation": targetGeneration,
			"state":      "pending",
			"plan":       map[string]any{},
		})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	origSetter := setBlockedStatus
	origDryRun := reincarnateDryRun
	t.Cleanup(func() {
		setBlockedStatus = origSetter
		reincarnateDryRun = origDryRun
	})

	run := func(t *testing.T, isSelf, dryRun bool) []int {
		t.Helper()
		var calls []int
		setBlockedStatus = func(gen int) { calls = append(calls, gen) }
		reincarnateDryRun = dryRun
		err := reincarnateAgentViaHub(hubCtx, "some-agent", "handoff", isSelf)
		require.NoError(t, err)
		return calls
	}

	t.Run("self, real migration: called once with the Hub's generation", func(t *testing.T) {
		calls := run(t, true, false)
		assert.Equal(t, []int{targetGeneration}, calls)
	})

	t.Run("self, dry run: never called", func(t *testing.T) {
		calls := run(t, true, true)
		assert.Empty(t, calls)
	})

	t.Run("other agent, real migration: never called", func(t *testing.T) {
		calls := run(t, false, false)
		assert.Empty(t, calls)
	})
}
