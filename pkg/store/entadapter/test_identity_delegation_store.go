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

	"entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/testidentitydelegation"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// TestIdentityDelegationStore implements store.TestIdentityDelegationStore
// using Ent. It is the only writer of the test_identity_delegations table.
type TestIdentityDelegationStore struct {
	client *ent.Client
}

// NewTestIdentityDelegationStore creates an Ent-backed
// TestIdentityDelegationStore.
func NewTestIdentityDelegationStore(client *ent.Client) *TestIdentityDelegationStore {
	return &TestIdentityDelegationStore{client: client}
}

func entTestIdentityDelegationToStore(d *ent.TestIdentityDelegation) *store.TestIdentityDelegation {
	return &store.TestIdentityDelegation{
		ID:          d.ID.String(),
		GrantorID:   d.GrantorID,
		WorkloadSub: d.WorkloadSub,
		Scope:       d.Scope,
		MemberCap:   d.MemberCap,
		Label:       d.Label,
		Created:     d.CreatedAt,
		ExpiresAt:   d.ExpiresAt,
		RevokedAt:   d.RevokedAt,
		RevokedBy:   d.RevokedBy,
	}
}

// CreateTestIdentityDelegation creates a delegation. See
// store.TestIdentityDelegationStore.
func (s *TestIdentityDelegationStore) CreateTestIdentityDelegation(ctx context.Context, d *store.TestIdentityDelegation) error {
	if d == nil {
		return fmt.Errorf("%w: nil delegation", store.ErrInvalidInput)
	}
	switch {
	case strings.TrimSpace(d.GrantorID) == "":
		return fmt.Errorf("%w: a delegation requires a grantor", store.ErrInvalidInput)
	case strings.TrimSpace(d.WorkloadSub) == "":
		return fmt.Errorf("%w: a delegation requires a workload subject", store.ErrInvalidInput)
	case strings.TrimSpace(d.Scope) == "" || strings.ContainsAny(d.Scope, " ,"):
		return fmt.Errorf("%w: a delegation requires exactly one scope", store.ErrInvalidInput)
	case d.MemberCap <= 0:
		return fmt.Errorf("%w: a delegation requires a positive member cap", store.ErrInvalidInput)
	case d.RevokedAt != nil || d.RevokedBy != nil:
		return fmt.Errorf("%w: a new delegation cannot be revoked", store.ErrInvalidInput)
	}
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	id, err := parseUUID(d.ID)
	if err != nil {
		return err
	}
	if d.Created.IsZero() {
		d.Created = time.Now()
	}
	if !d.ExpiresAt.After(d.Created) {
		return fmt.Errorf("%w: a delegation must expire after it is created", store.ErrInvalidInput)
	}
	_, err = s.client.TestIdentityDelegation.Create().
		SetID(id).
		SetGrantorID(d.GrantorID).
		SetWorkloadSub(d.WorkloadSub).
		SetScope(d.Scope).
		SetMemberCap(d.MemberCap).
		SetLabel(d.Label).
		SetCreatedAt(d.Created).
		SetExpiresAt(d.ExpiresAt).
		Save(ctx)
	return mapError(err)
}

// GetTestIdentityDelegation returns a delegation by ID.
func (s *TestIdentityDelegationStore) GetTestIdentityDelegation(ctx context.Context, id string) (*store.TestIdentityDelegation, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, store.ErrNotFound
	}
	row, err := s.client.TestIdentityDelegation.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entTestIdentityDelegationToStore(row), nil
}

// ListTestIdentityDelegations returns delegations, newest first.
func (s *TestIdentityDelegationStore) ListTestIdentityDelegations(ctx context.Context, grantorID string, limit int) ([]store.TestIdentityDelegation, error) {
	q := s.client.TestIdentityDelegation.Query()
	if grantorID != "" {
		q = q.Where(testidentitydelegation.GrantorIDEQ(grantorID))
	}
	q = q.Order(testidentitydelegation.ByCreatedAt(sql.OrderDesc()), testidentitydelegation.ByID())
	if limit > 0 {
		q = q.Limit(limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.TestIdentityDelegation, 0, len(rows))
	for _, r := range rows {
		out = append(out, *entTestIdentityDelegationToStore(r))
	}
	return out, nil
}

// RevokeTestIdentityDelegation revokes a delegation once.
func (s *TestIdentityDelegationStore) RevokeTestIdentityDelegation(ctx context.Context, id, revokedBy string, at time.Time) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return store.ErrNotFound
	}
	if strings.TrimSpace(revokedBy) == "" {
		return fmt.Errorf("%w: revocation requires the revoking principal", store.ErrInvalidInput)
	}
	n, err := s.client.TestIdentityDelegation.Update().
		Where(testidentitydelegation.IDEQ(uid), testidentitydelegation.RevokedAtIsNil()).
		SetRevokedAt(at).
		SetRevokedBy(revokedBy).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if n > 0 {
		return nil
	}
	// Nothing updated: already revoked (no change) or unknown.
	exists, err := s.client.TestIdentityDelegation.Query().Where(testidentitydelegation.IDEQ(uid)).Exist(ctx)
	if err != nil {
		return mapError(err)
	}
	if !exists {
		return store.ErrNotFound
	}
	return nil
}

// CountLiveTestFixtureUsersByDelegation counts live fixtures issued
// through one delegation.
func (s *TestIdentityDelegationStore) CountLiveTestFixtureUsersByDelegation(ctx context.Context, delegationID string, now time.Time) (int, error) {
	if delegationID == "" {
		return 0, fmt.Errorf("%w: a delegation ID is required", store.ErrInvalidInput)
	}
	n, err := s.client.User.Query().
		Where(user.KindEQ(user.KindTestFixture), user.IssuerDelegationIDEQ(delegationID), user.ExpiresAtGT(now)).
		Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// ListTestFixtureUsersByDelegation lists fixtures issued through one
// delegation, newest first.
func (s *TestIdentityDelegationStore) ListTestFixtureUsersByDelegation(ctx context.Context, delegationID string, liveAt time.Time, limit int) ([]store.User, error) {
	if delegationID == "" {
		return nil, fmt.Errorf("%w: a delegation ID is required", store.ErrInvalidInput)
	}
	q := s.client.User.Query().Where(user.KindEQ(user.KindTestFixture), user.IssuerDelegationIDEQ(delegationID))
	if !liveAt.IsZero() {
		q = q.Where(user.ExpiresAtGT(liveAt))
	}
	q = q.Order(user.ByCreated(sql.OrderDesc()), user.ByID())
	if limit > 0 {
		q = q.Limit(limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, *entUserToStore(r))
	}
	return out, nil
}
