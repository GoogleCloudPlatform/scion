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
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var (
	projectHealthAll  bool
	projectHealthJSON bool
)

var projectHealthCmd = &cobra.Command{
	Use:     "status [project-name]",
	Aliases: []string{"health"},
	Short:   "Show project agent status and activity metrics",
	Long: `Show agent lifecycle phases, runtime activity metrics, and status summary
for a Scion project.

Queries the Hub to report:
  - Agent counts across all lifecycle phases (created, provisioning, cloning, starting, running, stopping, stopped, error, suspended)
  - Agent counts across all activity states (working, thinking, executing, waiting_for_input, blocked, completed, stalled, limits_exceeded, offline, crashed)
  - Detailed agent matrix with template, harness, lifecycle phase, and activity state
  - Actionable troubleshooting hints for blocked, stalled, or errored agents

If no project name is provided, uses the current project context.
Use --all to display status metrics across all projects on the Hub.

Examples:
  # Show status for current project
  scion project status

  # Show status for a specific project
  scion project status okf-app

  # Show status across all projects
  scion project status --all

  # Output as JSON for automated monitoring or agent consumption
  scion project status okf-app --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runProjectHealth,
}

func init() {
	projectHealthCmd.Flags().BoolVar(&projectHealthAll, "all", false, "Report status across all projects")
	projectHealthCmd.Flags().BoolVar(&projectHealthJSON, "json", false, "Output as JSON")
	projectCmd.AddCommand(projectHealthCmd)
}

// ProjectHealthSummary contains aggregated phase and activity metrics for a project.
type ProjectHealthSummary struct {
	Total           int `json:"total"`
	Created         int `json:"created"`
	Provisioning    int `json:"provisioning"`
	Cloning         int `json:"cloning"`
	Starting        int `json:"starting"`
	Running         int `json:"running"`
	Suspended       int `json:"suspended"`
	Stopping        int `json:"stopping"`
	Stopped         int `json:"stopped"`
	Error           int `json:"error"`
	OtherPhase      int `json:"otherPhase,omitempty"`
	Working         int `json:"working"`
	Thinking        int `json:"thinking"`
	Executing       int `json:"executing"`
	WaitingForInput int `json:"waitingForInput"`
	Blocked         int `json:"blocked"`
	Completed       int `json:"completed"`
	LimitsExceeded  int `json:"limitsExceeded"`
	Stalled         int `json:"stalled"`
	Offline         int `json:"offline"`
	Crashed         int `json:"crashed"`
	OtherActivity   int `json:"otherActivity,omitempty"`
}

// ProjectHealthReport contains status details for a project.
type ProjectHealthReport struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	Slug      string               `json:"slug"`
	GitRemote string               `json:"gitRemote,omitempty"`
	Summary   ProjectHealthSummary `json:"summary"`
	Agents    []hubclient.Agent    `json:"agents"`
}

// ProjectStatusResponse is the typed JSON output wrapper for project status reports.
type ProjectStatusResponse struct {
	Projects []ProjectHealthReport `json:"projects"`
}

func runProjectHealth(cmd *cobra.Command, args []string) error {
	if projectHealthJSON {
		outputFormat = "json"
	}

	gp := projectPath
	if gp == "" && globalMode {
		gp = "global"
	}

	resolvedPath, isGlobal, err := config.ResolveProjectPath(gp)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	projectsResp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{})
	if err != nil {
		return fmt.Errorf("failed to list projects from Hub: %w", hubclient.HintProxyError(err))
	}

	var targetProjects []hubclient.Project
	if projectHealthAll {
		targetProjects = projectsResp.Projects
	} else if len(args) > 0 {
		query := args[0]
		for _, p := range projectsResp.Projects {
			if strings.EqualFold(p.Name, query) || strings.EqualFold(p.Slug, query) || p.ID == query {
				targetProjects = append(targetProjects, p)
				break
			}
		}
		if len(targetProjects) == 0 {
			return fmt.Errorf("project '%s' not found on Hub", query)
		}
	} else {
		// Detect current project in prioritized order: exact ID -> gitRemote -> global
		targetID := settings.GetHubProjectID()
		if targetID == "" {
			targetID = settings.ProjectID
		}
		var gitRemote string
		if !isGlobal {
			gitRemote = util.GetGitRemoteDir(filepath.Dir(resolvedPath))
		}

		var matchedProject *hubclient.Project
		if targetID != "" {
			for i := range projectsResp.Projects {
				p := &projectsResp.Projects[i]
				if p.ID == targetID {
					matchedProject = p
					break
				}
			}
		}
		if matchedProject == nil && gitRemote != "" {
			normalizedRemote := util.NormalizeGitRemote(gitRemote)
			for i := range projectsResp.Projects {
				p := &projectsResp.Projects[i]
				if util.NormalizeGitRemote(p.GitRemote) == normalizedRemote {
					matchedProject = p
					break
				}
			}
		}
		if matchedProject == nil && isGlobal {
			for i := range projectsResp.Projects {
				p := &projectsResp.Projects[i]
				if strings.EqualFold(p.Name, "global") {
					matchedProject = p
					break
				}
			}
		}
		if matchedProject != nil {
			targetProjects = append(targetProjects, *matchedProject)
		}

		if len(targetProjects) == 0 {
			return fmt.Errorf("current project is not linked to the Hub; specify a project name or run 'scion hub link'")
		}
	}

	var reports []ProjectHealthReport
	for _, p := range targetProjects {
		var allAgents []hubclient.Agent
		cursor := ""
		fetchErr := false
		for {
			agentsResp, err := client.Projects().ListAgents(ctx, p.ID, &hubclient.ListAgentsOptions{
				Page: apiclient.PageOptions{Limit: 200, Cursor: cursor},
			})
			if err != nil {
				statusf("Warning: failed to list agents for project %s: %v\n", p.Name, err)
				fetchErr = true
				break
			}
			allAgents = append(allAgents, agentsResp.Agents...)
			if agentsResp.Page.NextCursor == "" {
				break
			}
			cursor = agentsResp.Page.NextCursor
		}
		if fetchErr && len(allAgents) == 0 {
			continue
		}

		for i := range allAgents {
			allAgents[i].Template = config.FriendlyTemplateName(allAgents[i].Template)
		}

		report := ProjectHealthReport{
			ID:        p.ID,
			Name:      p.Name,
			Slug:      p.Slug,
			GitRemote: p.GitRemote,
			Agents:    allAgents,
		}

		report.Summary.Total = len(allAgents)
		for _, a := range allAgents {
			switch state.Phase(a.Phase) {
			case state.PhaseCreated:
				report.Summary.Created++
			case state.PhaseProvisioning:
				report.Summary.Provisioning++
			case state.PhaseCloning:
				report.Summary.Cloning++
			case state.PhaseStarting:
				report.Summary.Starting++
			case state.PhaseRunning:
				report.Summary.Running++
			case state.PhaseSuspended:
				report.Summary.Suspended++
			case state.PhaseStopping:
				report.Summary.Stopping++
			case state.PhaseStopped:
				report.Summary.Stopped++
			case state.PhaseError:
				report.Summary.Error++
			default:
				report.Summary.OtherPhase++
			}

			switch state.Activity(a.Activity) {
			case "":
				// No activity set (e.g., when agent is not running)
			case state.ActivityWorking:
				report.Summary.Working++
			case state.ActivityThinking:
				report.Summary.Thinking++
			case state.ActivityExecuting:
				report.Summary.Executing++
			case state.ActivityWaitingForInput:
				report.Summary.WaitingForInput++
			case state.ActivityBlocked:
				report.Summary.Blocked++
			case state.ActivityCompleted:
				report.Summary.Completed++
			case state.ActivityLimitsExceeded:
				report.Summary.LimitsExceeded++
			case state.ActivityStalled:
				report.Summary.Stalled++
			case state.ActivityOffline:
				report.Summary.Offline++
			case state.ActivityCrashed:
				report.Summary.Crashed++
			default:
				report.Summary.OtherActivity++
			}
		}
		reports = append(reports, report)
	}

	if isJSONOutput() {
		return outputJSON(ProjectStatusResponse{
			Projects: reports,
		})
	}

	printProjectHealthReports(os.Stdout, reports)
	return nil
}

func formatPhaseSummary(s ProjectHealthSummary) string {
	parts := []string{
		fmt.Sprintf("Total=%d", s.Total),
		fmt.Sprintf("Running=%d", s.Running),
		fmt.Sprintf("Error=%d", s.Error),
		fmt.Sprintf("Stopped=%d", s.Stopped),
	}
	extra := []struct {
		label string
		val   int
	}{
		{"Created", s.Created},
		{"Provisioning", s.Provisioning},
		{"Cloning", s.Cloning},
		{"Starting", s.Starting},
		{"Suspended", s.Suspended},
		{"Stopping", s.Stopping},
		{"Other", s.OtherPhase},
	}
	for _, b := range extra {
		if b.val > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", b.label, b.val))
		}
	}
	return strings.Join(parts, " | ")
}

func formatActivitySummary(s ProjectHealthSummary) string {
	parts := []string{
		fmt.Sprintf("Working=%d", s.Working),
		fmt.Sprintf("Thinking=%d", s.Thinking),
		fmt.Sprintf("Blocked=%d", s.Blocked),
		fmt.Sprintf("Completed=%d", s.Completed),
	}
	extra := []struct {
		label string
		val   int
	}{
		{"Executing", s.Executing},
		{"WaitingForInput", s.WaitingForInput},
		{"Stalled", s.Stalled},
		{"LimitsExceeded", s.LimitsExceeded},
		{"Offline", s.Offline},
		{"Crashed", s.Crashed},
		{"Other", s.OtherActivity},
	}
	for _, b := range extra {
		if b.val > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", b.label, b.val))
		}
	}
	return strings.Join(parts, " | ")
}

func printProjectHealthReports(w io.Writer, reports []ProjectHealthReport) {
	_, _ = fmt.Fprintln(w, "==================================================================")
	_, _ = fmt.Fprintln(w, "                     PROJECT STATUS & AGENT METRICS               ")
	_, _ = fmt.Fprintln(w, "==================================================================")

	for i, r := range reports {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		_, _ = fmt.Fprintf(w, "Project: %s (slug: %s, id: %s)\n", r.Name, r.Slug, r.ID)
		_, _ = fmt.Fprintf(w, "  Phases:   %s\n", formatPhaseSummary(r.Summary))
		_, _ = fmt.Fprintf(w, "  Activity: %s\n\n", formatActivitySummary(r.Summary))

		if len(r.Agents) == 0 {
			_, _ = fmt.Fprintln(w, "  No agents registered in this project.")
			continue
		}

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  AGENT\tTEMPLATE\tHARNESS\tPHASE\tACTIVITY")
		_, _ = fmt.Fprintln(tw, "  -----\t--------\t-------\t-----\t--------")
		for _, a := range r.Agents {
			name := a.Name
			if name == "" {
				name = a.Slug
			}
			tmpl := config.FriendlyTemplateName(a.Template)
			if tmpl == "" {
				tmpl = "default"
			}
			harness := a.HarnessConfig
			if harness == "" {
				harness = "-"
			}
			activity := a.Activity
			if activity == "" {
				activity = "-"
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
				truncate(name, 26),
				truncate(tmpl, 14),
				truncate(harness, 10),
				a.Phase,
				activity,
			)
		}
		_ = tw.Flush()

		// Troubleshooting hints for degraded states
		if r.Summary.Blocked > 0 || r.Summary.Error > 0 || r.Summary.Stalled > 0 {
			_, _ = fmt.Fprintln(w)
			if r.Summary.Blocked > 0 {
				_, _ = fmt.Fprintf(w, "  ! %d agent(s) are blocked — run 'scion look <agent>' to see the block reason.\n", r.Summary.Blocked)
			}
			if r.Summary.Error > 0 {
				_, _ = fmt.Fprintf(w, "  x %d agent(s) are in error phase. Run 'scion logs <agent>' or 'scion reset-auth <agent>'.\n", r.Summary.Error)
			}
			if r.Summary.Stalled > 0 {
				_, _ = fmt.Fprintf(w, "  ! %d agent(s) are stalled. Run 'scion look <agent>' to check terminal state.\n", r.Summary.Stalled)
			}
		}
	}
	_, _ = fmt.Fprintln(w)
}
