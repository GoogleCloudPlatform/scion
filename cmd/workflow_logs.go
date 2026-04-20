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
	"fmt"

	"github.com/spf13/cobra"
)

// workflowLogs flags
var (
	workflowLogsFollow bool
)

// workflowLogsCmd streams log events for a workflow run.
var workflowLogsCmd = &cobra.Command{
	Use:   "logs <run-id>",
	Short: "Stream or replay log events for a workflow run",
	Long: `Print log events for a workflow run.

Without -f/--follow: drains the buffered backlog of events already emitted by
the run and exits. If the run is already terminal, the channel is closed
immediately after replay.

With -f/--follow: stays connected to the WebSocket and streams live events
until the server sends a terminal event or closes the connection.

Use --json to emit each event as NDJSON (one JSON object per line).

Examples:
  scion workflow logs abc123
  scion workflow logs abc123 -f
  scion workflow logs abc123 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowLogs,
}

func runWorkflowLogs(cmd *cobra.Command, args []string) error {
	runID := args[0]

	settings, err := loadSettingsForWorkflow()
	if err != nil {
		return err
	}

	client, err := getHubClient(settings)
	if err != nil {
		return fmt.Errorf("connecting to Hub: %w", err)
	}

	statusf("Connecting to log stream for run %s...\n", runID)

	ctx := cmd.Context()
	ch, err := client.StreamWorkflowRunLogs(ctx, runID)
	if err != nil {
		return fmt.Errorf("opening log stream: %w", err)
	}

	jsonOut := isJSONOutput()

	if _, err := streamWorkflowLogs(ctx, ch, runID, workflowLogsFollow, jsonOut); err != nil {
		return err
	}

	return nil
}

func init() {
	workflowCmd.AddCommand(workflowLogsCmd)

	workflowLogsCmd.Flags().BoolVarP(&workflowLogsFollow, "follow", "f", false, "Follow log stream; stay connected until the run reaches a terminal state")
}
