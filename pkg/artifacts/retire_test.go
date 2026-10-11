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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- owner liveness reconciler ---

// fakeChecker is a PrincipalChecker answering from a table: a ref in gone
// is not live, a ref in live is live, any other ref is left out of the
// answer. failOn makes the call with that index (0-based) fail.
type fakeChecker struct {
	mu     sync.Mutex
	gone   map[string]bool
	live   map[string]bool
	failOn int
	calls  [][]string
}

func newFakeChecker() *fakeChecker {
	return &fakeChecker{gone: map[string]bool{}, live: map[string]bool{}, failOn: -1}
}

var errChecker = errors.New("checker down")

func (c *fakeChecker) PrincipalsExist(_ context.Context, refs []string) (map[string]bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.calls)
	c.calls = append(c.calls, append([]string(nil), refs...))
	if n == c.failOn {
		return nil, errChecker
	}
	out := map[string]bool{}
	for _, r := range refs {
		switch {
		case c.gone[r]:
			out[r] = false
		case c.live[r]:
			out[r] = true
		}
	}
	return out, nil
}

func orphanedAt(t *testing.T, st Store, id string) *time.Time {
	t.Helper()
	a, err := st.GetArtifact(context.Background(), id)
	if err != nil {
		t.Fatalf("GetArtifact %s: %v", id, err)
	}
	return a.OrphanedAt
}

// TestReconcileOwnersGoneAndBack: a pass marks the artifacts of an owner
// the checker reports gone, once; leaves live and unanswered owners alone;
// and a later pass clears the mark when the owner is back (a restored
// agent). Each change records one event.
func TestReconcileOwnersGoneAndBack(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		gone1 := seedOwned(t, st, PrincipalKindAgent, "agent-gone")
		gone2 := seedOwned(t, st, PrincipalKindAgent, "agent-gone")
		live := seedOwned(t, st, PrincipalKindUser, "user-live")
		unanswered := seedOwned(t, st, PrincipalKindAgent, "agent-unknown")
		c := newFakeChecker()
		c.gone["agent:agent-gone"] = true
		c.live["user:user-live"] = true

		now := time.Now()
		res, err := ReconcileOwners(ctx, st, c, now)
		if err != nil || res != (ReconcileResult{Owners: 3, Orphaned: 2}) {
			t.Fatalf("first pass = %+v, %v", res, err)
		}
		if len(c.calls) != 1 || len(c.calls[0]) != 3 {
			t.Errorf("checker calls = %v; want one call with three refs", c.calls)
		}
		for _, a := range []*Artifact{gone1, gone2} {
			if got := orphanedAt(t, st, a.ID); got == nil || !got.Equal(dbRounded(now)) {
				t.Errorf("artifact of a gone owner: orphaned_at = %v", got)
			}
		}
		for _, a := range []*Artifact{live, unanswered} {
			if got := orphanedAt(t, st, a.ID); got != nil {
				t.Errorf("artifact of a live or unanswered owner marked at %v", got)
			}
		}

		// Idempotent: a second pass changes nothing and keeps the first mark.
		res, err = ReconcileOwners(ctx, st, c, now.Add(time.Hour))
		if err != nil || res != (ReconcileResult{Owners: 3}) {
			t.Fatalf("second pass = %+v, %v", res, err)
		}
		if got := orphanedAt(t, st, gone1.ID); !got.Equal(dbRounded(now)) {
			t.Errorf("second pass moved orphaned_at to %v", got)
		}

		// The owner is back: the mark is cleared.
		delete(c.gone, "agent:agent-gone")
		c.live["agent:agent-gone"] = true
		res, err = ReconcileOwners(ctx, st, c, now.Add(2*time.Hour))
		if err != nil || res != (ReconcileResult{Owners: 3, Unorphaned: 2}) {
			t.Fatalf("restore pass = %+v, %v", res, err)
		}
		for _, a := range []*Artifact{gone1, gone2} {
			if got := orphanedAt(t, st, a.ID); got != nil {
				t.Errorf("artifact of a restored owner still marked at %v", got)
			}
			onlyEvent(t, st, a.ID, EventOrphaned, &reconcilerActor)
			onlyEvent(t, st, a.ID, EventUnorphaned, &reconcilerActor)
		}
		// An owner no longer answered at all counts as live: its mark is
		// cleared too.
		c.gone["agent:agent-gone"] = true
		delete(c.live, "agent:agent-gone")
		if _, err := ReconcileOwners(ctx, st, c, now); err != nil {
			t.Fatal(err)
		}
		delete(c.gone, "agent:agent-gone")
		if res, err := ReconcileOwners(ctx, st, c, now); err != nil || res.Unorphaned != 2 {
			t.Errorf("unanswered pass = %+v, %v; want the mark cleared", res, err)
		}

		// No checker: nothing happens.
		if res, err := ReconcileOwners(ctx, st, nil, now); err != nil || res != (ReconcileResult{}) {
			t.Errorf("nil checker = %+v, %v", res, err)
		}
	})
}

// TestReconcileOwnersCheckerError: a checker error changes nothing on the
// failing page or after it, reports how many owners it left unprocessed,
// and stops at that page; pages before it are applied.
func TestReconcileOwnersCheckerError(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		// ownerPageSize + 5 owners, every one gone: two pages.
		var all []*Artifact
		c := newFakeChecker()
		for i := 0; i < ownerPageSize+5; i++ {
			ref := fmt.Sprintf("agent-%03d", i)
			all = append(all, seedOwned(t, st, PrincipalKindAgent, ref))
			c.gone[PrincipalRef(PrincipalKindAgent, ref)] = true
		}

		// Failing on the first page: nothing changes at all.
		c.failOn = 0
		res, err := ReconcileOwners(ctx, st, c, time.Now())
		var cerr *CheckerError
		if !errors.As(err, &cerr) || !errors.Is(err, errChecker) || cerr.Unprocessed != ownerPageSize+5 {
			t.Fatalf("error on the first page = %v (%+v); want a CheckerError with %d unprocessed", err, cerr, ownerPageSize+5)
		}
		if res != (ReconcileResult{}) || len(c.calls) != 1 {
			t.Errorf("result %+v, %d checker calls; want nothing done after one call", res, len(c.calls))
		}
		for _, a := range all {
			if orphanedAt(t, st, a.ID) != nil {
				t.Fatalf("artifact %s marked despite the checker error", a.ID)
			}
			if evs := eventsOf(t, st, a.ID); len(evs) != 0 {
				t.Fatalf("events despite the checker error: %+v", evs)
			}
		}

		// Failing on the second page: the first page is applied, the stop
		// point is the second page, and its 5 owners stay unmarked.
		c.calls, c.failOn = nil, 1
		res, err = ReconcileOwners(ctx, st, c, time.Now())
		if !errors.As(err, &cerr) || cerr.Unprocessed != 5 {
			t.Fatalf("error on the second page = %v (%+v); want 5 unprocessed", err, cerr)
		}
		if res != (ReconcileResult{Owners: ownerPageSize, Orphaned: ownerPageSize}) || len(c.calls) != 2 || len(c.calls[1]) != 5 {
			t.Errorf("result %+v, calls %d; want the first page applied and the second asked once", res, len(c.calls))
		}
		for i, a := range all {
			if marked := orphanedAt(t, st, a.ID) != nil; marked != (i < ownerPageSize) {
				t.Errorf("owner %d marked = %v", i, marked)
			}
		}

		// The next pass, with a working checker, finishes the job.
		c.calls, c.failOn = nil, -1
		res, err = ReconcileOwners(ctx, st, c, time.Now())
		if err != nil || res.Orphaned != 5 || res.Owners != ownerPageSize+5 || len(c.calls) != 2 {
			t.Errorf("recovery pass = %+v, %v, %d calls", res, err, len(c.calls))
		}
	})
}

// --- DELETE /api/v1/artifacts/{id} ---

var (
	adminGrantee = principal{PrincipalKindUser, "user-admin-grantee", ""}
	retireAdmin  = principal{PrincipalKindUser, "user-project-admin", ""}
)

func artifactPath(id string) string { return "/api/v1/artifacts/" + id }

func (f *fixture) del(p principal, id string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(&p, http.MethodDelete, artifactPath(id), nil, nil)
}

// retireFixture is a fixture with an artifact owned by agentA in
// project-1 (now orphaned), readable by userU (a project member), with an
// admin grant to adminGrantee, and with retireAdmin an admin of project-1.
func retireFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t, false)
	f.svc.SetViewKey(testViewKey)
	id := f.publish(agentA, "agent.md", []byte("# by an agent"), "").Artifact.ID
	if _, err := f.db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES (?, ?, 'principal', ?, 'admin', ?)`, uuid.NewString(), id, PrincipalRef(adminGrantee.kind, adminGrantee.ref),
		time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
		t.Fatal(err)
	}
	f.host.allow(retireAdmin, "project-1", PermissionRead)
	f.host.allowAdmin(retireAdmin, "project-1")
	if _, err := f.store.SetOwnerOrphaned(context.Background(), Owner{Kind: agentA.kind, Ref: agentA.ref}, true, time.Now(), 10); err != nil {
		t.Fatal(err)
	}
	return f, id
}

// TestDeleteArtifactAllowed: the owner, an admin grantee and an admin of
// the home project may delete; each delete answers 204 and records one
// deleted event with the caller and its authority.
func TestDeleteArtifactAllowed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		who       principal
		authority string
	}{
		{"admin grantee", adminGrantee, AuthorityAdminGrant},
		{"project admin custodian", retireAdmin, AuthorityCustodianProjectAdmin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, id := retireFixture(t)
			if rec := f.del(tc.who, id); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
			}
			onlyEvent(t, f.store, id, EventDeleted, &Actor{Kind: tc.who.kind, Ref: tc.who.ref, Authority: tc.authority})
			if rec := f.do(&tc.who, http.MethodGet, artifactPath(id), nil, nil); rec.Code != http.StatusNotFound {
				t.Errorf("GET after delete: %d", rec.Code)
			}
		})
	}
	t.Run("user owner", func(t *testing.T) {
		f := newFixture(t, false)
		id := f.publish(userU, "mine.md", []byte("mine"), "scope=project-1").Artifact.ID
		if rec := f.del(userU, id); rec.Code != http.StatusNoContent {
			t.Fatalf("owner delete: %d %s", rec.Code, rec.Body.String())
		}
		onlyEvent(t, f.store, id, EventDeleted, &Actor{Kind: userU.kind, Ref: userU.ref, Authority: AuthorityOwner})
	})
}

// TestDeleteArtifactRefused: a reader without authority gets 403 with one
// fixed body whatever the reason (a member who is not an admin, an agent,
// even the owning agent, an admin whose credential does not permit
// artifact.manage, an admin grantee likewise); a caller that cannot read
// gets the 404 of a missing artifact, byte for byte. Nothing is deleted
// and nothing recorded.
func TestDeleteArtifactRefused(t *testing.T) {
	f, id := retireFixture(t)
	scopedAdmin := principal{PrincipalKindUser, "user-scoped-admin", ""}
	f.host.allow(scopedAdmin, "project-1", PermissionRead)
	f.host.allowAdmin(scopedAdmin, "project-1")
	f.host.deny(scopedAdmin, "project-1", PermissionManage)
	scopedGrantee := principal{PrincipalKindUser, "user-scoped-grantee", ""}
	f.exec(t, `INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES (?, ?, 'principal', ?, 'admin', ?)`, uuid.NewString(), id, PrincipalRef(scopedGrantee.kind, scopedGrantee.ref), linkNow())
	f.host.deny(scopedGrantee, "project-1", PermissionManage)
	// An admin of another project is not a custodian here.
	otherAdmin := principal{PrincipalKindUser, "user-other-admin", ""}
	f.host.allow(otherAdmin, "project-1", PermissionRead)
	f.host.allowAdmin(otherAdmin, "project-2")

	var forbidden string
	for _, who := range []principal{userU, agentA, agentB, scopedAdmin, scopedGrantee, otherAdmin} {
		rec := f.del(who, id)
		if rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
			t.Fatalf("%s: %d %s; want 403", who.ref, rec.Code, rec.Body.String())
		}
		if forbidden == "" {
			forbidden = response(rec)
		} else if got := response(rec); got != forbidden {
			t.Errorf("%s: the 403 differs:\n%s\nwant\n%s", who.ref, got, forbidden)
		}
	}

	missing := response(f.del(outside, uuid.NewString()))
	for _, who := range []principal{outside, agentX} {
		rec := f.del(who, id)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d; want 404", who.ref, rec.Code)
		}
		if got := response(rec); got != missing {
			t.Errorf("%s: the 404 differs from a missing artifact's:\n%s\nwant\n%s", who.ref, got, missing)
		}
	}
	if rec := f.do(nil, http.MethodDelete, artifactPath(id), nil, nil); response(rec) != missing {
		t.Errorf("unauthenticated delete: %s", response(rec))
	}
	if _, err := f.store.GetArtifact(context.Background(), id); err != nil {
		t.Fatalf("a refused delete deleted the artifact: %v", err)
	}
	for _, e := range eventsOf(t, f.store, id) {
		if e.Action == EventDeleted {
			t.Errorf("a refused delete recorded %+v", e)
		}
	}
}

// TestDeleteArtifactReadBeforeAuthority: a project admin custodian who
// cannot read the artifact gets the missing artifact's 404, not 403 or
// 204: the read check runs before the authority check, and the authority
// check is never asked.
func TestDeleteArtifactReadBeforeAuthority(t *testing.T) {
	f, id := retireFixture(t)
	blind := principal{PrincipalKindUser, "user-blind-admin", ""}
	f.host.allowAdmin(blind, "project-1")
	missing := response(f.del(blind, uuid.NewString()))
	f.host.calls = nil
	rec := f.del(blind, id)
	if got := response(rec); got != missing {
		t.Fatalf("non-reading custodian: %d; the answer differs from a missing artifact's:\n%s\nwant\n%s", rec.Code, got, missing)
	}
	for _, c := range f.host.calls {
		if c == "administers project-1" {
			t.Errorf("the authority check ran for a caller that cannot read: %v", f.host.calls)
		}
	}
	if _, err := f.store.GetArtifact(context.Background(), id); err != nil {
		t.Fatalf("deleted: %v", err)
	}
}

// TestDeleteArtifactEndsAccess: after a delete the artifact is unreadable
// on every route, its share links answer 404, its message references are
// unavailable, and a second delete is a 404.
func TestDeleteArtifactEndsAccess(t *testing.T) {
	f, id := newLinkFixture(t)
	token, _ := f.mustMintLink(userU, id, `{}`)
	if rec := f.shared(RouteShared+token, ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("link before delete: %d", rec.Code)
	}
	if rec := f.del(userU, id); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	for _, target := range []string{artifactPath(id), artifactPath(id) + "/versions", artifactPath(id) + "/versions/1",
		artifactPath(id) + "/files/doc.md", artifactPath(id) + "/grants", artifactPath(id) + "/links"} {
		if rec := f.do(&userU, http.MethodGet, target, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s after delete: %d", target, rec.Code)
		}
	}
	if rec := f.shared(RouteShared+token, ""); rec.Code != http.StatusNotFound {
		t.Errorf("link after delete: %d", rec.Code)
	}
	views, err := f.svc.ResolveRefs(withPrincipal(httptest.NewRequest(http.MethodGet, "/", nil), userU).Context(),
		[]MessageRef{{ArtifactID: id}})
	if err != nil || len(views) != 1 || views[0].Available {
		t.Errorf("message reference after delete = %+v, %v; want unavailable", views, err)
	}
	if rec := f.del(userU, id); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", rec.Code)
	}
	if got := f.list(&userU, "/api/v1/artifacts?mine=1"); len(got.Artifacts) != 0 {
		t.Errorf("list after delete = %v", listIDs(got))
	}
}

// TestOwnerDeletedInResponses: GET and the list report ownerDeleted for an
// orphaned artifact, and omit it otherwise.
func TestOwnerDeletedInResponses(t *testing.T) {
	f, id := retireFixture(t)
	mine := f.publish(userU, "mine.md", []byte("mine"), "scope=project-1").Artifact.ID
	get := decodeInto[ArtifactResponse](t, f.do(&userU, http.MethodGet, artifactPath(id), nil, nil))
	if !get.Artifact.OwnerDeleted {
		t.Errorf("GET of an orphaned artifact: ownerDeleted false")
	}
	rec := f.do(&userU, http.MethodGet, artifactPath(mine), nil, nil)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var art map[string]any
	if err := json.Unmarshal(raw["artifact"], &art); err != nil {
		t.Fatal(err)
	}
	if _, ok := art["ownerDeleted"]; ok {
		t.Errorf("GET of a live owner's artifact carries ownerDeleted: %s", rec.Body.String())
	}
	got := map[string]bool{}
	for _, a := range f.list(&userU, listPath).Artifacts {
		got[a.ID] = a.OwnerDeleted
	}
	if v, ok := got[id]; !ok || !v {
		t.Errorf("list: orphaned artifact ownerDeleted = %v (listed %v)", v, ok)
	}
	if got[mine] {
		t.Errorf("list: live owner's artifact marked ownerDeleted")
	}
}

// TestOwnerPathsRecordEvents: an owner's expiry change, grant removal and
// link revoke through the HTTP routes each record one event with the
// caller and authority owner; an admin grantee's with admin_grant.
func TestOwnerPathsRecordEvents(t *testing.T) {
	f, id := newLinkFixture(t)
	owner := &Actor{Kind: userU.kind, Ref: userU.ref, Authority: AuthorityOwner}
	exp := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	if rec := f.patch(userU, id, `{"expiresAt": "`+exp+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	onlyEvent(t, f.store, id, EventExpirySet, owner)
	if rec := f.patch(userU, id, `{"expiresAt": null}`); rec.Code != http.StatusOK {
		t.Fatalf("patch clear: %d %s", rec.Code, rec.Body.String())
	}
	onlyEvent(t, f.store, id, EventExpiryCleared, owner)

	rec, _ := f.putGrant(userU, id, SubjectPrincipal, PrincipalRef(adminGrantee.kind, adminGrantee.ref), GrantAdmin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d", rec.Code)
	}
	rec, rg := f.putGrant(userU, id, SubjectPrincipal, PrincipalRef(outside.kind, outside.ref), GrantRead)
	if rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d", rec.Code)
	}
	if rec := f.do(&adminGrantee, http.MethodDelete, grantsPath(id)+"/"+rg.ID, nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete grant: %d %s", rec.Code, rec.Body.String())
	}
	e := onlyEvent(t, f.store, id, EventGrantRemoved, &Actor{Kind: adminGrantee.kind, Ref: adminGrantee.ref, Authority: AuthorityAdminGrant})
	if e.Detail["grantId"] != rg.ID {
		t.Errorf("grant_removed detail = %+v", e.Detail)
	}

	_, link := f.mustMintLink(userU, id, `{}`)
	if rec := f.do(&userU, http.MethodDelete, linksPath(id)+"/"+link.Link.ID, nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	onlyEvent(t, f.store, id, EventLinkRevoked, owner)

	// A move records moved, with the dropped home grant.
	f.host.crossScope = true
	f.host.allow(userU, "project-2", PermissionCreate)
	if rec := f.patch(userU, id, `{"scopeRef": "project-2"}`); rec.Code != http.StatusOK {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}
	e = onlyEvent(t, f.store, id, EventMoved, owner)
	if e.Detail["fromScope"] != "project-1" || e.Detail["toScope"] != "project-2" || e.Detail["droppedGrantId"] == "" {
		t.Errorf("moved detail = %+v", e.Detail)
	}
}
