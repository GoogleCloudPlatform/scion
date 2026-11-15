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
	"errors"
	"fmt"
	"slices"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentdeliveryhold"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agenthold"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var _ store.MessageHoldStore = (*BrokerDispatchStore)(nil)

const defaultExpireHeldPageSize = 500

// messageDispatchAwaitingSessionLiteral matches dispatch_state = 'awaiting_session'
// with the value inlined as a literal so SQLite and Postgres generic plans can
// prove the partial index WHERE predicate without parameter knowledge.
func messageDispatchAwaitingSessionLiteral() predicate.Message {
	return func(s *entsql.Selector) {
		s.Where(entsql.ExprP(s.C(message.FieldDispatchState) + " = '" + store.MessageDispatchAwaitingSession + "'"))
	}
}

// holdNow reads the store clock on drv: a test override when set, Postgres
// SELECT now() on drv, or Go UTC wall clock on SQLite.
func (s *BrokerDispatchStore) holdNow(ctx context.Context, drv dialect.Driver) (time.Time, error) {
	if s.holdClock != nil {
		return s.holdClock().UTC(), nil
	}
	if drv.Dialect() != dialect.Postgres {
		return time.Now().UTC(), nil
	}
	var rows entsql.Rows
	if err := drv.Query(ctx, "SELECT now()", []any{}, &rows); err != nil {
		return time.Time{}, fmt.Errorf("message hold store: SELECT now(): %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return time.Time{}, fmt.Errorf("message hold store: SELECT now(): %w", err)
		}
		return time.Time{}, fmt.Errorf("message hold store: SELECT now(): no row")
	}
	var now time.Time
	if err := rows.Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("message hold store: SELECT now(): %w", err)
	}
	return now.UTC(), nil
}

// withHoldTx runs fn in a transaction (or reuses the ambient transaction when
// already inside WithTx), reading the database clock once at transaction start.
func (s *BrokerDispatchStore) withHoldTx(ctx context.Context, fn func(c *ent.Client, now time.Time) error) error {
	if s.inTx {
		now, err := s.holdNow(ctx, s.client.Driver())
		if err != nil {
			return err
		}
		return fn(s.client, now)
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		if errors.Is(err, ent.ErrTxStarted) {
			now, nowErr := s.holdNow(ctx, s.client.Driver())
			if nowErr != nil {
				return nowErr
			}
			return fn(s.client, now)
		}
		return mapError(err)
	}
	defer func() { _ = tx.Rollback() }()

	c := tx.Client()
	now, err := s.holdNow(ctx, c.Driver())
	if err != nil {
		return err
	}
	if err := fn(c, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapError(err)
	}
	return nil
}

func lockAgentForHold(ctx context.Context, c *ent.Client, agentUID uuid.UUID) (*ent.Agent, error) {
	q := c.Agent.Query().Where(agent.IDEQ(agentUID))
	if c.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate()
	}
	row, err := q.Select(
		agent.FieldID,
		agent.FieldPhase,
		agent.FieldRunID,
		agent.FieldDeletedAt,
		agent.FieldDeletionState,
		agent.FieldDeletionLeaseAt,
		agent.FieldReincarnationState,
	).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, mapError(err)
	}
	return row, nil
}

func agentEligibleForHold(ctx context.Context, c *ent.Client, a *ent.Agent, now time.Time) (bool, error) {
	if a == nil || a.DeletedAt != nil {
		return false, nil
	}
	if store.DeletionHoldsRow(a.DeletionState, a.DeletionLeaseAt, now) {
		return false, nil
	}
	if store.ReincarnationInFlight(a.ReincarnationState) {
		return false, nil
	}
	switch a.Phase {
	case "provisioning", "starting", "running":
	default:
		return false, nil
	}
	held, err := c.AgentHold.Query().
		Where(agenthold.AgentIDEQ(a.ID), agenthold.ClearedAtIsNil()).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return !held, nil
}

func lockHoldMarker(ctx context.Context, c *ent.Client, agentID string) (*ent.AgentDeliveryHold, error) {
	q := c.AgentDeliveryHold.Query().Where(agentdeliveryhold.IDEQ(agentID))
	if c.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate()
	}
	m, err := q.Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, mapError(err)
	}
	return m, nil
}

func countHeldMessages(ctx context.Context, c *ent.Client, agentID string) (int, error) {
	n, err := c.Message.Query().
		Where(
			messageDispatchAwaitingSessionLiteral(),
			message.RecipientIDEQ(agentID),
		).
		Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

func markerLeaseActive(m *ent.AgentDeliveryHold, now time.Time) bool {
	return m != nil && m.ClaimOwner != "" && m.ClaimUntil != nil && m.ClaimUntil.After(now)
}

// syncAgentDeliveryHold reconciles agentID's marker row with the live count of
// awaiting_session messages inside c's transaction: deletes the marker when
// zero remain (unless an unexpired re-drive lease is held on the marker, in
// which case HeldCount is set to 0 until ReleaseAgentRedriveLease completes),
// updates held_count and extends hold_until to max(marker.HoldUntil, max
// remaining awaiting_session hold_until) when a later hold episode holds rows
// with a later deadline (RV3-O1, without ever extending individual message
// rows' COALESCE bounds), or creates the marker when absent.
func syncAgentDeliveryHold(ctx context.Context, c *ent.Client, agentID string, fallbackHoldUntil time.Time, launchID string, now time.Time) error {
	count, err := countHeldMessages(ctx, c, agentID)
	if err != nil {
		return err
	}
	marker, err := lockHoldMarker(ctx, c, agentID)
	if err != nil {
		return err
	}
	if count == 0 {
		if marker != nil {
			if markerLeaseActive(marker, now) {
				if marker.HeldCount != 0 {
					if err := c.AgentDeliveryHold.UpdateOneID(agentID).SetHeldCount(0).Exec(ctx); err != nil {
						return mapError(err)
					}
				}
				return nil
			}
			if err := c.AgentDeliveryHold.DeleteOneID(agentID).Exec(ctx); err != nil && !ent.IsNotFound(err) {
				return mapError(err)
			}
		}
		return nil
	}

	maxHoldUntil := fallbackHoldUntil
	latest, qErr := c.Message.Query().
		Where(messageDispatchAwaitingSessionLiteral(), message.RecipientIDEQ(agentID), message.HoldUntilNotNil()).
		Order(ent.Desc(message.FieldHoldUntil)).
		Select(message.FieldHoldUntil).
		First(ctx)
	if qErr == nil && latest.HoldUntil != nil && latest.HoldUntil.After(maxHoldUntil) {
		maxHoldUntil = *latest.HoldUntil
	}

	if marker != nil {
		targetHoldUntil := marker.HoldUntil
		if maxHoldUntil.After(targetHoldUntil) {
			targetHoldUntil = maxHoldUntil
		}
		needLaunchUpdate := launchID != "" && marker.LaunchID != launchID
		if marker.HeldCount != count || !targetHoldUntil.Equal(marker.HoldUntil) || needLaunchUpdate {
			upd := c.AgentDeliveryHold.UpdateOneID(agentID).
				SetHeldCount(count).
				SetHoldUntil(targetHoldUntil)
			if needLaunchUpdate {
				upd = upd.SetLaunchID(launchID)
			}
			if err := upd.Exec(ctx); err != nil {
				return mapError(err)
			}
		}
		return nil
	}
	if maxHoldUntil.IsZero() {
		maxHoldUntil = now
	}
	return mapError(c.AgentDeliveryHold.Create().
		SetID(agentID).
		SetHoldUntil(maxHoldUntil).
		SetLaunchID(launchID).
		SetHeldCount(count).
		SetCreated(now).
		Exec(ctx))
}

func setOrClearHoldLaunchID(u *ent.MessageUpdate, launchID string) *ent.MessageUpdate {
	if launchID != "" {
		return u.SetHoldLaunchID(launchID)
	}
	return u.ClearHoldLaunchID()
}

// syncHoldMarkerAfterDirectFail keeps agent_delivery_holds consistent if
// MarkMessageFailed directly failed an awaiting_session row.
func (s *BrokerDispatchStore) syncHoldMarkerAfterDirectFail(ctx context.Context, msgUID uuid.UUID) {
	row, err := s.client.Message.Query().
		Where(message.IDEQ(msgUID)).
		Select(message.FieldRecipientID).
		Only(ctx)
	if err != nil || row.RecipientID == "" {
		return
	}
	exists, err := s.client.AgentDeliveryHold.Query().
		Where(agentdeliveryhold.IDEQ(row.RecipientID)).
		Exist(ctx)
	if err != nil || !exists {
		return
	}
	_ = s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		if agentUID, pErr := parseUUID(row.RecipientID); pErr == nil {
			if _, err := lockAgentForHold(ctx, c, agentUID); err != nil {
				return err
			}
		}
		return syncAgentDeliveryHold(ctx, c, row.RecipientID, time.Time{}, "", now)
	})
}

// UnbindMessageDeliveryWithProof implements store.MessageHoldStore.
func (s *BrokerDispatchStore) UnbindMessageDeliveryWithProof(ctx context.Context, deliveryID string, messageIDs []string, notReached string) error {
	if notReached == "" {
		return s.UnbindMessageDelivery(ctx, deliveryID, messageIDs)
	}
	if notReached != store.MessageNotReachedNoSession {
		return store.ErrInvalidInput
	}
	uids, err := parseDeliveryUUIDs(messageIDs)
	if err != nil {
		return err
	}
	now, err := s.holdNow(ctx, s.client.Driver())
	if err != nil {
		return err
	}
	_, err = s.client.Message.Update().
		Where(
			message.IDIn(uids...),
			message.DispatchStateEQ(store.MessageDispatchDelivering),
			message.DeliveryIDEQ(deliveryID),
		).
		SetDispatchState(store.MessageDispatchDispatched).
		ClearDeliveryID().
		ClearDeliveringAt().
		SetNotReached(notReached).
		SetNotReachedAt(now).
		Save(ctx)
	return mapError(err)
}

// PrepareOrHoldMessage implements store.MessageHoldStore.
func (s *BrokerDispatchStore) PrepareOrHoldMessage(ctx context.Context, agentID, messageID string, maxHeld int) (store.PrepareOrHoldResult, error) {
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return store.PrepareOrHoldResult{}, err
	}
	msgUID, err := parseUUID(messageID)
	if err != nil {
		return store.PrepareOrHoldResult{}, err
	}

	var out store.PrepareOrHoldResult
	err = s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		agentRow, err := lockAgentForHold(ctx, c, agentUID)
		if err != nil {
			return err
		}
		if agentRow == nil || agentRow.DeletedAt != nil {
			return store.ErrNotFound
		}
		launchID := agentRow.RunID
		out.HoldLaunchID = launchID

		msgRow, err := c.Message.Query().
			Where(message.IDEQ(msgUID), message.RecipientIDEQ(agentID)).
			Select(message.FieldID, message.FieldHoldUntil).
			Only(ctx)
		if err != nil {
			if ent.IsNotFound(err) {
				return store.ErrNotFound
			}
			return mapError(err)
		}

		marker, err := lockHoldMarker(ctx, c, agentID)
		if err != nil {
			return err
		}
		if marker != nil && maxHeld > 0 {
			eligible, err := agentEligibleForHold(ctx, c, agentRow, now)
			if err != nil {
				return err
			}
			if eligible && marker.HoldUntil.After(now) {
				heldCount, err := countHeldMessages(ctx, c, agentID)
				if err != nil {
					return err
				}
				if heldCount < maxHeld {
					holdUntil := marker.HoldUntil
					if msgRow.HoldUntil != nil {
						holdUntil = *msgRow.HoldUntil
					}
					upd := c.Message.Update().
						Where(
							message.IDEQ(msgUID),
							message.RecipientIDEQ(agentID),
							message.DispatchStateIn(store.MessageDispatchPending, store.MessageDispatchDispatched),
							message.DeliveryIDIsNil(),
						).
						SetDispatchState(store.MessageDispatchAwaitingSession).
						SetHoldUntil(holdUntil).
						ClearDispatchedAt()
					upd = setOrClearHoldLaunchID(upd, launchID)
					n, err := upd.Save(ctx)
					if err != nil {
						return mapError(err)
					}
					if n == 1 {
						if err := c.AgentDeliveryHold.UpdateOneID(agentID).
							SetHeldCount(heldCount + 1).
							Exec(ctx); err != nil {
							return mapError(err)
						}
						hu := holdUntil
						out.Held = true
						out.HoldUntil = &hu
						return nil
					}
				}
			}
		}

		// Not held: stamp hold_launch_id so any later broker-reported hold can
		// compare against the run_id captured at send time (R2).
		upd := c.Message.Update().
			Where(message.IDEQ(msgUID), message.RecipientIDEQ(agentID))
		upd = setOrClearHoldLaunchID(upd, launchID)
		if _, err := upd.Save(ctx); err != nil {
			return mapError(err)
		}
		return nil
	})
	if err != nil {
		return store.PrepareOrHoldResult{}, err
	}
	return out, nil
}

// HoldMessageAwaitingSession implements store.MessageHoldStore.
func (s *BrokerDispatchStore) HoldMessageAwaitingSession(ctx context.Context, p store.HoldMessageParams) (bool, error) {
	if p.MessageID == "" || p.AgentID == "" || p.MaxProofAge <= 0 || p.HoldTTL <= 0 || p.MaxHeld <= 0 {
		return false, nil
	}
	agentUID, err := parseUUID(p.AgentID)
	if err != nil {
		return false, err
	}
	msgUID, err := parseUUID(p.MessageID)
	if err != nil {
		return false, err
	}

	held := false
	err = s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		agentRow, err := lockAgentForHold(ctx, c, agentUID)
		if err != nil {
			return err
		}
		eligible, err := agentEligibleForHold(ctx, c, agentRow, now)
		if err != nil || !eligible {
			return err
		}

		heldCount, err := countHeldMessages(ctx, c, p.AgentID)
		if err != nil {
			return err
		}
		if heldCount >= p.MaxHeld {
			return nil
		}

		mq := c.Message.Query().Where(message.IDEQ(msgUID), message.RecipientIDEQ(p.AgentID))
		if c.Driver().Dialect() == dialect.Postgres {
			mq = mq.ForUpdate()
		}
		msgRow, err := mq.Select(
			message.FieldID,
			message.FieldDispatchState,
			message.FieldDeliveryID,
			message.FieldNotReached,
			message.FieldNotReachedAt,
			message.FieldHoldUntil,
		).Only(ctx)
		if err != nil {
			if ent.IsNotFound(err) {
				return nil
			}
			return mapError(err)
		}

		// COALESCE(hold_until, now + HoldTTL): never extend an existing bound.
		effectiveHoldUntil := now.Add(p.HoldTTL)
		if msgRow.HoldUntil != nil {
			effectiveHoldUntil = *msgRow.HoldUntil
		}
		if !effectiveHoldUntil.After(now) {
			return nil
		}

		proofCutoff := now.Add(-p.MaxProofAge)
		n, err := c.Message.Update().
			Where(
				message.IDEQ(msgUID),
				message.RecipientIDEQ(p.AgentID),
				message.DispatchStateEQ(store.MessageDispatchDispatched),
				message.DeliveryIDIsNil(),
				message.NotReachedEQ(store.MessageNotReachedNoSession),
				message.NotReachedAtNotNil(),
				message.NotReachedAtGTE(proofCutoff),
				message.Or(message.HoldUntilIsNil(), message.HoldUntilGT(now)),
			).
			SetDispatchState(store.MessageDispatchAwaitingSession).
			SetHoldUntil(effectiveHoldUntil).
			ClearNotReached().
			ClearNotReachedAt().
			ClearDispatchedAt().
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if n != 1 {
			return nil
		}

		if err := syncAgentDeliveryHold(ctx, c, p.AgentID, effectiveHoldUntil, agentRow.RunID, now); err != nil {
			return err
		}
		held = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return held, nil
}

// ClaimHeldMessages implements store.MessageHoldStore.
func (s *BrokerDispatchStore) ClaimHeldMessages(ctx context.Context, agentID string, limit int) ([]store.Message, error) {
	if limit <= 0 || agentID == "" {
		return nil, nil
	}
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	leaseOwner := store.RedriveLeaseOwnerFromContext(ctx)

	preNow, err := s.holdNow(ctx, s.client.Driver())
	if err != nil {
		return nil, err
	}
	// Query candidate rows before opening the write transaction so Ent query
	// interceptors in concurrency tests can pause multiple callers between the
	// read and conditional updates without deadlocking SQLite's single-writer
	// connection pool. Atomicity comes exclusively from the per-row conditional
	// UPDATE inside withHoldTx below (A2-S7).
	candidates, err := s.client.Message.Query().
		Where(
			messageDispatchAwaitingSessionLiteral(),
			message.RecipientIDEQ(agentID),
			message.Or(message.HoldUntilIsNil(), message.HoldUntilGT(preNow)),
		).
		Order(ent.Asc(message.FieldCreated), ent.Asc(message.FieldID)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if len(candidates) == 0 {
		hasMarker, mErr := s.HasAgentDeliveryHold(ctx, agentID)
		if mErr == nil && hasMarker {
			if err := s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
				if _, err := lockAgentForHold(ctx, c, agentUID); err != nil {
					return err
				}
				return syncAgentDeliveryHold(ctx, c, agentID, time.Time{}, "", now)
			}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	var claimed []store.Message
	err = s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		_, err := lockAgentForHold(ctx, c, agentUID)
		if err != nil {
			return err
		}
		marker, err := lockHoldMarker(ctx, c, agentID)
		if err != nil {
			return err
		}
		if leaseOwner != "" {
			if !markerLeaseActive(marker, now) || marker.ClaimOwner != leaseOwner {
				return nil
			}
		} else if markerLeaseActive(marker, now) {
			return nil
		}
		claimed = make([]store.Message, 0, len(candidates))
		for _, cand := range candidates {
			n, err := c.Message.Update().
				Where(
					message.IDEQ(cand.ID),
					message.RecipientIDEQ(agentID),
					messageDispatchAwaitingSessionLiteral(),
					message.Or(message.HoldUntilIsNil(), message.HoldUntilGT(now)),
				).
				SetDispatchState(store.MessageDispatchDispatched).
				SetDispatchedAt(now).
				Save(ctx)
			if err != nil {
				return mapError(err)
			}
			if n != 1 {
				continue
			}
			fresh, err := c.Message.Get(ctx, cand.ID)
			if err != nil {
				return mapError(err)
			}
			claimed = append(claimed, *entMessageToStore(fresh))
		}
		return syncAgentDeliveryHold(ctx, c, agentID, time.Time{}, "", now)
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// AcquireAgentRedriveLease implements store.MessageHoldStore.
func (s *BrokerDispatchStore) AcquireAgentRedriveLease(ctx context.Context, agentID, ownerID string, ttl time.Duration) (bool, error) {
	if agentID == "" || ownerID == "" || ttl <= 0 {
		return false, nil
	}
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}
	acquired := false
	err = s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		if _, err := lockAgentForHold(ctx, c, agentUID); err != nil {
			return err
		}
		marker, err := lockHoldMarker(ctx, c, agentID)
		if err != nil {
			return err
		}
		if marker == nil {
			return nil
		}
		if markerLeaseActive(marker, now) && marker.ClaimOwner != ownerID {
			return nil
		}
		until := now.Add(ttl)
		if err := c.AgentDeliveryHold.UpdateOneID(agentID).
			SetClaimOwner(ownerID).
			SetClaimUntil(until).
			Exec(ctx); err != nil {
			return mapError(err)
		}
		acquired = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return acquired, nil
}

// RenewAgentRedriveLease implements store.MessageHoldStore.
func (s *BrokerDispatchStore) RenewAgentRedriveLease(ctx context.Context, agentID, ownerID string, ttl time.Duration) (bool, error) {
	if agentID == "" || ownerID == "" || ttl <= 0 {
		return false, nil
	}
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}
	renewed := false
	err = s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		if _, err := lockAgentForHold(ctx, c, agentUID); err != nil {
			return err
		}
		marker, err := lockHoldMarker(ctx, c, agentID)
		if err != nil {
			return err
		}
		if marker == nil || marker.ClaimOwner != ownerID {
			return nil
		}
		until := now.Add(ttl)
		if err := c.AgentDeliveryHold.UpdateOneID(agentID).
			SetClaimUntil(until).
			Exec(ctx); err != nil {
			return mapError(err)
		}
		renewed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return renewed, nil
}

// ReleaseAgentRedriveLease implements store.MessageHoldStore.
func (s *BrokerDispatchStore) ReleaseAgentRedriveLease(ctx context.Context, agentID, ownerID string) error {
	if agentID == "" || ownerID == "" {
		return nil
	}
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return err
	}
	return s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		if _, err := lockAgentForHold(ctx, c, agentUID); err != nil {
			return err
		}
		marker, err := lockHoldMarker(ctx, c, agentID)
		if err != nil {
			return err
		}
		if marker == nil || marker.ClaimOwner != ownerID {
			return nil
		}
		if err := c.AgentDeliveryHold.UpdateOneID(agentID).
			SetClaimOwner("").
			ClearClaimUntil().
			Exec(ctx); err != nil {
			return mapError(err)
		}
		return syncAgentDeliveryHold(ctx, c, agentID, time.Time{}, "", now)
	})
}

// ReholdClaimedMessage implements store.MessageHoldStore.
func (s *BrokerDispatchStore) ReholdClaimedMessage(ctx context.Context, agentID, messageID string, maxHeld int) (bool, error) {
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}
	msgUID, err := parseUUID(messageID)
	if err != nil {
		return false, err
	}

	reheld := false
	err = s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		agentRow, err := lockAgentForHold(ctx, c, agentUID)
		if err != nil {
			return err
		}
		eligible, err := agentEligibleForHold(ctx, c, agentRow, now)
		if err != nil || !eligible {
			return err
		}
		if maxHeld > 0 {
			heldCount, err := countHeldMessages(ctx, c, agentID)
			if err != nil {
				return err
			}
			if heldCount >= maxHeld {
				return nil
			}
		}
		msgRow, err := c.Message.Query().
			Where(message.IDEQ(msgUID), message.RecipientIDEQ(agentID)).
			Select(message.FieldID, message.FieldHoldUntil).
			Only(ctx)
		if err != nil {
			if ent.IsNotFound(err) {
				return nil
			}
			return mapError(err)
		}
		if msgRow.HoldUntil == nil || !msgRow.HoldUntil.After(now) {
			return nil
		}

		n, err := c.Message.Update().
			Where(
				message.IDEQ(msgUID),
				message.RecipientIDEQ(agentID),
				message.DispatchStateEQ(store.MessageDispatchDispatched),
				message.DeliveryIDIsNil(),
				message.HoldUntilGT(now),
			).
			SetDispatchState(store.MessageDispatchAwaitingSession).
			ClearDispatchedAt().
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if n != 1 {
			return nil
		}
		if err := syncAgentDeliveryHold(ctx, c, agentID, *msgRow.HoldUntil, agentRow.RunID, now); err != nil {
			return err
		}
		reheld = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return reheld, nil
}

// HasAgentDeliveryHold implements store.MessageHoldStore.
func (s *BrokerDispatchStore) HasAgentDeliveryHold(ctx context.Context, agentID string) (bool, error) {
	if agentID == "" {
		return false, nil
	}
	ok, err := s.client.AgentDeliveryHold.Query().
		Where(agentdeliveryhold.IDEQ(agentID)).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return ok, nil
}

// ListAgentDeliveryHoldAgentIDs implements store.MessageHoldStore.
func (s *BrokerDispatchStore) ListAgentDeliveryHoldAgentIDs(ctx context.Context) ([]string, error) {
	rows, err := s.client.AgentDeliveryHold.Query().
		Order(ent.Asc(agentdeliveryhold.FieldCreated), ent.Asc(agentdeliveryhold.FieldID)).
		Select(agentdeliveryhold.FieldID).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out, nil
}

// ExpireHeldMessagesForAgent implements store.MessageHoldStore.
func (s *BrokerDispatchStore) ExpireHeldMessagesForAgent(ctx context.Context, agentID, reason string) ([]store.Message, error) {
	if agentID == "" {
		return nil, nil
	}
	var expired []store.Message
	err := s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		if agentUID, pErr := parseUUID(agentID); pErr == nil {
			if _, err := lockAgentForHold(ctx, c, agentUID); err != nil {
				return err
			}
		}
		candidates, err := c.Message.Query().
			Where(
				messageDispatchAwaitingSessionLiteral(),
				message.RecipientIDEQ(agentID),
			).
			Order(ent.Asc(message.FieldCreated), ent.Asc(message.FieldID)).
			All(ctx)
		if err != nil {
			return mapError(err)
		}
		expired = make([]store.Message, 0, len(candidates))
		for _, cand := range candidates {
			n, err := c.Message.Update().
				Where(
					message.IDEQ(cand.ID),
					messageDispatchAwaitingSessionLiteral(),
				).
				SetDispatchState(store.MessageDispatchFailed).
				SetNillableDispatchFailureReason(&reason).
				ClearNotReached().
				ClearNotReachedAt().
				Save(ctx)
			if err != nil {
				return mapError(err)
			}
			if n != 1 {
				continue
			}
			sm := *entMessageToStore(cand)
			sm.DispatchState = store.MessageDispatchFailed
			rCopy := reason
			sm.DispatchFailureReason = &rCopy
			sm.NotReached = ""
			sm.NotReachedAt = nil
			expired = append(expired, sm)
		}
		if marker, mErr := lockHoldMarker(ctx, c, agentID); mErr == nil && marker != nil && marker.ClaimOwner != "" {
			_ = c.AgentDeliveryHold.UpdateOneID(agentID).SetClaimOwner("").ClearClaimUntil().Exec(ctx)
		}
		return syncAgentDeliveryHold(ctx, c, agentID, time.Time{}, "", now)
	})
	if err != nil {
		return nil, err
	}
	return expired, nil
}

// ExpireHeldMessagesPastDeadline implements store.MessageHoldStore.
func (s *BrokerDispatchStore) ExpireHeldMessagesPastDeadline(ctx context.Context, reason string, limit int) ([]store.Message, error) {
	if limit <= 0 {
		limit = defaultExpireHeldPageSize
	}
	var expired []store.Message
	err := s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		candidates, err := c.Message.Query().
			Where(
				messageDispatchAwaitingSessionLiteral(),
				message.HoldUntilNotNil(),
				message.HoldUntilLTE(now),
			).
			Order(ent.Asc(message.FieldCreated), ent.Asc(message.FieldID)).
			Limit(limit).
			All(ctx)
		if err != nil {
			return mapError(err)
		}
		if len(candidates) == 0 {
			return nil
		}
		// Lock candidate recipient agents in sorted UUID order BEFORE updating
		// message rows so lock acquisition matches ClaimHeldMessages /
		// HoldMessageAwaitingSession (agents -> messages -> agent_delivery_holds)
		// and avoids Postgres deadlocks under contention.
		candidateAgentSet := make(map[string]struct{})
		for _, cand := range candidates {
			if cand.RecipientID != "" {
				candidateAgentSet[cand.RecipientID] = struct{}{}
			}
		}
		candidateAgentIDs := make([]string, 0, len(candidateAgentSet))
		for agentID := range candidateAgentSet {
			candidateAgentIDs = append(candidateAgentIDs, agentID)
		}
		slices.Sort(candidateAgentIDs)
		for _, agentID := range candidateAgentIDs {
			if agentUID, pErr := parseUUID(agentID); pErr == nil {
				if _, lErr := lockAgentForHold(ctx, c, agentUID); lErr != nil {
					return lErr
				}
			}
		}

		affectedAgents := make(map[string]struct{})
		expired = make([]store.Message, 0, len(candidates))
		for _, cand := range candidates {
			n, err := c.Message.Update().
				Where(
					message.IDEQ(cand.ID),
					messageDispatchAwaitingSessionLiteral(),
					message.HoldUntilNotNil(),
					message.HoldUntilLTE(now),
				).
				SetDispatchState(store.MessageDispatchFailed).
				SetNillableDispatchFailureReason(&reason).
				ClearNotReached().
				ClearNotReachedAt().
				Save(ctx)
			if err != nil {
				return mapError(err)
			}
			if n != 1 {
				continue
			}
			if cand.RecipientID != "" {
				affectedAgents[cand.RecipientID] = struct{}{}
			}
			sm := *entMessageToStore(cand)
			sm.DispatchState = store.MessageDispatchFailed
			rCopy := reason
			sm.DispatchFailureReason = &rCopy
			sm.NotReached = ""
			sm.NotReachedAt = nil
			expired = append(expired, sm)
		}
		for _, agentID := range candidateAgentIDs {
			if _, ok := affectedAgents[agentID]; !ok {
				continue
			}
			if err := syncAgentDeliveryHold(ctx, c, agentID, time.Time{}, "", now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return expired, nil
}

// ExpireHeldMessagesPastDeadlineForAgent implements store.MessageHoldStore.
func (s *BrokerDispatchStore) ExpireHeldMessagesPastDeadlineForAgent(ctx context.Context, agentID, reason string) ([]store.Message, error) {
	if agentID == "" {
		return nil, nil
	}
	var expired []store.Message
	err := s.withHoldTx(ctx, func(c *ent.Client, now time.Time) error {
		if agentUID, pErr := parseUUID(agentID); pErr == nil {
			if _, err := lockAgentForHold(ctx, c, agentUID); err != nil {
				return err
			}
		}
		candidates, err := c.Message.Query().
			Where(
				messageDispatchAwaitingSessionLiteral(),
				message.RecipientIDEQ(agentID),
				message.HoldUntilNotNil(),
				message.HoldUntilLTE(now),
			).
			Order(ent.Asc(message.FieldCreated), ent.Asc(message.FieldID)).
			All(ctx)
		if err != nil {
			return mapError(err)
		}
		if len(candidates) == 0 {
			return nil
		}
		expired = make([]store.Message, 0, len(candidates))
		for _, cand := range candidates {
			n, err := c.Message.Update().
				Where(
					message.IDEQ(cand.ID),
					messageDispatchAwaitingSessionLiteral(),
					message.HoldUntilNotNil(),
					message.HoldUntilLTE(now),
				).
				SetDispatchState(store.MessageDispatchFailed).
				SetNillableDispatchFailureReason(&reason).
				ClearNotReached().
				ClearNotReachedAt().
				Save(ctx)
			if err != nil {
				return mapError(err)
			}
			if n != 1 {
				continue
			}
			sm := *entMessageToStore(cand)
			sm.DispatchState = store.MessageDispatchFailed
			rCopy := reason
			sm.DispatchFailureReason = &rCopy
			sm.NotReached = ""
			sm.NotReachedAt = nil
			expired = append(expired, sm)
		}
		return syncAgentDeliveryHold(ctx, c, agentID, time.Time{}, "", now)
	})
	if err != nil {
		return nil, err
	}
	return expired, nil
}
