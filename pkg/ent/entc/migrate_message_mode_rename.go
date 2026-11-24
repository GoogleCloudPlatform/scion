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
	"fmt"
	"log/slog"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// MigrateMessageModeRename performs an idempotent, single-transaction SQL
// migration rewriting retired agent message_mode values on both SQLite and
// PostgreSQL:
//   - "lineage" -> "owner"
//   - "branch"  -> "direct"
//
// Raw SQL is used because Ent's generated MessageModeValidator rejects retired
// enum values in typed predicates. Soft-deleted rows are intentionally included
// (no deleted_at filter) so a restored agent cannot return with a retired mode.
func MigrateMessageModeRename(ctx context.Context, client *ent.Client) error {
	driver := client.Driver()
	switch driver.Dialect() {
	case dialect.SQLite, dialect.Postgres:
	default:
		return fmt.Errorf("migrate agent message modes: unsupported dialect %q", driver.Dialect())
	}

	sqlDrv, ok := SQLDriver(driver)
	if !ok {
		return fmt.Errorf("migrate agent message modes: underlying SQL driver unavailable")
	}

	tx, err := sqlDrv.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent message mode migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	resOwner, err := tx.ExecContext(ctx, `UPDATE agents SET message_mode = 'owner' WHERE message_mode = 'lineage'`)
	if err != nil {
		return fmt.Errorf("migrate agent message_mode lineage->owner: %w", err)
	}
	ownerRows, _ := resOwner.RowsAffected()

	resDirect, err := tx.ExecContext(ctx, `UPDATE agents SET message_mode = 'direct' WHERE message_mode = 'branch'`)
	if err != nil {
		return fmt.Errorf("migrate agent message_mode branch->direct: %w", err)
	}
	directRows, _ := resDirect.RowsAffected()

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent message mode migration: %w", err)
	}
	committed = true

	if ownerRows > 0 || directRows > 0 {
		slog.InfoContext(ctx, "migrated retired agent message_mode values",
			"lineage_to_owner", ownerRows,
			"branch_to_direct", directRows,
		)
	}
	return nil
}
