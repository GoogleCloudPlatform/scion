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
	"sort"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/allowlistentry"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/invitecode"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// AllowListStore implements store.AllowListStore and store.InviteCodeStore using
// Ent ORM. Both interfaces are co-located because the invite-only access control
// flow (allow list + invite codes) is a single logical domain.
type AllowListStore struct {
	client *ent.Client
}

// NewAllowListStore creates a new Ent-backed AllowListStore.
func NewAllowListStore(client *ent.Client) *AllowListStore {
	return &AllowListStore{client: client}
}

// entAllowListToStore converts an Ent AllowListEntry to a store model.
func entAllowListToStore(e *ent.AllowListEntry) *store.AllowListEntry {
	return &store.AllowListEntry{
		ID:       e.ID.String(),
		Email:    e.Email,
		Note:     e.Note,
		AddedBy:  e.AddedBy,
		InviteID: e.InviteID,
		Created:  e.Created,
	}
}

// entInviteToStore converts an Ent InviteCode to a store model.
func entInviteToStore(i *ent.InviteCode) *store.InviteCode {
	return &store.InviteCode{
		ID:         i.ID.String(),
		CodeHash:   i.CodeHash,
		CodePrefix: i.CodePrefix,
		MaxUses:    i.MaxUses,
		UseCount:   i.UseCount,
		ExpiresAt:  i.ExpiresAt,
		Revoked:    i.Revoked,
		CreatedBy:  i.CreatedBy,
		Note:       i.Note,
		Created:    i.Created,
	}
}

// ============================================================================
// Allow List Operations
// ============================================================================

// AddAllowListEntry adds a single email to the allow list.
func (s *AllowListStore) AddAllowListEntry(ctx context.Context, entry *store.AllowListEntry) error {
	uid, err := parseUUID(entry.ID)
	if err != nil {
		return err
	}
	if entry.Created.IsZero() {
		entry.Created = time.Now()
	}
	entry.Email = normalizeEmail(entry.Email)

	create := s.client.AllowListEntry.Create().
		SetID(uid).
		SetEmail(entry.Email).
		SetNote(entry.Note).
		SetAddedBy(entry.AddedBy).
		SetCreated(entry.Created)
	if entry.InviteID != "" {
		create.SetInviteID(entry.InviteID)
	}

	if err := create.Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// RemoveAllowListEntry removes an email from the allow list.
func (s *AllowListStore) RemoveAllowListEntry(ctx context.Context, email string) error {
	n, err := s.client.AllowListEntry.Delete().
		Where(allowlistentry.EmailEQ(normalizeEmail(email))).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetAllowListEntry retrieves an allow list entry by email.
func (s *AllowListStore) GetAllowListEntry(ctx context.Context, email string) (*store.AllowListEntry, error) {
	e, err := s.client.AllowListEntry.Query().
		Where(allowlistentry.EmailEQ(normalizeEmail(email))).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entAllowListToStore(e), nil
}

// ListAllowListEntries returns a keyset-paginated page of allow list entries
// ordered by (created DESC, id DESC), matching the legacy SQLite store.
func (s *AllowListStore) ListAllowListEntries(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.AllowListEntry], error) {
	totalCount, err := s.client.AllowListEntry.Query().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := s.client.AllowListEntry.Query()
	if opts.Cursor != "" {
		pred, err := s.allowListCursorPredicate(ctx, opts.Cursor)
		if err != nil {
			return nil, err
		}
		query.Where(pred)
	}

	entries, err := query.
		Order(allowlistentry.ByCreated(sql.OrderDesc()), allowlistentry.ByID(sql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.AllowListEntry, 0, len(entries))
	for _, e := range entries {
		items = append(items, *entAllowListToStore(e))
	}

	result := &store.ListResult[store.AllowListEntry]{
		Items:      items,
		TotalCount: totalCount,
	}
	if len(items) > limit {
		result.NextCursor = items[limit-1].ID
		result.Items = items[:limit]
	}
	return result, nil
}

// allowListCursorPredicate builds the keyset predicate for paginating after the
// entry identified by cursor (an entry ID).
func (s *AllowListStore) allowListCursorPredicate(ctx context.Context, cursor string) (predicate.AllowListEntry, error) {
	cursorUID, err := parseUUID(cursor)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	c, err := s.client.AllowListEntry.Get(ctx, cursorUID)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", mapError(err))
	}
	return allowlistentry.Or(
		allowlistentry.CreatedLT(c.Created),
		allowlistentry.And(allowlistentry.CreatedEQ(c.Created), allowlistentry.IDLT(cursorUID)),
	), nil
}

// IsEmailAllowListed reports whether an email is present on the allow list.
func (s *AllowListStore) IsEmailAllowListed(ctx context.Context, email string) (bool, error) {
	return s.client.AllowListEntry.Query().
		Where(allowlistentry.EmailEQ(normalizeEmail(email))).
		Exist(ctx)
}

// BulkAddAllowListEntries inserts many entries idempotently, skipping any whose
// email already exists. It mirrors the legacy `INSERT OR IGNORE` behavior:
// duplicate emails (already present or repeated within the batch) are counted as
// skipped rather than erroring. OnConflict().Ignore() additionally makes the
// bulk insert safe against rows inserted concurrently between the existence
// check and the write.
func (s *AllowListStore) BulkAddAllowListEntries(ctx context.Context, entries []*store.AllowListEntry) (int, int, error) {
	now := time.Now()
	for _, e := range entries {
		e.Email = normalizeEmail(e.Email)
		if e.Created.IsZero() {
			e.Created = now
		}
	}

	// Determine which emails already exist so we can report accurate counts.
	emails := make([]string, 0, len(entries))
	for _, e := range entries {
		emails = append(emails, e.Email)
	}
	existingRows, err := s.client.AllowListEntry.Query().
		Where(allowlistentry.EmailIn(emails...)).
		Select(allowlistentry.FieldEmail).
		Strings(ctx)
	if err != nil {
		return 0, 0, err
	}
	seen := make(map[string]bool, len(existingRows))
	for _, e := range existingRows {
		seen[e] = true
	}

	bulk := make([]*ent.AllowListEntryCreate, 0, len(entries))
	added, skipped := 0, 0
	for _, e := range entries {
		if seen[e.Email] {
			skipped++
			continue
		}
		seen[e.Email] = true // dedupe repeats within the same batch

		uid, err := parseUUID(e.ID)
		if err != nil {
			return added, skipped, err
		}
		create := s.client.AllowListEntry.Create().
			SetID(uid).
			SetEmail(e.Email).
			SetNote(e.Note).
			SetAddedBy(e.AddedBy).
			SetCreated(e.Created)
		if e.InviteID != "" {
			create.SetInviteID(e.InviteID)
		}
		bulk = append(bulk, create)
		added++
	}

	if len(bulk) > 0 {
		if err := s.client.AllowListEntry.CreateBulk(bulk...).
			OnConflictColumns(allowlistentry.FieldEmail).
			Ignore().
			Exec(ctx); err != nil {
			return 0, 0, err
		}
	}
	return added, skipped, nil
}

// ListEmailDomains returns the distinct, sorted set of email domains across all
// users.
func (s *AllowListStore) ListEmailDomains(ctx context.Context) ([]string, error) {
	emails, err := s.client.User.Query().
		Select(user.FieldEmail).
		Strings(ctx)
	if err != nil {
		return nil, err
	}

	domainSet := make(map[string]struct{})
	for _, e := range emails {
		at := strings.LastIndex(e, "@")
		if at < 0 || at == len(e)-1 {
			continue
		}
		domainSet[e[at+1:]] = struct{}{}
	}

	domains := make([]string, 0, len(domainSet))
	for d := range domainSet {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	return domains, nil
}

// UpdateAllowListEntryInviteID associates an invite code with an allow list entry.
func (s *AllowListStore) UpdateAllowListEntryInviteID(ctx context.Context, email string, inviteID string) error {
	n, err := s.client.AllowListEntry.Update().
		Where(allowlistentry.EmailEQ(normalizeEmail(email))).
		SetInviteID(inviteID).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListAllowListEntriesWithInvites returns allow list entries enriched with their
// associated invite code details. invite_id is a plain column (not an Ent edge),
// so the join is performed by batch-loading the referenced invite codes.
func (s *AllowListStore) ListAllowListEntriesWithInvites(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.AllowListEntryWithInvite], error) {
	base, err := s.ListAllowListEntries(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Collect referenced invite IDs.
	inviteUIDs := make([]uuid.UUID, 0)
	for i := range base.Items {
		if base.Items[i].InviteID == "" {
			continue
		}
		if uid, err := parseUUID(base.Items[i].InviteID); err == nil {
			inviteUIDs = append(inviteUIDs, uid)
		}
	}

	invitesByID := make(map[string]*ent.InviteCode)
	if len(inviteUIDs) > 0 {
		invites, err := s.client.InviteCode.Query().
			Where(invitecode.IDIn(inviteUIDs...)).
			All(ctx)
		if err != nil {
			return nil, err
		}
		for _, inv := range invites {
			invitesByID[inv.ID.String()] = inv
		}
	}

	items := make([]store.AllowListEntryWithInvite, 0, len(base.Items))
	for i := range base.Items {
		entry := store.AllowListEntryWithInvite{AllowListEntry: base.Items[i]}
		if inv, ok := invitesByID[base.Items[i].InviteID]; ok {
			entry.InviteCodePrefix = inv.CodePrefix
			entry.InviteMaxUses = inv.MaxUses
			entry.InviteUseCount = inv.UseCount
			entry.InviteExpiresAt = inv.ExpiresAt
			entry.InviteRevoked = inv.Revoked
		}
		items = append(items, entry)
	}

	return &store.ListResult[store.AllowListEntryWithInvite]{
		Items:      items,
		TotalCount: base.TotalCount,
		NextCursor: base.NextCursor,
	}, nil
}

// ============================================================================
// Invite Code Operations
// ============================================================================

// CreateInviteCode creates a new invite code.
func (s *AllowListStore) CreateInviteCode(ctx context.Context, invite *store.InviteCode) error {
	uid, err := parseUUID(invite.ID)
	if err != nil {
		return err
	}
	if invite.Created.IsZero() {
		invite.Created = time.Now()
	}

	create := s.client.InviteCode.Create().
		SetID(uid).
		SetCodeHash(invite.CodeHash).
		SetCodePrefix(invite.CodePrefix).
		SetMaxUses(invite.MaxUses).
		SetUseCount(invite.UseCount).
		SetExpiresAt(invite.ExpiresAt).
		SetRevoked(invite.Revoked).
		SetCreatedBy(invite.CreatedBy).
		SetNote(invite.Note).
		SetCreated(invite.Created)

	if err := create.Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetInviteCodeByHash retrieves an invite code by its hash.
func (s *AllowListStore) GetInviteCodeByHash(ctx context.Context, codeHash string) (*store.InviteCode, error) {
	i, err := s.client.InviteCode.Query().
		Where(invitecode.CodeHashEQ(codeHash)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entInviteToStore(i), nil
}

// GetInviteCode retrieves an invite code by ID.
func (s *AllowListStore) GetInviteCode(ctx context.Context, id string) (*store.InviteCode, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	i, err := s.client.InviteCode.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entInviteToStore(i), nil
}

// ListInviteCodes returns a keyset-paginated page of invite codes ordered by
// (created DESC, id DESC). CodeHash is sensitive and not exposed in listings.
func (s *AllowListStore) ListInviteCodes(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.InviteCode], error) {
	totalCount, err := s.client.InviteCode.Query().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := s.client.InviteCode.Query()
	if opts.Cursor != "" {
		cursorUID, err := parseUUID(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		c, err := s.client.InviteCode.Get(ctx, cursorUID)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", mapError(err))
		}
		query.Where(invitecode.Or(
			invitecode.CreatedLT(c.Created),
			invitecode.And(invitecode.CreatedEQ(c.Created), invitecode.IDLT(cursorUID)),
		))
	}

	invites, err := query.
		Order(invitecode.ByCreated(sql.OrderDesc()), invitecode.ByID(sql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.InviteCode, 0, len(invites))
	for _, i := range invites {
		si := entInviteToStore(i)
		si.CodeHash = "" // not exposed in listings, matching the legacy store
		items = append(items, *si)
	}

	result := &store.ListResult[store.InviteCode]{
		Items:      items,
		TotalCount: totalCount,
	}
	if len(items) > limit {
		result.NextCursor = items[limit-1].ID
		result.Items = items[:limit]
	}
	return result, nil
}

// IncrementInviteUseCount atomically increments use_count for an invite code
// that is still redeemable (not revoked, not expired, and below max_uses).
//
// This is expressed as a single conditional UPDATE rather than a
// SELECT-then-UPDATE read-modify-write: the predicate and the increment execute
// in one statement, so the operation is race-free on both SQLite and Postgres
// without needing SELECT ... FOR UPDATE row locking. (The sql/lock feature is
// enabled and ForUpdate is available for genuine multi-statement RMW paths.)
func (s *AllowListStore) IncrementInviteUseCount(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	n, err := s.client.InviteCode.Update().
		Where(
			invitecode.IDEQ(uid),
			invitecode.RevokedEQ(false),
			invitecode.ExpiresAtGT(time.Now()),
			// (max_uses = 0 OR use_count < max_uses) — a column-to-column
			// comparison expressed via a raw selector predicate.
			predicate.InviteCode(func(sel *sql.Selector) {
				sel.Where(sql.Or(
					sql.EQ(sel.C(invitecode.FieldMaxUses), 0),
					sql.ColumnsLT(sel.C(invitecode.FieldUseCount), sel.C(invitecode.FieldMaxUses)),
				))
			}),
		).
		AddUseCount(1).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// RevokeInviteCode marks an invite code as revoked.
func (s *AllowListStore) RevokeInviteCode(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	n, err := s.client.InviteCode.Update().
		Where(invitecode.IDEQ(uid)).
		SetRevoked(true).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteInviteCode removes an invite code by ID.
func (s *AllowListStore) DeleteInviteCode(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.InviteCode.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetInviteStats returns aggregate statistics about invite codes and the allow
// list.
func (s *AllowListStore) GetInviteStats(ctx context.Context) (*store.InviteStats, error) {
	stats := &store.InviteStats{
		RecentRedemptions: []store.InviteCodeInfo{},
	}

	// The invite_codes table is small (admin-managed); load it once ordered by
	// recency and derive pending count, total redemptions, and recent
	// redemptions in a single pass.
	codes, err := s.client.InviteCode.Query().
		Order(invitecode.ByCreated(sql.OrderDesc())).
		All(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	for _, c := range codes {
		stats.TotalRedemptions += c.UseCount
		if !c.Revoked && c.ExpiresAt.After(now) && (c.MaxUses == 0 || c.UseCount < c.MaxUses) {
			stats.PendingInvites++
		}
		if c.UseCount > 0 && len(stats.RecentRedemptions) < 10 {
			stats.RecentRedemptions = append(stats.RecentRedemptions, store.InviteCodeInfo{
				ID:         c.ID.String(),
				CodePrefix: c.CodePrefix,
				UseCount:   c.UseCount,
				MaxUses:    c.MaxUses,
				ExpiresAt:  c.ExpiresAt,
				Note:       c.Note,
				Created:    c.Created,
			})
		}
	}

	allowCount, err := s.client.AllowListEntry.Query().Count(ctx)
	if err != nil {
		return nil, err
	}
	stats.AllowListCount = allowCount

	return stats, nil
}
