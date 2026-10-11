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
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testActor is the actor store tests make changes as.
var testActor = &Actor{Kind: PrincipalKindUser, Ref: "user-t", Authority: AuthorityOwner}

// seedOwned writes a published artifact owned by kind:ref in project-1.
func seedOwned(t *testing.T, st Store, kind, ref string) *Artifact {
	t.Helper()
	now := time.Now()
	a := &Artifact{ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: "project-1", OwnerKind: kind,
		OwnerRef: ref, Title: "T", CreatedAt: now, UpdatedAt: now}
	v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: "a.md",
		TotalBytes: 1, FileCount: 1, CreatedAt: now, State: VersionStateReady}
	f := File{VersionID: v.ID, Path: "a.md", Size: 1, SHA256: sha([]byte(a.ID)), MediaType: "text/markdown"}
	g := Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: "project-1", Permission: GrantRead, CreatedAt: now}
	if err := st.CreatePublished(context.Background(), a, v, []File{f}, []Grant{g}); err != nil {
		t.Fatal(err)
	}
	return a
}

func eventsOf(t *testing.T, st Store, id string) []Event {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return evs
}

// onlyEvent asserts that id has exactly one event with action and actor,
// and returns it.
func onlyEvent(t *testing.T, st Store, id, action string, actor *Actor) Event {
	t.Helper()
	var match []Event
	for _, e := range eventsOf(t, st, id) {
		if e.Action == action {
			match = append(match, e)
		}
	}
	if len(match) != 1 {
		t.Fatalf("%s events of %s = %+v; want one", action, id, match)
	}
	e := match[0]
	if e.ActorKind != actor.Kind || e.ActorRef != actor.Ref || e.Authority != actor.Authority || e.At.IsZero() || e.ID == "" {
		t.Errorf("%s event = %+v; want actor %+v", action, e, *actor)
	}
	return e
}

// TestStoreMigratesOrphans: a database at migration 0006 is upgraded by
// Init to the orphan columns and the event table; existing rows keep every
// value (no expiry change, no orphan mark) and Init stays idempotent.
func TestStoreMigratesOrphans(t *testing.T) {
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver).(*sqlStore)
			ledger := ledgerSQLite
			if st.dialect == dialectPostgres {
				ledger = ledgerPostgres
			}
			if _, err := db.Exec(ledger); err != nil {
				t.Fatal(err)
			}
			for _, m := range migrations {
				if m.name == migrationOrphans {
					break
				}
				ddl := m.sqlite
				if st.dialect == dialectPostgres {
					ddl = m.postgres
				}
				if _, err := db.Exec(ddl); err != nil {
					t.Fatalf("%s: %v", m.name, err)
				}
				if _, err := db.Exec(st.rebind("INSERT INTO artifact_migrations (name, applied_at) VALUES (?, ?)"), m.name, st.timeArg(time.Now())); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			exp := now.Add(48 * time.Hour)
			if _, err := db.Exec(st.rebind(`INSERT INTO artifact (id, scope_kind, scope_ref, owner_kind, owner_ref, title, current_seq,
				expires_at, created_at, updated_at) VALUES ('a1', 'project', 'p', 'agent', 'gone', 't', 1, ?, ?, ?)`),
				st.timeArg(exp), st.timeArg(now), st.timeArg(now)); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatalf("Init over 0006: %v", err)
			}
			a, err := st.GetArtifact(ctx, "a1")
			if err != nil {
				t.Fatal(err)
			}
			if a.OrphanedAt != nil || a.ExpiresAt == nil || !a.ExpiresAt.Equal(dbRounded(exp)) {
				t.Errorf("migrated row = %+v", a)
			}
			var nonNull int
			if err := db.QueryRow(`SELECT COUNT(*) FROM artifact WHERE orphaned_at IS NOT NULL OR sponsor_ref IS NOT NULL
				OR expiry_source IS NOT NULL OR former_owner_kind IS NOT NULL OR former_owner_ref IS NOT NULL
				OR transferred_at IS NOT NULL OR former_key IS NOT NULL`).Scan(&nonNull); err != nil || nonNull != 0 {
				t.Errorf("rows with a new column set = %d, %v; want 0 (no backfill)", nonNull, err)
			}
			if evs := eventsOf(t, st, "a1"); len(evs) != 0 {
				t.Errorf("events after migration = %+v", evs)
			}
			var n int
			if err := db.QueryRow("SELECT COUNT(*) FROM artifact_migrations").Scan(&n); err != nil || n != len(migrations) {
				t.Errorf("ledger rows = %d, %v; want %d", n, err, len(migrations))
			}
			if err := st.Init(ctx); err != nil {
				t.Errorf("Init again: %v", err)
			}
		})
	}
}

// TestStoreDeleteArtifact: the one soft delete removes grants and links
// with the artifact and records one deleted event with the counts, in one
// transaction; it needs an actor, refuses a deleted artifact, and with
// ExpiredAt deletes only an expired one.
func TestStoreDeleteArtifact(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		a := seedOwned(t, st, PrincipalKindAgent, "agent-1")
		if _, err := st.PutGrant(ctx, &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal,
			SubjectRef: "user:x", Permission: GrantRead, CreatedAt: time.Now()}, 10, true); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateLink(ctx, linkGrant(a.ID, LinkTokenHash("tok"), time.Now().Add(time.Hour)), 5, time.Now()); err != nil {
			t.Fatal(err)
		}

		for _, bad := range []*Actor{nil, {Kind: "user"}, {Kind: "user", Ref: "u"}} {
			if _, err := st.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: bad}); !errors.Is(err, errNoActor) {
				t.Errorf("DeleteArtifact with actor %+v: %v", bad, err)
			}
		}
		future := time.Now().Add(-time.Hour)
		if _, err := st.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: testActor, ExpiredAt: &future}); !errors.Is(err, ErrNotFound) {
			t.Errorf("ExpiredAt on an artifact without expiry: %v", err)
		}
		if _, err := st.GetArtifact(ctx, a.ID); err != nil {
			t.Fatalf("refused deletes changed the artifact: %v", err)
		}
		if evs := eventsOf(t, st, a.ID); len(evs) != 0 {
			t.Fatalf("refused deletes recorded events: %+v", evs)
		}

		res, err := st.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: testActor})
		if err != nil || res.Grants != 2 || res.Links != 1 {
			t.Fatalf("DeleteArtifact = %+v, %v; want 2 grants, 1 link", res, err)
		}
		if _, err := st.GetArtifact(ctx, a.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetArtifact after delete: %v", err)
		}
		if gs, err := st.ListGrants(ctx, a.ID); err != nil || len(gs) != 0 {
			t.Errorf("grants after delete = %+v, %v", gs, err)
		}
		if _, _, err := st.ResolveLink(ctx, LinkTokenHash("tok"), time.Now()); !errors.Is(err, ErrNotFound) {
			t.Errorf("link after delete: %v", err)
		}
		e := onlyEvent(t, st, a.ID, EventDeleted, testActor)
		if e.Detail["grantsRemoved"] != float64(2) || e.Detail["linksRevoked"] != float64(1) {
			t.Errorf("deleted detail = %+v", e.Detail)
		}
		if _, err := st.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: testActor}); !errors.Is(err, ErrNotFound) {
			t.Errorf("second delete: %v", err)
		}
		if _, err := st.DeleteArtifact(ctx, uuid.NewString(), DeleteOptions{Actor: testActor}); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete of a missing artifact: %v", err)
		}
		if n := len(eventsOf(t, st, a.ID)); n != 1 {
			t.Errorf("events after refused second delete = %d; want 1", n)
		}

		// The expiry sweep goes through the same primitive: actor system,
		// authority retention.
		x := seedOwned(t, st, PrincipalKindUser, "user-1")
		past := time.Now().Add(-time.Minute)
		if _, err := st.SetExpiry(ctx, x.ID, &past, testActor); err != nil {
			t.Fatal(err)
		}
		if n, err := st.SweepExpired(ctx, time.Now(), 10); err != nil || n != 1 {
			t.Fatalf("SweepExpired = %d, %v", n, err)
		}
		e = onlyEvent(t, st, x.ID, EventDeleted, &retentionActor)
		if e.Detail["grantsRemoved"] != float64(1) {
			t.Errorf("sweep detail = %+v", e.Detail)
		}
	})
}

// TestStoreEventsOnOwnerPaths: an expiry change, a move, a grant removal
// and a link revoke each record one event by the actor in the change's own
// transaction; without an actor the change is refused and nothing changes.
func TestStoreEventsOnOwnerPaths(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		a := seedOwned(t, st, PrincipalKindUser, "user-1")
		g := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal, SubjectRef: "user:x",
			Permission: GrantWrite, CreatedAt: time.Now()}
		if _, err := st.PutGrant(ctx, g, 10, true); err != nil {
			t.Fatal(err)
		}
		link := linkGrant(a.ID, LinkTokenHash("tok"), time.Now().Add(time.Hour))
		if err := st.CreateLink(ctx, link, 5, time.Now()); err != nil {
			t.Fatal(err)
		}
		exp := time.Now().Add(72 * time.Hour)

		// No actor: refused, nothing changed, nothing recorded.
		if _, err := st.SetExpiry(ctx, a.ID, &exp, nil); !errors.Is(err, errNoActor) {
			t.Errorf("SetExpiry without actor: %v", err)
		}
		if _, err := st.UpdateArtifact(ctx, a.ID, ArtifactUpdate{HomeGrant: scopeGrant(a.ID, "project-2", GrantRead), MaxGrants: 10}); !errors.Is(err, errNoActor) {
			t.Errorf("move without actor: %v", err)
		}
		if err := st.DeleteGrant(ctx, a.ID, g.ID, nil); !errors.Is(err, errNoActor) {
			t.Errorf("DeleteGrant without actor: %v", err)
		}
		if err := st.RevokeLink(ctx, a.ID, link.ID, &Actor{Kind: "user", Ref: "u"}); !errors.Is(err, errNoActor) {
			t.Errorf("RevokeLink without authority: %v", err)
		}
		got, _ := st.GetArtifact(ctx, a.ID)
		if gs, _ := st.ListGrants(ctx, a.ID); len(gs) != 3 || got.ExpiresAt != nil || got.ScopeRef != "project-1" {
			t.Fatalf("refused changes applied: %+v grants %+v", got, gs)
		}
		if evs := eventsOf(t, st, a.ID); len(evs) != 0 {
			t.Fatalf("refused changes recorded: %+v", evs)
		}

		admin := &Actor{Kind: PrincipalKindUser, Ref: "user-9", Authority: AuthorityAdminGrant}
		if _, err := st.SetExpiry(ctx, a.ID, &exp, testActor); err != nil {
			t.Fatal(err)
		}
		e := onlyEvent(t, st, a.ID, EventExpirySet, testActor)
		if e.Detail["newExpiresAt"] != dbRounded(exp).Format(time.RFC3339Nano) || e.Detail["oldExpiresAt"] != nil {
			t.Errorf("expiry_set detail = %+v", e.Detail)
		}
		if _, err := st.SetExpiry(ctx, a.ID, nil, admin); err != nil {
			t.Fatal(err)
		}
		e = onlyEvent(t, st, a.ID, EventExpiryCleared, admin)
		if e.Detail["oldExpiresAt"] != dbRounded(exp).Format(time.RFC3339Nano) || e.Detail["newExpiresAt"] != nil {
			t.Errorf("expiry_cleared detail = %+v", e.Detail)
		}

		if err := st.DeleteGrant(ctx, a.ID, g.ID, admin); err != nil {
			t.Fatal(err)
		}
		e = onlyEvent(t, st, a.ID, EventGrantRemoved, admin)
		if e.Detail["grantId"] != g.ID || e.Detail["subjectRef"] != "user:x" || e.Detail["permission"] != GrantWrite {
			t.Errorf("grant_removed detail = %+v", e.Detail)
		}
		if err := st.DeleteGrant(ctx, a.ID, g.ID, admin); !errors.Is(err, ErrNotFound) {
			t.Errorf("second DeleteGrant: %v", err)
		}

		if err := st.RevokeLink(ctx, a.ID, link.ID, testActor); err != nil {
			t.Fatal(err)
		}
		e = onlyEvent(t, st, a.ID, EventLinkRevoked, testActor)
		if e.Detail["linkId"] != link.ID {
			t.Errorf("link_revoked detail = %+v", e.Detail)
		}

		home, _ := st.ListGrants(ctx, a.ID)
		if len(home) != 1 {
			t.Fatalf("grants before move = %+v", home)
		}
		if _, err := st.UpdateArtifact(ctx, a.ID, ArtifactUpdate{HomeGrant: scopeGrant(a.ID, "project-2", GrantRead),
			MaxGrants: 10, Actor: testActor}); err != nil {
			t.Fatal(err)
		}
		e = onlyEvent(t, st, a.ID, EventMoved, testActor)
		if e.Detail["fromScope"] != "project-1" || e.Detail["toScope"] != "project-2" || e.Detail["droppedGrantId"] != home[0].ID {
			t.Errorf("moved detail = %+v", e.Detail)
		}
		if n := len(eventsOf(t, st, a.ID)); n != 5 {
			t.Errorf("events = %d; want 5", n)
		}
		// A move to the current home is no move and records nothing.
		if _, err := st.UpdateArtifact(ctx, a.ID, ArtifactUpdate{HomeGrant: scopeGrant(a.ID, "project-2", GrantRead),
			MaxGrants: 10, Actor: testActor}); err != nil {
			t.Fatal(err)
		}
		if n := len(eventsOf(t, st, a.ID)); n != 5 {
			t.Errorf("events after a no-op move = %d; want 5", n)
		}
	})
}

// TestStoreOwnerReconcilerQueries: owners page in (kind, ref) order with
// their mark counts, deleted artifacts do not count, and SetOwnerOrphaned
// sets and clears the mark once per artifact, a batch at a time, with one
// event each.
func TestStoreOwnerReconcilerQueries(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		gone := []*Artifact{seedOwned(t, st, PrincipalKindAgent, "a-1"), seedOwned(t, st, PrincipalKindAgent, "a-1"),
			seedOwned(t, st, PrincipalKindAgent, "a-1")}
		seedOwned(t, st, PrincipalKindAgent, "a-2")
		seedOwned(t, st, PrincipalKindUser, "u-1")
		dead := seedOwned(t, st, PrincipalKindUser, "u-dead")
		if _, err := st.DeleteArtifact(ctx, dead.ID, DeleteOptions{Actor: testActor}); err != nil {
			t.Fatal(err)
		}

		if n, err := st.CountLiveOwners(ctx, nil); err != nil || n != 3 {
			t.Errorf("CountLiveOwners = %d, %v; want 3", n, err)
		}
		page, err := st.ListLiveOwners(ctx, nil, 2)
		if err != nil || len(page) != 2 || page[0] != (Owner{Kind: "agent", Ref: "a-1", Unmarked: 3}) || page[1].Ref != "a-2" {
			t.Fatalf("first page = %+v, %v", page, err)
		}
		after := &Owner{Kind: page[1].Kind, Ref: page[1].Ref}
		if n, err := st.CountLiveOwners(ctx, after); err != nil || n != 1 {
			t.Errorf("CountLiveOwners after a-2 = %d, %v; want 1", n, err)
		}
		page, err = st.ListLiveOwners(ctx, after, 2)
		if err != nil || len(page) != 1 || page[0].Kind != "user" || page[0].Ref != "u-1" {
			t.Fatalf("second page = %+v, %v", page, err)
		}
		if _, err := st.ListLiveOwners(ctx, nil, 0); err == nil {
			t.Error("ListLiveOwners with no limit accepted")
		}

		now := time.Now()
		owner := Owner{Kind: PrincipalKindAgent, Ref: "a-1"}
		if n, err := st.SetOwnerOrphaned(ctx, owner, true, now, 2); err != nil || n != 2 {
			t.Fatalf("first batch = %d, %v; want 2", n, err)
		}
		if n, err := st.SetOwnerOrphaned(ctx, owner, true, now, 2); err != nil || n != 1 {
			t.Fatalf("second batch = %d, %v; want 1", n, err)
		}
		if n, err := st.SetOwnerOrphaned(ctx, owner, true, now.Add(time.Hour), 2); err != nil || n != 0 {
			t.Fatalf("marking again = %d, %v; want 0", n, err)
		}
		for _, a := range gone {
			got, err := st.GetArtifact(ctx, a.ID)
			if err != nil || got.OrphanedAt == nil || !got.OrphanedAt.Equal(dbRounded(now)) {
				t.Errorf("artifact %s after marking = %+v, %v", a.ID, got, err)
			}
			e := onlyEvent(t, st, a.ID, EventOrphaned, &reconcilerActor)
			if e.Detail["owner"] != "agent:a-1" {
				t.Errorf("orphaned detail = %+v", e.Detail)
			}
		}
		page, _ = st.ListLiveOwners(ctx, nil, 1)
		if page[0].Marked != 3 || page[0].Unmarked != 0 {
			t.Errorf("counts after marking = %+v", page[0])
		}
		// A listed candidate carries the mark too.
		cands, err := st.ListCandidates(ctx, CandidateQuery{PrincipalKind: PrincipalKindAgent, PrincipalRef: "a-1", OwnedOnly: true, Limit: 10, Now: time.Now()})
		if err != nil || len(cands) != 3 || cands[0].OrphanedAt == nil {
			t.Errorf("candidates = %+v, %v", cands, err)
		}

		if n, err := st.SetOwnerOrphaned(ctx, owner, false, now, 10); err != nil || n != 3 {
			t.Fatalf("clearing = %d, %v; want 3", n, err)
		}
		if n, err := st.SetOwnerOrphaned(ctx, owner, false, now, 10); err != nil || n != 0 {
			t.Fatalf("clearing again = %d, %v; want 0", n, err)
		}
		for _, a := range gone {
			if got, _ := st.GetArtifact(ctx, a.ID); got.OrphanedAt != nil {
				t.Errorf("artifact %s still marked", a.ID)
			}
			onlyEvent(t, st, a.ID, EventUnorphaned, &reconcilerActor)
		}
		// A deleted artifact is never marked.
		if n, err := st.SetOwnerOrphaned(ctx, Owner{Kind: PrincipalKindUser, Ref: "u-dead"}, true, now, 10); err != nil || n != 0 {
			t.Errorf("marking a deleted artifact's owner = %d, %v", n, err)
		}
		if _, err := st.SetOwnerOrphaned(ctx, owner, true, now, 0); err == nil {
			t.Error("SetOwnerOrphaned with no limit accepted")
		}
	})
}
