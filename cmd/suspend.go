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
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

var (
	suspendAll bool
)

var suspendCmd = &cobra.Command{
	Use:   "suspend [agent]",
	Short: "Suspend an agent for later resume",
	Long: `Suspend a running agent, preserving its state for later resume.

Unlike 'stop', which signals termination, 'suspend' signals that the agent
will be resumed later with its harness session intact. When you 'start' a
suspended agent, the harness picks up where it left off (implicit resume).
When you 'start' a stopped agent, it starts a fresh session.

Suspend requires the agent's harness to support session resume. Harnesses
that do not (e.g. generic) will return an error — use 'stop' instead.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if suspendAll {
			if len(args) > 0 {
				return fmt.Errorf("no arguments allowed when using --all")
			}
			return nil
		}
		if len(args) != 1 {
			return fmt.Errorf("requires exactly 1 argument (agent name)")
		}
		return nil
	},
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if suspendAll {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return getAgentNames(cmd, args, toComplete)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if suspendAll {
			hubCtx, err := CheckHubAvailability(projectPath)
			if err != nil {
				return err
			}
			if hubCtx != nil {
				return suspendAllAgentsViaHub(hubCtx)
			}
			return suspendAllAgents()
		}

		agentName := api.Slugify(args[0])

		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, false)
		if err != nil {
			return err
		}

		if hubCtx != nil {
			return suspendAgentViaHub(hubCtx, agentName)
		}

		// Local mode — verify the harness supports resume before suspending.
		if err := checkHarnessResumeSupport(agentName, projectPath); err != nil {
			return err
		}

		rt := runtime.GetRuntime(projectPath, profile)
		mgr := agent.NewManager(rt)

		statusf("Suspending agent '%s'...\n", agentName)
		if err := mgr.Stop(context.Background(), agentName, grovePath); err != nil {
			return err
		}

		_ = agent.UpdateAgentConfig(agentName, projectPath, "suspended", "", "")

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "suspend",
				Agent:   agentName,
				Message: fmt.Sprintf("Agent '%s' suspended.", agentName),
			})
		}
		statusf("Agent '%s' suspended.\n", agentName)
		return nil
	},
}

// checkHarnessResumeSupport resolves the agent's harness and returns an error
// if the harness does not support session resume.
func checkHarnessResumeSupport(agentName, projectPath string) error {
	harnessConfigName := agent.GetSavedHarnessConfig(agentName, projectPath)
	if harnessConfigName == "" {
		return nil
	}
	h := harness.New(harnessConfigName)
	caps := h.AdvancedCapabilities()
	if caps.Resume.Support == api.SupportNo {
		reason := caps.Resume.Reason
		if reason == "" {
			reason = "harness does not support session resume"
		}
		return fmt.Errorf("cannot suspend agent '%s': %s. Use 'stop' instead", agentName, reason)
	}
	return nil
}

func suspendAllAgents() error {
	rt := runtime.GetRuntime(projectPath, profile)
	mgr := agent.NewManager(rt)

	filters := map[string]string{
		"scion.agent": "true",
	}

	projectDir, _ := config.GetResolvedProjectDir(projectPath)
	if projectDir != "" {
		filters["scion.project_path"] = projectDir
		filters["scion.project"] = config.GetProjectName(projectDir)
	}

	agents, err := mgr.List(context.Background(), filters)
	if err != nil {
		return err
	}

	type runningAgent struct {
		Name string
	}
	var running []runningAgent
	for _, a := range agents {
		if a.ContainerID == "" {
			continue
		}
		agentName := a.Labels["scion.name"]
		if agentName == "" {
			continue
		}
		status := strings.ToLower(a.ContainerStatus)
		if strings.HasPrefix(status, "up") ||
			strings.HasPrefix(status, "running") {
			running = append(running, runningAgent{Name: agentName})
		}
	}

	if len(running) == 0 {
		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"status":  "success",
				"command": "suspend",
				"message": "No running agents found.",
				"results": []interface{}{},
			})
		}
		statusln("No running agents found.")
		return nil
	}

	type agentResult struct {
		Name   string
		Status string
		Error  string
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results []agentResult
	)

	for _, ra := range running {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()

			res := agentResult{Name: name, Status: "success"}

			if err := checkHarnessResumeSupport(name, projectPath); err != nil {
				res.Status = "skipped"
				res.Error = err.Error()
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}

			agentRt := runtime.GetRuntime(projectPath, profile)
			agentMgr := agent.NewManager(agentRt)

			if err := agentMgr.Stop(context.Background(), name, grovePath); err != nil {
				res.Status = "error"
				res.Error = err.Error()
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}

			_ = agent.UpdateAgentConfig(name, projectPath, "suspended", "", "")

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(ra.Name)
	}

	wg.Wait()

	if isJSONOutput() {
		jsonResults := make([]map[string]interface{}, len(results))
		hasErrors := false
		for i, r := range results {
			entry := map[string]interface{}{
				"agent":  r.Name,
				"status": r.Status,
			}
			if r.Error != "" {
				entry["error"] = r.Error
				hasErrors = true
			}
			jsonResults[i] = entry
		}
		overallStatus := "success"
		if hasErrors {
			overallStatus = "partial"
		}
		return outputJSON(map[string]interface{}{
			"status":  overallStatus,
			"command": "suspend",
			"results": jsonResults,
		})
	}

	var errs []string
	for _, r := range results {
		switch r.Status {
		case "skipped":
			statusf("Agent '%s': skipped: %s\n", r.Name, r.Error)
		case "error":
			statusf("Agent '%s': error: %s\n", r.Name, r.Error)
			errs = append(errs, fmt.Sprintf("%s: %s", r.Name, r.Error))
		default:
			statusf("Agent '%s' suspended.\n", r.Name)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to suspend some agents:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func suspendAllAgentsViaHub(hubCtx *HubContext) error {
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agentSvc := hubCtx.Client.ProjectAgents(projectID)
	resp, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{})
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list agents via Hub: %w", err))
	}

	var running []hubclient.Agent
	for _, a := range resp.Agents {
		status := strings.ToLower(a.ContainerStatus)
		if strings.HasPrefix(status, "up") ||
			strings.HasPrefix(status, "running") {
			running = append(running, a)
		}
	}

	if len(running) == 0 {
		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"status":  "success",
				"command": "suspend",
				"message": "No running agents found.",
				"results": []interface{}{},
			})
		}
		statusln("No running agents found.")
		return nil
	}

	type agentResult struct {
		Name   string
		Status string
		Error  string
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results []agentResult
	)

	for _, a := range running {
		wg.Add(1)
		go func(ag hubclient.Agent) {
			defer wg.Done()

			res := agentResult{Name: ag.Name, Status: "success"}

			agentCtx, agentCancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer agentCancel()

			if err := agentSvc.Suspend(agentCtx, ag.Name); err != nil {
				res.Status = "error"
				res.Error = wrapHubError(fmt.Errorf("failed to suspend: %w", err)).Error()
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(a)
	}

	wg.Wait()

	if isJSONOutput() {
		jsonResults := make([]map[string]interface{}, len(results))
		hasErrors := false
		for i, r := range results {
			entry := map[string]interface{}{
				"agent":  r.Name,
				"status": r.Status,
			}
			if r.Error != "" {
				entry["error"] = r.Error
				hasErrors = true
			}
			jsonResults[i] = entry
		}
		overallStatus := "success"
		if hasErrors {
			overallStatus = "partial"
		}
		return outputJSON(map[string]interface{}{
			"status":  overallStatus,
			"command": "suspend",
			"results": jsonResults,
		})
	}

	var errs []string
	for _, r := range results {
		if r.Error != "" {
			statusf("Agent '%s': error: %s\n", r.Name, r.Error)
			errs = append(errs, fmt.Sprintf("%s: %s", r.Name, r.Error))
		} else {
			statusf("Agent '%s' suspended via Hub.\n", r.Name)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to suspend some agents via Hub:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func suspendAgentViaHub(hubCtx *HubContext, agentName string) error {
	PrintUsingHub(hubCtx.Endpoint)
	statusf("Suspending agent '%s'...\n", agentName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	if err := agentSvc.Suspend(ctx, agentName); err != nil {
		return wrapHubError(fmt.Errorf("failed to suspend agent via Hub: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "suspend",
			Agent:   agentName,
			Message: fmt.Sprintf("Agent '%s' suspended via Hub.", agentName),
			Details: map[string]interface{}{"hub": true},
		})
	}
	statusf("Agent '%s' suspended via Hub.\n", agentName)
	return nil
}

func init() {
	suspendCmd.Flags().BoolVarP(&suspendAll, "all", "a", false, "Suspend all running agents in the current grove")
	rootCmd.AddCommand(suspendCmd)
}
