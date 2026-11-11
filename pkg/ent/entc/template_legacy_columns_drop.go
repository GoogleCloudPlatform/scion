// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package entc

import (
	"context"
	"fmt"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// templateLegacyColumns are the retired templates columns, in drop order.
// Both were write-refused by the hub (400 template_config_in_file /
// template_image_not_supported) and ignored for agents before they were
// removed from the ent schema (ptone/scion#4224). They carry no index,
// constraint or foreign key.
var templateLegacyColumns = []string{"config", "image"}

// templateColumnsQuery lists the templates table's column names, per dialect.
// On Postgres it reads the connection's current schema, the one ent migrates.
var templateColumnsQuery = map[string]string{
	dialect.SQLite: "SELECT name FROM pragma_table_info('templates')",
	dialect.Postgres: "SELECT column_name FROM information_schema.columns " +
		"WHERE table_schema = current_schema() AND table_name = 'templates'",
}

// dropTemplateLegacyColumns physically removes the retired templates.config
// and templates.image columns after schema migration. AutoMigrate runs with
// migrate.WithDropColumn(false), so removing the ent fields alone leaves the
// columns (and their values) in place.
//
// It is idempotent: it first lists the table's columns and drops only the
// retired ones still present, so a database that no longer has them runs no
// DDL at all (on Postgres, no ACCESS EXCLUSIVE lock on templates at every
// startup). The Postgres statement also says IF EXISTS. The probe and the
// drops run in one transaction, so a failure leaves the table unchanged. No
// other table or column is modified.
func dropTemplateLegacyColumns(ctx context.Context, client *ent.Client) error {
	driver := client.Driver()
	name := driver.Dialect()
	query, ok := templateColumnsQuery[name]
	if !ok {
		return fmt.Errorf("drop retired template columns: unsupported dialect %q", name)
	}
	tx, err := driver.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin retired template column removal: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	present, err := templateColumnsPresent(ctx, tx, query)
	if err != nil {
		return err
	}
	for _, col := range templateLegacyColumns {
		if !present[col] {
			continue
		}
		stmt := "ALTER TABLE templates DROP COLUMN " + col
		if name == dialect.Postgres {
			stmt = "ALTER TABLE templates DROP COLUMN IF EXISTS " + col
		}
		if err := tx.Exec(ctx, stmt, []any{}, nil); err != nil {
			return fmt.Errorf("drop retired template column %s: %w", col, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit retired template column removal: %w", err)
	}
	committed = true
	return nil
}

// templateColumnsPresent runs query (a templateColumnsQuery) and returns the
// templates table's column names as a set.
func templateColumnsPresent(ctx context.Context, tx dialect.Tx, query string) (map[string]bool, error) {
	var rows entsql.Rows
	if err := tx.Query(ctx, query, []any{}, &rows); err != nil {
		return nil, fmt.Errorf("list template columns: %w", err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan template column: %w", err)
		}
		present[col] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("list template columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list template columns: %w", err)
	}
	return present, nil
}
