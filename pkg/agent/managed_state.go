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

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const managedAgentStateFile = "managed-agent-state.json"

// ManagedAgentState persists the cloud-side identifiers and interaction chain
// for a managed agent so that the local CLI can reconnect across restarts.
type ManagedAgentState struct {
	CloudAgentID        string `json:"cloud_agent_id"`
	CloudProvider       string `json:"cloud_provider"`
	APIKeyRef           string `json:"api_key_ref,omitempty"`
	LatestInteractionID string `json:"latest_interaction_id,omitempty"`
	LatestEnvironmentID string `json:"latest_environment_id,omitempty"`
	// InteractionChain grows unbounded in v1; a future version should cap or
	// rotate old entries for long-lived agents.
	InteractionChain []string `json:"interaction_chain,omitempty"`
	LastStatus       string   `json:"last_status,omitempty"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
}

// LoadManagedAgentState reads the managed agent state from the given agent directory.
func LoadManagedAgentState(agentDir string) (*ManagedAgentState, error) {
	data, err := os.ReadFile(filepath.Join(agentDir, managedAgentStateFile))
	if err != nil {
		return nil, fmt.Errorf("loading managed agent state: %w", err)
	}
	var s ManagedAgentState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing managed agent state: %w", err)
	}
	return &s, nil
}

// SaveManagedAgentState writes the managed agent state atomically to the given agent directory.
func SaveManagedAgentState(agentDir string, s *ManagedAgentState) error {
	s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling managed agent state: %w", err)
	}
	target := filepath.Join(agentDir, managedAgentStateFile)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing managed agent state: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("renaming managed agent state: %w", err)
	}
	return nil
}

// managedAgentDir resolves the local agent directory from agent name and project path.
func managedAgentDir(agentName, projectPath string) (string, error) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", fmt.Errorf("resolving project dir: %w", err)
	}
	return filepath.Join(projectDir, "agents", agentName), nil
}
