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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokerreportedrun"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RecordBrokerReportedRuns implements store.AgentStore.RecordBrokerReportedRuns.
// Each run is its own upsert, so a run whose agent row is gone (the
// foreign key refuses it) does not keep the others from being recorded.
func (s *AgentStore) RecordBrokerReportedRuns(ctx context.Context, brokerID string, runs []store.BrokerReportedRun, seenAt, pruneBefore time.Time) error {
	var errs []error
	seen := make(map[store.BrokerReportedRun]bool, len(runs))
	for _, r := range runs {
		if r.AgentID == "" || r.RunID == "" || brokerID == "" || seen[r] {
			continue
		}
		seen[r] = true
		uid, err := parseUUID(r.AgentID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = s.client.BrokerReportedRun.Create().
			SetAgentID(uid).
			SetRunID(r.RunID).
			SetBrokerID(brokerID).
			SetSeenAt(seenAt.UTC()).
			OnConflictColumns(brokerreportedrun.FieldAgentID, brokerreportedrun.FieldRunID, brokerreportedrun.FieldBrokerID).
			UpdateSeenAt().
			Exec(ctx)
		if err != nil {
			errs = append(errs, mapError(err))
		}
	}
	if !pruneBefore.IsZero() {
		if _, err := s.client.BrokerReportedRun.Delete().
			Where(brokerreportedrun.SeenAtLT(pruneBefore.UTC())).
			Exec(ctx); err != nil {
			errs = append(errs, mapError(err))
		}
	}
	return errors.Join(errs...)
}

// AgentRunReportedSince implements store.AgentStore.AgentRunReportedSince.
func (s *AgentStore) AgentRunReportedSince(ctx context.Context, agentID, runID string, since time.Time) (bool, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}
	ok, err := s.client.BrokerReportedRun.Query().
		Where(
			brokerreportedrun.AgentIDEQ(uid),
			brokerreportedrun.RunIDEQ(runID),
			brokerreportedrun.SeenAtGTE(since.UTC()),
		).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return ok, nil
}
