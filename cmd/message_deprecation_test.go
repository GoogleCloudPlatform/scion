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
	"fmt"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// removedFlagNames lists every retired scion message flag kept registered
// (hidden) so invoking it returns an actionable error instead of Cobra's
// "unknown flag".
var removedFlagNames = []string{
	"broadcast", "all", "raw",
	"in", "at", "plain", "notify", "channel", "thread-id", "cc",
}

// resetMessageFlags resets all message-related global flags to their defaults
// and returns a restore function.
func resetMessageFlags() func() {
	orig := struct {
		interrupt bool
		attach    []string
		wake      bool
		artifacts []string
	}{
		msgInterrupt, msgAttach, msgWake, msgArtifacts,
	}

	type flagState struct {
		changed bool
		value   string
		slice   []string
		isSlice bool
	}
	savedRemoved := make(map[string]flagState, len(removedFlagNames))
	for _, name := range removedFlagNames {
		if f := messageCmd.Flags().Lookup(name); f != nil {
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				savedRemoved[name] = flagState{changed: f.Changed, slice: append([]string(nil), sv.GetSlice()...), isSlice: true}
				_ = sv.Replace(nil)
			} else {
				savedRemoved[name] = flagState{changed: f.Changed, value: f.Value.String()}
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		}
	}

	// Reset all
	msgInterrupt = false
	msgAttach = nil
	msgWake = false
	msgArtifacts = nil
	msgArtifactRefs = nil
	resetDecisionFlags()
	for _, name := range []string{"decisions", "decisions-file", "choices", "choices-file", "update"} {
		_ = messageCmd.Flags().Lookup(name).Value.Set(messageCmd.Flags().Lookup(name).DefValue)
		messageCmd.Flags().Lookup(name).Changed = false
	}

	return func() {
		msgInterrupt = orig.interrupt
		msgAttach = orig.attach
		msgWake = orig.wake
		msgArtifacts = orig.artifacts
		msgArtifactRefs = nil
		resetDecisionFlags()
		for _, name := range []string{"decisions", "decisions-file", "choices", "choices-file", "update"} {
			_ = messageCmd.Flags().Lookup(name).Value.Set(messageCmd.Flags().Lookup(name).DefValue)
			messageCmd.Flags().Lookup(name).Changed = false
		}
		for name, st := range savedRemoved {
			if f := messageCmd.Flags().Lookup(name); f != nil {
				if st.isSlice {
					if sv, ok := f.Value.(pflag.SliceValue); ok {
						_ = sv.Replace(st.slice)
					}
				} else {
					_ = f.Value.Set(st.value)
				}
				f.Changed = st.changed
			}
		}
	}
}

// TestDeprecatedFlag_Broadcast tests that --broadcast is refused in human mode
// with an error pointing at scion broadcast.
func TestDeprecatedFlag_Broadcast(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	// Ensure human mode (default)
	t.Setenv("SCION_CLI_MODE", "")

	// Simulate the flag being set via cobra
	require.NoError(t, messageCmd.Flags().Set("broadcast", "true"))

	err := messageCmd.RunE(messageCmd, []string{"hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--broadcast has been removed")
	assert.Contains(t, err.Error(), "scion broadcast")
}

// TestDeprecatedFlag_All tests that --all is refused in human mode with an
// error pointing at scion broadcast --all.
func TestDeprecatedFlag_All(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	t.Setenv("SCION_CLI_MODE", "")

	require.NoError(t, messageCmd.Flags().Set("all", "true"))

	err := messageCmd.RunE(messageCmd, []string{"hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all has been removed")
	assert.Contains(t, err.Error(), "scion broadcast --all")
}

// TestDeprecatedFlag_Broadcast_AgentMode tests that --broadcast in agent mode
// does NOT recommend scion broadcast (which is unavailable to agents) and
// instead tells the agent to address recipients explicitly.
func TestDeprecatedFlag_Broadcast_AgentMode(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	t.Setenv("SCION_CLI_MODE", "agent")

	require.NoError(t, messageCmd.Flags().Set("broadcast", "true"))

	err := messageCmd.RunE(messageCmd, []string{"hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--broadcast has been removed")
	assert.Contains(t, err.Error(), "not available in agent mode")
	assert.Contains(t, err.Error(), "address your recipients explicitly")
	assert.NotContains(t, err.Error(), "scion broadcast",
		"agent-mode refusal must not recommend scion broadcast (not in agentAllowed)")
}

// TestDeprecatedFlag_All_AgentMode tests that --all in agent mode does NOT
// recommend scion broadcast --all and instead tells the agent to address
// recipients explicitly.
func TestDeprecatedFlag_All_AgentMode(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	t.Setenv("SCION_CLI_MODE", "agent")

	require.NoError(t, messageCmd.Flags().Set("all", "true"))

	err := messageCmd.RunE(messageCmd, []string{"hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all has been removed")
	assert.Contains(t, err.Error(), "not available in agent mode")
	assert.Contains(t, err.Error(), "address your recipients explicitly")
	assert.NotContains(t, err.Error(), "scion broadcast",
		"agent-mode refusal must not recommend scion broadcast --all (not in agentAllowed)")
}

// TestRemovedFlag_Raw tests that --raw, with any value, is refused by the
// argument validator with guidance naming 'scion keys'.
func TestRemovedFlag_Raw(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		t.Run(value, func(t *testing.T) {
			orig := saveMessageTestState()
			defer orig.restore()
			restore := resetMessageFlags()
			defer restore()

			require.NoError(t, messageCmd.Flags().Set("raw", value))

			err := messageCmd.Args(messageCmd, []string{"my-agent", "Escape"})
			require.ErrorIs(t, err, errRawFlagRemoved)
			assert.Contains(t, err.Error(), "--raw has been removed")
			assert.Contains(t, err.Error(), "scion keys")
		})
	}
}

// TestRemovedFlags_RefusedWithActionableError verifies that each of the 7
// retired scion message flags (--channel, --thread-id, --plain, --cc, --in,
// --at, --notify) fails non-zero in both Args and RunE with a usage error
// naming its replacement.
func TestRemovedFlags_RefusedWithActionableError(t *testing.T) {
	tests := []struct {
		flag             string
		value            string
		wantReplacements []string
	}{
		{
			flag:             "channel",
			value:            "slack",
			wantReplacements: []string{"conv:<uuid>", "scion conversation list", "@<agent-name>"},
		},
		{
			flag:             "thread-id",
			value:            "thread-123",
			wantReplacements: []string{"conv:<uuid>", "scion conversation list"},
		},
		{
			flag:             "plain",
			value:            "true",
			wantReplacements: []string{"scion schedule create --plain", "scion trigger create --plain"},
		},
		{
			flag:             "cc",
			value:            "agent-a",
			wantReplacements: []string{"group[...]", "@mentions"},
		},
		{
			flag:             "in",
			value:            "30m",
			wantReplacements: []string{"scion schedule create --in"},
		},
		{
			flag:             "at",
			value:            "2030-01-01T00:00:00Z",
			wantReplacements: []string{"scion schedule create --at"},
		},
		{
			flag:             "notify",
			value:            "true",
			wantReplacements: []string{"scion notifications subscribe"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.flag, func(t *testing.T) {
			orig := saveMessageTestState()
			defer orig.restore()
			restore := resetMessageFlags()
			defer restore()

			require.NoError(t, messageCmd.Flags().Set(tc.flag, tc.value))

			argsErr := messageCmd.Args(messageCmd, []string{"my-agent", "hello"})
			require.Error(t, argsErr)
			assert.Contains(t, argsErr.Error(), "--"+tc.flag+" has been removed")
			for _, want := range tc.wantReplacements {
				assert.Contains(t, argsErr.Error(), want)
			}
			assert.True(t, showUsageForError(messageCmd, argsErr, true), "removed flag should be a usage error")

			runErr := messageCmd.RunE(messageCmd, []string{"my-agent", "hello"})
			require.Error(t, runErr)
			assert.Contains(t, runErr.Error(), "--"+tc.flag+" has been removed")
			for _, want := range tc.wantReplacements {
				assert.Contains(t, runErr.Error(), want)
			}
		})
	}
}

// TestRemovedFlags_SupportedFlagsStillWork verifies that retained flags
// (--interrupt, --wake) pass argument validation and deliver messages normally.
func TestRemovedFlags_SupportedFlagsStillWork(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	require.NoError(t, messageCmd.Flags().Set("interrupt", "true"))
	require.NoError(t, messageCmd.Flags().Set("wake", "true"))
	defer func() {
		for _, name := range []string{"interrupt", "wake"} {
			f := messageCmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}()

	require.NoError(t, messageCmd.Args(messageCmd, []string{"my-agent", "hello"}))

	projectID := "proj-retained-flags"
	server, sent := newMessageMockHubServer(t, projectID, nil)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	err = sendMessageViaHub(hubCtx, "my-agent", "hello", msgInterrupt, msgWake)
	require.NoError(t, err)
	require.Len(t, *sent, 1)
	assert.True(t, (*sent)[0].Interrupt)
	assert.Equal(t, "hello", (*sent)[0].Message)
}

// TestDeprecatedFlag_BroadcastRefusedViaRunE verifies that --broadcast
// is refused early in RunE with an actionable error, regardless of Hub
// availability.
func TestDeprecatedFlag_BroadcastRefusedViaRunE(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	t.Setenv("SCION_CLI_MODE", "")

	require.NoError(t, messageCmd.Flags().Set("broadcast", "true"))

	// Even with a valid recipient, --broadcast must be refused.
	err := messageCmd.RunE(messageCmd, []string{"agent1", "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--broadcast has been removed")
	assert.Contains(t, err.Error(), "scion broadcast")
}

// TestDeprecatedFlags_Hidden verifies that deprecated and removed flags
// are hidden from help output.
func TestDeprecatedFlags_Hidden(t *testing.T) {
	deprecatedFlags := []string{
		"broadcast", "all", // removed, still registered to avoid "unknown flag" errors
		"in", "at", "plain", "raw",
		"notify", "channel", "thread-id", "cc",
		"choices", "choices-file",
	}
	for _, name := range deprecatedFlags {
		f := messageCmd.Flags().Lookup(name)
		require.NotNil(t, f, "flag --%s should exist", name)
		assert.True(t, f.Hidden, "flag --%s should be hidden", name)
	}
}

// TestRetainedFlags_NotHidden verifies that retained flags are NOT hidden.
func TestRetainedFlags_NotHidden(t *testing.T) {
	retainedFlags := []string{
		"interrupt", "wake", "attach",
		"decisions", "decisions-file", "update",
	}
	for _, name := range retainedFlags {
		f := messageCmd.Flags().Lookup(name)
		require.NotNil(t, f, "flag --%s should exist", name)
		assert.False(t, f.Hidden, "flag --%s should NOT be hidden", name)
	}
}

// TestBroadcastCmd_IsRegistered verifies the broadcast command is available
// on the root command.
func TestBroadcastCmd_IsRegistered(t *testing.T) {
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "broadcast" {
			found = true
			break
		}
	}
	assert.True(t, found, "broadcast command should be registered on rootCmd")
}

// findReplacementProblems scans guidance text for replacement command
// references ("scion <subcommand>") and validates each resolves against
// rootCmd. Returns problems found and the count of replacements examined.
func findReplacementProblems(text string) (problems []string, checked int) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		searchFrom := 0
		for {
			idx := strings.Index(line[searchFrom:], "scion ")
			if idx < 0 {
				break
			}
			pos := searchFrom + idx
			searchFrom = pos + len("scion ")

			rest := line[pos:]
			fields := strings.Fields(rest)
			if len(fields) < 2 {
				continue
			}
			// Collect subcommand tokens (skip "scion" itself), stop at flags, quotes, commas
			var subArgs []string
			for _, f := range fields[1:] {
				if strings.HasPrefix(f, "--") || strings.HasPrefix(f, "-") {
					break
				}
				cleaned := strings.Trim(f, "\"'`,;)")
				if cleaned == "" {
					break
				}
				cleaned2 := strings.TrimRight(f, "\"'`,;)")
				if cleaned2 == "" {
					break
				}
				subArgs = append(subArgs, cleaned2)
				if cleaned2 != f {
					break
				}
			}
			if len(subArgs) == 0 {
				continue
			}
			checked++
			cmd, _, err := rootCmd.Find(subArgs)
			if err != nil {
				problems = append(problems,
					fmt.Sprintf("replacement command not found: scion %s (from: %s)",
						strings.Join(subArgs, " "), line))
				continue
			}
			if cmd.Name() != subArgs[len(subArgs)-1] {
				problems = append(problems,
					fmt.Sprintf("replacement resolves to wrong command: wanted %s, got %s (from: %s)",
						subArgs[len(subArgs)-1], cmd.Name(), line))
			}
		}
	}
	return problems, checked
}

// TestRemovedFlags_ReplacementsExist validates that every removed-flag error
// naming a 'scion <subcommand>' replacement resolves via rootCmd.Find().
func TestRemovedFlags_ReplacementsExist(t *testing.T) {
	var lines []string
	for _, r := range removedFlagErrors {
		lines = append(lines, r.Message)
	}

	problems, checked := findReplacementProblems(strings.Join(lines, "\n"))
	for _, p := range problems {
		t.Error(p)
	}
	// plain (schedule create + trigger create), notify (notifications subscribe),
	// in (schedule create), at (schedule create), channel (conversation list),
	// thread-id (conversation list) = 7 command references.
	require.GreaterOrEqual(t, checked, 7,
		"expected at least 7 replacement command references in removedFlagErrors; got %d", checked)

	// The --raw removal error names 'scion keys'; check that command resolves
	// with the same extractor.
	require.Contains(t, errRawFlagRemoved.Error(), "'scion keys <agent-name> <keystrokes>'")
	rawProblems, rawChecked := findReplacementProblems("Warning: --raw has been removed, use 'scion keys' instead")
	for _, p := range rawProblems {
		t.Error(p)
	}
	require.Equal(t, 1, rawChecked, "the --raw replacement must be checked")

	// Rule 10: prove findReplacementProblems catches bad replacements.
	t.Run("catches_deepest_match_blind_spot", func(t *testing.T) {
		problems, _ := findReplacementProblems(
			"Warning: --x is deprecated, use 'scion schedule message' instead")
		assert.NotEmpty(t, problems, "should catch nonexistent subcommand via deepest-match blind spot")
	})
	t.Run("catches_backtick_quoted_unknown", func(t *testing.T) {
		problems, _ := findReplacementProblems(
			"Warning: --x is deprecated, use `scion agent poke` instead")
		assert.NotEmpty(t, problems, "should catch unknown command in backtick-quoted reference")
	})
	t.Run("catches_unquoted_unknown", func(t *testing.T) {
		problems, _ := findReplacementProblems(
			"Warning: --x is deprecated, use scion nonexistent-thing instead")
		assert.NotEmpty(t, problems, "should catch unknown command in unquoted reference")
	})
	t.Run("accepts_valid_replacement", func(t *testing.T) {
		problems, checked := findReplacementProblems(
			"Warning: --x is deprecated, use 'scion broadcast' instead")
		assert.Empty(t, problems, "should accept valid replacement command")
		assert.Equal(t, 1, checked, "should have checked exactly one reference")
	})
}
