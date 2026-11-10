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
	"errors"

	"github.com/spf13/cobra"
)

var (
	migrateProject     string
	migrateCredentials string
	migrateDryRun      bool
	migrateForce       bool
	migrateHubID       string
)

// errSecretMigrateDisabled is what `scion hub secret migrate` returns. The
// command copied each secret's stored at-rest form into Secret Manager
// without decoding it, so it refuses to run until it uses the same decode
// and read-back verification as the Hub's maintenance migration
// (ptone/scion#7633).
var errSecretMigrateDisabled = errors.New("scion hub secret migrate is disabled: it copies each secret's stored form into GCP Secret Manager without decoding it, which leaves migrated secrets unusable. " +
	"Use the Hub's secret-hub-id-migration maintenance migration instead (Admin > Maintenance, or POST /api/v1/admin/maintenance/migrations/secret-hub-id-migration/run), " +
	"which decodes each value with the Hub's local key and verifies it in Secret Manager. See ptone/scion#7633")

var hubSecretMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate secrets to GCP Secret Manager (disabled; use the Hub maintenance migration)",
	Long: `Disabled. This command copied each secret's stored form from the Hub
database into GCP Secret Manager without decoding it, which left migrated
secrets unusable, so it now refuses to run.

Use the Hub's secret-hub-id-migration maintenance migration instead (Admin >
Maintenance, or POST /api/v1/admin/maintenance/migrations/secret-hub-id-migration/run).
It decodes each value with the Hub's local key, verifies it in Secret Manager,
and repairs values that an earlier run copied in encoded form.

See ptone/scion#7633.`,
	Args: gcpProjectArgs(cobra.ArbitraryArgs),
	RunE: runSecretMigrate,
}

func init() {
	hubSecretCmd.AddCommand(hubSecretMigrateCmd)

	hubSecretMigrateCmd.Flags().StringVar(&migrateProject, "gcp-project", "", "GCP project ID")
	hubSecretMigrateCmd.Flags().StringVar(&migrateCredentials, "credentials", "", "Path to GCP credentials JSON file")
	hubSecretMigrateCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false, "Show what would be migrated without making changes")
	hubSecretMigrateCmd.Flags().BoolVar(&migrateForce, "force", false, "Re-migrate secrets that already have a GCP SM reference")
	hubSecretMigrateCmd.Flags().StringVar(&migrateHubID, "hub-id", "", "Hub instance ID for secret namespacing (defaults to sha256(hostname)[:12])")
	// No flag is required, so every invocation reaches the disabled
	// message instead of a missing-flag error.
}

// runSecretMigrate refuses to run; it opens no database and contacts no
// Secret Manager. See errSecretMigrateDisabled.
func runSecretMigrate(_ *cobra.Command, _ []string) error {
	return errSecretMigrateDisabled
}
