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
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Reference grammar (§2.6)
// ---------------------------------------------------------------------------

// ReferenceKind identifies which form a parsed reference takes.
type ReferenceKind int

const (
	// RefConversation is a canonical conv:<id> reference.
	RefConversation ReferenceKind = iota + 1
	// RefAgent is an @<agent-slug> reference (resolved within current project).
	RefAgent
	// RefEmail is an @<email> reference (global DM).
	RefEmail
	// RefThread is a #<thread-name> reference (group conversation in project space).
	RefThread
)

// Reference is the structured result of parsing a conversation reference string.
type Reference struct {
	Kind  ReferenceKind
	Value string // the parsed value: UUID, slug, email, or thread name
	Raw   string // the original input string
}

// ParseReference parses a conversation reference string into a structured
// Reference. It accepts exactly four forms:
//
//	conv:<uuid>      → RefConversation
//	@<agent-slug>    → RefAgent (no '@' in slug)
//	@<email>         → RefEmail (contains '@' after the leading '@')
//	#<thread-name>   → RefThread (no '/' allowed — §2.6.1)
//
// Returns an error for any other form.
func ParseReference(raw string) (*Reference, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty reference: %w", store.ErrInvalidInput)
	}

	switch {
	case strings.HasPrefix(raw, "conv:"):
		id := strings.TrimPrefix(raw, "conv:")
		if _, err := uuid.Parse(id); err != nil {
			return nil, fmt.Errorf("invalid conversation ID in %q: %w", raw, store.ErrInvalidInput)
		}
		return &Reference{Kind: RefConversation, Value: id, Raw: raw}, nil

	case strings.HasPrefix(raw, "@"):
		value := raw[1:]
		if value == "" {
			return nil, fmt.Errorf("empty @ reference: %w", store.ErrInvalidInput)
		}
		// If it contains '@' after the leading '@', treat as email.
		if strings.Contains(value, "@") {
			return &Reference{Kind: RefEmail, Value: value, Raw: raw}, nil
		}
		return &Reference{Kind: RefAgent, Value: value, Raw: raw}, nil

	case strings.HasPrefix(raw, "#"):
		value := raw[1:]
		if value == "" {
			return nil, fmt.Errorf("empty # reference: %w", store.ErrInvalidInput)
		}
		// §2.6.1: reject #<space>/<thread> form.
		if strings.Contains(value, "/") {
			return nil, fmt.Errorf("invalid thread reference %q: #<space>/<thread> form is not supported (AC-31): %w", raw, store.ErrInvalidInput)
		}
		return &Reference{Kind: RefThread, Value: value, Raw: raw}, nil

	default:
		return nil, fmt.Errorf("unrecognized reference format %q: %w", raw, store.ErrInvalidInput)
	}
}

// ---------------------------------------------------------------------------
// Resolution context and result types
// ---------------------------------------------------------------------------

// ResolveContext carries caller-provided context needed for resolution.
type ResolveContext struct {
	SenderPrincipalKind string // "user" or "agent"
	SenderPrincipalID   string // UUID of the sender
	ProjectID           string // current project (may be empty for global DMs)
}

// ResolveResult is the outcome of a successful resolution.
type ResolveResult struct {
	ConversationID string
	Created        bool            // true if a new conversation was created
	Unresolved     []UnresolvedRef // mentions that could not be resolved (non-fatal in DMs)
}

// UnresolvedRef describes a reference that could not be resolved.
type UnresolvedRef struct {
	Text       string   // the original reference text
	Reason     string   // "ambiguous" | "not-found" | "no-shared-project" | "not-a-participant" | "boundary-violation"
	Candidates []string // qualified forms that would work (for ambiguous)
}

// ---------------------------------------------------------------------------
// Resolution store interface
// ---------------------------------------------------------------------------

// ResolutionStore defines the subset of store methods needed by the resolver.
// It decouples the resolution logic from the full store.Store.
type ResolutionStore interface {
	// Conversation CRUD
	GetConversation(ctx context.Context, id string) (*store.Conversation, error)
	CreateConversation(ctx context.Context, conv *store.Conversation) error
	ListConversations(ctx context.Context, filter store.ConversationFilter, opts store.ListOptions) (*store.ListResult[store.Conversation], error)

	// Participants
	AddParticipant(ctx context.Context, p *store.ConversationParticipant) error
	ListParticipants(ctx context.Context, conversationID string) ([]store.ConversationParticipant, error)
	GetConversationsForPrincipal(ctx context.Context, principalKind, principalID string) ([]store.Conversation, error)

	// Upsert
	UpsertConversationByExternalRef(ctx context.Context, conv *store.Conversation) (*store.Conversation, error)

	// Agent resolution
	GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error)

	// User resolution
	GetUserByEmail(ctx context.Context, email string) (*store.User, error)
}

// ---------------------------------------------------------------------------
// Resolve function
// ---------------------------------------------------------------------------

// Resolve resolves a conversation reference string to a conversation ID,
// creating the conversation if needed (resolve-or-create for @ references).
//
// Project isolation rules (§2.6.1, AC-30):
//   - conv:<id>: if the conversation belongs to a different project, the error
//     distinguishes between boundary-violation (sender belongs to that project)
//     and not-found (sender does not, preventing information leakage).
//   - @<agent-slug>: resolved only within rctx.ProjectID.
//   - @<email>: global DM, no project restriction.
//   - #<thread-name>: resolved only within rctx.ProjectID's space.
func Resolve(ctx context.Context, s ResolutionStore, ref string, rctx ResolveContext) (*ResolveResult, error) {
	parsed, err := ParseReference(ref)
	if err != nil {
		return nil, err
	}

	var result *ResolveResult
	switch parsed.Kind {
	case RefConversation:
		result, err = resolveConvByID(ctx, s, parsed.Value, rctx)
	case RefAgent:
		result, err = resolveAgentDM(ctx, s, parsed.Value, rctx)
	case RefEmail:
		result, err = resolveEmailDM(ctx, s, parsed.Value, rctx)
	case RefThread:
		result, err = resolveThread(ctx, s, parsed.Value, rctx)
	default:
		return nil, fmt.Errorf("unsupported reference kind: %w", store.ErrInvalidInput)
	}
	if err != nil {
		return nil, err
	}

	// Post-resolution authorisation: verify the sender is allowed to access
	// the resolved conversation. The rules depend on conversation kind:
	//
	//   - direct: sender MUST be a participant (covers conv:<id>, @<agent>,
	//     @<email>). A conv:<id> from a log could be someone else's DM —
	//     project membership alone must not open it.
	//
	//   - group: project membership authorises access (already enforced by the
	//     project isolation check in resolveConvByID / resolveThread). An agent
	//     who has never spoken in #general must still be able to post there.
	//
	//   - global DMs (nil ProjectID): participant check is the ONLY auth gate
	//     since there is no project to check against.
	//
	// Newly-created conversations (result.Created == true) are exempt: the
	// resolve-or-create path adds both parties as participants before returning.
	if !result.Created {
		if authErr := checkPostResolutionAuth(ctx, s, result.ConversationID, ref, rctx); authErr != nil {
			return nil, authErr
		}
	}

	return result, nil
}

// checkPostResolutionAuth verifies the sender is authorised to access the
// resolved conversation based on its kind.
func checkPostResolutionAuth(ctx context.Context, s ResolutionStore, convID, ref string, rctx ResolveContext) error {
	conv, err := s.GetConversation(ctx, convID)
	if err != nil {
		return fmt.Errorf("loading conversation for auth check: %w", err)
	}

	switch conv.Kind {
	case "direct":
		// Direct conversations: derive auth from the kind-encoded DM key
		// rather than the participants table. This is strictly tighter than
		// a table scan — it checks BOTH kind AND ID, so a user UUID that
		// coincidentally matches an agent position is rejected.
		kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(conv.ExternalRef)
		if parseErr != nil {
			// Fail closed: unparseable key (old format, empty, corrupt) → deny.
			return &ResolutionError{Ref: ref, Reason: "not-a-participant"}
		}
		if (rctx.SenderPrincipalKind == kindA && rctx.SenderPrincipalID == idA) ||
			(rctx.SenderPrincipalKind == kindB && rctx.SenderPrincipalID == idB) {
			return nil // authorized
		}
		return &ResolutionError{Ref: ref, Reason: "not-a-participant"}
	case "group":
		// Group conversations are authorised by project membership, which is
		// already enforced by the project isolation check. No additional
		// participant check needed — agents must be able to post in rooms
		// they have never spoken in.
		return nil
	default:
		// Unknown kind — fail closed with participant check.
		return requireParticipant(ctx, s, conv.ID, ref, rctx)
	}
}

// requireParticipant checks that the sender is a participant in the given
// conversation. Returns a ResolutionError with reason "not-a-participant" if not.
func requireParticipant(ctx context.Context, s ResolutionStore, convID, ref string, rctx ResolveContext) error {
	participants, err := s.ListParticipants(ctx, convID)
	if err != nil {
		return fmt.Errorf("listing participants for auth check: %w", err)
	}
	for _, p := range participants {
		if p.PrincipalKind == rctx.SenderPrincipalKind && p.PrincipalID == rctx.SenderPrincipalID {
			return nil // sender is a participant
		}
	}
	return &ResolutionError{
		Ref:    ref,
		Reason: "not-a-participant",
	}
}

// resolveConvByID resolves a conv:<id> reference with project isolation checks.
func resolveConvByID(ctx context.Context, s ResolutionStore, id string, rctx ResolveContext) (*ResolveResult, error) {
	conv, err := s.GetConversation(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &ResolutionError{
				Ref:    "conv:" + id,
				Reason: "not-found",
			}
		}
		return nil, fmt.Errorf("fetching conversation %s: %w", id, err)
	}

	// Project isolation check (AC-30).
	if conv.ProjectID != nil && *conv.ProjectID != "" {
		if rctx.ProjectID != *conv.ProjectID {
			// The conversation belongs to a different project.
			// Check if the sender belongs to that other project.
			if senderBelongsToProject(ctx, s, rctx.SenderPrincipalKind, rctx.SenderPrincipalID, *conv.ProjectID) {
				// Sender can see that project — tell them it's a boundary violation.
				return nil, &ResolutionError{
					Ref:    "conv:" + id,
					Reason: "boundary-violation",
				}
			}
			// Sender does NOT belong to that project — return not-found to
			// prevent information leakage. The error message must be
			// identical to a genuine not-found.
			return nil, &ResolutionError{
				Ref:    "conv:" + id,
				Reason: "not-found",
			}
		}
	}
	// nil ProjectID means global DM — always allowed.

	return &ResolveResult{
		ConversationID: conv.ID,
		Created:        false,
	}, nil
}

// senderBelongsToProject checks whether the sender participates in any
// conversation within the given project. This is a heuristic proxy for
// "belongs to project" — a sender with at least one conversation in the
// project is considered a member.
func senderBelongsToProject(ctx context.Context, s ResolutionStore, principalKind, principalID, projectID string) bool {
	convs, err := s.GetConversationsForPrincipal(ctx, principalKind, principalID)
	if err != nil {
		return false
	}
	for _, c := range convs {
		if c.ProjectID != nil && *c.ProjectID == projectID {
			return true
		}
	}
	return false
}

// resolveAgentDM resolves an @<agent-slug> reference to a direct conversation.
// Creates the conversation if it doesn't exist (resolve-or-create).
//
// The project context is used ONLY to resolve the agent slug. The resulting
// DM conversation is global (nil ProjectID) — DMs are not project-scoped.
func resolveAgentDM(ctx context.Context, s ResolutionStore, slug string, rctx ResolveContext) (*ResolveResult, error) {
	if rctx.ProjectID == "" {
		return nil, &ResolutionError{
			Ref:    "@" + slug,
			Reason: "no-shared-project",
		}
	}

	// Resolve the agent slug within the current project.
	agent, err := s.GetAgentBySlug(ctx, rctx.ProjectID, slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &ResolutionError{
				Ref:    "@" + slug,
				Reason: "not-found",
			}
		}
		return nil, fmt.Errorf("resolving agent slug %q: %w", slug, err)
	}

	// Compute deterministic DM key.
	extRef, err := messages.DMConversationKey("agent", agent.ID, rctx.SenderPrincipalKind, rctx.SenderPrincipalID)
	if err != nil {
		return nil, fmt.Errorf("computing DM key for @%s: %w", slug, err)
	}

	// Upsert: find-or-create by (surface, external_ref). No ProjectID — DMs
	// are global, fixing DEF-10.
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
		DriftState:  DriftStateActive,
		// ProjectID intentionally nil — DMs are global.
	})
	if err != nil {
		return nil, fmt.Errorf("upserting DM conversation for @%s: %w", slug, err)
	}

	// Ensure both participants exist. AddParticipant returns
	// ErrAlreadyExists on a re-add — that is expected, not an error.
	created := ensureParticipant(ctx, s, conv.ID, rctx.SenderPrincipalKind, rctx.SenderPrincipalID)
	ensureParticipant(ctx, s, conv.ID, "agent", agent.ID)

	return &ResolveResult{
		ConversationID: conv.ID,
		Created:        created,
	}, nil
}

// resolveEmailDM resolves an @<email> reference to a global direct conversation.
// Creates the conversation if it doesn't exist.
func resolveEmailDM(ctx context.Context, s ResolutionStore, email string, rctx ResolveContext) (*ResolveResult, error) {
	// Resolve the user by email.
	user, err := s.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &ResolutionError{
				Ref:    "@" + email,
				Reason: "not-found",
			}
		}
		return nil, fmt.Errorf("resolving user email %q: %w", email, err)
	}

	// Compute deterministic DM key.
	extRef, err := messages.DMConversationKey("user", user.ID, rctx.SenderPrincipalKind, rctx.SenderPrincipalID)
	if err != nil {
		return nil, fmt.Errorf("computing DM key for @%s: %w", email, err)
	}

	// Upsert: find-or-create by (surface, external_ref). No ProjectID — email
	// DMs are global.
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
		DriftState:  DriftStateActive,
		// ProjectID intentionally nil — email DMs are global.
	})
	if err != nil {
		return nil, fmt.Errorf("upserting DM conversation for @%s: %w", email, err)
	}

	// Ensure both participants exist.
	created := ensureParticipant(ctx, s, conv.ID, rctx.SenderPrincipalKind, rctx.SenderPrincipalID)
	ensureParticipant(ctx, s, conv.ID, "user", user.ID)

	return &ResolveResult{
		ConversationID: conv.ID,
		Created:        created,
	}, nil
}

// resolveThread resolves a #<thread-name> reference within the current project.
func resolveThread(ctx context.Context, s ResolutionStore, name string, rctx ResolveContext) (*ResolveResult, error) {
	if rctx.ProjectID == "" {
		return nil, &ResolutionError{
			Ref:    "#" + name,
			Reason: "no-shared-project",
		}
	}

	// Paginate through all group conversations in this project to find one
	// matching the display name. A previous implementation passed Limit:0
	// expecting "no limit", but clampLimit(0) returns 50 (the default page
	// size), silently missing threads beyond the first page.
	var cursor string
	for {
		result, err := s.ListConversations(ctx, store.ConversationFilter{
			ProjectID: rctx.ProjectID,
			Kind:      "group",
		}, store.ListOptions{Limit: 100, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("listing conversations in project %s: %w", rctx.ProjectID, err)
		}

		for _, c := range result.Items {
			if c.DisplayName == name {
				return &ResolveResult{
					ConversationID: c.ID,
					Created:        false,
				}, nil
			}
		}

		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	return nil, &ResolutionError{
		Ref:    "#" + name,
		Reason: "not-found",
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ensureParticipant adds a participant to a conversation, returning true if
// the participant was newly added (i.e. the conversation was just created for
// this pair). ErrAlreadyExists is swallowed — re-adding an existing
// participant is expected on the upsert path.
//
// G2 EXCEPTION — same class as EnsureParticipant in conversation.go.
// Participants are a LISTING concern, not an access concern: authorization
// is key-derived (the DM key IS the ACL), not participant-derived. Denying
// a send because a listing row failed to write turns a cosmetic gap into
// an outage. Errors are logged as warnings and self-repair on the next
// message in the same conversation.
func ensureParticipant(ctx context.Context, s ResolutionStore, convID, kind, id string) bool {
	err := s.AddParticipant(ctx, &store.ConversationParticipant{
		ID:             uuid.NewString(),
		ConversationID: convID,
		PrincipalKind:  kind,
		PrincipalID:    id,
		Role:           "member",
	})
	if err != nil && errors.Is(err, store.ErrAlreadyExists) {
		return false // already a participant — not newly created
	}
	if err != nil {
		// Non-ErrAlreadyExists failure: participant listing gap. Auth is key-derived
		// so this is visibility, not access, but something that fails quietly and
		// often becomes normal.
		slog.WarnContext(ctx, "ensureParticipant: AddParticipant failed (listing gap)",
			"conversation_id", convID,
			"principal_kind", kind,
			"principal_id", id,
			"error", err)
		return false
	}
	return true // newly added
}

// ---------------------------------------------------------------------------
// Error types
// ---------------------------------------------------------------------------

// ResolutionError represents a resolution failure with a categorized reason.
type ResolutionError struct {
	Ref        string   // the reference that failed to resolve
	Reason     string   // "not-found" | "boundary-violation" | "ambiguous" | "no-shared-project" | "not-a-participant"
	Candidates []string // qualified forms that would work (for ambiguous)
}

func (e *ResolutionError) Error() string {
	switch e.Reason {
	case "not-found":
		return fmt.Sprintf("conversation reference %q: not found", e.Ref)
	case "boundary-violation":
		return fmt.Sprintf("conversation reference %q: belongs to a different project", e.Ref)
	case "ambiguous":
		return fmt.Sprintf("conversation reference %q: ambiguous, candidates: %v", e.Ref, e.Candidates)
	case "no-shared-project":
		return fmt.Sprintf("conversation reference %q: requires a project context", e.Ref)
	case "not-a-participant":
		return fmt.Sprintf("conversation reference %q: sender is not a participant", e.Ref)
	default:
		return fmt.Sprintf("conversation reference %q: %s", e.Ref, e.Reason)
	}
}

// IsNotFound returns true if the error is a resolution not-found error.
func (e *ResolutionError) IsNotFound() bool {
	return e.Reason == "not-found"
}

// IsBoundaryViolation returns true if the error is a project boundary violation.
func (e *ResolutionError) IsBoundaryViolation() bool {
	return e.Reason == "boundary-violation"
}
