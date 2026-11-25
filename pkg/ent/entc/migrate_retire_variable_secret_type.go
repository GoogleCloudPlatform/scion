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

package entc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const retiredSecretTypeVariable = "variable"

// VariableSecretDeleter is the narrow subset of secret.SecretBackend used by
// MigrateRetireVariableSecretTypeLeftovers to delete backend values alongside
// leftover "variable" rows in secrets and pending_secrets.
type VariableSecretDeleter interface {
	Delete(ctx context.Context, name, scope, scopeID string) error
	DropPending(ctx context.Context, ref string) error
}

// MigrateRetireVariableSecretType performs the database-only half of retiring
// the "variable" secret type (steps 1, 2 and 5 of the migration):
//   - Step 1 (secrets): Hub-scope rows whose key is a Hub internal producer key
//     (chat integrations, GitHub App, plugin auto-migration) are rewritten to
//     secret_type = 'internal'.
//   - Step 2 (secrets): Project- and agent-scope rows are rewritten to
//     secret_type = 'on_demand', target = key, injection_mode = 'as_needed'.
//   - Step 5 (secret_requests): All 'variable' rows are rewritten to 'on_demand'.
//
// Leftover 'variable' rows in secrets (hub non-producer, user, runtime_broker)
// and pending_secrets are handled after the secret backend is constructed by
// MigrateRetireVariableSecretTypeLeftovers so backend values (e.g., GCP Secret
// Manager) are deleted alongside the database rows.
//
// Each table runs in its own transaction using raw SQL and is idempotent on
// both SQLite and PostgreSQL.
func MigrateRetireVariableSecretType(ctx context.Context, client *ent.Client) error {
	driver := client.Driver()
	dial := driver.Dialect()
	switch dial {
	case dialect.SQLite, dialect.Postgres:
	default:
		return fmt.Errorf("retire variable secret type: unsupported dialect %q", dial)
	}

	// Table 1: secrets (steps 1 and 2 in one transaction).
	if err := migrateRetireVariableSecretsTable(ctx, driver, dial); err != nil {
		return err
	}

	// Table 2: secret_requests (step 5 in one transaction).
	if err := migrateRetireVariableSecretRequestsTable(ctx, driver, dial); err != nil {
		return err
	}

	return nil
}

func tableHasVariableSecrets(ctx context.Context, tx dialect.Tx, dial, table string) (bool, error) {
	query := fmt.Sprintf(
		"SELECT 1 FROM %s WHERE secret_type = %s LIMIT 1",
		table,
		sqlPlaceholder(dial, 1),
	)
	var rows entsql.Rows
	if err := tx.Query(ctx, query, []any{retiredSecretTypeVariable}, &rows); err != nil {
		return false, fmt.Errorf("probe %s for variable rows: %w", table, err)
	}
	found := rows.Next()
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("probe %s for variable rows: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("probe %s for variable rows: %w", table, err)
	}
	return found, nil
}

func migrateRetireVariableSecretsTable(ctx context.Context, driver dialect.Driver, dial string) error {
	tx, err := driver.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin retire variable secret type (secrets): %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Skip UPDATE statements when no 'variable' rows exist. This avoids
	// unnecessary writes on fresh/already-migrated databases and keeps
	// AutoMigrate free of parameterized Exec calls when enttest captures its
	// Postgres schema snapshot on an empty schema.
	hasRows, err := tableHasVariableSecrets(ctx, tx, dial, "secrets")
	if err != nil {
		return fmt.Errorf("retire variable secret type (secrets): %w", err)
	}
	if !hasRows {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit retire variable secret type (secrets): %w", err)
		}
		committed = true
		return nil
	}

	// Step 1: Hub producer keys -> internal.
	hubKeys := config.HubProducerSecretKeys()
	if len(hubKeys) > 0 {
		placeholders := make([]string, len(hubKeys))
		args := make([]any, 0, len(hubKeys)+3)
		args = append(args, store.SecretTypeInternal, retiredSecretTypeVariable, store.ScopeHub)
		for i, k := range hubKeys {
			placeholders[i] = sqlPlaceholder(dial, i+4)
			args = append(args, k)
		}
		stmt := fmt.Sprintf(
			"UPDATE secrets SET secret_type = %s WHERE secret_type = %s AND scope = %s AND key IN (%s)",
			sqlPlaceholder(dial, 1),
			sqlPlaceholder(dial, 2),
			sqlPlaceholder(dial, 3),
			strings.Join(placeholders, ", "),
		)
		if err := tx.Exec(ctx, stmt, args, nil); err != nil {
			return fmt.Errorf("retire variable secret type (step 1 hub producer keys): %w", err)
		}
	}

	// Step 2: Project and agent scope -> on_demand, target=key, injection_mode='as_needed'.
	step2Stmt := fmt.Sprintf(
		"UPDATE secrets SET secret_type = %s, target = key, injection_mode = %s WHERE secret_type = %s AND scope IN (%s, %s)",
		sqlPlaceholder(dial, 1),
		sqlPlaceholder(dial, 2),
		sqlPlaceholder(dial, 3),
		sqlPlaceholder(dial, 4),
		sqlPlaceholder(dial, 5),
	)
	step2Args := []any{
		store.SecretTypeOnDemand,
		store.InjectionModeAsNeeded,
		retiredSecretTypeVariable,
		store.ScopeProject,
		store.ScopeAgent,
	}
	if err := tx.Exec(ctx, step2Stmt, step2Args, nil); err != nil {
		return fmt.Errorf("retire variable secret type (step 2 project/agent): %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit retire variable secret type (secrets): %w", err)
	}
	committed = true
	return nil
}

func migrateRetireVariableSecretRequestsTable(ctx context.Context, driver dialect.Driver, dial string) error {
	tx, err := driver.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin retire variable secret type (secret_requests): %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	hasRows, err := tableHasVariableSecrets(ctx, tx, dial, "secret_requests")
	if err != nil {
		return fmt.Errorf("retire variable secret type (secret_requests): %w", err)
	}
	if !hasRows {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit retire variable secret type (secret_requests): %w", err)
		}
		committed = true
		return nil
	}

	stmt := fmt.Sprintf(
		"UPDATE secret_requests SET secret_type = %s WHERE secret_type = %s",
		sqlPlaceholder(dial, 1),
		sqlPlaceholder(dial, 2),
	)
	if err := tx.Exec(ctx, stmt, []any{store.SecretTypeOnDemand, retiredSecretTypeVariable}, nil); err != nil {
		return fmt.Errorf("retire variable secret type (step 5 secret_requests): %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit retire variable secret type (secret_requests): %w", err)
	}
	committed = true
	return nil
}

// MigrateRetireVariableSecretTypeLeftovers performs steps 3 and 4 of retiring
// the "variable" secret type once the secret backend is available:
//   - Step 3: Logs one slog.Warn per leftover secret_type='variable' row in
//     secrets and deletes it (deleting the backend value via deleter when
//     configured).
//   - Step 4: Logs one slog.Warn per leftover secret_type='variable' row in
//     pending_secrets, drops its held backend value via deleter when configured,
//     and deletes the row.
//
// Idempotent on both SQLite and PostgreSQL. Callers should guard it with an
// advisory lock at Hub startup.
func MigrateRetireVariableSecretTypeLeftovers(ctx context.Context, client *ent.Client, deleter VariableSecretDeleter) error {
	if client == nil {
		return nil
	}
	driver := client.Driver()
	dial := driver.Dialect()
	switch dial {
	case dialect.SQLite, dialect.Postgres:
	default:
		return fmt.Errorf("retire variable secret leftovers: unsupported dialect %q", dial)
	}
	db := SQLDB(driver)
	if db == nil {
		return fmt.Errorf("retire variable secret leftovers: underlying *sql.DB unavailable")
	}

	// Step 3: leftover variable rows in secrets.
	type leftoverSecret struct {
		key     string
		scope   string
		scopeID string
	}
	selectSecrets := fmt.Sprintf(
		"SELECT key, scope, scope_id FROM secrets WHERE secret_type = %s",
		sqlPlaceholder(dial, 1),
	)
	rows, err := db.QueryContext(ctx, selectSecrets, retiredSecretTypeVariable)
	if err != nil {
		return fmt.Errorf("retire variable secret leftovers (query secrets): %w", err)
	}
	var secretLeftovers []leftoverSecret
	for rows.Next() {
		var r leftoverSecret
		if err := rows.Scan(&r.key, &r.scope, &r.scopeID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("retire variable secret leftovers (scan secrets): %w", err)
		}
		secretLeftovers = append(secretLeftovers, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("retire variable secret leftovers (iterate secrets): %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("retire variable secret leftovers (close secrets): %w", err)
	}

	recheckSecretSQL := fmt.Sprintf(
		"SELECT secret_type FROM secrets WHERE key = %s AND scope = %s AND scope_id = %s",
		sqlPlaceholder(dial, 1),
		sqlPlaceholder(dial, 2),
		sqlPlaceholder(dial, 3),
	)
	deleteSecretSQL := fmt.Sprintf(
		"DELETE FROM secrets WHERE key = %s AND scope = %s AND scope_id = %s AND secret_type = %s",
		sqlPlaceholder(dial, 1),
		sqlPlaceholder(dial, 2),
		sqlPlaceholder(dial, 3),
		sqlPlaceholder(dial, 4),
	)
	var errs []error
	for _, r := range secretLeftovers {
		// Re-check immediately before calling the name-keyed backend Delete so
		// a concurrent API writer that replaced the row with a non-variable
		// secret between the initial SELECT and this delete is not wiped.
		// Note: a tiny residual race window remains between this re-check and
		// deleter.Delete because backend Delete is keyed by (name, scope,
		// scopeID) rather than type; this hook runs only at startup under an
		// advisory lock and only for keys still typed "variable".
		var currentType string
		err := db.QueryRowContext(ctx, recheckSecretSQL, r.key, r.scope, r.scopeID).Scan(&currentType)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			rowErr := fmt.Errorf("retire variable secret leftovers (recheck secret row %s/%s/%s): %w", r.scope, r.scopeID, r.key, err)
			slog.Error("failed to recheck retired variable secret row; continuing",
				"key", r.key, "scope", r.scope, "scope_id", r.scopeID, "error", err)
			errs = append(errs, rowErr)
			continue
		}
		if currentType != retiredSecretTypeVariable {
			continue
		}
		slog.Warn("retired variable secret deleted; recreate as environment/file/on_demand",
			"key", r.key, "scope", r.scope, "scope_id", r.scopeID)
		if deleter != nil {
			if err := deleter.Delete(ctx, r.key, r.scope, r.scopeID); err != nil && !errors.Is(err, store.ErrNotFound) {
				rowErr := fmt.Errorf("retire variable secret leftovers (backend delete %s/%s/%s): %w", r.scope, r.scopeID, r.key, err)
				slog.Error("failed to delete retired variable secret from backend; continuing",
					"key", r.key, "scope", r.scope, "scope_id", r.scopeID, "error", err)
				errs = append(errs, rowErr)
				continue
			}
		}
		// Ensure the DB row is removed even when deleter is nil or a custom
		// deleter only removes the external value. No-op if deleter already
		// deleted the row.
		if _, err := db.ExecContext(ctx, deleteSecretSQL, r.key, r.scope, r.scopeID, retiredSecretTypeVariable); err != nil {
			rowErr := fmt.Errorf("retire variable secret leftovers (delete secret row %s/%s/%s): %w", r.scope, r.scopeID, r.key, err)
			slog.Error("failed to delete retired variable secret row; continuing",
				"key", r.key, "scope", r.scope, "scope_id", r.scopeID, "error", err)
			errs = append(errs, rowErr)
			continue
		}
	}

	// Step 4: leftover variable rows in pending_secrets.
	type leftoverPending struct {
		id       string
		key      string
		scope    string
		scopeID  string
		valueRef string
	}
	selectPending := fmt.Sprintf(
		"SELECT id, key, scope, scope_id, value_ref FROM pending_secrets WHERE secret_type = %s",
		sqlPlaceholder(dial, 1),
	)
	pRows, err := db.QueryContext(ctx, selectPending, retiredSecretTypeVariable)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("retire variable secret leftovers (query pending_secrets): %w", err))...)
	}
	var pendingLeftovers []leftoverPending
	for pRows.Next() {
		var r leftoverPending
		var valueRef sql.NullString
		if err := pRows.Scan(&r.id, &r.key, &r.scope, &r.scopeID, &valueRef); err != nil {
			_ = pRows.Close()
			return errors.Join(append(errs, fmt.Errorf("retire variable secret leftovers (scan pending_secrets): %w", err))...)
		}
		r.valueRef = valueRef.String
		pendingLeftovers = append(pendingLeftovers, r)
	}
	if err := pRows.Err(); err != nil {
		_ = pRows.Close()
		return errors.Join(append(errs, fmt.Errorf("retire variable secret leftovers (iterate pending_secrets): %w", err))...)
	}
	if err := pRows.Close(); err != nil {
		return errors.Join(append(errs, fmt.Errorf("retire variable secret leftovers (close pending_secrets): %w", err))...)
	}

	deletePendingSQL := fmt.Sprintf(
		"DELETE FROM pending_secrets WHERE id = %s AND secret_type = %s",
		sqlPlaceholder(dial, 1),
		sqlPlaceholder(dial, 2),
	)
	for _, r := range pendingLeftovers {
		slog.Warn("retired variable pending secret deleted; recreate as environment/file/on_demand",
			"key", r.key, "scope", r.scope, "scope_id", r.scopeID)
		if deleter != nil && r.valueRef != "" {
			if err := deleter.DropPending(ctx, r.valueRef); err != nil {
				rowErr := fmt.Errorf("retire variable secret leftovers (drop pending %s/%s/%s): %w", r.scope, r.scopeID, r.key, err)
				slog.Error("failed to drop retired variable pending secret from backend; continuing",
					"key", r.key, "scope", r.scope, "scope_id", r.scopeID, "error", err)
				errs = append(errs, rowErr)
				continue
			}
		}
		if _, err := db.ExecContext(ctx, deletePendingSQL, r.id, retiredSecretTypeVariable); err != nil {
			rowErr := fmt.Errorf("retire variable secret leftovers (delete pending row %s): %w", r.id, err)
			slog.Error("failed to delete retired variable pending secret row; continuing",
				"key", r.key, "scope", r.scope, "scope_id", r.scopeID, "error", err)
			errs = append(errs, rowErr)
			continue
		}
	}

	return errors.Join(errs...)
}

func sqlPlaceholder(dial string, pos int) string {
	if dial == dialect.Postgres {
		return fmt.Sprintf("$%d", pos)
	}
	return "?"
}
