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

package entadapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/testidentitylogincode"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ReplaceTestIdentityLoginCode stores code as the user's only web-login
// code. See store.UserStore.ReplaceTestIdentityLoginCode.
func (s *UserStore) ReplaceTestIdentityLoginCode(ctx context.Context, code *store.TestIdentityLoginCode) error {
	if code == nil || strings.TrimSpace(code.CodeHash) == "" || strings.TrimSpace(code.IssuedBy) == "" || code.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: a test identity login code requires a user, a code hash, an issuer and an expiry", store.ErrInvalidInput)
	}
	uid, err := parseUUID(code.UserID)
	if err != nil {
		return err
	}
	id := uuid.New()
	if code.ID != "" {
		if id, err = parseUUID(code.ID); err != nil {
			return err
		}
	}
	if code.Created.IsZero() {
		code.Created = time.Now()
	}
	if _, err := s.client.TestIdentityLoginCode.Delete().
		Where(testidentitylogincode.UserIDEQ(uid)).
		Exec(ctx); err != nil {
		return mapError(err)
	}
	if _, err := s.client.TestIdentityLoginCode.Create().
		SetID(id).
		SetUserID(uid).
		SetCodeHash(code.CodeHash).
		SetIssuedBy(code.IssuedBy).
		SetIssuerCredentialID(code.IssuerCredentialID).
		SetExpiresAt(code.ExpiresAt.UTC()).
		SetCreatedAt(code.Created.UTC()).
		Save(ctx); err != nil {
		return mapError(err)
	}
	code.ID = id.String()
	code.ConsumedAt = nil
	return nil
}

// ConsumeTestIdentityLoginCode redeems the code with the given hash at most
// once. See store.UserStore.ConsumeTestIdentityLoginCode.
func (s *UserStore) ConsumeTestIdentityLoginCode(ctx context.Context, codeHash string, now time.Time) (*store.TestIdentityLoginCode, error) {
	if strings.TrimSpace(codeHash) == "" {
		return nil, store.ErrNotFound
	}
	now = now.UTC()
	// The conditional update is the single-use guarantee: the WHERE clause
	// is re-checked by the database for each writer, so of two concurrent
	// redeems at most one changes the row.
	n, err := s.client.TestIdentityLoginCode.Update().
		Where(
			testidentitylogincode.CodeHashEQ(codeHash),
			testidentitylogincode.ConsumedAtIsNil(),
			testidentitylogincode.ExpiresAtGT(now),
		).
		SetConsumedAt(now).
		Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	row, err := s.client.TestIdentityLoginCode.Query().
		Where(testidentitylogincode.CodeHashEQ(codeHash)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, mapError(err)
	}
	if n != 1 {
		if row.ConsumedAt != nil {
			return nil, store.ErrTestIdentityLoginCodeConsumed
		}
		return nil, store.ErrTestIdentityLoginCodeExpired
	}
	return entTestIdentityLoginCodeToStore(row), nil
}

func entTestIdentityLoginCodeToStore(r *ent.TestIdentityLoginCode) *store.TestIdentityLoginCode {
	return &store.TestIdentityLoginCode{
		ID:                 r.ID.String(),
		UserID:             r.UserID.String(),
		CodeHash:           r.CodeHash,
		IssuedBy:           r.IssuedBy,
		IssuerCredentialID: r.IssuerCredentialID,
		ExpiresAt:          r.ExpiresAt,
		ConsumedAt:         r.ConsumedAt,
		Created:            r.CreatedAt,
	}
}
