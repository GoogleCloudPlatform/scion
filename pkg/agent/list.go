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
	"os"
	"path/filepath"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
)

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
			agentInfoJSON := filepath.Join(agentDir, "home", "agent-info.json")

			// Try agent-info.json first for latest status from container
			if data, err := os.ReadFile(agentInfoJSON); err == nil {
				var info api.AgentInfo
				if err := json.Unmarshal(data, &info); err == nil {
					agents[i].Status = info.Status
					agents[i].SessionStatus = info.SessionStatus
					if info.Runtime != "" {
						agents[i].Runtime = info.Runtime
					}
					agents[i].Profile = info.Profile
				}
			}

			// Then load scion-agent.json for legacy support or missing fields
			if data, err := os.ReadFile(scionJSON); err == nil {
				var cfg api.ScionConfig
				if err := json.Unmarshal(data, &cfg); err == nil && cfg.Info != nil {
					if agents[i].Status == "" {
						agents[i].Status = cfg.Info.Status
					}
					if agents[i].SessionStatus == "" {
						agents[i].SessionStatus = cfg.Info.SessionStatus
					}
					if agents[i].Runtime == "" {
						agents[i].Runtime = cfg.Info.Runtime
					}
					if agents[i].Profile == "" {
						agents[i].Profile = cfg.Info.Profile
					}
				}
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
			agentInfoJSON := filepath.Join(agentDir, "home", "agent-info.json")

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
						Name: e.Name(),
						Grove: groveName,
						Status: "unknown",
					}
				} else {
					continue
				}
			}

			agents = append(agents, api.AgentInfo{
				Name:            e.Name(),
				Template:        info.Template,
				Grove:           groveName,
				GrovePath:       gp,
				ContainerStatus: "created",
				Image:           info.Image,
				Status:          info.Status,
				SessionStatus:   info.SessionStatus,
				Runtime:         info.Runtime,
				Profile:         info.Profile,
			})
		}
	}

	return agents, nil
}
