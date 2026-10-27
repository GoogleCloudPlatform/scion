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
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLite extended result codes for unique-key violations
// (https://www.sqlite.org/rescode.html). The modernc.org/sqlite driver
// enables extended result codes on every connection, so its *sqlite.Error
// reports these rather than the bare SQLITE_CONSTRAINT (19).
const (
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// pgUniqueViolation is the PostgreSQL SQLSTATE for unique_violation.
const pgUniqueViolation = "23505"

// isUniqueViolation reports whether err is a unique or primary-key constraint
// violation, as opposed to any other constraint failure (foreign key, check,
// not null). It decides from the driver's structured error code rather than
// the error text, so it does not depend on driver message wording.
//
// The driver errors are matched through small interfaces instead of their
// concrete types so this package does not import either driver: pgx's
// *pgconn.PgError exposes SQLState(), and modernc.org/sqlite's *sqlite.Error
// exposes Code(). Ent's *ConstraintError wraps the driver error, so
// errors.As reaches it through ent's wrapping.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == pgUniqueViolation
	}
	var sqliteErr interface{ Code() int }
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return true
		}
	}
	return false
}

// hubSettingSectionConstraints names the Postgres unique constraints on
// hub_settings.section: the field's unique constraint and the schema's
// unique index (pkg/ent/schema/hubsetting.go). Either one rejects a second
// row for a section.
var hubSettingSectionConstraints = map[string]bool{
	"hub_settings_section_key": true,
	"hubsetting_section":       true,
}

// isHubSettingSectionConflict reports whether err is a unique violation on
// the hub_settings section key, the only error that means another writer
// created the same section first. On Postgres the violated constraint must
// be one of hubSettingSectionConstraints. On SQLite a unique or primary-key
// violation qualifies: the table's only other unique key is the random
// UUID primary key. Any other constraint error (check, foreign key, not
// null, or a unique key added later) is not a create race.
func isHubSettingSectionConflict(err error) bool {
	if !isUniqueViolation(err) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return hubSettingSectionConstraints[pgErr.ConstraintName]
	}
	return true
}
