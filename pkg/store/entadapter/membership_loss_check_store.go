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
	"time"
	"unicode/utf8"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/membershiplosscheck"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// maxMembershipLossCheckErrorLen bounds the stored last_error text.
const maxMembershipLossCheckErrorLen = 2000

// truncateUTF8 returns s cut to at most n bytes on a rune boundary
// (PostgreSQL rejects invalid UTF-8).
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// MembershipLossCheckStore implements store.MembershipLossCheckStore using
// Ent ORM.
type MembershipLossCheckStore struct {
	client *ent.Client
	inTx   bool // true when client wraps an ambient WithTx transaction
}

// NewMembershipLossCheckStore creates a new Ent-backed
// MembershipLossCheckStore.
func NewMembershipLossCheckStore(client *ent.Client) *MembershipLossCheckStore {
	return &MembershipLossCheckStore{client: client}
}

var _ store.MembershipLossCheckStore = (*MembershipLossCheckStore)(nil)

// entMembershipLossCheckToStore converts an Ent MembershipLossCheck to a
// store model.
func entMembershipLossCheckToStore(c *ent.MembershipLossCheck) *store.MembershipLossCheck {
	out := &store.MembershipLossCheck{
		ID:            c.ID.String(),
		UserID:        c.UserID,
		Trigger:       store.MembershipLossTrigger(c.Trigger),
		ActorKind:     c.ActorKind,
		ActorID:       c.ActorID,
		CorrelationID: c.CorrelationID,
		CreatedAt:     c.CreatedAt,
		Attempts:      c.Attempts,
		LastError:     c.LastError,
		LeaseUntil:    c.LeaseUntil,
	}
	if c.ProjectID != nil {
		out.ProjectID = *c.ProjectID
	}
	return out
}

// EnqueueMembershipLossCheck inserts check.
func (s *MembershipLossCheckStore) EnqueueMembershipLossCheck(ctx context.Context, check *store.MembershipLossCheck) error {
	if check == nil || check.UserID == "" {
		return fmt.Errorf("%w: membership loss check requires a user", store.ErrInvalidInput)
	}
	if !store.ValidMembershipLossTrigger(check.Trigger) {
		return fmt.Errorf("%w: unknown membership loss trigger %q", store.ErrInvalidInput, check.Trigger)
	}
	userID, err := parseUUID(check.UserID)
	if err != nil {
		return err
	}
	var projectID *string
	if check.ProjectID != "" {
		pid, err := parseUUID(check.ProjectID)
		if err != nil {
			return err
		}
		canonical := pid.String()
		projectID = &canonical
	}
	id := uuid.New()
	if check.ID != "" {
		if id, err = parseUUID(check.ID); err != nil {
			return err
		}
	}
	if check.CreatedAt.IsZero() {
		check.CreatedAt = time.Now()
	}
	_, err = s.client.MembershipLossCheck.Create().
		SetID(id).
		SetUserID(userID.String()).
		SetNillableProjectID(projectID).
		SetTrigger(membershiplosscheck.Trigger(check.Trigger)).
		SetActorKind(check.ActorKind).
		SetActorID(check.ActorID).
		SetCorrelationID(check.CorrelationID).
		SetCreatedAt(check.CreatedAt).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	check.ID = id.String()
	check.UserID = userID.String()
	if projectID != nil {
		check.ProjectID = *projectID
	}
	return nil
}

// ClaimMembershipLossChecks selects up to limit claimable rows (FOR UPDATE
// SKIP LOCKED on PostgreSQL), leases them and returns them, all in one
// transaction (the ambient one when called inside WithTx).
func (s *MembershipLossCheckStore) ClaimMembershipLossChecks(ctx context.Context, limit int, lease time.Duration) ([]*store.MembershipLossCheck, error) {
	if limit <= 0 || lease <= 0 {
		return nil, fmt.Errorf("%w: claim requires a positive limit and lease", store.ErrInvalidInput)
	}
	if s.inTx {
		return claimMembershipLossChecks(ctx, s.client, limit, lease)
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	claimed, err := claimMembershipLossChecks(ctx, tx.Client(), limit, lease)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

// claimMembershipLossChecks runs the claim on c, which must be a
// transactional client.
func claimMembershipLossChecks(ctx context.Context, c *ent.Client, limit int, lease time.Duration) ([]*store.MembershipLossCheck, error) {
	now := time.Now()
	q := c.MembershipLossCheck.Query().
		Where(membershiplosscheck.Or(
			membershiplosscheck.LeaseUntilIsNil(),
			membershiplosscheck.LeaseUntilLT(now),
		)).
		Order(ent.Asc(membershiplosscheck.FieldCreatedAt), ent.Asc(membershiplosscheck.FieldID)).
		Limit(limit)
	if c.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
	}
	ids, err := q.IDs(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if err := c.MembershipLossCheck.Update().
		Where(membershiplosscheck.IDIn(ids...)).
		SetLeaseUntil(now.Add(lease)).
		AddAttempts(1).
		Exec(ctx); err != nil {
		return nil, mapError(err)
	}
	rows, err := c.MembershipLossCheck.Query().
		Where(membershiplosscheck.IDIn(ids...)).
		Order(ent.Asc(membershiplosscheck.FieldCreatedAt), ent.Asc(membershiplosscheck.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.MembershipLossCheck, len(rows))
	for i, r := range rows {
		out[i] = entMembershipLossCheckToStore(r)
	}
	return out, nil
}

// CompleteMembershipLossCheck deletes the check if it is still held by the
// claim whose Attempts value is claim.
func (s *MembershipLossCheckStore) CompleteMembershipLossCheck(ctx context.Context, id string, claim int) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	n, err := s.client.MembershipLossCheck.Delete().
		Where(membershiplosscheck.IDEQ(uid), membershiplosscheck.AttemptsEQ(claim)).
		Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrClaimLost
	}
	return nil
}

// FailMembershipLossCheck records errText on the check and keeps its lease,
// if the check is still held by the claim whose Attempts value is claim.
func (s *MembershipLossCheckStore) FailMembershipLossCheck(ctx context.Context, id string, claim int, errText string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	errText = truncateUTF8(errText, maxMembershipLossCheckErrorLen)
	n, err := s.client.MembershipLossCheck.Update().
		Where(membershiplosscheck.IDEQ(uid), membershiplosscheck.AttemptsEQ(claim)).
		SetLastError(errText).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrClaimLost
	}
	return nil
}
