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
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// ---------------------------------------------------------------------------
// Regression coverage: every existing-record sign-in path must apply the
// same live sign-in policy and account-state handling that interactive
// login applies (see sign_in_policy.go / google_identity_resolver.go),
// against a real store so role assignment and grant sync exercise their
// real implementation rather than a fake.
// ---------------------------------------------------------------------------

// signInPolicyHarness bundles a GEExchangeService, its resolver, and the
// backing stores, wired exactly as server.go wires production. Tests that
// only need the exchange surface can use h.svc directly; tests that need to
// drive Resolve with a specific ResolvePolicy, or swap the audit logger,
// can use h.resolver / h.srv.
type signInPolicyHarness struct {
	svc      *GEExchangeService
	store    store.Store
	extStore ExternalIdentityStore
	srv      *Server
	resolver *GoogleIdentityResolver
}

// newSignInPolicyHarness builds a signInPolicyHarness backed by a real
// ent/SQLite store, with the resolver wired exactly as server.go wires
// production: existing-record sign-ins get the same account-state handling
// (invited activation, role re-evaluation, super-admin binding, grant sync,
// audit) as interactive login.
func newSignInPolicyHarness(t *testing.T, cfg ServerConfig, validator GoogleCredentialValidator) *signInPolicyHarness {
	t.Helper()
	driverName := sqliteDriverName()
	if driverName == "" {
		t.Skip("skipping: requires SQLite driver (excluded by no_sqlite build tag)")
	}

	dbPath := t.TempDir() + "/signin-policy-test.db"
	// Opens through entc.OpenSQLite (not a raw sql.Open) so the store
	// boundary gets the same "_timezone=UTC" DSN option and UTC mutation
	// hook as every other ent/SQLite client (tz-refactor design §2.1.2) — a
	// raw sql.Open here previously bypassed both, so a bare time.Now()
	// default (e.g. ExternalIdentity.CreatedAt, User.Created) stored a
	// numeric-zone-abbreviation wall clock under a Kathmandu-like
	// time.Local, which ent then failed to Scan back.
	client, err := entc.OpenSQLite("file:"+dbPath, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
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
	// existing-record branches the same account-state handling as
	// interactive login. The closures inside signInPolicyDeps read s.*
	// fields at call time, so mutating srv fields (e.g. auditLogger) after
	// this call still takes effect on the next Resolve/Exchange.
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
	return &signInPolicyHarness{svc: svc, store: st, extStore: extStore, srv: srv, resolver: resolver}
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

// recordingAuditLogger records invite audit events for assertions. Every
// other AuditLogger method is a no-op; nothing else under test here reads
// them.
type recordingAuditLogger struct {
	mu     sync.Mutex
	invite []*InviteAuditEvent
}

func (l *recordingAuditLogger) LogBrokerAuthEvent(context.Context, *BrokerAuthEvent) error {
	return nil
}
func (l *recordingAuditLogger) LogGCPTokenEvent(context.Context, *GCPTokenEvent) error { return nil }
func (l *recordingAuditLogger) LogGCSLinkFetchEvent(context.Context, *GCSLinkFetchEvent) error {
	return nil
}
func (l *recordingAuditLogger) LogInviteAuditEvent(_ context.Context, event *InviteAuditEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.invite = append(l.invite, event)
	return nil
}
func (l *recordingAuditLogger) LogLifecycleHookEvent(context.Context, *LifecycleHookEvent) error {
	return nil
}
func (l *recordingAuditLogger) LogLifecycleHookExecutionEvent(context.Context, *LifecycleHookExecutionEvent) error {
	return nil
}
func (l *recordingAuditLogger) LogAgentSecretReadEvent(context.Context, *AgentSecretReadEvent) error {
	return nil
}
func (l *recordingAuditLogger) RecordSAAssignment(context.Context, *store.SAAssignmentEvent) error {
	return nil
}

func (l *recordingAuditLogger) deniedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.invite {
		if e.EventType == InviteAuditLoginDenied && !e.Success {
			n++
		}
	}
	return n
}

// TestGEExchange_ExistingUserByEmail_InvitedActivatesConsistently is an
// end-to-end regression: an invited record, resolved by email through the
// GE exchange path (no prior Google binding), must be activated and granted
// hub access identically to an interactive-login sign-in.
func TestGEExchange_ExistingUserByEmail_InvitedActivatesConsistently(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}

	h := newSignInPolicyHarness(t, ServerConfig{UserAccessMode: "invite_only"}, validator)

	ctx := context.Background()
	invitedUserID := uuid.New().String()
	invitedBy := "admin@example.com"
	if err := h.store.CreateUser(ctx, &store.User{
		ID:        invitedUserID,
		Email:     identity.Email,
		Role:      store.UserRoleMember, // placeholder role on an invited row
		Status:    store.UserStatusInvited,
		InvitedBy: &invitedBy,
		Created:   time.Now(),
	}); err != nil {
		t.Fatalf("seed invited user: %v", err)
	}

	resp, status, err := h.svc.Exchange(ctx, &ExchangeRequest{
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

	stored, err := h.store.GetUser(ctx, invitedUserID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if stored.Status != store.UserStatusActive {
		t.Fatalf("expected invited record to be activated (status=active), got %q", stored.Status)
	}
	if !isHubMember(t, h.store, invitedUserID) {
		t.Error("expected the activated user to be granted hub-members access")
	}
}

// TestGEExchange_ExistingUserByEmail_NotAuthorized_DeniedNoBinding is a
// negative regression: a pre-existing record whose email fails the live
// sign-in policy must be denied — with no external identity binding created
// and no state mutation — even though it was found by email and not by an
// existing binding.
func TestGEExchange_ExistingUserByEmail_NotAuthorized_DeniedNoBinding(t *testing.T) {
	identity := validWorkspaceIdentity() // user@company.com
	validator := &fakeGoogleValidator{idTokenResult: identity}

	h := newSignInPolicyHarness(t, ServerConfig{
		UserAccessMode:    "domain_restricted",
		AuthorizedDomains: []string{"not-company.com"},
	}, validator)

	ctx := context.Background()
	activeUserID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID:      activeUserID,
		Email:   identity.Email,
		Role:    store.UserRoleMember,
		Status:  store.UserStatusActive,
		Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}

	_, status, err := h.svc.Exchange(ctx, &ExchangeRequest{
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
	stored, getErr := h.store.GetUser(ctx, activeUserID)
	if getErr != nil {
		t.Fatalf("get user: %v", getErr)
	}
	if stored.Role != store.UserRoleMember || stored.Status != store.UserStatusActive {
		t.Fatalf("expected the denied record to be unchanged, got role=%q status=%q", stored.Role, stored.Status)
	}

	// No identity link: a subsequent exchange under an authorized policy
	// must still resolve by email again (bootstrap), not by a binding
	// created during the denied attempt.
	if _, lookupErr := h.extStore.GetExternalIdentity(ctx, "google", googleCanonicalIssuer, identity.Subject); !errors.Is(lookupErr, store.ErrNotFound) {
		t.Fatalf("expected no external identity binding to be created on denial, lookup returned err=%v", lookupErr)
	}
}

// TestGEExchange_ExistingUserByEmail_NotAuthorized_WritesOneDenialAudit
// verifies the resolver path's denial writes exactly one audit record (the
// same failure event provisionUser's own denial path writes) — not zero
// (silently dropped) and not more than one (double-audited).
func TestGEExchange_ExistingUserByEmail_NotAuthorized_WritesOneDenialAudit(t *testing.T) {
	identity := validWorkspaceIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}

	h := newSignInPolicyHarness(t, ServerConfig{
		UserAccessMode:    "domain_restricted",
		AuthorizedDomains: []string{"not-company.com"},
	}, validator)
	auditLog := &recordingAuditLogger{}
	h.srv.auditLogger = auditLog

	ctx := context.Background()
	activeUserID := uuid.New().String()
	if err := h.store.CreateUser(ctx, &store.User{
		ID:      activeUserID,
		Email:   identity.Email,
		Role:    store.UserRoleMember,
		Status:  store.UserStatusActive,
		Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed active user: %v", err)
	}

	_, status, err := h.svc.Exchange(ctx, &ExchangeRequest{
		Credential:     "token",
		CredentialType: "id_token",
	})
	if err == nil {
		t.Fatal("expected an error for an unauthorized domain")
	}
	if status != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", status)
	}
	if got := auditLog.deniedCount(); got != 1 {
		t.Fatalf("expected exactly one denial audit record, got %d (total invite events=%d)", got, len(auditLog.invite))
	}
}

// TestGoogleIdentityResolver_EmailMatch_PreAuthorized_SkipsPolicyNotState is
// the other half of PreAuthorized coverage on the email-match branch: it
// skips the sign-in policy check only, never the account-state handling —
// an active record still gets its role re-evaluated and grants applied, and
// an invited record still activates and gets grants, even though the
// authorize callback here denies everyone.
func TestGoogleIdentityResolver_EmailMatch_PreAuthorized_SkipsPolicyNotState(t *testing.T) {
	neverAuthorized := func(context.Context, string) bool { return false }

	t.Run("active record succeeds", func(t *testing.T) {
		identity := validGmailIdentity()
		h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})
		h.resolver = NewGoogleIdentityResolver(h.store, h.extStore, neverAuthorized, nil, nil)
		h.resolver.SetSignInPolicyDeps(h.srv.signInPolicyDeps())

		ctx := context.Background()
		userID := uuid.New().String()
		if err := h.store.CreateUser(ctx, &store.User{
			ID: userID, Email: identity.Email, Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
		}); err != nil {
			t.Fatalf("seed active user: %v", err)
		}

		user, err := h.resolver.Resolve(ctx, identity, ResolvePolicy{PreAuthorized: true})
		if err != nil {
			t.Fatalf("expected PreAuthorized to admit the record despite a deny-all policy: %v", err)
		}
		if user.ID != userID {
			t.Fatalf("expected the existing record to be reused, got %q", user.ID)
		}
		if !isHubMember(t, h.store, userID) {
			t.Error("expected the record to be granted hub-members access")
		}
	})

	t.Run("invited record activates", func(t *testing.T) {
		identity := validWorkspaceIdentity()
		h := newSignInPolicyHarness(t, ServerConfig{}, &fakeGoogleValidator{idTokenResult: identity})
		h.resolver = NewGoogleIdentityResolver(h.store, h.extStore, neverAuthorized, nil, nil)
		h.resolver.SetSignInPolicyDeps(h.srv.signInPolicyDeps())

		ctx := context.Background()
		userID := uuid.New().String()
		invitedBy := "admin@example.com"
		if err := h.store.CreateUser(ctx, &store.User{
			ID: userID, Email: identity.Email, Role: store.UserRoleMember, Status: store.UserStatusInvited, InvitedBy: &invitedBy, Created: time.Now(),
		}); err != nil {
			t.Fatalf("seed invited user: %v", err)
		}

		user, err := h.resolver.Resolve(ctx, identity, ResolvePolicy{PreAuthorized: true})
		if err != nil {
			t.Fatalf("expected PreAuthorized to admit the record despite a deny-all policy: %v", err)
		}
		if user.Status != store.UserStatusActive {
			t.Fatalf("expected the invited record to activate, got status=%q", user.Status)
		}
		if !isHubMember(t, h.store, userID) {
			t.Error("expected the activated record to be granted hub-members access")
		}
	})
}
