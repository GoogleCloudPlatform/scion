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
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversation"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversationparticipant"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/messageaddressee"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ConversationStore implements store.ConversationStore using the Ent ORM.
type ConversationStore struct {
	client *ent.Client
}

// NewConversationStore creates a new Ent-backed ConversationStore.
func NewConversationStore(client *ent.Client) *ConversationStore {
	return &ConversationStore{client: client}
}

// ---------------------------------------------------------------------------
// Conversion helpers
// ---------------------------------------------------------------------------

func entConversationToStore(e *ent.Conversation) *store.Conversation {
	c := &store.Conversation{
		ID:             e.ID.String(),
		Kind:           string(e.Kind),
		Surface:        string(e.Surface),
		ExternalRef:    e.ExternalRef,
		ParentRef:      e.ParentRef,
		DisplayName:    e.DisplayName,
		DriftState:     string(e.DriftState),
		LastActivityAt: e.LastActivityAt,
		CreatedAt:      e.CreatedAt,
		ArchivedAt:     e.ArchivedAt,
		DeletedAt:      e.DeletedAt,
	}
	if e.ProjectID != nil {
		s := e.ProjectID.String()
		c.ProjectID = &s
	}
	if e.DefaultAgentID != nil {
		s := e.DefaultAgentID.String()
		c.DefaultAgentID = &s
	}
	return c
}

func entParticipantToStore(e *ent.ConversationParticipant) store.ConversationParticipant {
	return store.ConversationParticipant{
		ID:             e.ID.String(),
		ConversationID: e.ConversationID.String(),
		PrincipalKind:  string(e.PrincipalKind),
		PrincipalID:    e.PrincipalID,
		Role:           string(e.Role),
		JoinedAt:       e.JoinedAt,
		LeftAt:         e.LeftAt,
	}
}

func entAddresseeToStore(e *ent.MessageAddressee) store.MessageAddressee {
	return store.MessageAddressee{
		ID:            e.ID.String(),
		MessageID:     e.MessageID.String(),
		PrincipalKind: string(e.PrincipalKind),
		PrincipalID:   e.PrincipalID,
		Via:           string(e.Via),
		DeliveryState: string(e.DeliveryState),
		FailureReason: e.FailureReason,
	}
}

// validateDefaultAgentID checks that the provided DefaultAgentID is a valid UUID.
// Slugs are rejected — the store layer only accepts UUIDs.
func validateDefaultAgentID(id *string) (*uuid.UUID, error) {
	if id == nil {
		return nil, nil
	}
	uid, err := uuid.Parse(*id)
	if err != nil {
		return nil, fmt.Errorf("defaultAgentId must be a valid UUID, got %q: %w", *id, store.ErrInvalidInput)
	}
	return &uid, nil
}

// ---------------------------------------------------------------------------
// Conversation CRUD
// ---------------------------------------------------------------------------

// CreateConversation creates a new conversation.
func (s *ConversationStore) CreateConversation(ctx context.Context, conv *store.Conversation) error {
	if conv == nil {
		return fmt.Errorf("conversation is nil: %w", store.ErrInvalidInput)
	}
	// A direct conversation's external_ref IS the access control basis: the DM
	// key names who is entitled to see the messages. A keyless direct row has no
	// ACL at all. Group conversations may legitimately omit the external ref
	// (native groups have no upstream surface to point at).
	if conv.Kind == "direct" && conv.ExternalRef == "" {
		return fmt.Errorf("direct conversation requires a non-empty external_ref (the DM key is the access authority): %w", store.ErrInvalidInput)
	}
	if conv.ID == "" {
		return fmt.Errorf("conversation ID is required: %w", store.ErrInvalidInput)
	}
	uid, err := parseUUID(conv.ID)
	if err != nil {
		return err
	}

	agentUID, err := validateDefaultAgentID(conv.DefaultAgentID)
	if err != nil {
		return err
	}

	create := s.client.Conversation.Create().
		SetID(uid).
		SetKind(conversation.Kind(conv.Kind)).
		SetSurface(conversation.Surface(conv.Surface)).
		SetExternalRef(conv.ExternalRef).
		SetParentRef(conv.ParentRef).
		SetDisplayName(conv.DisplayName).
		SetDriftState(conversation.DriftState(conv.DriftState))

	if conv.ProjectID != nil {
		pid, err := parseUUID(*conv.ProjectID)
		if err != nil {
			return err
		}
		create.SetProjectID(pid)
	}
	if agentUID != nil {
		create.SetDefaultAgentID(*agentUID)
	}
	if !conv.LastActivityAt.IsZero() {
		create.SetLastActivityAt(conv.LastActivityAt)
	}
	if !conv.CreatedAt.IsZero() {
		create.SetCreatedAt(conv.CreatedAt)
	}
	if conv.ArchivedAt != nil {
		create.SetArchivedAt(*conv.ArchivedAt)
	}
	if conv.DeletedAt != nil {
		create.SetDeletedAt(*conv.DeletedAt)
	}
	// Default drift_state if not set.
	if conv.DriftState == "" {
		create.SetDriftState(conversation.DriftStateActive)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	conv.CreatedAt = created.CreatedAt
	conv.LastActivityAt = created.LastActivityAt
	conv.DriftState = string(created.DriftState)
	return nil
}

// GetConversation retrieves a conversation by ID, excluding soft-deleted rows.
func (s *ConversationStore) GetConversation(ctx context.Context, id string) (*store.Conversation, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.Conversation.Query().
		Where(
			conversation.IDEQ(uid),
			conversation.DeletedAtIsNil(),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entConversationToStore(e), nil
}

// UpdateConversation updates an existing conversation.
func (s *ConversationStore) UpdateConversation(ctx context.Context, conv *store.Conversation) error {
	if conv == nil {
		return fmt.Errorf("conversation is nil: %w", store.ErrInvalidInput)
	}
	uid, err := parseUUID(conv.ID)
	if err != nil {
		return err
	}

	agentUID, err := validateDefaultAgentID(conv.DefaultAgentID)
	if err != nil {
		return err
	}

	update := s.client.Conversation.UpdateOneID(uid).
		SetKind(conversation.Kind(conv.Kind)).
		SetSurface(conversation.Surface(conv.Surface)).
		SetExternalRef(conv.ExternalRef).
		SetParentRef(conv.ParentRef).
		SetDisplayName(conv.DisplayName).
		SetDriftState(conversation.DriftState(conv.DriftState)).
		SetLastActivityAt(conv.LastActivityAt)

	if conv.ProjectID != nil {
		pid, err := parseUUID(*conv.ProjectID)
		if err != nil {
			return err
		}
		update.SetProjectID(pid)
	} else {
		update.ClearProjectID()
	}
	if agentUID != nil {
		update.SetDefaultAgentID(*agentUID)
	} else {
		update.ClearDefaultAgentID()
	}
	if conv.ArchivedAt != nil {
		update.SetArchivedAt(*conv.ArchivedAt)
	} else {
		update.ClearArchivedAt()
	}
	if conv.DeletedAt != nil {
		update.SetDeletedAt(*conv.DeletedAt)
	} else {
		update.ClearDeletedAt()
	}

	_, err = update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteConversation soft-deletes a conversation by setting deleted_at.
func (s *ConversationStore) DeleteConversation(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	now := time.Now()
	n, err := s.client.Conversation.Update().
		Where(
			conversation.IDEQ(uid),
			conversation.DeletedAtIsNil(),
		).
		SetDeletedAt(now).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListConversations returns conversations matching the filter, excluding
// soft-deleted rows. Supports cursor-based pagination.
func (s *ConversationStore) ListConversations(ctx context.Context, filter store.ConversationFilter, opts store.ListOptions) (*store.ListResult[store.Conversation], error) {
	query := s.client.Conversation.Query().
		Where(conversation.DeletedAtIsNil())

	if filter.ProjectID != "" {
		pid, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		query.Where(conversation.ProjectIDEQ(pid))
	}
	if filter.Kind != "" {
		query.Where(conversation.KindEQ(conversation.Kind(filter.Kind)))
	}
	if filter.Surface != "" {
		query.Where(conversation.SurfaceEQ(conversation.Surface(filter.Surface)))
	}
	if filter.DriftState != "" {
		query.Where(conversation.DriftStateEQ(conversation.DriftState(filter.DriftState)))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	// Cursor-based keyset pagination using the same encoding as message_store.
	if opts.Cursor != "" {
		cursorCreated, cursorID, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		query.Where(conversation.Or(
			conversation.LastActivityAtLT(cursorCreated),
			conversation.And(
				conversation.LastActivityAtEQ(cursorCreated),
				conversation.IDLT(cursorID),
			),
		))
	}

	limit := clampLimit(opts.Limit)
	entities, err := query.
		Order(conversation.ByLastActivityAt(entsql.OrderDesc())).
		Order(conversation.ByID(entsql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.Conversation, 0, len(entities))
	for _, e := range entities {
		items = append(items, *entConversationToStore(e))
	}

	result := &store.ListResult[store.Conversation]{TotalCount: totalCount}
	if len(items) > limit {
		result.Items = items[:limit]
		last := items[limit-1]
		result.NextCursor = encodeCursor(last.LastActivityAt, last.ID)
	} else {
		result.Items = items
	}
	return result, nil
}

// GetConversationByExternalRef looks up a conversation by (surface, external_ref).
// Returns store.ErrNotFound if no matching active (non-deleted) conversation exists.
func (s *ConversationStore) GetConversationByExternalRef(ctx context.Context, surface, externalRef string) (*store.Conversation, error) {
	if externalRef == "" {
		return nil, fmt.Errorf("externalRef is required: %w", store.ErrInvalidInput)
	}
	if surface == "" {
		surface = "native"
	}

	e, err := s.client.Conversation.Query().
		Where(
			conversation.SurfaceEQ(conversation.Surface(surface)),
			conversation.ExternalRefEQ(externalRef),
			conversation.DeletedAtIsNil(),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return entConversationToStore(e), nil
}

// UpsertConversationByExternalRef creates or updates a conversation keyed on
// (surface, external_ref). The partial unique index in the schema is the guard
// for concurrency safety.
//
// Implementation note: SQLite does not support ON CONFLICT with partial unique
// indexes, so we implement this as a query-then-create/update pattern. The
// partial unique index still prevents concurrent inserts from creating
// duplicates; a constraint-violation on insert triggers a bounded retry that
// updates the existing row instead.
func (s *ConversationStore) UpsertConversationByExternalRef(ctx context.Context, conv *store.Conversation) (*store.Conversation, error) {
	if conv == nil {
		return nil, fmt.Errorf("conversation is nil: %w", store.ErrInvalidInput)
	}
	if conv.ExternalRef == "" {
		return nil, fmt.Errorf("externalRef is required for upsert: %w", store.ErrInvalidInput)
	}

	agentUID, err := validateDefaultAgentID(conv.DefaultAgentID)
	if err != nil {
		return nil, err
	}

	const maxRetries = 3
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Look for an existing non-deleted conversation with the same (surface, external_ref).
		existing, err := s.client.Conversation.Query().
			Where(
				conversation.SurfaceEQ(conversation.Surface(conv.Surface)),
				conversation.ExternalRefEQ(conv.ExternalRef),
				conversation.DeletedAtIsNil(),
			).
			Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return nil, err
		}

		if existing != nil {
			// Update the existing row.
			update := s.client.Conversation.UpdateOneID(existing.ID).
				SetLastActivityAt(time.Now())

			// Only update optional fields if a non-empty value is provided,
			// preventing accidental clobber of existing values.
			if conv.ParentRef != "" {
				update.SetParentRef(conv.ParentRef)
			}
			if conv.DisplayName != "" {
				update.SetDisplayName(conv.DisplayName)
			}

			if conv.DriftState != "" {
				update.SetDriftState(conversation.DriftState(conv.DriftState))
			}
			if conv.ProjectID != nil {
				pid, pErr := parseUUID(*conv.ProjectID)
				if pErr != nil {
					return nil, pErr
				}
				update.SetProjectID(pid)
			}
			if agentUID != nil {
				update.SetDefaultAgentID(*agentUID)
			}

			updated, uErr := update.Save(ctx)
			if uErr != nil {
				return nil, uErr
			}
			return entConversationToStore(updated), nil
		}

		// No existing row — create a new one.
		uid := uuid.New()
		if conv.ID != "" {
			uid, err = parseUUID(conv.ID)
			if err != nil {
				return nil, err
			}
		}

		driftState := conversation.DriftState(conv.DriftState)
		if conv.DriftState == "" {
			driftState = conversation.DriftStateActive
		}

		create := s.client.Conversation.Create().
			SetID(uid).
			SetKind(conversation.Kind(conv.Kind)).
			SetSurface(conversation.Surface(conv.Surface)).
			SetExternalRef(conv.ExternalRef).
			SetParentRef(conv.ParentRef).
			SetDisplayName(conv.DisplayName).
			SetDriftState(driftState)

		if conv.ProjectID != nil {
			pid, pErr := parseUUID(*conv.ProjectID)
			if pErr != nil {
				return nil, pErr
			}
			create.SetProjectID(pid)
		}
		if agentUID != nil {
			create.SetDefaultAgentID(*agentUID)
		}
		if !conv.LastActivityAt.IsZero() {
			create.SetLastActivityAt(conv.LastActivityAt)
		}

		created, cErr := create.Save(ctx)
		if cErr != nil {
			// If we hit a unique constraint violation (race with a concurrent insert),
			// retry by looping back to query the now-existing row.
			if isUniqueConstraintError(cErr) {
				lastErr = cErr
				continue
			}
			return nil, mapError(cErr)
		}
		return entConversationToStore(created), nil
	}

	return nil, fmt.Errorf("upsert failed after %d retries: %w", maxRetries, lastErr)
}

// isUniqueConstraintError checks if the error is a unique constraint violation.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	// ent wraps constraint errors — check the error message for both SQLite
	// and Postgres constraint violation patterns.
	errStr := err.Error()
	return strings.Contains(errStr, "UNIQUE constraint failed") ||
		strings.Contains(errStr, "duplicate key value violates unique constraint") ||
		strings.Contains(errStr, "unique_violation")
}

// ---------------------------------------------------------------------------
// Participant operations
// ---------------------------------------------------------------------------

// checkDMParticipantKey is the shared immutability guard for direct
// conversations. Only principals named in the kind-encoded DM key may be
// added or ensured. Both AddParticipant and EnsureParticipant route through
// this predicate so the guard cannot diverge across call sites.
//
// For non-direct conversations the function is a no-op (returns nil).
func checkDMParticipantKey(conv *ent.Conversation, principalKind, principalID string) error {
	if conv == nil {
		return fmt.Errorf("%w: conversation is nil", store.ErrInvalidInput)
	}
	if err := messages.CheckDMParticipantKey(string(conv.Kind), conv.ExternalRef, principalKind, principalID); err != nil {
		return fmt.Errorf("%w: %w", store.ErrInvalidInput, err)
	}
	return nil
}

// AddParticipant adds a principal to a conversation.
// If a soft-removed participant with the same (conversation_id, principal_kind,
// principal_id) exists (left_at IS NOT NULL), the row is re-activated instead
// of inserting a duplicate.
func (s *ConversationStore) AddParticipant(ctx context.Context, p *store.ConversationParticipant) error {
	if p.ConversationID == "" || p.PrincipalID == "" {
		return fmt.Errorf("conversationID and principalID are required: %w", store.ErrInvalidInput)
	}
	convUID, err := parseUUID(p.ConversationID)
	if err != nil {
		return err
	}

	// Immutability guard for direct conversations: only principals named in
	// the kind-encoded DM key may be added. A count-based guard (count>=2)
	// is wrong because soft-remove B → count drops to 1 → AddParticipant(C)
	// would pass → conversation has {A, C} while key says {A, B}. The
	// key-derived guard prevents this regardless of current participant count.
	conv, err := s.client.Conversation.Get(ctx, convUID)
	if err != nil {
		return fmt.Errorf("loading conversation for participant guard: %w", err)
	}
	if err := checkDMParticipantKey(conv, p.PrincipalKind, p.PrincipalID); err != nil {
		return err
	}

	// Check for a soft-removed participant that can be re-joined.
	existing, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.PrincipalKindEQ(conversationparticipant.PrincipalKind(p.PrincipalKind)),
			conversationparticipant.PrincipalIDEQ(p.PrincipalID),
			conversationparticipant.LeftAtNotNil(),
		).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return err
	}

	if existing != nil {
		// Re-join: clear left_at and update role.
		role := conversationparticipant.Role(p.Role)
		if p.Role == "" {
			role = conversationparticipant.RoleMember
		}
		updated, uErr := s.client.ConversationParticipant.UpdateOneID(existing.ID).
			ClearLeftAt().
			SetRole(role).
			Save(ctx)
		if uErr != nil {
			return uErr
		}
		p.ID = updated.ID.String()
		p.JoinedAt = updated.JoinedAt
		p.LeftAt = nil
		return nil
	}

	create := s.client.ConversationParticipant.Create().
		SetConversationID(convUID).
		SetPrincipalKind(conversationparticipant.PrincipalKind(p.PrincipalKind)).
		SetPrincipalID(p.PrincipalID).
		SetRole(conversationparticipant.Role(p.Role))

	if p.ID != "" {
		uid, err := parseUUID(p.ID)
		if err != nil {
			return err
		}
		create.SetID(uid)
	}
	if !p.JoinedAt.IsZero() {
		create.SetJoinedAt(p.JoinedAt)
	}
	if p.Role == "" {
		create.SetRole(conversationparticipant.RoleMember)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	p.ID = created.ID.String()
	p.JoinedAt = created.JoinedAt
	return nil
}

// EnsureParticipant ensures a participant row exists for the given principal.
// If the participant already exists (active or soft-removed), the row is left
// completely untouched — including left_at. Unlike AddParticipant, this does
// NOT clear left_at on existing rows.
//
// Semantics: after a successful call the participant row exists. Already-existing
// rows (whether active or soft-removed) are not modified and return nil, not an
// error. A race-induced unique-constraint violation on insert also returns nil.
func (s *ConversationStore) EnsureParticipant(ctx context.Context, p *store.ConversationParticipant) error {
	if p.ConversationID == "" || p.PrincipalID == "" {
		return fmt.Errorf("conversationID and principalID are required: %w", store.ErrInvalidInput)
	}
	convUID, err := parseUUID(p.ConversationID)
	if err != nil {
		return err
	}

	// D-1 guard: shared predicate with AddParticipant.
	conv, err := s.client.Conversation.Get(ctx, convUID)
	if err != nil {
		return fmt.Errorf("loading conversation for participant guard: %w", err)
	}
	if err := checkDMParticipantKey(conv, p.PrincipalKind, p.PrincipalID); err != nil {
		return err
	}

	// Check for ANY existing row (active or soft-removed).
	existing, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.PrincipalKindEQ(conversationparticipant.PrincipalKind(p.PrincipalKind)),
			conversationparticipant.PrincipalIDEQ(p.PrincipalID),
		).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return err
	}
	if existing != nil {
		// Row exists — leave it untouched (including left_at).
		// Read back ID and JoinedAt so the caller's struct matches
		// AddParticipant's post-condition on both paths.
		p.ID = existing.ID.String()
		p.JoinedAt = existing.JoinedAt
		return nil
	}

	// No row exists — create it.
	create := s.client.ConversationParticipant.Create().
		SetConversationID(convUID).
		SetPrincipalKind(conversationparticipant.PrincipalKind(p.PrincipalKind)).
		SetPrincipalID(p.PrincipalID).
		SetRole(conversationparticipant.Role(p.Role))

	if p.ID != "" {
		uid, pErr := parseUUID(p.ID)
		if pErr != nil {
			return pErr
		}
		create.SetID(uid)
	}
	if !p.JoinedAt.IsZero() {
		create.SetJoinedAt(p.JoinedAt)
	}
	if p.Role == "" {
		create.SetRole(conversationparticipant.RoleMember)
	}

	created, err := create.Save(ctx)
	if err != nil {
		// Race-safe: a concurrent insert may have created the row between
		// our query and this insert. Treat that as success.
		if isUniqueConstraintError(err) {
			return nil
		}
		return mapError(err)
	}
	p.ID = created.ID.String()
	p.JoinedAt = created.JoinedAt
	return nil
}

// RemoveParticipant soft-removes a participant by setting left_at.
func (s *ConversationStore) RemoveParticipant(ctx context.Context, conversationID, principalKind, principalID string) error {
	convUID, err := parseUUID(conversationID)
	if err != nil {
		return err
	}
	now := time.Now()
	n, err := s.client.ConversationParticipant.Update().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.PrincipalKindEQ(conversationparticipant.PrincipalKind(principalKind)),
			conversationparticipant.PrincipalIDEQ(principalID),
			conversationparticipant.LeftAtIsNil(),
		).
		SetLeftAt(now).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListParticipants returns active participants (left_at IS NULL).
func (s *ConversationStore) ListParticipants(ctx context.Context, conversationID string) ([]store.ConversationParticipant, error) {
	convUID, err := parseUUID(conversationID)
	if err != nil {
		return nil, err
	}
	entities, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.ConversationIDEQ(convUID),
			conversationparticipant.LeftAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]store.ConversationParticipant, 0, len(entities))
	for _, e := range entities {
		result = append(result, entParticipantToStore(e))
	}
	return result, nil
}

// GetConversationsForPrincipal returns conversations a principal participates in
// (active participation: left_at IS NULL, conversation not soft-deleted).
func (s *ConversationStore) GetConversationsForPrincipal(ctx context.Context, principalKind, principalID string) ([]store.Conversation, error) {
	// First find active participation records.
	participants, err := s.client.ConversationParticipant.Query().
		Where(
			conversationparticipant.PrincipalKindEQ(conversationparticipant.PrincipalKind(principalKind)),
			conversationparticipant.PrincipalIDEQ(principalID),
			conversationparticipant.LeftAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(participants) == 0 {
		return nil, nil
	}

	// Collect conversation IDs.
	convIDs := make([]uuid.UUID, 0, len(participants))
	for _, p := range participants {
		convIDs = append(convIDs, p.ConversationID)
	}

	// Fetch non-deleted conversations.
	entities, err := s.client.Conversation.Query().
		Where(
			conversation.IDIn(convIDs...),
			conversation.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]store.Conversation, 0, len(entities))
	for _, e := range entities {
		result = append(result, *entConversationToStore(e))
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Addressee operations
// ---------------------------------------------------------------------------

// AddAddressee adds an addressee record to a message.
func (s *ConversationStore) AddAddressee(ctx context.Context, a *store.MessageAddressee) error {
	if a.MessageID == "" || a.PrincipalID == "" {
		return fmt.Errorf("messageID and principalID are required: %w", store.ErrInvalidInput)
	}
	msgUID, err := parseUUID(a.MessageID)
	if err != nil {
		return err
	}

	create := s.client.MessageAddressee.Create().
		SetMessageID(msgUID).
		SetPrincipalKind(messageaddressee.PrincipalKind(a.PrincipalKind)).
		SetPrincipalID(a.PrincipalID).
		SetVia(messageaddressee.Via(a.Via)).
		SetDeliveryState(messageaddressee.DeliveryState(a.DeliveryState))

	if a.ID != "" {
		uid, err := parseUUID(a.ID)
		if err != nil {
			return err
		}
		create.SetID(uid)
	}
	if a.FailureReason != nil {
		create.SetFailureReason(*a.FailureReason)
	}
	if a.DeliveryState == "" {
		create.SetDeliveryState(messageaddressee.DeliveryStatePending)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	a.ID = created.ID.String()
	return nil
}

// ListAddressees returns all addressees for a message.
func (s *ConversationStore) ListAddressees(ctx context.Context, messageID string) ([]store.MessageAddressee, error) {
	msgUID, err := parseUUID(messageID)
	if err != nil {
		return nil, err
	}
	entities, err := s.client.MessageAddressee.Query().
		Where(messageaddressee.MessageIDEQ(msgUID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]store.MessageAddressee, 0, len(entities))
	for _, e := range entities {
		result = append(result, entAddresseeToStore(e))
	}
	return result, nil
}

// UpdateDeliveryState updates the delivery state of an addressee.
func (s *ConversationStore) UpdateDeliveryState(ctx context.Context, id string, state string, failureReason *string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	update := s.client.MessageAddressee.UpdateOneID(uid).
		SetDeliveryState(messageaddressee.DeliveryState(state))
	if failureReason != nil {
		update.SetFailureReason(*failureReason)
	} else {
		update.ClearFailureReason()
	}
	_, err = update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}
