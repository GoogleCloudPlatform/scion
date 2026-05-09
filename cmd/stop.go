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
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

var (
	stopRm  bool
	stopAll bool
)

// stopCmd represents the stop command
var stopCmd = &cobra.Command{
	Use:   "stop [agent]",
	Short: "Stop an agent",
	Args: func(cmd *cobra.Command, args []string) error {
		if stopAll {
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
		if stopAll {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return getAgentNames(cmd, args, toComplete)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if stopAll {
			hubCtx, err := CheckHubAvailability(projectPath)
			if err != nil {
				return err
			}
			if hubCtx != nil {
				return stopAllAgentsViaHub(hubCtx)
			}
			return stopAllAgents()
		}

		agentName := api.Slugify(args[0])

		// Check if Hub should be used, excluding the target agent from sync requirements.
		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, false)
		if err != nil {
			return err
		}

		if hubCtx != nil {
			return stopAgentViaHub(hubCtx, agentName)
		}

		// Local mode
		rt := runtime.GetRuntime(projectPath, profile)
		mgr := agent.NewManager(rt)

		statusf("Stopping agent '%s'...\n", agentName)
		if err := mgr.Stop(context.Background(), agentName); err != nil {
			return err
		}

		_ = agent.UpdateAgentConfig(agentName, projectPath, "stopped", "", "")

		if stopRm {
			if _, err := mgr.Delete(context.Background(), agentName, true, projectPath, false); err != nil {
				return err
			}
			if isJSONOutput() {
				return outputJSON(ActionResult{
					Status:  "success",
					Command: "stop",
					Agent:   agentName,
					Message: fmt.Sprintf("Agent '%s' stopped and removed.", agentName),
					Details: map[string]interface{}{"removed": true},
				})
			}
			statusf("Agent '%s' stopped and removed.\n", agentName)
		} else {
			if isJSONOutput() {
				return outputJSON(ActionResult{
					Status:  "success",
					Command: "stop",
					Agent:   agentName,
					Message: fmt.Sprintf("Agent '%s' stopped.", agentName),
				})
			}
			statusf("Agent '%s' stopped.\n", agentName)
		}

		return nil
	},
}

// stopAllAgents stops all running agents in the current grove using the local runtime.
func stopAllAgents() error {
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

	// Filter for running agents
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
				"command": "stop",
				"message": "No running agents found.",
				"results": []interface{}{},
			})
		}
		statusln("No running agents found.")
		return nil
	}

	if stopRm {
		fmt.Printf("\nThe following %d agent(s) will be stopped and removed:\n", len(running))
		for _, ra := range running {
			fmt.Printf("  - %s\n", ra.Name)
		}
		fmt.Println()
		if !hubsync.ConfirmAction("Continue?", false, autoConfirm) {
			return nil
		}
	}

	type agentResult struct {
		Name    string
		Status  string
		Error   string
		Removed bool
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

			agentRt := runtime.GetRuntime(projectPath, profile)
			agentMgr := agent.NewManager(agentRt)

			if err := agentMgr.Stop(context.Background(), name); err != nil {
				res.Status = "error"
				res.Error = err.Error()
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}

			_ = agent.UpdateAgentConfig(name, projectPath, "stopped", "", "")

			if stopRm {
				if _, err := agentMgr.Delete(context.Background(), name, true, projectPath, false); err != nil {
					res.Status = "error"
					res.Error = fmt.Sprintf("stopped but failed to remove: %v", err)
					mu.Lock()
					results = append(results, res)
					mu.Unlock()
					return
				}
				res.Removed = true
			}

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
			if r.Removed {
				entry["removed"] = true
			}
			jsonResults[i] = entry
		}
		overallStatus := "success"
		if hasErrors {
			overallStatus = "partial"
		}
		return outputJSON(map[string]interface{}{
			"status":  overallStatus,
			"command": "stop",
			"results": jsonResults,
		})
	}

	var errs []string
	for _, r := range results {
		if r.Error != "" {
			statusf("Agent '%s': error: %s\n", r.Name, r.Error)
			errs = append(errs, fmt.Sprintf("%s: %s", r.Name, r.Error))
		} else if r.Removed {
			statusf("Agent '%s' stopped and removed.\n", r.Name)
		} else {
			statusf("Agent '%s' stopped.\n", r.Name)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to stop some agents:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// stopAllAgentsViaHub stops all running agents in the current grove via the Hub.
func stopAllAgentsViaHub(hubCtx *HubContext) error {
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

	// Filter for running agents
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
				"command": "stop",
				"message": "No running agents found.",
				"results": []interface{}{},
			})
		}
		statusln("No running agents found.")
		return nil
	}

	if stopRm {
		fmt.Printf("\nThe following %d agent(s) will be stopped and removed:\n", len(running))
		for _, a := range running {
			fmt.Printf("  - %s\n", a.Name)
		}
		fmt.Println()
		if !hubsync.ConfirmAction("Continue?", false, autoConfirm) {
			return nil
		}
	}

	type agentResult struct {
		Name    string
		Status  string
		Error   string
		Removed bool
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

			if err := agentSvc.Stop(agentCtx, ag.Name); err != nil {
				res.Status = "error"
				res.Error = wrapHubError(fmt.Errorf("failed to stop: %w", err)).Error()
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}

			if stopRm {
				opts := &hubclient.DeleteAgentOptions{
					DeleteFiles:  true,
					RemoveBranch: false,
				}
				if err := agentSvc.Delete(agentCtx, ag.Name, opts); err != nil {
					res.Status = "error"
					res.Error = wrapHubError(fmt.Errorf("stopped but failed to remove: %w", err)).Error()
					mu.Lock()
					results = append(results, res)
					mu.Unlock()
					return
				}
				res.Removed = true
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(a)
	}

	wg.Wait()

	if stopRm && hubCtx.ProjectPath != "" {
		removedAny := false
		for _, r := range results {
			if r.Removed && r.Error == "" {
				removedAny = true
				break
			}
		}
		if removedAny {
			// Keep sync watermark current after hub-side delete operations.
			hubsync.UpdateLastSyncedAt(hubCtx.ProjectPath, time.Time{}, hubCtx.IsGlobal)
			for _, r := range results {
				if r.Removed && r.Error == "" {
					hubsync.RemoveSyncedAgent(hubCtx.ProjectPath, r.Name)
				}
			}
		}
	}

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
			if r.Removed {
				entry["removed"] = true
			}
			jsonResults[i] = entry
		}
		overallStatus := "success"
		if hasErrors {
			overallStatus = "partial"
		}
		return outputJSON(map[string]interface{}{
			"status":  overallStatus,
			"command": "stop",
			"results": jsonResults,
		})
	}

	var errs []string
	for _, r := range results {
		if r.Error != "" {
			statusf("Agent '%s': error: %s\n", r.Name, r.Error)
			errs = append(errs, fmt.Sprintf("%s: %s", r.Name, r.Error))
		} else if r.Removed {
			statusf("Agent '%s' stopped and removed via Hub.\n", r.Name)
		} else {
			statusf("Agent '%s' stopped via Hub.\n", r.Name)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to stop some agents via Hub:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func stopAgentViaHub(hubCtx *HubContext, agentName string) error {
	PrintUsingHub(hubCtx.Endpoint)
	statusf("Stopping agent '%s'...\n", agentName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Get the grove ID for this project
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	// Use grove-scoped client to allow lookup by name/slug
	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	if err := agentSvc.Stop(ctx, agentName); err != nil {
		return wrapHubError(fmt.Errorf("failed to stop agent via Hub: %w", err))
	}

	if stopRm {
		opts := &hubclient.DeleteAgentOptions{
			DeleteFiles:  true,
			RemoveBranch: false,
		}
		if err := agentSvc.Delete(ctx, agentName, opts); err != nil {
			return wrapHubError(fmt.Errorf("agent stopped but failed to delete via Hub: %w", err))
		}
		if hubCtx.ProjectPath != "" {
			// Keep sync watermark current after hub-side delete operations.
			hubsync.UpdateLastSyncedAt(hubCtx.ProjectPath, time.Time{}, hubCtx.IsGlobal)
			hubsync.RemoveSyncedAgent(hubCtx.ProjectPath, agentName)
		}
		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "stop",
				Agent:   agentName,
				Message: fmt.Sprintf("Agent '%s' stopped and removed via Hub.", agentName),
				Details: map[string]interface{}{"removed": true, "hub": true},
			})
		}
		statusf("Agent '%s' stopped and removed via Hub.\n", agentName)
	} else {
		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "stop",
				Agent:   agentName,
				Message: fmt.Sprintf("Agent '%s' stopped via Hub.", agentName),
				Details: map[string]interface{}{"hub": true},
			})
		}
		statusf("Agent '%s' stopped via Hub.\n", agentName)
	}

	return nil
}

func init() {
	stopCmd.Flags().BoolVar(&stopRm, "rm", false, "Remove the agent after stopping")
	stopCmd.Flags().BoolVarP(&stopAll, "all", "a", false, "Stop all running agents in the current grove")
	rootCmd.AddCommand(stopCmd)
}
