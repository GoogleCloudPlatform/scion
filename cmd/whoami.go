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
	"os"
	"os/exec"

	"github.com/spf13/cobra"
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

		if isJSONOutput() {
			result := buildWhoamiResult(slug, name)
			return outputJSON(result)
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
		result.HubURL = fmt.Sprintf("%s/agents/%s", hubEndpoint, id)
	}

	return result
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
}
