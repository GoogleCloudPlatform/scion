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
	"time"

	"github.com/spf13/cobra"
)

// workflowCancelCmd cancels a workflow run.
var workflowCancelCmd = &cobra.Command{
	Use:   "cancel <run-id>",
	Short: "Cancel a workflow run",
	Long: `Request cancellation of a workflow run.

The Hub returns an error (HTTP 409) if the run is already in a terminal state
(succeeded, failed, timed_out, canceled). The updated run record is printed
after a successful cancellation request.

Examples:
  scion workflow cancel abc123
  scion workflow cancel abc123 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowCancel,
}

func runWorkflowCancel(cmd *cobra.Command, args []string) error {
	runID := args[0]

	settings, err := loadSettingsForWorkflow()
	if err != nil {
		return err
	}

	client, err := getHubClient(settings)
	if err != nil {
		return fmt.Errorf("connecting to Hub: %w", err)
	}

	if !isJSONOutput() {
		PrintUsingHub(GetHubEndpoint(settings))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, err := client.CancelWorkflowRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("canceling workflow run: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(run)
	}

	fmt.Printf("Run %s: %s\n", run.ID, run.Status)
	return nil
}

func init() {
	workflowCmd.AddCommand(workflowCancelCmd)
}
