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
	"strconv"
	"strings"
	"time"

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
	}
}

// entUserToStore converts an Ent User entity to a store.User model.
func entUserToStore(u *ent.User) *store.User {
	su := &store.User{
		ID:          u.ID.String(),
		Email:       u.Email,
		DisplayName: u.DisplayName,
		AvatarURL:   u.AvatarURL,
		Role:        string(u.Role),
		Status:      string(u.Status),
		Preferences: entPrefsToStore(u.Preferences),
		Created:     u.Created,
	}
	if u.LastLogin != nil {
		su.LastLogin = *u.LastLogin
	}
	if u.LastSeen != nil {
		su.LastSeen = *u.LastSeen
	}
	return su
}

// CreateUser creates a new user record.
func (s *UserStore) CreateUser(ctx context.Context, u *store.User) error {
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
	if u.Preferences != nil {
		create.SetPreferences(storePrefsToEnt(u.Preferences))
	}
	if !u.LastLogin.IsZero() {
		create.SetLastLogin(u.LastLogin)
	}
	if !u.LastSeen.IsZero() {
		create.SetLastSeen(u.LastSeen)
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
