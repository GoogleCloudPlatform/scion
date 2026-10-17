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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
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
			hubCtx, err := requireHub(projectPath)
			if err != nil {
				return err
			}
			return suspendAllAgentsViaHub(hubCtx)
		}

		agentName := api.Slugify(args[0])

		hubCtx, err := requireHub(projectPath)
		if err != nil {
			return err
		}

		return suspendAgentViaHub(hubCtx, agentName)
	},
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
		if a.Phase == string(state.PhaseRunning) {
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
		results []agentResult
	)

	// ptone/scion#3602: at most maxFanOutConcurrency agents in flight at once.
	boundedFanOut(context.Background(), len(running), maxFanOutConcurrency, lifecycleFanOutQueuedHook,
		func(i int) {
			ag := running[i]

			res := agentResult{Name: ag.Name, Status: "success"}

			agentCtx, agentCancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer agentCancel()

			if _, err := agentSvc.Suspend(agentCtx, ag.Name); err != nil {
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
		}, func(int) {})

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
		return outputJSONResult(map[string]interface{}{
			"status":  overallStatus,
			"command": "suspend",
			"results": jsonResults,
		}, hasErrors, "failed to suspend some agents via Hub")
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

	suspendResp, err := agentSvc.Suspend(ctx, agentName)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to suspend agent via Hub: %w", err))
	}
	printLifecycleWarnings(suspendResp)

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
	suspendCmd.Flags().BoolVarP(&suspendAll, "all", "a", false, "Suspend all running agents in the current project")
	rootCmd.AddCommand(suspendCmd)
}
