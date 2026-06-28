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
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
)

// ManagedAgentManager implements the Manager interface for cloud-managed agents.
// It delegates agent lifecycle operations to a ManagedAgentBackend (e.g. Google)
// instead of managing local containers.
type ManagedAgentManager struct {
	Backend  managedagent.ManagedAgentBackend
	stateDir string
}

// NewManagedAgentManager creates a Manager backed by a cloud managed-agent service.
func NewManagedAgentManager(backend managedagent.ManagedAgentBackend, stateDir string) Manager {
	return &ManagedAgentManager{
		Backend:  backend,
		stateDir: stateDir,
	}
}

func (m *ManagedAgentManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return nil, fmt.Errorf("resolving project dir: %w", err)
	}

	agentDir := filepath.Join(projectDir, "agents", opts.Name)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, fmt.Errorf("creating agent directory: %w", err)
	}

	// Resolve template chain and build scion config
	templateName := opts.Template
	if templateName == "" {
		templateName = "default"
	}

	finalCfg := &api.ScionConfig{}

	chain, err := config.GetTemplateChainInProject(templateName, opts.ProjectPath)
	if err != nil {
		slog.Debug("managed: template chain not found, using empty config", "template", templateName, "err", err)
	} else {
		for _, tpl := range chain {
			tplCfg, loadErr := tpl.LoadConfig()
			if loadErr != nil {
				return nil, fmt.Errorf("loading template config %s: %w", tpl.Name, loadErr)
			}
			finalCfg = config.MergeScionConfig(finalCfg, tplCfg)
		}
	}

	if opts.InlineConfig != nil {
		finalCfg = config.MergeScionConfig(finalCfg, opts.InlineConfig)
	}

	projectName := config.GetProjectName(projectDir)
	displayTemplateName := templateName
	if len(chain) > 0 {
		displayTemplateName = chain[len(chain)-1].Name
	}

	info := &api.AgentInfo{
		Name:        opts.Name,
		Template:    displayTemplateName,
		Project:     projectName,
		ProjectPath: projectDir,
		Phase:       "created",
		Runtime:     "managed:" + m.Backend.Name(),
		Created:     time.Now(),
	}
	finalCfg.Info = info

	cfgData, err := json.MarshalIndent(finalCfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling agent config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), cfgData, 0644); err != nil {
		return nil, fmt.Errorf("writing agent config: %w", err)
	}

	agentHome := config.GetAgentHomePath(projectDir, opts.Name)
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		return nil, fmt.Errorf("creating agent home: %w", err)
	}

	infoData, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling agent info: %w", err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0644); err != nil {
		return nil, fmt.Errorf("writing agent info: %w", err)
	}

	return finalCfg, nil
}

func (m *ManagedAgentManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return nil, fmt.Errorf("resolving project dir: %w", err)
	}
	agentDir := filepath.Join(projectDir, "agents", opts.Name)

	// Provision if the agent directory does not exist yet.
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		if _, provErr := m.Provision(ctx, opts); provErr != nil {
			return nil, fmt.Errorf("provisioning managed agent: %w", provErr)
		}
	}

	// Resolve system instruction from template config.
	scionJSON := filepath.Join(agentDir, "scion-agent.json")
	var systemInstruction string
	if data, readErr := os.ReadFile(scionJSON); readErr == nil {
		var cfg api.ScionConfig
		if json.Unmarshal(data, &cfg) == nil {
			systemInstruction = cfg.SystemPrompt
			if systemInstruction == "" {
				systemInstruction = cfg.AgentInstructions
			}
		}
	}

	// Create the cloud-side agent.
	cloudAgentID, err := m.Backend.CreateAgent(ctx, managedagent.CreateAgentConfig{
		SystemInstruction: systemInstruction,
	})
	if err != nil {
		return nil, fmt.Errorf("creating cloud agent: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	agentState := &ManagedAgentState{
		CloudAgentID:  cloudAgentID,
		CloudProvider: m.Backend.Name(),
		CreatedAt:     now,
	}

	// If a task was provided, create the first interaction.
	if opts.Task != "" {
		handle, err := m.Backend.CreateInteraction(ctx, managedagent.InteractionRequest{
			CloudAgentID: cloudAgentID,
			Input:        opts.Task,
			Background:   true,
		})
		if err != nil {
			return nil, fmt.Errorf("creating initial interaction: %w", err)
		}
		agentState.LatestInteractionID = handle.InteractionID
		agentState.LatestEnvironmentID = handle.EnvironmentID
		agentState.InteractionChain = []string{handle.InteractionID}
		agentState.LastStatus = string(handle.Status)
	}

	if err := SaveManagedAgentState(agentDir, agentState); err != nil {
		return nil, fmt.Errorf("saving managed agent state: %w", err)
	}

	// Update agent-info.json with running phase.
	info := &api.AgentInfo{
		Name:        opts.Name,
		Project:     config.GetProjectName(projectDir),
		ProjectPath: projectDir,
		Phase:       "running",
		Runtime:     "managed:" + m.Backend.Name(),
		Created:     time.Now(),
	}

	agentHome := config.GetAgentHomePath(projectDir, opts.Name)
	infoPath := filepath.Join(agentHome, "agent-info.json")
	if existingData, readErr := os.ReadFile(infoPath); readErr == nil {
		var existing api.AgentInfo
		if json.Unmarshal(existingData, &existing) == nil {
			existing.Phase = "running"
			existing.Runtime = "managed:" + m.Backend.Name()
			info = &existing
		}
	}

	infoData, _ := json.MarshalIndent(info, "", "  ")
	_ = os.WriteFile(infoPath, infoData, 0644)

	return info, nil
}

func (m *ManagedAgentManager) Stop(ctx context.Context, agentID string, projectPath string) error {
	agentDir, err := managedAgentDir(agentID, projectPath)
	if err != nil {
		return err
	}

	agentState, err := LoadManagedAgentState(agentDir)
	if err != nil {
		return fmt.Errorf("loading managed agent state: %w", err)
	}

	// Cancel the active interaction if one is in progress.
	if agentState.LatestInteractionID != "" {
		interactionState, getErr := m.Backend.GetInteraction(ctx, agentState.LatestInteractionID)
		if getErr == nil && interactionState.Status == managedagent.StatusInProgress {
			if cancelErr := m.Backend.CancelInteraction(ctx, agentState.LatestInteractionID); cancelErr != nil {
				slog.Warn("failed to cancel interaction", "interaction_id", agentState.LatestInteractionID, "err", cancelErr)
			}
		}
	}

	agentState.LastStatus = string(managedagent.StatusCancelled)
	if err := SaveManagedAgentState(agentDir, agentState); err != nil {
		slog.Warn("failed to save state after stop", "err", err)
	}

	projectDir, _ := config.GetResolvedProjectDir(projectPath)
	if projectDir != "" {
		agentHome := config.GetAgentHomePath(projectDir, agentID)
		_ = persistAgentInfoState(filepath.Join(agentHome, "agent-info.json"), "stopped", "")
	}

	return nil
}

func (m *ManagedAgentManager) Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	// Stop first (best-effort).
	_ = m.Stop(ctx, agentID, projectPath)

	agentDir, err := managedAgentDir(agentID, projectPath)
	if err != nil {
		if deleteFiles {
			return false, err
		}
		return false, nil
	}

	agentState, loadErr := LoadManagedAgentState(agentDir)
	if loadErr == nil && agentState.CloudAgentID != "" {
		if delErr := m.Backend.DeleteAgent(ctx, agentState.CloudAgentID); delErr != nil {
			slog.Warn("failed to delete cloud agent", "cloud_agent_id", agentState.CloudAgentID, "err", delErr)
		}
	}

	if deleteFiles {
		if err := os.RemoveAll(agentDir); err != nil {
			return false, fmt.Errorf("removing agent directory: %w", err)
		}
		projectDir, _ := config.GetResolvedProjectDir(projectPath)
		if projectDir != "" {
			agentHome := config.GetAgentHomePath(projectDir, agentID)
			_ = os.RemoveAll(agentHome)
		}
	}

	return false, nil
}

func (m *ManagedAgentManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	var projectsToScan []string

	projectPath := filter["scion.project_path"]
	if projectPath == "" {
		projectPath = filter["scion.grove_path"]
	}
	if projectPath != "" {
		projectsToScan = append(projectsToScan, projectPath)
	} else if len(filter) == 0 || (len(filter) == 1 && filter["scion.agent"] == "true") {
		pd, _ := config.GetResolvedProjectDir("")
		if pd != "" {
			projectsToScan = append(projectsToScan, pd)
		}
	}

	var agents []api.AgentInfo
	for _, projectDir := range projectsToScan {
		agentsDir := filepath.Join(projectDir, "agents")
		entries, err := os.ReadDir(agentsDir)
		if err != nil {
			continue
		}
		projectName := config.GetProjectName(projectDir)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			agentDir := filepath.Join(agentsDir, e.Name())
			stateFile := filepath.Join(agentDir, managedAgentStateFile)
			if _, err := os.Stat(stateFile); err != nil {
				continue
			}

			agentState, loadErr := LoadManagedAgentState(agentDir)
			if loadErr != nil {
				continue
			}

			info := api.AgentInfo{
				Name:        e.Name(),
				Project:     projectName,
				ProjectPath: projectDir,
				Runtime:     "managed:" + agentState.CloudProvider,
				Phase:       "running",
			}

			agentHome := config.GetAgentHomePath(projectDir, e.Name())
			infoPath := filepath.Join(agentHome, "agent-info.json")
			if data, readErr := os.ReadFile(infoPath); readErr == nil {
				var savedInfo api.AgentInfo
				if json.Unmarshal(data, &savedInfo) == nil {
					info.Phase = savedInfo.Phase
					info.Activity = savedInfo.Activity
					info.Template = savedInfo.Template
					info.HarnessConfig = savedInfo.HarnessConfig
					info.Profile = savedInfo.Profile
					info.Detail = savedInfo.Detail
					info.Created = savedInfo.Created
				}
			}

			agents = append(agents, info)
		}
	}

	return agents, nil
}

func (m *ManagedAgentManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	projectPath := ""
	if projectID != "" {
		projectPath = projectID
	}
	agentDir, err := managedAgentDir(agentID, projectPath)
	if err != nil {
		return err
	}

	agentState, err := LoadManagedAgentState(agentDir)
	if err != nil {
		return fmt.Errorf("loading managed agent state: %w", err)
	}

	if interrupt && agentState.LatestInteractionID != "" {
		if cancelErr := m.Backend.CancelInteraction(ctx, agentState.LatestInteractionID); cancelErr != nil {
			slog.Warn("failed to cancel interaction for interrupt", "err", cancelErr)
		}
	}

	if message == "" {
		return nil
	}

	req := managedagent.InteractionRequest{
		CloudAgentID:          agentState.CloudAgentID,
		Input:                 message,
		PreviousInteractionID: agentState.LatestInteractionID,
		EnvironmentID:         agentState.LatestEnvironmentID,
		Background:            true,
	}

	handle, err := m.Backend.CreateInteraction(ctx, req)
	if err != nil {
		return fmt.Errorf("creating interaction: %w", err)
	}

	agentState.LatestInteractionID = handle.InteractionID
	if handle.EnvironmentID != "" {
		agentState.LatestEnvironmentID = handle.EnvironmentID
	}
	agentState.InteractionChain = append(agentState.InteractionChain, handle.InteractionID)
	agentState.LastStatus = string(handle.Status)

	if err := SaveManagedAgentState(agentDir, agentState); err != nil {
		return fmt.Errorf("saving managed agent state: %w", err)
	}

	return nil
}

func (m *ManagedAgentManager) MessageRaw(_ context.Context, _, _ string, _ string) error {
	return fmt.Errorf("MessageRaw is not supported for managed agents")
}

func (m *ManagedAgentManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	agentDir, err := managedAgentDir(agentID, m.stateDir)
	if err != nil {
		return nil, err
	}

	agentState, err := LoadManagedAgentState(agentDir)
	if err != nil {
		return nil, fmt.Errorf("loading managed agent state: %w", err)
	}

	if agentState.LatestInteractionID == "" {
		return nil, fmt.Errorf("no active interaction for agent %s", agentID)
	}

	ch := make(chan api.StatusEvent, 16)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				interactionState, err := m.Backend.GetInteraction(ctx, agentState.LatestInteractionID)
				if err != nil {
					ch <- api.StatusEvent{
						AgentID:   agentID,
						Status:    "error",
						Message:   err.Error(),
						Timestamp: time.Now().UTC().Format(time.RFC3339),
					}
					return
				}

				status := mapInteractionStatus(interactionState.Status)
				ch <- api.StatusEvent{
					AgentID:   agentID,
					Status:    status,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
				}

				if isTerminalStatus(interactionState.Status) {
					return
				}
			}
		}
	}()

	return ch, nil
}

func (m *ManagedAgentManager) Close() {}

func mapInteractionStatus(s managedagent.InteractionStatus) string {
	switch s {
	case managedagent.StatusInProgress:
		return "running"
	case managedagent.StatusRequiresAction:
		return "waiting_for_input"
	case managedagent.StatusCompleted:
		return "completed"
	case managedagent.StatusFailed:
		return "error"
	case managedagent.StatusCancelled:
		return "stopped"
	case managedagent.StatusIncomplete:
		return "error"
	default:
		return string(s)
	}
}

func isTerminalStatus(s managedagent.InteractionStatus) bool {
	switch s {
	case managedagent.StatusCompleted, managedagent.StatusFailed,
		managedagent.StatusCancelled, managedagent.StatusIncomplete:
		return true
	}
	return false
}
