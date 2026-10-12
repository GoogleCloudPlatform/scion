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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin-kind test identities (ptone/scion#4240, admin tier A1), store
// half. Like user_test_fixture_test.go, these run on SQLite by default and
// on Postgres with -tags integration and SCION_TEST_POSTGRES_URL.

// newTestAdminSystemRole creates a system-scoped role definition for the
// issuance binding (the hub seeds hub-admin; the store tests seed their own).
func newTestAdminSystemRole(t *testing.T, cs *CompositeStore, name string) *store.RoleDefinition {
	t.Helper()
	rd, err := cs.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		ID: uuid.NewString(), Name: name + "-" + uuid.NewString()[:8], Description: "test admin role",
		ScopeType: store.RoleScopeSystem, Permissions: []string{"user.read"},
	})
	require.NoError(t, err)
	return rd
}

func newTestAdminUser(kind, issuer string, expiresAt time.Time) *store.User {
	purpose := "store admin test"
	issuance := uuid.NewString()
	return &store.User{
		ID:          uuid.NewString(),
		Email:       "test-admin-" + uuid.NewString()[:8] + "@" + store.TestAdminEmailDomain(kind),
		DisplayName: "Test admin",
		Role:        store.TestAdminRole(kind),
		Status:      store.UserStatusActive,
		Kind:        kind,
		ExpiresAt:   &expiresAt,
		IssuedBy:    &issuer,
		Purpose:     &purpose,
		IssuanceID:  &issuance,
	}
}

// The general CreateUser refuses every admin-kind shape and both admin
// domains; CreateTestFixtureUser refuses the admin kinds too.
func TestCreateUser_RefusesTestAdminRows(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	issuance := "iss"
	for name, u := range map[string]*store.User{
		"hub admin kind":     {Kind: store.UserKindTestHubAdmin, Email: "a@example.com"},
		"super admin kind":   {Kind: store.UserKindTestSuperAdmin, Email: "b@example.com"},
		"hub admin domain":   {Email: "c@" + store.TestHubAdminEmailDomain},
		"super admin domain": {Email: "d@" + store.TestSuperAdminEmailDomain},
		"subdomain":          {Email: "e@x." + store.TestHubAdminEmailDomain},
		"case folded":        {Email: "f@SCION-TEST-HUB-ADMIN.INVALID"},
		"issuance id":        {Email: "g@example.com", IssuanceID: &issuance},
		"issuer credential":  {Email: "h@example.com", IssuerCredentialID: &issuance},
		"expiry":             {Email: "i@example.com", ExpiresAt: &exp},
	} {
		u.ID = uuid.NewString()
		u.DisplayName = name
		require.ErrorIs(t, cs.CreateUser(ctx, u), store.ErrTestFixtureKindRefused, name)
	}
	admin := newTestAdminUser(store.UserKindTestHubAdmin, uuid.NewString(), exp)
	require.Error(t, cs.CreateTestFixtureUser(ctx, admin), "the fixture path never writes an admin kind")
	fx := newFixtureUser(uuid.NewString(), exp)
	fx.IssuanceID = &issuance
	require.ErrorIs(t, cs.CreateTestFixtureUser(ctx, fx), store.ErrInvalidInput, "issuance fields are admin-only")
	// scion-test.invalid (test-login) stays an ordinary domain.
	require.NoError(t, cs.CreateUser(ctx, &store.User{ID: uuid.NewString(), Email: "tl-" + uuid.NewString()[:8] + "@scion-test.invalid"}))
}

// CreateTestAdminUser writes the row and one system binding that expires
// with the identity, created by the issuance sentinel.
func TestCreateTestAdminUser_RowAndBinding(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	rd := newTestAdminSystemRole(t, cs, "hub-admin")
	issuer := uuid.NewString()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	u := newTestAdminUser(store.UserKindTestHubAdmin, issuer, exp)
	cred := uuid.NewString()
	u.IssuerCredentialID = &cred

	b, err := cs.CreateTestAdminUser(ctx, u, rd.ID)
	require.NoError(t, err)
	require.NotNil(t, b)

	got, err := cs.GetUser(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserKindTestHubAdmin, got.Kind)
	assert.Equal(t, store.UserRoleMember, got.Role)
	assert.True(t, strings.HasSuffix(got.Email, "@"+store.TestHubAdminEmailDomain))
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, got.ExpiresAt.Equal(exp))
	require.NotNil(t, got.IssuanceID)
	assert.Equal(t, *u.IssuanceID, *got.IssuanceID)
	require.NotNil(t, got.IssuerCredentialID)
	assert.Equal(t, cred, *got.IssuerCredentialID)
	assert.Equal(t, issuer, *got.IssuedBy)
	assert.True(t, got.IsTestAdmin())
	assert.False(t, got.IsTestFixture())

	bindings, err := cs.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	assert.Equal(t, rd.ID, bindings[0].RoleDefinitionID)
	assert.Equal(t, store.RoleScopeSystem, bindings[0].ScopeType)
	assert.Equal(t, store.TestIdentityIssuanceCreatedBy, bindings[0].CreatedBy)
	require.NotNil(t, bindings[0].ExpiresAt)
	assert.True(t, bindings[0].ExpiresAt.Equal(exp), "the binding expires with the identity")
}

func TestCreateTestAdminUser_Validation(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	rd := newTestAdminSystemRole(t, cs, "hub-admin")
	exp := time.Now().Add(time.Hour)
	blank := " "
	cases := map[string]func(u *store.User){
		"human kind":        func(u *store.User) { u.Kind = store.UserKindHuman },
		"fixture kind":      func(u *store.User) { u.Kind = store.UserKindTestFixture },
		"no expiry":         func(u *store.User) { u.ExpiresAt = nil },
		"no issuer":         func(u *store.User) { u.IssuedBy = nil },
		"blank issuer":      func(u *store.User) { u.IssuedBy = &blank },
		"no issuance id":    func(u *store.User) { u.IssuanceID = nil },
		"blank credential":  func(u *store.User) { u.IssuerCredentialID = &blank },
		"wrong domain":      func(u *store.User) { u.Email = "x-" + uuid.NewString()[:8] + "@" + store.TestFixtureEmailDomain },
		"other kind domain": func(u *store.User) { u.Email = "x-" + uuid.NewString()[:8] + "@" + store.TestSuperAdminEmailDomain },
		"ordinary domain":   func(u *store.User) { u.Email = "x-" + uuid.NewString()[:8] + "@example.com" },
		"admin role":        func(u *store.User) { u.Role = store.UserRoleAdmin },
		"viewer role":       func(u *store.User) { u.Role = store.UserRoleViewer },
	}
	for name, mutate := range cases {
		u := newTestAdminUser(store.UserKindTestHubAdmin, uuid.NewString(), exp)
		mutate(u)
		_, err := cs.CreateTestAdminUser(ctx, u, rd.ID)
		assert.ErrorIs(t, err, store.ErrInvalidInput, name)
	}
	// A project-scoped role is refused, and nothing is written.
	prd, err := cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
		ID: uuid.NewString(), Name: "proj-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"},
	})
	require.NoError(t, err)
	u := newTestAdminUser(store.UserKindTestHubAdmin, uuid.NewString(), exp)
	_, err = cs.CreateTestAdminUser(ctx, u, prd.ID)
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.GetUser(ctx, u.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// The super-admin kind's binding is refused by the super-admin binding
// guard until the admin tier's policy phase; the row rolls back with it.
func TestCreateTestAdminUser_SuperAdminBindingRefused(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	rd, err := cs.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		rd, err = cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
			ID: uuid.NewString(), Name: store.SystemRoleSuperAdmin, ScopeType: store.RoleScopeSystem, Permissions: []string{"user.read"},
		})
		require.NoError(t, err)
	}
	u := newTestAdminUser(store.UserKindTestSuperAdmin, uuid.NewString(), time.Now().Add(30*time.Minute))
	_, err = cs.CreateTestAdminUser(ctx, u, rd.ID)
	require.ErrorIs(t, err, store.ErrSuperAdminBindingRestricted)
	_, err = cs.GetUser(ctx, u.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "the row rolls back with the refused binding")
}

// The DB CHECK rejects an admin-kind row without an expiry.
func TestUsers_DBCheckRejectsTestAdminWithoutExpiry(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)
	for _, kind := range []string{store.UserKindTestHubAdmin, store.UserKindTestSuperAdmin} {
		q := rebindForDialect(cs.Dialect(),
			"INSERT INTO users (id, email, display_name, role, status, created, session_generation, kind, issued_by) VALUES (?, ?, 'x', 'member', 'active', ?, 0, ?, ?)")
		_, err := db.ExecContext(ctx, q, uuid.NewString(), "raw-"+uuid.NewString()[:8]+"@"+store.TestAdminEmailDomain(kind), time.Now().UTC(), kind, uuid.NewString())
		require.Error(t, err, kind)
		require.Contains(t, strings.ToLower(err.Error()), "check constraint", kind)
	}
}

// A system binding on an admin-kind row is refused unless it is the
// issuance binding with an expiry no later than the identity's.
// Project-scoped bindings stay allowed.
func TestCreateRoleBinding_TestAdminSystemBindingGuard(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	rd := newTestAdminSystemRole(t, cs, "hub-admin")
	other := newTestAdminSystemRole(t, cs, "other")
	exp := time.Now().Add(time.Hour)
	u := newTestAdminUser(store.UserKindTestHubAdmin, uuid.NewString(), exp)
	_, err := cs.CreateTestAdminUser(ctx, u, rd.ID)
	require.NoError(t, err)

	later := exp.Add(time.Minute)
	earlier := exp.Add(-time.Minute)
	for name, rb := range map[string]*store.RoleBinding{
		"no expiry":             {CreatedBy: store.TestIdentityIssuanceCreatedBy},
		"admin api sentinel":    {CreatedBy: store.AdminAPICreatedBy, ExpiresAt: &earlier},
		"ordinary creator":      {CreatedBy: "someone", ExpiresAt: &earlier},
		"outlives the identity": {CreatedBy: store.TestIdentityIssuanceCreatedBy, ExpiresAt: &later},
	} {
		rb.RoleDefinitionID = other.ID
		rb.PrincipalType = store.RoleBindingPrincipalUser
		rb.PrincipalID = u.ID
		rb.ScopeType = store.RoleScopeSystem
		_, err := cs.CreateRoleBinding(ctx, rb)
		assert.ErrorIs(t, err, store.ErrInvalidInput, name)
	}
	_, err = cs.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: other.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: u.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: store.TestIdentityIssuanceCreatedBy, ExpiresAt: &earlier,
	})
	assert.NoError(t, err, "an issuance-shaped binding within the identity's lifetime is accepted")

	// A human row is unaffected.
	human := &store.User{ID: uuid.NewString(), Email: "h-" + uuid.NewString()[:8] + "@example.com", DisplayName: "h"}
	require.NoError(t, cs.CreateUser(ctx, human))
	_, err = cs.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: other.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: human.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "someone",
	})
	assert.NoError(t, err)
}

func TestTestAdminUsers_CountAndList(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	rd := newTestAdminSystemRole(t, cs, "hub-admin")
	now := time.Now()
	a, b := uuid.NewString(), uuid.NewString()
	for _, u := range []*store.User{
		newTestAdminUser(store.UserKindTestHubAdmin, a, now.Add(time.Hour)),
		newTestAdminUser(store.UserKindTestHubAdmin, b, now.Add(time.Hour)),
		newTestAdminUser(store.UserKindTestHubAdmin, a, now.Add(-time.Minute)), // expired
	} {
		_, err := cs.CreateTestAdminUser(ctx, u, rd.ID)
		require.NoError(t, err)
	}
	// Member fixtures and humans are never counted.
	require.NoError(t, cs.CreateTestFixtureUser(ctx, newFixtureUser(a, now.Add(time.Hour))))
	require.NoError(t, cs.CreateUser(ctx, &store.User{ID: uuid.NewString(), Email: "h-" + uuid.NewString()[:8] + "@example.com"}))

	for _, tc := range []struct {
		kind, issuer string
		want         int
	}{
		{"", a, 1}, {"", b, 1}, {"", "", 2},
		{store.UserKindTestHubAdmin, "", 2}, {store.UserKindTestSuperAdmin, "", 0},
	} {
		n, err := cs.CountLiveTestAdminUsers(ctx, tc.kind, tc.issuer, now)
		require.NoError(t, err)
		assert.Equal(t, tc.want, n, "kind=%q issuer=%q", tc.kind, tc.issuer)
	}
	_, err := cs.CountLiveTestAdminUsers(ctx, store.UserKindTestFixture, "", now)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	all, err := cs.ListTestAdminUsers(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 3, "live and expired admin-kind rows, no fixtures or humans")
	for _, u := range all {
		assert.True(t, u.IsTestAdmin())
	}
}

// T5 for the admin kinds: the issuance lock plus count-then-create in one
// transaction never exceeds the per-issuer cap (1) or the hub cap (2)
// under concurrency. On SQLite the single writer serializes; on Postgres
// the advisory lock does.
func TestTestAdminCaps_SerializeCountAndCreate(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	rd := newTestAdminSystemRole(t, cs, "hub-admin")
	const issuerCap, hubCap, workers = 1, 2, 12
	issuers := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}

	errCap := errors.New("cap")
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		issuer := issuers[i%len(issuers)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- retryBusy(func() error {
				return cs.WithTx(ctx, func(tx store.Store) error {
					if err := tx.LockTestFixtureIssuance(ctx); err != nil {
						return err
					}
					now := time.Now()
					n, err := tx.CountLiveTestAdminUsers(ctx, "", issuer, now)
					if err != nil {
						return err
					}
					if n >= issuerCap {
						return errCap
					}
					n, err = tx.CountLiveTestAdminUsers(ctx, "", "", now)
					if err != nil {
						return err
					}
					if n >= hubCap {
						return errCap
					}
					_, err = tx.CreateTestAdminUser(ctx, newTestAdminUser(store.UserKindTestHubAdmin, issuer, now.Add(time.Hour)), rd.ID)
					return err
				})
			})
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errCap):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, hubCap, ok)
	n, err := cs.CountLiveTestAdminUsers(ctx, "", "", time.Now())
	require.NoError(t, err)
	assert.Equal(t, hubCap, n)
	for _, issuer := range issuers {
		n, err := cs.CountLiveTestAdminUsers(ctx, "", issuer, time.Now())
		require.NoError(t, err)
		assert.LessOrEqual(t, n, issuerCap)
	}
}

// An admin-kind row's email cannot leave its kind's domain, and a human
// row cannot move into an admin domain.
func TestUpdateUser_EmailStaysInTestAdminDomain(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	rd := newTestAdminSystemRole(t, cs, "hub-admin")
	u := newTestAdminUser(store.UserKindTestHubAdmin, uuid.NewString(), time.Now().Add(time.Hour))
	_, err := cs.CreateTestAdminUser(ctx, u, rd.ID)
	require.NoError(t, err)

	got, err := cs.GetUser(ctx, u.ID)
	require.NoError(t, err)
	for _, email := range []string{
		"x-" + uuid.NewString()[:8] + "@example.com",
		"x-" + uuid.NewString()[:8] + "@" + store.TestSuperAdminEmailDomain,
		"x-" + uuid.NewString()[:8] + "@" + store.TestFixtureEmailDomain,
	} {
		moved := *got
		moved.Email = email
		require.ErrorIs(t, cs.UpdateUser(ctx, &moved), store.ErrTestFixtureKindRefused, email)
	}
	got.DisplayName = "renamed"
	require.NoError(t, cs.UpdateUser(ctx, got), "an in-place update keeps the domain")

	human := &store.User{ID: uuid.NewString(), Email: "h-" + uuid.NewString()[:8] + "@example.com", DisplayName: "h"}
	require.NoError(t, cs.CreateUser(ctx, human))
	human.Email = "moved@" + store.TestHubAdminEmailDomain
	require.ErrorIs(t, cs.UpdateUser(ctx, human), store.ErrTestFixtureKindRefused)
}

// The issuance binding check compares expiries at their stored precision:
// a binding expiry equal to the identity's before the identity's round
// trip (sub-microsecond digits the stored row no longer has) is allowed,
// and the expiry to store is returned at that precision. One microsecond
// later is refused.
func TestCheckTestAdminSystemBinding_ComparesAtStoredPrecision(t *testing.T) {
	stored := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	u := &ent.User{Kind: user.Kind(store.UserKindTestHubAdmin), ExpiresAt: &stored}
	same := stored.Add(999 * time.Nanosecond)
	rb := &store.RoleBinding{ScopeType: store.RoleScopeSystem, CreatedBy: store.TestIdentityIssuanceCreatedBy, ExpiresAt: &same}
	exp, err := checkTestAdminSystemBinding(u, rb)
	require.NoError(t, err)
	require.NotNil(t, exp)
	assert.True(t, exp.Equal(stored), "the expiry is stored at the identity's precision")

	later := stored.Add(time.Microsecond)
	rb.ExpiresAt = &later
	_, err = checkTestAdminSystemBinding(u, rb)
	require.ErrorIs(t, err, store.ErrInvalidInput)
}

// An identity expiry with sub-microsecond digits is issued on either
// backend, and the stored binding expires exactly when the stored identity
// does.
func TestCreateTestAdminUser_SubMicrosecondExpiry(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	rd := newTestAdminSystemRole(t, cs, "hub-admin")
	exp := time.Now().Add(time.Hour).Truncate(time.Microsecond).Add(999 * time.Nanosecond)
	u := newTestAdminUser(store.UserKindTestHubAdmin, uuid.NewString(), exp)
	created, err := cs.CreateTestAdminUser(ctx, u, rd.ID)
	require.NoError(t, err)
	require.NotNil(t, created.ExpiresAt)

	got, err := cs.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	bindings, err := cs.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	require.NotNil(t, bindings[0].ExpiresAt)
	assert.True(t, bindings[0].ExpiresAt.Equal(*got.ExpiresAt), "binding %v, identity %v", bindings[0].ExpiresAt, got.ExpiresAt)
	assert.True(t, created.ExpiresAt.Equal(*got.ExpiresAt))
}
