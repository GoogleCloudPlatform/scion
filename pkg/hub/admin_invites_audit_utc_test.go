//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAdminInvitesCreate_AuditLogExpiresAtIsUTC covers the invite audit-log
// "expires_at" fix (admin_invites.go:177, design §2.2 "Invite audit-log
// expires_at") at the real call site, not merely a formatting helper.
//
// Review round 1, R1-1: the original test (TestFormatUTCTimestamp) called a
// free-standing formatUTCTimestamp(t, layout) helper directly. Reverting
// admin_invites.go:177 to its pre-fix form (invite.ExpiresAt.Format(...),
// no .UTC()) left that test green, because nothing exercised the handler.
// That helper is now deleted; this test drives handleAdminInvites end to
// end and inspects the audit event it writes.
//
// It deliberately does not pin time.Local (see R1-3's race/masking finding):
// pkg/hub tests are unpinned, and pinning time.Local here would race
// goroutines that other pkg/hub tests in the same binary may have leaked.
// Run under TZ=Asia/Tokyo or TZ=Asia/Kathmandu, admin_invites.go:143's
// expiresAt := time.Now().Add(duration) is already a non-UTC time.Time,
// which is exactly the input this fix must still format correctly -- so the
// required both-TZ runs exercise the bug class without any fixture helping.
func TestAdminInvitesCreate_AuditLogExpiresAtIsUTC(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	auditLog := &recordingAuditLogger{}
	srv := &Server{
		store:         s,
		config:        ServerConfig{HubEndpoint: "https://hub.example.com"},
		inviteService: NewInviteService(s),
		events:        noopEventPublisher{},
		auditLogger:   auditLog,
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := strings.NewReader(`{"expiresIn":"1h"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/invites", body)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()

	srv.handleAdminInvites(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp InviteCreateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Invite == nil {
		t.Fatal("response has no invite")
	}

	var created *InviteAuditEvent
	for _, e := range auditLog.invite {
		if e.EventType == InviteAuditInviteCreated {
			created = e
			break
		}
	}
	if created == nil {
		t.Fatalf("expected an %s audit event, got %d invite events: %+v", InviteAuditInviteCreated, len(auditLog.invite), auditLog.invite)
	}

	gotStr, ok := created.Details["expires_at"]
	if !ok {
		t.Fatalf("audit event details missing expires_at: %+v", created.Details)
	}
	if !strings.HasSuffix(gotStr, "Z") {
		t.Fatalf("expires_at = %q, want a Z-suffixed (UTC) RFC3339 timestamp", gotStr)
	}
	got, err := time.Parse(time.RFC3339, gotStr)
	if err != nil {
		t.Fatalf("parsing expires_at %q: %v", gotStr, err)
	}
	want := resp.Invite.ExpiresAt.Truncate(time.Second)
	if !got.Equal(want) {
		t.Fatalf("expires_at round-tripped to a different instant: got %v, want %v (resp.Invite.ExpiresAt=%v)", got, want, resp.Invite.ExpiresAt)
	}
}
