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
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/accessconstraint"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AccessConstraintStore implements store.AccessConstraintStore using Ent ORM.
type AccessConstraintStore struct {
	client      *ent.Client
	dialectOnce sync.Once
	dialectName string
}

// NewAccessConstraintStore creates a new Ent-backed AccessConstraintStore.
func NewAccessConstraintStore(client *ent.Client) *AccessConstraintStore {
	return &AccessConstraintStore{client: client}
}

// usesRowLocks reports whether the backend supports SELECT ... FOR UPDATE.
func (s *AccessConstraintStore) usesRowLocks(ctx context.Context) bool {
	s.dialectOnce.Do(func() {
		_, _ = s.client.AccessConstraint.Query().
			Where(func(sel *entsql.Selector) { s.dialectName = sel.Dialect() }).
			Exist(ctx)
	})
	return s.dialectName == dialect.Postgres
}

// entAccessConstraintToStore converts an Ent AccessConstraint entity to a
// store.AccessConstraint model.
func entAccessConstraintToStore(e *ent.AccessConstraint) *store.AccessConstraint {
	result := &store.AccessConstraint{
		ID:                 e.ID.String(),
		Name:               e.Name,
		SubjectKind:        string(e.SubjectKind),
		ScopeType:          string(e.ScopeType),
		ScopeID:            e.ScopeID,
		MaximumPermissions: e.MaximumPermissions,
		NotBefore:          e.NotBefore,
		ExpiresAt:          e.ExpiresAt,
		Disabled:           e.Disabled,
		Revision:           e.Revision,
		Purpose:            e.Purpose,
		CreatedBy:          e.CreatedBy,
		CreatedAt:          e.Created,
		UpdatedAt:          e.Updated,
	}
	if e.SubjectPrincipalType != nil {
		result.SubjectPrincipalType = e.SubjectPrincipalType
	}
	if e.SubjectPrincipalID != nil {
		result.SubjectPrincipalID = e.SubjectPrincipalID
	}
	if e.SubjectGroupID != nil {
		result.SubjectGroupID = e.SubjectGroupID
	}
	if e.UpdatedBy != nil {
		result.UpdatedBy = *e.UpdatedBy
	}
	return result
}

// validateReferences checks that referenced entities (user, agent, group,
// project) exist in the database. Runs inside a transaction.
func (s *AccessConstraintStore) validateReferences(ctx context.Context, tx *ent.Tx, c *store.AccessConstraint) error {
	switch c.SubjectKind {
	case store.ConstraintSubjectPrincipal:
		if c.SubjectPrincipalType == nil || c.SubjectPrincipalID == nil {
			return fmt.Errorf("principal type and ID required for principal subject: %w", store.ErrInvalidInput)
		}
		principalID, err := uuid.Parse(*c.SubjectPrincipalID)
		if err != nil {
			return fmt.Errorf("invalid principal ID %q: %w", *c.SubjectPrincipalID, store.ErrInvalidInput)
		}
		switch *c.SubjectPrincipalType {
		case store.ConstraintPrincipalTypeUser:
			exists, err := tx.User.Query().Where(func(sel *entsql.Selector) {
				sel.Where(entsql.EQ("id", principalID))
			}).Exist(ctx)
			if err != nil {
				return fmt.Errorf("check user existence: %w", err)
			}
			if !exists {
				return fmt.Errorf("user %s not found: %w", *c.SubjectPrincipalID, store.ErrNotFound)
			}
		case store.ConstraintPrincipalTypeAgent:
			exists, err := tx.Agent.Query().Where(func(sel *entsql.Selector) {
				sel.Where(entsql.EQ("id", principalID))
			}).Exist(ctx)
			if err != nil {
				return fmt.Errorf("check agent existence: %w", err)
			}
			if !exists {
				return fmt.Errorf("agent %s not found: %w", *c.SubjectPrincipalID, store.ErrNotFound)
			}
		case store.ConstraintPrincipalTypeGroup:
			exists, err := tx.Group.Query().Where(func(sel *entsql.Selector) {
				sel.Where(entsql.EQ("id", principalID))
			}).Exist(ctx)
			if err != nil {
				return fmt.Errorf("check group existence: %w", err)
			}
			if !exists {
				return fmt.Errorf("group %s not found: %w", *c.SubjectPrincipalID, store.ErrNotFound)
			}
		default:
			return fmt.Errorf("invalid principal type %q: %w", *c.SubjectPrincipalType, store.ErrInvalidInput)
		}

	case store.ConstraintSubjectGroupClosure:
		if c.SubjectGroupID == nil {
			return fmt.Errorf("group ID required for group_closure subject: %w", store.ErrInvalidInput)
		}
		groupID, err := uuid.Parse(*c.SubjectGroupID)
		if err != nil {
			return fmt.Errorf("invalid group ID %q: %w", *c.SubjectGroupID, store.ErrInvalidInput)
		}
		exists, err := tx.Group.Query().Where(func(sel *entsql.Selector) {
			sel.Where(entsql.EQ("id", groupID))
		}).Exist(ctx)
		if err != nil {
			return fmt.Errorf("check group existence: %w", err)
		}
		if !exists {
			return fmt.Errorf("group %s not found: %w", *c.SubjectGroupID, store.ErrNotFound)
		}

	case store.ConstraintSubjectAllPrincipals:
		// No reference to validate.
	}

	// Validate project scope reference.
	if c.ScopeType == "project" && c.ScopeID != "" {
		projectID, err := uuid.Parse(c.ScopeID)
		if err != nil {
			return fmt.Errorf("invalid project ID %q: %w", c.ScopeID, store.ErrInvalidInput)
		}
		exists, err := tx.Project.Query().Where(func(sel *entsql.Selector) {
			sel.Where(entsql.EQ("id", projectID))
		}).Exist(ctx)
		if err != nil {
			return fmt.Errorf("check project existence: %w", err)
		}
		if !exists {
			return fmt.Errorf("project %s not found: %w", c.ScopeID, store.ErrNotFound)
		}
	}

	return nil
}

// CreateAccessConstraint creates a new access constraint with reference
// validation inside a transaction.
func (s *AccessConstraintStore) CreateAccessConstraint(ctx context.Context, c *store.AccessConstraint) (*store.AccessConstraint, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Validate references inside the transaction.
	if err := s.validateReferences(ctx, tx, c); err != nil {
		return nil, err
	}

	builder := tx.AccessConstraint.Create().
		SetName(c.Name).
		SetSubjectKind(accessconstraint.SubjectKind(c.SubjectKind)).
		SetScopeType(accessconstraint.ScopeType(c.ScopeType)).
		SetScopeID(c.ScopeID).
		SetMaximumPermissions(c.MaximumPermissions).
		SetDisabled(c.Disabled).
		SetCreatedBy(c.CreatedBy).
		SetRevision(1)

	// Only set purpose when non-empty; let the Ent schema default apply otherwise.
	if c.Purpose != "" {
		builder.SetPurpose(c.Purpose)
	}

	if c.SubjectPrincipalType != nil {
		builder.SetSubjectPrincipalType(*c.SubjectPrincipalType)
	}
	if c.SubjectPrincipalID != nil {
		builder.SetSubjectPrincipalID(*c.SubjectPrincipalID)
	}
	if c.SubjectGroupID != nil {
		builder.SetSubjectGroupID(*c.SubjectGroupID)
	}
	if c.NotBefore != nil {
		builder.SetNotBefore(*c.NotBefore)
	}
	if c.ExpiresAt != nil {
		builder.SetExpiresAt(*c.ExpiresAt)
	}
	if c.UpdatedBy != "" {
		builder.SetUpdatedBy(c.UpdatedBy)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return entAccessConstraintToStore(created), nil
}

// GetAccessConstraint retrieves an access constraint by ID.
func (s *AccessConstraintStore) GetAccessConstraint(ctx context.Context, id string) (*store.AccessConstraint, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.AccessConstraint.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entAccessConstraintToStore(e), nil
}

// UpdateAccessConstraint updates an existing access constraint with reference
// validation and optimistic concurrency control.
// If expectedRevision > 0, returns ErrRevisionConflict if the stored revision
// differs. Revision is always incremented atomically.
func (s *AccessConstraintStore) UpdateAccessConstraint(ctx context.Context, c *store.AccessConstraint, expectedRevision int64) (*store.AccessConstraint, error) {
	uid, err := parseGetID(c.ID)
	if err != nil {
		return nil, err
	}

	// Detect dialect before starting a transaction to avoid deadlocking on
	// single-connection SQLite backends (MaxOpenConns=1).
	rowLocks := s.usesRowLocks(ctx)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read existing constraint inside transaction for optimistic concurrency.
	q := tx.AccessConstraint.Query().Where(accessconstraint.IDEQ(uid))
	if rowLocks {
		q = q.ForUpdate()
	}
	existing, err := q.Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	// Check optimistic concurrency if expectedRevision is provided.
	if expectedRevision > 0 && existing.Revision != expectedRevision {
		return nil, store.ErrRevisionConflict
	}

	// Validate references inside the transaction.
	if err := s.validateReferences(ctx, tx, c); err != nil {
		return nil, err
	}

	builder := tx.AccessConstraint.UpdateOneID(uid).
		SetName(c.Name).
		SetSubjectKind(accessconstraint.SubjectKind(c.SubjectKind)).
		SetScopeType(accessconstraint.ScopeType(c.ScopeType)).
		SetScopeID(c.ScopeID).
		SetMaximumPermissions(c.MaximumPermissions).
		SetDisabled(c.Disabled).
		SetRevision(existing.Revision + 1)

	// Only set purpose when non-empty; preserve existing otherwise.
	if c.Purpose != "" {
		builder.SetPurpose(c.Purpose)
	}

	if c.SubjectPrincipalType != nil {
		builder.SetSubjectPrincipalType(*c.SubjectPrincipalType)
	} else {
		builder.ClearSubjectPrincipalType()
	}
	if c.SubjectPrincipalID != nil {
		builder.SetSubjectPrincipalID(*c.SubjectPrincipalID)
	} else {
		builder.ClearSubjectPrincipalID()
	}
	if c.SubjectGroupID != nil {
		builder.SetSubjectGroupID(*c.SubjectGroupID)
	} else {
		builder.ClearSubjectGroupID()
	}
	if c.NotBefore != nil {
		builder.SetNotBefore(*c.NotBefore)
	} else {
		builder.ClearNotBefore()
	}
	if c.ExpiresAt != nil {
		builder.SetExpiresAt(*c.ExpiresAt)
	} else {
		builder.ClearExpiresAt()
	}
	if c.UpdatedBy != "" {
		builder.SetUpdatedBy(c.UpdatedBy)
	} else {
		builder.ClearUpdatedBy()
	}

	updated, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return entAccessConstraintToStore(updated), nil
}

// DeleteAccessConstraint deletes an access constraint by ID.
func (s *AccessConstraintStore) DeleteAccessConstraint(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	err = s.client.AccessConstraint.DeleteOneID(uid).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// ListAccessConstraints returns all access constraints with offset-based
// pagination. Kept for backward compatibility with loadAllAccessConstraints.
func (s *AccessConstraintStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	entities, err := s.client.AccessConstraint.Query().
		Order(ent.Asc(accessconstraint.FieldCreated)).
		Limit(limit).
		Offset(offset).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]*store.AccessConstraint, len(entities))
	for i, e := range entities {
		result[i] = entAccessConstraintToStore(e)
	}
	return result, nil
}

// ListAccessConstraintsFiltered returns access constraints with SQL-level
// filtering, sorting, and cursor-based pagination.
func (s *AccessConstraintStore) ListAccessConstraintsFiltered(ctx context.Context, opts store.AccessConstraintListOptions) ([]*store.AccessConstraint, string, int, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 1000 {
		pageSize = 1000
	}

	// Build filter predicates.
	var preds []predicate.AccessConstraint

	if opts.SubjectKind != "" {
		preds = append(preds, accessconstraint.SubjectKindEQ(accessconstraint.SubjectKind(opts.SubjectKind)))
	}
	if opts.SubjectPrincipalType != "" {
		preds = append(preds, accessconstraint.SubjectPrincipalTypeEQ(opts.SubjectPrincipalType))
	}
	if opts.ScopeType != "" {
		preds = append(preds, accessconstraint.ScopeTypeEQ(accessconstraint.ScopeType(opts.ScopeType)))
	}
	if opts.ScopeID != "" {
		preds = append(preds, accessconstraint.ScopeIDEQ(opts.ScopeID))
	}
	if opts.NameContains != "" {
		preds = append(preds, accessconstraint.NameContainsFold(opts.NameContains))
	}

	// Status filter (derived from disabled flag and time window).
	now := time.Now()
	switch opts.Status {
	case "active":
		// Not disabled, not_before <= now (or null), expires_at > now (or null)
		preds = append(preds, accessconstraint.DisabledEQ(false))
		preds = append(preds, accessconstraint.Or(
			accessconstraint.NotBeforeIsNil(),
			accessconstraint.NotBeforeLTE(now),
		))
		preds = append(preds, accessconstraint.Or(
			accessconstraint.ExpiresAtIsNil(),
			accessconstraint.ExpiresAtGT(now),
		))
	case "scheduled":
		// Not disabled, not_before > now
		preds = append(preds, accessconstraint.DisabledEQ(false))
		preds = append(preds, accessconstraint.NotBeforeGT(now))
	case "expired":
		// Not disabled, expires_at <= now
		preds = append(preds, accessconstraint.DisabledEQ(false))
		preds = append(preds, accessconstraint.ExpiresAtNotNil())
		preds = append(preds, accessconstraint.ExpiresAtLTE(now))
	case "recovery_disabled":
		preds = append(preds, accessconstraint.DisabledEQ(true))
	}

	// Determine sort field and order.
	sortField := accessconstraint.FieldCreated
	switch opts.SortBy {
	case "name":
		sortField = accessconstraint.FieldName
	case "updated":
		sortField = accessconstraint.FieldUpdated
	case "created":
		sortField = accessconstraint.FieldCreated
	}

	sortDesc := strings.EqualFold(opts.SortOrder, "desc")

	// Count total matching records.
	totalQuery := s.client.AccessConstraint.Query()
	if len(preds) > 0 {
		totalQuery = totalQuery.Where(preds...)
	}
	totalCount, err := totalQuery.Count(ctx)
	if err != nil {
		return nil, "", 0, mapError(err)
	}

	// Build paginated query.
	query := s.client.AccessConstraint.Query()
	if len(preds) > 0 {
		query = query.Where(preds...)
	}

	// Apply cursor.
	if opts.PageToken != "" {
		cursorVal, cursorID, err := decodeConstraintCursor(opts.PageToken)
		if err != nil {
			return nil, "", 0, fmt.Errorf("invalid page token: %w", err)
		}
		// Keyset pagination: for asc, get records where (sort_field, id) > (cursor_val, cursor_id)
		if sortDesc {
			query = query.Where(accessconstraint.Or(
				constraintFieldLT(sortField, cursorVal),
				accessconstraint.And(
					constraintFieldEQ(sortField, cursorVal),
					accessconstraint.IDLT(cursorID),
				),
			))
		} else {
			query = query.Where(accessconstraint.Or(
				constraintFieldGT(sortField, cursorVal),
				accessconstraint.And(
					constraintFieldEQ(sortField, cursorVal),
					accessconstraint.IDGT(cursorID),
				),
			))
		}
	}

	// Apply ordering.
	if sortDesc {
		query = query.Order(ent.Desc(sortField), ent.Desc(accessconstraint.FieldID))
	} else {
		query = query.Order(ent.Asc(sortField), ent.Asc(accessconstraint.FieldID))
	}

	// Fetch one extra to detect if there's a next page.
	entities, err := query.Limit(pageSize + 1).All(ctx)
	if err != nil {
		return nil, "", 0, mapError(err)
	}

	hasNextPage := len(entities) > pageSize
	if hasNextPage {
		entities = entities[:pageSize]
	}

	result := make([]*store.AccessConstraint, len(entities))
	for i, e := range entities {
		result[i] = entAccessConstraintToStore(e)
	}

	var nextPageToken string
	if hasNextPage && len(entities) > 0 {
		last := entities[len(entities)-1]
		var cursorVal string
		switch sortField {
		case accessconstraint.FieldUpdated:
			cursorVal = last.Updated.Format(time.RFC3339Nano)
		case accessconstraint.FieldName:
			cursorVal = last.Name
		default:
			cursorVal = last.Created.Format(time.RFC3339Nano)
		}
		nextPageToken = encodeConstraintCursor(cursorVal, last.ID.String())
	}

	return result, nextPageToken, totalCount, nil
}

// CountAccessConstraints returns the total number of access constraints.
func (s *AccessConstraintStore) CountAccessConstraints(ctx context.Context) (int, error) {
	count, err := s.client.AccessConstraint.Query().Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return count, nil
}

// ResolveApplicableConstraints returns all constraints that may apply to the
// given principals and scopes.
func (s *AccessConstraintStore) ResolveApplicableConstraints(
	ctx context.Context,
	principals []store.PrincipalRef,
	scopeTypes []string,
	scopeIDs []string,
) ([]*store.AccessConstraint, error) {
	// Build subject predicates: match all_principals, or specific principals.
	var subjectPreds []predicate.AccessConstraint

	// Always include all_principals constraints.
	subjectPreds = append(subjectPreds,
		accessconstraint.SubjectKindEQ(accessconstraint.SubjectKindAllPrincipals))

	// Build principal and group_closure predicates from the principal closure.
	for _, p := range principals {
		// Exact principal match.
		subjectPreds = append(subjectPreds, accessconstraint.And(
			accessconstraint.SubjectKindEQ(accessconstraint.SubjectKindPrincipal),
			accessconstraint.SubjectPrincipalIDEQ(p.ID),
		))
		// Group closure match — the principal's groups are in the closure.
		subjectPreds = append(subjectPreds, accessconstraint.And(
			accessconstraint.SubjectKindEQ(accessconstraint.SubjectKindGroupClosure),
			accessconstraint.SubjectGroupIDEQ(p.ID),
		))
	}

	// Build scope predicates.
	var scopePreds []predicate.AccessConstraint
	// System-scoped constraints always apply.
	scopePreds = append(scopePreds,
		accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeSystem))
	// Project-scoped constraints apply only to matching projects.
	for _, scopeID := range scopeIDs {
		if scopeID != "" {
			scopePreds = append(scopePreds, accessconstraint.And(
				accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeProject),
				accessconstraint.ScopeIDEQ(scopeID),
			))
		}
	}

	// Only return non-disabled constraints.
	query := s.client.AccessConstraint.Query().
		Where(
			accessconstraint.DisabledEQ(false),
			accessconstraint.Or(subjectPreds...),
			accessconstraint.Or(scopePreds...),
		)

	entities, err := query.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]*store.AccessConstraint, len(entities))
	for i, e := range entities {
		result[i] = entAccessConstraintToStore(e)
	}
	return result, nil
}

// ListConstraintsForScope returns all constraints scoped to the given scope.
func (s *AccessConstraintStore) ListConstraintsForScope(ctx context.Context, scopeType, scopeID string) ([]*store.AccessConstraint, error) {
	var preds []predicate.AccessConstraint

	if scopeType == "system" {
		// For system scope, include system-scoped constraints.
		preds = append(preds, accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeSystem))
	} else {
		// For project scope, include both system-scoped and project-scoped constraints.
		preds = append(preds,
			accessconstraint.Or(
				accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeSystem),
				accessconstraint.And(
					accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeProject),
					accessconstraint.ScopeIDEQ(scopeID),
				),
			),
		)
	}

	entities, err := s.client.AccessConstraint.Query().
		Where(preds...).
		Order(ent.Asc(accessconstraint.FieldCreated)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]*store.AccessConstraint, len(entities))
	for i, e := range entities {
		result[i] = entAccessConstraintToStore(e)
	}
	return result, nil
}

// DisableAccessConstraint disables a constraint (for offline recovery).
func (s *AccessConstraintStore) DisableAccessConstraint(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	err = s.client.AccessConstraint.UpdateOneID(uid).
		SetDisabled(true).
		Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cursor helpers for access constraint pagination
// ---------------------------------------------------------------------------

func encodeConstraintCursor(sortVal string, id string) string {
	raw := sortVal + "," + id
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

func decodeConstraintCursor(cursor string) (string, uuid.UUID, error) {
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return "", uuid.UUID{}, fmt.Errorf("base64 decode: %w", err)
	}
	s := string(raw)
	// Split at the last comma — UUIDs never contain commas, so the sort
	// value (which may contain commas, e.g. in constraint names) is
	// everything before the last comma.
	lastComma := strings.LastIndex(s, ",")
	if lastComma < 0 {
		return "", uuid.UUID{}, fmt.Errorf("expected 'value,id' format")
	}
	id, err := uuid.Parse(s[lastComma+1:])
	if err != nil {
		return "", uuid.UUID{}, fmt.Errorf("parse id: %w", err)
	}
	return s[:lastComma], id, nil
}

// ---------------------------------------------------------------------------
// Time field comparison helpers for keyset pagination
// ---------------------------------------------------------------------------

// mustParseCursorTime parses a time string from a server-generated cursor.
// Cursors are always encoded with RFC3339Nano, so parse errors indicate a
// corrupted cursor. Returns time.Time{} and logs a warning on failure rather
// than silently discarding the error.
func mustParseCursorTime(val string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, val)
	if err != nil {
		slog.Warn("malformed cursor time value, defaulting to zero time",
			"value", val, "error", err)
	}
	return t
}

func constraintFieldGT(field string, val string) predicate.AccessConstraint {
	switch field {
	case accessconstraint.FieldName:
		return accessconstraint.NameGT(val)
	case accessconstraint.FieldUpdated:
		return accessconstraint.UpdatedGT(mustParseCursorTime(val))
	default: // created
		return accessconstraint.CreatedGT(mustParseCursorTime(val))
	}
}

func constraintFieldLT(field string, val string) predicate.AccessConstraint {
	switch field {
	case accessconstraint.FieldName:
		return accessconstraint.NameLT(val)
	case accessconstraint.FieldUpdated:
		return accessconstraint.UpdatedLT(mustParseCursorTime(val))
	default: // created
		return accessconstraint.CreatedLT(mustParseCursorTime(val))
	}
}

func constraintFieldEQ(field string, val string) predicate.AccessConstraint {
	switch field {
	case accessconstraint.FieldName:
		return accessconstraint.NameEQ(val)
	case accessconstraint.FieldUpdated:
		return accessconstraint.UpdatedEQ(mustParseCursorTime(val))
	default: // created
		return accessconstraint.CreatedEQ(mustParseCursorTime(val))
	}
}
