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
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	sciontoolhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// WhoamiResult is the JSON output shape for `scion whoami --format json`.
type WhoamiResult struct {
	// --- Tier 1: env-var fields (always populated, zero latency) ---
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	ID          string `json:"id"`
	Project     string `json:"project,omitempty"`
	ProjectID   string `json:"projectId,omitempty"`
	Template    string `json:"template,omitempty"`
	Harness     string `json:"harness,omitempty"`
	Model       string `json:"model,omitempty"`
	Creator     string `json:"creator,omitempty"`
	BrokerName  string `json:"brokerName,omitempty"`
	BrokerID    string `json:"brokerId,omitempty"`
	CLIMode     string `json:"cliMode,omitempty"`
	HubEndpoint string `json:"hubEndpoint,omitempty"`
	HubURL      string `json:"hubUrl,omitempty"` // constructed: {hubEndpoint}/agents/{id}

	// --- Tier 2: Hub API fields (only with --full, omitted otherwise) ---
	Phase       string            `json:"phase,omitempty"`
	Activity    string            `json:"activity,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Ancestry    []string          `json:"ancestry,omitempty"`
	TaskSummary string            `json:"taskSummary,omitempty"`
}

// whoamiFull controls whether the --full flag was set.
var whoamiFull bool

// newHubClient is the default Hub client factory. Overridden in tests.
var newHubClient = func() *sciontoolhub.Client {
	return sciontoolhub.NewClient()
}

var whoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Print the current agent's identity",
	Long: `Print the current agent's identity when running inside an agent container.
Returns the agent slug by default, or full identity details with --format json.

When run outside an agent container, falls back to the system whoami command.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		slug := os.Getenv("SCION_AGENT_SLUG")
		name := os.Getenv("SCION_AGENT_NAME")

		if slug == "" && name == "" {
			return runSystemWhoami()
		}

		if slug == "" {
			slug = name
		}

		result := buildWhoamiResult(slug, name)

		// Enrich with Hub data when --full is requested.
		if whoamiFull {
			enrichFromHub(cmd, &result)
		}

		if isJSONOutput() {
			return outputJSON(result)
		}

		if whoamiFull {
			printFullPlainText(result)
			return nil
		}

		fmt.Println(slug)
		return nil
	},
}

// buildWhoamiResult populates a WhoamiResult from the env var allowlist.
func buildWhoamiResult(slug, name string) WhoamiResult {
	id := os.Getenv("SCION_AGENT_ID")
	hubEndpoint := os.Getenv("SCION_HUB_ENDPOINT")

	result := WhoamiResult{
		Slug:        slug,
		Name:        name,
		ID:          id,
		Project:     os.Getenv("SCION_PROJECT"),
		ProjectID:   os.Getenv("SCION_PROJECT_ID"),
		Template:    os.Getenv("SCION_TEMPLATE_NAME"),
		Harness:     os.Getenv("SCION_HARNESS"),
		Model:       os.Getenv("SCION_MODEL"),
		Creator:     os.Getenv("SCION_CREATOR"),
		BrokerName:  os.Getenv("SCION_BROKER_NAME"),
		BrokerID:    os.Getenv("SCION_BROKER_ID"),
		CLIMode:     os.Getenv("SCION_CLI_MODE"),
		HubEndpoint: hubEndpoint,
	}

	// Construct hubUrl from hubEndpoint + id (no API call needed).
	if hubEndpoint != "" && id != "" {
		result.HubURL = fmt.Sprintf("%s/agents/%s", strings.TrimSuffix(hubEndpoint, "/"), id)
	}

	return result
}

// enrichFromHub attempts to populate Tier 2 fields from the Hub API.
// On failure, it emits a stderr warning and returns Tier 1 fields only.
func enrichFromHub(cmd *cobra.Command, result *WhoamiResult) {
	client := newHubClient()
	if client == nil || !client.IsConfigured() {
		fmt.Fprintln(os.Stderr, "Warning: Hub not available; --full fields omitted")
		return
	}

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()

	self, err := client.GetSelf(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Hub query failed: %v; --full fields omitted\n", err)
		return
	}

	result.Phase = self.Phase
	result.Activity = self.Activity
	result.Labels = self.Labels
	result.Annotations = self.Annotations
	result.Ancestry = self.Ancestry
	result.TaskSummary = self.TaskSummary
}

// printFullPlainText prints a human-readable multi-line summary of all available fields.
func printFullPlainText(r WhoamiResult) {
	if r.Name != "" {
		fmt.Printf("Agent:    %s (%s)\n", r.Slug, r.Name)
	} else {
		fmt.Printf("Agent:    %s\n", r.Slug)
	}
	if r.ID != "" {
		fmt.Printf("ID:       %s\n", r.ID)
	}
	if r.Project != "" {
		fmt.Printf("Project:  %s\n", r.Project)
	}
	if r.Template != "" {
		fmt.Printf("Template: %s\n", r.Template)
	}
	if r.Harness != "" {
		fmt.Printf("Harness:  %s\n", r.Harness)
	}
	if r.Model != "" {
		fmt.Printf("Model:    %s\n", r.Model)
	}
	if r.Creator != "" {
		fmt.Printf("Creator:  %s\n", r.Creator)
	}
	if r.BrokerName != "" {
		fmt.Printf("Broker:   %s\n", r.BrokerName)
	}
	if r.Phase != "" {
		fmt.Printf("Phase:    %s\n", r.Phase)
	}
	if r.Activity != "" {
		fmt.Printf("Activity: %s\n", r.Activity)
	}
	if r.HubURL != "" {
		fmt.Printf("Hub:      %s\n", r.HubURL)
	}
}

func runSystemWhoami() error {
	path, err := exec.LookPath("whoami")
	if err != nil {
		return fmt.Errorf("not running as a scion agent and system whoami not found")
	}
	sysCmd := exec.Command(path)
	sysCmd.Stdout = os.Stdout
	sysCmd.Stderr = os.Stderr
	return sysCmd.Run()
}

func init() {
	rootCmd.AddCommand(whoamiCmd)
	whoamiCmd.Flags().BoolVar(&whoamiFull, "full", false,
		"Include enriched fields from the Hub (phase, activity, labels, ancestry)")
}
