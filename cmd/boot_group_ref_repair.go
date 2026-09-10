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

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// topicLink holds the project_id and topic id recovered from a webchat_topic
// row linked to a conversation via webchat_topic.conversation_id.
type topicLink struct {
	TopicID   string
	ProjectID string
}

// runGroupRefRepair repairs kind='group' conversations whose external_ref is
// empty by computing the canonical "thread:<projectID>:<topicID>" value from
// the linked webchat_topic row, using the production derivation function
// (messaging.ThreadConversationExternalRef, pkg/messaging/derive_key.go:119).
//
// M-1' semantics (same as runDMKeyMigration, cmd/boot_data_migrations.go:96):
// a completion marker records that a full pass completed without a run-level
// failure. Row-level refusals (no linked topic, multiple linked topics, unique
// constraint collision on the partial index conversation_surface_external_ref)
// are counted and logged but do NOT block the marker. A marker must never be
// written for a pass that did not finish.
//
// Three per-row outcomes, all handled:
//
//  1. Exactly one linked topic, UPDATE succeeds — the expected case.
//  2. No linked topic, or more than one — skip and log at WARN. Row-level
//     refusal, does not block the marker.
//  3. Exactly one linked topic, UPDATE fails on the partial unique index
//     (surface, external_ref) — a "shadow" conversation already holds the
//     computed ref. Skip and log at WARN. Row-level refusal, does not block
//     the marker. (DEF-166 §6.0: 3 such rows on gteam.)
//
// This migration scopes exclusively to kind='group'. kind='direct' rows are
// never touched — their external_ref is the ACL (DEF-29).
func runGroupRefRepair(ctx context.Context, s store.Store) {
	// Fast path: already complete. O(1) with respect to data volume.
	done, err := IsMigrationComplete(ctx, s, MigrationGroupRefRepair)
	if err != nil {
		slog.Error("Group ref repair: failed to check completion marker; will attempt migration",
			"error", err)
		// Fall through: attempting the migration is safer than skipping it
		// when we cannot read the marker. The migration is idempotent.
	} else if done {
		slog.Debug("Group ref repair: already complete, skipping")
		return
	}

	slog.Info("Group ref repair: starting")

	// We need raw SQL access for the webchat_topic table, which lives
	// outside the ent schema (pkg/hub/webchannel_store_postgres.go:74).
	// Use the same interface-assertion pattern as server_foreground.go:596.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	if !ok {
		slog.Error("Group ref repair: store does not provide raw DB access; will retry next boot")
		return
	}
	db := dbProvider.DB()
	if db == nil {
		slog.Error("Group ref repair: raw DB is nil; will retry next boot")
		return
	}

	// Step 1: Find all kind='group' conversations with empty external_ref.
	// Use ListConversations with Kind filter and paginate; filter for empty
	// ExternalRef in Go. The population is small (41 on gteam) so this is
	// not a performance concern.
	var broken []store.Conversation
	cursor := ""
	for {
		result, listErr := s.ListConversations(ctx, store.ConversationFilter{
			Kind: "group",
		}, store.ListOptions{Limit: 500, Cursor: cursor})
		if listErr != nil {
			// RUN-LEVEL failure: could not list. Do NOT write the marker.
			slog.Error("Group ref repair: failed to list group conversations; will retry next boot",
				"error", listErr)
			return
		}
		for _, conv := range result.Items {
			if conv.ExternalRef == "" {
				broken = append(broken, conv)
			}
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	slog.Info("Group ref repair: found broken conversations",
		"count", len(broken))

	if len(broken) == 0 {
		// No broken rows — an empty pass is still a completed pass.
		if markErr := MarkMigrationComplete(ctx, s, MigrationGroupRefRepair, 0); markErr != nil {
			slog.Error("Group ref repair: failed to write completion marker; will retry next boot",
				"error", markErr)
		}
		return
	}

	// Step 2: For each broken conversation, find linked webchat_topic(s),
	// compute the ref, and update.
	repaired := 0
	skippedNoTopic := 0
	skippedMultiTopic := 0
	skippedCollision := 0
	skippedDeriveErr := 0

	for _, conv := range broken {
		// Find linked topics via webchat_topic.conversation_id.
		topics, topicErr := findTopicsByConversationID(ctx, db, conv.ID)
		if topicErr != nil {
			// RUN-LEVEL failure: could not query topics table.
			slog.Error("Group ref repair: failed to query webchat_topic; will retry next boot",
				"conversation_id", conv.ID,
				"error", topicErr)
			return
		}

		if len(topics) == 0 {
			// Row-level refusal: no linked topic. Skip, do not block marker.
			slog.Warn("Group ref repair: conversation has no linked webchat_topic; skipping",
				"conversation_id", conv.ID)
			skippedNoTopic++
			continue
		}

		if len(topics) > 1 {
			// Row-level refusal: ambiguous — more than one linked topic.
			slog.Warn("Group ref repair: conversation has multiple linked webchat_topics; skipping",
				"conversation_id", conv.ID,
				"topic_count", len(topics))
			skippedMultiTopic++
			continue
		}

		// Exactly one linked topic. Compute the ref using the production
		// function (messaging.ThreadConversationExternalRef,
		// pkg/messaging/derive_key.go:119).
		topic := topics[0]
		extRef, deriveErr := messaging.ThreadConversationExternalRef(topic.ProjectID, topic.TopicID)
		if deriveErr != nil {
			// Row-level refusal: derivation failed. Shouldn't happen with
			// real project IDs and topic IDs, but treat as a skip.
			slog.Warn("Group ref repair: ThreadConversationExternalRef failed; skipping",
				"conversation_id", conv.ID,
				"project_id", topic.ProjectID,
				"topic_id", topic.TopicID,
				"error", deriveErr)
			skippedDeriveErr++
			continue
		}

		// Update the conversation's external_ref.
		conv.ExternalRef = extRef
		updateErr := s.UpdateConversation(ctx, &conv)
		if updateErr != nil {
			// Check for unique constraint violation on the partial unique
			// index conversation_surface_external_ref (surface, external_ref)
			// where external_ref <> '' AND deleted_at IS NULL.
			// (pkg/ent/schema/conversation.go:86-92)
			//
			// mapError (pkg/store/entadapter/group_store.go:78) converts ent
			// constraint errors to store.ErrAlreadyExists.
			if errors.Is(updateErr, store.ErrAlreadyExists) {
				// Row-level refusal: a shadow conversation already holds
				// this ref on the same surface. Skip, do not block marker.
				// (DEF-166 §6.0: 3 such rows on gteam.)
				slog.Warn("Group ref repair: unique constraint collision (shadow conversation holds this ref); skipping",
					"conversation_id", conv.ID,
					"computed_ref", extRef,
					"surface", conv.Surface)
				skippedCollision++
				continue
			}
			// Any other update error is a run-level failure.
			slog.Error("Group ref repair: failed to update conversation; will retry next boot",
				"conversation_id", conv.ID,
				"error", updateErr)
			return
		}

		repaired++
	}

	// Pass completed. Row-level refusals do not block the marker.
	totalSkipped := skippedNoTopic + skippedMultiTopic + skippedCollision + skippedDeriveErr
	slog.Info("Group ref repair: pass completed",
		"repaired", repaired,
		"skipped_no_topic", skippedNoTopic,
		"skipped_multi_topic", skippedMultiTopic,
		"skipped_collision", skippedCollision,
		"skipped_derive_error", skippedDeriveErr,
		"total_skipped", totalSkipped,
	)

	if markErr := MarkMigrationComplete(ctx, s, MigrationGroupRefRepair, totalSkipped); markErr != nil {
		slog.Error("Group ref repair: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}

// findTopicsByConversationID returns all webchat_topic rows linked to the
// given conversation_id. The link direction is webchat_topic.conversation_id →
// conversations.id (confirmed empirically — the conversations table has no FK
// back to the topic; see DEF-166-REPAIR-PLAN.md §4).
//
// Returns an empty slice (not an error) when no topics are linked.
func findTopicsByConversationID(ctx context.Context, db *sql.DB, conversationID string) ([]topicLink, error) {
	const query = `SELECT id, project_id FROM webchat_topic WHERE conversation_id = $1`
	rows, err := db.QueryContext(ctx, query, conversationID)
	if err != nil {
		return nil, fmt.Errorf("querying webchat_topic by conversation_id %s: %w", conversationID, err)
	}
	defer rows.Close()

	var links []topicLink
	for rows.Next() {
		var tl topicLink
		if scanErr := rows.Scan(&tl.TopicID, &tl.ProjectID); scanErr != nil {
			return nil, fmt.Errorf("scanning webchat_topic row for conversation_id %s: %w", conversationID, scanErr)
		}
		links = append(links, tl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating webchat_topic rows for conversation_id %s: %w", conversationID, err)
	}
	return links, nil
}
