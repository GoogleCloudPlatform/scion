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

// workflowGet flags
var (
	workflowGetShowSource bool
)

// workflowGetCmd retrieves details of a single workflow run.
var workflowGetCmd = &cobra.Command{
	Use:   "get <run-id>",
	Short: "Get details of a workflow run",
	Long: `Show detailed information about a workflow run.

Fields printed: id, grove, status, created, started, finished, error,
broker, trace URL. Use --show-source to also print the submitted YAML.

Examples:
  scion workflow get abc123
  scion workflow get abc123 --json
  scion workflow get abc123 --show-source`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowGet,
}

func runWorkflowGet(cmd *cobra.Command, args []string) error {
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

	include := []string{}
	if workflowGetShowSource {
		include = append(include, "source")
	}

	run, err := client.GetWorkflowRun(ctx, runID, include...)
	if err != nil {
		return fmt.Errorf("getting workflow run: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(run)
	}

	fmt.Printf("ID:      %s\n", run.ID)
	fmt.Printf("Grove:   %s\n", run.GroveID)
	fmt.Printf("Status:  %s\n", run.Status)
	fmt.Printf("Created: %s\n", run.CreatedAt.Format(time.RFC3339))

	if run.StartedAt != nil {
		fmt.Printf("Started: %s\n", run.StartedAt.Format(time.RFC3339))
	}
	if run.FinishedAt != nil {
		fmt.Printf("Finished: %s\n", run.FinishedAt.Format(time.RFC3339))
	}
	if run.BrokerID != nil {
		fmt.Printf("Broker:  %s\n", *run.BrokerID)
	}
	if run.TraceURL != nil {
		fmt.Printf("Trace:   %s\n", *run.TraceURL)
	}
	if run.Error != nil && *run.Error != "" {
		fmt.Printf("Error:   %s\n", *run.Error)
	}
	if workflowGetShowSource && run.Source != nil {
		fmt.Printf("\nSource:\n%s\n", *run.Source)
	}

	return nil
}

func init() {
	workflowCmd.AddCommand(workflowGetCmd)

	workflowGetCmd.Flags().BoolVar(&workflowGetShowSource, "show-source", false, "Include the submitted workflow YAML source in output")
}
