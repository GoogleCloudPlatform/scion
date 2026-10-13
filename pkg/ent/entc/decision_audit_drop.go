// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package entc

import (
	"context"
	"fmt"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// dropDecisionAuditTable removes retired persistence after schema migration.
// The table's absence is the idempotency marker; no other table is modified.
func dropDecisionAuditTable(ctx context.Context, client *ent.Client) error {
	driver := client.Driver()
	switch driver.Dialect() {
	case dialect.SQLite, dialect.Postgres:
	default:
		return fmt.Errorf("drop retired decision audit table: unsupported dialect %q", driver.Dialect())
	}
	tx, err := driver.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin decision audit table removal: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := tx.Exec(ctx, "DROP TABLE IF EXISTS decision_audits", []any{}, nil); err != nil {
		return fmt.Errorf("drop retired decision audit table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit decision audit table removal: %w", err)
	}
	committed = true
	return nil
}
