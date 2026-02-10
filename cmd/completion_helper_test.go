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
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestGetAgentNames(t *testing.T) {
	// Isolate from user environment
	originalHome := os.Getenv("HOME")
	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", originalHome)

	// Setup temp directory for grove
	tmpDir, err := os.MkdirTemp("", "scion-completion-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	groveDir := filepath.Join(tmpDir, ".scion")
	agentsDir := filepath.Join(groveDir, "agents")
	err = os.MkdirAll(agentsDir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	// Create some dummy agents
	createAgent := func(name string) {
		agentDir := filepath.Join(agentsDir, name)
		err := os.MkdirAll(agentDir, 0755)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(agentDir, "scion-agent.json"))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	createAgent("agent1")
	createAgent("agent2")
	createAgent("foobar")

	// Create a directory that is NOT an agent
	err = os.MkdirAll(filepath.Join(agentsDir, "not-an-agent"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	// Mock command
	cmd := &cobra.Command{}
	cmd.Flags().String("grove", "", "")
	cmd.Flags().Bool("global", false, "")

	// Test with explicit grove path
	cmd.Flags().Set("grove", groveDir)

	t.Run("Complete all agents", func(t *testing.T) {
		names, _ := getAgentNames(cmd, []string{}, "")
		assert.Contains(t, names, "agent1")
		assert.Contains(t, names, "agent2")
		assert.Contains(t, names, "foobar")
		assert.NotContains(t, names, "not-an-agent")
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
}
