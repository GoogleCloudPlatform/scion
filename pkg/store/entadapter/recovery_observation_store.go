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

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentrecovery"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokertargetinventory"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RecordRecoveryObservations implements store.AgentStore.RecordRecoveryObservations.
func (s *AgentStore) RecordRecoveryObservations(ctx context.Context, brokerID string, completeTargets []string, obs []store.RecoveryObservation) (time.Time, error) {
	if brokerID == "" {
		return time.Time{}, fmt.Errorf("%w: broker ID must not be empty", store.ErrInvalidInput)
	}
	complete := make(map[string]bool, len(completeTargets))
	for _, t := range completeTargets {
		if t != "" {
			complete[t] = true
		}
	}
	var keep []store.RecoveryObservation
	ids := make([]string, 0, len(obs))
	for _, o := range obs {
		if o.AgentID == "" || !o.State.Valid() {
			return time.Time{}, fmt.Errorf("%w: invalid observation %+v", store.ErrInvalidInput, o)
		}
		if !complete[o.Target] {
			continue
		}
		keep = append(keep, o)
		ids = append(ids, o.AgentID)
	}
	if len(complete) == 0 {
		return time.Time{}, nil
	}

	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer ltx.cleanup()
	isPG := s.dialect(ctx) == dialect.Postgres
	committed := false
	defer func() {
		if !committed {
			_ = ltx.tx.Rollback()
		}
	}()

	raw, err := storeNow(ctx, ltx.tx, isPG)
	if err != nil {
		return time.Time{}, err
	}
	now := claimTime(raw)

	existing := map[string]*time.Time{} // agent ID -> stored first_absent_at, for rows that are absent
	present := map[string]bool{}
	if len(ids) > 0 {
		rows, err := ltx.client.AgentRecovery.Query().Where(agentrecovery.IDIn(ids...)).All(ctx)
		if err != nil {
			return time.Time{}, mapError(err)
		}
		for _, r := range rows {
			present[r.ID] = true
			if r.ObservedState == string(store.ObservedAbsent) {
				existing[r.ID] = r.FirstAbsentAt
			}
		}
	}

	for _, o := range keep {
		var firstAbsent *time.Time
		if o.State == store.ObservedAbsent {
			t := now
			if prev, ok := existing[o.AgentID]; ok && prev != nil {
				t = *prev
			}
			firstAbsent = &t
		}
		if present[o.AgentID] {
			u := ltx.client.AgentRecovery.UpdateOneID(o.AgentID).
				SetBrokerID(brokerID).
				SetObservedState(string(o.State)).
				SetObservedTarget(o.Target).
				SetObservedAt(now).
				SetObservedInFlight(o.InFlight)
			if firstAbsent != nil {
				u.SetFirstAbsentAt(*firstAbsent)
			} else {
				u.ClearFirstAbsentAt()
			}
			if _, err := u.Save(ctx); err != nil {
				return time.Time{}, mapError(err)
			}
			continue
		}
		c := ltx.client.AgentRecovery.Create().
			SetID(o.AgentID).
			SetBrokerID(brokerID).
			SetObservedState(string(o.State)).
			SetObservedTarget(o.Target).
			SetObservedAt(now).
			SetObservedInFlight(o.InFlight).
			SetNillableFirstAbsentAt(firstAbsent)
		if _, err := c.Save(ctx); err != nil {
			return time.Time{}, mapError(err)
		}
	}

	for t := range complete {
		if err := ltx.client.BrokerTargetInventory.Create().
			SetBrokerID(brokerID).
			SetTarget(t).
			SetLastCompleteInventoryAt(now).
			OnConflictColumns(brokertargetinventory.FieldBrokerID, brokertargetinventory.FieldTarget).
			UpdateLastCompleteInventoryAt().
			Exec(ctx); err != nil {
			return time.Time{}, mapError(err)
		}
	}

	if err := ltx.tx.Commit(); err != nil {
		return time.Time{}, fmt.Errorf("recovery observations: commit: %w", err)
	}
	committed = true
	return now, nil
}

// GetRecoveryObservations implements store.AgentStore.GetRecoveryObservations.
func (s *AgentStore) GetRecoveryObservations(ctx context.Context, agentIDs []string) (map[string]store.RecoveryObservationRecord, error) {
	out := make(map[string]store.RecoveryObservationRecord, len(agentIDs))
	if len(agentIDs) == 0 {
		return out, nil
	}
	rows, err := s.client.AgentRecovery.Query().Where(agentrecovery.IDIn(agentIDs...)).All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range rows {
		rec := store.RecoveryObservationRecord{
			AgentID:       r.ID,
			BrokerID:      r.BrokerID,
			Target:        r.ObservedTarget,
			State:         store.RecoveryObservedState(r.ObservedState),
			FirstAbsentAt: copyTimePtr(r.FirstAbsentAt),
			InFlight:      r.ObservedInFlight,
		}
		if r.ObservedAt != nil {
			rec.ObservedAt = *r.ObservedAt
		}
		out[r.ID] = rec
	}
	return out, nil
}

// ListBrokerTargetInventory implements store.AgentStore.ListBrokerTargetInventory.
func (s *AgentStore) ListBrokerTargetInventory(ctx context.Context, brokerID string) ([]store.BrokerTargetInventory, error) {
	rows, err := s.client.BrokerTargetInventory.Query().
		Where(brokertargetinventory.BrokerIDEQ(brokerID)).
		Order(brokertargetinventory.ByTarget(sql.OrderAsc())).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.BrokerTargetInventory, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.BrokerTargetInventory{
			BrokerID:                r.BrokerID,
			Target:                  r.Target,
			LastCompleteInventoryAt: r.LastCompleteInventoryAt,
		})
	}
	return out, nil
}
