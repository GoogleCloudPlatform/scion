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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var resetAuthCmd = &cobra.Command{
	Use:   "reset-auth [<agent> | --all --reissue-scopes]",
	Short: "Reset authentication for a running agent",
	Long: `Inject a fresh Hub token into a running agent without restarting it.

This is useful when an agent's token has expired and cannot be refreshed
(e.g., after hub signing key rotation). The command generates a new token
on the Hub, pushes it into the agent's container, and signals the agent
to restart its token refresh loop.

The agent must be running — stopped agents get a fresh token on next start.

With --reissue-scopes, the Hub instead re-issues the agent's role scopes
from its delegator's current authority: the agent's delegation record is
re-recorded through the same checks agent creation applies, the agent's
current credentials are revoked, and a new token is pushed to a running
agent. Scopes the delegator no longer supports are removed. The operation
requires a hub super-admin session and is audited. Use --dry-run to see the
change without applying it. Descendant agents are not changed; re-issue
them one by one, parents first, or use --all.

With --all --reissue-scopes, every agent on the Hub is re-issued, parents
before children within each project. This is a dry run unless --apply is
given: review the summary first, then apply.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if resetAuthAll {
			if len(args) != 0 {
				return fmt.Errorf("--all takes no agent argument")
			}
			return nil
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return getAgentNames(cmd, args, toComplete)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if resetAuthAll && !resetAuthReissueScopes {
			return fmt.Errorf("--all requires --reissue-scopes")
		}
		if resetAuthApply && !resetAuthAll {
			return fmt.Errorf("--apply applies only with --all --reissue-scopes")
		}
		if resetAuthAll && resetAuthDryRun {
			return fmt.Errorf("--all is a dry run unless --apply is given; --dry-run is not used with --all")
		}

		hubCtx, err := CheckHubAvailability(projectPath)
		if err != nil {
			return err
		}
		if hubCtx == nil {
			return fmt.Errorf("reset-auth requires Hub connectivity (hub not configured)")
		}

		if resetAuthAll {
			return reissueScopesAllViaHub(hubCtx, resetAuthApply)
		}
		agentName := api.Slugify(args[0])
		if resetAuthReissueScopes {
			return reissueScopesViaHub(hubCtx, agentName, resetAuthDryRun)
		}
		if resetAuthDryRun {
			return fmt.Errorf("--dry-run applies only with --reissue-scopes")
		}
		return resetAuthViaHub(hubCtx, agentName)
	},
}

var (
	resetAuthReissueScopes bool
	resetAuthDryRun        bool
	resetAuthAll           bool
	resetAuthApply         bool
)

func init() {
	resetAuthCmd.Flags().BoolVar(&resetAuthReissueScopes, "reissue-scopes", false,
		"Re-issue the agent's role scopes from its delegator's current authority (hub super-admin only)")
	resetAuthCmd.Flags().BoolVar(&resetAuthDryRun, "dry-run", false,
		"With --reissue-scopes: show the change without applying it")
	resetAuthCmd.Flags().BoolVar(&resetAuthAll, "all", false,
		"With --reissue-scopes: re-issue every agent on the Hub (dry run unless --apply)")
	resetAuthCmd.Flags().BoolVar(&resetAuthApply, "apply", false,
		"With --all --reissue-scopes: apply the re-issue instead of a dry run")
	rootCmd.AddCommand(resetAuthCmd)
}

func reissueScopesAllViaHub(hubCtx *HubContext, apply bool) error {
	PrintUsingHub(hubCtx.Endpoint)
	if apply {
		statusf("Re-issuing scopes for every agent on the Hub...\n")
	} else {
		statusf("Computing scope re-issue for every agent on the Hub (dry run)...\n")
	}
	// The hub bounds the run with its own deadline (30 minutes,
	// hub.ReissueBulkRunTimeout); wait a little longer for the response.
	ctx, cancel := context.WithTimeout(context.Background(), reissueBulkCLITimeout)
	defer cancel()
	reissuer, ok := hubCtx.Client.Agents().(hubclient.BulkScopeReissuer)
	if !ok {
		return fmt.Errorf("this Hub client does not support scope re-issue")
	}
	res, err := reissuer.ReissueScopesAll(ctx, apply)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to re-issue scopes via Hub: %w", err))
	}
	for _, a := range res.Agents {
		if a.Outcome == "noop" {
			continue
		}
		statusf("%s (%s) %s", a.Name, a.ID, a.Outcome)
		if a.Cause != "" {
			statusf(": %s", a.Cause)
		}
		statusf("\n")
		if a.RoleBefore != a.RoleAfter && a.RoleAfter != "" {
			statusf("  role:     %s -> %s\n", a.RoleBefore, a.RoleAfter)
		}
		if len(a.Added) > 0 {
			printScopeList("added", a.Added)
		}
		if len(a.Removed) > 0 {
			printScopeList("removed", a.Removed)
		}
		if a.DispatchError != "" {
			statusf("  token not delivered: %s\n", a.DispatchError)
		}
	}
	verb := "would change"
	if !res.DryRun {
		verb = "changed"
	}
	statusf("%d agents: %d %s, %d unchanged, %d refused, %d token not delivered\n",
		res.Total, len(res.Succeeded), verb, len(res.Noop), len(res.Refused), len(res.PushFailed))
	if res.DryRun {
		statusf("Dry run: nothing was changed. Re-run with --apply to apply.\n")
	}
	if len(res.DepthUnresolved) > 0 {
		statusf("%d agents could not be ordered (their delegation record could not be read); see their results above\n", len(res.DepthUnresolved))
	}
	if !res.BatchAuditRecorded {
		return fmt.Errorf("the hub could not record the batch audit entry (batch %s); the per-agent results above stand", res.BatchOpID)
	}
	if len(res.PushFailed) > 0 {
		return fmt.Errorf("%d agents did not receive their new token; run reset-auth for each", len(res.PushFailed))
	}
	return nil
}

// reissueBulkCLITimeout is how long the CLI waits for a bulk re-issue: the
// hub's run deadline plus a margin.
const reissueBulkCLITimeout = 31 * time.Minute

func reissueScopesViaHub(hubCtx *HubContext, agentName string, dryRun bool) error {
	PrintUsingHub(hubCtx.Endpoint)
	if dryRun {
		statusf("Computing scope re-issue for agent '%s' (dry run)...\n", agentName)
	} else {
		statusf("Re-issuing scopes for agent '%s'...\n", agentName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}
	reissuer, ok := hubCtx.Client.ProjectAgents(projectID).(hubclient.ScopeReissuer)
	if !ok {
		return fmt.Errorf("this Hub client does not support scope re-issue")
	}
	res, err := reissuer.ReissueScopes(ctx, agentName, dryRun)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to re-issue scopes via Hub: %w", err))
	}
	printScopeReissueResult(res)
	if res.DispatchError != "" {
		return fmt.Errorf("scopes re-issued, but the new token was not delivered: %s", res.DispatchError)
	}
	return nil
}

func printScopeReissueResult(res *hubclient.ScopeReissueResult) {
	statusf("%s\n", res.Message)
	statusf("  role:     %s -> %s\n", res.RoleBefore, res.RoleAfter)
	statusf("  source:   %s %s (%s ceiling)\n", res.CeilingSource.DelegatorKind, res.CeilingSource.DelegatorID, res.CeilingSource.CeilingKind)
	printScopeList("added", res.Added)
	printScopeList("removed", res.Removed)
	if len(res.Withheld) > 0 {
		statusf("  withheld:\n")
		for _, w := range res.Withheld {
			statusf("    %s (%s)\n", w.Scope, w.Cause)
		}
	}
	if !res.DryRun && !res.Noop {
		statusf("  credentials revoked: %d; token dispatched: %t\n", res.CredentialsRevoked, res.Dispatched)
	}
}

func printScopeList(label string, scopes []string) {
	if len(scopes) == 0 {
		statusf("  %-9s (none)\n", label+":")
		return
	}
	statusf("  %-9s %s\n", label+":", strings.Join(scopes, ", "))
}

func resetAuthViaHub(hubCtx *HubContext, agentName string) error {
	PrintUsingHub(hubCtx.Endpoint)
	statusf("Resetting auth for agent '%s'...\n", agentName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	agentSvc := hubCtx.Client.ProjectAgents(projectID)
	if err := agentSvc.ResetAuth(ctx, agentName); err != nil {
		return wrapHubError(fmt.Errorf("failed to reset auth via Hub: %w", err))
	}

	statusf("Auth reset dispatched for agent '%s'. The agent will pick up the new token shortly.\n", agentName)
	return nil
}
