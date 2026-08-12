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

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/chatlinkcode"
)

// ChatLinkStore is a database-backed store for chat account-link codes.
// It replaces the per-instance in-memory maps so that codes registered on
// one Hub instance can be verified on another.
type ChatLinkStore struct {
	client *ent.Client
}

// NewChatLinkStore creates a new ChatLinkStore backed by the given ent client.
func NewChatLinkStore(client *ent.Client) *ChatLinkStore {
	return &ChatLinkStore{client: client}
}

// hashCode returns the hex-encoded SHA-256 hash of the uppercased code.
func hashCode(code string) string {
	h := sha256.Sum256([]byte(strings.ToUpper(code)))
	return hex.EncodeToString(h[:])
}

// RegisterCode stores a pending link code. Any existing pending code for the
// same provider+userIdentifier is removed first (a user can only have one
// active code at a time). The delete + insert are wrapped in a transaction
// so that a crash between the two cannot leave the user without a code.
func (s *ChatLinkStore) RegisterCode(ctx context.Context, code, userIdentifier string, provider chatlinkcode.Provider, ttl time.Duration) error {
	codeHash := hashCode(code)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("chat link register: begin tx: %w", err)
	}

	// Delete any previous non-confirmed code for this provider + user.
	// Confirmed codes represent completed account links and must not be
	// removed when the user requests a new link code.
	if _, err := tx.ChatLinkCode.
		Delete().
		Where(
			chatlinkcode.ProviderEQ(provider),
			chatlinkcode.UserIdentifierEQ(userIdentifier),
			chatlinkcode.StatusNEQ(chatlinkcode.StatusConfirmed),
		).
		Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("chat link register: delete old codes: %w", err)
	}

	// Insert the new code.
	if err := tx.ChatLinkCode.
		Create().
		SetCodeHash(codeHash).
		SetUserIdentifier(userIdentifier).
		SetProvider(provider).
		SetStatus(chatlinkcode.StatusPending).
		SetExpiresAt(time.Now().Add(ttl)).
		Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("chat link register: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("chat link register: commit: %w", err)
	}
	return nil
}

// VerifyCode confirms a pending link code. On success it marks the code as
// confirmed, stores the Scion user details, and returns the platform-specific
// user identifier.
//
// The state transition uses a conditional UPDATE (WHERE status='pending')
// to prevent a TOCTOU race: two concurrent verifications on different
// instances cannot both succeed.
//
// Returns:
//   - (userIdentifier, "")       — success
//   - ("", "code_not_found")     — no matching code found
//   - ("", "code_expired")       — code exists but is expired
//   - ("", "db_error")           — a database error occurred (callers may fall back)
func (s *ChatLinkStore) VerifyCode(ctx context.Context, code string, provider chatlinkcode.Provider, userID, userEmail string) (userIdentifier string, errReason string) {
	codeHash := hashCode(code)

	// First, look up the code to get its metadata.
	row, err := s.client.ChatLinkCode.
		Query().
		Where(
			chatlinkcode.CodeHashEQ(codeHash),
			chatlinkcode.ProviderEQ(provider),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return "", "code_not_found"
		}
		// Genuine DB error — return a distinct reason so callers can
		// distinguish this from a legitimate not-found and optionally
		// fall back to in-memory.
		return "", "db_error"
	}

	if time.Now().After(row.ExpiresAt) {
		// Expired — delete and report.
		_ = s.client.ChatLinkCode.DeleteOneID(row.ID).Exec(ctx)
		return "", "code_expired"
	}

	if row.Status == chatlinkcode.StatusConfirmed {
		// Already confirmed — return the identifier.
		return row.UserIdentifier, ""
	}

	// Atomic conditional UPDATE: only transition pending → confirmed.
	// If another instance raced us, n will be 0.
	n, err := s.client.ChatLinkCode.
		Update().
		Where(
			chatlinkcode.IDEQ(row.ID),
			chatlinkcode.StatusEQ(chatlinkcode.StatusPending),
		).
		SetStatus(chatlinkcode.StatusConfirmed).
		SetUserID(userID).
		SetUserEmail(userEmail).
		Save(ctx)
	if err != nil {
		return "", "db_error"
	}
	if n == 0 {
		// Another instance confirmed this code between our read and update.
		// The code is consumed; treat as not found for this caller.
		return "", "code_not_found"
	}

	return row.UserIdentifier, ""
}

// GetStatusByUser returns the linking status for a provider-specific user
// identifier. Returns (status, scionUserID, scionUserEmail).
// On genuine DB errors, returns ("db_error", "", "") so callers can fall back.
//
// When multiple rows exist for the same provider+user (e.g. a confirmed code
// and a new pending code), the most recently created entry is returned.
func (s *ChatLinkStore) GetStatusByUser(ctx context.Context, provider chatlinkcode.Provider, userIdentifier string) (status, userID, userEmail string) {
	row, err := s.client.ChatLinkCode.
		Query().
		Where(
			chatlinkcode.ProviderEQ(provider),
			chatlinkcode.UserIdentifierEQ(userIdentifier),
		).
		Order(chatlinkcode.ByCreatedAt(entsql.OrderDesc())).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return "not_found", "", ""
		}
		return "db_error", "", ""
	}

	if time.Now().After(row.ExpiresAt) {
		return "expired", "", ""
	}

	uid := ""
	if row.UserID != nil {
		uid = *row.UserID
	}
	email := ""
	if row.UserEmail != nil {
		email = *row.UserEmail
	}
	return string(row.Status), uid, email
}

// ConsumePending removes a confirmed entry so it isn't returned again.
func (s *ChatLinkStore) ConsumePending(ctx context.Context, provider chatlinkcode.Provider, userIdentifier string) {
	_, err := s.client.ChatLinkCode.
		Delete().
		Where(
			chatlinkcode.ProviderEQ(provider),
			chatlinkcode.UserIdentifierEQ(userIdentifier),
		).
		Exec(ctx)
	if err != nil {
		slog.Error("chat link: ConsumePending failed",
			"provider", string(provider),
			"user_identifier", userIdentifier,
			"error", err,
		)
	}
}

// PurgeExpired deletes all link codes where expires_at < now.
func (s *ChatLinkStore) PurgeExpired(ctx context.Context) error {
	_, err := s.client.ChatLinkCode.
		Delete().
		Where(chatlinkcode.ExpiresAtLT(time.Now())).
		Exec(ctx)
	return err
}
