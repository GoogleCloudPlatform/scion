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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

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
	if err := autoMigrateTestClient(context.Background(), client); err != nil {
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
