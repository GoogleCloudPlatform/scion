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
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

var (
	adminPromoteEmail  string
	adminPromoteDB     string
	adminPromoteConfig string
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
  scion admin promote --email user@example.com --db postgres://user:pass@host:5432/db

  # Promote a user with a specific config file
  scion admin promote --email user@example.com --config /path/to/server.yaml`,
	RunE: runAdminPromote,
}

func init() {
	adminCmd.AddCommand(adminPromoteCmd)
	rootCmd.AddCommand(adminCmd)

	adminPromoteCmd.Flags().StringVar(&adminPromoteEmail, "email", "", "Email address of the user to promote (required)")
	adminPromoteCmd.Flags().StringVar(&adminPromoteDB, "db", "", "Database URL/path (overrides config)")
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
	if adminPromoteDB != "" {
		if strings.HasPrefix(adminPromoteDB, "postgres://") || strings.HasPrefix(adminPromoteDB, "postgresql://") || strings.Contains(adminPromoteDB, "host=") {
			cfg.Database.Driver = "postgres"
		} else {
			cfg.Database.Driver = "sqlite"
		}
		cfg.Database.URL = adminPromoteDB
	}

	if cfg.Database.URL == "" {
		return fmt.Errorf("no database URL configured; provide --db flag or ensure server config exists")
	}

	printURL := cfg.Database.URL
	if u, err := url.Parse(printURL); err == nil && u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
			printURL = u.String()
		}
	}
	_, _ = fmt.Fprintf(out, "Database: %s (%s)\n", cfg.Database.Driver, printURL)

	// Open the database
	s, err := openRecoveryStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	// Look up the user by email
	user, err := s.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("user with email %q not found in the database; the user must already exist", email)
		}
		return fmt.Errorf("failed to look up user: %w", err)
	}

	// Check if already admin
	if user.Role == store.UserRoleAdmin {
		_, _ = fmt.Fprintf(out, "User %q (%s) is already an admin. No action taken.\n", email, user.ID)
		return nil
	}

	// Get super-admin role definition
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		return fmt.Errorf("failed to retrieve super-admin role definition: %w", err)
	}

	// Promote to admin: update role + create binding atomically
	previousRole := user.Role
	user.Role = store.UserRoleAdmin
	err = s.WithTx(ctx, func(tx store.Store) error {
		if err := tx.UpdateUser(ctx, user); err != nil {
			return fmt.Errorf("failed to update user role: %w", err)
		}
		_, err = tx.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      user.ID,
			ScopeType:        store.RoleScopeSystem,
			ScopeID:          "",
			CreatedBy:        store.AdminAPICreatedBy,
		})
		if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return fmt.Errorf("failed to create super-admin role binding: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "Successfully promoted user %q (%s) from %q to %q.\n", email, user.ID, previousRole, store.UserRoleAdmin)
	return nil
}
