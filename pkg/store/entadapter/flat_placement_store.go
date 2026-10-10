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

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Flat Runtime Broker placement persistence
// (.design/flat-runtime-brokers-contract.md section 8).

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// isFlatRuntimeBroker reports whether the Runtime Broker row stores a
// runtime target. A missing row is not flat (it counts as legacy).
func (s *AgentStore) isFlatRuntimeBroker(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := s.client.RuntimeBroker.Query().
		Where(runtimebroker.IDEQ(id), runtimebroker.RuntimeTargetIDNotNil()).
		Count(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// flatRuntimeBrokerIDs returns the IDs of every flat Runtime Broker row.
func (s *AgentStore) flatRuntimeBrokerIDs(ctx context.Context) ([]string, error) {
	rows, err := s.client.RuntimeBroker.Query().
		Where(runtimebroker.RuntimeTargetIDNotNil()).
		Select(runtimebroker.FieldID).
		All(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID.String())
	}
	return ids, nil
}

// SetAgentPinnedRuntimeTarget implements store.AgentStore.
func (s *AgentStore) SetAgentPinnedRuntimeTarget(ctx context.Context, agentID string, expected, next store.PinnedPlacement) (*store.Agent, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	if next.RuntimeBrokerID == "" || next.RuntimeTargetID == "" || next.RuntimeTargetType == "" {
		return nil, fmt.Errorf("%w: next placement must name a Runtime Broker and a runtime target", store.ErrInvalidPinnedPlacement)
	}

	preds := []predicate.Agent{agent.IDEQ(uid)}
	if expected.RuntimeBrokerID == "" {
		// runtime_broker_id is nullable: "" matches both '' and NULL.
		preds = append(preds, agent.Or(agent.RuntimeBrokerIDEQ(""), agent.RuntimeBrokerIDIsNil()))
	} else {
		preds = append(preds, agent.RuntimeBrokerIDEQ(expected.RuntimeBrokerID))
	}
	if expected.RuntimeTargetID == "" {
		preds = append(preds, agent.PinnedRuntimeTargetIDIsNil(), agent.PinnedRuntimeBrokerIDIsNil())
	} else {
		preds = append(preds,
			agent.PinnedRuntimeBrokerIDEQ(expected.RuntimeBrokerID),
			agent.PinnedRuntimeTargetIDEQ(expected.RuntimeTargetID),
			agent.PinnedRuntimeTargetTypeEQ(expected.RuntimeTargetType))
	}

	affected, err := s.client.Agent.Update().
		Where(preds...).
		SetRuntimeBrokerID(next.RuntimeBrokerID).
		SetPinnedRuntimeBrokerID(next.RuntimeBrokerID).
		SetPinnedRuntimeTargetID(next.RuntimeTargetID).
		SetPinnedRuntimeTargetType(next.RuntimeTargetType).
		AddStateVersion(1).
		Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if affected == 0 {
		exists, err := s.client.Agent.Query().Where(agent.IDEQ(uid)).Exist(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if !exists {
			return nil, store.ErrNotFound
		}
		return nil, store.ErrPinnedPlacementChanged
	}
	return s.GetAgent(ctx, agentID)
}

// GetLegacyRuntimeBrokerByName implements store.ProjectStore.
func (s *ProjectStore) GetLegacyRuntimeBrokerByName(ctx context.Context, name string) (*store.RuntimeBroker, error) {
	b, err := s.client.RuntimeBroker.Query().
		Where(runtimebroker.NameEqualFold(name), runtimebroker.RuntimeTargetIDIsNil()).
		Order(ent.Asc(runtimebroker.FieldCreated), ent.Asc(runtimebroker.FieldID)).
		First(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entBrokerToStore(b), nil
}

// SetRuntimeBrokerTarget implements store.ProjectStore. It updates only the
// display name of an already-flat row with the same target ID and type.
func (s *ProjectStore) SetRuntimeBrokerTarget(ctx context.Context, brokerID string, desc api.RuntimeTargetDescriptor) (*store.RuntimeBroker, error) {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return nil, err
	}
	cur, err := s.client.RuntimeBroker.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	if cur.RuntimeTargetID == nil || *cur.RuntimeTargetID == "" {
		return nil, store.ErrRuntimeBrokerNotFlat
	}
	if *cur.RuntimeTargetID != desc.ID || cur.RuntimeTargetType != desc.Type {
		return nil, store.ErrRuntimeTargetChanged
	}
	affected, err := s.client.RuntimeBroker.Update().
		Where(runtimebroker.IDEQ(uid),
			runtimebroker.RuntimeTargetIDEQ(desc.ID),
			runtimebroker.RuntimeTargetTypeEQ(desc.Type)).
		SetRuntimeTargetDisplayName(desc.DisplayName).
		Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if affected == 0 {
		return nil, store.ErrRuntimeTargetChanged
	}
	return s.GetRuntimeBroker(ctx, brokerID)
}
