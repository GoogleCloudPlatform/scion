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
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// TestGetAgentNames covers agent-name completion. The names come from the
// current hub; agent directories on disk are not scanned.
func TestGetAgentNames(t *testing.T) {
	setupContextTest(t)
	hub := agentListHub(t, "agent1", "agent2", "foobar")
	saveTestContext(t, hub.URL, "")
	cmd := completionCmd()

	t.Run("Complete all agents", func(t *testing.T) {
		names, _ := getAgentNames(cmd, []string{}, "")
		assert.Contains(t, names, "agent1")
		assert.Contains(t, names, "agent2")
		assert.Contains(t, names, "foobar")
		assert.Len(t, names, 3)
	})

	t.Run("Complete prefix", func(t *testing.T) {
		names, _ := getAgentNames(cmd, []string{}, "agent")
		assert.Contains(t, names, "agent1")
		assert.Contains(t, names, "agent2")
		assert.NotContains(t, names, "foobar")
		assert.Len(t, names, 2)
	})

	t.Run("Complete no match", func(t *testing.T) {
		names, _ := getAgentNames(cmd, []string{}, "z")
		assert.Len(t, names, 0)
	})

	t.Run("Args present", func(t *testing.T) {
		names, _ := getAgentNames(cmd, []string{"already-have-arg"}, "")
		assert.Nil(t, names)
	})

	t.Run("Multi complete excludes already provided", func(t *testing.T) {
		names, dir := getMultiAgentNames(cmd, []string{"agent1"}, "")
		assert.Contains(t, names, "agent2")
		assert.Contains(t, names, "foobar")
		assert.NotContains(t, names, "agent1")
		assert.Len(t, names, 2)
		assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, dir)
	})

	t.Run("Multi complete with prefix and exclusion", func(t *testing.T) {
		names, _ := getMultiAgentNames(cmd, []string{"agent1"}, "agent")
		assert.Contains(t, names, "agent2")
		assert.NotContains(t, names, "agent1")
		assert.Len(t, names, 1)
	})

	t.Run("Multi complete no args same as single", func(t *testing.T) {
		names, _ := getMultiAgentNames(cmd, []string{}, "")
		assert.Contains(t, names, "agent1")
		assert.Contains(t, names, "agent2")
		assert.Contains(t, names, "foobar")
		assert.Len(t, names, 3)
	})
}
