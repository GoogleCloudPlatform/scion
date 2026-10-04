package hub

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type store struct{ db *sql.DB }

type topic struct {
	ID        string
	CreatedAt time.Time
}

const touchQuery = `INSERT INTO webchat_thread (user_id, last_activity_at) VALUES (?, ?)`

func (s *store) clean(ctx context.Context, userID string, at time.Time, tp topic) error {
	if _, err := s.db.ExecContext(ctx, touchQuery, userID, at.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	nowT := time.Now().UTC()
	now := nowT.Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO webchat_topic (id, created_at) VALUES (?, ?)`, tp.ID, now); err != nil {
		return err
	}
	// The ent conversations table takes the time.Time.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET last_activity_at = ?, created_at = ?
		 WHERE id = ?`,
		nowT, nowT, tp.ID); err != nil {
		return err
	}
	query := `SELECT id FROM messages WHERE created > ?`
	args := []any{tp.CreatedAt.UTC()}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	return rows.Close()
}

func (s *store) violations(ctx context.Context, userID string, at time.Time, tp topic) error {
	// A raw time.Time into a SQLite webchat TEXT column.
	if _, err := s.db.ExecContext(ctx, touchQuery, userID, at); err != nil { // want webchat-bind-time
		return err
	}
	if _, err := s.db.Exec(`UPDATE webchat_topic SET created_at = ? WHERE id = ?`, tp.CreatedAt, tp.ID); err != nil { // want webchat-bind-time
		return err
	}
	// A formatted string into the ent conversations table.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET last_activity_at = ?, created_at = ? WHERE id = ?`,
		now, now, tp.ID); err != nil { // want ent-bind-formatted
		return err
	}
	query := "SELECT id FROM messages WHERE 1=1"
	var args []any
	query += " AND created < ?"
	args = append(args, tp.CreatedAt.UTC().Format(time.RFC3339Nano)) // want ent-bind-formatted
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	_ = rows.Close()
	q := fmt.Sprintf("DELETE FROM agents WHERE created < ? AND kind = '%s'", "x")
	_, err = s.db.ExecContext(ctx, q, time.Now().UTC().Format(time.RFC3339)) // want ent-bind-formatted
	return err
}
