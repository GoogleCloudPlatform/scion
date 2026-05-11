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
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentDispatcherAdapter adapts the agent.Manager to the hub.AgentDispatcher interface.
// This enables the Hub to dispatch agent creation to a co-located runtime broker.
type agentDispatcherAdapter struct {
	manager  agent.Manager
	store    store.Store
	brokerID string // The ID of this runtime broker
}

// newAgentDispatcherAdapter creates a new dispatcher adapter.
func newAgentDispatcherAdapter(mgr agent.Manager, s store.Store, brokerID string) *agentDispatcherAdapter {
	return &agentDispatcherAdapter{
		manager:  mgr,
		store:    s,
		brokerID: brokerID,
	}
}

// DispatchAgentCreate implements hub.AgentDispatcher.
// It starts the agent on the runtime broker and updates the hub store with runtime info.
func (d *agentDispatcherAdapter) DispatchAgentCreate(ctx context.Context, hubAgent *store.Agent) error {
	projectPath := d.resolveProjectPath(ctx, hubAgent.ProjectID)
	opts := d.buildStartOptions(hubAgent, projectPath, false)

	// Ensure grove ID label is present for tracking
	if hubAgent.Labels == nil {
		hubAgent.Labels = make(map[string]string)
	}
	hubAgent.Labels["scion.project"] = hubAgent.ProjectID
	hubAgent.Labels["scion.grove"] = hubAgent.ProjectID

	// Start the agent on the runtime broker
	agentInfo, err := d.manager.Start(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to start agent: %w", err)
	}

	// Update the hub agent record with runtime information
	hubAgent.Phase = string(state.PhaseRunning)
	hubAgent.ContainerStatus = agentInfo.ContainerStatus
	if agentInfo.ID != "" {
		hubAgent.RuntimeState = "container:" + agentInfo.ID
	}
	hubAgent.LastSeen = time.Now()

	if err := d.store.UpdateAgent(ctx, hubAgent); err != nil {
		log.Printf("Warning: failed to update agent with runtime info: %v", err)
	}

	return nil
}

// DispatchAgentStart implements hub.AgentDispatcher.
// For co-located runtime brokers, this resumes a stopped agent.
func (d *agentDispatcherAdapter) DispatchAgentStart(ctx context.Context, hubAgent *store.Agent, task string) error {
	projectPath := d.resolveProjectPath(ctx, hubAgent.ProjectID)
	opts := d.buildStartOptions(hubAgent, projectPath, true)

	// Ensure grove ID label is present for tracking
	if hubAgent.Labels == nil {
		hubAgent.Labels = make(map[string]string)
	}
	hubAgent.Labels["scion.project"] = hubAgent.ProjectID
	hubAgent.Labels["scion.grove"] = hubAgent.ProjectID
	if task != "" {
		opts.Task = task
	}

	// Start the agent on the runtime broker
	agentInfo, err := d.manager.Start(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to start agent: %w", err)
	}

	// Update the hub agent record with runtime information
	hubAgent.Phase = string(state.PhaseRunning)
	hubAgent.ContainerStatus = agentInfo.ContainerStatus
	if agentInfo.ID != "" {
		hubAgent.RuntimeState = "container:" + agentInfo.ID
	}
	hubAgent.LastSeen = time.Now()

	if err := d.store.UpdateAgent(ctx, hubAgent); err != nil {
		log.Printf("Warning: failed to update agent with runtime info: %v", err)
	}

	return nil
}

// DispatchAgentStop implements hub.AgentDispatcher.
// It stops a running agent on the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentStop(ctx context.Context, hubAgent *store.Agent) error {
	if err := d.manager.Stop(ctx, hubAgent.Name, ""); err != nil {
		return fmt.Errorf("failed to stop agent: %w", err)
	}

	// Update the hub agent record
	hubAgent.Phase = string(state.PhaseStopped)
	hubAgent.LastSeen = time.Now()

	if err := d.store.UpdateAgent(ctx, hubAgent); err != nil {
		log.Printf("Warning: failed to update agent status: %v", err)
	}

	return nil
}

// DispatchAgentRestart implements hub.AgentDispatcher.
// It restarts an agent on the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentRestart(ctx context.Context, hubAgent *store.Agent) error {
	// Stop then start
	if err := d.manager.Stop(ctx, hubAgent.Name, ""); err != nil {
		log.Printf("Warning: failed to stop agent during restart: %v", err)
	}

	projectPath := d.resolveProjectPath(ctx, hubAgent.ProjectID)
	opts := d.buildStartOptions(hubAgent, projectPath, true)

	// Ensure grove ID label is present for tracking
	if hubAgent.Labels == nil {
		hubAgent.Labels = make(map[string]string)
	}
	hubAgent.Labels["scion.project"] = hubAgent.ProjectID
	hubAgent.Labels["scion.grove"] = hubAgent.ProjectID

	agentInfo, err := d.manager.Start(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to restart agent: %w", err)
	}

	hubAgent.Phase = string(state.PhaseRunning)
	hubAgent.ContainerStatus = agentInfo.ContainerStatus
	if agentInfo.ID != "" {
		hubAgent.RuntimeState = "container:" + agentInfo.ID
	}
	hubAgent.LastSeen = time.Now()

	if err := d.store.UpdateAgent(ctx, hubAgent); err != nil {
		log.Printf("Warning: failed to update agent with runtime info: %v", err)
	}

	return nil
}

func (d *agentDispatcherAdapter) buildStartOptions(hubAgent *store.Agent, projectPath string, resume bool) api.StartOptions {
	// Build StartOptions from the hub agent record
	env := make(map[string]string)
	if hubAgent.AppliedConfig != nil && hubAgent.AppliedConfig.Env != nil {
		env = hubAgent.AppliedConfig.Env
	}

	opts := api.StartOptions{
		Name:      hubAgent.Name,
		Template:  hubAgent.Template,
		Image:     hubAgent.Image,
		Env:       env,
		Detached:  &hubAgent.Detached,
		ProjectPath: projectPath,
		Resume:    resume,
	}

	if hubAgent.AppliedConfig != nil {
		opts.HarnessConfig = hubAgent.AppliedConfig.HarnessConfig
		if hubAgent.AppliedConfig.Task != "" {
			opts.Task = hubAgent.AppliedConfig.Task
		}
	}
	return opts
}

func (d *agentDispatcherAdapter) resolveProjectPath(ctx context.Context, projectID string) string {
	if projectID == "" || d.brokerID == "" {
		return ""
	}
	provider, err := d.store.GetProjectProvider(ctx, projectID, d.brokerID)
	if err != nil {
		log.Printf("Warning: failed to get project provider for path lookup: %v", err)
		return ""
	}
	return provider.LocalPath
}

// DispatchAgentDelete implements hub.AgentDispatcher.
// It removes an agent from the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentDelete(ctx context.Context, hubAgent *store.Agent, deleteFiles, removeBranch, _ bool, _ time.Time) error {
	// Look up the local path for this grove on this runtime broker
	var projectPath string
	if hubAgent.ProjectID != "" && d.brokerID != "" {
		provider, err := d.store.GetProjectProvider(ctx, hubAgent.ProjectID, d.brokerID)
		if err != nil {
			log.Printf("Warning: failed to get project provider for path lookup: %v", err)
		} else if provider.LocalPath != "" {
			projectPath = provider.LocalPath
		}
	}

	// For hub-native groves the provider LocalPath is typically empty.
	// Resolve from the grove slug so file cleanup can find the agent
	// directory at ~/.scion/groves/<slug>/.scion/agents/<name>.
	if projectPath == "" && hubAgent.ProjectID != "" && deleteFiles {
		grove, err := d.store.GetProject(ctx, hubAgent.ProjectID)
		if err == nil && grove.GitRemote == "" && grove.Slug != "" {
			if globalDir, gErr := config.GetGlobalDir(); gErr == nil {
				candidate := filepath.Join(globalDir, "groves", grove.Slug)
				if _, sErr := os.Stat(candidate); sErr == nil {
					projectPath = candidate
				}
			}
		}
	}

	// Stop the agent first (ignore error if already stopped)
	_ = d.manager.Stop(ctx, hubAgent.Name, "")

	// Delete the agent
	_, err := d.manager.Delete(ctx, hubAgent.Name, deleteFiles, projectPath, removeBranch)
	if err != nil {
		return fmt.Errorf("failed to delete agent: %w", err)
	}

	return nil
}

// DispatchAgentMessage implements hub.AgentDispatcher.
// It sends a message to an agent on the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentMessage(ctx context.Context, hubAgent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	// Raw messages bypass the paste buffer and send literal bytes via send-keys
	if structuredMsg != nil && structuredMsg.Raw {
		deliveryText := messages.FormatForDelivery(structuredMsg)
		if err := d.manager.MessageRaw(ctx, hubAgent.Name, hubAgent.ProjectID, deliveryText); err != nil {
			return fmt.Errorf("failed to send raw message: %w", err)
		}
		return nil
	}

	// When a structured message is provided, format it for delivery
	deliveryText := message
	if structuredMsg != nil {
		deliveryText = messages.FormatForDelivery(structuredMsg)
	}
	if err := d.manager.Message(ctx, hubAgent.Name, hubAgent.ProjectID, deliveryText, interrupt); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	return nil
}
