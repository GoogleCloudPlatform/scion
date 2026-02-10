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

package hubsync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncResult_IsInSync(t *testing.T) {
	tests := []struct {
		name     string
		result   SyncResult
		expected bool
	}{
		{
			name: "empty result is in sync",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				InSync:     nil,
			},
			expected: true,
		},
		{
			name: "only in sync agents",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				InSync:     []string{"agent1", "agent2"},
			},
			expected: true,
		},
		{
			name: "agents to register",
			result: SyncResult{
				ToRegister: []string{"new-agent"},
				ToRemove:   nil,
				InSync:     []string{"agent1"},
			},
			expected: false,
		},
		{
			name: "agents to remove",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   []AgentRef{{Name: "old-agent", ID: "old-agent-id"}},
				InSync:     []string{"agent1"},
			},
			expected: false,
		},
		{
			name: "both register and remove",
			result: SyncResult{
				ToRegister: []string{"new-agent"},
				ToRemove:   []AgentRef{{Name: "old-agent", ID: "old-agent-id"}},
				InSync:     nil,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.IsInSync(); got != tt.expected {
				t.Errorf("IsInSync() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestGetLocalAgents(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "hubsync-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create agents directory structure
	agentsDir := filepath.Join(tmpDir, "agents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		t.Fatalf("Failed to create agents dir: %v", err)
	}

	// Create agent1 with YAML config
	agent1Dir := filepath.Join(agentsDir, "agent1")
	if err := os.MkdirAll(agent1Dir, 0755); err != nil {
		t.Fatalf("Failed to create agent1 dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agent1Dir, "scion-agent.yaml"), []byte("harness: claude"), 0644); err != nil {
		t.Fatalf("Failed to write agent1 config: %v", err)
	}

	// Create agent2 with JSON config
	agent2Dir := filepath.Join(agentsDir, "agent2")
	if err := os.MkdirAll(agent2Dir, 0755); err != nil {
		t.Fatalf("Failed to create agent2 dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agent2Dir, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("Failed to write agent2 config: %v", err)
	}

	// Create a directory without config (should be ignored)
	orphanDir := filepath.Join(agentsDir, "orphan")
	if err := os.MkdirAll(orphanDir, 0755); err != nil {
		t.Fatalf("Failed to create orphan dir: %v", err)
	}

	// Test GetLocalAgents
	agents, err := GetLocalAgents(tmpDir)
	if err != nil {
		t.Fatalf("GetLocalAgents failed: %v", err)
	}

	if len(agents) != 2 {
		t.Errorf("Expected 2 agents, got %d", len(agents))
	}

	// Check that both agents are found
	agentMap := make(map[string]bool)
	for _, a := range agents {
		agentMap[a] = true
	}

	if !agentMap["agent1"] {
		t.Error("Expected to find agent1")
	}
	if !agentMap["agent2"] {
		t.Error("Expected to find agent2")
	}
	if agentMap["orphan"] {
		t.Error("Should not find orphan directory")
	}
}

func TestGetLocalAgents_EmptyDir(t *testing.T) {
	// Create a temporary directory without agents
	tmpDir, err := os.MkdirTemp("", "hubsync-test-empty-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agents, err := GetLocalAgents(tmpDir)
	if err != nil {
		t.Fatalf("GetLocalAgents failed: %v", err)
	}

	if len(agents) != 0 {
		t.Errorf("Expected 0 agents, got %d", len(agents))
	}
}

func TestGetLocalAgents_NoDir(t *testing.T) {
	// Test with a path that doesn't exist
	agents, err := GetLocalAgents("/nonexistent/path")
	if err != nil {
		t.Fatalf("GetLocalAgents should not error on missing dir: %v", err)
	}

	if len(agents) != 0 {
		t.Errorf("Expected 0 agents for nonexistent path, got %d", len(agents))
	}
}

func TestSyncResult_ExcludeAgent(t *testing.T) {
	tests := []struct {
		name           string
		result         SyncResult
		excludeAgent   string
		expectedSync   bool
		expectedRegLen int
		expectedRemLen int
	}{
		{
			name: "exclude agent from ToRegister",
			result: SyncResult{
				ToRegister: []string{"agent1", "agent2"},
				ToRemove:   []AgentRef{},
				InSync:     []string{"agent3"},
			},
			excludeAgent:   "agent1",
			expectedSync:   false, // still has agent2 to register
			expectedRegLen: 1,
			expectedRemLen: 0,
		},
		{
			name: "exclude agent from ToRemove",
			result: SyncResult{
				ToRegister: []string{},
				ToRemove:   []AgentRef{{Name: "agent1", ID: "id1"}, {Name: "agent2", ID: "id2"}},
				InSync:     []string{"agent3"},
			},
			excludeAgent:   "agent1",
			expectedSync:   false, // still has agent2 to remove
			expectedRegLen: 0,
			expectedRemLen: 1,
		},
		{
			name: "exclude only agent in ToRegister makes it in sync",
			result: SyncResult{
				ToRegister: []string{"agent1"},
				ToRemove:   []AgentRef{},
				InSync:     []string{"agent2"},
			},
			excludeAgent:   "agent1",
			expectedSync:   true,
			expectedRegLen: 0,
			expectedRemLen: 0,
		},
		{
			name: "exclude only agent in ToRemove makes it in sync",
			result: SyncResult{
				ToRegister: []string{},
				ToRemove:   []AgentRef{{Name: "agent1", ID: "id1"}},
				InSync:     []string{"agent2"},
			},
			excludeAgent:   "agent1",
			expectedSync:   true,
			expectedRegLen: 0,
			expectedRemLen: 0,
		},
		{
			name: "exclude agent from both lists",
			result: SyncResult{
				ToRegister: []string{"agent1"},
				ToRemove:   []AgentRef{{Name: "agent1", ID: "id1"}}, // unlikely but test the logic
				InSync:     []string{},
			},
			excludeAgent:   "agent1",
			expectedSync:   true,
			expectedRegLen: 0,
			expectedRemLen: 0,
		},
		{
			name: "exclude non-existent agent has no effect",
			result: SyncResult{
				ToRegister: []string{"agent1"},
				ToRemove:   []AgentRef{{Name: "agent2", ID: "id2"}},
				InSync:     []string{},
			},
			excludeAgent:   "agent3",
			expectedSync:   false,
			expectedRegLen: 1,
			expectedRemLen: 1,
		},
		{
			name: "empty exclude agent has no effect",
			result: SyncResult{
				ToRegister: []string{"agent1"},
				ToRemove:   []AgentRef{{Name: "agent2", ID: "id2"}},
				InSync:     []string{},
			},
			excludeAgent:   "",
			expectedSync:   false,
			expectedRegLen: 1,
			expectedRemLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := tt.result.ExcludeAgent(tt.excludeAgent)
			if filtered.IsInSync() != tt.expectedSync {
				t.Errorf("IsInSync() = %v, want %v", filtered.IsInSync(), tt.expectedSync)
			}
			if len(filtered.ToRegister) != tt.expectedRegLen {
				t.Errorf("len(ToRegister) = %d, want %d", len(filtered.ToRegister), tt.expectedRegLen)
			}
			if len(filtered.ToRemove) != tt.expectedRemLen {
				t.Errorf("len(ToRemove) = %d, want %d", len(filtered.ToRemove), tt.expectedRemLen)
			}
		})
	}
}

func TestSyncResult_PendingNotAffectIsInSync(t *testing.T) {
	// Pending agents should not affect the IsInSync check
	tests := []struct {
		name     string
		result   SyncResult
		expected bool
	}{
		{
			name: "only pending agents is in sync",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				Pending:    []AgentRef{{Name: "pending-agent", ID: "pending-id"}},
				InSync:     nil,
			},
			expected: true, // Pending agents don't require sync
		},
		{
			name: "pending with in sync agents",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				Pending:    []AgentRef{{Name: "pending-agent", ID: "pending-id"}},
				InSync:     []string{"agent1"},
			},
			expected: true,
		},
		{
			name: "pending with agents to register",
			result: SyncResult{
				ToRegister: []string{"new-agent"},
				ToRemove:   nil,
				Pending:    []AgentRef{{Name: "pending-agent", ID: "pending-id"}},
				InSync:     nil,
			},
			expected: false, // ToRegister requires action
		},
		{
			name: "pending with agents to remove",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   []AgentRef{{Name: "old-agent", ID: "old-id"}},
				Pending:    []AgentRef{{Name: "pending-agent", ID: "pending-id"}},
				InSync:     nil,
			},
			expected: false, // ToRemove requires action
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.IsInSync(); got != tt.expected {
				t.Errorf("IsInSync() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSyncResult_ExcludeAgent_WithPending(t *testing.T) {
	result := SyncResult{
		ToRegister: []string{"agent1"},
		ToRemove:   []AgentRef{{Name: "agent2", ID: "id2"}},
		Pending:    []AgentRef{{Name: "pending1", ID: "p1"}, {Name: "pending2", ID: "p2"}},
		InSync:     []string{"agent3"},
	}

	// Exclude a pending agent
	filtered := result.ExcludeAgent("pending1")

	if len(filtered.Pending) != 1 {
		t.Errorf("Expected 1 pending agent, got %d", len(filtered.Pending))
	}
	if len(filtered.Pending) > 0 && filtered.Pending[0].Name != "pending2" {
		t.Errorf("Expected pending2, got %s", filtered.Pending[0].Name)
	}

	// Original lists should be unchanged
	if len(filtered.ToRegister) != 1 {
		t.Errorf("Expected 1 ToRegister agent, got %d", len(filtered.ToRegister))
	}
	if len(filtered.ToRemove) != 1 {
		t.Errorf("Expected 1 ToRemove agent, got %d", len(filtered.ToRemove))
	}
}

func TestContainsIgnoreCase(t *testing.T) {
	tests := []struct {
		s        string
		substr   string
		expected bool
	}{
		{"Hello World", "hello", true},
		{"Hello World", "WORLD", true},
		{"Hello World", "llo wor", true},
		{"404 Not Found", "404", true},
		{"404 Not Found", "not found", true},
		{"Hello World", "goodbye", false},
		{"", "test", false},
		{"test", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.s+"_"+tt.substr, func(t *testing.T) {
			if got := containsIgnoreCase(tt.s, tt.substr); got != tt.expected {
				t.Errorf("containsIgnoreCase(%q, %q) = %v, want %v", tt.s, tt.substr, got, tt.expected)
			}
		})
	}
}

func TestGroveChoice_Constants(t *testing.T) {
	// Verify that the choice constants have expected values
	if GroveChoiceCancel != 0 {
		t.Errorf("GroveChoiceCancel should be 0, got %d", GroveChoiceCancel)
	}
	if GroveChoiceLink != 1 {
		t.Errorf("GroveChoiceLink should be 1, got %d", GroveChoiceLink)
	}
	if GroveChoiceRegisterNew != 2 {
		t.Errorf("GroveChoiceRegisterNew should be 2, got %d", GroveChoiceRegisterNew)
	}
}

func TestGroveMatch_Fields(t *testing.T) {
	match := GroveMatch{
		ID:        "test-id",
		Name:      "test-grove",
		GitRemote: "github.com/test/repo",
	}

	if match.ID != "test-id" {
		t.Errorf("Expected ID 'test-id', got %s", match.ID)
	}
	if match.Name != "test-grove" {
		t.Errorf("Expected Name 'test-grove', got %s", match.Name)
	}
	if match.GitRemote != "github.com/test/repo" {
		t.Errorf("Expected GitRemote 'github.com/test/repo', got %s", match.GitRemote)
	}
}
