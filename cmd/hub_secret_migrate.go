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
	"log"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/spf13/cobra"
)

var (
	migrateProject     string
	migrateCredentials string
	migrateDryRun      bool
	migrateForce       bool
	migrateHubID       string
)

var hubSecretMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate secrets to GCP Secret Manager",
	Long: `Migrate existing secrets from the Hub database to GCP Secret Manager.

This command reads all secrets from the Hub database, creates corresponding
secrets in GCP Secret Manager, and updates the database entries to reference
the GCP SM secrets instead of storing values locally.

This operation is idempotent - existing GCP SM secrets will be overwritten.

Examples:
  # Dry run to see what would be migrated
  scion hub secret migrate --project=my-project --dry-run

  # Perform the migration
  scion hub secret migrate --project=my-project

  # With explicit credentials
  scion hub secret migrate --project=my-project --credentials=/path/to/creds.json

  # Force re-migration of already-migrated secrets (e.g., after naming scheme change)
  scion hub secret migrate --project=my-project --force`,
	RunE: runSecretMigrate,
}

func init() {
	hubSecretCmd.AddCommand(hubSecretMigrateCmd)

	hubSecretMigrateCmd.Flags().StringVar(&migrateProject, "project", "", "GCP project ID (required)")
	hubSecretMigrateCmd.Flags().StringVar(&migrateCredentials, "credentials", "", "Path to GCP credentials JSON file")
	hubSecretMigrateCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false, "Show what would be migrated without making changes")
	hubSecretMigrateCmd.Flags().BoolVar(&migrateForce, "force", false, "Re-migrate secrets that already have a GCP SM reference")
	hubSecretMigrateCmd.Flags().StringVar(&migrateHubID, "hub-id", "", "Hub instance ID for secret namespacing (defaults to sha256(hostname)[:12])")

	_ = hubSecretMigrateCmd.MarkFlagRequired("project")
}

func runSecretMigrate(cmd *cobra.Command, args []string) error {
	if migrateProject == "" {
		return fmt.Errorf("--project flag is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Load config to find database
	cfg, err := config.LoadGlobalConfig("")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Open database (single Ent-backed store)
	entClient, err := entc.OpenSQLite("file:"+cfg.Database.URL+"?cache=shared", entc.PoolConfig{})
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	db := entadapter.NewCompositeStore(entClient)
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("failed to migrate database: %w", err)
	}

	// Read credentials file if provided
	credentialsJSON := ""
	if migrateCredentials != "" {
		data, err := os.ReadFile(migrateCredentials)
		if err != nil {
			return fmt.Errorf("failed to read credentials file: %w", err)
		}
		credentialsJSON = string(data)
	}

	// Resolve hub ID for secret namespacing
	hubID := migrateHubID
	if hubID == "" {
		hubID = config.DefaultHubID()
	}
	log.Printf("Using hub ID: %s", hubID)

	// Create GCP backend
	gcpBackend, err := secret.NewGCPBackend(ctx, db, secret.GCPBackendConfig{
		ProjectID:       migrateProject,
		CredentialsJSON: credentialsJSON,
	}, hubID)
	if err != nil {
		return fmt.Errorf("failed to create GCP backend: %w", err)
	}

	// Query all secrets without scope filter
	allSecrets, err := db.ListSecrets(ctx, store.SecretFilter{})
	if err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}

	if len(allSecrets) == 0 {
		fmt.Println("No secrets found to migrate.")
		return nil
	}

	fmt.Printf("Found %d secret(s) to migrate to GCP SM (project: %s)\n\n", len(allSecrets), migrateProject)

	migrated := 0
	skipped := 0
	for _, s := range allSecrets {
		// Skip secrets that already have a GCP SM reference (unless --force)
		if s.SecretRef != "" && !migrateForce {
			if migrateDryRun {
				fmt.Printf("  SKIP  %s (scope: %s/%s) - already has ref: %s\n", s.Key, s.Scope, s.ScopeID, s.SecretRef)
			}
			skipped++
			continue
		}

		var value string
		if s.SecretRef != "" && migrateForce {
			// --force: read value from existing GCP SM secret via its secretRef
			ref := s.SecretRef
			// secretRef format: "gcpsm:projects/{project}/secrets/{name}"
			if !strings.HasPrefix(ref, "gcpsm:") {
				log.Printf("  WARN  %s (scope: %s/%s) - unsupported ref format: %s", s.Key, s.Scope, s.ScopeID, ref)
				skipped++
				continue
			}
			smPath := strings.TrimPrefix(ref, "gcpsm:")
			val, err := gcpBackend.AccessSecretValueByRef(ctx, smPath)
			if err != nil {
				log.Printf("  WARN  %s (scope: %s/%s) - failed to read from old ref: %v", s.Key, s.Scope, s.ScopeID, err)
				skipped++
				continue
			}
			value = val
		} else {
			// Normal migration: get value from DB
			var err error
			value, err = db.GetSecretValue(ctx, s.Key, s.Scope, s.ScopeID)
			if err != nil {
				log.Printf("  WARN  %s (scope: %s/%s) - failed to get value: %v", s.Key, s.Scope, s.ScopeID, err)
				skipped++
				continue
			}
		}

		if migrateDryRun {
			action := "WOULD MIGRATE"
			if s.SecretRef != "" {
				action = "WOULD RE-MIGRATE"
			}
			fmt.Printf("  %s  %s (scope: %s/%s, type: %s)\n", action, s.Key, s.Scope, s.ScopeID, s.SecretType)
		} else {
			input := &secret.SetSecretInput{
				Name:        s.Key,
				Value:       value,
				SecretType:  s.SecretType,
				Target:      s.Target,
				Scope:       s.Scope,
				ScopeID:     s.ScopeID,
				Description: s.Description,
				CreatedBy:   s.CreatedBy,
				UpdatedBy:   s.UpdatedBy,
			}
			_, _, err := gcpBackend.Set(ctx, input)
			if err != nil {
				log.Printf("  ERROR  %s (scope: %s/%s) - %v", s.Key, s.Scope, s.ScopeID, err)
				skipped++
				continue
			}
			action := "MIGRATED"
			if s.SecretRef != "" {
				action = "RE-MIGRATED"
			}
			fmt.Printf("  %s  %s (scope: %s/%s, type: %s)\n", action, s.Key, s.Scope, s.ScopeID, s.SecretType)
		}
		migrated++
	}

	status := "complete"
	if migrateDryRun {
		status = "dry run complete"
	}
	fmt.Printf("\nMigration %s: %d migrated, %d skipped\n", status, migrated, skipped)

	return nil
}
