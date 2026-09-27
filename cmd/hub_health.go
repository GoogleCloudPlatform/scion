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
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
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
  - Runtime broker status, engine readiness, and last heartbeat
  - Fleet agent tallies by lifecycle phase
  - Callouts for stalled, crashed, or errored agents

The detailed summary requires the Hub admin role (hub.health.read). Without
it, the command falls back to the public /healthz status.

JSON output (--json) always has a top-level "detailed" boolean:
  - "detailed": true  -> "summary" holds the full admin health summary
  - "detailed": false -> "basic" holds the public /healthz response and
                         "note" explains why the summary is unavailable

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

// hubHealthUnhealthyListCap mirrors the server-side cap on each unhealthy
// agent name list (AggregateAgentHealth). A list of exactly this length may
// have been truncated by the Hub.
const hubHealthUnhealthyListCap = 100

// hubHealthMaxNamesShown bounds how many agent names are printed per list in
// the human-readable output.
const hubHealthMaxNamesShown = 10

const hubHealthBasicNote = "Detailed subsystem health summary requires the Hub admin role; showing basic health."

// HubHealthOutput is the JSON shape of 'scion hub health --json'. Detailed
// discriminates between the two possible payloads so consumers never have to
// guess which shape they received.
type HubHealthOutput struct {
	Detailed bool                             `json:"detailed"`
	Summary  *hubclient.HealthSummaryResponse `json:"summary,omitempty"`
	Basic    *HubHealthBasic                  `json:"basic,omitempty"`
	Note     string                           `json:"note,omitempty"`
}

// HubHealthBasic is the subset of the public /healthz response shown when the
// caller lacks the admin role.
type HubHealthBasic struct {
	Status       string            `json:"status"`
	Version      string            `json:"version"`
	ScionVersion string            `json:"scionVersion"`
	Uptime       string            `json:"uptime"`
	Checks       map[string]string `json:"checks,omitempty"`
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

	return reportHubHealth(ctx, client, os.Stdout, GetHubEndpoint(settings), isJSONOutput())
}

// reportHubHealth fetches the admin health summary and renders it to w. If the
// caller is forbidden (403, i.e. authenticated but not an admin) it falls back
// to the public /healthz response. Any other error is returned unchanged.
func reportHubHealth(ctx context.Context, client hubclient.Client, w io.Writer, endpoint string, asJSON bool) error {
	summary, err := client.HealthSummary(ctx)
	if err != nil {
		if !apiclient.IsForbiddenError(err) {
			return fmt.Errorf("failed to fetch Hub health summary: %w", hubclient.HintProxyError(err))
		}
		basic, basicErr := client.Health(ctx)
		if basicErr != nil {
			return fmt.Errorf("hub health summary requires admin role, and basic health check failed: %w",
				hubclient.HintProxyError(basicErr))
		}
		out := HubHealthOutput{
			Detailed: false,
			Basic: &HubHealthBasic{
				Status:       basic.Status,
				Version:      basic.Version,
				ScionVersion: basic.ScionVersion,
				Uptime:       basic.Uptime,
				Checks:       basic.Checks,
			},
			Note: hubHealthBasicNote,
		}
		if asJSON {
			return writeJSON(w, out)
		}
		printHubHealthBasic(w, out.Basic)
		return nil
	}

	if asJSON {
		return writeJSON(w, HubHealthOutput{Detailed: true, Summary: summary})
	}
	printHubHealthSummary(w, summary, endpoint)
	return nil
}

func writeJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printHubHealthBasic(w io.Writer, b *HubHealthBasic) {
	_, _ = fmt.Fprintf(w, "Hub Status: %s\n", b.Status)
	_, _ = fmt.Fprintf(w, "Version:    %s (scion %s)\n", b.Version, b.ScionVersion)
	_, _ = fmt.Fprintf(w, "Uptime:     %s\n", b.Uptime)
	_, _ = fmt.Fprintln(w, "\nNote: Full subsystem telemetry requires the Hub admin role.")
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
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\n",
				truncate(b.Name, 24),
				b.Status,
				b.Runtime,
				avail,
				b.AgentHealthy,
				b.AgentCount,
				formatRelativeTime(b.LastHeartbeat),
			)
		}
		_ = tw.Flush()
	}
	_, _ = fmt.Fprintln(w)

	// Fleet Agent Health. The phase tally is a complete bucketing (every phase,
	// plus Other for unknown ones), so it always sums to Total.
	_, _ = fmt.Fprintln(w, "--- Fleet Agent Health -------------------------------------------")
	_, _ = fmt.Fprintf(w, "Phases: %s\n", bucketPhases(s.Agents.ByPhase).String())

	// Name lists are fleet-wide, but agent commands resolve names within the
	// current project, hence the --project note in each hint.
	if len(s.Agents.Stalled) > 0 {
		_, _ = fmt.Fprintf(w, "! Stalled Agents %s\n", formatAgentNameList(s.Agents.Stalled))
		_, _ = fmt.Fprintln(w, "  Hint: Run 'scion look <agent>' or 'scion attach <agent>' to inspect terminal state (add --project <project> if outside the agent's project).")
	}
	if len(s.Agents.Crashed) > 0 {
		_, _ = fmt.Fprintf(w, "x Crashed Agents %s\n", formatAgentNameList(s.Agents.Crashed))
		_, _ = fmt.Fprintln(w, "  Hint: Run 'scion logs <agent>' to view termination logs (add --project <project> if outside the agent's project).")
	}
	if len(s.Agents.Errored) > 0 {
		_, _ = fmt.Fprintf(w, "x Errored Agents %s\n", formatAgentNameList(s.Agents.Errored))
		_, _ = fmt.Fprintln(w, "  Hint: Run 'scion logs <agent>' to see why provisioning or start failed (add --project <project> if outside the agent's project).")
	}

	if s.Dispatch != nil && (s.Dispatch.StuckMessages > 0 || s.Dispatch.Failed1h > 0) {
		_, _ = fmt.Fprintf(w, "! Dispatch Queue Issues: Stuck Messages=%d, Failed (1h)=%d\n",
			s.Dispatch.StuckMessages, s.Dispatch.Failed1h)
	}
	_, _ = fmt.Fprintln(w)
}

// formatAgentNameList renders "(N): a, b, c" for an unhealthy-agent list.
// The count is shown as "100+" when the list hit the server-side cap, and at
// most hubHealthMaxNamesShown names are printed, followed by "… and N more".
func formatAgentNameList(names []string) string {
	count := fmt.Sprintf("%d", len(names))
	if len(names) >= hubHealthUnhealthyListCap {
		count = fmt.Sprintf("%d+", hubHealthUnhealthyListCap)
	}
	shown := names
	suffix := ""
	if len(names) > hubHealthMaxNamesShown {
		shown = names[:hubHealthMaxNamesShown]
		more := fmt.Sprintf("%d", len(names)-hubHealthMaxNamesShown)
		if len(names) >= hubHealthUnhealthyListCap {
			more += "+"
		}
		suffix = fmt.Sprintf(", … and %s more", more)
	}
	return fmt.Sprintf("(%s): %s%s", count, strings.Join(shown, ", "), suffix)
}
