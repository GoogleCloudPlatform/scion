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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
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
	MessageMode string            `json:"messageMode,omitempty"` // effective message mode (none, lineage, branch, project, hub)

	// Scopes are the scopes of the agent's current hub token, read from
	// the token itself (only with --full). They show whether, for example,
	// the artifact scopes were issued (ptone/scion#4569).
	Scopes []string `json:"scopes,omitempty"`

	// Experiments are the hub experiments that are on now, read live from
	// GET /api/v1/experiments (only with --full; absent when the hub could
	// not be asked, an empty list when none is on). That endpoint reports
	// the experiments with a web layer, which include server-checked ones
	// such as hub.artifacts. SCION_HUB_EXPERIMENTS, by contrast, lists only
	// the experiments dispatched to the agent, fixed at start
	// (ptone/scion#4573).
	Experiments *[]string `json:"experiments,omitempty"`
}

// whoamiFull controls whether the --full flag was set.
var whoamiFull bool

// whoamiToken returns the agent's current hub token: the canonical token
// file, or SCION_AUTH_TOKEN before the file exists. Overridden in tests.
var whoamiToken = func() string {
	if tok := sciontoolhub.ReadTokenFile(); tok != "" {
		return tok
	}
	return os.Getenv(sciontoolhub.EnvHubToken)
}

// tokenScopes returns the scopes claim of an agent JWT, decoded without
// verifying the signature: it describes the caller's own credential and
// grants nothing (the hub verifies the token on every request). It returns
// nil when the token is not a JWT or carries no scopes.
func tokenScopes(token string) []string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims.Scopes
}

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

		// Enrich with the token's scopes and Hub data when --full is
		// requested.
		if whoamiFull {
			result.Scopes = tokenScopes(whoamiToken())
			enrichFromHub(cmd, &result)
		}

		if rootOptions(cmd).JSONOutput() {
			return writeJSONTo(cmd.OutOrStdout(), result)
		}

		if whoamiFull {
			return printFullPlainText(cmd.OutOrStdout(), result)
		}

		_, err := fmt.Fprintln(cmd.OutOrStdout(), slug)
		return err
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
		ProjectID:   projectkeys.ProjectIDFromEnv(os.Getenv),
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
	result.MessageMode = self.MessageMode

	enabled, err := client.GetExperiments(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Hub experiments query failed: %v; experiments omitted\n", err)
		return
	}
	result.Experiments = &enabled
}

// printFullPlainText writes a human-readable multi-line summary of all
// available fields to w, with the values aligned in one column.
func printFullPlainText(w io.Writer, r WhoamiResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 1, ' ', 0)
	if r.Name != "" {
		_, _ = fmt.Fprintf(tw, "Agent:\t%s (%s)\n", r.Slug, r.Name)
	} else {
		_, _ = fmt.Fprintf(tw, "Agent:\t%s\n", r.Slug)
	}
	if r.ID != "" {
		_, _ = fmt.Fprintf(tw, "ID:\t%s\n", r.ID)
	}
	if r.Project != "" {
		_, _ = fmt.Fprintf(tw, "Project:\t%s\n", r.Project)
	}
	if r.Template != "" {
		_, _ = fmt.Fprintf(tw, "Template:\t%s\n", r.Template)
	}
	if r.Harness != "" {
		_, _ = fmt.Fprintf(tw, "Harness:\t%s\n", r.Harness)
	}
	if r.Model != "" {
		_, _ = fmt.Fprintf(tw, "Model:\t%s\n", r.Model)
	}
	if r.Creator != "" {
		_, _ = fmt.Fprintf(tw, "Creator:\t%s\n", r.Creator)
	}
	if r.BrokerName != "" {
		_, _ = fmt.Fprintf(tw, "Broker:\t%s\n", r.BrokerName)
	}
	if r.Phase != "" {
		_, _ = fmt.Fprintf(tw, "Phase:\t%s\n", r.Phase)
	}
	if r.Activity != "" {
		_, _ = fmt.Fprintf(tw, "Activity:\t%s\n", r.Activity)
	}
	if r.MessageMode != "" {
		_, _ = fmt.Fprintf(tw, "Msg Mode:\t%s\n", r.MessageMode)
	}
	if r.HubURL != "" {
		_, _ = fmt.Fprintf(tw, "Hub:\t%s\n", r.HubURL)
	}
	if len(r.Scopes) > 0 {
		_, _ = fmt.Fprintf(tw, "Scopes:\t%s\n", strings.Join(r.Scopes, ", "))
	}
	if r.Experiments != nil {
		list := strings.Join(*r.Experiments, ", ")
		if list == "" {
			list = "(none)"
		}
		_, _ = fmt.Fprintf(tw, "Experiments:\t%s\n", list)
	}
	return tw.Flush()
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
		"Include the token's scopes and enriched fields from the Hub (phase, activity, message mode, labels, ancestry, enabled experiments)")
}
