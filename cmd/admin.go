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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/spf13/cobra"
)

var (
	adminPromoteEmail    string
	adminPromoteDBURL    string
	adminPromoteConfig   string
)

// adminCmd is the top-level command group for administrative operations.
var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Administrative operations (break-glass recovery)",
	Long: `Administrative operations for emergency recovery scenarios.

These commands connect directly to the database, bypassing the running server.
They are intended for break-glass situations where normal admin access
has been lost.`,
}

// adminPromoteCmd promotes a user to admin role via direct database access.
var adminPromoteCmd = &cobra.Command{
	Use:   "promote",
	Short: "Promote a user to admin role (break-glass)",
	Long: `Promote an existing user to admin role by connecting directly to the database.

This is a break-glass recovery command for situations where all admin users
have been removed or an organization has lost admin access. The command
bypasses the running server and modifies the database directly.

The user must already exist in the database. This command will not create
new users.

Examples:
  # Promote a user using config-derived database connection
  scion admin promote --email user@example.com

  # Promote a user with explicit database URL
  scion admin promote --email user@example.com --db-url postgres://user:pass@host:5432/db

  # Promote a user with a specific config file
  scion admin promote --email user@example.com --config /path/to/server.yaml`,
	RunE: runAdminPromote,
}

func init() {
	adminCmd.AddCommand(adminPromoteCmd)
	rootCmd.AddCommand(adminCmd)

	adminPromoteCmd.Flags().StringVar(&adminPromoteEmail, "email", "", "Email address of the user to promote (required)")
	adminPromoteCmd.Flags().StringVar(&adminPromoteDBURL, "db-url", "", "Database URL/path (overrides config)")
	adminPromoteCmd.Flags().StringVar(&adminPromoteConfig, "config", "", "Path to server configuration file")

	_ = adminPromoteCmd.MarkFlagRequired("email")
}

func runAdminPromote(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	out := cmd.OutOrStdout()

	email := strings.TrimSpace(strings.ToLower(adminPromoteEmail))
	if email == "" {
		return fmt.Errorf("--email is required")
	}

	// Load config to find database
	cfg, err := config.LoadGlobalConfig(adminPromoteConfig)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Override database URL if provided
	if adminPromoteDBURL != "" {
		if strings.HasPrefix(adminPromoteDBURL, "postgres://") || strings.HasPrefix(adminPromoteDBURL, "postgresql://") || strings.Contains(adminPromoteDBURL, "host=") {
			cfg.Database.Driver = "postgres"
		} else {
			cfg.Database.Driver = "sqlite"
		}
		cfg.Database.URL = adminPromoteDBURL
	}

	if cfg.Database.URL == "" {
		return fmt.Errorf("no database URL configured; provide --db-url flag or ensure server config exists")
	}

	_, _ = fmt.Fprintf(out, "Database: %s (%s)\n", cfg.Database.Driver, cfg.Database.URL)

	// Open the database
	s, err := openAdminStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	// Look up the user by email
	user, err := s.GetUserByEmail(ctx, email)
	if err != nil {
		if err == store.ErrNotFound {
			return fmt.Errorf("user with email %q not found in the database; the user must already exist", email)
		}
		return fmt.Errorf("failed to look up user: %w", err)
	}

	// Check if already admin
	if user.Role == store.UserRoleAdmin {
		_, _ = fmt.Fprintf(out, "User %q (%s) is already an admin. No action taken.\n", email, user.ID)
		return nil
	}

	// Promote to admin
	previousRole := user.Role
	user.Role = store.UserRoleAdmin
	if err := s.UpdateUser(ctx, user); err != nil {
		return fmt.Errorf("failed to update user role: %w", err)
	}

	_, _ = fmt.Fprintf(out, "Successfully promoted user %q (%s) from %q to %q.\n", email, user.ID, previousRole, store.UserRoleAdmin)
	return nil
}

// openAdminStore opens a database connection for the admin command.
// This follows the same pattern as openRecoveryStore in server_recover_authz.go.
func openAdminStore(ctx context.Context, cfg *config.GlobalConfig) (*entadapter.CompositeStore, error) {
	pool := entc.PoolConfig{
		MaxOpenConns: 2,
		MaxIdleConns: 2,
	}

	var entClient interface{ Close() error }
	var cs *entadapter.CompositeStore

	switch strings.ToLower(cfg.Database.Driver) {
	case "sqlite", "":
		sqliteDSN := cfg.Database.URL
		if !strings.HasPrefix(sqliteDSN, "file:") {
			sqliteDSN = "file:" + sqliteDSN
		}
		if !strings.Contains(sqliteDSN, "?") {
			sqliteDSN += "?cache=shared"
		} else if !strings.Contains(sqliteDSN, "cache=") {
			sqliteDSN += "&cache=shared"
		}
		ec, err := entc.OpenSQLite(sqliteDSN, pool)
		if err != nil {
			return nil, err
		}
		entClient = ec
		cs = entadapter.NewCompositeStore(ec)
	case "postgres":
		ec, err := entc.OpenPostgres(cfg.Database.URL, pool)
		if err != nil {
			return nil, err
		}
		entClient = ec
		cs = entadapter.NewCompositeStore(ec)
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
	}

	// Run migrations to ensure schema is up to date
	if err := cs.Migrate(ctx); err != nil {
		_ = entClient.Close()
		return nil, fmt.Errorf("failed to run database migration: %w", err)
	}

	return cs, nil
}
