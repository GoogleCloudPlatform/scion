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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/spf13/cobra"
)

// knownHubScopeSecretKeys lists hub-scope secret keys that must be considered
// for name migration even if their Hub DB record is missing (e.g. a database
// reset that left the value only in GCP SM). This is a short, explicit list —
// not a GCP SM listing call, per the ptone/scion#2152 decision to enumerate
// legacy secrets from the Hub DB and known hub-scope key names only, never
// from Secret Manager itself.
var knownHubScopeSecretKeys = []string{
	hub.SecretKeyAgentSigningKey,
	hub.SecretKeyUserSigningKey,
}

var (
	migrateNamesProject      string
	migrateNamesCredentials  string
	migrateNamesDryRun       bool
	migrateNamesDeleteLegacy bool
	migrateNamesHubID        string
)

// hubSecretMigrateNamesCmd renames GCP Secret Manager secrets from the legacy
// (pre hub-prefix) naming scheme to the hub-prefixed scheme introduced by
// ptone/scion#2152. It is distinct from `scion hub secret migrate` (the
// DB->GCP-SM value migration): that command's semantics are unchanged, and
// this command only ever touches secrets that already live in GCP SM under
// the legacy name.
var hubSecretMigrateNamesCmd = &cobra.Command{
	Use:   "migrate-names",
	Short: "Migrate GCP Secret Manager secret names to the hub-prefixed naming scheme",
	Long: `Migrate GCP Secret Manager secret names from the legacy (pre hub-prefix)
naming scheme to the hub-prefixed scheme (ptone/scion#2152).

The hub-prefixed scheme lets a least-privilege IAM grant scope a hub's own
secrets with a conditioned role binding:

  resource.name.startsWith("projects/<PROJECT_NUMBER>/secrets/scion-<h12>-")

(note: PROJECT_NUMBER, not project ID)

For each secret it finds under its legacy name, this command:
  1. Copies the latest version's value (and GCP SM labels) to the new,
     hub-prefixed name.
  2. Updates the secret's Hub DB record to reference the new name.
  3. Leaves the legacy secret in place, unless --delete-legacy is passed.

It is idempotent: a secret already migrated is skipped, so a partially
completed or interrupted run can simply be re-run. Legacy secrets are only
deleted as an explicit, separate step (--delete-legacy) after this command has
verified the new copy is readable and matches.

Examples:
  # Show what would be migrated without making changes
  scion hub secret migrate-names --gcp-project=my-project --dry-run

  # Migrate names, keeping legacy secrets in place
  scion hub secret migrate-names --gcp-project=my-project

  # After confirming the migration, remove the now-unused legacy secrets
  scion hub secret migrate-names --gcp-project=my-project --delete-legacy`,
	PreRunE: checkGCPProjectFlag,
	RunE:    runSecretMigrateNames,
}

func init() {
	hubSecretCmd.AddCommand(hubSecretMigrateNamesCmd)

	hubSecretMigrateNamesCmd.Flags().StringVar(&migrateNamesProject, "gcp-project", "", "GCP project ID (required)")
	hubSecretMigrateNamesCmd.Flags().StringVar(&migrateNamesCredentials, "credentials", "", "Path to GCP credentials JSON file")
	hubSecretMigrateNamesCmd.Flags().BoolVar(&migrateNamesDryRun, "dry-run", false, "Print the migration plan without making changes")
	hubSecretMigrateNamesCmd.Flags().BoolVar(&migrateNamesDeleteLegacy, "delete-legacy", false, "Delete each legacy secret after verifying its hub-prefixed copy (run a plain migrate-names first)")
	hubSecretMigrateNamesCmd.Flags().StringVar(&migrateNamesHubID, "hub-id", "", "Hub instance ID for secret namespacing (defaults to the resolved server hub ID)")

	_ = hubSecretMigrateNamesCmd.MarkFlagRequired("gcp-project")
}

func runSecretMigrateNames(cmd *cobra.Command, args []string) error {
	if migrateNamesProject == "" {
		return fmt.Errorf("--gcp-project flag is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, err := config.LoadGlobalConfig("")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	entClient, err := entc.OpenSQLite("file:"+cfg.Database.URL+"?cache=shared", entc.PoolConfig{})
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	db := entadapter.NewCompositeStore(entClient)
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("failed to migrate database: %w", err)
	}

	credentialsJSON := ""
	if migrateNamesCredentials != "" {
		data, err := os.ReadFile(migrateNamesCredentials)
		if err != nil {
			return fmt.Errorf("failed to read credentials file: %w", err)
		}
		credentialsJSON = string(data)
	}

	hubID := migrateNamesHubID
	if hubID == "" {
		hubID = config.ResolveHubIDFromEnv()
	}
	fmt.Printf("Using hub ID: %s\n", hubID)

	gcpBackend, err := secret.NewGCPBackend(ctx, db, secret.GCPBackendConfig{
		ProjectID:       migrateNamesProject,
		CredentialsJSON: credentialsJSON,
	}, hubID)
	if err != nil {
		return fmt.Errorf("failed to create GCP backend: %w", err)
	}

	return runMigrateNames(ctx, gcpBackend, db, hubID, migrateNamesDryRun, migrateNamesDeleteLegacy, os.Stdout)
}

// migrateNamesCandidate identifies one secret identity to check for name
// migration.
type migrateNamesCandidate struct {
	name, scope, scopeID string
}

// runMigrateNames drives the migrate-names plan/execute loop. It is factored
// out of runSecretMigrateNames so it can be exercised in tests against a
// GCPBackend built with a fake SMClient and an in-memory DB, without a real
// GCP project or Hub deployment.
//
// Candidates are enumerated from two sources only — the Hub DB's secret
// records, and the fixed list of known hub-scope signing-key names — never
// from a GCP Secret Manager listing call (ptone/scion#2152 decision: no
// secrets.list anywhere in the migration path).
func runMigrateNames(ctx context.Context, backend *secret.GCPBackend, db store.SecretStore, hubID string, dryRun, deleteLegacy bool, out io.Writer) error {
	seen := make(map[migrateNamesCandidate]bool)
	var candidates []migrateNamesCandidate

	dbSecrets, err := db.ListSecrets(ctx, store.SecretFilter{})
	if err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}
	for _, s := range dbSecrets {
		c := migrateNamesCandidate{name: s.Key, scope: s.Scope, scopeID: s.ScopeID}
		if !seen[c] {
			seen[c] = true
			candidates = append(candidates, c)
		}
	}
	for _, keyName := range knownHubScopeSecretKeys {
		c := migrateNamesCandidate{name: keyName, scope: store.ScopeHub, scopeID: hubID}
		if !seen[c] {
			seen[c] = true
			candidates = append(candidates, c)
		}
	}

	// Deterministic order makes --dry-run output and test assertions stable.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].scope != candidates[j].scope {
			return candidates[i].scope < candidates[j].scope
		}
		if candidates[i].scopeID != candidates[j].scopeID {
			return candidates[i].scopeID < candidates[j].scopeID
		}
		return candidates[i].name < candidates[j].name
	})

	if len(candidates) == 0 {
		fmt.Fprintln(out, "No secrets found to check for name migration.")
		return nil
	}

	var migrated, skipped, failed, deletedLegacy int
	for _, c := range candidates {
		needs, err := backend.NeedsNameMigration(ctx, c.name, c.scope, c.scopeID)
		if err != nil {
			if err == store.ErrNotFound {
				// Not present in GCP SM under either name (e.g. a DB record
				// whose GCP SM write never completed, or a signing key never
				// provisioned). Nothing to do.
				skipped++
				continue
			}
			fmt.Fprintf(out, "  ERROR  %s (scope: %s/%s) - failed to check: %v\n", c.name, c.scope, c.scopeID, err)
			failed++
			continue
		}
		if !needs {
			// Already migrated (or never had a legacy name).
			skipped++
			continue
		}

		if dryRun {
			action := "WOULD MIGRATE"
			if deleteLegacy {
				action = "WOULD MIGRATE AND DELETE LEGACY"
			}
			fmt.Fprintf(out, "  %s  %s (scope: %s/%s)\n", action, c.name, c.scope, c.scopeID)
			migrated++
			continue
		}

		if _, err := backend.MigrateNameForward(ctx, c.name, c.scope, c.scopeID); err != nil {
			fmt.Fprintf(out, "  ERROR  %s (scope: %s/%s) - failed to migrate: %v\n", c.name, c.scope, c.scopeID, err)
			failed++
			continue
		}
		if err := backend.UpdateSecretRefToPrefixed(ctx, c.name, c.scope, c.scopeID); err != nil {
			fmt.Fprintf(out, "  WARN  %s (scope: %s/%s) - migrated GCP SM value but failed to update DB ref: %v\n", c.name, c.scope, c.scopeID, err)
		}
		fmt.Fprintf(out, "  MIGRATED  %s (scope: %s/%s)\n", c.name, c.scope, c.scopeID)
		migrated++

		if deleteLegacy {
			if err := backend.DeleteLegacySecretName(ctx, c.name, c.scope, c.scopeID); err != nil {
				fmt.Fprintf(out, "  ERROR  %s (scope: %s/%s) - failed to delete legacy secret: %v\n", c.name, c.scope, c.scopeID, err)
				failed++
				continue
			}
			fmt.Fprintf(out, "  DELETED LEGACY  %s (scope: %s/%s)\n", c.name, c.scope, c.scopeID)
			deletedLegacy++
		}
	}

	status := "complete"
	if dryRun {
		status = "dry run complete"
	}
	fmt.Fprintf(out, "\nMigrate-names %s: %d migrated, %d skipped (already migrated or absent), %d failed, %d legacy secrets deleted\n",
		status, migrated, skipped, failed, deletedLegacy)

	if failed > 0 {
		return fmt.Errorf("migrate-names finished with %d failure(s); re-run to retry (idempotent)", failed)
	}
	return nil
}
