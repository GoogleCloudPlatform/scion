package hub

import (
	"context"
	"time"
)

// Postgres webchat columns are TIMESTAMPTZ: binding time.Time is correct.
func (s *store) postgresClean(ctx context.Context, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE webchat_thread SET last_activity_at = $1`, at.UTC())
	return err
}

// The ent rule still applies to the Postgres twin.
func (s *store) postgresViolation(ctx context.Context, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET last_activity_at = $1`, at.UTC().Format(time.RFC3339)) // want ent-bind-formatted
	return err
}
