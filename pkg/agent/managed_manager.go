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

	now := time.Now().UTC().Format(time.RFC3339)
	agentState := &ManagedAgentState{
		CloudProvider: m.Backend.Name(),
		CreatedAt:     now,
	}

	// Create the first interaction directly using the interactions API.
	// We skip the agents CRUD endpoint (/v1beta/agents) because it may
	// not be generally available; the interactions endpoint works with
	// both model names and built-in agent names.
	if opts.Task != "" {
		handle, err := m.Backend.CreateInteraction(ctx, managedagent.InteractionRequest{
			Input:             opts.Task,
			SystemInstruction: systemInstruction,
			Environment:       &managedagent.EnvironmentConfig{Type: "remote"},
			Background:        true,
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

	// Monitor interaction completion asynchronously.
	if agentState.LatestInteractionID != "" {
		go m.monitorInteraction(opts.Name, projectDir, agentDir, agentState.LatestInteractionID)
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

	infoData, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		slog.Warn("failed to marshal agent-info.json", "agent", opts.Name, "err", err)
	} else if writeErr := os.WriteFile(infoPath, infoData, 0644); writeErr != nil {
		slog.Warn("failed to write agent-info.json", "agent", opts.Name, "err", writeErr)
	}

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

	// Update agent-info.json to reflect that the agent is working on the new message.
	projectDir, _ := config.GetResolvedProjectDir(projectPath)
	if projectDir != "" {
		agentHome := config.GetAgentHomePath(projectDir, agentID)
		infoPath := filepath.Join(agentHome, "agent-info.json")
		if err := persistAgentInfoState(infoPath, "running", "working"); err != nil {
			slog.Warn("failed to update agent-info after message", "agent", agentID, "err", err)
		}
	}

	// Monitor interaction completion asynchronously and update agent-info.json
	// when the interaction reaches a terminal state.
	if projectDir != "" {
		go m.monitorInteraction(agentID, projectDir, agentDir, handle.InteractionID)
	}

	return nil
}

func (m *ManagedAgentManager) MessageRaw(_ context.Context, _, _ string, _ string) error {
	return fmt.Errorf("MessageRaw is not supported for managed agents")
}

// Watch uses m.stateDir as the project path to resolve the agent directory.
// Callers must ensure stateDir was set to the project path at construction time.
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

// monitorInteraction polls the backend for interaction status and updates
// agent-info.json when the interaction reaches a terminal state. Runs as a
// background goroutine launched by Start and Message.
func (m *ManagedAgentManager) monitorInteraction(agentID, projectDir, agentDir, interactionID string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	ctx := context.Background()
	for range ticker.C {
		state, err := m.Backend.GetInteraction(ctx, interactionID)
		if err != nil {
			slog.Warn("interaction monitor: poll failed", "agent", agentID, "interaction", interactionID, "err", err)
			continue
		}

		if !isTerminalStatus(state.Status) {
			continue
		}

		activity := mapInteractionStatus(state.Status)
		phase := "running"
		if state.Status == managedagent.StatusCancelled || state.Status == managedagent.StatusFailed {
			phase = "stopped"
		}

		agentHome := config.GetAgentHomePath(projectDir, agentID)
		infoPath := filepath.Join(agentHome, "agent-info.json")
		if err := persistAgentInfoState(infoPath, phase, activity); err != nil {
			slog.Warn("interaction monitor: failed to update agent-info", "agent", agentID, "err", err)
		}

		// Update managed-agent-state with the final status.
		if agentState, loadErr := LoadManagedAgentState(agentDir); loadErr == nil {
			agentState.LastStatus = string(state.Status)
			if saveErr := SaveManagedAgentState(agentDir, agentState); saveErr != nil {
				slog.Warn("interaction monitor: failed to save state", "agent", agentID, "err", saveErr)
			}
		}
		return
	}
}

func (m *ManagedAgentManager) Close() {}

func mapInteractionStatus(s managedagent.InteractionStatus) string {
	switch s {
	case managedagent.StatusInProgress:
		return "running"
	case managedagent.StatusRequiresAction:
		// requires_action is unexpected for managed agents — the cloud service
		// should handle tool calls internally. Log a warning so operators notice.
		slog.Warn("unexpected requires_action status from managed agent backend", "status", s)
		return "error"
	case managedagent.StatusCompleted:
		return "completed"
	case managedagent.StatusFailed:
		return "error"
	case managedagent.StatusCancelled:
		return "stopped"
	case managedagent.StatusIncomplete:
		return "limits_exceeded"
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
