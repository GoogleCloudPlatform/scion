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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimeorphan"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runtimeOrphanKey is a runtime orphan's identity (its unique index).
type runtimeOrphanKey struct {
	kind, target, project, slug, agentID string
}

// RecordRuntimeOrphans implements store.AgentStore.RecordRuntimeOrphans.
// Each write is its own statement: the report tolerates a concurrent
// heartbeat of the same broker on another replica (the upsert keeps one row
// per identity), at worst logging a new orphan twice.
func (s *AgentStore) RecordRuntimeOrphans(ctx context.Context, brokerID string, completeTargets []string, orphans []store.RuntimeOrphan, now time.Time, removeAfter int) ([]store.RuntimeOrphan, error) {
	if brokerID == "" || removeAfter < 1 {
		return nil, fmt.Errorf("%w: broker ID and removeAfter are required", store.ErrInvalidInput)
	}
	if len(completeTargets) == 0 {
		return nil, nil
	}
	now = now.UTC()
	rows, err := s.client.RuntimeOrphan.Query().
		Where(runtimeorphan.KindEQ(store.RuntimeOrphanKindInstance), runtimeorphan.BrokerIDEQ(brokerID),
			runtimeorphan.RuntimeTargetIn(completeTargets...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	existing := make(map[runtimeOrphanKey]*ent.RuntimeOrphan, len(rows))
	for _, r := range rows {
		existing[runtimeOrphanKey{r.Kind, r.RuntimeTarget, r.ProjectID, r.Slug, r.AgentID}] = r
	}
	complete := make(map[string]bool, len(completeTargets))
	for _, t := range completeTargets {
		complete[t] = true
	}

	var created []store.RuntimeOrphan
	for _, o := range orphans {
		if !complete[o.RuntimeTarget] {
			continue
		}
		k := runtimeOrphanKey{store.RuntimeOrphanKindInstance, o.RuntimeTarget, o.ProjectID, o.Slug, o.AgentID}
		if _, seen := existing[k]; seen {
			delete(existing, k)
			err = s.client.RuntimeOrphan.Update().
				Where(runtimeorphan.KindEQ(k.kind), runtimeorphan.BrokerIDEQ(brokerID), runtimeorphan.RuntimeTargetEQ(k.target),
					runtimeorphan.ProjectIDEQ(k.project), runtimeorphan.SlugEQ(k.slug), runtimeorphan.AgentIDEQ(k.agentID)).
				SetRunID(o.RunID).SetReason(o.Reason).SetObservedPhase(o.ObservedPhase).
				SetLastSeenAt(now).SetMissedInventories(0).
				Exec(ctx)
			if err != nil {
				return created, mapError(err)
			}
			continue
		}
		row, err := s.client.RuntimeOrphan.Create().
			SetKind(k.kind).SetBrokerID(brokerID).SetRuntimeTarget(k.target).SetProjectID(k.project).
			SetSlug(k.slug).SetAgentID(k.agentID).SetRunID(o.RunID).SetReason(o.Reason).
			SetObservedPhase(o.ObservedPhase).SetFirstSeenAt(now).SetLastSeenAt(now).
			Save(ctx)
		if err != nil {
			return created, mapError(err)
		}
		created = append(created, entRuntimeOrphanToStore(row))
	}

	// Rows the inventory no longer lists.
	for _, r := range existing {
		if r.MissedInventories+1 >= removeAfter {
			err = s.client.RuntimeOrphan.DeleteOneID(r.ID).Exec(ctx)
		} else {
			err = s.client.RuntimeOrphan.UpdateOneID(r.ID).AddMissedInventories(1).Exec(ctx)
		}
		if err != nil && !ent.IsNotFound(err) {
			return created, mapError(err)
		}
	}
	return created, nil
}

// ListRuntimeOrphans implements store.AgentStore.ListRuntimeOrphans.
func (s *AgentStore) ListRuntimeOrphans(ctx context.Context, f store.RuntimeOrphanFilter) ([]store.RuntimeOrphan, error) {
	q := s.client.RuntimeOrphan.Query()
	if f.BrokerID != "" {
		q = q.Where(runtimeorphan.BrokerIDEQ(f.BrokerID))
	}
	if f.ProjectID != "" {
		q = q.Where(runtimeorphan.ProjectIDEQ(f.ProjectID))
	}
	if f.Kind != "" {
		q = q.Where(runtimeorphan.KindEQ(f.Kind))
	}
	rows, err := q.Order(ent.Asc(runtimeorphan.FieldFirstSeenAt), ent.Asc(runtimeorphan.FieldID)).All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.RuntimeOrphan, 0, len(rows))
	for _, r := range rows {
		out = append(out, entRuntimeOrphanToStore(r))
	}
	return out, nil
}

// CountRuntimeOrphans implements store.AgentStore.CountRuntimeOrphans.
func (s *AgentStore) CountRuntimeOrphans(ctx context.Context) ([]store.RuntimeOrphanCount, error) {
	var rows []struct {
		BrokerID string `json:"broker_id"`
		Kind     string `json:"kind"`
		Reason   string `json:"reason"`
		Count    int    `json:"count"`
	}
	err := s.client.RuntimeOrphan.Query().
		GroupBy(runtimeorphan.FieldBrokerID, runtimeorphan.FieldKind, runtimeorphan.FieldReason).
		Aggregate(ent.Count()).
		Scan(ctx, &rows)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.RuntimeOrphanCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.RuntimeOrphanCount{BrokerID: r.BrokerID, Kind: r.Kind, Reason: r.Reason, Count: r.Count})
	}
	return out, nil
}

func entRuntimeOrphanToStore(r *ent.RuntimeOrphan) store.RuntimeOrphan {
	return store.RuntimeOrphan{
		ID: r.ID.String(), Kind: r.Kind, BrokerID: r.BrokerID, RuntimeTarget: r.RuntimeTarget,
		ProjectID: r.ProjectID, Slug: r.Slug, AgentID: r.AgentID, RunID: r.RunID, Reason: r.Reason,
		ObservedPhase: r.ObservedPhase, FirstSeenAt: r.FirstSeenAt, LastSeenAt: r.LastSeenAt,
		MissedInventories: r.MissedInventories, ActionState: r.ActionState, ActionAt: r.ActionAt,
		ActionError: r.ActionError,
	}
}
