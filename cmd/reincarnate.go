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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	reincarnateHandoffFile string
	reincarnateDryRun      bool
)

// reincarnateCmd represents the `scion reincarnate` command (design
// /scion-volumes/scratchpad/projects/agent-migrate/design.md §3.2): stop an
// agent, re-resolve its configuration against the current template/harness
// catalog, and start a fresh generation with the same identity, handing it
// an agent-authored handoff as its first task. Phase 1 supports only the
// handoff and dry-run; every override flag (--image, --model, --harness,
// --rollback, etc.) is design-scoped for later phases.
var reincarnateCmd = &cobra.Command{
	Use:   "reincarnate [agent]",
	Short: "Migrate an agent to a fresh generation (new template/config, same identity)",
	Long: `Reincarnate stops an agent, re-resolves its configuration against the
current template and harness-config catalog, and starts a new generation
with the same agent ID and slug. The new generation's first task is a
hub-built preamble plus the handoff you provide with --handoff-file.

Run with no argument inside an agent container to migrate yourself
(self-migration); a handoff file is required in that case, since there is no
one else to describe the work in progress. When migrating another agent, the
handoff is optional.

Use --dry-run to see the planned changes (template, image, harness config,
model, env keys, branch) without migrating anything.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 1 {
			return fmt.Errorf("accepts at most 1 argument (agent name)")
		}
		return nil
	},
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		selfName := os.Getenv("SCION_AGENT_NAME")

		var agentName string
		if len(args) == 1 {
			agentName = api.Slugify(args[0])
		} else {
			if selfName == "" {
				return fmt.Errorf("specify an agent name, or run this inside an agent container to migrate yourself")
			}
			agentName = api.Slugify(selfName)
		}

		isSelf := selfName != "" && api.Slugify(selfName) == agentName

		var handoff string
		if reincarnateHandoffFile != "" {
			data, err := os.ReadFile(reincarnateHandoffFile)
			if err != nil {
				return fmt.Errorf("failed to read --handoff-file: %w", err)
			}
			handoff = string(data)
		} else if isSelf && !reincarnateDryRun {
			return fmt.Errorf("self-migration requires --handoff-file: write a handoff describing your work in progress, canonical files, and next action for the new generation, then pass it with --handoff-file")
		}

		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, true)
		if err != nil {
			return err
		}
		if hubCtx == nil {
			return fmt.Errorf("agent migration requires hub mode")
		}

		return reincarnateAgentViaHub(hubCtx, agentName, handoff, isSelf)
	},
}

func reincarnateAgentViaHub(hubCtx *HubContext, agentName, handoff string, isSelf bool) error {
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	req := &hubclient.ReincarnateAgentRequest{
		Handoff: handoff,
		DryRun:  reincarnateDryRun,
	}

	if reincarnateDryRun {
		statusf("Resolving reincarnation plan for '%s'...\n", agentName)
	} else if isSelf {
		statusf("Reincarnating '%s' (self-migration)...\n", agentName)
	} else {
		statusf("Reincarnating '%s'...\n", agentName)
	}

	resp, err := agentSvc.Reincarnate(ctx, agentName, req)
	if err != nil {
		if apiclient.IsConflictError(err) {
			return wrapHubError(fmt.Errorf("a reincarnation is already pending for '%s': %w", agentName, err))
		}
		return wrapHubError(fmt.Errorf("failed to reincarnate agent via Hub: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(resp)
	}

	printReincarnationPlan(resp.Plan)

	if reincarnateDryRun {
		fmt.Println("\nDry run only; nothing was changed.")
		return nil
	}

	fmt.Printf("\nAgent '%s' is reincarnating: generation %d, state=%s.\n", agentName, resp.Generation, resp.State)
	if isSelf {
		fmt.Println("This container will be stopped shortly as part of the migration.")
	}
	return nil
}

// printReincarnationPlan renders a ReincarnationPlan as plain text: old vs
// new for each scalar field, added/removed/changed env key names (never
// values, per design §3.2), the resolved branch, and any warnings from a
// legacy-agent CreateInputs reconstruction.
func printReincarnationPlan(plan hubclient.ReincarnationPlan) {
	printChange := func(label string, c hubclient.FieldChange) {
		if c.Old == c.New {
			fmt.Printf("  %-14s %s (unchanged)\n", label+":", valueOrNone(c.New))
			return
		}
		fmt.Printf("  %-14s %s -> %s\n", label+":", valueOrNone(c.Old), valueOrNone(c.New))
	}

	fmt.Println("Reincarnation plan:")
	printChange("Template", plan.Template)
	printChange("Image", plan.Image)
	printChange("Harness cfg", plan.HarnessCfg)
	printChange("Model", plan.Model)
	fmt.Printf("  %-14s %s\n", "Branch:", valueOrNone(plan.Branch))

	if len(plan.EnvKeys.Added) > 0 {
		fmt.Printf("  Env added:     %s\n", strings.Join(plan.EnvKeys.Added, ", "))
	}
	if len(plan.EnvKeys.Removed) > 0 {
		fmt.Printf("  Env removed:   %s\n", strings.Join(plan.EnvKeys.Removed, ", "))
	}
	if len(plan.EnvKeys.Changed) > 0 {
		fmt.Printf("  Env changed:   %s\n", strings.Join(plan.EnvKeys.Changed, ", "))
	}

	for _, w := range plan.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}
}

func init() {
	reincarnateCmd.Flags().StringVar(&reincarnateHandoffFile, "handoff-file", "", "File whose content becomes the new generation's first task (required for self-migration)")
	reincarnateCmd.Flags().BoolVar(&reincarnateDryRun, "dry-run", false, "Print the resolved reincarnation plan without migrating anything")
	rootCmd.AddCommand(reincarnateCmd)
}
