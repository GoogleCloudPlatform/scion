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

package hub

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// ---------------------------------------------------------------------------
// Regression coverage: the resolver's existing-record-by-email branch (see
// sign_in_policy.go / google_identity_resolver.go) must apply the same live
// sign-in policy and account-state handling that interactive login applies,
// end to end through the GE exchange endpoint, against a real store so role
// assignment and grant sync exercise their real implementation rather than a
// fake.
// ---------------------------------------------------------------------------

// newSignInPolicyExchangeService builds a GEExchangeService backed by a real
// ent/SQLite store, with the resolver wired exactly as server.go wires
// production: existing-record-by-email sign-ins get the same account-state
// handling (invited activation, role re-evaluation, super-admin binding,
// grant sync, audit) as interactive login. Returns the service, the backing
// store, and the external-identity store so tests can seed/assert state.
func newSignInPolicyExchangeService(t *testing.T, cfg ServerConfig, validator GoogleCredentialValidator) (*GEExchangeService, store.Store, ExternalIdentityStore) {
	t.Helper()
	driverName := sqliteDriverName()
	if driverName == "" {
		t.Skip("skipping: requires SQLite driver (excluded by no_sqlite build tag)")
	}

	dbPath := t.TempDir() + "/signin-policy-test.db"
	dsn := "file:" + dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		t.Fatalf("enable sqlite foreign keys: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
	t.Cleanup(func() { _ = client.Close() })
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	st := entadapter.NewCompositeStore(client)
	extStore := entadapter.NewExternalIdentityStore(client)

	ctx := context.Background()
	seedRoleDefinitions(ctx, st)
	seedDefaultGroupsAndBindings(ctx, st)

	srv := &Server{
		store:       st,
		auditLogger: &LogAuditLogger{},
		config:      cfg,
	}

	resolver := NewGoogleIdentityResolver(st, extStore, srv.isUserAuthorized, nil, nil)
	// Matches server.go's production wiring: give the resolver's
	// existing-record-by-email branch the same account-state handling as
	// interactive login.
	resolver.SetSignInPolicyDeps(srv.signInPolicyDeps())

	tokenSvc, err := NewUserTokenService(UserTokenConfig{AccessTokenDuration: DefaultGETokenTTL})
	if err != nil {
		t.Fatalf("new user token service: %v", err)
	}
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator,
		tokenSvc,
		resolver,
		slog.Default(),
	)
	return svc, st, extStore
}

// isHubMember reports whether userID is a member of the canonical
// "hub-members" group, the positive authority source syncHubRoleGrants
// grants a "member"-role user.
func isHubMember(t *testing.T, st store.Store, userID string) bool {
	t.Helper()
	group, err := st.GetGroupBySlug(context.Background(), "hub-members")
	if err != nil {
		t.Fatalf("get hub-members group: %v", err)
	}
	_, err = st.GetGroupMembership(context.Background(), group.ID, store.GroupMemberTypeUser, userID)
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	t.Fatalf("get group membership: %v", err)
	return false
}

// TestGEExchange_ExistingUserByEmail_InvitedActivatesConsistently is the
// Phase-1 end-to-end regression: an invited record, resolved by email
// through the GE exchange path (no prior Google binding), must be activated
// and granted hub access identically to an interactive-login sign-in.
func TestGEExchange_ExistingUserByEmail_InvitedActivatesConsistently(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}

	svc, st, _ := newSignInPolicyExchangeService(t, ServerConfig{UserAccessMode: "invite_only"}, validator)

	ctx := context.Background()
	invitedUserID := uuid.New().String()
	invitedBy := "admin@example.com"
	if err := st.CreateUser(ctx, &store.User{
		ID:        invitedUserID,
		Email:     identity.Email,
		Role:      store.UserRoleMember, // placeholder role on an invited row
		Status:    store.UserStatusInvited,
		InvitedBy: &invitedBy,
		Created:   time.Now(),
	}); err != nil {
		t.Fatalf("seed invited user: %v", err)
	}

	resp, status, err := svc.Exchange(ctx, &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if resp.AccessToken == "" {
		t.Fatal("expected a Hub access token to be issued")
	}
	if resp.User.ID != invitedUserID {
		t.Fatalf("expected the invited record to be reused, got user %q", resp.User.ID)
	}
	if resp.User.Role != store.UserRoleMember {
		t.Fatalf("expected role %q, got %q", store.UserRoleMember, resp.User.Role)
	}

	stored, err := st.GetUser(ctx, invitedUserID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if stored.Status != store.UserStatusActive {
		t.Fatalf("expected invited record to be activated (status=active), got %q", stored.Status)
	}
	if !isHubMember(t, st, invitedUserID) {
		t.Error("expected the activated user to be granted hub-members access")
	}
}

// TestGEExchange_ExistingUserByEmail_NotAuthorized_DeniedNoBinding is the
// Phase-1 negative regression: a pre-existing record whose email fails the
// live sign-in policy must be denied — with no external identity binding
// created and no state mutation — even though it was found by email and not
// by an existing binding.
func TestGEExchange_ExistingUserByEmail_NotAuthorized_DeniedNoBinding(t *testing.T) {
	identity := validWorkspaceIdentity() // user@company.com
	validator := &fakeGoogleValidator{idTokenResult: identity}

	svc, st, extStore := newSignInPolicyExchangeService(t, ServerConfig{
		UserAccessMode:    "domain_restricted",
		AuthorizedDomains: []string{"not-company.com"},
	}, validator)

	ctx := context.Background()
	activeUserID := uuid.New().String()
	if err := st.CreateUser(ctx, &store.User{
		ID:      activeUserID,
		Email:   identity.Email,
		Role:    store.UserRoleMember,
		Status:  store.UserStatusActive,
		Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}

	_, status, err := svc.Exchange(ctx, &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected an error for an unauthorized domain")
	}
	if status != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", status)
	}

	// No state mutation: the record must be untouched.
	stored, getErr := st.GetUser(ctx, activeUserID)
	if getErr != nil {
		t.Fatalf("get user: %v", getErr)
	}
	if stored.Role != store.UserRoleMember || stored.Status != store.UserStatusActive {
		t.Fatalf("expected the denied record to be unchanged, got role=%q status=%q", stored.Role, stored.Status)
	}

	// No identity link: a subsequent exchange under an authorized policy
	// must still resolve by email again (bootstrap), not by a binding
	// created during the denied attempt.
	if _, lookupErr := extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, identity.Subject); !errors.Is(lookupErr, store.ErrNotFound) {
		t.Fatalf("expected no external identity binding to be created on denial, lookup returned err=%v", lookupErr)
	}
}
