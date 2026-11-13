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
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const decisionChatKeyBackfillMarkerSection = "migration_decision_chat_keys_v1"

// BackfillDecisionChatKeys populates decisions.chat_key from the associated
// prompt message's thread_id for rows created before PR 3A (ptone/scion#7799).
// Rows whose prompt message was already purged by chat retention or had an
// empty thread_id remain NULL (hidden from project-wide listing and returning
// 404 on direct row lookup).
//
// Gated by a HubSetting completion marker so subsequent boots short-circuit in
// one lookup, while the underlying SQL UPDATE is inherently idempotent.
func (c *CompositeStore) BackfillDecisionChatKeys(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, decisionChatKeyBackfillMarkerSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	db := c.DB()
	if db == nil {
		return nil
	}

	const query = `
UPDATE decisions
SET chat_key = (
	SELECT m.thread_id
	FROM messages m
	WHERE m.id = decisions.message_id AND m.thread_id IS NOT NULL AND m.thread_id <> ''
)
WHERE chat_key IS NULL
  AND EXISTS (
	SELECT 1
	FROM messages m
	WHERE m.id = decisions.message_id AND m.thread_id IS NOT NULL AND m.thread_id <> ''
  )`

	result, err := db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("backfill decisions.chat_key from messages.thread_id: %w", err)
	}
	if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("backfilled decision chat_key from prompt messages", "rows_updated", n)
	}

	_, err = c.UpsertHubSetting(ctx, decisionChatKeyBackfillMarkerSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}
