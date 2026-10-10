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
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests need no database, so they build with every tag set.

func TestParseShareTTL(t *testing.T) {
	for in, want := range map[string]int{
		"": 0, "1h": 1, "24h": 24, "7d": 168, "30d": 720, "720h": 720,
		"7D": 168, "24H": 24, " 7d ": 168, "007d": 168, "1048576h": 1 << 20,
	} {
		got, err := parseShareTTL(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for in, msg := range map[string]string{
		"24":        "needs a unit",
		"0d":        "at least 1h",
		"0h":        "at least 1h",
		"-1h":       "whole number",
		"+5h":       "whole number",
		"h":         "whole number",
		"1.5h":      "whole number",
		"1m":        "whole number",
		"1w":        "whole number",
		"1 d":       "whole number",
		"1e3h":      "whole number",
		"10000000d": "too long",
		"43691d":    "too long",
		"99999999h": "too long",
	} {
		_, err := parseShareTTL(in)
		if assert.Error(t, err, in) {
			assert.Contains(t, err.Error(), msg, in)
		}
	}
}

// TestArtifactShareModes: share is available to users in human and
// assistant mode and removed in agent mode, while the other artifact verbs
// stay available to agents.
func TestArtifactShareModes(t *testing.T) {
	assert.False(t, agentAllowed["artifact.share"])
	assert.False(t, assistantDenied["artifact.share"])
	assert.False(t, assistantDenied["artifact"])
	assert.True(t, agentAllowed["artifact.get"])

	build := func() *cobra.Command {
		root := &cobra.Command{Use: "scion"}
		art := &cobra.Command{Use: "artifact"}
		for _, v := range []string{"share", "get", "publish", "versions"} {
			art.AddCommand(&cobra.Command{Use: v})
		}
		root.AddCommand(art)
		return root
	}
	for mode, wantShare := range map[string]bool{"human": true, "assistant": true, "agent": false} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SCION_CLI_MODE", mode)
			root := build()
			applyModeRestrictions(root, resolveMode())
			names := collectCommandNames(root)
			assert.Equal(t, wantShare, slices.Contains(names, "artifact.share"), "artifact.share in %s mode", mode)
			assert.Contains(t, names, "artifact.get")
		})
	}
}

// TestArtifactShareRefusedInAgentMode: in agent mode "scion artifact share"
// fails with the generic unknown-command error, the same as for a command
// that does not exist (.design/cli-modes.md section 4.3), instead of
// printing the artifact help and succeeding, and the artifact help no
// longer lists share. In human mode share runs and is listed.
func TestArtifactShareRefusedInAgentMode(t *testing.T) {
	build := func() (*cobra.Command, *cobra.Command, *bool, *bytes.Buffer) {
		ran := false
		root := &cobra.Command{Use: "scion", SilenceErrors: true, SilenceUsage: true}
		art := &cobra.Command{Use: "artifact", Long: artifactCmd.Long, Args: artifactCmd.Args, Run: artifactCmd.Run}
		art.AddCommand(&cobra.Command{Use: "share <ref>", Args: cobra.ExactArgs(1), Run: func(*cobra.Command, []string) { ran = true }})
		for _, v := range []string{"get", "publish", "versions"} {
			art.AddCommand(&cobra.Command{Use: v, Run: func(*cobra.Command, []string) {}})
		}
		root.AddCommand(art)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		return root, art, &ran, &out
	}

	t.Run("agent", func(t *testing.T) {
		root, art, ran, out := build()
		applyModeRestrictions(root, ModeAgent)
		root.SetArgs([]string{"artifact", "share", "scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d"})
		err := root.Execute()
		require.Error(t, err, "share must fail, not fall through to help")
		assert.Contains(t, err.Error(), `unknown command "share" for "scion artifact"`)
		assert.NotContains(t, err.Error(), "mode")
		assert.False(t, *ran)
		shareOut := out.String()

		// The refusal is exactly what a nonexistent subcommand gets, with
		// only the name changed, and prints the same (nothing).
		out.Reset()
		root.SetArgs([]string{"artifact", "nosuch", "scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d"})
		nosuch := root.Execute()
		require.Error(t, nosuch)
		assert.Equal(t, strings.ReplaceAll(nosuch.Error(), `"nosuch"`, `"share"`), err.Error())
		assert.Equal(t, out.String(), shareOut)
		assert.Empty(t, shareOut)
		assert.NotContains(t, art.Long, "scion artifact share")
		assert.Contains(t, art.Long, "scion artifact get <ref>")

		// A bare "artifact" still prints help and succeeds.
		root.SetArgs([]string{"artifact"})
		assert.NoError(t, root.Execute())
	})

	t.Run("human", func(t *testing.T) {
		root, art, ran, _ := build()
		applyModeRestrictions(root, ModeHuman)
		root.SetArgs([]string{"artifact", "share", "scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d"})
		require.NoError(t, root.Execute())
		assert.True(t, *ran)
		assert.Contains(t, art.Long, "scion artifact share <ref>")

		root.SetArgs([]string{"artifact", "nosuch"})
		err := root.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `unknown command "nosuch" for "scion artifact"`)
	})
}

func TestDropCommandLines(t *testing.T) {
	long := strings.Join([]string{
		"Intro mentioning scion artifact share in prose.",
		"",
		"Commands:",
		"  scion artifact get <ref>",
		"  scion artifact share <ref> [--ttl 7d]        Create a share link",
		"  scion artifact share",
		"  scion artifact shared-thing",
	}, "\n")
	got := dropCommandLines(long, "scion artifact share")
	assert.Equal(t, strings.Join([]string{
		"Intro mentioning scion artifact share in prose.",
		"",
		"Commands:",
		"  scion artifact get <ref>",
		"  scion artifact shared-thing",
	}, "\n"), got)
	assert.Equal(t, "no match", dropCommandLines("no match", "scion artifact share"))
}
