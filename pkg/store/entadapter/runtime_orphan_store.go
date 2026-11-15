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
// Each write is its own statement, and a failed write does not stop the
// others (errors are joined). Duplicate identities in one batch are written
// once. A row another replica removed since the read is created again. A
// create that loses a race with another replica handling a
// heartbeat of the same broker hits the unique index and is skipped: that
// replica's row stands and is not returned as created here.
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
	var errs []error
	batch := make(map[runtimeOrphanKey]bool, len(orphans))
	for _, o := range orphans {
		k := runtimeOrphanKey{store.RuntimeOrphanKindInstance, o.RuntimeTarget, o.ProjectID, o.Slug, o.AgentID}
		if !complete[o.RuntimeTarget] || batch[k] {
			continue
		}
		batch[k] = true
		if ext, seen := existing[k]; seen {
			delete(existing, k)
			err = s.client.RuntimeOrphan.UpdateOneID(ext.ID).
				SetRunID(o.RunID).SetReason(o.Reason).SetObservedPhase(o.ObservedPhase).
				SetLastSeenAt(now).SetMissedInventories(0).
				Exec(ctx)
			if err == nil {
				continue
			}
			if !ent.IsNotFound(err) {
				errs = append(errs, mapError(err))
				continue
			}
			// Another replica removed the row since the read: create it
			// again below (or skip it if that replica already did).
		}
		row, err := s.client.RuntimeOrphan.Create().
			SetKind(k.kind).SetBrokerID(brokerID).SetRuntimeTarget(k.target).SetProjectID(k.project).
			SetSlug(k.slug).SetAgentID(k.agentID).SetRunID(o.RunID).SetReason(o.Reason).
			SetObservedPhase(o.ObservedPhase).SetFirstSeenAt(now).SetLastSeenAt(now).
			Save(ctx)
		if err != nil {
			if !ent.IsConstraintError(err) {
				errs = append(errs, mapError(err))
			}
			continue
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
			errs = append(errs, mapError(err))
		}
	}
	return created, errors.Join(errs...)
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
