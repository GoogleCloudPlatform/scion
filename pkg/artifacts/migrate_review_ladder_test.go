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

package artifacts

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// rawGrant writes an artifact_grant row directly, as an older binary or the
// removed review path did, with no event.
func rawGrant(t *testing.T, db *sql.DB, s *sqlStore, artifactID, kind, ref, perm, by string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := db.ExecContext(context.Background(), s.rebind(`INSERT INTO artifact_grant
		(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
		VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`), id, artifactID, kind, ref, perm, by, s.timeArg(time.Now())); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	return id
}

func grantPermission(t *testing.T, db *sql.DB, s *sqlStore, id string) string {
	t.Helper()
	var p string
	if err := db.QueryRowContext(context.Background(), s.rebind(`SELECT permission FROM artifact_grant WHERE id = ?`), id).Scan(&p); err != nil {
		t.Fatalf("read grant %s: %v", id, err)
	}
	return p
}

// migrationEvents returns the grant_changed events of artifactID recorded by
// a migration.
func migrationEvents(t *testing.T, st Store, artifactID string) []Event {
	t.Helper()
	var out []Event
	for _, e := range eventsOf(t, st, artifactID) {
		if e.Action == EventGrantChanged && e.Authority == AuthorityMigration {
			out = append(out, e)
		}
	}
	return out
}

// TestMigration0014ReviewLadder: the step converts exactly the user write
// grants on live agent-owned artifacts that no grant event names (the
// removed review-only path's rows), records one grant_changed per
// conversion, and leaves every grant a real writer holds alone.
func TestMigration0014ReviewLadder(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		s := st.(*sqlStore)
		agentArt := seedOwned(t, st, PrincipalKindAgent, "agent-1")
		userArt := seedOwned(t, st, PrincipalKindUser, "owner-u")
		goneArt := seedOwned(t, st, PrincipalKindAgent, "agent-2")

		// Converted: the review path's event-less write grant, and one
		// re-PUT at write since (a no-op, so still no event).
		legacy := rawGrant(t, db, s, agentArt.ID, SubjectPrincipal, "user:legacy", GrantWrite, "user:delegator")
		reput := rawGrant(t, db, s, agentArt.ID, SubjectPrincipal, "user:reput", GrantWrite, "user:delegator")
		if put, err := st.PutGrant(ctx, &Grant{ID: uuid.NewString(), ArtifactID: agentArt.ID, SubjectKind: SubjectPrincipal,
			SubjectRef: "user:reput", Permission: GrantWrite, CreatedAt: time.Now()}, 100, false, testActor); err != nil || put != GrantPutUnchanged {
			t.Fatalf("re-PUT at write = %v, %v; want unchanged", put, err)
		}

		// Not converted.
		keep := map[string]string{
			"user write on a user-owned artifact, no event": rawGrant(t, db, s, userArt.ID, SubjectPrincipal, "user:pre-events", GrantWrite, "user:owner-u"),
			"scope write on an agent artifact":              rawGrant(t, db, s, agentArt.ID, SubjectScope, "project-2", GrantWrite, "user:x"),
			"link row":                                      rawGrant(t, db, s, agentArt.ID, SubjectLink, "link-hash", GrantWrite, "user:x"),
			"agent principal write, no event":               rawGrant(t, db, s, agentArt.ID, SubjectPrincipal, "agent:other", GrantWrite, "user:x"),
			"sponsor review grant":                          rawGrant(t, db, s, agentArt.ID, SubjectPrincipal, "user:sponsor", GrantReview, SponsorReviewCreator),
			"user write on a deleted agent artifact":        rawGrant(t, db, s, goneArt.ID, SubjectPrincipal, "user:on-deleted", GrantWrite, "user:x"),
		}
		// Through the real PutGrant, so the grantId key its events carry is
		// the one the step reads: a create at write (grant_created), and a
		// create at read raised to write (grant_changed).
		steward := &Grant{ID: uuid.NewString(), ArtifactID: agentArt.ID, SubjectKind: SubjectPrincipal,
			SubjectRef: "user:steward-given", Permission: GrantWrite, CreatedAt: time.Now()}
		if put, err := st.PutGrant(ctx, steward, 100, false, testActor); err != nil || put != GrantPutCreated {
			t.Fatalf("PutGrant write = %v, %v; want created", put, err)
		}
		keep["write created through PutGrant (grant_created)"] = steward.ID
		raised := &Grant{ID: uuid.NewString(), ArtifactID: agentArt.ID, SubjectKind: SubjectPrincipal,
			SubjectRef: "user:raised", Permission: GrantRead, CreatedAt: time.Now()}
		if put, err := st.PutGrant(ctx, raised, 100, false, testActor); err != nil || put != GrantPutCreated {
			t.Fatalf("PutGrant read = %v, %v; want created", put, err)
		}
		raise := &Grant{ID: uuid.NewString(), ArtifactID: agentArt.ID, SubjectKind: SubjectPrincipal,
			SubjectRef: "user:raised", Permission: GrantWrite, CreatedAt: time.Now()}
		if put, err := st.PutGrant(ctx, raise, 100, false, testActor); err != nil || put != GrantPutChanged || raise.ID != raised.ID {
			t.Fatalf("PutGrant raise = %v, %v (id %s, want %s); want changed", put, err, raise.ID, raised.ID)
		}
		keep["read raised to write through PutGrant (grant_changed)"] = raised.ID
		if _, err := db.ExecContext(ctx, s.rebind(`UPDATE artifact SET deleted_at = ? WHERE id = ?`), s.timeArg(time.Now()), goneArt.ID); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{}
		for name, id := range keep {
			before[name] = grantPermission(t, db, s, id)
		}

		// Run the step again over these rows.
		if _, err := db.ExecContext(ctx, s.rebind(`DELETE FROM artifact_migrations WHERE name = ?`), migrationReviewLadder); err != nil {
			t.Fatal(err)
		}
		if err := st.Init(ctx); err != nil {
			t.Fatalf("Init: %v", err)
		}

		for _, id := range []string{legacy, reput} {
			if p := grantPermission(t, db, s, id); p != GrantReview {
				t.Errorf("grant %s = %s, want review", id, p)
			}
		}
		for name, id := range keep {
			if p := grantPermission(t, db, s, id); p != before[name] {
				t.Errorf("%s: permission %s, want %s (not converted)", name, p, before[name])
			}
		}
		evs := migrationEvents(t, st, agentArt.ID)
		if len(evs) != 2 {
			t.Fatalf("migration events = %d, want 2: %+v", len(evs), evs)
		}
		seen := map[string]bool{}
		for _, e := range evs {
			if e.ActorKind != ActorKindSystem || e.ActorRef != AuthorityMigration || e.Detail["oldPermission"] != GrantWrite ||
				e.Detail["newPermission"] != GrantReview || e.Detail["subjectKind"] != SubjectPrincipal {
				t.Errorf("migration event = %+v", e)
			}
			id, _ := e.Detail["grantId"].(string)
			seen[id] = true
		}
		if !seen[legacy] || !seen[reput] {
			t.Errorf("migration events name %v, want %s and %s", seen, legacy, reput)
		}
		for _, a := range []*Artifact{userArt, goneArt} {
			if n := len(migrationEvents(t, st, a.ID)); n != 0 {
				t.Errorf("artifact %s: %d migration events, want 0", a.ID, n)
			}
		}
		var recorded int
		if err := db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_migrations WHERE name = ?`), migrationReviewLadder).Scan(&recorded); err != nil || recorded != 1 {
			t.Fatalf("ledger rows for %s = %d, %v; want 1", migrationReviewLadder, recorded, err)
		}

		// A second Init changes nothing.
		if err := st.Init(ctx); err != nil {
			t.Fatalf("second Init: %v", err)
		}
		if n := len(migrationEvents(t, st, agentArt.ID)); n != 2 {
			t.Errorf("after a second Init: %d migration events, want 2", n)
		}

		// The counts the step logs: nothing left to convert, and every user
		// principal review grant counted (the 2 converted and the sponsor
		// grant).
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		r, err := s.convertReviewLadder(ctx, tx, time.Now())
		if err != nil || r.converted != 0 || r.undecodable != 0 || r.reviewGrants != 3 {
			t.Errorf("convertReviewLadder = %+v, %v; want 0 converted, 0 undecodable, 3 review grants", r, err)
		}
	})
}

// TestNoMigrationNamed0012: 0012 is the sponsor backfill's ledger name, not
// a schema step (schema.go).
func TestNoMigrationNamed0012(t *testing.T) {
	for _, m := range migrations {
		if strings.HasPrefix(m.name, "0012_") {
			t.Errorf("migration %q: 0012 is reserved for %s", m.name, migrationSponsorBackfill)
		}
	}
}

// TestMigrationLedgerOrder: every step is named NNNN_name and the numbers
// rise strictly, so a step appended later, or ptone/scion#6849's 0013, must
// keep that order.
func TestMigrationLedgerOrder(t *testing.T) {
	re := regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+$`)
	last := 0
	for _, m := range migrations {
		g := re.FindStringSubmatch(m.name)
		if g == nil {
			t.Fatalf("migration %q: want NNNN_name", m.name)
		}
		n, _ := strconv.Atoi(g[1])
		if n <= last {
			t.Errorf("migration %q (%d) does not follow %d", m.name, n, last)
		}
		last = n
	}
}

// TestGrantWritesAllowlist: the only non-test statements that insert or
// update artifact_grant rows are the ones listed here. Every one of them
// that writes a principal grant records its grant event in the same
// transaction (TestPutGrantEventFaultRollsBack), which is what lets the
// review ladder step identify the removed review path's rows by "no grant
// event". A new site must be added here, and to that reasoning. Only the
// non-test files of pkg/artifacts are scanned; a writer outside the package
// is out of this test's scope.
func TestGrantWritesAllowlist(t *testing.T) {
	want := map[string]int{
		"store_grants.go:putGrant":                     1, // a level change; grant_changed
		"store_grants.go:insertGrant":                  1, // putGrant's create (grant_created) and moveArtifact's home scope grant
		"store_links.go:CreateLink":                    1, // a link row; link_created
		"store_sql.go:createArtifact":                  1, // the home scope grant, and the automatic review grant with its grant_created
		"migrate_review_ladder.go:convertReviewLadder": 1, // the review ladder step; grant_changed
	}
	// Matched over the whole file, so a statement split across lines is
	// still found, and attributed to the func it sits in. INSERT OR <word>
	// INTO and REPLACE INTO count as inserts.
	stmt := regexp.MustCompile(`(?is)\b(INSERT\s+(?:OR\s+\w+\s+)?INTO|REPLACE\s+INTO|UPDATE)\s+artifact_grant\b`)
	fn := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?([A-Za-z0-9_]+)\(`)
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		funcs := fn.FindAllSubmatchIndex(src, -1)
		for _, m := range stmt.FindAllIndex(src, -1) {
			current := ""
			for _, f := range funcs {
				if f[0] < m[0] {
					current = string(src[f[2]:f[3]])
				}
			}
			got[name+":"+current]++
		}
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if got[k] != want[k] {
			t.Errorf("artifact_grant writes in %s = %d, want %d", k, got[k], want[k])
		}
	}
}

// TestPutGrantEventFaultRollsBack: when the grant event cannot be written,
// the grant write rolls back with it, on both the create and the level
// change branch, so no principal grant is ever stored without its event.
func TestPutGrantEventFaultRollsBack(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		s := st.(*sqlStore)
		a := seedOwned(t, st, PrincipalKindUser, "owner-u")
		breakEvents := func() {
			t.Helper()
			if _, err := db.ExecContext(ctx, `ALTER TABLE artifact_event RENAME TO artifact_event_off`); err != nil {
				t.Fatal(err)
			}
		}
		restoreEvents := func() {
			t.Helper()
			if _, err := db.ExecContext(ctx, `ALTER TABLE artifact_event_off RENAME TO artifact_event`); err != nil {
				t.Fatal(err)
			}
		}
		grant := func(perm string) *Grant {
			return &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal, SubjectRef: "user:u2",
				Permission: perm, CreatedAt: time.Now()}
		}
		count := func() int {
			t.Helper()
			var n int
			if err := db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_grant WHERE artifact_id = ? AND subject_ref = ?`),
				a.ID, "user:u2").Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}

		breakEvents()
		if _, err := st.PutGrant(ctx, grant(GrantRead), 100, false, testActor); err == nil {
			t.Fatal("create with no event table: want an error")
		}
		restoreEvents()
		if n := count(); n != 0 {
			t.Fatalf("after a failed create: %d grants, want 0", n)
		}

		g := grant(GrantRead)
		if _, err := st.PutGrant(ctx, g, 100, false, testActor); err != nil {
			t.Fatal(err)
		}
		breakEvents()
		if _, err := st.PutGrant(ctx, grant(GrantWrite), 100, false, testActor); err == nil {
			t.Fatal("level change with no event table: want an error")
		}
		restoreEvents()
		var p string
		if err := db.QueryRowContext(ctx, s.rebind(`SELECT permission FROM artifact_grant WHERE artifact_id = ? AND subject_ref = ?`),
			a.ID, "user:u2").Scan(&p); err != nil || p != GrantRead {
			t.Fatalf("after a failed level change: permission %q, %v; want read", p, err)
		}
	})
}

// TestMigration0014DefersOnUnreadableEvent: when a grant event's detail is
// NULL, empty, unreadable or naming no grant, the step converts nothing and records
// no ledger row, Init still succeeds, and the steps after it still run and
// are recorded (a sentinel step appended for the test); once the event is
// repaired, the next Init converts and records the step once.
func TestMigration0014DefersOnUnreadableEvent(t *testing.T) {
	for _, bc := range []struct {
		name   string
		detail any
	}{
		{"undecodable", "{not json"},
		{"NULL", nil},
		{"empty", ""},
		{"no grantId", "{}"},
		{"JSON null", "null"},
	} {
		t.Run(bc.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
				ctx := context.Background()
				s := st.(*sqlStore)
				a := seedOwned(t, st, PrincipalKindAgent, "agent-1")
				legacy := rawGrant(t, db, s, a.ID, SubjectPrincipal, "user:legacy", GrantWrite, "user:delegator")
				bad := uuid.NewString()
				if _, err := db.ExecContext(ctx, s.rebind(`INSERT INTO artifact_event
					(id, artifact_id, at, action, actor_kind, actor_ref, authority, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
					bad, a.ID, s.timeArg(time.Now()), EventGrantCreated, PrincipalKindUser, "user-t", AuthorityOwner, bc.detail); err != nil {
					t.Fatal(err)
				}
				// A later step, so the deferral's "continue with the later
				// steps" branch runs: it creates a table and writes a marker.
				const sentinel = "9999_test_sentinel"
				saved := migrations
				migrations = append(append([]migration(nil), saved...), migration{
					name:     sentinel,
					sqlite:   `CREATE TABLE artifact_test_sentinel (n INTEGER NOT NULL)`,
					postgres: `CREATE TABLE artifact_test_sentinel (n INTEGER NOT NULL)`,
					apply: func(ctx context.Context, tx *sql.Tx, _ *sqlStore) error {
						_, err := tx.ExecContext(ctx, `INSERT INTO artifact_test_sentinel (n) VALUES (1)`)
						return err
					},
				})
				t.Cleanup(func() { migrations = saved })
				count := func(q string, args ...any) int {
					t.Helper()
					var n int
					if err := db.QueryRowContext(ctx, s.rebind(q), args...).Scan(&n); err != nil {
						t.Fatal(err)
					}
					return n
				}
				if _, err := db.ExecContext(ctx, s.rebind(`DELETE FROM artifact_migrations WHERE name = ?`), migrationReviewLadder); err != nil {
					t.Fatal(err)
				}

				// (a) Deferred: nothing converted, no 0014 ledger row, the
				// sentinel step after it recorded, and Init succeeds.
				for i := 0; i < 2; i++ {
					if err := st.Init(ctx); err != nil {
						t.Fatalf("Init with an unreadable event: %v", err)
					}
					if p := grantPermission(t, db, s, legacy); p != GrantWrite {
						t.Fatalf("deferred step converted the grant to %s", p)
					}
					if n := count(`SELECT COUNT(*) FROM artifact_migrations WHERE name = ?`, migrationReviewLadder); n != 0 {
						t.Fatalf("deferred step recorded %d ledger rows", n)
					}
					if n := count(`SELECT COUNT(*) FROM artifact_migrations WHERE name = ?`, sentinel); n != 1 {
						t.Fatalf("the later step: %d ledger rows, want 1", n)
					}
					if n := count(`SELECT COUNT(*) FROM artifact_test_sentinel`); n != 1 {
						t.Fatalf("the later step's marker: %d rows, want 1", n)
					}
					if all := count(`SELECT COUNT(*) FROM artifact_migrations`); all != len(migrations)-1 {
						t.Fatalf("ledger rows %d, want %d", all, len(migrations)-1)
					}
					// Counted in SQL: ListEvents would refuse the unreadable detail.
					if n := count(`SELECT COUNT(*) FROM artifact_event WHERE artifact_id = ? AND authority = ?`, a.ID, AuthorityMigration); n != 0 {
						t.Fatalf("deferred step wrote %d events", n)
					}
				}

				// (b) Repaired: the next Init converts and records the step once.
				if _, err := db.ExecContext(ctx, s.rebind(`UPDATE artifact_event SET detail = ? WHERE id = ?`), `{"grantId":"`+uuid.NewString()+`"}`, bad); err != nil {
					t.Fatal(err)
				}
				if err := st.Init(ctx); err != nil {
					t.Fatalf("Init after the repair: %v", err)
				}
				if p := grantPermission(t, db, s, legacy); p != GrantReview {
					t.Fatalf("after the repair: grant %s, want review", p)
				}
				if n := count(`SELECT COUNT(*) FROM artifact_migrations WHERE name = ?`, migrationReviewLadder); n != 1 {
					t.Fatalf("after the repair: %d ledger rows for %s, want 1", n, migrationReviewLadder)
				}
				if all := count(`SELECT COUNT(*) FROM artifact_migrations`); all != len(migrations) {
					t.Fatalf("after the repair: ledger rows %d, want %d", all, len(migrations))
				}
				if n := count(`SELECT COUNT(*) FROM artifact_test_sentinel`); n != 1 {
					t.Fatalf("after the repair: the later step ran again (%d markers)", n)
				}
				if n := len(migrationEvents(t, st, a.ID)); n != 1 {
					t.Fatalf("after the repair: %d migration events, want 1", n)
				}
			})
		})
	}
}

// TestMigration0014NoCandidatesIgnoresUnreadableEvent: with no candidate
// grant there is nothing to convert, so an unreadable grant event does not
// defer the step. The next Init records it, converts nothing and writes no
// event.
func TestMigration0014NoCandidatesIgnoresUnreadableEvent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		s := st.(*sqlStore)
		a := seedOwned(t, st, PrincipalKindAgent, "agent-1")
		// A user read grant on the agent's artifact: not a candidate, since
		// only write grants are.
		reader := rawGrant(t, db, s, a.ID, SubjectPrincipal, "user:reader", GrantRead, "user:delegator")
		if _, err := db.ExecContext(ctx, s.rebind(`INSERT INTO artifact_event
			(id, artifact_id, at, action, actor_kind, actor_ref, authority, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			uuid.NewString(), a.ID, s.timeArg(time.Now()), EventGrantCreated, PrincipalKindUser, "user-t", AuthorityOwner, "{not json"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, s.rebind(`DELETE FROM artifact_migrations WHERE name = ?`), migrationReviewLadder); err != nil {
			t.Fatal(err)
		}
		if err := st.Init(ctx); err != nil {
			t.Fatalf("Init with no candidates and an unreadable event: %v", err)
		}
		count := func(q string, args ...any) int {
			t.Helper()
			var n int
			if err := db.QueryRowContext(ctx, s.rebind(q), args...).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if n := count(`SELECT COUNT(*) FROM artifact_migrations WHERE name = ?`, migrationReviewLadder); n != 1 {
			t.Fatalf("%s: %d ledger rows, want 1 (recorded, not deferred)", migrationReviewLadder, n)
		}
		if p := grantPermission(t, db, s, reader); p != GrantRead {
			t.Fatalf("the read grant became %s", p)
		}
		// Counted in SQL: ListEvents would refuse the unreadable detail.
		if n := count(`SELECT COUNT(*) FROM artifact_event WHERE artifact_id = ? AND authority = ?`, a.ID, AuthorityMigration); n != 0 {
			t.Fatalf("the step wrote %d events", n)
		}
	})
}
