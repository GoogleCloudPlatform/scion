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
	"strings"
	"testing"

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
	restoreAllSilenceUsage(t)
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
	restoreAllSilenceUsage(t)
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
