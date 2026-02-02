// Package sqlite provides a SQLite implementation of the Store interface.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/store"
)

// ============================================================================
// Host Secret Operations
// ============================================================================

// CreateHostSecret creates a new host secret record.
func (s *SQLiteStore) CreateHostSecret(ctx context.Context, secret *store.HostSecret) error {
	if secret.HostID == "" {
		return store.ErrInvalidInput
	}

	now := time.Now()
	if secret.CreatedAt.IsZero() {
		secret.CreatedAt = now
	}
	if secret.Algorithm == "" {
		secret.Algorithm = store.HostSecretAlgorithmHMACSHA256
	}
	if secret.Status == "" {
		secret.Status = store.HostSecretStatusActive
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_secrets (
			host_id, secret_key, algorithm,
			created_at, rotated_at, expires_at, status
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		secret.HostID, secret.SecretKey, secret.Algorithm,
		secret.CreatedAt, nullableTime(secret.RotatedAt), nullableTime(secret.ExpiresAt), secret.Status,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") ||
			strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

// GetHostSecret retrieves a host secret by host ID.
func (s *SQLiteStore) GetHostSecret(ctx context.Context, hostID string) (*store.HostSecret, error) {
	secret := &store.HostSecret{}
	var rotatedAt, expiresAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT host_id, secret_key, algorithm,
			created_at, rotated_at, expires_at, status
		FROM host_secrets WHERE host_id = ?
	`, hostID).Scan(
		&secret.HostID, &secret.SecretKey, &secret.Algorithm,
		&secret.CreatedAt, &rotatedAt, &expiresAt, &secret.Status,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if rotatedAt.Valid {
		secret.RotatedAt = rotatedAt.Time
	}
	if expiresAt.Valid {
		secret.ExpiresAt = expiresAt.Time
	}

	return secret, nil
}

// GetActiveSecrets retrieves all active and deprecated secrets for a host.
// This supports dual-secret validation during rotation grace periods.
func (s *SQLiteStore) GetActiveSecrets(ctx context.Context, hostID string) ([]*store.HostSecret, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT host_id, secret_key, algorithm,
			created_at, rotated_at, expires_at, status
		FROM host_secrets
		WHERE host_id = ? AND status IN (?, ?)
		ORDER BY created_at DESC
	`, hostID, store.HostSecretStatusActive, store.HostSecretStatusDeprecated)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var secrets []*store.HostSecret
	for rows.Next() {
		secret := &store.HostSecret{}
		var rotatedAt, expiresAt sql.NullTime

		if err := rows.Scan(
			&secret.HostID, &secret.SecretKey, &secret.Algorithm,
			&secret.CreatedAt, &rotatedAt, &expiresAt, &secret.Status,
		); err != nil {
			return nil, err
		}

		if rotatedAt.Valid {
			secret.RotatedAt = rotatedAt.Time
		}
		if expiresAt.Valid {
			secret.ExpiresAt = expiresAt.Time
		}

		secrets = append(secrets, secret)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return secrets, nil
}

// UpdateHostSecret updates an existing host secret.
func (s *SQLiteStore) UpdateHostSecret(ctx context.Context, secret *store.HostSecret) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE host_secrets SET
			secret_key = ?,
			algorithm = ?,
			rotated_at = ?,
			expires_at = ?,
			status = ?
		WHERE host_id = ?
	`,
		secret.SecretKey, secret.Algorithm,
		nullableTime(secret.RotatedAt), nullableTime(secret.ExpiresAt), secret.Status,
		secret.HostID,
	)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteHostSecret removes a host secret.
func (s *SQLiteStore) DeleteHostSecret(ctx context.Context, hostID string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM host_secrets WHERE host_id = ?
	`, hostID)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ============================================================================
// Host Join Token Operations
// ============================================================================

// CreateJoinToken creates a new join token for host registration.
func (s *SQLiteStore) CreateJoinToken(ctx context.Context, token *store.HostJoinToken) error {
	if token.HostID == "" || token.TokenHash == "" {
		return store.ErrInvalidInput
	}

	now := time.Now()
	if token.CreatedAt.IsZero() {
		token.CreatedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_join_tokens (
			host_id, token_hash, expires_at, created_at, created_by
		) VALUES (?, ?, ?, ?, ?)
	`,
		token.HostID, token.TokenHash, token.ExpiresAt, token.CreatedAt, token.CreatedBy,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return store.ErrNotFound
		}
		return err
	}
	return nil
}

// GetJoinToken retrieves a join token by token hash.
func (s *SQLiteStore) GetJoinToken(ctx context.Context, tokenHash string) (*store.HostJoinToken, error) {
	token := &store.HostJoinToken{}

	err := s.db.QueryRowContext(ctx, `
		SELECT host_id, token_hash, expires_at, created_at, created_by
		FROM host_join_tokens WHERE token_hash = ?
	`, tokenHash).Scan(
		&token.HostID, &token.TokenHash, &token.ExpiresAt, &token.CreatedAt, &token.CreatedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return token, nil
}

// GetJoinTokenByHostID retrieves a join token by host ID.
func (s *SQLiteStore) GetJoinTokenByHostID(ctx context.Context, hostID string) (*store.HostJoinToken, error) {
	token := &store.HostJoinToken{}

	err := s.db.QueryRowContext(ctx, `
		SELECT host_id, token_hash, expires_at, created_at, created_by
		FROM host_join_tokens WHERE host_id = ?
	`, hostID).Scan(
		&token.HostID, &token.TokenHash, &token.ExpiresAt, &token.CreatedAt, &token.CreatedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return token, nil
}

// DeleteJoinToken removes a join token by host ID.
func (s *SQLiteStore) DeleteJoinToken(ctx context.Context, hostID string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM host_join_tokens WHERE host_id = ?
	`, hostID)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// CleanExpiredJoinTokens removes all expired join tokens.
func (s *SQLiteStore) CleanExpiredJoinTokens(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM host_join_tokens WHERE expires_at < ?
	`, time.Now())
	return err
}
