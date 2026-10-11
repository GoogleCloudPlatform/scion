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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres SQLSTATEs a retry resolves.
const (
	pgSerializationFailure = "40001"
	pgDeadlockDetected     = "40P01"
)

// SQLite primary result codes a retry resolves. Extended codes carry the
// primary code in their low byte.
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// sqliteCodeError matches the SQLite driver's error type (modernc.org/sqlite
// *sqlite.Error) by its Code method, so this file does not import the
// driver, which no_sqlite builds leave out.
type sqliteCodeError interface {
	error
	Code() int
}

// isTransientDBError reports whether err is, by its typed driver error, a
// conflict a retry resolves.
func isTransientDBError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected
	}
	var sqErr sqliteCodeError
	if errors.As(err, &sqErr) && strings.Contains(strings.ToLower(fmt.Sprintf("%T", sqErr)), "sqlite") {
		code := sqErr.Code() & 0xff
		return code == sqliteBusy || code == sqliteLocked
	}
	return false
}

// markTransient wraps err with store.ErrTransient when it is a transient
// database conflict, keeping the original in the chain.
func markTransient(err error) error {
	if err == nil || errors.Is(err, store.ErrTransient) || !isTransientDBError(err) {
		return err
	}
	return fmt.Errorf("%w: %w", store.ErrTransient, err)
}
