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
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/spf13/cobra"
)

var (
	recoverDisableConstraint  string
	recoverDisableAll         bool
	recoverOperator           string
	recoverConfigPath         string
	recoverDBURL              string
	recoverConfirmationPhrase string
)

// recoverAuthzCmd is the offline authorization recovery command.
// It deactivates broken access constraints without evaluating HTTP authorization.
// This is the last-resort recovery path for software defects or database corruption.
var recoverAuthzCmd = &cobra.Command{
	Use:   "recover-authz",
	Short: "Offline authorization recovery: disable broken access constraints",
	Long: `Deactivate a broken access constraint without evaluating HTTP authorization.

This is the LAST-RESORT recovery path for software defects or database corruption
that have locked all administrators out of the system.

The command:
  - Opens the database directly (no running server required)
  - Acquires an exclusive maintenance lock (refuses if another server is running)
  - Displays the constraint details and affected subjects
  - Requires explicit confirmation before proceeding
  - Deactivates ONLY the named constraint (does not delete it)
  - Records a recovery audit row

This command NEVER creates a user, role, or RoleBinding. Recovery only removes
restrictions so that existing grants take effect.

Examples:
  # Disable a specific constraint by ID
  scion server recover-authz --disable-constraint <constraint-id>

  # Disable all constraints (nuclear option — requires explicit confirmation phrase)
  scion server recover-authz --disable-all-constraints \
    --confirm "I understand this disables all access constraints"

  # Specify database URL explicitly
  scion server recover-authz --disable-constraint <id> --db "postgres://..."

  # Specify operator identity for audit
  scion server recover-authz --disable-constraint <id> --operator "admin@example.com"`,
	RunE: runRecoverAuthz,
}

func init() {
	serverCmd.AddCommand(recoverAuthzCmd)

	recoverAuthzCmd.Flags().StringVar(&recoverDisableConstraint, "disable-constraint", "", "ID of the access constraint to disable")
	recoverAuthzCmd.Flags().BoolVar(&recoverDisableAll, "disable-all-constraints", false, "Disable ALL access constraints (requires --confirm)")
	recoverAuthzCmd.Flags().StringVar(&recoverOperator, "operator", "", "Operator identity for audit record (defaults to $USER@hostname)")
	recoverAuthzCmd.Flags().StringVar(&recoverConfigPath, "config", "", "Path to server configuration file")
	recoverAuthzCmd.Flags().StringVar(&recoverDBURL, "db", "", "Database URL/path (overrides config)")
	recoverAuthzCmd.Flags().StringVar(&recoverConfirmationPhrase, "confirm", "", `Confirmation phrase (required for --disable-all-constraints: "I understand this disables all access constraints")`)
}

// DisableAllConfirmPhrase is the exact phrase required when using --disable-all-constraints.
const DisableAllConfirmPhrase = "I understand this disables all access constraints"

// LockRecoveryAuthz is the advisory lock key for offline authorization recovery.
// It prevents concurrent recovery operations and ensures no server is running.
const LockRecoveryAuthz store.AdvisoryLockKey = 0x5C100020

// recoverConfirmReader is the source of user input for interactive confirmation.
// Tests replace this with a strings.Reader to avoid blocking.
var recoverConfirmReader io.Reader = os.Stdin

func runRecoverAuthz(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()

	out := cmd.OutOrStdout()

	// Validate flags
	if recoverDisableConstraint == "" && !recoverDisableAll {
		return fmt.Errorf("either --disable-constraint <id> or --disable-all-constraints is required")
	}
	if recoverDisableConstraint != "" && recoverDisableAll {
		return fmt.Errorf("--disable-constraint and --disable-all-constraints are mutually exclusive")
	}

	// For --disable-all-constraints, require the explicit confirmation phrase
	if recoverDisableAll {
		if recoverConfirmationPhrase != DisableAllConfirmPhrase {
			return fmt.Errorf("--disable-all-constraints requires --confirm %q", DisableAllConfirmPhrase)
		}
	}

	// Resolve operator identity
	operator := recoverOperator
	if operator == "" {
		operator = resolveOperatorIdentity()
	}

	// Load config to find database
	cfg, err := config.LoadGlobalConfig(recoverConfigPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Override database URL if provided
	if recoverDBURL != "" {
		if strings.HasPrefix(recoverDBURL, "postgres://") || strings.HasPrefix(recoverDBURL, "postgresql://") || strings.Contains(recoverDBURL, "host=") {
			cfg.Database.Driver = "postgres"
		} else {
			cfg.Database.Driver = "sqlite"
		}
		cfg.Database.URL = recoverDBURL
	}

	if cfg.Database.URL == "" {
		return fmt.Errorf("no database URL configured; provide --db flag or ensure server config exists")
	}

	_, _ = fmt.Fprintf(out, "Database: %s (%s)\n", cfg.Database.Driver, cfg.Database.URL)
	_, _ = fmt.Fprintf(out, "Operator: %s\n\n", operator)

	// Open the database
	s, err := openRecoveryStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	// Acquire exclusive maintenance lock
	if err := acquireRecoveryLock(ctx, s, out); err != nil {
		return err
	}

	if recoverDisableAll {
		return recoverDisableAllConstraints(ctx, s, operator, out)
	}
	return recoverDisableSingleConstraint(ctx, s, recoverDisableConstraint, operator, out)
}

func openRecoveryStore(ctx context.Context, cfg *config.GlobalConfig) (*entadapter.CompositeStore, error) {
	pool := entc.PoolConfig{
		MaxOpenConns: 1,
		MaxIdleConns: 1,
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

// acquireRecoveryLock tries to acquire an exclusive advisory lock.
// If the lock cannot be acquired, it means another server instance or recovery
// process is running, and the command refuses to proceed.
//
// On Postgres, this uses pg_try_advisory_lock. On SQLite, the lock is a no-op
// (always succeeds) because the single-writer model already serializes access.
func acquireRecoveryLock(ctx context.Context, s *entadapter.CompositeStore, out io.Writer) error {
	acquired, release, err := s.TryAdvisoryLock(ctx, LockRecoveryAuthz)
	if err != nil {
		return fmt.Errorf("failed to acquire maintenance lock: %w", err)
	}
	if !acquired {
		_ = release()
		return fmt.Errorf("cannot acquire maintenance lock: another server instance or recovery process is running\n\nStop all server instances before running offline recovery:\n  scion server stop\n\nThen retry this command")
	}
	// Keep the lock held — it will be released when the connection closes.
	// We intentionally don't defer release() here because the store's Close()
	// will close the connection, releasing the lock.
	_, _ = fmt.Fprintln(out, "Maintenance lock acquired.")
	return nil
}

func recoverDisableSingleConstraint(ctx context.Context, s *entadapter.CompositeStore, constraintID, operator string, out io.Writer) error {
	// Look up the constraint
	constraint, err := s.GetAccessConstraint(ctx, constraintID)
	if err != nil {
		if err == store.ErrNotFound {
			return fmt.Errorf("constraint %q not found", constraintID)
		}
		return fmt.Errorf("failed to look up constraint: %w", err)
	}

	if constraint.Disabled {
		_, _ = fmt.Fprintf(out, "Constraint %q (%s) is already disabled. No action taken.\n", constraint.Name, constraint.ID)
		return nil
	}

	// Display constraint details
	recoverDisplayConstraint(constraint, out)

	// Prompt for confirmation
	if !recoverPromptConfirmation(out, fmt.Sprintf("Disable constraint %q?", constraint.Name)) {
		_, _ = fmt.Fprintln(out, "Aborted.")
		return nil
	}

	// Disable the constraint
	if err := s.DisableAccessConstraint(ctx, constraint.ID); err != nil {
		return fmt.Errorf("failed to disable constraint: %w", err)
	}

	// Write audit record
	hostname, _ := os.Hostname()
	if err := s.CreateMutationAudit(ctx, &store.MutationAuditRecord{
		Timestamp:          time.Now().UTC(),
		MutationType:       "offline_recovery.disable_constraint",
		ActorPrincipalKind: "operator",
		ActorPrincipalID:   operator,
		TargetType:         "access_constraint",
		TargetID:           constraint.ID,
		BeforeSummary:      fmt.Sprintf("constraint %q (name=%s, scope=%s/%s, disabled=false)", constraint.ID, constraint.Name, constraint.ScopeType, constraint.ScopeID),
		AfterSummary:       fmt.Sprintf("constraint %q (name=%s, disabled=true), operator=%s, host=%s", constraint.ID, constraint.Name, operator, hostname),
	}); err != nil {
		// Log the audit failure but don't fail the recovery —
		// the constraint is already disabled.
		_, _ = fmt.Fprintf(out, "WARNING: failed to write audit record: %v\n", err)
	}

	_, _ = fmt.Fprintf(out, "\nConstraint %q (%s) has been disabled.\n", constraint.Name, constraint.ID)
	_, _ = fmt.Fprintln(out, "Existing role bindings will take effect on the next server start.")
	return nil
}

func recoverDisableAllConstraints(ctx context.Context, s *entadapter.CompositeStore, operator string, out io.Writer) error {
	// List all constraints
	constraints, err := s.ListAccessConstraints(ctx, 1000, 0)
	if err != nil {
		return fmt.Errorf("failed to list constraints: %w", err)
	}

	if len(constraints) == 0 {
		_, _ = fmt.Fprintln(out, "No access constraints found. Nothing to do.")
		return nil
	}

	// Collect active (non-disabled) constraints
	var active []*store.AccessConstraint
	for _, c := range constraints {
		if !c.Disabled {
			active = append(active, c)
		}
	}

	if len(active) == 0 {
		_, _ = fmt.Fprintln(out, "All access constraints are already disabled. Nothing to do.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "Found %d active access constraint(s):\n\n", len(active))
	for _, c := range active {
		recoverDisplayConstraint(c, out)
		_, _ = fmt.Fprintln(out)
	}

	// Final interactive confirmation
	if !recoverPromptConfirmation(out, fmt.Sprintf("Disable ALL %d active constraint(s)?", len(active))) {
		_, _ = fmt.Fprintln(out, "Aborted.")
		return nil
	}

	hostname, _ := os.Hostname()
	var disabledCount int

	for _, c := range active {
		if err := s.DisableAccessConstraint(ctx, c.ID); err != nil {
			_, _ = fmt.Fprintf(out, "ERROR: failed to disable constraint %q (%s): %v\n", c.Name, c.ID, err)
			continue
		}

		// Write individual audit record for each disabled constraint
		if err := s.CreateMutationAudit(ctx, &store.MutationAuditRecord{
			Timestamp:          time.Now().UTC(),
			MutationType:       "offline_recovery.disable_constraint",
			ActorPrincipalKind: "operator",
			ActorPrincipalID:   operator,
			TargetType:         "access_constraint",
			TargetID:           c.ID,
			BeforeSummary:      fmt.Sprintf("constraint %q (name=%s, scope=%s/%s, disabled=false)", c.ID, c.Name, c.ScopeType, c.ScopeID),
			AfterSummary:       fmt.Sprintf("constraint %q (name=%s, disabled=true), operator=%s, host=%s, via=--disable-all-constraints", c.ID, c.Name, operator, hostname),
		}); err != nil {
			_, _ = fmt.Fprintf(out, "WARNING: failed to write audit record for constraint %q: %v\n", c.ID, err)
		}

		_, _ = fmt.Fprintf(out, "  Disabled: %s (%s)\n", c.Name, c.ID)
		disabledCount++
	}

	_, _ = fmt.Fprintf(out, "\n%d constraint(s) disabled.\n", disabledCount)
	_, _ = fmt.Fprintln(out, "Existing role bindings will take effect on the next server start.")
	return nil
}

func recoverDisplayConstraint(c *store.AccessConstraint, out io.Writer) {
	_, _ = fmt.Fprintf(out, "  Constraint: %s\n", c.Name)
	_, _ = fmt.Fprintf(out, "  ID:         %s\n", c.ID)
	_, _ = fmt.Fprintf(out, "  Scope:      %s", c.ScopeType)
	if c.ScopeID != "" {
		_, _ = fmt.Fprintf(out, " (%s)", c.ScopeID)
	}
	_, _ = fmt.Fprintln(out)

	// Subject
	switch c.SubjectKind {
	case store.ConstraintSubjectPrincipal:
		pType := "<unknown>"
		pID := "<unknown>"
		if c.SubjectPrincipalType != nil {
			pType = *c.SubjectPrincipalType
		}
		if c.SubjectPrincipalID != nil {
			pID = *c.SubjectPrincipalID
		}
		_, _ = fmt.Fprintf(out, "  Subject:    %s %s\n", pType, pID)
	case store.ConstraintSubjectGroupClosure:
		gID := "<unknown>"
		if c.SubjectGroupID != nil {
			gID = *c.SubjectGroupID
		}
		_, _ = fmt.Fprintf(out, "  Subject:    group closure %s\n", gID)
	case store.ConstraintSubjectAllPrincipals:
		_, _ = fmt.Fprintf(out, "  Subject:    all principals\n")
	default:
		_, _ = fmt.Fprintf(out, "  Subject:    %s\n", c.SubjectKind)
	}

	_, _ = fmt.Fprintf(out, "  Disabled:   %v\n", c.Disabled)
	_, _ = fmt.Fprintf(out, "  Max Perms:  %d permission(s)\n", len(c.MaximumPermissions))
	if len(c.MaximumPermissions) > 0 && len(c.MaximumPermissions) <= 20 {
		_, _ = fmt.Fprintf(out, "              [%s]\n", strings.Join(c.MaximumPermissions, ", "))
	}
	_, _ = fmt.Fprintf(out, "  Created:    %s by %s\n", c.CreatedAt.Format(time.RFC3339), c.CreatedBy)
}

func recoverPromptConfirmation(out io.Writer, question string) bool {
	_, _ = fmt.Fprintf(out, "%s [y/N]: ", question)
	reader := bufio.NewReader(recoverConfirmReader)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes"
}

func resolveOperatorIdentity() string {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	if user == "" {
		user = "unknown"
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	return user + "@" + hostname
}
