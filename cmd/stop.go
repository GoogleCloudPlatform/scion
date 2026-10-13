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
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

var (
	stopRm   bool
	stopAll  bool
	stopSelf bool
)

// stopCmd represents the stop command
var stopCmd = &cobra.Command{
	Use:   "stop [agent]",
	Short: "Stop an agent",
	Long: `Stop an agent, or every running agent of the project with --all.

With --rm the agent is also removed after it stops, with its local files
and worktree (the git branch is kept).

Inside an agent, --rm does not remove the current agent (the one the
command runs in) unless --self is given: naming the current agent with
--rm is refused and nothing is stopped, and --all --rm skips the current
agent and stops and removes the others. Outside an agent container --self
has no effect.`,
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
		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, true)
		if err != nil {
			return err
		}

		// Checked before the stop, so a refusal leaves the agent running.
		if stopRm {
			if err := refuseCurrentAgentTarget([]string{agentName}, hubProjectID(hubCtx), stopSelf); err != nil {
				return err
			}
		}

		if hubCtx != nil {
			return stopAgentViaHub(hubCtx, agentName)
		}

		// Local mode
		rt := runtime.GetRuntime(projectPath, profile)
		mgr := agent.NewManager(rt)

		statusf("Stopping agent '%s'...\n", agentName)
		if err := mgr.Stop(context.Background(), agentName, projectPath, ""); err != nil {
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

// stopAllAgents stops all running agents in the current project using the local runtime.
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

		if a.Phase == string(state.PhaseRunning) {
			running = append(running, runningAgent{Name: agentName})
		}
	}

	// Without a Hub the project ID is unknown, so the current agent is
	// matched by name (fail closed).
	running, skipped := skipCurrentAgentForStopAllRm(running, "", func(ra runningAgent) []string {
		return []string{ra.Name}
	})

	if len(running) == 0 {
		return reportNoAgentsToStop(skipped)
	}

	if stopRm {
		names := make([]string, len(running))
		for i, ra := range running {
			names[i] = ra.Name
		}
		if !confirmStopAllRm(names) {
			// Declined: nothing is stopped; in JSON mode stdout stays empty.
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
		results []agentResult
	)

	// ptone/scion#3602: at most maxFanOutConcurrency agents in flight at once.
	boundedFanOut(context.Background(), len(running), maxFanOutConcurrency, lifecycleFanOutQueuedHook,
		func(i int) {
			name := running[i].Name

			res := agentResult{Name: name, Status: "success"}

			agentRt := runtime.GetRuntime(projectPath, profile)
			agentMgr := agent.NewManager(agentRt)

			if err := agentMgr.Stop(context.Background(), name, projectPath, ""); err != nil {
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
			if r.Removed {
				entry["removed"] = true
			}
			jsonResults[i] = entry
		}
		overallStatus := "success"
		if hasErrors {
			overallStatus = "partial"
		}
		return outputJSONResult(withSkippedAgents(map[string]interface{}{
			"status":  overallStatus,
			"command": "stop",
			"results": jsonResults,
		}, skipped), hasErrors, "failed to stop some agents")
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

// confirmStopAllRm lists the agents that stop --all --rm will stop and
// remove, and asks the user to continue. In JSON mode the list and the
// prompt go to stderr, and --yes skips the prompt silently, so stdout
// carries only the JSON document.
func confirmStopAllRm(names []string) bool {
	if !isJSONOutput() {
		fmt.Printf("\nThe following %d agent(s) will be stopped and removed:\n", len(names))
		for _, n := range names {
			fmt.Printf("  - %s\n", n)
		}
		fmt.Println()
		return hubsync.ConfirmAction("Continue?", false, autoConfirm)
	}
	if autoConfirm {
		return true
	}
	fmt.Fprintf(os.Stderr, "\nThe following %d agent(s) will be stopped and removed:\n", len(names))
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  - %s\n", n)
	}
	fmt.Fprint(os.Stderr, "\nContinue? (y/N): ")
	input, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		// Like hubsync.ConfirmAction: any read error, including EOF
		// without a trailing newline, means the default (No).
		return false
	}
	input = strings.ToLower(strings.TrimSpace(input))
	return input == "y" || input == "yes"
}

// stopAllAgentsViaHub stops all running agents in the current project via the Hub.
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
		if a.Phase == string(state.PhaseRunning) {
			running = append(running, a)
		}
	}

	running, skipped := skipCurrentAgentForStopAllRm(running, projectID, func(a hubclient.Agent) []string {
		return []string{a.Name, a.Slug, a.ID}
	})

	if len(running) == 0 {
		return reportNoAgentsToStop(skipped)
	}

	if stopRm {
		names := make([]string, len(running))
		for i, a := range running {
			names[i] = a.Name
		}
		if !confirmStopAllRm(names) {
			// Declined: nothing is stopped; in JSON mode stdout stays empty.
			return nil
		}
	}

	type agentResult struct {
		Name    string
		Status  string
		Error   string
		Removed bool
		// Pending is set when removal was accepted (202) but its completion
		// could not be observed.
		Pending  string
		Queued   bool
		Warnings []string
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

			stopResp, err := agentSvc.Stop(agentCtx, ag.Name)
			if err != nil {
				res.Status = "error"
				res.Error = wrapHubError(fmt.Errorf("failed to stop: %w", err)).Error()
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}
			if stopResp != nil {
				res.Queued = stopResp.Queued
				res.Warnings = stopResp.Warnings
			}

			// A queued stop has not run yet, so the agent is not removed.
			if stopRm && !res.Queued {
				opts := &hubclient.DeleteAgentOptions{
					DeleteFiles:  true,
					RemoveBranch: false,
				}
				outcome, err := deleteViaHubAndWait(agentCtx, agentSvc, ag.Name, opts, func() {
					statusf("Agent '%s': removal in progress...\n", ag.Name)
				})
				if err != nil {
					err = wrapHubError(err)
				} else {
					err = hubDeleteFailure(ag.Name, outcome, "agent kept")
				}
				if err != nil {
					res.Status = "error"
					res.Error = fmt.Errorf("stopped but failed to remove: %w", err).Error()
					mu.Lock()
					results = append(results, res)
					mu.Unlock()
					return
				}
				if outcome.Confirmed() {
					res.Removed = true
				} else {
					res.Pending = hubRemovalPendingMessage(outcome)
				}
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}, func(int) {})

	if stopRm {
		// Confirmed removals get the same local cleanup as scion delete
		// (ptone/scion#2896): agent files, worktree and sync state. It runs
		// here, one agent at a time, because git worktree operations must
		// not overlap. The branch is kept, as with a local stop --rm.
		for i := range results {
			r := &results[i]
			if r.Removed && r.Error == "" {
				if _, err := cleanupAfterHubDelete(hubCtx, r.Name, false); err != nil {
					r.Warnings = append(r.Warnings, stopRmCleanupWarning(r.Name, err))
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
			if r.Pending != "" {
				entry["removalPending"] = true
				entry["message"] = r.Pending
			}
			if r.Queued {
				entry["queued"] = true
			}
			if len(r.Warnings) > 0 {
				entry["warnings"] = r.Warnings
			}
			jsonResults[i] = entry
		}
		overallStatus := "success"
		if hasErrors {
			overallStatus = "partial"
		}
		return outputJSONResult(withSkippedAgents(map[string]interface{}{
			"status":  overallStatus,
			"command": "stop",
			"results": jsonResults,
		}, skipped), hasErrors, "failed to stop some agents via Hub")
	}

	var errs []string
	for _, r := range results {
		for _, w := range r.Warnings {
			statusf("Agent '%s': warning: %s\n", r.Name, w)
		}
		if r.Error != "" {
			statusf("Agent '%s': error: %s\n", r.Name, r.Error)
			errs = append(errs, fmt.Sprintf("%s: %s", r.Name, r.Error))
		} else if r.Queued && stopRm {
			statusf("Agent '%s': %s\n", r.Name, stopQueuedNotRemovedMessage)
		} else if r.Queued {
			statusf("Agent '%s': stop queued via Hub.\n", r.Name)
		} else if r.Removed {
			statusf("Agent '%s' stopped and removed via Hub.\n", r.Name)
		} else if r.Pending != "" {
			statusf("Agent '%s' stopped via Hub; %s.\n", r.Name, r.Pending)
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

	// Get the project ID for this project
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	// Use project-scoped client to allow lookup by name/slug
	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	stopResp, err := agentSvc.Stop(ctx, agentName)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to stop agent via Hub: %w", err))
	}
	if stopResp != nil && stopResp.Queued {
		return reportQueuedStop(agentName, stopResp.Warnings)
	}
	printLifecycleWarnings(stopResp)

	if stopRm {
		opts := &hubclient.DeleteAgentOptions{
			DeleteFiles:  true,
			RemoveBranch: false,
		}
		outcome, err := deleteViaHubAndWait(ctx, agentSvc, agentName, opts, func() {
			statusf("Agent '%s' stopped; removal in progress...\n", agentName)
		})
		if err != nil {
			return wrapHubError(fmt.Errorf("agent stopped but failed to delete via Hub: %w", err))
		}
		if err := hubDeleteFailure(agentName, outcome, "agent kept"); err != nil {
			return fmt.Errorf("agent stopped but failed to delete via Hub: %w", err)
		}
		if !outcome.Confirmed() {
			// Accepted but not observed: not a failure; leave the sync state
			// alone until the removal is known to be done.
			msg := hubRemovalPendingMessage(outcome)
			if isJSONOutput() {
				return outputJSON(ActionResult{
					Status:  "success",
					Command: "stop",
					Agent:   agentName,
					Message: fmt.Sprintf("Agent '%s' stopped via Hub; %s.", agentName, msg),
					Details: map[string]interface{}{"removed": false, "removalPending": true, "hub": true},
				})
			}
			statusf("Agent '%s' stopped via Hub; %s.\n", agentName, msg)
			return nil
		}
		// Same local cleanup as scion delete (ptone/scion#2896); the branch
		// is kept, as with a local stop --rm.
		var warnings []string
		if _, err := cleanupAfterHubDelete(hubCtx, agentName, false); err != nil {
			warnings = append(warnings, stopRmCleanupWarning(agentName, err))
		}
		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:   "success",
				Command:  "stop",
				Agent:    agentName,
				Message:  fmt.Sprintf("Agent '%s' stopped and removed via Hub.", agentName),
				Warnings: warnings,
				Details:  map[string]interface{}{"removed": true, "hub": true},
			})
		}
		for _, w := range warnings {
			statusf("Warning: %s\n", w)
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

// skipCurrentAgentForStopAllRm returns items without the current agent when
// stop --all --rm runs without --self, and the skipped agents. It notes each
// skipped agent on stderr (in every output mode, so in JSON mode stdout
// still carries only the JSON document; the JSON result lists them under
// "skipped"). Otherwise it returns items unchanged and no skipped agents.
// targetProjectID and refs are as for withoutCurrentAgent. It runs before
// the confirm list, so that list does not name the current agent.
func skipCurrentAgentForStopAllRm[T any](items []T, targetProjectID string, refs func(T) []string) (kept []T, skipped []string) {
	if !stopRm || stopSelf {
		return items, nil
	}
	kept, skipped = withoutCurrentAgent(items, targetProjectID, refs)
	for _, name := range skipped {
		fmt.Fprintf(os.Stderr, "Skipping agent '%s': it is the current agent (this command is running inside it); add --self to stop and remove it too.\n", name)
	}
	return kept, skipped
}

// noOtherRunningAgentsMessage is reported by stop --all --rm when the only
// running agent was the current agent, which it skipped.
const noOtherRunningAgentsMessage = "No other running agents to stop (current agent skipped; add --self to include it)"

// reportNoAgentsToStop reports that stop --all found nothing to stop.
// skipped are the agents skipCurrentAgentForStopAllRm left out; when there
// are any, the message says so and the JSON result lists them.
func reportNoAgentsToStop(skipped []string) error {
	msg := "No running agents found."
	if len(skipped) > 0 {
		msg = noOtherRunningAgentsMessage
	}
	if isJSONOutput() {
		return outputJSON(withSkippedAgents(map[string]interface{}{
			"status":  "success",
			"command": "stop",
			"message": msg,
			"results": []interface{}{},
		}, skipped))
	}
	statusln(msg)
	return nil
}

// withSkippedAgents adds the agents stop --all --rm skipped to a JSON
// result document, as "skipped", when there are any.
func withSkippedAgents(doc map[string]interface{}, skipped []string) map[string]interface{} {
	if len(skipped) > 0 {
		doc["skipped"] = skipped
	}
	return doc
}

// stopRmCleanupWarning is the warning for a stop --rm whose Hub removal
// was confirmed but whose local cleanup failed. The command still succeeds.
// stop --rm keeps the git branch, so the retry command does too.
func stopRmCleanupWarning(agentName string, err error) string {
	return fmt.Sprintf("removed via Hub but local cleanup failed: %v; run '%s' to retry", err, cleanupOnlyCommand(agentName, true))
}

// stopQueuedNotRemovedMessage is printed when stop --rm finds the stop queued
// for an offline broker: the agent is not removed.
const stopQueuedNotRemovedMessage = "Broker offline: stop queued; the agent was not removed. Run scion delete when the broker is back."

// reportQueuedStop reports a stop the Hub queued for an offline broker. With
// --rm the agent is not removed, since its container has not stopped yet.
// Either way the command succeeds.
func reportQueuedStop(agentName string, warnings []string) error {
	msg := fmt.Sprintf("Agent '%s': stop queued via Hub.", agentName)
	if stopRm {
		msg = stopQueuedNotRemovedMessage
	}
	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:   "success",
			Command:  "stop",
			Agent:    agentName,
			Message:  msg,
			Warnings: warnings,
			Details:  map[string]interface{}{"hub": true, "queued": true, "removed": false},
		})
	}
	for _, w := range warnings {
		statusf("Warning: %s\n", w)
	}
	statusf("%s\n", msg)
	return nil
}

// printLifecycleWarnings prints the warnings a Hub lifecycle action returned.
func printLifecycleWarnings(resp *hubclient.LifecycleResponse) {
	if resp == nil {
		return
	}
	for _, w := range resp.Warnings {
		statusf("Warning: %s\n", w)
	}
}

func init() {
	stopCmd.Flags().BoolVar(&stopRm, "rm", false, "Remove the agent after stopping, including its local files and worktree (the git branch is kept)")
	stopCmd.Flags().BoolVarP(&stopAll, "all", "a", false, "Stop all running agents in the current project")
	stopCmd.Flags().BoolVar(&stopSelf, "self", false, "With --rm, allow removing the current agent when run inside an agent; without it, naming the current agent is refused and --all skips it")
	rootCmd.AddCommand(stopCmd)
}
