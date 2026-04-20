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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// streamWorkflowLogs consumes the event channel and prints log lines and
// status events to stdout. Returns the terminal status string (or "" if the
// stream ended without a terminal event) and any error.
//
// If follow is false, exits on the first terminal event.
// If jsonOut is true, each event is emitted as a single JSON line.
func streamWorkflowLogs(ctx context.Context, ch <-chan hubclient.LogEvent, runID string, follow, jsonOut bool) (string, error) {
	_ = ctx // reserved for future cancellation propagation

	terminalStatus := ""

	for evt := range ch {
		// Always handle terminal events.
		if evt.Event == "terminal" {
			terminalStatus = evt.Status
			if jsonOut {
				b, _ := json.Marshal(evt)
				fmt.Println(string(b))
			} else {
				statusf("Run %s: %s\n", runID, evt.Status)
			}
			if !follow {
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

	return terminalStatus, nil
}
