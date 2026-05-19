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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

var (
	ErrInviteNotFound      = errors.New("invite code not found")
	ErrInviteExpired       = errors.New("invite code has expired")
	ErrInviteRevoked       = errors.New("invite code has been revoked")
	ErrInviteExhausted     = errors.New("invite code has reached its maximum uses")
	ErrInviteInvalidFormat = errors.New("invalid invite code format")
	ErrInviteExpiryTooLong = errors.New("invite code expiry exceeds maximum (5 days)")
)

// InviteService handles invite code generation, validation, and redemption.
type InviteService struct {
	invites   store.InviteCodeStore
	allowList store.AllowListStore
}

// NewInviteService creates a new invite service.
func NewInviteService(invites store.InviteCodeStore, allowList store.AllowListStore) *InviteService {
	return &InviteService{
		invites:   invites,
		allowList: allowList,
	}
}

// CreateInvite generates a new invite code.
// Returns the plaintext code (shown only once) and the stored metadata.
func (s *InviteService) CreateInvite(ctx context.Context, createdBy string, expiresAt time.Time, maxUses int, note string) (string, *store.InviteCode, error) {
	now := time.Now()

	if expiresAt.Before(now) {
		return "", nil, fmt.Errorf("expiry must be in the future")
	}
	if expiresAt.After(now.Add(store.InviteCodeMaxExpiry)) {
		return "", nil, ErrInviteExpiryTooLong
	}

	randomBytes := make([]byte, store.InviteCodeRandomBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", nil, fmt.Errorf("failed to generate random bytes: %w", err)
	}

	keyBody := base64.RawURLEncoding.EncodeToString(randomBytes)
	fullCode := store.InviteCodePrefix + keyBody

	prefix := fullCode[:len(store.InviteCodePrefix)+store.InviteCodePrefixLength]

	hash := sha256.Sum256([]byte(fullCode))
	hashStr := hex.EncodeToString(hash[:])

	invite := &store.InviteCode{
		ID:         uuid.New().String(),
		CodeHash:   hashStr,
		CodePrefix: prefix,
		MaxUses:    maxUses,
		ExpiresAt:  expiresAt,
		CreatedBy:  createdBy,
		Note:       note,
		Created:    now,
	}

	if err := s.invites.CreateInviteCode(ctx, invite); err != nil {
		return "", nil, fmt.Errorf("failed to create invite code: %w", err)
	}

	return fullCode, invite, nil
}

// ValidateCode checks that an invite code is valid (exists, not expired, not revoked, not exhausted).
func (s *InviteService) ValidateCode(ctx context.Context, code string) (*store.InviteCode, error) {
	if !strings.HasPrefix(code, store.InviteCodePrefix) {
		return nil, ErrInviteInvalidFormat
	}

	hash := sha256.Sum256([]byte(code))
	hashStr := hex.EncodeToString(hash[:])

	invite, err := s.invites.GetInviteCodeByHash(ctx, hashStr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInviteNotFound
		}
		return nil, fmt.Errorf("failed to look up invite: %w", err)
	}

	if invite.Revoked {
		return nil, ErrInviteRevoked
	}

	if time.Now().After(invite.ExpiresAt) {
		return nil, ErrInviteExpired
	}

	if invite.MaxUses > 0 && invite.UseCount >= invite.MaxUses {
		return nil, ErrInviteExhausted
	}

	return invite, nil
}

// RedeemCode validates an invite code and adds the email to the allow list.
// Returns nil if the email is already on the allow list (idempotent).
func (s *InviteService) RedeemCode(ctx context.Context, code, email, userID string) (*store.InviteCode, error) {
	invite, err := s.ValidateCode(ctx, code)
	if err != nil {
		return nil, err
	}

	emailLower := strings.ToLower(email)

	// Atomically claim a use slot before modifying the allow list.
	// IncrementInviteUseCount uses a conditional UPDATE (WHERE use_count < max_uses)
	// so only one concurrent request can claim the last slot.
	if err := s.invites.IncrementInviteUseCount(ctx, invite.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInviteExhausted
		}
		return nil, fmt.Errorf("failed to increment use count: %w", err)
	}

	entry := &store.AllowListEntry{
		ID:       uuid.New().String(),
		Email:    emailLower,
		Note:     fmt.Sprintf("Added via invite code %s", invite.CodePrefix),
		AddedBy:  userID,
		InviteID: invite.ID,
	}

	if err := s.allowList.AddAllowListEntry(ctx, entry); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			// Already on the allow list — idempotent success.
			return invite, nil
		}
		return nil, fmt.Errorf("failed to add to allow list: %w", err)
	}

	invite.UseCount++
	return invite, nil
}
