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

// ResolveOrCreateConversationByKey performs UpsertConversationByExternalRef with
// pre-derived key parameters. This is the shared resolve step used by both the
// delegating conversation.go functions and handler call sites.
func ResolveOrCreateConversationByKey(
	ctx context.Context,
	cs ConversationUpserter,
	log *slog.Logger,
	extRef, kind string,
	projectID *string,
) *ConversationResult {
	conv := &store.Conversation{
		Kind:        kind,
		Surface:     "native",
		ExternalRef: extRef,
		DriftState:  DriftStateActive,
		ProjectID:   projectID,
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
