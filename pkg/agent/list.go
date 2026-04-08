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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const legacyFailedContainerStatusPrefix = "failed"

func (m *AgentManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	agents, err := m.Runtime.List(ctx, filter)
	if err != nil {
		return nil, err
	}

	// Also find "created" agents that don't have a container yet
	// We need to know which groves to scan.
	// If filter has scion.grove, we scan that one.
	// Otherwise, we scan current and global?

	var grovesToScan []string
	if groveName, ok := filter["scion.grove"]; ok {
		_ = groveName
		// We need to resolve groveName to a path. This is currently not easy without searching.
		// For now, if scion.grove is provided, we assume we only care about running ones
		// OR we need to be passed a grove path.
	}

	// This logic is a bit tied to how CLI uses it.
	// Let's at least support scanning a specific grove if provided in filter?
	// Or maybe Add a special filter key for GrovePath.

	grovePath := filter["scion.grove_path"]
	if grovePath != "" {
		grovesToScan = append(grovesToScan, grovePath)
	} else if len(filter) == 0 || (len(filter) == 1 && filter["scion.agent"] == "true") {
		// Default: scan current resolved project dir and global dir
		pd, _ := config.GetResolvedProjectDir("")
		if pd != "" {
			grovesToScan = append(grovesToScan, pd)
		}
		gd, _ := config.GetGlobalDir()
		if gd != "" && gd != pd {
			grovesToScan = append(grovesToScan, gd)
		}
	}

	runningNames := make(map[string]bool)
	for i := range agents {
		runningNames[agents[i].Name] = true
		if agents[i].GrovePath != "" {
			agentDir := filepath.Join(agents[i].GrovePath, "agents", agents[i].Name)
			scionJSON := filepath.Join(agentDir, "scion-agent.json")
			agentHome := config.GetAgentHomePath(agents[i].GrovePath, agents[i].Name)
			agentInfoJSON := filepath.Join(agentHome, "agent-info.json")
			terminalPhase := terminalRuntimePhase(agents[i])

			// Try agent-info.json first for latest status from container
			if data, err := os.ReadFile(agentInfoJSON); err == nil {
				var info api.AgentInfo
				if err := json.Unmarshal(data, &info); err == nil {
					if terminalPhase == "" {
						agents[i].Phase = info.Phase
						agents[i].Activity = info.Activity
					}
					if agents[i].Runtime == "" {
						agents[i].Runtime = info.Runtime
					}
					agents[i].Profile = info.Profile
					if agents[i].Template == "" {
						agents[i].Template = info.Template
					}
					if agents[i].HarnessConfig == "" {
						agents[i].HarnessConfig = info.HarnessConfig
					}
					if info.Detail != nil {
						agents[i].Detail = info.Detail
					}
				}
			}

			if terminalPhase != "" {
				agents[i].Phase = terminalPhase
				agents[i].Activity = ""
				_ = persistAgentInfoState(agentInfoJSON, terminalPhase, "")
			}

			// Use agent-info.json mtime as LastSeen for local agents
			if fi, err := os.Stat(agentInfoJSON); err == nil {
				agents[i].LastSeen = fi.ModTime()
			}

			// Then load scion-agent.json for legacy support or missing fields
			if data, err := os.ReadFile(scionJSON); err == nil {
				var cfg api.ScionConfig
				if err := json.Unmarshal(data, &cfg); err == nil && cfg.Info != nil {
					if agents[i].Phase == "" {
						agents[i].Phase = cfg.Info.Phase
					}
					if agents[i].Runtime == "" {
						agents[i].Runtime = cfg.Info.Runtime
					}
					if agents[i].Profile == "" {
						agents[i].Profile = cfg.Info.Profile
					}
					if agents[i].Template == "" {
						agents[i].Template = cfg.Info.Template
					}
					if agents[i].HarnessConfig == "" {
						agents[i].HarnessConfig = cfg.Info.HarnessConfig
					}
				}
			}
		}

		// Reconcile phase with actual container status.
		// Container runtime status is authoritative for running/stopped.
		containerStatusLower := strings.ToLower(agents[i].ContainerStatus)
		isContainerRunning := strings.HasPrefix(containerStatusLower, "up") || containerStatusLower == "running"
		isContainerStopped := strings.HasPrefix(containerStatusLower, "exited") || containerStatusLower == "stopped"

		if isContainerRunning && agents[i].Phase == string(state.PhaseStopped) {
			agents[i].Phase = string(state.PhaseRunning)
		}
		if isContainerStopped {
			p := state.Phase(agents[i].Phase)
			switch p {
			case state.PhaseRunning:
				agents[i].Phase = string(state.PhaseStopped)
				agents[i].Activity = ""
			case state.PhaseCloning, state.PhaseStarting, state.PhaseProvisioning:
				// Container exited during a pre-running phase (e.g. clone failure
				// where agent-info.json wasn't updated). Mark as error so the
				// UI doesn't show a stale "cloning" or "starting" phase.
				agents[i].Phase = string(state.PhaseError)
				agents[i].Activity = ""
			case state.PhaseError, state.PhaseStopped:
				// Already terminal — preserve as-is
			}
		}
	}

	for _, gp := range grovesToScan {
		agentsDir := filepath.Join(gp, "agents")
		entries, err := os.ReadDir(agentsDir)
		if err != nil {
			continue
		}
		groveName := config.GetGroveName(gp)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if runningNames[e.Name()] {
				continue
			}

			// Check scion-agent.json and home/agent-info.json
			agentDir := filepath.Join(agentsDir, e.Name())
			agentScionJSON := filepath.Join(agentDir, "scion-agent.json")
			agentHome := config.GetAgentHomePath(gp, e.Name())
			agentInfoJSON := filepath.Join(agentHome, "agent-info.json")

			var info *api.AgentInfo

			// Try agent-info.json first
			if data, err := os.ReadFile(agentInfoJSON); err == nil {
				var ai api.AgentInfo
				if err := json.Unmarshal(data, &ai); err == nil {
					info = &ai
				}
			}

			// Fallback to scion-agent.json if info is missing (legacy)
			if info == nil {
				if data, err := os.ReadFile(agentScionJSON); err == nil {
					var cfg api.ScionConfig
					if err := json.Unmarshal(data, &cfg); err == nil {
						info = cfg.Info
					}
				}
			}

			// If we still have no info, check if scion-agent.json exists at all to confirm it's an agent
			// but we can't report much.
			if info == nil {
				if _, err := os.Stat(agentScionJSON); err == nil {
					// It's an agent directory but we can't read info.
					// Maybe report minimal info?
					info = &api.AgentInfo{
						Name:  e.Name(),
						Grove: groveName,
						Phase: "unknown",
					}
				} else {
					continue
				}
			}

			agentEntry := api.AgentInfo{
				Name:            e.Name(),
				Template:        info.Template,
				HarnessConfig:   info.HarnessConfig,
				Grove:           groveName,
				GrovePath:       gp,
				ContainerStatus: "created",
				Image:           info.Image,
				Phase:           info.Phase,
				Activity:        info.Activity,
				Runtime:         info.Runtime,
				Profile:         info.Profile,
			}

			// Use agent-info.json mtime as LastSeen for local agents
			if fi, err := os.Stat(agentInfoJSON); err == nil {
				agentEntry.LastSeen = fi.ModTime()
			}

			// Warn about stale soft-deleted agents
			if !info.DeletedAt.IsZero() {
				agentEntry.Warnings = append(agentEntry.Warnings,
					fmt.Sprintf("soft-deleted at %s", info.DeletedAt.Format("2006-01-02 15:04")))
			}

			agents = append(agents, agentEntry)
		}
	}

	return agents, nil
}

func terminalRuntimePhase(agent api.AgentInfo) string {
	switch state.Phase(agent.Phase) {
	case state.PhaseStopped, state.PhaseError:
		return agent.Phase
	case state.PhaseCreated, state.PhaseProvisioning, state.PhaseCloning,
		state.PhaseStarting, state.PhaseRunning, state.PhaseStopping:
		return ""
	}
	if agent.Phase != scionruntime.LegacyAgentPhaseEnded {
		return ""
	}
	containerStatus := strings.ToLower(agent.ContainerStatus)
	if strings.Contains(containerStatus, legacyFailedContainerStatusPrefix) {
		return string(state.PhaseError)
	}
	return string(state.PhaseStopped)
}

func persistAgentInfoState(path, phase, activity string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return err
	}

	if info.Phase == phase && info.Activity == activity {
		return nil
	}

	info.Phase = phase
	info.Activity = activity

	updated, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, updated, 0644)
}
