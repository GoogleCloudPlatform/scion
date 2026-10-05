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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// List returns the agents the runtime reports, plus "created" agents that
// exist on disk but have no container yet.
//
// The runtime layer applies filter to each container's labels. The on-disk
// scan applies the same filter through the same matcher
// (scionruntime.LabelsMatchFilter), evaluated against createdAgentLabels:
// the label set the agent's container would carry once started. Per key:
//
//   - scion.agent: always "true" for an on-disk agent.
//   - scion.name: the agent directory name (a slug).
//   - scion.project, scion.project_path: the scanned project's name and path.
//   - scion.project_id: the project's Hub-linked project ID, the value the
//     container label is populated from; empty for an unlinked project, so a
//     project ID filter never matches its created agents (nor its containers).
//   - scion.template, scion.harness_config, scion.harness_auth: from the
//     agent's recorded info.
//   - status: no agent carries a "status" label, so a status filter matches
//     no created agent, exactly as it matches no container.
//   - Any other key (for example agent_id, assigned only at start) is unknown
//     to a created agent and never matches.
//
// The scan runs only when filter carries scion.project_path, or when filter
// is empty or only {scion.agent: true} (the current and global projects).
func (m *AgentManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	agents, err := m.Runtime.List(ctx, filter)
	if err != nil {
		return nil, err
	}

	// Also find "created" agents that don't have a container yet. An explicit
	// project path identifies which local project directory to scan.
	var projectsToScan []string
	projectPath := filter["scion.project_path"]
	if projectPath != "" {
		projectsToScan = append(projectsToScan, projectPath)
	} else if len(filter) == 0 || (len(filter) == 1 && filter["scion.agent"] == "true") {
		// Default: scan current resolved project dir and global dir
		pd, _ := config.GetResolvedProjectDir("")
		if pd != "" {
			projectsToScan = append(projectsToScan, pd)
		}
		gd, _ := config.GetGlobalDir()
		if gd != "" && gd != pd {
			projectsToScan = append(projectsToScan, gd)
		}
	}

	runningNames := make(map[string]bool)
	runtimePhases := make(map[string]string, len(agents))
	for i := range agents {
		// Capture the runtime's Phase before agent-info.json overwrites it.
		// The runtime Phase (derived from container state) is authoritative
		// for running/stopped reconciliation below.
		runtimePhases[agents[i].Name] = agents[i].Phase
		runningNames[agents[i].Name] = true
		if agents[i].ProjectPath != "" {
			// ResolveAgentDir probes both worktree and shared-workspace
			// layouts (see .design/hub-shared-workspace-isolation.md) since
			// the runtime label set doesn't carry the workspace mode.
			agentDir := config.ResolveAgentDir(agents[i].ProjectPath, agents[i].Name)
			scionJSON := filepath.Join(agentDir, "scion-agent.json")
			agentHome := config.GetAgentHomePath(agents[i].ProjectPath, agents[i].Name)
			agentInfoJSON := filepath.Join(agentHome, "agent-info.json")
			terminalPhase := terminalRuntimePhase(agents[i])

			// Try agent-info.json first for latest status from container
			var parsedInfo *api.AgentInfo
			if data, err := os.ReadFile(agentInfoJSON); err == nil {
				var info api.AgentInfo
				if err := json.Unmarshal(data, &info); err == nil {
					parsedInfo = &info
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
				// Best-effort convergence: only persist if on-disk state
				// differs from the terminal phase we want to record.
				if parsedInfo != nil && (parsedInfo.Phase != terminalPhase || parsedInfo.Activity != "") {
					if err := persistAgentInfoState(agentInfoJSON, terminalPhase, ""); err != nil {
						slog.Debug("failed to persist terminal agent state", "path", agentInfoJSON, "err", err)
					}
				}
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
		// The runtime Phase (captured before agent-info.json merge) is
		// authoritative for whether the container is running or stopped.
		runtimePhase := runtimePhases[agents[i].Name]
		isContainerRunning := runtimePhase == string(state.PhaseRunning)
		isContainerStopped := runtimePhase == string(state.PhaseStopped) || runtimePhase == string(state.PhaseError)

		if isContainerRunning && agents[i].Phase == string(state.PhaseStopped) {
			agents[i].Phase = string(state.PhaseRunning)
		}
		if isContainerStopped {
			// A non-zero exit code means the agent crashed; map to error
			// (restartable) rather than a clean stop. A zero exit (or no
			// exit code) is a clean stop.
			exitCode := 0
			hasCode := agents[i].ExitCode != nil
			if hasCode {
				exitCode = *agents[i].ExitCode
			}
			crashed := (hasCode && exitCode != 0) || runtimePhase == string(state.PhaseError)
			p := state.Phase(agents[i].Phase)
			switch p {
			case state.PhaseRunning:
				if crashed {
					agents[i].Phase = string(state.PhaseError)
				} else {
					agents[i].Phase = string(state.PhaseStopped)
				}
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

	for _, gp := range projectsToScan {
		// Walk both the in-project agents dir (worktree-mode agents) and the
		// external split-storage agents dir (shared-workspace agents, whose
		// state lives outside the project tree per
		// .design/hub-shared-workspace-isolation.md).
		seenNames := make(map[string]bool)
		dirsToScan := []string{filepath.Join(gp, "agents")}
		if extDir, err := config.GetGitProjectExternalAgentsDir(gp); err == nil && extDir != "" {
			dirsToScan = append(dirsToScan, extDir)
		}
		projectName := config.GetProjectName(gp)
		projectIDLabel := lazyHubProjectID(gp)
		for _, agentsDir := range dirsToScan {
			entries, err := os.ReadDir(agentsDir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				if runningNames[e.Name()] || seenNames[e.Name()] {
					continue
				}
				seenNames[e.Name()] = true

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
							Name:    e.Name(),
							Project: projectName,
							Phase:   "unknown",
						}
					} else {
						continue
					}
				}

				if !scionruntime.LabelsMatchFilter(createdAgentLabels(e.Name(), projectName, gp, info, filter, projectIDLabel), filter) {
					continue
				}

				agentEntry := api.AgentInfo{
					Name:            e.Name(),
					Template:        info.Template,
					HarnessConfig:   info.HarnessConfig,
					Project:         projectName,
					ProjectPath:     gp,
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
						fmt.Sprintf("soft-deleted at %s", clitime.Format(info.DeletedAt, clitime.Minute)))
				}

				agents = append(agents, agentEntry)
			}
		}
	}

	return agents, nil
}

// createdAgentLabels returns the label set a created agent's container
// would carry once started (see the Labels/Annotations built for
// runtime.RunConfig in run.go), for matching List's filter against an
// on-disk agent. projectID is consulted only when filter asks for the
// project ID, because resolving it loads the project's settings.
func createdAgentLabels(name, projectName, projectPath string, info *api.AgentInfo, filter map[string]string, projectID func() string) map[string]string {
	labels := map[string]string{
		"scion.agent":          "true",
		"scion.name":           name,
		"scion.template":       info.Template,
		"scion.harness_config": info.HarnessConfig,
		"scion.harness_auth":   info.HarnessAuth,
	}
	for k, v := range projectkeys.ProjectNameLabels(projectName) {
		labels[k] = v
	}
	for k, v := range projectkeys.ProjectPathLabels(projectPath) {
		labels[k] = v
	}
	if _, ok := filter[projectkeys.LabelProjectID]; ok {
		if id := projectID(); id != "" {
			for k, v := range projectkeys.ProjectIDLabels(id) {
				labels[k] = v
			}
		}
	}
	return labels
}

// lazyHubProjectID returns a memoised lookup of projectDir's Hub-linked
// project ID (settings.Hub.ProjectID), the value run.go labels a locally
// started container with. It is not the local project-id marker that
// agent-info.json records as ProjectID.
func lazyHubProjectID(projectDir string) func() string {
	var (
		loaded bool
		id     string
	)
	return func() string {
		if !loaded {
			loaded = true
			if settings, _, err := config.LoadEffectiveSettings(projectDir); err == nil && settings != nil && settings.Hub != nil {
				id = settings.Hub.ProjectID
			}
		}
		return id
	}
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
	if agent.ExitCode != nil && *agent.ExitCode != 0 {
		return string(state.PhaseError)
	}
	return string(state.PhaseStopped)
}

func persistAgentInfoState(path, phase, activity string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
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

	return writeAgentInfoFile(path, updated, fi.Mode().Perm())
}
