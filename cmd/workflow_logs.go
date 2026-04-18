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
	"encoding/json"
	"fmt"
	"os"

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

	ctx := context.Background()
	ch, err := client.StreamWorkflowRunLogs(ctx, runID)
	if err != nil {
		return fmt.Errorf("opening log stream: %w", err)
	}

	jsonOut := isJSONOutput()

	for evt := range ch {
		// Always print terminal event metadata.
		if evt.Event == "terminal" {
			if jsonOut {
				b, _ := json.Marshal(evt)
				fmt.Println(string(b))
			} else {
				statusf("Run %s: %s\n", runID, evt.Status)
			}
			if !workflowLogsFollow {
				// Drain remaining messages then stop.
				break
			}
			// In follow mode a terminal event means the server will close the
			// connection next; keep reading until the channel is drained.
			continue
		}

		// "log" events carry actual workflow output lines — print them.
		// Other non-empty event types (e.g. "status", "logs_not_yet_wired") are
		// control events that are only emitted in JSON mode.
		if evt.Event == "log" {
			if jsonOut {
				b, _ := json.Marshal(evt)
				fmt.Println(string(b))
				continue
			}
			if evt.Line != "" {
				ts := evt.TS
				if ts == "" {
					ts = "-"
				}
				stream := evt.Stream
				if stream == "" {
					stream = "stdout"
				}
				if stream == "stderr" {
					fmt.Fprintf(os.Stderr, "[%s] [%s] %s\n", ts, stream, evt.Line)
				} else {
					fmt.Printf("[%s] [%s] %s\n", ts, stream, evt.Line)
				}
			}
			continue
		}

		// Skip other non-terminal control events (e.g. "status", "logs_not_yet_wired").
		if evt.Event != "" {
			if jsonOut {
				b, _ := json.Marshal(evt)
				fmt.Println(string(b))
			}
			continue
		}

		// Fallback: events without an Event field (old wire format or direct line).
		if jsonOut {
			b, _ := json.Marshal(evt)
			fmt.Println(string(b))
			continue
		}

		if evt.Line != "" {
			ts := evt.TS
			if ts == "" {
				ts = "-"
			}
			stream := evt.Stream
			if stream == "" {
				stream = "stdout"
			}
			if stream == "stderr" {
				fmt.Fprintf(os.Stderr, "[%s] [%s] %s\n", ts, stream, evt.Line)
			} else {
				fmt.Printf("[%s] [%s] %s\n", ts, stream, evt.Line)
			}
		}
	}

	return nil
}

func init() {
	workflowCmd.AddCommand(workflowLogsCmd)

	workflowLogsCmd.Flags().BoolVarP(&workflowLogsFollow, "follow", "f", false, "Follow log stream; stay connected until the run reaches a terminal state")
}
