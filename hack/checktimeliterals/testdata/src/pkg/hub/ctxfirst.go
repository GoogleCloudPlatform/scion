package hub

import (
	"context"
	"time"
)

type pgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (any, error)
}

type entDriver interface {
	Exec(ctx context.Context, query string, args, v any) error
	Query(ctx context.Context, query string, args, v any) error
}

// ctx-first APIs: pgx Exec(ctx, sql, args...).
func pgxForm(ctx context.Context, p pgxPool, t time.Time, id string) {
	_, _ = p.Exec(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, t.UTC().Format(time.RFC3339Nano), id) // want ent-bind-formatted
	_, _ = p.Exec(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, t.UTC(), id)
	_, _ = p.Exec(ctx, `UPDATE webchat_topic SET updated_at = $1`, t) // want webchat-bind-time
	_, _ = p.Exec(ctx, `UPDATE webchat_topic SET updated_at = $1`, t.UTC().Format(time.RFC3339Nano))
}

// The ent dialect driver: Exec/Query(ctx, sql, args, v), binds in args.
func entDriverForm(ctx context.Context, d entDriver, t time.Time, id string) {
	formatted := t.UTC().Format(time.RFC3339Nano)
	var rows any
	_ = d.Query(ctx, `SELECT id FROM groups WHERE created < ?`, []any{formatted}, &rows) // want ent-bind-formatted
	_ = d.Query(ctx, `SELECT id FROM groups WHERE created < ?`, []any{t.UTC()}, &rows)
	args := []any{id}
	args = append(args, formatted) // want ent-bind-formatted (reported where the bind was appended)
	_ = d.Exec(ctx, `UPDATE groups SET updated_at = ? WHERE id = ?`, args, nil)
	clean := []any{t.UTC(), id}
	_ = d.Exec(ctx, `UPDATE groups SET updated_at = ? WHERE id = ?`, clean, nil)
	_ = d.Exec(ctx, `UPDATE webchat_topic SET updated_at = ? WHERE id = ?`, []any{t, id}, nil) // want webchat-bind-time
	_ = d.Exec(ctx, `UPDATE webchat_topic SET updated_at = ? WHERE id = ?`, []any{formatted, id}, nil)
}
