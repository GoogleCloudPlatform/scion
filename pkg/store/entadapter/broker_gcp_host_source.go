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
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runtimeBrokerGCPHostSourceBackfillSection marks the one-time
// BackfillRuntimeBrokerGCPHostSource as done.
const runtimeBrokerGCPHostSourceBackfillSection = "migration_runtime_broker_gcp_host_source_v1"

// BackfillRuntimeBrokerGCPHostSource records a GCP host source on rows
// written before the column existed, once:
//
//   - "detected" for the co-located broker's row (a legacy row with the
//     embedded broker role label and no owner), so its metadata detection
//     keeps refreshing the pair;
//   - "admin" for every other row with a stored service account, so a value
//     set through the update API keeps its current rule;
//   - unchanged ("") otherwise.
//
// It runs once, behind a hub-setting marker: a later update-API call that
// hands the pair back to registration sets the source to "" on purpose, and
// a rerun must not change that.
func (c *CompositeStore) BackfillRuntimeBrokerGCPHostSource(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, runtimeBrokerGCPHostSourceBackfillSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	rows, err := c.client.RuntimeBroker.Query().
		Where(gcpHostSourceUnset(), runtimebroker.GcpHostServiceAccountEmailNotNil(), runtimebroker.GcpHostServiceAccountEmailNEQ("")).
		All(ctx)
	if err != nil {
		return err
	}
	var detected, admin int
	for _, b := range rows {
		source := store.GCPHostSourceAdmin
		if b.RuntimeTargetID == nil && b.CreatedBy == "" && b.Labels[store.BrokerRoleLabel] == store.BrokerRoleEmbedded {
			source = store.GCPHostSourceDetected
		}
		if _, err := c.client.RuntimeBroker.Update().
			Where(runtimebroker.IDEQ(b.ID), gcpHostSourceUnset()).
			SetGcpHostSource(source).
			Save(ctx); err != nil {
			return mapError(err)
		}
		if source == store.GCPHostSourceDetected {
			detected++
		} else {
			admin++
		}
	}
	slog.Info("recorded runtime broker GCP host source", "detected", detected, "admin", admin)

	_, err = c.UpsertHubSetting(ctx, runtimeBrokerGCPHostSourceBackfillSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}
