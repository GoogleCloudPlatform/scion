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
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// TestArtifactListViewGrantsModes: list and view are read-only and
// available to agents; grants is human-only, removed in agent mode and
// dropped from the artifact help there (ptone/scion#4572, ptone/scion#4579).
func TestArtifactListViewGrantsModes(t *testing.T) {
	assert.True(t, agentAllowed["artifact.list"])
	assert.True(t, agentAllowed["artifact.view"])
	assert.False(t, agentAllowed["artifact.grants"])

	for _, name := range []string{"list", "view", "grants"} {
		_, _, err := rootCmd.Find([]string{"artifact", name})
		assert.NoError(t, err, "artifact %s is registered", name)
	}

	build := func() (*cobra.Command, *cobra.Command) {
		root := &cobra.Command{Use: "scion"}
		art := &cobra.Command{Use: "artifact", Long: artifactCmd.Long}
		for _, v := range []string{"list", "view", "grants", "get"} {
			art.AddCommand(&cobra.Command{Use: v})
		}
		root.AddCommand(art)
		return root, art
	}
	for mode, wantGrants := range map[CLIMode]bool{ModeHuman: true, ModeAgent: false} {
		root, art := build()
		applyModeRestrictions(root, mode)
		names := collectCommandNames(root)
		assert.Equal(t, wantGrants, slices.Contains(names, "artifact.grants"), "artifact.grants in %s mode", mode)
		assert.Contains(t, names, "artifact.list")
		assert.Contains(t, names, "artifact.view")
		assert.Equal(t, wantGrants, containsLine(art.Long, "scion artifact grants <ref>"), "grants help line in %s mode", mode)
		assert.True(t, containsLine(art.Long, "scion artifact list [--mine] [--all]"))
		assert.True(t, containsLine(art.Long, "scion artifact view <ref> [--check]"))
	}
}

// containsLine reports whether some line of s, with leading space removed,
// starts with prefix.
func containsLine(s, prefix string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(l, " \t"), prefix) {
			return true
		}
	}
	return false
}
