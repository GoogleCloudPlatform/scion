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
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// workflowList flags
var (
	workflowListStatus string
	workflowListLimit  int
	workflowListCursor string
	workflowListGrove  string
)

// workflowListCmd lists workflow runs in a grove.
var workflowListCmd = &cobra.Command{
	Use:   "list",
	Short: "List workflow runs in the current grove",
	Long: `List workflow runs dispatched to the Hub for the current grove.

Requires Hub mode. Use --grove to specify a grove explicitly, or configure
a Hub grove in settings.yaml.

Examples:
  scion workflow list
  scion workflow list --status succeeded
  scion workflow list --limit 5
  scion workflow list --json`,
	RunE: runWorkflowList,
}

func runWorkflowList(cmd *cobra.Command, args []string) error {
	settings, err := loadSettingsForWorkflow()
	if err != nil {
		return err
	}

	client, groveID, err := resolveWorkflowHubClient(settings, workflowListGrove)
	if err != nil {
		return err
	}

	if !isJSONOutput() {
		PrintUsingHub(GetHubEndpoint(settings))
	}

	opts := &hubclient.ListWorkflowRunsOptions{
		Status: workflowListStatus,
		Page: apiclient.PageOptions{
			Limit:  workflowListLimit,
			Cursor: workflowListCursor,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runs, nextCursor, err := client.ListWorkflowRuns(ctx, groveID, opts)
	if err != nil {
		return fmt.Errorf("listing workflow runs: %w", err)
	}

	if isJSONOutput() {
		type listOut struct {
			Runs       []hubclient.WorkflowRun `json:"runs"`
			NextCursor string                  `json:"nextCursor,omitempty"`
		}
		return outputJSON(listOut{Runs: runs, NextCursor: nextCursor})
	}

	if len(runs) == 0 {
		fmt.Println("No workflow runs found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tCREATED\tSOURCE")
	for _, run := range runs {
		id := run.ID
		if len(id) > 8 {
			id = id[:8]
		}
		created := formatRelativeTime(run.CreatedAt)
		source := "-"
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", id, run.Status, created, source)
	}
	w.Flush()

	if nextCursor != "" {
		fmt.Fprintf(os.Stderr, "\nMore results available. Use --cursor %s to continue.\n", nextCursor)
	}

	return nil
}

// resolveWorkflowHubClient resolves the hub client and grove ID for workflow commands.
// groveIDOverride may be empty (uses hub context resolution).
func resolveWorkflowHubClient(settings *config.Settings, groveIDOverride string) (hubclient.Client, string, error) {
	client, err := getHubClient(settings)
	if err != nil {
		return nil, "", fmt.Errorf("connecting to Hub: %w", err)
	}

	groveID := groveIDOverride

	if groveID == "" {
		hubCtx, hubErr := CheckHubAvailabilityWithOptions(grovePath, true)
		if hubErr != nil {
			return nil, "", fmt.Errorf("resolving grove: %w\n\nUse --grove <id> to specify a grove ID explicitly", hubErr)
		}
		if hubCtx != nil {
			var lookupErr error
			groveID, lookupErr = GetGroveID(hubCtx)
			if lookupErr != nil {
				return nil, "", fmt.Errorf("resolving grove ID: %w", lookupErr)
			}
			client = hubCtx.Client
		}
	}

	if groveID == "" {
		return nil, "", fmt.Errorf("no grove ID resolved; use --grove <id> or configure a Hub grove in settings.yaml")
	}

	return client, groveID, nil
}

func init() {
	workflowCmd.AddCommand(workflowListCmd)

	workflowListCmd.Flags().StringVar(&workflowListGrove, "grove", "", "Grove ID for Hub lookup (overrides grove resolution)")
	workflowListCmd.Flags().StringVar(&workflowListStatus, "status", "", "Filter by status (pending, running, succeeded, failed, timed_out, canceled)")
	workflowListCmd.Flags().IntVar(&workflowListLimit, "limit", 20, "Maximum number of runs to return")
	workflowListCmd.Flags().StringVar(&workflowListCursor, "cursor", "", "Pagination cursor from a previous list response")
}
