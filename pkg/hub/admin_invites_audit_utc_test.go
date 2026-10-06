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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "time/tzdata"
)

// TestAdminInvitesCreate_AuditLogExpiresAtIsUTC covers the invite audit-log
// "expires_at" fix (admin_invites.go:177, design §2.2 "Invite audit-log
// expires_at") at the real call site, by driving handleAdminInvites end to
// end and inspecting the audit event it writes, rather than unit-testing a
// formatting helper in isolation.
//
// admin_invites.go:143's expiresAt := time.Now().Add(duration) is a non-UTC
// time.Time only when the process's TZ is non-UTC, so this test re-execs
// itself in a child process pinned to a fixed non-UTC zone (see below). It
// deliberately does not set time.Local in the current process: pkg/hub
// tests are unpinned, and writing time.Local in the shared test binary
// would race goroutines that other pkg/hub tests may have leaked.
func TestAdminInvitesCreate_AuditLogExpiresAtIsUTC(t *testing.T) {
	// CI runs this test binary with TZ unset (UTC), in which case
	// expiresAt above is already UTC regardless of whether admin_invites.go
	// converts it, so the assertions below would pass even if .UTC() were
	// removed from the real site. Re-exec just this test in a child process
	// with TZ=Asia/Tokyo so it is a real guard. Kathmandu is not used here:
	// under TZ=Asia/Kathmandu, newTestStore's migration hits a known,
	// pre-existing SQLite scan error (tz-refactor task 2) before this
	// handler ever runs.
	if os.Getenv("SCION_TZ_CHILD") == "" {
		pattern := "^" + regexp.QuoteMeta(t.Name()) + "$"
		cmd := exec.Command(os.Args[0], "-test.run="+pattern, "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), "SCION_TZ_CHILD=1", "TZ=Asia/Tokyo")
		out, err := cmd.CombinedOutput()
		switch {
		case err != nil:
			t.Fatalf("child process (TZ=Asia/Tokyo) failed: %v\n%s", err, out)
		case bytes.Contains(out, []byte("--- SKIP: "+t.Name())):
			// Propagate a skip from the child (e.g. the sqlite driver isn't
			// registered in this build) instead of masking it as either a
			// pass or a failure. Checked only once a zero exit status rules
			// out a crash or a race report that happens to print SKIP text
			// on its way down.
			t.Skip("child process skipped:\n" + string(out))
		case !bytes.Contains(out, []byte("--- PASS: "+t.Name())):
			// The child exited 0 but its output never reported running this
			// test by name -- e.g. -test.run matched no tests, which
			// "go test" treats as success ("testing: warning: no tests to
			// run"). Catches that case regardless of why the pattern or
			// flags stopped matching.
			t.Fatalf("child process did not report running %s (pattern %q matched nothing?):\n%s", t.Name(), pattern, out)
		}
		return
	}

	// Sanity-check that TZ=Asia/Tokyo actually took effect, so a silent
	// zone-lookup failure (e.g. no zoneinfo and no embedded tzdata) fails
	// loudly instead of leaving expiresAt already UTC and passing
	// vacuously. time.Local can't be compared to time.UTC by identity:
	// time.Local always points at the same *Location value, and a failed
	// TZ lookup leaves that value holding UTC data named "UTC" rather than
	// repointing time.Local at the time.UTC pointer, so the two never
	// compare equal either way -- check the actual UTC offset instead. The
	// _ "time/tzdata" import above also means this child never depends on
	// the host having system zoneinfo.
	if _, off := time.Now().Zone(); off != 9*60*60 {
		t.Fatalf("expected TZ=Asia/Tokyo (+09:00) in the child process, got time.Local=%q offset=%ds", time.Local, off)
	}

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
