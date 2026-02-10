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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/harness"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/ptone/scion-agent/pkg/util"
)

type Manager interface {
	// Provision prepares the agent directory and configuration without starting it
	Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error)

	// Start launches a new agent with the given configuration
	Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error)

	// Stop terminates an agent
	Stop(ctx context.Context, agentID string) error

	// Delete terminates and removes an agent
	Delete(ctx context.Context, agentID string, deleteFiles bool, grovePath string, removeBranch bool) (bool, error)

	// List returns active agents
	List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error)

	// Message sends a message to an agent's harness via tmux
	Message(ctx context.Context, agentID string, message string, interrupt bool) error

	// Watch returns a channel of status updates for an agent
	Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error)
}

type AgentManager struct {
	Runtime runtime.Runtime
}

func NewManager(rt runtime.Runtime) Manager {
	return &AgentManager{
		Runtime: rt,
	}
}

func (m *AgentManager) Stop(ctx context.Context, agentID string) error {
	return m.Runtime.Stop(ctx, agentID)
}

func (m *AgentManager) Delete(ctx context.Context, agentID string, deleteFiles bool, grovePath string, removeBranch bool) (bool, error) {
	// 1. Check if container exists
	// We use name filter if possible, but runtime.List might take map[string]string
	agents, err := m.Runtime.List(ctx, map[string]string{"scion.name": agentID})
	containerExists := false
	var targetID string
	if err == nil {
		for _, a := range agents {
			if a.Name == agentID || a.ContainerID == agentID || strings.TrimPrefix(a.Name, "/") == agentID {
				containerExists = true
				targetID = a.ContainerID
				break
			}
		}
	}

	if containerExists {
		util.Debugf("delete: starting runtime delete for container %s", targetID)
		if err := m.Runtime.Delete(ctx, targetID); err != nil {
			return false, fmt.Errorf("failed to delete container: %w", err)
		}
		util.Debugf("delete: runtime delete completed for container %s", targetID)
	}

	if deleteFiles {
		util.Debugf("delete: starting filesystem cleanup for agent %s", agentID)
		branchDeleted, err := DeleteAgentFiles(agentID, grovePath, removeBranch)
		util.Debugf("delete: filesystem cleanup completed for agent %s", agentID)
		return branchDeleted, err
	}
	return false, nil
}

func (m *AgentManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, fmt.Errorf("Watch not implemented")
}

func (m *AgentManager) Message(ctx context.Context, agentID string, message string, interrupt bool) error {
	// 1. Find the agent
	agents, err := m.List(ctx, nil)
	if err != nil {
		return err
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if a.Name == agentID || a.ContainerID == agentID || strings.TrimPrefix(a.Name, "/") == agentID {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' not found or not running", agentID)
	}

	if agent.Labels["scion.tmux"] != "true" {
		return fmt.Errorf("agent '%s' was not started with tmux support, cannot send message", agentID)
	}

	// 2. Resolve harness
	harnessName := "generic"
	if agent.GrovePath != "" {
		scionJSON := filepath.Join(agent.GrovePath, "agents", agent.Name, "scion-agent.json")
		if data, err := os.ReadFile(scionJSON); err == nil {
			var cfg api.ScionConfig
			if err := json.Unmarshal(data, &cfg); err == nil && cfg.Harness != "" {
				harnessName = cfg.Harness
			}
		}
	}
	h := harness.New(harnessName)

	// 3. Prepare commands
	var cmds [][]string

	if interrupt {
		key := h.GetInterruptKey()
		cmds = append(cmds, []string{"tmux", "send-keys", "-t", "scion", key})
	}

	// tmux send-keys -t scion "message" Enter
	cmds = append(cmds, []string{"tmux", "send-keys", "-t", "scion", message, "Enter"})
	cmds = append(cmds, []string{"tmux", "send-keys", "-t", "scion", "Enter"})

	// 4. Execute
	for _, cmd := range cmds {
		_, err := m.Runtime.Exec(ctx, agent.ContainerID, cmd)
		if err != nil {
			return fmt.Errorf("failed to send message to agent '%s': %w", agent.Name, err)
		}
	}

	return nil
}
