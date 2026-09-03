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
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
// Kind, Surface and DisplayName are populated from the same row the resolver
// already loaded — no additional query is required.
type ConversationResult struct {
	ConversationID string
	ExternalRef    string // actual external_ref from the DB, not reconstructed
	Kind           string // "direct" or "group"
	Surface        string // "native", "discord", "slack", "telegram", etc.
	DisplayName    string // human-readable, may be empty
}

// ResolveOrCreateDMConversation resolves (or creates) a direct-message
// conversation for the given sender/recipient pair. DM conversations are
// GLOBAL — they have no ProjectID (design 2.4.1). The external_ref is
// deterministic and kind-encoded: dm:<kind>:<uuid>:<kind>:<uuid> (sorted).
//
// G2 contract (replaces B10): on any error the function returns an error.
// Callers MUST deny the write — a message written without a conversation_id
// is a message that disappears once reads are scoped by conversation_id.
func ResolveOrCreateDMConversation(
	ctx context.Context,
	cs ConversationUpserter,
	pe ParticipantEnsurer,
	log *slog.Logger,
	senderKind, senderID, recipientKind, recipientID string,
) (*ConversationResult, error) {
	if senderID == "" || recipientID == "" {
		return nil, fmt.Errorf("conversation resolution refused: missing sender or recipient ID (sender_id=%q, recipient_id=%q)", senderID, recipientID)
	}

	extRef, err := messages.DMConversationKey(senderKind, senderID, recipientKind, recipientID)
	if err != nil {
		return nil, fmt.Errorf("conversation resolution refused: invalid DM key inputs: %w", err)
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
		return nil, fmt.Errorf("conversation upsert failed (external_ref=%q): %w", extRef, err)
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
			Kind:           result.Kind,
			Surface:        result.Surface,
			DisplayName:    result.DisplayName,
		}, nil
	}

	// Register both participants so the DM appears in each party's sidebar.
	//
	// G2 EXCEPTION — EnsureParticipant failure stays non-fatal.
	// Participants are a LISTING concern, not an access concern: authorization
	// is key-derived (the DM key IS the ACL), not participant-derived. Denying
	// a send because a listing row failed to write turns a cosmetic gap into
	// an outage. The failure is logged and self-repairs on the next message in
	// the same DM.
	//
	// This registration runs on EVERY resolve, not only on first create.
	// EnsureParticipant is insert-if-absent: if the row already exists (active
	// or soft-removed), it is left untouched — including left_at. This prevents
	// resolve-driven calls from silently overwriting a user's listing preference
	// (B6 un-leaving fix).
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
		Kind:           result.Kind,
		Surface:        result.Surface,
		DisplayName:    result.DisplayName,
	}, nil
}

// ResolveDMConversationForRead looks up a DM conversation without creating it.
// This is the read-only counterpart of ResolveOrCreateDMConversation,
// used by the Phase 8 read-switch to query by ConversationID.
//
// DEF-127a: returns (nil, nil) when the conversation does not exist
// (store.ErrNotFound) and (nil, err) on infrastructure errors. Callers
// must distinguish the two: absence is a normal first-use state for DMs,
// while an infrastructure error is a 500.
func ResolveDMConversationForRead(
	ctx context.Context,
	cr ConversationReader,
	log *slog.Logger,
	idAKind, idA, idBKind, idB string,
) (*ConversationResult, error) {
	if idA == "" || idB == "" {
		return nil, nil
	}

	extRef, err := messages.DMConversationKey(idAKind, idA, idBKind, idB)
	if err != nil {
		log.Debug("read-switch: invalid DM key inputs, skipping lookup",
			"id_a_kind", idAKind, "id_a", idA,
			"id_b_kind", idBKind, "id_b", idB,
			"error", err)
		return nil, nil
	}

	conv, err := cr.GetConversationByExternalRef(ctx, "native", extRef)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("read-switch: DM conversation not found (normal for first-use DMs)",
				"external_ref", extRef)
			return nil, nil
		}
		// DEF-127a: infrastructure error — log at Error level, not Debug.
		// A connection failure or query timeout must not masquerade as
		// "no matching conversation record exists".
		log.Error("read-switch: DM conversation lookup failed",
			"external_ref", extRef, "error", err)
		return nil, fmt.Errorf("DM conversation lookup: %w", err)
	}

	return &ConversationResult{
		ConversationID: conv.ID,
		ExternalRef:    conv.ExternalRef,
		Kind:           conv.Kind,
		Surface:        conv.Surface,
		DisplayName:    conv.DisplayName,
	}, nil
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
// G2 contract (replaces B10): on any error the function returns an error.
// Callers MUST deny the write.
func ResolveOrCreateThreadConversation(
	ctx context.Context,
	cs ConversationUpserter,
	log *slog.Logger,
	threadID, projectID string,
	opts ...ThreadConversationOption,
) (*ConversationResult, error) {
	if threadID == "" {
		return nil, fmt.Errorf("thread conversation resolution refused: empty threadID")
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
		return nil, fmt.Errorf("conversation key derivation refused: %w", err)
	}

	// Forward topic lookup and surface to the shared sink so all paths
	// benefit from the sink-level guard (DEF-20 unify) and carry the
	// originating channel (DEF-140).
	var keyOpts []ConversationByKeyOption
	if cfg.topicLookup != nil {
		keyOpts = append(keyOpts, WithKeyTopicLookup(cfg.topicLookup))
	}
	if cfg.surface != "" {
		keyOpts = append(keyOpts, WithSurface(cfg.surface))
	}
	return ResolveOrCreateConversationByKey(ctx, cs, log, extRef, kind, projID, keyOpts...)
}

// threadConversationConfig holds optional parameters for ResolveOrCreateThreadConversation.
type threadConversationConfig struct {
	topicLookup TopicConversationLookup
	surface     string // override for the conversation surface; empty keeps the default ("native")
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

// WithThreadSurface overrides the default surface ("native") for thread
// conversations. The value must be a valid surface string as returned by
// ChannelToSurface — callers MUST NOT pass raw channel strings.
func WithThreadSurface(s string) ThreadConversationOption {
	return func(c *threadConversationConfig) {
		c.surface = s
	}
}

// validSurfaces is the whitelist of channel strings that map 1:1 to surface
// enum values. This must match the SurfaceValidator enum in
// pkg/ent/conversation/conversation.go:129-136.
var validSurfaces = map[string]bool{
	"native":   true,
	"discord":  true,
	"slack":    true,
	"telegram": true,
	"gchat":    true,
	"teams":    true,
}

// channelToSurface maps channel names that are not 1:1 with a surface enum
// value. "web" is the web-chat channel; its surface is "native".
var channelToSurface = map[string]string{
	"web": "native",
}

// ChannelToSurface maps a channel name to a valid surface enum value. Channels
// that are valid surface names pass through directly. Known aliases (e.g.
// "web" → "native") are mapped explicitly. Unknown or empty channels fall back
// to "native" and log a warning so unmapped channels are visible in telemetry.
//
// This is the ONLY place where channel→surface mapping occurs. All call sites
// that thread a channel into conversation creation must use this function
// rather than passing the raw channel string.
func ChannelToSurface(channel string, log *slog.Logger) string {
	if channel == "" {
		return "native"
	}
	// Direct match — channel is itself a valid surface.
	if validSurfaces[channel] {
		return channel
	}
	// Known alias.
	if mapped, ok := channelToSurface[channel]; ok {
		return mapped
	}
	// Unknown channel — fall back to "native" to avoid write denial.
	if log != nil {
		log.Warn("unmapped channel falling back to native surface",
			"channel", channel)
	}
	return "native"
}

// readThreadConfig holds optional parameters for ResolveThreadConversationForRead.
type readThreadConfig struct {
	topicLookup TopicConversationLookup
}

// ReadThreadOption is a functional option for ResolveThreadConversationForRead.
type ReadThreadOption func(*readThreadConfig)

// WithReadTopicLookup injects a TopicConversationLookup into the read-only
// resolution path. When set, native topic threads are resolved via the
// topic's linked conversation_id — the same intercept the write path
// (ResolveOrCreateConversationByKey) uses. Without this option the function
// falls through to the external_ref lookup, which fails for native topics
// because their conversations row has external_ref = ”.
func WithReadTopicLookup(tl TopicConversationLookup) ReadThreadOption {
	return func(c *readThreadConfig) { c.topicLookup = tl }
}

// ResolveThreadConversationForRead looks up a thread conversation without
// creating it. Returns nil if the conversation does not exist or the lookup
// fails. This is the read-only counterpart of ResolveOrCreateThreadConversation,
// used by the Phase 8 read-switch to query by ConversationID.
//
// DEF-100: when a TopicConversationLookup is provided via WithReadTopicLookup,
// the function intercepts "thread:" group refs and resolves via the webchat
// topic's linked conversation_id — the same intercept the write path has in
// ResolveOrCreateConversationByKey. Order: topic lookup first; only if the
// thread is not a native topic (store.ErrNotFound), fall through to the
// external_ref lookup. This ensures native topics (whose conversations rows
// have external_ref = ”) resolve correctly on the read path.
//
// Note: the projectID empty-check is intentionally omitted from the early
// return. DeriveConversationKey case 2 validates empty ProjectID for thread
// keys, while dm:-prefixed ThreadIDs (case 1) do not require projectID at all.
func ResolveThreadConversationForRead(
	ctx context.Context,
	cr ConversationReader,
	log *slog.Logger,
	threadID, projectID string,
	opts ...ReadThreadOption,
) *ConversationResult {
	if threadID == "" {
		return nil
	}

	var cfg readThreadConfig
	for _, o := range opts {
		o(&cfg)
	}

	extRef, kind, _, err := DeriveConversationKey(KeyInputs{
		ThreadID:  threadID,
		ProjectID: projectID,
	})
	if err != nil {
		log.Debug("read-switch: conversation key derivation refused",
			"thread_id", threadID, "error", err)
		return nil
	}

	// DEF-100 topic-lookup intercept: when kind is "group" and extRef has a
	// "thread:" prefix, attempt to resolve via the webchat topic's linked
	// conversation_id. This mirrors the write-path intercept in
	// ResolveOrCreateConversationByKey. Native topics write external_ref = ''
	// on the conversations row, so the external_ref lookup below will never
	// match them — the topic lookup is the only correct resolution path.
	if cfg.topicLookup != nil && kind == "group" && strings.HasPrefix(extRef, "thread:") {
		parts := strings.SplitN(extRef, ":", 3)
		if len(parts) == 3 {
			topicThreadID := parts[2]
			convID, lookupErr := cfg.topicLookup.GetTopicConversationIDIncludingDeleted(ctx, topicThreadID)
			if lookupErr == nil && convID != "" {
				log.Debug("read-switch: conversation resolved via topic lookup (DEF-100)",
					"external_ref", extRef, "conversation_id", convID)
				return &ConversationResult{
					ConversationID: convID,
					Kind:           kind,     // from DeriveConversationKey
					Surface:        "native", // native topics write external_ref='' so the external_ref lookup below never matches them; this topic-lookup path is the only resolution route, and it only exists for native topics (readThreadConfig carries no surface option)
				}
			}
			if lookupErr == nil && convID == "" {
				// Topic exists but not yet backfilled — no conversation to resolve.
				log.Debug("read-switch: topic has no conversation_id yet",
					"external_ref", extRef)
				return nil
			}
			if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
				// Infrastructure error — do not fall through.
				log.Warn("read-switch: topic lookup infrastructure error",
					"external_ref", extRef, "error", lookupErr)
				return nil
			}
			// store.ErrNotFound — not a native topic, fall through to
			// external_ref lookup (normal for non-native surface threads).
		}
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
		Kind:           conv.Kind,
		Surface:        conv.Surface,
		DisplayName:    conv.DisplayName,
	}
}
