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
	"strings"
	"time"

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
// active code at a time).
func (s *ChatLinkStore) RegisterCode(ctx context.Context, code, userIdentifier, provider string, ttl time.Duration) error {
	codeHash := hashCode(code)

	// Delete any previous pending code for this provider + user.
	_, _ = s.client.ChatLinkCode.
		Delete().
		Where(
			chatlinkcode.ProviderEQ(provider),
			chatlinkcode.UserIdentifierEQ(userIdentifier),
		).
		Exec(ctx)

	// Insert the new code.
	return s.client.ChatLinkCode.
		Create().
		SetCodeHash(codeHash).
		SetUserIdentifier(userIdentifier).
		SetProvider(provider).
		SetStatus("pending").
		SetExpiresAt(time.Now().Add(ttl)).
		Exec(ctx)
}

// VerifyCode confirms a pending link code. On success it marks the code as
// confirmed, stores the Scion user details, and returns the platform-specific
// user identifier. Returns ("", reason) on failure.
func (s *ChatLinkStore) VerifyCode(ctx context.Context, code, provider, userID, userEmail string) (userIdentifier string, errReason string) {
	codeHash := hashCode(code)

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
		return "", "code_not_found"
	}

	if time.Now().After(row.ExpiresAt) {
		// Expired — delete and report.
		_ = s.client.ChatLinkCode.DeleteOneID(row.ID).Exec(ctx)
		return "", "code_expired"
	}

	if row.Status == "confirmed" {
		// Already confirmed — return the identifier.
		return row.UserIdentifier, ""
	}

	// Mark as confirmed and store the verifying user's details.
	_ = s.client.ChatLinkCode.
		UpdateOneID(row.ID).
		SetStatus("confirmed").
		SetUserID(userID).
		SetUserEmail(userEmail).
		Exec(ctx)

	return row.UserIdentifier, ""
}

// GetStatusByUser returns the linking status for a provider-specific user
// identifier. Returns (status, scionUserID, scionUserEmail).
func (s *ChatLinkStore) GetStatusByUser(ctx context.Context, provider, userIdentifier string) (status, userID, userEmail string) {
	row, err := s.client.ChatLinkCode.
		Query().
		Where(
			chatlinkcode.ProviderEQ(provider),
			chatlinkcode.UserIdentifierEQ(userIdentifier),
		).
		Only(ctx)
	if err != nil {
		return "not_found", "", ""
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
	return row.Status, uid, email
}

// ConsumePending removes a confirmed entry so it isn't returned again.
func (s *ChatLinkStore) ConsumePending(ctx context.Context, provider, userIdentifier string) {
	_, _ = s.client.ChatLinkCode.
		Delete().
		Where(
			chatlinkcode.ProviderEQ(provider),
			chatlinkcode.UserIdentifierEQ(userIdentifier),
		).
		Exec(ctx)
}

// PurgeExpired deletes all link codes where expires_at < now.
func (s *ChatLinkStore) PurgeExpired(ctx context.Context) error {
	_, err := s.client.ChatLinkCode.
		Delete().
		Where(chatlinkcode.ExpiresAtLT(time.Now())).
		Exec(ctx)
	return err
}
