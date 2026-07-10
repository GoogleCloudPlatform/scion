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
	"strings"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

var (
	migrateStorageDryRun        bool
	migrateStorageCleanupLegacy bool
	migrateStorageConfigPath    string
)

var serverMigrateStorageCmd = &cobra.Command{
	Use:   "migrate-storage",
	Short: "Migrate GCS storage paths to hub-namespaced layout",
	Long: `Migrate legacy un-namespaced GCS storage paths to hub-scoped paths
(hubs/{hub-id}/...). This is used when upgrading to hub-namespaced storage.

The migration copies GCS objects from legacy paths to hub-namespaced paths
and updates DB records. Legacy objects are preserved unless --cleanup-legacy
is specified.

Examples:
  # Preview what would be migrated
  scion server migrate-storage --dry-run

  # Run the migration
  scion server migrate-storage

  # Migrate and delete legacy objects (only when all hubs are migrated)
  scion server migrate-storage --cleanup-legacy`,
	RunE: runMigrateStorage,
}

func init() {
	serverCmd.AddCommand(serverMigrateStorageCmd)
	serverMigrateStorageCmd.Flags().BoolVar(&migrateStorageDryRun, "dry-run", false, "Preview migration without making changes")
	serverMigrateStorageCmd.Flags().BoolVar(&migrateStorageCleanupLegacy, "cleanup-legacy", false, "Delete legacy objects after migration")
	serverMigrateStorageCmd.Flags().StringVarP(&migrateStorageConfigPath, "config", "c", "", "Path to server configuration file")
}

func runMigrateStorage(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	cfgPath := migrateStorageConfigPath
	if cfgPath == "" {
		cfgPath = serverConfigPath
	}

	cfg, err := config.LoadGlobalConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	hubID := cfg.Hub.ResolveHubID()
	if hubID == "" {
		return fmt.Errorf("hub_id is required for storage migration; set server.hub_id in config or SCION_SERVER_HUB_ID env var")
	}
	_, _ = fmt.Fprintf(out, "Hub ID: %s\n", hubID)

	bucket := cfg.Storage.Bucket
	if bucket == "" {
		return fmt.Errorf("storage bucket is required; set storage.bucket in config or --storage-bucket flag")
	}

	_, _ = fmt.Fprintf(out, "Opening database: %s (%s)\n", cfg.Database.Driver, cfg.Database.URL)
	var s *entadapter.CompositeStore
	switch cfg.Database.Driver {
	case "sqlite":
		sqliteDSN := cfg.Database.URL
		if !strings.HasPrefix(sqliteDSN, "file:") {
			sqliteDSN = "file:" + sqliteDSN
		}
		if !strings.Contains(sqliteDSN, "?") {
			sqliteDSN += "?cache=shared"
		} else if !strings.Contains(sqliteDSN, "cache=") {
			sqliteDSN += "&cache=shared"
		}
		client, err := entc.OpenSQLite(sqliteDSN, entc.PoolConfig{})
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}
		s = entadapter.NewCompositeStore(client)
		defer func() { _ = s.Close() }()
	case "postgres":
		client, err := entc.OpenPostgres(cfg.Database.URL, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}
		s = entadapter.NewCompositeStore(client)
		defer func() { _ = s.Close() }()
	default:
		return fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
	}

	_, _ = fmt.Fprintf(out, "Initializing storage: gs://%s\n", bucket)
	stor, err := storage.New(ctx, storage.Config{
		Provider: storage.Provider(cfg.Storage.Provider),
		Bucket:   bucket,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}
	defer func() { _ = stor.Close() }()

	hubSrv, err := hub.New(hub.ServerConfig{HubID: hubID}, s)
	if err != nil {
		return fmt.Errorf("failed to create hub server: %w", err)
	}
	hubSrv.SetStorage(stor)

	if migrateStorageDryRun {
		_, _ = fmt.Fprintf(out, "\nStorage migration for hub %q (dry run)\n", hubID)
	} else {
		_, _ = fmt.Fprintf(out, "\nRunning storage migration for hub %q\n", hubID)
	}

	report := hubSrv.MigrateStorage(ctx, migrateStorageDryRun, migrateStorageCleanupLegacy)

	_, _ = fmt.Fprintf(out, "\nMigration complete: %d migrated, %d skipped, %d failed\n",
		report.Migrated, report.Skipped, report.Failed)

	return nil
}
