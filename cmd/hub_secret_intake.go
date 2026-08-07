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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	intakeScope       string
	intakeProject     string
	intakeType        string
	intakeTarget      string
	intakeDescription string
	intakeTTL         string
)

// hubSecretIntakeCmd creates a secret intake link
var hubSecretIntakeCmd = &cobra.Command{
	Use:   "intake KEY",
	Short: "Create a secret intake link",
	Long: `Create a short-lived, one-time-use URL that lets someone paste a secret
value into the Hub securely.

The intake link opens a page where the recipient can submit a secret value.
After submission, you will be asked to confirm or reject in your chat channel.

The link expires after 15 minutes by default.

Examples:
  # Create an intake link for a GitHub token
  scion hub secret intake GITHUB_TOKEN --description "GitHub PAT for repo access"

  # Project-scoped secret
  scion hub secret intake API_KEY --project my-app --type environment

  # Custom TTL (max 1 hour)
  scion hub secret intake DB_PASSWORD --ttl 30m --description "Database password"`,
	Args: cobra.ExactArgs(1),
	RunE: runSecretIntake,
}

func init() {
	hubSecretCmd.AddCommand(hubSecretIntakeCmd)

	hubSecretIntakeCmd.Flags().StringVar(&intakeScope, "scope", "", "Scope level: hub, user (default: user)")
	hubSecretIntakeCmd.Flags().StringVar(&intakeProject, "project", "", "Project scope (bare flag infers current project)")
	hubSecretIntakeCmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel
	hubSecretIntakeCmd.Flags().StringVar(&intakeType, "type", "", "Secret type: environment (default), variable, file")
	hubSecretIntakeCmd.Flags().StringVar(&intakeTarget, "target", "", "Projection target (env var name, json key, or file path)")
	hubSecretIntakeCmd.Flags().StringVar(&intakeDescription, "description", "", "Human-readable description shown in the intake page")
	hubSecretIntakeCmd.Flags().StringVar(&intakeTTL, "ttl", "15m", "Link expiry duration (max 1h, e.g. 15m, 30m, 1h)")
}

func runSecretIntake(cmd *cobra.Command, args []string) error {
	key := args[0]

	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	// Resolve scope. Reuse the secret scope resolution logic but adapted
	// for the intake-specific flags.
	scope, scopeID, err := resolveIntakeScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		return err
	}

	// Parse TTL.
	ttl, err := time.ParseDuration(intakeTTL)
	if err != nil {
		return fmt.Errorf("invalid --ttl value %q: %w", intakeTTL, err)
	}
	ttlSeconds := int(ttl.Seconds())
	if ttlSeconds <= 0 {
		return fmt.Errorf("--ttl must be positive")
	}

	req := &hubclient.CreateIntakeRequest{
		Key:         key,
		Scope:       scope,
		ScopeID:     scopeID,
		SecretType:  intakeType,
		Target:      intakeTarget,
		Description: intakeDescription,
		TTLSeconds:  ttlSeconds,
	}

	resp, err := client.SecretIntake().Create(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create intake link: %w", err)
	}

	fmt.Printf("Intake link (expires at %s):\n", resp.ExpiresAt)
	fmt.Printf("  %s\n\n", resp.URL)
	fmt.Println("Send this link to the user. After they paste the value,")
	fmt.Println("you will be asked to confirm in this channel.")

	return nil
}

// resolveIntakeScope determines scope and scopeID from intake-specific flags.
func resolveIntakeScope(cmd *cobra.Command, settings *config.Settings) (scope, scopeID string, err error) {
	scopeSet := cmd.Flags().Changed("scope")
	projectSet := cmd.Flags().Changed("project")

	if scopeSet && projectSet {
		return "", "", fmt.Errorf("cannot specify both --scope and --project")
	}

	if scopeSet {
		switch intakeScope {
		case "hub":
			return "hub", "", nil
		case "user", "":
			return "user", "", nil
		default:
			return "", "", fmt.Errorf("invalid --scope value %q: must be 'hub' or 'user'", intakeScope)
		}
	}

	if projectSet {
		scope = "project"
		projectVal := intakeProject
		if projectVal == scopeInferSentinel {
			projectVal = ""
		}
		if projectVal != "" {
			scopeID = projectVal
		} else {
			if settings.Hub != nil && settings.Hub.ProjectID != "" {
				scopeID = settings.Hub.ProjectID
			} else if settings.ProjectID != "" {
				scopeID = settings.ProjectID
			} else {
				return "", "", fmt.Errorf("cannot infer project ID: not linked with Hub. Use 'scion hub link' first or provide explicit project ID")
			}
		}
		return scope, scopeID, nil
	}

	return "user", "", nil
}
