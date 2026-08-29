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

// KeyInputs holds the inputs for conversation key derivation.
type KeyInputs struct {
	ThreadID      string
	ProjectID     string
	SenderKind    string
	SenderID      string
	RecipientKind string
	RecipientID   string
}

// DeriveConversationKey is the ONLY function that should construct a conversation
// external_ref (thread: or dm: key). All call sites must use this function.
//
// Returns the canonical external_ref, the conversation kind, and the project
// scope (nil for direct conversations, which are global per section 2.4.1).
//
// Parse failure returns an error. Callers MUST NOT create a row on error and
// MUST NOT fall back to a constructed key: any guess on any input to the key
// derivation is a guess on the ACL.
func DeriveConversationKey(in KeyInputs) (extRef string, kind string, projectID *string, err error) {
	// Case 1: ThreadID has "dm:" prefix — parse, verify canonicality, return verbatim.
	if strings.HasPrefix(in.ThreadID, "dm:") {
		kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(in.ThreadID)
		if parseErr != nil {
			// DO NOT fall through to case 2 — falling through is exactly how
			// DEF-15 produces its defective row.
			return "", "", nil, fmt.Errorf("dm key parse failed: %w", parseErr)
		}

		rederived, deriveErr := messages.DMConversationKey(kindA, idA, kindB, idB)
		if deriveErr != nil {
			return "", "", nil, fmt.Errorf("dm key re-derivation failed: %w", deriveErr)
		}

		// We re-derive to verify canonicality (token order, UUID format, kind casing)
		// but return the ORIGINAL key, not the re-derived one. Normalising here would
		// make the stored identity differ from the string the caller may already have
		// authorised against — that is the read-gate normalisation refused in §2.15.4(c).
		// Differ means error, never silent rewrite.
		if rederived != in.ThreadID {
			return "", "", nil, fmt.Errorf("dm key is not canonical: got %q, canonical form is %q", in.ThreadID, rederived)
		}

		return in.ThreadID, "direct", nil, nil
	}

	// Case 2: ThreadID non-empty, no "dm:" prefix — thread conversation.
	if in.ThreadID != "" {
		if in.ProjectID == "" {
			return "", "", nil, fmt.Errorf("thread key requires non-empty projectID")
		}
		pid := in.ProjectID
		return fmt.Sprintf("thread:%s:%s", in.ProjectID, in.ThreadID), "group", &pid, nil
	}

	// Case 3: ThreadID empty — derive from principal pair.
	ref, deriveErr := messages.DMConversationKey(in.SenderKind, in.SenderID, in.RecipientKind, in.RecipientID)
	if deriveErr != nil {
		return "", "", nil, fmt.Errorf("dm key derivation from principals failed: %w", deriveErr)
	}
	return ref, "direct", nil, nil
}

// conversationByKeyConfig holds optional parameters for ResolveOrCreateConversationByKey.
type conversationByKeyConfig struct {
	topicLookup    TopicConversationLookup
	surface        string
	parentRef      string
	defaultAgentID *string
}

// ConversationByKeyOption is a functional option for ResolveOrCreateConversationByKey.
type ConversationByKeyOption func(*conversationByKeyConfig)

// WithKeyTopicLookup injects a TopicConversationLookup into the resolve step.
func WithKeyTopicLookup(tl TopicConversationLookup) ConversationByKeyOption {
	return func(c *conversationByKeyConfig) { c.topicLookup = tl }
}

// WithSurface overrides the default surface ("native").
func WithSurface(s string) ConversationByKeyOption {
	return func(c *conversationByKeyConfig) { c.surface = s }
}

// WithParentRef sets the parent_ref for the conversation.
func WithParentRef(pr string) ConversationByKeyOption {
	return func(c *conversationByKeyConfig) { c.parentRef = pr }
}

// WithDefaultAgentID sets the default_agent_id for the conversation.
func WithDefaultAgentID(id *string) ConversationByKeyOption {
	return func(c *conversationByKeyConfig) { c.defaultAgentID = id }
}

// ResolveOrCreateConversationByKey performs UpsertConversationByExternalRef with
// pre-derived key parameters. This is the shared resolve step used by both the
// delegating conversation.go functions and handler call sites.
//
// When a TopicConversationLookup is provided via WithKeyTopicLookup, the function
// intercepts "thread:" group refs and attempts to resolve via the webchat topic's
// linked conversation_id. This is the sink-level guard that prevents all paths
// from minting shadow conversations for native topics that already have a
// conversation.
func ResolveOrCreateConversationByKey(
	ctx context.Context,
	cs ConversationUpserter,
	log *slog.Logger,
	extRef, kind string,
	projectID *string,
	opts ...ConversationByKeyOption,
) *ConversationResult {
	var cfg conversationByKeyConfig
	cfg.surface = "native" // default
	for _, o := range opts {
		o(&cfg)
	}

	// Topic lookup intercept: when kind is "group" and extRef has a
	// "thread:" prefix, attempt to resolve via the webchat topic's
	// linked conversation_id. This is the sink-level guard that
	// prevents all paths from minting shadow conversations for
	// native topics that already have a conversation.
	if cfg.topicLookup != nil && kind == "group" && strings.HasPrefix(extRef, "thread:") {
		// Extract threadID from "thread:<projectID>:<threadID>"
		parts := strings.SplitN(extRef, ":", 3)
		if len(parts) != 3 {
			// Malformed thread: ref — refuse, don't fall through to mint.
			// DeriveConversationKey always produces 3-part refs, but handler
			// sites pass extRef directly, so the guarantee is convention, not
			// type. Treating this as a refusal closes the shape that DEF-20
			// was opened to fix.
			log.Warn("malformed thread: ref, refusing to resolve (non-fatal)",
				"external_ref", extRef, "parts", len(parts))
			return nil
		}
		threadID := parts[2]
		convID, lookupErr := cfg.topicLookup.GetTopicConversationIDIncludingDeleted(ctx, threadID)
		if lookupErr == nil && convID != "" {
			log.Debug("conversation resolved via topic lookup (sink-level)",
				"external_ref", extRef, "conversation_id", convID)
			return &ConversationResult{ConversationID: convID}
		}
		if lookupErr == nil && convID == "" {
			// Topic exists but not yet backfilled — return nil (don't mint).
			log.Debug("topic has no conversation_id, returning unresolved (non-fatal)",
				"external_ref", extRef)
			return nil
		}
		if lookupErr != nil {
			if errors.Is(lookupErr, store.ErrNotFound) {
				// Not a native topic — fall through to upsert.
				// This is the normal case for non-native surface threads.
			} else {
				// Infrastructure failure — return nil, don't mint.
				log.Warn("topic lookup infrastructure error, returning unresolved (non-fatal)",
					"external_ref", extRef, "error", lookupErr)
				return nil
			}
		}
	}

	conv := &store.Conversation{
		Kind:        kind,
		Surface:     cfg.surface,
		ExternalRef: extRef,
		DriftState:  DriftStateActive,
		ProjectID:   projectID,
	}
	if cfg.parentRef != "" {
		conv.ParentRef = cfg.parentRef
	}
	if cfg.defaultAgentID != nil {
		conv.DefaultAgentID = cfg.defaultAgentID
	}

	result, err := cs.UpsertConversationByExternalRef(ctx, conv)
	if err != nil {
		log.Error("conversation resolution failed (non-fatal)",
			"external_ref", extRef,
			"kind", kind,
			"error", err,
		)
		return nil
	}

	return &ConversationResult{
		ConversationID: result.ID,
		ExternalRef:    result.ExternalRef,
	}
}
