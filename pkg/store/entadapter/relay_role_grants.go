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

package entadapter

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"entgo.io/ent/dialect"
	"github.com/jackc/pgx/v5"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// maxPostgresIdentBytes is PostgreSQL's NAMEDATALEN - 1 identifier byte limit.
const maxPostgresIdentBytes = 63

// relayRoleCheckQuery inspects whether the configured relay role exists in
// pg_roles, resolves current_database() and current_schema(), and checks
// whether the role already holds CONNECT on the database and USAGE on the
// schema (for example via PUBLIC) so database/schema-level GRANTs run only
// when actually missing.
const relayRoleCheckQuery = `
SELECT
	EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1),
	current_database(),
	current_schema(),
	CASE WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)
		THEN has_database_privilege($1, current_database(), 'CONNECT')
		ELSE false
	END,
	CASE WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1) AND current_schema() IS NOT NULL
		THEN has_schema_privilege($1, current_schema(), 'USAGE')
		ELSE false
	END`

// RelayRoleNotFoundError reports that server.database.relay_role names a
// PostgreSQL role that does not exist in pg_catalog.pg_roles.
type RelayRoleNotFoundError struct {
	Role string
}

func (e *RelayRoleNotFoundError) Error() string {
	return fmt.Sprintf(
		"refusing to start: PostgreSQL relay role %q (server.database.relay_role / SCION_SERVER_DATABASE_RELAYROLE) does not exist; "+
			"create the role first or unset server.database.relay_role",
		e.Role,
	)
}

// ValidateRelayRole checks that role is empty (disabled) or a valid,
// untrimmed-clean PostgreSQL role identifier within NAMEDATALEN - 1 bytes with
// no NUL or control characters.
func ValidateRelayRole(role string) error {
	if role == "" {
		return nil
	}
	if strings.TrimSpace(role) != role {
		return fmt.Errorf("server.database.relay_role %q has leading or trailing whitespace", role)
	}
	if len(role) > maxPostgresIdentBytes {
		return fmt.Errorf("server.database.relay_role %q (%d bytes) exceeds PostgreSQL's %d-byte identifier limit",
			role, len(role), maxPostgresIdentBytes)
	}
	for i := 0; i < len(role); i++ {
		if role[i] < 0x20 || role[i] == 0x7f {
			return fmt.Errorf("server.database.relay_role contains a NUL or control byte at offset %d", i)
		}
	}
	return nil
}

// relayRoleGrantStatements builds the idempotent least-privilege PostgreSQL
// GRANT statements for a separate conduit relay (`scion server --role relay`).
//
// Deliberately omits `ALTER DEFAULT PRIVILEGES ... ON TABLES`: in PostgreSQL,
// default table privileges apply to every future table created in the schema,
// which would automatically grant the relay role access to future hub-only
// tables (tokens, secrets, auth). Instead, CompositeStore.Migrate runs this
// explicit allowlist on every hub startup right after entc.AutoMigrate (under
// store.LockSchemaMigration), so schema upgrades re-grant automatically while
// keeping all non-relay tables ungranted.
func relayRoleGrantStatements(role, dbName, schemaName string, hasConnect, hasUsage bool) []string {
	qRole := pgx.Identifier{role}.Sanitize()
	qDB := pgx.Identifier{dbName}.Sanitize()
	qSchema := pgx.Identifier{schemaName}.Sanitize()
	qualTable := func(tbl string) string {
		return pgx.Identifier{schemaName, tbl}.Sanitize()
	}

	stmts := make([]string, 0, 5)
	if !hasConnect {
		stmts = append(stmts, fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", qDB, qRole))
	}
	if !hasUsage {
		stmts = append(stmts, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", qSchema, qRole))
	}
	stmts = append(stmts,
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE %s, %s TO %s",
			qualTable("relay_instances"), qualTable("conduit_sessions"), qRole),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE ON TABLE %s TO %s",
			qualTable("conduit_principal_epochs"), qRole),
		fmt.Sprintf("GRANT SELECT ON TABLE %s TO %s",
			qualTable("hub_settings"), qRole),
	)
	return stmts
}

// EnsureRelayRoleGrants applies the least-privilege PostgreSQL grants for role
// on client's connection when client uses the Postgres dialect and role is
// non-empty. Used by standalone migration commands (`scion server migrate`)
// that migrate a raw *ent.Client rather than a *CompositeStore.
func EnsureRelayRoleGrants(ctx context.Context, client *ent.Client, role string, logger *slog.Logger) error {
	return ensureRelayRoleGrants(ctx, client.Driver().Dialect(), clientDB(client), role, logger)
}

func ensureRelayRoleGrants(ctx context.Context, d string, db *sql.DB, role string, logger *slog.Logger) error {
	if err := ValidateRelayRole(role); err != nil {
		return err
	}
	if role == "" || d != dialect.Postgres || db == nil {
		return nil
	}

	var (
		roleExists bool
		dbName     string
		schemaName sql.NullString
		hasConnect bool
		hasUsage   bool
	)
	if err := db.QueryRowContext(ctx, relayRoleCheckQuery, role).Scan(
		&roleExists, &dbName, &schemaName, &hasConnect, &hasUsage,
	); err != nil {
		return fmt.Errorf("checking PostgreSQL relay role %q: %w", role, err)
	}
	if !roleExists {
		return &RelayRoleNotFoundError{Role: role}
	}
	if !schemaName.Valid || schemaName.String == "" {
		return fmt.Errorf("granting PostgreSQL relay role %q: current_schema() is unset", role)
	}

	for _, stmt := range relayRoleGrantStatements(role, dbName, schemaName.String, hasConnect, hasUsage) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("granting PostgreSQL relay role privileges (%s): %w", stmt, err)
		}
	}

	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("Ensured least-privilege PostgreSQL grants for conduit relay role",
		"role", role, "schema", schemaName.String)
	return nil
}
