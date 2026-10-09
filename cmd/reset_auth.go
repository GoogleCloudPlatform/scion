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
	Use:   "reset-auth <agent>",
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
them one by one, parents first.`,
	Args: cobra.ExactArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return getAgentNames(cmd, args, toComplete)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])

		hubCtx, err := CheckHubAvailability(projectPath)
		if err != nil {
			return err
		}
		if hubCtx == nil {
			return fmt.Errorf("reset-auth requires Hub connectivity (hub not configured)")
		}

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
)

func init() {
	resetAuthCmd.Flags().BoolVar(&resetAuthReissueScopes, "reissue-scopes", false,
		"Re-issue the agent's role scopes from its delegator's current authority (hub super-admin only)")
	resetAuthCmd.Flags().BoolVar(&resetAuthDryRun, "dry-run", false,
		"With --reissue-scopes: show the change without applying it")
	rootCmd.AddCommand(resetAuthCmd)
}

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
