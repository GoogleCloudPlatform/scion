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
	"path/filepath"
	"text/tabwriter"
	"time"

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
	Use:   "health [project-name]",
	Short: "Show project agent health and activity metrics",
	Long: `Show agent lifecycle phases, runtime activity metrics, and health status
for a Scion project.

Queries the Hub to report:
  - Agent counts by phase (running, error, stopped, suspended)
  - Agent counts by activity (working, thinking, blocked, completed, stalled)
  - Detailed agent matrix with template, harness, lifecycle phase, and activity state
  - Actionable troubleshooting hints for blocked, stalled, or errored agents

If no project name is provided, uses the current project context.
Use --all to display health metrics across all projects on the Hub.

Examples:
  # Show health for current project
  scion project health

  # Show health for a specific project
  scion project health okf-app

  # Show health across all projects
  scion project health --all

  # Output as JSON for automated monitoring or agent consumption
  scion project health okf-app --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runProjectHealth,
}

func init() {
	projectHealthCmd.Flags().BoolVar(&projectHealthAll, "all", false, "Report health across all projects")
	projectHealthCmd.Flags().BoolVar(&projectHealthJSON, "json", false, "Output as JSON")
	projectCmd.AddCommand(projectHealthCmd)
}

// ProjectHealthSummary contains aggregated metrics for a project.
type ProjectHealthSummary struct {
	Total     int `json:"total"`
	Running   int `json:"running"`
	Error     int `json:"error"`
	Stopped   int `json:"stopped"`
	Suspended int `json:"suspended"`
	Working   int `json:"working"`
	Thinking  int `json:"thinking"`
	Blocked   int `json:"blocked"`
	Completed int `json:"completed"`
	Stalled   int `json:"stalled"`
}

// ProjectHealthReport contains health details for a project.
type ProjectHealthReport struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	Slug      string               `json:"slug"`
	GitRemote string               `json:"gitRemote,omitempty"`
	Summary   ProjectHealthSummary `json:"summary"`
	Agents    []hubclient.Agent    `json:"agents"`
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
			if p.Name == query || p.Slug == query || p.ID == query {
				targetProjects = append(targetProjects, p)
				break
			}
		}
		if len(targetProjects) == 0 {
			return fmt.Errorf("project '%s' not found on Hub", query)
		}
	} else {
		// Detect current project
		targetID := settings.GetHubProjectID()
		if targetID == "" {
			targetID = settings.ProjectID
		}
		var gitRemote string
		if !isGlobal {
			gitRemote = util.GetGitRemoteDir(filepath.Dir(resolvedPath))
		}

		for _, p := range projectsResp.Projects {
			if targetID != "" && p.ID == targetID {
				targetProjects = append(targetProjects, p)
				break
			}
			if gitRemote != "" && util.NormalizeGitRemote(p.GitRemote) == util.NormalizeGitRemote(gitRemote) {
				targetProjects = append(targetProjects, p)
				break
			}
			if isGlobal && p.Name == "global" {
				targetProjects = append(targetProjects, p)
				break
			}
		}

		if len(targetProjects) == 0 {
			return fmt.Errorf("current project is not linked to the Hub. Specify a project name or run 'scion hub link'.")
		}
	}

	var reports []ProjectHealthReport
	for _, p := range targetProjects {
		agentsResp, err := client.Projects().ListAgents(ctx, p.ID, &hubclient.ListAgentsOptions{
			Page: apiclient.PageOptions{Limit: 200},
		})
		if err != nil {
			statusf("Warning: failed to list agents for project %s: %v\n", p.Name, err)
			continue
		}

		report := ProjectHealthReport{
			ID:        p.ID,
			Name:      p.Name,
			Slug:      p.Slug,
			GitRemote: p.GitRemote,
			Agents:    agentsResp.Agents,
		}

		report.Summary.Total = len(agentsResp.Agents)
		for _, a := range agentsResp.Agents {
			switch a.Phase {
			case "running":
				report.Summary.Running++
			case "error":
				report.Summary.Error++
			case "stopped":
				report.Summary.Stopped++
			case "suspended":
				report.Summary.Suspended++
			}

			switch a.Activity {
			case "working":
				report.Summary.Working++
			case "thinking":
				report.Summary.Thinking++
			case "blocked":
				report.Summary.Blocked++
			case "completed":
				report.Summary.Completed++
			case "stalled":
				report.Summary.Stalled++
			}
		}
		reports = append(reports, report)
	}

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"projects": reports,
		})
	}

	printProjectHealthReports(reports)
	return nil
}

func printProjectHealthReports(reports []ProjectHealthReport) {
	fmt.Println("==================================================================")
	fmt.Println("                     PROJECT HEALTH & AGENT METRICS               ")
	fmt.Println("==================================================================")

	for i, r := range reports {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("Project: %s (slug: %s, id: %s)\n", r.Name, r.Slug, r.ID)
		fmt.Printf("  Summary: Total=%d | Running=%d | Error=%d | Working/Thinking=%d | Blocked=%d | Completed=%d\n\n",
			r.Summary.Total,
			r.Summary.Running,
			r.Summary.Error,
			r.Summary.Working+r.Summary.Thinking,
			r.Summary.Blocked,
			r.Summary.Completed,
		)

		if len(r.Agents) == 0 {
			fmt.Println("  No agents registered in this project.")
			continue
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  AGENT\tTEMPLATE\tHARNESS\tPHASE\tACTIVITY")
		fmt.Fprintln(w, "  -----\t--------\t-------\t-----\t--------")
		for _, a := range r.Agents {
			name := a.Name
			if name == "" {
				name = a.Slug
			}
			tmpl := a.Template
			if tmpl == "" {
				tmpl = "default"
			}
			harness := a.HarnessConfig
			if harness == "" {
				harness = "claude"
			}
			activity := a.Activity
			if activity == "" {
				activity = "-"
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
				truncate(name, 26),
				truncate(tmpl, 14),
				truncate(harness, 10),
				a.Phase,
				activity,
			)
		}
		_ = w.Flush()

		// Troubleshooting hints for degraded states
		if r.Summary.Blocked > 0 || r.Summary.Error > 0 || r.Summary.Stalled > 0 {
			fmt.Println()
			if r.Summary.Blocked > 0 {
				fmt.Printf("  ! %d agent(s) are blocked waiting on permissions or inputs.\n", r.Summary.Blocked)
			}
			if r.Summary.Error > 0 {
				fmt.Printf("  x %d agent(s) are in error phase. Run 'scion logs <agent>' or 'scion reset-auth <agent>'.\n", r.Summary.Error)
			}
			if r.Summary.Stalled > 0 {
				fmt.Printf("  ! %d agent(s) are stalled. Run 'scion look <agent>' to check terminal state.\n", r.Summary.Stalled)
			}
		}
	}
	fmt.Println()
}
