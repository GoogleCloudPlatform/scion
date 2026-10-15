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
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The steward precondition (design 4.5, ptone/scion#5144): every store
// write a steward can reach checks, under the artifact's lock, that the
// artifact is still homed and owned as it was when the steward authority
// was decided.

// stewardWrite is one of the seven steward-reachable store writes, made
// on artifact a (owned by agent-1, homed in project-1) as actor. It
// returns the write's error.
type stewardWrite struct {
	name string
	// prep makes what the write needs (a grant or link to remove).
	prep  func(t *testing.T, st Store, a *Artifact) string
	write func(st Store, a *Artifact, target string, actor *Actor) error
}

func stewardWrites() []stewardWrite {
	ctx := context.Background()
	noPrep := func(*testing.T, Store, *Artifact) string { return "" }
	return []stewardWrite{
		{"PutGrant", noPrep, func(st Store, a *Artifact, _ string, actor *Actor) error {
			_, err := st.PutGrant(ctx, &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal,
				SubjectRef: "user:outsider", Permission: GrantRead, CreatedAt: time.Now()}, 10, true, actor)
			return err
		}},
		{"CreateLink", noPrep, func(st Store, a *Artifact, _ string, actor *Actor) error {
			return st.CreateLink(ctx, linkGrant(a.ID, LinkTokenHash(uuid.NewString()), time.Now().Add(time.Hour)), 5, time.Now(), actor)
		}},
		{"DeleteGrant", func(t *testing.T, st Store, a *Artifact) string {
			g := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal, SubjectRef: "user:victim",
				Permission: GrantRead, CreatedAt: time.Now()}
			if _, err := st.PutGrant(ctx, g, 10, true, testActor); err != nil {
				t.Fatal(err)
			}
			return g.ID
		}, func(st Store, a *Artifact, id string, actor *Actor) error {
			return st.DeleteGrant(ctx, a.ID, id, actor)
		}},
		{"RevokeLink", func(t *testing.T, st Store, a *Artifact) string {
			l := linkGrant(a.ID, LinkTokenHash(uuid.NewString()), time.Now().Add(time.Hour))
			if err := st.CreateLink(ctx, l, 5, time.Now(), testActor); err != nil {
				t.Fatal(err)
			}
			return l.ID
		}, func(st Store, a *Artifact, id string, actor *Actor) error {
			return st.RevokeLink(ctx, a.ID, id, actor)
		}},
		{"SetExpiry", noPrep, func(st Store, a *Artifact, _ string, actor *Actor) error {
			exp := time.Now().Add(24 * time.Hour)
			_, err := st.SetExpiry(ctx, a.ID, &exp, actor)
			return err
		}},
		{"UpdateArtifact", noPrep, func(st Store, a *Artifact, _ string, actor *Actor) error {
			_, err := st.UpdateArtifact(ctx, a.ID, ArtifactUpdate{SetExpiry: true, ExpiresAt: nil, Actor: actor})
			return err
		}},
		{"DeleteArtifact", noPrep, func(st Store, a *Artifact, _ string, actor *Actor) error {
			_, err := st.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: actor, ScopeRef: a.ScopeRef})
			return err
		}},
	}
}

// stewardChanges are the concurrent changes a steward write must not
// commit across: a move, and an ownership transfer (the artifact becomes
// user-owned; written directly, as the transfer route does not exist yet).
var stewardChanges = []struct {
	name   string
	change func(t *testing.T, db *sql.DB, st Store, a *Artifact)
}{
	{"move", func(t *testing.T, _ *sql.DB, st Store, a *Artifact) {
		if _, err := st.UpdateArtifact(context.Background(), a.ID, ArtifactUpdate{HomeGrant: scopeGrant(a.ID, "project-2", GrantRead),
			MaxGrants: 10, Actor: testActor}); err != nil {
			t.Fatal(err)
		}
	}},
	{"transfer", func(t *testing.T, db *sql.DB, st Store, a *Artifact) {
		q := `UPDATE artifact SET owner_kind = 'user', owner_ref = 'user-new' WHERE id = ?`
		if st.(*sqlStore).dialect == dialectPostgres {
			q = `UPDATE artifact SET owner_kind = 'user', owner_ref = 'user-new' WHERE id = $1`
		}
		if _, err := db.Exec(q, a.ID); err != nil {
			t.Fatal(err)
		}
	}},
}

// snapshot is what a refused write must leave unchanged.
type snapshot struct {
	scope, ownerKind, ownerRef, expires string
	grants, events                      int
}

func takeSnapshot(t *testing.T, st Store, id string) snapshot {
	t.Helper()
	a, err := st.GetArtifact(context.Background(), id)
	if err != nil {
		t.Fatalf("artifact gone: %v", err)
	}
	gs, err := st.ListGrants(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot{scope: a.ScopeRef, ownerKind: a.OwnerKind, ownerRef: a.OwnerRef, grants: len(gs), events: len(eventsOf(t, st, id))}
	if a.ExpiresAt != nil {
		s.expires = a.ExpiresAt.UTC().String()
	}
	return s
}

// TestStoreStewardExpect: on each of the seven writes, a steward actor
// whose Expect no longer matches (after a move or a transfer) is refused
// with ErrStewardPrecondition, which is ErrNotFound, writing nothing; a
// matching Expect succeeds; a steward actor without an Expect is refused;
// an owner actor without one is not checked.
func TestStoreStewardExpect(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		for _, w := range stewardWrites() {
			for _, ch := range stewardChanges {
				t.Run(w.name+"/after "+ch.name, func(t *testing.T) {
					a := seedOwned(t, st, PrincipalKindAgent, "agent-1")
					target := w.prep(t, st, a)
					actor := &Actor{Kind: PrincipalKindUser, Ref: "user-s", Authority: AuthorityStewardDelegator, Expect: expectFor(a)}
					ch.change(t, db, st, a)
					before := takeSnapshot(t, st, a.ID)
					err := w.write(st, a, target, actor)
					if !errors.Is(err, ErrStewardPrecondition) || !errors.Is(err, ErrNotFound) {
						t.Fatalf("write after a %s: %v; want ErrStewardPrecondition", ch.name, err)
					}
					if after := takeSnapshot(t, st, a.ID); after != before {
						t.Fatalf("refused write changed the artifact:\n%+v\n%+v", before, after)
					}
				})
			}
			t.Run(w.name+"/matching", func(t *testing.T) {
				a := seedOwned(t, st, PrincipalKindAgent, "agent-1")
				target := w.prep(t, st, a)
				actor := &Actor{Kind: PrincipalKindUser, Ref: "user-s", Authority: AuthorityStewardProjectOwner, Expect: expectFor(a)}
				if err := w.write(st, a, target, actor); err != nil {
					t.Fatalf("write with a matching Expect: %v", err)
				}
				evs := eventsOf(t, st, a.ID)
				if len(evs) == 0 {
					t.Fatal("the write recorded no event")
				}
				if last := evs[len(evs)-1]; last.Authority != AuthorityStewardProjectOwner || last.ActorRef != "user-s" {
					t.Errorf("event = %+v", last)
				}
			})
			t.Run(w.name+"/steward without Expect", func(t *testing.T) {
				a := seedOwned(t, st, PrincipalKindAgent, "agent-1")
				target := w.prep(t, st, a)
				before := takeSnapshot(t, st, a.ID)
				actor := &Actor{Kind: PrincipalKindUser, Ref: "user-s", Authority: AuthorityStewardDelegator}
				if err := w.write(st, a, target, actor); !errors.Is(err, errNoExpect) {
					t.Fatalf("steward write without Expect: %v", err)
				}
				if after := takeSnapshot(t, st, a.ID); after != before {
					t.Fatalf("refused write changed the artifact")
				}
			})
			t.Run(w.name+"/owner after move", func(t *testing.T) {
				a := seedOwned(t, st, PrincipalKindAgent, "agent-1")
				target := w.prep(t, st, a)
				stewardChanges[0].change(t, db, st, a)
				moved := *a
				moved.ScopeRef = "project-2"
				// The owner path passes no Expect: no precondition.
				if err := w.write(st, &moved, target, testActor); err != nil {
					t.Fatalf("owner write after a move: %v", err)
				}
			})
		}
	})
}

// racingStore makes a concurrent change (race) just before each
// steward-reachable write, between the route's authority decision and the
// store's commit.
type racingStore struct {
	Store
	race func()
}

func (s *racingStore) PutGrant(ctx context.Context, g *Grant, max int, cross bool, actor *Actor) (bool, error) {
	s.race()
	return s.Store.PutGrant(ctx, g, max, cross, actor)
}

func (s *racingStore) CreateLink(ctx context.Context, g *Grant, max int, now time.Time, actor *Actor) error {
	s.race()
	return s.Store.CreateLink(ctx, g, max, now, actor)
}

func (s *racingStore) DeleteGrant(ctx context.Context, artifactID, grantID string, actor *Actor) error {
	s.race()
	return s.Store.DeleteGrant(ctx, artifactID, grantID, actor)
}

func (s *racingStore) RevokeLink(ctx context.Context, artifactID, linkID string, actor *Actor) error {
	s.race()
	return s.Store.RevokeLink(ctx, artifactID, linkID, actor)
}

func (s *racingStore) UpdateArtifact(ctx context.Context, artifactID string, u ArtifactUpdate) (*Artifact, error) {
	s.race()
	return s.Store.UpdateArtifact(ctx, artifactID, u)
}

func (s *racingStore) DeleteArtifact(ctx context.Context, artifactID string, opts DeleteOptions) (DeleteResult, error) {
	s.race()
	return s.Store.DeleteArtifact(ctx, artifactID, opts)
}

// TestStewardRaceAnswersNotFound: a steward's share, link, removal, expiry
// change or delete that races a move or an ownership transfer answers 404
// and commits nothing against the new state, and the decision log names
// steward_precondition_failed. The same racing write by an admin grantee
// is not checked (its authority does not depend on the home or owner).
func TestStewardRaceAnswersNotFound(t *testing.T) {
	writes := map[string]bool{"grant user read": true, "create link": true, "remove grant": true,
		"revoke link": true, "set expiry": true, "delete": true}
	for _, act := range stewardActions() {
		if !writes[act.name] {
			continue
		}
		for _, ch := range []string{"move", "transfer"} {
			t.Run(act.name+"/"+ch, func(t *testing.T) {
				f := newStewardFixture(t)
				c := captureLog(f.fixture, slog.LevelDebug)
				once := false
				f.svc.SetStore(&racingStore{Store: f.store, race: func() {
					if once {
						return
					}
					once = true
					ctx := context.Background()
					if ch == "move" {
						if _, err := f.store.UpdateArtifact(ctx, f.id, ArtifactUpdate{HomeGrant: scopeGrant(f.id, "project-2", GrantRead),
							MaxGrants: MaxGrantsPerArtifact, Actor: testActor}); err != nil {
							t.Fatal(err)
						}
						return
					}
					if _, err := f.db.Exec(`UPDATE artifact SET owner_kind = 'user', owner_ref = 'user-new' WHERE id = ?`, f.id); err != nil {
						t.Fatal(err)
					}
				}})
				before := takeSnapshot(t, f.store, f.id)
				c.take(t)
				rec := act.do(f, delegSteward)
				if rec.Code != http.StatusNotFound {
					t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
				}
				after := takeSnapshot(t, f.store, f.id)
				// The race itself moved or transferred the artifact (and a
				// move wrote one event and swapped the home grant); the
				// steward's write added nothing.
				wantEvents := before.events
				if ch == "move" {
					wantEvents++
				}
				if after.events != wantEvents || after.expires != "" {
					t.Fatalf("racing steward write committed: events %d -> %d, %+v", before.events, after.events, after)
				}
				entries, _ := c.take(t)
				found := false
				for _, e := range entries {
					if e[logKeyReason] == logReasonStewardPreconditionFailed {
						found = true
					}
				}
				if !found {
					t.Errorf("no steward_precondition_failed entry: %v", entries)
				}
			})
		}
	}
	t.Run("admin grantee is not checked", func(t *testing.T) {
		f := newStewardFixture(t)
		once := false
		f.svc.SetStore(&racingStore{Store: f.store, race: func() {
			if once {
				return
			}
			once = true
			if _, err := f.db.Exec(`UPDATE artifact SET owner_kind = 'user', owner_ref = 'user-new' WHERE id = ?`, f.id); err != nil {
				t.Fatal(err)
			}
		}})
		if rec := f.jsonDo(stAdminGrantee, http.MethodPost, linksPath(f.id), ""); rec.Code != http.StatusCreated {
			t.Fatalf("admin grantee link across a transfer: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// TestStoreCreationEvents: PutGrant, PutReviewGrant and CreateLink need an
// actor and record grant_created, grant_changed (only when the permission
// changes) and link_created by it in the write's own transaction
// (ptone/scion#4882); a refused write records nothing.
func TestStoreCreationEvents(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		a := seedOwned(t, st, PrincipalKindAgent, "agent-1")
		g := func(perm string) *Grant {
			return &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal, SubjectRef: "user:x", Permission: perm, CreatedAt: time.Now()}
		}
		for _, bad := range []*Actor{nil, {Kind: "user", Ref: "u"}} {
			if _, err := st.PutGrant(ctx, g(GrantRead), 10, true, bad); !errors.Is(err, errNoActor) {
				t.Errorf("PutGrant with actor %+v: %v", bad, err)
			}
			if _, err := st.PutReviewGrant(ctx, g(GrantReview), 10, "project-1", bad); !errors.Is(err, errNoActor) {
				t.Errorf("PutReviewGrant with actor %+v: %v", bad, err)
			}
			if err := st.CreateLink(ctx, linkGrant(a.ID, LinkTokenHash("x"), time.Now().Add(time.Hour)), 5, time.Now(), bad); !errors.Is(err, errNoActor) {
				t.Errorf("CreateLink with actor %+v: %v", bad, err)
			}
		}
		if gs, _ := st.ListGrants(ctx, a.ID); len(gs) != 1 || len(eventsOf(t, st, a.ID)) != 0 {
			t.Fatalf("refused writes changed the artifact: %+v", gs)
		}

		first := g(GrantRead)
		if _, err := st.PutGrant(ctx, first, 10, true, testActor); err != nil {
			t.Fatal(err)
		}
		e := onlyEvent(t, st, a.ID, EventGrantCreated, testActor)
		if e.Detail["grantId"] != first.ID || e.Detail["subjectKind"] != SubjectPrincipal || e.Detail["subjectRef"] != "user:x" || e.Detail["permission"] != GrantRead {
			t.Errorf("grant_created detail = %+v", e.Detail)
		}
		admin := &Actor{Kind: PrincipalKindUser, Ref: "user-9", Authority: AuthorityAdminGrant}
		if _, err := st.PutGrant(ctx, g(GrantWrite), 10, true, admin); err != nil {
			t.Fatal(err)
		}
		e = onlyEvent(t, st, a.ID, EventGrantChanged, admin)
		if e.Detail["grantId"] != first.ID || e.Detail["oldPermission"] != GrantRead || e.Detail["newPermission"] != GrantWrite {
			t.Errorf("grant_changed detail = %+v", e.Detail)
		}
		// The same permission again changes nothing and records nothing.
		if _, err := st.PutGrant(ctx, g(GrantWrite), 10, true, admin); err != nil {
			t.Fatal(err)
		}
		onlyEvent(t, st, a.ID, EventGrantChanged, admin)

		rg := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal, SubjectRef: "user:r", Permission: GrantReview, CreatedAt: time.Now()}
		reviewActor := &Actor{Kind: PrincipalKindUser, Ref: "user-pa", Authority: AuthorityCustodianProjectAdmin}
		if _, err := st.PutReviewGrant(ctx, rg, 10, "project-1", reviewActor); err != nil {
			t.Fatal(err)
		}
		if evs := eventsOf(t, st, a.ID); len(evs) == 0 || evs[len(evs)-1].Action != EventGrantCreated || evs[len(evs)-1].ActorRef != "user-pa" ||
			evs[len(evs)-1].Detail["permission"] != GrantReview {
			t.Errorf("review grant events = %+v", evs)
		}

		token := "the-token-is-never-recorded"
		link := linkGrant(a.ID, LinkTokenHash(token), time.Now().Add(time.Hour))
		if err := st.CreateLink(ctx, link, 5, time.Now(), testActor); err != nil {
			t.Fatal(err)
		}
		e = onlyEvent(t, st, a.ID, EventLinkCreated, testActor)
		if e.Detail["linkId"] != link.ID || e.Detail["expiresAt"] != dbRounded(*link.ExpiresAt).Format(time.RFC3339Nano) || len(e.Detail) != 2 {
			t.Errorf("link_created detail = %+v", e.Detail)
		}
		var raw string
		q := `SELECT COALESCE(detail, '') FROM artifact_event WHERE id = ?`
		if st.(*sqlStore).dialect == dialectPostgres {
			q = `SELECT COALESCE(detail, '') FROM artifact_event WHERE id = $1`
		}
		if err := db.QueryRow(q, e.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if raw == "" || strings.Contains(raw, token) || strings.Contains(raw, LinkTokenHash(token)) {
			t.Errorf("link_created row detail %q carries the token or its hash", raw)
		}
		// A refused link (too many) records nothing.
		n := len(eventsOf(t, st, a.ID))
		if err := st.CreateLink(ctx, linkGrant(a.ID, LinkTokenHash("y"), time.Now().Add(time.Hour)), 1, time.Now(), testActor); !errors.Is(err, ErrTooManyLinks) {
			t.Fatalf("over the cap: %v", err)
		}
		if len(eventsOf(t, st, a.ID)) != n {
			t.Errorf("a refused link recorded an event")
		}
	})
}
