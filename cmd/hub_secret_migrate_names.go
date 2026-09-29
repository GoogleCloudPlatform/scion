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
	"strings"
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
// from Secret Manager itself. Every key here is one ensureSigningKey or
// OIDCKeyManager.loadOrCreateKey resolves through syncSigningKeyToBackend /
// CopyHubSecretForward (review finding 5).
var knownHubScopeSecretKeys = []string{
	hub.SecretKeyAgentSigningKey,
	hub.SecretKeyUserSigningKey,
	hub.SecretKeyOIDCSigningKey,
	hub.SecretKeyDownloadSigningKey,
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

For each candidate secret identity, this command independently checks three
things and performs whichever apply, so it is safe to run repeatedly at any
point in the migration and always converges:
  1. If the legacy name has a value the prefixed name doesn't yet, copy the
     latest version's value (and GCP SM labels) to the prefixed name.
  2. If the secret's Hub DB record exists and its SecretRef isn't the
     prefixed name yet (whether because this run just copied it, an earlier
     run partially failed, or the hub-startup signing-key copy-forward
     created the copy without ever being asked to touch the DB), repair the
     ref.
  3. If --delete-legacy is set and the legacy name still exists, delete it —
     but only once step 2 has confirmed the DB ref no longer depends on it,
     so a secret can never become unreadable as a result.

Deploy ordering: grant the new hub-prefixed IAM condition to this hub's
service account BEFORE deploying a binary built from this or a later commit —
every write immediately targets the prefixed name, so writes fail with a
permission error otherwise. Keep the legacy grant in place until
--delete-legacy has been run and verified; only then remove it.

It is idempotent: re-running is always safe, and any candidate already fully
migrated (copied, ref repaired, legacy gone or never existed) is skipped.

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

	db, err := openMigrateNamesStore(ctx, cfg, migrateNamesDryRun)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() { _ = db.Close() }()

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
	if hubID == "" {
		// An empty hubID still produces a valid, deterministic prefix
		// (sha256("")[:12]), so this would silently migrate every secret
		// into a shared, meaningless namespace instead of failing loudly
		// (ptone/scion#2152 review finding 15).
		return fmt.Errorf("resolved hub ID is empty; pass --hub-id explicitly or configure server.hub.hub_id")
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

// openMigrateNamesStore opens the Hub database using the driver configured in
// server settings (sqlite or postgres), mirroring openRecoveryStore in
// cmd/server_recover_authz.go.
//
// Under --dry-run, a sqlite database is opened with entc.OpenSQLiteReadOnly
// instead of entc.OpenSQLite: the plain (read-write) opener creates the file
// if it's missing and switches its journal to WAL, both of which are writes,
// so --dry-run was not actually write-free even though schema migration was
// already skipped (ptone/scion#2152 round-2 review finding 5, refining
// round-1 finding 6). The DSN also gets an explicit "mode=ro" so a missing
// database file is a clear open error instead of being silently created.
// Postgres has no equivalent stray-file failure mode (a missing database
// doesn't get created by connecting to it), so --dry-run there is already
// write-free by skipping Migrate.
func openMigrateNamesStore(ctx context.Context, cfg *config.GlobalConfig, dryRun bool) (*entadapter.CompositeStore, error) {
	switch strings.ToLower(cfg.Database.Driver) {
	case "sqlite", "":
		dsn := cfg.Database.URL
		if !strings.HasPrefix(dsn, "file:") {
			dsn = "file:" + dsn
		}
		if dryRun {
			roDSN := dsn
			if strings.Contains(roDSN, "?") {
				roDSN += "&mode=ro"
			} else {
				roDSN += "?mode=ro"
			}
			ec, err := entc.OpenSQLiteReadOnly(roDSN)
			if err != nil {
				return nil, fmt.Errorf("failed to open sqlite database read-only: %w", err)
			}
			return entadapter.NewCompositeStore(ec), nil
		}
		if !strings.Contains(dsn, "cache=") {
			if strings.Contains(dsn, "?") {
				dsn += "&cache=shared"
			} else {
				dsn += "?cache=shared"
			}
		}
		ec, err := entc.OpenSQLite(dsn, entc.PoolConfig{})
		if err != nil {
			return nil, fmt.Errorf("failed to open sqlite database: %w", err)
		}
		cs := entadapter.NewCompositeStore(ec)
		if err := cs.Migrate(ctx); err != nil {
			_ = cs.Close()
			return nil, fmt.Errorf("failed to run database migration: %w", err)
		}
		return cs, nil
	case "postgres":
		ec, err := entc.OpenPostgres(cfg.Database.URL, entc.PoolConfig{})
		if err != nil {
			return nil, fmt.Errorf("failed to open postgres database: %w", err)
		}
		cs := entadapter.NewCompositeStore(ec)
		if dryRun {
			return cs, nil
		}
		if err := cs.Migrate(ctx); err != nil {
			_ = cs.Close()
			return nil, fmt.Errorf("failed to run database migration: %w", err)
		}
		return cs, nil
	default:
		return nil, fmt.Errorf("unsupported database driver %q", cfg.Database.Driver)
	}
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
//
// Each candidate is independently checked for three, separately-applicable
// actions (copy, ref-repair, legacy-delete) rather than an early-exit
// if/else chain, so that re-running after a partial success or after the
// hub-startup signing-key copy-forward (which creates the prefixed copy
// without going through this command) still finishes the job instead of
// reporting "already migrated" and skipping the remaining steps
// (ptone/scion#2152 review findings 1 and 2).
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
		if dryRun {
			acted, err := planMigrateNamesCandidate(ctx, backend, c, deleteLegacy, out)
			if err != nil {
				fmt.Fprintf(out, "  ERROR  %s (scope: %s/%s) - failed to check: %v\n", c.name, c.scope, c.scopeID, err)
				failed++
				continue
			}
			if acted {
				migrated++
			} else {
				skipped++
			}
			continue
		}

		acted, err := migrateOneCandidate(ctx, backend, c, deleteLegacy, out, &deletedLegacy)
		if err != nil {
			fmt.Fprintf(out, "  ERROR  %s (scope: %s/%s) - %v\n", c.name, c.scope, c.scopeID, err)
			failed++
			continue
		}
		if acted {
			migrated++
		} else {
			skipped++
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

// migrateNamesActionLabel maps a GCPBackend.RepairRefToPrefixed/PlanRefRepair
// action to the CLI's human-readable label for it.
func migrateNamesActionLabel(action string) string {
	switch action {
	case "copied":
		return "MIGRATED"
	case "resynced":
		return "RESYNCED"
	case "repaired":
		return "REPAIRED REF"
	default:
		return action
	}
}

// migrateOneCandidate performs whichever of copy-or-resync / legacy-delete
// apply to one secret identity, stopping at the first error. acted is true if
// any action actually did something (used for the migrated/skipped
// counters); an identity that was already fully migrated and has nothing
// left to do returns (false, nil).
//
// When a DB record exists, its SecretRef is authoritative (see
// GCPBackend.RepairRefToPrefixed) and a single call to it handles copying,
// resyncing a stale prefixed copy, and/or repairing the ref, whichever
// applies — this is what makes the documented two-step workflow (plain run,
// then a later --delete-legacy run) converge, and what makes a stale
// prefixed copy from a mixed-version rolling deploy or a rollback get
// corrected rather than blindly repointed to (ptone/scion#2152 round-2
// review findings 1 and 2). Any error from it — including the legacy name
// being permission-denied — is a real migration failure, not something to
// silently skip (finding 3): a DB record whose ref depends on an unreadable
// legacy secret is broken, and reporting success would hide that.
//
// Only when no DB record exists at all (a hub-scope key recovered directly
// from GCP SM, with no ref to consult) does this fall back to the
// presence-based NeedsNameMigration/MigrateNameForward check, which is where
// the softer "PermissionDenied on the legacy name means absent" rule still
// applies (there being no ref that depends on it).
func migrateOneCandidate(ctx context.Context, backend *secret.GCPBackend, c migrateNamesCandidate, deleteLegacy bool, out io.Writer, deletedLegacy *int) (acted bool, err error) {
	hasRecord, refIsPrefixed, err := backend.RefPointsAtPrefixed(ctx, c.name, c.scope, c.scopeID)
	if err != nil {
		return false, fmt.Errorf("failed to check DB ref: %w", err)
	}

	if hasRecord {
		if !refIsPrefixed {
			action, err := backend.RepairRefToPrefixed(ctx, c.name, c.scope, c.scopeID)
			if err != nil {
				return false, fmt.Errorf("failed to migrate/repair (check the hub's IAM grant on the legacy name): %w", err)
			}
			if action != "" {
				fmt.Fprintf(out, "  %s  %s (scope: %s/%s)\n", migrateNamesActionLabel(action), c.name, c.scope, c.scopeID)
				acted = true
			}
		}
	} else {
		needsCopy, err := backend.NeedsNameMigration(ctx, c.name, c.scope, c.scopeID)
		if err != nil && err != store.ErrNotFound {
			return false, fmt.Errorf("failed to check name migration status: %w", err)
		}
		if needsCopy {
			if _, err := backend.MigrateNameForward(ctx, c.name, c.scope, c.scopeID); err != nil {
				return false, fmt.Errorf("failed to migrate: %w", err)
			}
			fmt.Fprintf(out, "  MIGRATED  %s (scope: %s/%s)\n", c.name, c.scope, c.scopeID)
			acted = true
		}
	}

	// Delete the legacy name, only once nothing (that we can detect) still
	// depends on it. DeleteLegacySecretName independently re-verifies the DB
	// ref and the GCP SM value match before deleting, so this is defense in
	// depth, not the only check. Note this runs even after a resync above:
	// once resynced, the prefixed and legacy values are identical, so the
	// value-equality check below passes.
	if deleteLegacy {
		legacyPresent, err := backend.LegacyStillPresent(ctx, c.name, c.scope, c.scopeID)
		if err != nil {
			return acted, fmt.Errorf("failed to check legacy secret: %w", err)
		}
		if legacyPresent {
			if err := backend.DeleteLegacySecretName(ctx, c.name, c.scope, c.scopeID); err != nil {
				return acted, fmt.Errorf("failed to delete legacy secret: %w", err)
			}
			fmt.Fprintf(out, "  DELETED LEGACY  %s (scope: %s/%s)\n", c.name, c.scope, c.scopeID)
			*deletedLegacy++
			acted = true
		}
	}

	return acted, nil
}

// planMigrateNamesCandidate is the --dry-run counterpart of
// migrateOneCandidate: it performs the same checks, using only read-only
// backend calls (PlanRefRepair instead of RepairRefToPrefixed), and prints
// what would happen instead of doing it. A value-mismatch is planned as
// RESYNC, matching what migrateOneCandidate would actually do — not as a
// plain MIGRATE/REPAIR REF that would misrepresent the action taken
// (ptone/scion#2152 round-2 review non-blocking finding 13).
func planMigrateNamesCandidate(ctx context.Context, backend *secret.GCPBackend, c migrateNamesCandidate, deleteLegacy bool, out io.Writer) (planned bool, err error) {
	var actions []string
	absent := false

	hasRecord, refIsPrefixed, err := backend.RefPointsAtPrefixed(ctx, c.name, c.scope, c.scopeID)
	if err != nil {
		return false, fmt.Errorf("failed to check DB ref: %w", err)
	}

	if hasRecord {
		if !refIsPrefixed {
			action, err := backend.PlanRefRepair(ctx, c.name, c.scope, c.scopeID)
			if err != nil {
				return false, fmt.Errorf("failed to check migrate/repair plan (check the hub's IAM grant on the legacy name): %w", err)
			}
			switch action {
			case "copied":
				actions = append(actions, "MIGRATE")
			case "resynced":
				actions = append(actions, "RESYNC")
			case "repaired":
				actions = append(actions, "REPAIR REF")
			}
		}
	} else {
		needsCopy, err := backend.NeedsNameMigration(ctx, c.name, c.scope, c.scopeID)
		if err != nil && err != store.ErrNotFound {
			return false, fmt.Errorf("failed to check name migration status: %w", err)
		}
		absent = err == store.ErrNotFound
		if needsCopy {
			actions = append(actions, "MIGRATE")
		}
	}

	if deleteLegacy && !absent {
		legacyPresent, err := backend.LegacyStillPresent(ctx, c.name, c.scope, c.scopeID)
		if err != nil {
			return false, fmt.Errorf("failed to check legacy secret: %w", err)
		}
		if legacyPresent {
			actions = append(actions, "DELETE LEGACY")
		}
	}

	if len(actions) == 0 {
		return false, nil
	}
	fmt.Fprintf(out, "  WOULD %s  %s (scope: %s/%s)\n", strings.Join(actions, " AND "), c.name, c.scope, c.scopeID)
	return true, nil
}
