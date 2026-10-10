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
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entschema "github.com/GoogleCloudPlatform/scion/pkg/ent/schema"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// sortOpt returns the ent ordering option for the given sort direction,
// defaulting to descending (newest first) to match the legacy SQLite store.
func sortOpt(dir string) sql.OrderTermOption {
	if dir == "asc" {
		return sql.OrderAsc()
	}
	return sql.OrderDesc()
}

// UserStore implements store.UserStore using Ent ORM.
type UserStore struct {
	client *ent.Client
}

// NewUserStore creates a new Ent-backed UserStore.
func NewUserStore(client *ent.Client) *UserStore {
	return &UserStore{client: client}
}

// normalizeEmail lower-cases an email so that the plain unique index on the
// email column enforces case-insensitive uniqueness. The legacy SQLite schema
// used UNIQUE COLLATE NOCASE; Postgres has no NOCASE collation, so we normalize
// at the port layer instead of relying on a functional lower(email) index that
// ent codegen + AutoMigrate cannot emit across both dialects.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// storePrefsToEnt converts a store.UserPreferences to the ent schema type.
func storePrefsToEnt(p *store.UserPreferences) *entschema.UserPreferences {
	if p == nil {
		return nil
	}
	return &entschema.UserPreferences{
		DefaultTemplate: p.DefaultTemplate,
		DefaultProfile:  p.DefaultProfile,
		Theme:           p.Theme,
		Timezone:        p.Timezone,
	}
}

// entPrefsToStore converts an ent schema UserPreferences to the store type.
func entPrefsToStore(p *entschema.UserPreferences) *store.UserPreferences {
	if p == nil {
		return nil
	}
	return &store.UserPreferences{
		DefaultTemplate: p.DefaultTemplate,
		DefaultProfile:  p.DefaultProfile,
		Theme:           p.Theme,
		Timezone:        p.Timezone,
	}
}

// entUserToStore converts an Ent User entity to a store.User model.
func entUserToStore(u *ent.User) *store.User {
	su := &store.User{
		ID:                u.ID.String(),
		Email:             u.Email,
		DisplayName:       u.DisplayName,
		AvatarURL:         u.AvatarURL,
		Role:              string(u.Role),
		Status:            string(u.Status),
		InvitedBy:         u.InvitedBy,
		InviteNote:        u.InviteNote,
		Preferences:       entPrefsToStore(u.Preferences),
		SessionGeneration: u.SessionGeneration,
		Kind:              string(u.Kind),
		ExpiresAt:         u.ExpiresAt,
		IssuedBy:          u.IssuedBy,
		Purpose:           u.Purpose,
		Created:           u.Created,
	}
	if u.LastLogin != nil {
		su.LastLogin = *u.LastLogin
	}
	if u.LastSeen != nil {
		su.LastSeen = *u.LastSeen
	}
	return su
}

// CreateUser creates a new user record. It refuses a test-fixture row and
// any email in the reserved test-fixture domain: only
// CreateTestFixtureUser writes those.
func (s *UserStore) CreateUser(ctx context.Context, u *store.User) error {
	if (u.Kind != "" && u.Kind != store.UserKindHuman) || u.ExpiresAt != nil || u.IssuedBy != nil || u.Purpose != nil || store.IsTestFixtureEmail(u.Email) {
		return store.ErrTestFixtureKindRefused
	}
	return s.createUser(ctx, u, nil)
}

// CreateTestFixtureUser creates a hub-issued test fixture user. See
// store.UserStore.CreateTestFixtureUser for the required fields.
func (s *UserStore) CreateTestFixtureUser(ctx context.Context, u *store.User) error {
	if u.Kind != store.UserKindTestFixture {
		return fmt.Errorf("%w: kind must be %s", store.ErrInvalidInput, store.UserKindTestFixture)
	}
	if u.ExpiresAt == nil || u.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: a test fixture user requires an expiry", store.ErrInvalidInput)
	}
	if u.IssuedBy == nil || strings.TrimSpace(*u.IssuedBy) == "" {
		return fmt.Errorf("%w: a test fixture user requires an issuer", store.ErrInvalidInput)
	}
	if !store.IsTestFixtureEmail(u.Email) {
		return fmt.Errorf("%w: a test fixture user requires an email in %s", store.ErrInvalidInput, store.TestFixtureEmailDomain)
	}
	if u.Role != store.UserRoleMember && u.Role != store.UserRoleViewer {
		return fmt.Errorf("%w: a test fixture user must have the member or viewer role", store.ErrInvalidInput)
	}
	return s.createUser(ctx, u, func(create *ent.UserCreate) {
		create.SetKind(user.KindTestFixture).
			SetExpiresAt(*u.ExpiresAt).
			SetIssuedBy(*u.IssuedBy)
		if u.Purpose != nil {
			create.SetPurpose(*u.Purpose)
		}
	})
}

// CountLiveTestFixtureUsers counts test fixture users that expire after
// now, for one issuer when issuedBy is non-empty.
func (s *UserStore) CountLiveTestFixtureUsers(ctx context.Context, issuedBy string, now time.Time) (int, error) {
	q := s.client.User.Query().Where(user.KindEQ(user.KindTestFixture), user.ExpiresAtGT(now))
	if issuedBy != "" {
		q = q.Where(user.IssuedByEQ(issuedBy))
	}
	n, err := q.Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// ListTestFixtureUsers returns test fixture users, newest first, for one
// issuer when issuedBy is non-empty.
func (s *UserStore) ListTestFixtureUsers(ctx context.Context, issuedBy string) ([]store.User, error) {
	q := s.client.User.Query().Where(user.KindEQ(user.KindTestFixture))
	if issuedBy != "" {
		q = q.Where(user.IssuedByEQ(issuedBy))
	}
	rows, err := q.Order(user.ByCreated(sql.OrderDesc()), user.ByID()).All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, *entUserToStore(r))
	}
	return out, nil
}

// LockTestFixtureIssuance takes the transaction-scoped issuance advisory
// lock on PostgreSQL. SQLite serializes writers, so it is a no-op there.
func (s *UserStore) LockTestFixtureIssuance(ctx context.Context) error {
	if s.client.Driver().Dialect() != dialect.Postgres {
		return nil
	}
	var res sql.Result
	if err := s.client.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1)", []any{int64(store.LockTestIdentityIssuance)}, &res); err != nil {
		return fmt.Errorf("lock test identity issuance: %w", err)
	}
	return nil
}

// createUser is the shared create core of CreateUser and
// CreateTestFixtureUser. extra, when non-nil, sets the test-fixture
// fields; CreateUser never passes it.
func (s *UserStore) createUser(ctx context.Context, u *store.User, extra func(*ent.UserCreate)) error {
	uid, err := parseUUID(u.ID)
	if err != nil {
		return err
	}

	if u.Created.IsZero() {
		u.Created = time.Now()
	}
	u.Email = normalizeEmail(u.Email)

	create := s.client.User.Create().
		SetID(uid).
		SetEmail(u.Email).
		SetDisplayName(u.DisplayName).
		SetCreated(u.Created)

	if u.AvatarURL != "" {
		create.SetAvatarURL(u.AvatarURL)
	}
	// Role and Status fall back to the schema defaults (member/active) when the
	// caller leaves them empty, matching how the enum validation expects a
	// non-empty value.
	if u.Role != "" {
		create.SetRole(user.Role(u.Role))
	}
	if u.Status != "" {
		create.SetStatus(user.Status(u.Status))
	}
	if u.InvitedBy != nil {
		create.SetInvitedBy(*u.InvitedBy)
	}
	if u.InviteNote != nil {
		create.SetInviteNote(*u.InviteNote)
	}
	if u.Preferences != nil {
		create.SetPreferences(storePrefsToEnt(u.Preferences))
	}
	if !u.LastLogin.IsZero() {
		create.SetLastLogin(u.LastLogin)
	}
	if !u.LastSeen.IsZero() {
		create.SetLastSeen(u.LastSeen)
	}
	if extra != nil {
		extra(create)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	u.Created = created.Created
	return nil
}

// GetUser retrieves a user by ID.
func (s *UserStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}

	u, err := s.client.User.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entUserToStore(u), nil
}

// GetUserByEmail retrieves a user by email using a case-insensitive match,
// preserving the COLLATE NOCASE semantics of the legacy SQLite schema.
func (s *UserStore) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	u, err := s.client.User.Query().
		Where(user.EmailEqualFold(normalizeEmail(email))).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUserToStore(u), nil
}

// UpdateUser updates an existing user.
func (s *UserStore) UpdateUser(ctx context.Context, u *store.User) error {
	uid, err := parseUUID(u.ID)
	if err != nil {
		return err
	}

	u.Email = normalizeEmail(u.Email)

	// kind is Immutable, so no update can change it. The email must also
	// stay on the right side of the reserved test-fixture domain: a human
	// row cannot move into it and a test-fixture row cannot leave it.
	current, err := s.client.User.Query().Where(user.IDEQ(uid)).Select(user.FieldKind).Only(ctx)
	if err != nil {
		return mapError(err)
	}
	if (current.Kind == user.KindTestFixture) != store.IsTestFixtureEmail(u.Email) {
		return store.ErrTestFixtureKindRefused
	}

	update := s.client.User.UpdateOneID(uid).
		SetEmail(u.Email).
		SetDisplayName(u.DisplayName)

	if u.AvatarURL != "" {
		update.SetAvatarURL(u.AvatarURL)
	} else {
		update.ClearAvatarURL()
	}
	if u.Role != "" {
		update.SetRole(user.Role(u.Role))
	}
	if u.Status != "" {
		update.SetStatus(user.Status(u.Status))
	}
	if u.InvitedBy != nil {
		update.SetInvitedBy(*u.InvitedBy)
	} else {
		update.ClearInvitedBy()
	}
	if u.InviteNote != nil {
		update.SetInviteNote(*u.InviteNote)
	} else {
		update.ClearInviteNote()
	}
	if u.Preferences != nil {
		update.SetPreferences(storePrefsToEnt(u.Preferences))
	} else {
		update.ClearPreferences()
	}
	if !u.LastLogin.IsZero() {
		update.SetLastLogin(u.LastLogin)
	} else {
		update.ClearLastLogin()
	}
	if !u.LastSeen.IsZero() {
		update.SetLastSeen(u.LastSeen)
	} else {
		update.ClearLastSeen()
	}

	if err := update.Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// UpdateUserLastSeen sets only the last_seen timestamp for a user.
func (s *UserStore) UpdateUserLastSeen(ctx context.Context, id string, t time.Time) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.User.UpdateOneID(uid).SetLastSeen(t).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// LockUserRow locks the user row until the surrounding transaction ends.
// On PostgreSQL it runs
//
//	SELECT id FROM users WHERE id = $1 FOR UPDATE      (exclusive)
//	SELECT id FROM users WHERE id = $1 FOR KEY SHARE   (shared)
//
// so a user delete and an agent create or restore for that user serialize
// under READ COMMITTED (ptone/scion#2769). The shared mode is FOR KEY SHARE,
// not FOR SHARE: it conflicts only with FOR UPDATE (which the delete takes,
// and which DELETE takes too), so plain UPDATEs of the user row, which take
// FOR NO KEY UPDATE (last seen, profile edits, session revoke), do not wait
// for an open create or restore transaction. On SQLite all writes are already
// database-serialized, so it issues a plain read (the same dialect check as
// ProjectStore.LockProjectForMembership).
func (s *UserStore) LockUserRow(ctx context.Context, id string, exclusive bool) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}

	q := s.client.User.Query().Where(user.IDEQ(uid))
	if s.client.Driver().Dialect() == dialect.Postgres {
		if exclusive {
			q = q.ForUpdate()
		} else {
			q = q.ForShare(lockKeyShare)
		}
	}

	exists, err := q.Exist(ctx)
	if err != nil {
		return fmt.Errorf("lock user row: %w", mapError(err))
	}
	if !exists {
		return store.ErrNotFound
	}
	return nil
}

// lockKeyShare turns ForShare into SELECT ... FOR KEY SHARE (the generated
// UserQuery has no lock-strength option of its own).
func lockKeyShare(o *sql.LockOptions) {
	o.Strength = sql.LockKeyShare
}

// DeleteUser removes a user by ID.
func (s *UserStore) DeleteUser(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.User.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListUsers returns users matching the filter criteria. Pagination is
// offset-based to match the legacy SQLite store: the cursor is the integer
// offset of the next page.
func (s *UserStore) ListUsers(ctx context.Context, filter store.UserFilter, opts store.ListOptions) (*store.ListResult[store.User], error) {
	query := s.client.User.Query()

	if filter.Role != "" {
		query.Where(user.RoleEQ(user.Role(filter.Role)))
	}
	if filter.Status != "" {
		query.Where(user.StatusEQ(user.Status(filter.Status)))
	}
	if filter.Search != "" {
		query.Where(user.Or(
			user.EmailContainsFold(filter.Search),
			user.DisplayNameContainsFold(filter.Search),
		))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	offset := 0
	if opts.Cursor != "" {
		if parsed, err := strconv.Atoi(opts.Cursor); err == nil && parsed > 0 {
			offset = parsed
		}
	}

	// Map the sort field to an ordering, whitelisting the supported columns.
	var order user.OrderOption
	switch opts.SortBy {
	case "name":
		// Name defaults to ascending unless an explicit direction is given.
		if opts.SortDir == "desc" {
			order = user.ByDisplayName(sql.OrderDesc())
		} else {
			order = user.ByDisplayName(sql.OrderAsc())
		}
	case "lastSeen":
		order = user.ByLastSeen(sortOpt(opts.SortDir))
	default: // "created" and unspecified
		order = user.ByCreated(sortOpt(opts.SortDir))
	}

	users, err := query.
		Order(order).
		Limit(limit + 1).
		Offset(offset).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.User, 0, len(users))
	for _, u := range users {
		items = append(items, *entUserToStore(u))
	}

	result := &store.ListResult[store.User]{
		Items:      items,
		TotalCount: totalCount,
	}
	if len(items) > limit {
		result.Items = items[:limit]
		result.NextCursor = strconv.Itoa(offset + limit)
	}
	return result, nil
}

// IsUserInvitedOrActive returns true if a User record exists with the given
// email and status in ("invited", "active"). Used by the invite_only
// authorization gate as a replacement for IsEmailAllowListed.
func (s *UserStore) IsUserInvitedOrActive(ctx context.Context, email string) (bool, error) {
	return s.client.User.Query().
		Where(
			user.EmailEqualFold(normalizeEmail(email)),
			user.StatusIn(user.StatusInvited, user.StatusActive),
		).
		Exist(ctx)
}

// IncrementSessionGeneration atomically increments the user's
// session_generation counter, invalidating all existing sessions.
func (s *UserStore) IncrementSessionGeneration(ctx context.Context, userID string) error {
	uid, err := parseUUID(userID)
	if err != nil {
		return err
	}
	return s.client.User.UpdateOneID(uid).
		AddSessionGeneration(1).
		Exec(ctx)
}
