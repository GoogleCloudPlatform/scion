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

package messaging

import (
	"context"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ConversationUpserter is the minimal interface needed by conversation
// resolution functions. It is satisfied by store.Store (which embeds
// ConversationStore).
type ConversationUpserter interface {
	UpsertConversationByExternalRef(ctx context.Context, conv *store.Conversation) (*store.Conversation, error)
}

// ConversationReader is the minimal interface for read-only conversation
// lookups. It is satisfied by store.Store (which embeds ConversationStore).
type ConversationReader interface {
	GetConversationByExternalRef(ctx context.Context, surface, externalRef string) (*store.Conversation, error)
}

// ConversationResult carries the outcome of a resolve-or-create operation,
// including the actual ExternalRef read back from the database.
type ConversationResult struct {
	ConversationID string
	ExternalRef    string // actual external_ref from the DB, not reconstructed
}

// ResolveOrCreateDMConversation resolves (or creates) a direct-message
// conversation for the given sender/recipient pair. DM conversations are
// GLOBAL — they have no ProjectID (design 2.4.1). The external_ref is
// deterministic and kind-encoded: dm:<kind>:<uuid>:<kind>:<uuid> (sorted).
//
// On any error the function returns nil and logs the failure. Callers MUST NOT
// treat a nil return as fatal — message delivery continues without a
// conversation_id (Phase 5 non-fatal contract).
func ResolveOrCreateDMConversation(
	ctx context.Context,
	cs ConversationUpserter,
	log *slog.Logger,
	senderKind, senderID, recipientKind, recipientID string,
) *ConversationResult {
	if senderID == "" || recipientID == "" {
		log.Warn("skipping conversation resolution: missing sender or recipient ID",
			"sender_id", senderID, "recipient_id", recipientID)
		return nil
	}

	extRef, err := messages.DMConversationKey(senderKind, senderID, recipientKind, recipientID)
	if err != nil {
		log.Warn("skipping conversation resolution: invalid DM key inputs (non-fatal)",
			"sender_kind", senderKind, "sender_id", senderID,
			"recipient_kind", recipientKind, "recipient_id", recipientID,
			"error", err)
		return nil
	}

	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
		DriftState:  DriftStateActive,
	}
	// DM conversations are global — ProjectID is intentionally nil.

	result, err := cs.UpsertConversationByExternalRef(ctx, conv)
	if err != nil {
		log.Error("conversation resolution failed (non-fatal)",
			"external_ref", extRef,
			"sender_id", senderID,
			"recipient_id", recipientID,
			"error", err,
		)
		return nil
	}

	return &ConversationResult{
		ConversationID: result.ID,
		ExternalRef:    result.ExternalRef,
	}
}

// ResolveDMConversationForRead looks up a DM conversation without creating it.
// Returns nil if the conversation does not exist or the lookup fails.
// This is the read-only counterpart of ResolveOrCreateDMConversation,
// used by the Phase 8 read-switch to query by ConversationID.
func ResolveDMConversationForRead(
	ctx context.Context,
	cr ConversationReader,
	log *slog.Logger,
	idAKind, idA, idBKind, idB string,
) *ConversationResult {
	if idA == "" || idB == "" {
		return nil
	}

	extRef, err := messages.DMConversationKey(idAKind, idA, idBKind, idB)
	if err != nil {
		log.Debug("read-switch: invalid DM key inputs, skipping lookup",
			"id_a_kind", idAKind, "id_a", idA,
			"id_b_kind", idBKind, "id_b", idB,
			"error", err)
		return nil
	}

	conv, err := cr.GetConversationByExternalRef(ctx, "native", extRef)
	if err != nil {
		log.Debug("read-switch: DM conversation lookup returned no result",
			"external_ref", extRef, "error", err)
		return nil
	}

	return &ConversationResult{
		ConversationID: conv.ID,
		ExternalRef:    conv.ExternalRef,
	}
}

// ResolveOrCreateThreadConversation resolves (or creates) a thread-based
// conversation for the given thread ID and project. Thread conversations
// are project-scoped. External ref format: thread:{projectID}:{threadID}.
//
// When threadID carries a "dm:" prefix the key is treated as a direct-message
// conversation and validated for canonicality by DeriveConversationKey. A
// non-canonical dm: key is refused — never silently resolved.
//
// On any error the function returns nil and logs the failure.
// Callers MUST NOT treat a nil return as fatal (Phase 5 non-fatal contract).
func ResolveOrCreateThreadConversation(
	ctx context.Context,
	cs ConversationUpserter,
	log *slog.Logger,
	threadID, projectID string,
) *ConversationResult {
	if threadID == "" {
		log.Warn("skipping thread conversation resolution: empty threadID")
		return nil
	}

	extRef, kind, projID, err := DeriveConversationKey(KeyInputs{
		ThreadID:  threadID,
		ProjectID: projectID,
	})
	if err != nil {
		// CHANGE 5: A dm: key that fails canonicality is a REFUSAL, not a resolution miss.
		// Log distinctly so it's visible on the divergence board.
		log.Warn("conversation key derivation refused (non-fatal)",
			"thread_id", threadID,
			"project_id", projectID,
			"error", err,
		)
		return nil
	}

	return ResolveOrCreateConversationByKey(ctx, cs, log, extRef, kind, projID)
}

// ResolveThreadConversationForRead looks up a thread conversation without
// creating it. Returns nil if the conversation does not exist or the lookup
// fails. This is the read-only counterpart of ResolveOrCreateThreadConversation,
// used by the Phase 8 read-switch to query by ConversationID.
//
// Note: the projectID empty-check is intentionally omitted from the early
// return. DeriveConversationKey case 2 validates empty ProjectID for thread
// keys, while dm:-prefixed ThreadIDs (case 1) do not require projectID at all.
func ResolveThreadConversationForRead(
	ctx context.Context,
	cr ConversationReader,
	log *slog.Logger,
	threadID, projectID string,
) *ConversationResult {
	if threadID == "" {
		return nil
	}

	extRef, _, _, err := DeriveConversationKey(KeyInputs{
		ThreadID:  threadID,
		ProjectID: projectID,
	})
	if err != nil {
		log.Debug("read-switch: conversation key derivation refused",
			"thread_id", threadID, "error", err)
		return nil
	}

	conv, lookupErr := cr.GetConversationByExternalRef(ctx, "native", extRef)
	if lookupErr != nil {
		log.Debug("read-switch: thread conversation lookup returned no result",
			"external_ref", extRef, "error", lookupErr)
		return nil
	}

	return &ConversationResult{
		ConversationID: conv.ID,
		ExternalRef:    conv.ExternalRef,
	}
}
