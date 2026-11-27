// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package entc

import (
	"context"
	"fmt"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// dropLegacyTriggerUniqueIndexes drops the pre-P9 unique indexes on
// trigger_fires(event_id, trigger_id) and trigger_subject_claims(trigger_id, subject)
// after schema migration adds the widened (..., action_index) unique indexes.
// AutoMigrate runs client.Schema.Create with migrate.WithDropIndex(false), so
// without this explicit drop an upgraded database would keep the old tighter
// unique indexes and reject multi-action rows (action_index > 0).
func dropLegacyTriggerUniqueIndexes(ctx context.Context, client *ent.Client) error {
	driver := client.Driver()
	switch driver.Dialect() {
	case dialect.SQLite, dialect.Postgres:
	default:
		return fmt.Errorf("drop legacy trigger unique indexes: unsupported dialect %q", driver.Dialect())
	}
	tx, err := driver.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin legacy trigger unique index removal: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for _, idx := range []string{
		"triggerfire_event_id_trigger_id",
		"triggersubjectclaim_trigger_id_subject",
	} {
		if err := tx.Exec(ctx, "DROP INDEX IF EXISTS "+idx, []any{}, nil); err != nil {
			return fmt.Errorf("drop legacy index %s: %w", idx, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy trigger unique index removal: %w", err)
	}
	committed = true
	return nil
}
