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
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var hubHealthCmd = &cobra.Command{
	Use:   "health",
	Short: "Show Hub subsystem health and runtime metrics",
	Long: `Show comprehensive subsystem health, database connection pool metrics,
and runtime broker status for the Scion Hub.

Queries the Hub health summary telemetry endpoint to report:
  - Hub server status, version, uptime, and fleet counts
  - Database pool counters (active, max, idle, wait count total)
  - Runtime broker status, engine readiness, and heartbeat latency
  - Fleet agent tallies by lifecycle phase
  - Callouts for stalled, crashed, or errored agents

Examples:
  # Show Hub health and subsystem scorecard
  scion hub health

  # Query a specific Hub endpoint
  scion hub health --hub https://hub.example.com/

  # Output as JSON for automated monitoring or agent consumption
  scion hub health --json`,
	RunE: runHubHealth,
}

func init() {
	hubHealthCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output as JSON")
	hubCmd.AddCommand(hubHealthCmd)
}

func runHubHealth(cmd *cobra.Command, args []string) error {
	if hubOutputJSON {
		outputFormat = "json"
	}

	gp := projectPath
	if gp == "" && globalMode {
		gp = "global"
	}

	resolvedPath, _, err := config.ResolveProjectPath(gp)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return fmt.Errorf("failed to initialize Hub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	summary, err := client.HealthSummary(ctx)
	if err != nil {
		// If admin health summary is forbidden (non-admin token), attempt basic Health fallback
		if strings.Contains(err.Error(), "403") {
			basicHealth, basicErr := client.Health(ctx)
			if basicErr == nil {
				if isJSONOutput() {
					return outputJSON(map[string]interface{}{
						"status":       basicHealth.Status,
						"version":      basicHealth.Version,
						"scionVersion": basicHealth.ScionVersion,
						"uptime":       basicHealth.Uptime,
						"checks":       basicHealth.Checks,
						"note":         "Detailed subsystem health summary requires admin role; showing basic health.",
					})
				}
				fmt.Printf("Hub Status: %s\n", basicHealth.Status)
				fmt.Printf("Version:    %s (scion %s)\n", basicHealth.Version, basicHealth.ScionVersion)
				fmt.Printf("Uptime:     %s\n", basicHealth.Uptime)
				fmt.Println("\nNote: Full subsystem telemetry requires Hub admin role.")
				return nil
			}
		}
		return fmt.Errorf("failed to fetch Hub health summary: %w", hubclient.HintProxyError(err))
	}

	if isJSONOutput() {
		return outputJSON(summary)
	}

	printHubHealthSummary(os.Stdout, summary, settings.GetHubEndpoint())
	return nil
}

func printHubHealthSummary(w io.Writer, s *hubclient.HealthSummaryResponse, endpoint string) {
	_, _ = fmt.Fprintln(w, "==================================================================")
	_, _ = fmt.Fprintln(w, "                     SCION HUB HEALTH & METRICS                   ")
	_, _ = fmt.Fprintln(w, "==================================================================")
	if endpoint != "" {
		_, _ = fmt.Fprintf(w, "Endpoint: %s\n", endpoint)
	}

	_, _ = fmt.Fprintf(w, "Overall Status:      %s\n", s.Status)
	_, _ = fmt.Fprintf(w, "Hub Server Version:  %s (Uptime: %s)\n", s.Hub.Version, s.Hub.Uptime)
	_, _ = fmt.Fprintf(w, "Registered Projects: %d  |  Active Agents: %d  |  Brokers: %d\n\n",
		s.Hub.Projects, s.Hub.ActiveAgents, s.Hub.ConnectedBrokers)

	// Database Subsystem
	_, _ = fmt.Fprintln(w, "--- Database Subsystem -------------------------------------------")
	_, _ = fmt.Fprintf(w, "Status:     %s\n", s.Database.Status)
	_, _ = fmt.Fprintf(w, "Pool Stats: Active=%d / Max=%d  |  Idle=%d  |  Wait Count Total=%d\n\n",
		s.Database.PoolActive, s.Database.PoolMax, s.Database.PoolIdle, s.Database.PoolWaitCountTotal)

	// Runtime Brokers
	_, _ = fmt.Fprintln(w, "--- Runtime Brokers ----------------------------------------------")
	if len(s.Brokers) == 0 {
		_, _ = fmt.Fprintln(w, "No runtime brokers registered.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "BROKER\tSTATUS\tRUNTIME\tAVAILABLE\tAGENTS(OK/TOT)\tLAST HEARTBEAT")
		_, _ = fmt.Fprintln(tw, "------\t------\t-------\t---------\t--------------\t--------------")
		for _, b := range s.Brokers {
			avail := "no"
			if b.RuntimeAvailable {
				avail = "yes"
			}
			hb := "never"
			if !b.LastHeartbeat.IsZero() {
				hb = b.LastHeartbeat.Format("2006-01-02 15:04:05")
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\n",
				truncate(b.Name, 24),
				b.Status,
				b.Runtime,
				avail,
				b.AgentHealthy,
				b.AgentCount,
				hb,
			)
		}
		_ = tw.Flush()
	}
	_, _ = fmt.Fprintln(w)

	// Fleet Agent Health
	_, _ = fmt.Fprintln(w, "--- Fleet Agent Health -------------------------------------------")
	running := s.Agents.ByPhase["running"]
	errors := s.Agents.ByPhase["error"]
	_, _ = fmt.Fprintf(w, "Total Agents: %d  (Running: %d, Errors: %d)\n", s.Agents.Total, running, errors)

	if len(s.Agents.Stalled) > 0 {
		_, _ = fmt.Fprintf(w, "! Stalled Agents (%d): %s\n", len(s.Agents.Stalled), strings.Join(s.Agents.Stalled, ", "))
		_, _ = fmt.Fprintln(w, "  Hint: Run 'scion look <agent>' or 'scion attach <agent>' to inspect terminal state.")
	}
	if len(s.Agents.Crashed) > 0 {
		_, _ = fmt.Fprintf(w, "x Crashed Agents (%d): %s\n", len(s.Agents.Crashed), strings.Join(s.Agents.Crashed, ", "))
		_, _ = fmt.Fprintln(w, "  Hint: Run 'scion logs <agent>' to view termination logs.")
	}
	if len(s.Agents.Errored) > 0 {
		_, _ = fmt.Fprintf(w, "x Errored Agents (%d): %s\n", len(s.Agents.Errored), strings.Join(s.Agents.Errored, ", "))
		_, _ = fmt.Fprintln(w, "  Hint: Run 'scion logs <agent>' or 'scion reset-auth <agent>' to troubleshoot.")
	}

	if s.Dispatch != nil && (s.Dispatch.StuckMessages > 0 || s.Dispatch.Failed1h > 0) {
		_, _ = fmt.Fprintf(w, "! Dispatch Queue Issues: Stuck Messages=%d, Failed (1h)=%d\n",
			s.Dispatch.StuckMessages, s.Dispatch.Failed1h)
	}
	_, _ = fmt.Fprintln(w)
}
