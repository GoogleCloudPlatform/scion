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

// dropTemplateLegacyColumns physically removes the retired templates.config
// and templates.image columns after schema migration. AutoMigrate runs with
// migrate.WithDropColumn(false), so removing the ent fields alone leaves the
// columns (and their values) in place.
//
// It is idempotent: on Postgres each column is dropped with DROP COLUMN IF
// EXISTS; on SQLite, which has no IF EXISTS form, each column is dropped only
// when pragma_table_info lists it. Both drops run in one transaction, so a
// failure leaves the table unchanged. No other table or column is modified.
func dropTemplateLegacyColumns(ctx context.Context, client *ent.Client) error {
	driver := client.Driver()
	name := driver.Dialect()
	switch name {
	case dialect.SQLite, dialect.Postgres:
	default:
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
	drop := templateLegacyColumns
	if name == dialect.SQLite {
		if drop, err = sqliteTemplateLegacyColumnsPresent(ctx, tx); err != nil {
			return err
		}
	}
	for _, col := range drop {
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

// sqliteTemplateLegacyColumnsPresent returns the templateLegacyColumns that
// the SQLite templates table still has, in templateLegacyColumns order.
func sqliteTemplateLegacyColumnsPresent(ctx context.Context, tx dialect.Tx) ([]string, error) {
	var rows entsql.Rows
	if err := tx.Query(ctx, "SELECT name FROM pragma_table_info('templates')", []any{}, &rows); err != nil {
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
	var out []string
	for _, col := range templateLegacyColumns {
		if present[col] {
			out = append(out, col)
		}
	}
	return out, nil
}
