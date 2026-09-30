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
	migrateNamesTimeout      time.Duration
	migrateNamesConfigPath   string
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
point in the migration and always converges. A plan line is printed for each
action taken (or, under --dry-run, that would be taken):
  1. If the legacy name has a value the prefixed name doesn't yet, copy the
     latest version's value (and GCP SM labels) to the prefixed name
     (MIGRATED). If the prefixed name already exists but holds a different
     value than the one the secret's ref designates (a mixed-version rolling
     deploy, or a rollback-then-roll-forward window), overwrite it with the
     ref-designated value instead of leaving it stale (RESYNCED).
  2. If the secret's Hub DB record exists and its SecretRef isn't the
     prefixed name yet (whether because this run just copied it, an earlier
     run partially failed, or the hub-startup signing-key copy-forward
     created the copy without ever being asked to touch the DB), repair the
     ref (REPAIRED REF). A record whose stored ref designates a value that's
     gone is reported as ORPHAN and skipped, not treated as a failure — there
     is nothing left for it to copy.
  3. If --delete-legacy is set and the legacy name still exists, delete it —
     but only once step 2 has confirmed the DB ref no longer depends on it,
     so a secret cannot become unreadable as a result once no old-binary
     writer remains (DELETED LEGACY) — see the --delete-legacy precondition
     below; GCP SM has no conditional delete, so this is not automatic.

If step 1 or 2 writes a new version to the prefixed name and then detects
that a concurrent write already changed the secret's ref, that secret is
reported as CONFLICT with a non-zero exit instead of being silently treated
as done: its prefixed name's latest version may now be stale relative to
the concurrent write. Diagnose by re-running migrate-names once WITHOUT
--delete-legacy (safe either way): if that re-run reports MIGRATED, RESYNCED
or REPAIRED REF for this secret, the CONFLICT was a false positive
(something bumped the record's Version without moving its ref -- e.g. a
metadata edit, or a legacy-name rotation) and is now resolved. DELETED
LEGACY / WOULD DELETE LEGACY is NOT that signal -- a true conflict's ref
already matches the prefixed name, so --delete-legacy deletes the legacy
copy regardless of whether the CONFLICT was ever actually resolved, and a
re-run that includes --delete-legacy reports that action even for a true
conflict, wrongly appearing to resolve it. If a plain re-run (without
--delete-legacy) reports nothing further to do for that secret, the ref
itself was already moved by a concurrent write, and further re-runs will
NOT detect or repair it -- you must re-set the secret directly to its
intended value through the normal secret-set path. (For a hub-scope
signing key, a CONFLICT caused by this command racing a hub replica's own
boot-time copy-forward of the same key is benign -- both copy identical
key material. Two replicas racing each other at boot show up only as a hub
WARN log, never as a migrate-names CONFLICT line, and are likewise benign.)

Hub ID: resolved the same way the running hub server resolves it at startup
(--hub-id, then this command's --config file's server.hub.hub_id, then the
server's own environment/hostname fallback) — pass --config if the server
runs with a non-default configuration file, or --hub-id explicitly, so this
command's hub ID always matches the server's.

Deploy ordering: grant the new hub-prefixed IAM condition to this hub's
service account BEFORE deploying a binary built from this or a later commit —
every write immediately targets the prefixed name, so writes fail with a
permission error otherwise. Keep the legacy grant in place until
--delete-legacy has been run and verified; only then remove it. Run a final
"--dry-run --delete-legacy" pass to confirm zero pending items BEFORE
removing the legacy grant, not after — once it's gone, this command can no
longer read legacy names at all, so a "zero pending" dry-run at that point
only proves IAM is narrowed, not that migration finished.

--delete-legacy precondition: run --delete-legacy only after EVERY replica
of this hub is running a binary that includes this change, i.e. no replica
is still writing legacy names. GCP SM has no conditional delete, so if an
older binary is still a live writer during a mixed-version rolling deploy,
its Set can land between this command's safety check and the actual delete
call, and the delete then destroys that write's only copy along with the
legacy container it lived in.

It is idempotent: re-running is always safe, and any candidate already fully
migrated (copied, ref repaired, legacy gone or never existed) is skipped.

Does not require a scion project; it can be run from any directory or
environment, for example a Cloud Run job.

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
	hubSecretMigrateNamesCmd.Flags().DurationVar(&migrateNamesTimeout, "timeout", 5*time.Minute, "Maximum time to run before aborting (increase for a large number of secrets)")
	hubSecretMigrateNamesCmd.Flags().StringVarP(&migrateNamesConfigPath, "config", "c", "", "Path to server configuration file (must match the one the running hub server uses, so hub ID resolution agrees)")

	_ = hubSecretMigrateNamesCmd.MarkFlagRequired("gcp-project")
}

func runSecretMigrateNames(cmd *cobra.Command, args []string) error {
	if migrateNamesProject == "" {
		return fmt.Errorf("--gcp-project flag is required")
	}

	// Default 5 minutes; --timeout raises this for a fleet with a large
	// number of secrets to check (ptone/scion#2152 round-3 review
	// non-blocking finding 11). Derived from cmd.Context(), not
	// context.Background(), so an operator's Ctrl+C (which cobra/cancelable
	// command execution cancels through cmd.Context()) also cancels this
	// command's own timeout context instead of being ignored until the timeout
	// fires on its own (GoogleCloudPlatform/scion#2123 review discussion_r4144099499).
	ctx, cancel := migrateNamesTimeoutContext(cmd, migrateNamesTimeout)
	defer cancel()

	cfg, err := config.LoadGlobalConfig(migrateNamesConfigPath)
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

	hubID, err := resolveMigrateNamesHubID(cfg, migrateNamesDryRun)
	if err != nil {
		return err
	}
	if hubID == "" {
		// An empty hubID still produces a valid, deterministic prefix
		// (sha256("")[:12]), so this would silently migrate every secret
		// into a shared, meaningless namespace instead of failing loudly
		// (ptone/scion#2152 review finding 15).
		return fmt.Errorf("resolved hub ID is empty; pass --hub-id explicitly or configure server.hub.hub_id")
	}
	if migrateNamesHubID == "" {
		if err := checkMigrateNamesHubIDAgainstExistingRecords(ctx, db, hubID); err != nil {
			return err
		}
	}
	printMigrateNamesHubID(cmd, hubID, secret.SecretNamePrefixForHubID(hubID))

	gcpBackend, err := secret.NewGCPBackend(ctx, db, secret.GCPBackendConfig{
		ProjectID:       migrateNamesProject,
		CredentialsJSON: credentialsJSON,
	}, hubID)
	if err != nil {
		return fmt.Errorf("failed to create GCP backend: %w", err)
	}

	return runMigrateNames(ctx, gcpBackend, db, hubID, migrateNamesDryRun, migrateNamesDeleteLegacy, cmd.OutOrStdout())
}

// migrateNamesTimeoutContext derives runSecretMigrateNames' own timeout
// context from cmd.Context(), not context.Background(), so a caller's
// cancellation of the command (e.g. Ctrl+C, or a test) also cancels this
// command's work instead of running until --timeout fires on its own
// (GoogleCloudPlatform/scion#2123 review discussion_r4144099499). Factored
// out so this derivation can be exercised in a test without constructing a
// real GCP backend.
func migrateNamesTimeoutContext(cmd *cobra.Command, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), timeout)
}

// printMigrateNamesHubID writes the resolved hub ID/prefix line to the
// command's own output writer (cmd.OutOrStdout()), not directly to
// fmt.Printf/os.Stdout, so the command's output can be captured in a test
// via cmd.SetOut (GoogleCloudPlatform/scion#2123 review
// discussion_r4144099512). Factored out so this routing can be tested
// without constructing a real GCP backend.
func printMigrateNamesHubID(cmd *cobra.Command, hubID, prefix string) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Using hub ID: %s (prefix: %s)\n", hubID, prefix)
}

// resolveMigrateNamesHubID resolves the hub ID exactly the way the running
// hub server does at startup — --hub-id, then settings server.hub.hub_id,
// then the server's own environment/hostname fallback
// (HubServerConfig.ResolveHubID()) — instead of a parallel resolution path
// that could silently disagree with the server and rename every secret out
// from under it (ptone/scion#2152 round-3 review finding 1, critical).
//
// Under --dry-run, the environment/hostname fallback is refused rather than
// followed when it would need to write: on a workstation with no persisted
// ~/.scion/hub-id yet, the real resolution path (PersistentHubID) creates and
// persists one as a side effect, and --dry-run must perform no writes at all.
// Rather than derive a value that might not match whatever a real run ends
// up persisting, this fails closed and asks for --hub-id.
func resolveMigrateNamesHubID(cfg *config.GlobalConfig, dryRun bool) (string, error) {
	if migrateNamesHubID != "" {
		return migrateNamesHubID, nil
	}
	if cfg.Hub.HubID != "" {
		return cfg.Hub.HubID, nil
	}
	if !dryRun {
		return cfg.Hub.ResolveHubID(), nil
	}
	id, ok := config.ResolveHubIDFromEnvReadOnly()
	if !ok {
		return "", fmt.Errorf("cannot resolve a hub ID without writing to disk for --dry-run (no server.hub.hub_id configured, no SCION_SERVER_HUB_HUBID or K_SERVICE set, and no persisted ~/.scion/hub-id yet); pass --hub-id explicitly")
	}
	return id, nil
}

// checkMigrateNamesHubIDAgainstExistingRecords refuses to proceed with a
// resolved hub ID that disagrees with an existing hub-scope secret record's
// ScopeID, unless the operator passed --hub-id explicitly (ptone/scion#2152
// round-3 review finding 1): such a mismatch — a workstation hostname
// change with a stale or missing persisted ~/.scion/hub-id, or a
// reconfigured server.hub.hub_id — would otherwise silently treat every
// secret already migrated under the old ID as unrelated to the prefix this
// run computes, rather than failing loudly.
func checkMigrateNamesHubIDAgainstExistingRecords(ctx context.Context, db store.SecretStore, hubID string) error {
	records, err := db.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub})
	if err != nil {
		return fmt.Errorf("failed to check existing hub-scope secret records: %w", err)
	}
	for _, r := range records {
		if r.ScopeID != "" && r.ScopeID != hubID {
			return fmt.Errorf("resolved hub ID %q does not match existing hub-scope secret record %q's scope ID %q; pass --hub-id explicitly if this is intentional (e.g. a deliberate hub rename)", hubID, r.Key, r.ScopeID)
		}
	}
	return nil
}

// stripDSNQueryParam removes every occurrence of a "key=..." query parameter
// from a "file:"-style sqlite DSN, leaving other parameters and their order
// intact. Used before forcing a specific value for a parameter the DSN might
// already set (ptone/scion#2152 round-3 review nit 12).
func stripDSNQueryParam(dsn, key string) string {
	idx := strings.Index(dsn, "?")
	if idx < 0 {
		return dsn
	}
	base, query := dsn[:idx], dsn[idx+1:]
	var kept []string
	for _, p := range strings.Split(query, "&") {
		if p == "" || p == key || strings.HasPrefix(p, key+"=") {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return base
	}
	return base + "?" + strings.Join(kept, "&")
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
			// Strip any existing "mode" query param before forcing "ro":
			// appending unconditionally could otherwise produce a DSN with
			// two conflicting "mode" params (e.g. a configured "mode=rwc")
			// where the driver's tie-break between them is undocumented
			// behavior to depend on (ptone/scion#2152 round-3 review nit 12).
			roDSN := stripDSNQueryParam(dsn, "mode")
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
		if dryRun {
			// OpenPostgresReadOnly sets default_transaction_read_only=on for
			// every connection: defense in depth on top of --dry-run only
			// ever calling read-only backend methods (ptone/scion#2152
			// round-3 review finding 10).
			ec, err := entc.OpenPostgresReadOnly(cfg.Database.URL, entc.PoolConfig{})
			if err != nil {
				return nil, fmt.Errorf("failed to open postgres database read-only: %w", err)
			}
			return entadapter.NewCompositeStore(ec), nil
		}
		ec, err := entc.OpenPostgres(cfg.Database.URL, entc.PoolConfig{})
		if err != nil {
			return nil, fmt.Errorf("failed to open postgres database: %w", err)
		}
		cs := entadapter.NewCompositeStore(ec)
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
		_, _ = fmt.Fprintln(out, "No secrets found to check for name migration.")
		return nil
	}

	var migrated, skipped, failed, deletedLegacy int
	for _, c := range candidates {
		if dryRun {
			acted, err := planMigrateNamesCandidate(ctx, backend, c, deleteLegacy, out)
			if err != nil {
				_, _ = fmt.Fprintf(out, "  ERROR  %s (scope: %s/%s) - failed to check: %v\n", c.name, c.scope, c.scopeID, err)
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
			if errors.Is(err, secret.ErrConflictingWrite) {
				// Distinct from a generic ERROR (round-5 review finding 2):
				// a concurrent write raced with this run's own write to the
				// prefixed name, so the secret's current value can't be
				// trusted without a human looking at it. Still counts
				// toward a non-zero exit, since silently continuing would
				// hide it. The guidance distinguishes a true conflict (the
				// ref itself was repointed by a concurrent Set -- a re-run
				// is then a no-op, since the ref already matches, so the
				// CONFLICT would simply vanish from the report while the
				// stale value remains; round-6 review finding 3) from a
				// false positive (a metadata edit or a legacy-name rotation
				// bumped Version without moving the ref -- a re-run then
				// converges normally; a known, deferred limitation, see
				// ptone/scion#2254 item 5). Re-running once without
				// --delete-legacy is always safe either way, and its result
				// tells the two cases apart.
				_, _ = fmt.Fprintf(out, "  CONFLICT  %s (scope: %s/%s) - a concurrent write was detected after this run already wrote a version to the prefixed name; re-run migrate-names once WITHOUT --delete-legacy (safe either way) -- if it now reports MIGRATED, RESYNCED or REPAIRED REF for this secret, the CONFLICT was a false positive and is resolved (DELETED LEGACY / WOULD DELETE LEGACY is NOT that signal); if it reports nothing further to do, the ref was already moved by a concurrent write and you must re-set the secret directly to its intended value\n", c.name, c.scope, c.scopeID)
			} else {
				_, _ = fmt.Fprintf(out, "  ERROR  %s (scope: %s/%s) - %v\n", c.name, c.scope, c.scopeID, err)
			}
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
	_, _ = fmt.Fprintf(out, "\nMigrate-names %s: %d migrated, %d skipped (already migrated or absent), %d failed, %d legacy secrets deleted\n",
		status, migrated, skipped, failed, deletedLegacy)

	if failed > 0 {
		return fmt.Errorf("migrate-names finished with %d failure(s); re-run to retry (idempotent)", failed)
	}
	return nil
}

// migrateNamesActionLabel maps a GCPBackend.RepairRefToPrefixed/PlanRefRepair
// action to the CLI's human-readable label for it.
func migrateNamesActionLabel(action secret.RefRepairAction) string {
	switch action {
	case secret.RefRepairCopied:
		return "MIGRATED"
	case secret.RefRepairResynced:
		return "RESYNCED"
	case secret.RefRepairRepaired:
		return "REPAIRED REF"
	default:
		return string(action)
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
// review findings 1 and 2). A record with a *stored* ref that turns out to
// be unreadable (including permission-denied) is a real migration failure,
// not something to silently skip (round-2 finding 3): a DB record whose ref
// depends on an unreadable legacy secret is broken, and reporting success
// would hide that. A record with no ref at all, and no value under the
// computed legacy name either, has nothing to migrate — that's
// store.ErrNotFound, reported as a skip — and a stored ref whose designated
// value is simply gone is reported as a visible ORPHAN, also not a failure
// (round-3 review finding 4): migrate-names has no value to copy in either
// case, so refusing to make progress on every *other* candidate because of
// one already-absent secret would be its own bug.
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
			switch {
			case err == nil:
				if action != "" {
					_, _ = fmt.Fprintf(out, "  %s  %s (scope: %s/%s)\n", migrateNamesActionLabel(action), c.name, c.scope, c.scopeID)
					acted = true
				}
			case err == store.ErrNotFound:
				// No stored ref, and the computed legacy name has no
				// accessible value either: nothing to migrate for this
				// identity, not a failure (ptone/scion#2152 round-3 review
				// finding 4).
			case errors.Is(err, secret.ErrOrphanedRef):
				// A distinct, visible condition: the record's stored ref
				// designates a value that's gone. Reported for operator
				// awareness, but not a migrate-names failure — there is
				// nothing for it to copy (round-3 review finding 4).
				_, _ = fmt.Fprintf(out, "  ORPHAN  %s (scope: %s/%s) - stored ref has no accessible value; nothing to migrate\n", c.name, c.scope, c.scopeID)
			case errors.Is(err, secret.ErrConflictingWrite):
				// Propagated as-is; runMigrateNames reports this as a
				// distinct CONFLICT outcome (round-5 review finding 2) rather
				// than a generic ERROR, since the advice differs (see the
				// CONFLICT line in runMigrateNames: a diagnostic re-run
				// without --delete-legacy, then re-set directly for a true
				// conflict).
				return false, err
			default:
				return false, fmt.Errorf("failed to migrate/repair (check the hub's IAM grant on the legacy name): %w", err)
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
			_, _ = fmt.Fprintf(out, "  MIGRATED  %s (scope: %s/%s)\n", c.name, c.scope, c.scopeID)
			acted = true
		}
	}

	// Delete the legacy name, only once nothing (that we can detect) still
	// depends on it. DeleteLegacySecretName re-verifies this itself via
	// canDeleteLegacyName: once a DB ref designates the prefixed name,
	// deletion is safe once the prefixed copy is confirmed accessible --
	// value equality with the legacy copy is not required and is not checked
	// in that case (a rotation performed after migration legitimately leaves
	// the legacy value stale; see ptone/scion#2152 round-3 review finding 2).
	// Value equality is used only for the no-DB-record path, where there is
	// no ref to establish authority from. Called directly here (instead of
	// via the PlanLegacyDeletion + DeleteLegacySecretName pair
	// planMigrateNamesCandidate's --dry-run path still uses) so this,
	// non-dry-run, path runs canDeleteLegacyName's GCP SM calls only once per
	// candidate instead of twice (GoogleCloudPlatform/scion#2123 review
	// discussion_r4144099464 / discussion_r4144099474).
	if deleteLegacy {
		deleted, err := backend.DeleteLegacySecretName(ctx, c.name, c.scope, c.scopeID)
		if err != nil {
			return acted, fmt.Errorf("failed to delete legacy secret: %w", err)
		}
		if deleted {
			_, _ = fmt.Fprintf(out, "  DELETED LEGACY  %s (scope: %s/%s)\n", c.name, c.scope, c.scopeID)
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

	// actionPlanned tracks whether a MIGRATE/RESYNC/REPAIR REF action was
	// just planned for this candidate. A real run performs that action
	// (RepairRefToPrefixed writes synchronously, and so does
	// MigrateNameForward for the no-DB-record path) before it ever checks
	// delete-legacy in the same invocation, so by the time it does, the
	// prefixed copy already exists and (where there's a DB record) the ref
	// already designates it. --dry-run never writes, so nothing actually
	// changes throughout the whole pass — the delete-legacy check below must
	// simulate the post-action state instead of asking canDeleteLegacyName
	// about today's (pre-action) state, or it would wrongly refuse a
	// combined "REPAIR REF AND DELETE LEGACY" (or "MIGRATE AND DELETE
	// LEGACY") plan that a real run completes successfully in one pass.
	actionPlanned := false
	if hasRecord {
		if !refIsPrefixed {
			action, err := backend.PlanRefRepair(ctx, c.name, c.scope, c.scopeID)
			switch {
			case err == nil:
				actionPlanned = true
				switch action {
				case secret.RefRepairCopied:
					actions = append(actions, "MIGRATE")
				case secret.RefRepairResynced:
					actions = append(actions, "RESYNC")
				case secret.RefRepairRepaired:
					actions = append(actions, "REPAIR REF")
				}
			case err == store.ErrNotFound:
				// Nothing to migrate for this identity (round-3 review
				// finding 4); leave actions empty rather than failing.
			case errors.Is(err, secret.ErrOrphanedRef):
				_, _ = fmt.Fprintf(out, "  ORPHAN  %s (scope: %s/%s) - stored ref has no accessible value; nothing to migrate\n", c.name, c.scope, c.scopeID)
			default:
				return false, fmt.Errorf("failed to check migrate/repair plan (check the hub's IAM grant on the legacy name): %w", err)
			}
		}
	} else {
		needsCopy, err := backend.NeedsNameMigration(ctx, c.name, c.scope, c.scopeID)
		if err != nil && err != store.ErrNotFound {
			return false, fmt.Errorf("failed to check name migration status: %w", err)
		}
		absent = err == store.ErrNotFound
		if needsCopy {
			actionPlanned = true
			actions = append(actions, "MIGRATE")
		}
	}

	if deleteLegacy && !absent {
		var canDelete bool
		var err error
		if actionPlanned {
			// The ref-repair/copy/resync above hasn't actually happened yet
			// (--dry-run never writes), but a real run would have already
			// performed it by this point in the same invocation: simulate
			// that post-action state. Presence is what matters here, not
			// today's (pre-action) ref value.
			canDelete, err = backend.LegacyStillPresent(ctx, c.name, c.scope, c.scopeID)
		} else {
			// No ref action will happen this pass (no DB record, the ref
			// already designated the prefixed name at entry — round-3
			// review finding 2's post-migration-rotation case — or there
			// was genuinely nothing to migrate/repair): use the exact
			// decision DeleteLegacySecretName itself makes, so --dry-run
			// reports exactly what --delete-legacy would do.
			canDelete, err = backend.PlanLegacyDeletion(ctx, c.name, c.scope, c.scopeID)
		}
		if err != nil {
			return false, fmt.Errorf("failed to check legacy secret: %w", err)
		}
		if canDelete {
			actions = append(actions, "DELETE LEGACY")
		}
	}

	if len(actions) == 0 {
		return false, nil
	}
	_, _ = fmt.Fprintf(out, "  WOULD %s  %s (scope: %s/%s)\n", strings.Join(actions, " AND "), c.name, c.scope, c.scopeID)
	return true, nil
}
