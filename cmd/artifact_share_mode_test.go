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
	"slices"
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
