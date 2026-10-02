package hub

import (
	"context"
	"time"
)

// SQL text held in a local variable is resolved at the call: the latest
// assignment that must have run, plus later += fragments and assignments in
// branches that may have run.
func (s *store) sqlVars(ctx context.Context, id string, cond bool) {
	// A := literal followed by += building.
	query := "UPDATE "
	query += "webchat_topic SET last_activity_at = ? WHERE id = ?"
	_, _ = s.db.ExecContext(ctx, query, time.Now(), id) // want webchat-bind-time

	// The same with var and =.
	var q2 string
	q2 = "UPDATE "
	q2 += "webchat_topic SET last_activity_at = ? WHERE id = ?"
	_, _ = s.db.ExecContext(ctx, q2, time.Now(), id) // want webchat-bind-time

	// Reassignment: each call sees the statement assigned last.
	stmt := `UPDATE conversations SET last_activity_at = ? WHERE id = ?`
	_, _ = s.db.ExecContext(ctx, stmt, time.Now().UTC(), id)
	stmt = `UPDATE webchat_topic SET last_activity_at = ? WHERE id = ?`
	_, _ = s.db.ExecContext(ctx, stmt, time.Now(), id) // want webchat-bind-time
	_, _ = s.db.ExecContext(ctx, stmt, time.Now().UTC().Format(time.RFC3339Nano), id)
	stmt = `UPDATE conversations SET last_activity_at = ? WHERE id = ?`
	_, _ = s.db.ExecContext(ctx, stmt, time.Now().UTC().Format(time.RFC3339Nano), id) // want ent-bind-formatted

	// Self-referencing build.
	sel := "UPDATE webchat_thread SET last_activity_at = ?"
	sel = sel + " WHERE id = ?"
	_, _ = s.db.ExecContext(ctx, sel, time.Now(), id) // want webchat-bind-time

	// An assignment in a branch may have run, so both tables are checked.
	br := `UPDATE conversations SET updated_at = ? WHERE id = ?`
	if cond {
		br = `UPDATE webchat_topic SET updated_at = ? WHERE id = ?`
	}
	_, _ = s.db.ExecContext(ctx, br, time.Now(), id) // want webchat-bind-time

	// Replaced by an unresolvable value: skipped, not misread as the literal.
	dyn := `UPDATE webchat_topic SET updated_at = ? WHERE id = ?`
	dyn = s.build()
	_, _ = s.db.ExecContext(ctx, dyn, time.Now(), id)
}

func (s *store) build() string { return "" }

// A zero time.Time is UTC, so formatting it needs no conversion.
func zeroTime(raw string) string {
	var last time.Time
	return last.Format(time.RFC3339Nano)
}

// A *time.Time is not known to be UTC.
func zeroPtr() string {
	var last *time.Time
	return last.Format(time.RFC3339Nano) // want format-utc
}
