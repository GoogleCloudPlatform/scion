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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var (
	deleteStopped bool
	deleteForce   bool
)

// deleteCmd represents the delete command
var deleteCmd = &cobra.Command{
	Use:     "delete <agent> [agent...]",
	Aliases: []string{"rm"},
	Short:   "Delete one or more agents",
	Long: `Stop and remove one or more agent containers and their associated files and worktrees.

With a Hub, --force removes the agent from the Hub even when its runtime
broker cannot be reached or cannot resolve the agent. A forced delete is
permanent: it skips soft-delete retention, so the agent cannot be
restored. Runtime resources left on the broker (containers, worktrees)
may then need separate cleanup on that broker. --force does not purge an
agent that is already soft-deleted.

--force cannot be combined with --stopped; name the agents to
force-delete. In local mode (no Hub), --force has no effect: local delete
already removes the container.`,
	ValidArgsFunction: getMultiAgentNames,
	Args: func(cmd *cobra.Command, args []string) error {
		if deleteStopped {
			if deleteForce {
				return fmt.Errorf("--force cannot be combined with --stopped; name the agents to force-delete")
			}
			if len(args) > 0 {
				return fmt.Errorf("no arguments allowed when using --stopped")
			}
			return nil
		}
		if len(args) < 1 {
			return fmt.Errorf("requires at least 1 argument (agent name)")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Normalize agent names to slugs for consistent lookup
		for i, a := range args {
			args[i] = api.Slugify(a)
		}

		projectDir, _ := config.GetResolvedProjectDir(projectPath)
		if preserveBranch && !util.IsGitRepoDir(projectDir) {
			statusln("Warning: --preserve-branch used outside a git repository; this flag has no effect.")
		}

		// Check if Hub should be used, excluding all target agents from sync requirements.
		var excludedAgents []string
		if !deleteStopped {
			excludedAgents = args
		}
		hubCtx, err := CheckHubAvailabilityForAgents(projectPath, excludedAgents, true)
		if err != nil {
			return err
		}

		// --force only changes Hub behaviour. Local delete already removes the
		// container unconditionally, so warn and proceed rather than fail.
		if deleteForce && hubCtx == nil {
			statusln("Warning: --force has no effect without a Hub; deleting locally as usual.")
		}

		if deleteStopped {
			if hubCtx != nil {
				return deleteStoppedViaHub(hubCtx)
			}

			// Require an explicit project context — error if not in a project (unless --global)
			resolvedProjectPath, _, err := config.RequireProjectPath(projectPath)
			if err != nil {
				return err
			}

			rt := runtime.GetRuntime(projectPath, profile)
			mgr := agent.NewManager(rt)

			filters := map[string]string{
				"scion.agent":        "true",
				"scion.project_path": resolvedProjectPath,
				"scion.project":      config.GetProjectName(resolvedProjectPath),
			}

			agents, err := mgr.List(context.Background(), filters)
			if err != nil {
				return err
			}

			var deletedCount int
			for _, a := range agents {
				if a.ContainerID == "" {
					continue // No container
				}

				// Get the canonical agent name from labels (Docker Names field has leading slash)
				agentName := a.Labels["scion.name"]
				if agentName == "" {
					continue // Not a scion-managed container
				}

				// Skip running/provisioning agents
				if a.Phase == string(state.PhaseRunning) || a.Phase == string(state.PhaseProvisioning) {
					continue
				}

				statusf("Deleting stopped agent '%s' (status: %s)...\n", agentName, a.ContainerStatus)

				targetProjectPath := a.ProjectPath
				if targetProjectPath == "" {
					targetProjectPath = resolvedProjectPath
				}

				branchDeleted, err := mgr.Delete(context.Background(), agentName, true, targetProjectPath, !preserveBranch)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Failed to delete agent '%s': %v\n", agentName, err)
					continue
				}

				if branchDeleted {
					statusf("Git branch associated with agent '%s' deleted.\n", agentName)
				}
				statusf("Agent '%s' deleted.\n", agentName)
				deletedCount++
			}

			if deletedCount == 0 {
				statusln("No stopped agents found.")
			}
			return nil
		}

		// Use Hub if available
		if hubCtx != nil {
			return deleteAgentsViaHub(hubCtx, args)
		}

		// Local mode - delete each agent
		var errs []string
		var results []map[string]interface{}
		for _, agentName := range args {
			if err := deleteAgentLocal(agentName); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", agentName, err))
				if isJSONOutput() {
					results = append(results, map[string]interface{}{
						"agent":  agentName,
						"status": "error",
						"error":  err.Error(),
					})
				}
			} else if isJSONOutput() {
				results = append(results, map[string]interface{}{
					"agent":  agentName,
					"status": "success",
				})
			}
		}

		if isJSONOutput() {
			status := "success"
			if len(errs) > 0 {
				status = "partial"
			}
			return outputJSON(map[string]interface{}{
				"status":  status,
				"command": "delete",
				"results": results,
			})
		}

		if len(errs) > 0 {
			return fmt.Errorf("failed to delete some agents:\n  %s", strings.Join(errs, "\n  "))
		}
		return nil
	},
}

// hubDeletePollConcurrency caps how many accepted (202) deletes
// deleteAgentsViaHub polls at once. Tests override it.
var hubDeletePollConcurrency = 4

func deleteAgentsViaHub(hubCtx *HubContext, agentNames []string) error {
	PrintUsingHub(hubCtx.Endpoint)

	opts := &hubclient.DeleteAgentOptions{
		DeleteFiles:  true,
		RemoveBranch: !preserveBranch,
		Force:        deleteForce,
	}

	// Send the DELETEs one at a time, in order. Each 202 starts a poll in
	// the background (at most hubDeletePollConcurrency at once), so a slow
	// broker costs one poll budget for the whole run, not one per agent.
	// Results are then handled in input order, which keeps the output and
	// the local cleanup (git worktree removal) sequential and deterministic.
	agentSvc := hubCtx.Client.ProjectAgents(hubCtx.ProjectID)
	type hubDeleteJob struct {
		outcome hubDeleteOutcome
		err     error
		done    chan struct{}
	}
	jobs := make([]*hubDeleteJob, len(agentNames))
	sem := make(chan struct{}, max(hubDeletePollConcurrency, 1))
	for i, agentName := range agentNames {
		statusf("Deleting agent '%s'...\n", agentName)
		job := &hubDeleteJob{done: make(chan struct{})}
		jobs[i] = job

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		// Use project-scoped client which supports agent lookup by name/slug.
		// On 202 the hub is still deleting; the job polls until the outcome
		// is known (design ptone/scion#2483 R4).
		sent, err := sendHubDelete(ctx, agentSvc, agentName, opts)
		cancel()
		if err != nil || !sent.NeedsPoll() {
			job.err = err
			if err == nil {
				job.outcome = sent.Wait(context.Background()) // known already; no poll
			}
			close(job.done)
			continue
		}
		statusf("Agent '%s': the Hub is still deleting it; waiting for the delete to finish...\n", agentName)
		go func() {
			defer close(job.done)
			sem <- struct{}{}
			defer func() { <-sem }()
			// The poll has its own budget (hubDeletionWaitOptions); it does
			// not inherit the DELETE request's timeout.
			job.outcome = sent.Wait(context.Background())
		}()
	}

	var errs []string
	var results []map[string]interface{}
	for i, agentName := range agentNames {
		job := jobs[i]
		<-job.done
		outcome, err := job.outcome, job.err
		if err != nil {
			err = wrapHubError(err)
		} else {
			err = hubDeleteFailure(agentName, outcome, "local worktree kept")
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", agentName, err))
			if isJSONOutput() {
				results = append(results, map[string]interface{}{
					"agent":  agentName,
					"status": "error",
					"error":  err.Error(),
				})
			}
			continue
		}

		if !outcome.Confirmed() {
			// Accepted, but completion could not be observed (403 or poll
			// timeout; failures were handled above). Not a failure, but
			// nothing local is touched: the worktree is kept, and the sync
			// state is left alone so that a later sync sees the agent as
			// stale once the hub finishes.
			msg := hubDeletePendingMessage(outcome)
			if isJSONOutput() {
				results = append(results, map[string]interface{}{
					"agent":        agentName,
					"status":       "accepted",
					"message":      msg,
					"worktreeKept": true,
				})
			} else {
				statusf("Agent '%s': %s.\n", agentName, msg)
			}
			continue
		}

		// Confirmed (204, or 202 then the poll saw the agent gone).
		// Also clean up local agent files (worktree, agent directory).
		// The Hub dispatches container cleanup to the runtime broker, but local
		// filesystem artifacts must be removed by the CLI to avoid orphaned agents.
		branchDeleted, err := agent.DeleteAgentFiles(agentName, projectPath, !preserveBranch)
		if err != nil {
			statusf("Warning: Hub record deleted but local cleanup failed for '%s': %v\n", agentName, err)
			statusf("Run 'scion --no-hub delete %s' to retry targeted cleanup, or 'scion clean' to reset the project.\n", agentName)
		}

		// Keep sync watermark current after a successful Hub delete. If hub server
		// time is unavailable in this flow, UpdateLastSyncedAt falls back to local UTC.
		if hubCtx != nil && hubCtx.ProjectPath != "" {
			hubsync.UpdateLastSyncedAt(hubCtx.ProjectPath, time.Time{})
			hubsync.RemoveSyncedAgent(hubCtx.ProjectPath, agentName)
		}
		if branchDeleted {
			statusf("Git branch associated with agent '%s' deleted.\n", agentName)
		}

		if isJSONOutput() {
			results = append(results, map[string]interface{}{
				"agent":  agentName,
				"status": "success",
			})
		} else {
			statusf("Agent '%s' deleted via Hub.\n", agentName)
		}
	}

	if isJSONOutput() {
		status := "success"
		if len(errs) > 0 {
			status = "partial"
		}
		return outputJSON(map[string]interface{}{
			"status":  status,
			"command": "delete",
			"results": results,
		})
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to delete some agents via Hub:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func deleteStoppedViaHub(hubCtx *HubContext) error {
	PrintUsingHub(hubCtx.Endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agentSvc := hubCtx.Client.ProjectAgents(hubCtx.ProjectID)
	resp, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{Phase: "stopped"})
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list agents via Hub: %w", err))
	}

	if len(resp.Agents) == 0 {
		statusln("No stopped agents found.")
		return nil
	}

	var agentNames []string
	for _, a := range resp.Agents {
		agentNames = append(agentNames, a.Name)
	}

	return deleteAgentsViaHub(hubCtx, agentNames)
}

func deleteAgentLocal(agentName string) error {
	rt := runtime.GetRuntime(projectPath, profile)
	mgr := agent.NewManager(rt)

	fmt.Printf("Deleting agent '%s'...\n", agentName)

	// We check if it exists in List to provide better feedback
	util.Debugf("delete: listing containers for %s", agentName)
	listStart := time.Now()
	agents, _ := mgr.List(context.Background(), map[string]string{"scion.name": agentName})
	util.Debugf("delete: container list completed in %v", time.Since(listStart))
	containerFound := false
	for _, a := range agents {
		if strings.EqualFold(a.Name, agentName) || a.ID == agentName || strings.EqualFold(strings.TrimPrefix(a.Name, "/"), agentName) {
			containerFound = true
			break
		}
	}

	if !containerFound {
		// Check if agent definition exists on the filesystem
		agentDirExists := false
		if projectDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
			if _, err := os.Stat(filepath.Join(projectDir, "agents", agentName)); err == nil {
				agentDirExists = true
			}
		}
		if !agentDirExists {
			if globalDir, err := config.GetGlobalAgentsDir(); err == nil {
				if _, err := os.Stat(filepath.Join(globalDir, agentName)); err == nil {
					agentDirExists = true
				}
			}
		}
		if !agentDirExists {
			return fmt.Errorf("agent '%s' not found", agentName)
		}
		fmt.Println("No container found, removing agent definition...")
	}

	branchDeleted, err := mgr.Delete(context.Background(), agentName, true, projectPath, !preserveBranch)
	if err != nil {
		return err
	}

	if branchDeleted {
		fmt.Printf("Git branch associated with agent '%s' deleted.\n", agentName)
	}

	fmt.Printf("Agent '%s' deleted.\n", agentName)
	return nil
}

var preserveBranch bool

func init() {
	rootCmd.AddCommand(deleteCmd)
	deleteCmd.Flags().BoolVarP(&preserveBranch, "preserve-branch", "b", false, "Preserve the git branch associated with the worktree")
	deleteCmd.Flags().BoolVar(&deleteStopped, "stopped", false, "Delete all agents with stopped containers")
	deleteCmd.Flags().BoolVarP(&deleteForce, "force", "f", false, "Delete from the Hub even if the broker is unreachable or cannot find the agent; permanent (no soft-delete)")
}
