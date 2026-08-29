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

// TopicConversationLookup is the minimal interface for looking up a topic's
// linked conversation_id. The webchat store implements this. When injected
// into ResolveOrCreateThreadConversation, it enables the function to resolve
// native topic threads via the existing dual-write link instead of minting
// a shadow conversation row.
type TopicConversationLookup interface {
	GetTopicConversationID(ctx context.Context, topicID string) (string, error)
	// GetTopicConversationIDIncludingDeleted returns the conversation_id for a
	// webchat topic regardless of its deletion state.
	//
	// Soft-deletion is not declassification. A tombstoned native topic is still
	// a native topic for the purpose of "should I mint." Deletion hides a topic
	// from users; it must not make the mint guard forget the topic was ours.
	GetTopicConversationIDIncludingDeleted(ctx context.Context, topicID string) (string, error)
}

// ConversationReader is the minimal interface for read-only conversation
// lookups. It is satisfied by store.Store (which embeds ConversationStore).
type ConversationReader interface {
	GetConversationByExternalRef(ctx context.Context, surface, externalRef string) (*store.Conversation, error)
}

// ParticipantAdder is the minimal interface for registering conversation
// participants. Separated from ConversationUpserter to keep each interface
// single-purpose.
type ParticipantAdder interface {
	AddParticipant(ctx context.Context, p *store.ConversationParticipant) error
}

// ParticipantEnsurer is the minimal interface for idempotent participant
// registration that preserves existing row state (including left_at).
// Separated from ParticipantAdder because EnsureParticipant has different
// semantics: insert-if-absent vs upsert-and-revive.
type ParticipantEnsurer interface {
	EnsureParticipant(ctx context.Context, p *store.ConversationParticipant) error
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
	pe ParticipantEnsurer,
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

	// B7 nil-pe guard: a nil ParticipantEnsurer must not panic. The function
	// advertises non-fatal semantics for participant registration; a nil pe
	// that panics would violate that contract. Log and skip.
	if pe == nil {
		log.Warn("skipping participant registration: nil ParticipantEnsurer (non-fatal)",
			"external_ref", extRef)
		return &ConversationResult{
			ConversationID: result.ID,
			ExternalRef:    result.ExternalRef,
		}
	}

	// Register both participants so the DM appears in each party's sidebar.
	// Errors are logged but not returned — participant registration is a listing
	// concern, not an access concern (the DM key IS the access authority).
	//
	// This registration runs on EVERY resolve, not only on first create.
	// EnsureParticipant is insert-if-absent: if the row already exists (active
	// or soft-removed), it is left untouched — including left_at. This prevents
	// resolve-driven calls from silently overwriting a user's listing preference
	// (B6 un-leaving fix).
	//
	// Registration is self-repairing: if one of the two EnsureParticipant calls
	// fails transiently, the next message in the same DM retries it.
	//
	// Race note: concurrent ResolveOrCreateDMConversation calls may both
	// attempt EnsureParticipant. This is benign: EnsureParticipant is
	// idempotent and race-safe (unique constraint violations are mapped to nil).
	for _, pp := range []struct{ kind, id string }{
		{senderKind, senderID},
		{recipientKind, recipientID},
	} {
		ensureErr := pe.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: result.ID,
			PrincipalKind:  pp.kind,
			PrincipalID:    pp.id,
			Role:           "member",
		})
		if ensureErr != nil {
			log.Warn("participant registration failed (listing gap, not access)",
				"conversation_id", result.ID,
				"principal_kind", pp.kind,
				"principal_id", pp.id,
				"error", ensureErr)
		}
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
// When a TopicConversationLookup is provided, it is forwarded to the shared
// sink (ResolveOrCreateConversationByKey) via WithKeyTopicLookup. The sink
// intercepts thread: group refs and resolves via the topic's linked
// conversation_id. If the topic has no conversation_id (not yet backfilled),
// the sink returns nil (don't mint). If the topic does not exist
// (store.ErrNotFound), the sink falls through to upsert — this is the normal
// path for non-native surfaces where the threadID is not a webchat topic UUID.
//
// On any error the function returns nil and logs the failure.
// Callers MUST NOT treat a nil return as fatal (Phase 5 non-fatal contract).
func ResolveOrCreateThreadConversation(
	ctx context.Context,
	cs ConversationUpserter,
	log *slog.Logger,
	threadID, projectID string,
	opts ...ThreadConversationOption,
) *ConversationResult {
	if threadID == "" {
		log.Warn("skipping thread conversation resolution: empty threadID")
		return nil
	}

	// Apply options.
	var cfg threadConversationConfig
	for _, o := range opts {
		o(&cfg)
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

	// Forward topic lookup to the shared sink so all paths benefit from
	// the sink-level guard (DEF-20 unify).
	var keyOpts []ConversationByKeyOption
	if cfg.topicLookup != nil {
		keyOpts = append(keyOpts, WithKeyTopicLookup(cfg.topicLookup))
	}
	return ResolveOrCreateConversationByKey(ctx, cs, log, extRef, kind, projID, keyOpts...)
}

// threadConversationConfig holds optional parameters for ResolveOrCreateThreadConversation.
type threadConversationConfig struct {
	topicLookup TopicConversationLookup
}

// ThreadConversationOption is a functional option for ResolveOrCreateThreadConversation.
type ThreadConversationOption func(*threadConversationConfig)

// WithTopicLookup injects a TopicConversationLookup into the resolution path.
// When set, native topic threads are resolved via the dual-write link instead
// of minting a new conversations row.
func WithTopicLookup(tl TopicConversationLookup) ThreadConversationOption {
	return func(c *threadConversationConfig) {
		c.topicLookup = tl
	}
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
