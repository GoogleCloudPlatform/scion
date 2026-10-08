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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func scopeGrant(artifactID, scope, perm string) *Grant {
	return &Grant{ID: uuid.NewString(), ArtifactID: artifactID, SubjectKind: SubjectScope, SubjectRef: scope,
		Permission: perm, CreatedByRef: "user:u", CreatedAt: time.Now()}
}

// TestStorePutGrant: insert, update in place, cap, deleted artifact, and
// delete, on both dialects.
func TestStorePutGrant(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		a, _, _, home := seedArtifact(t, st, "")
		g := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal, SubjectRef: "user:x",
			Permission: GrantRead, CreatedAt: time.Now()}
		created, err := st.PutGrant(ctx, g, 3)
		if err != nil || !created {
			t.Fatalf("PutGrant: %v %v", created, err)
		}
		again := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal, SubjectRef: "user:x",
			Permission: GrantAdmin, CreatedAt: time.Now()}
		created, err = st.PutGrant(ctx, again, 3)
		if err != nil || created || again.ID != g.ID {
			t.Fatalf("update: %v %v id %s want %s", created, err, again.ID, g.ID)
		}
		grants, _ := st.ListGrants(ctx, a.ID)
		if len(grants) != 2 {
			t.Fatalf("%d grants", len(grants))
		}
		for _, x := range grants {
			if x.ID == g.ID && x.Permission != GrantAdmin {
				t.Errorf("permission not updated: %+v", x)
			}
		}
		if _, err := st.PutGrant(ctx, scopeGrant(a.ID, "p2", GrantRead), 3); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutGrant(ctx, scopeGrant(a.ID, "p3", GrantRead), 3); !errors.Is(err, ErrTooManyGrants) {
			t.Errorf("over the cap: %v", err)
		}
		// Links do not count toward the cap.
		if err := st.CreateLink(ctx, linkGrant(a.ID, LinkTokenHash("t"), time.Now().Add(time.Hour)), 5, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteGrant(ctx, a.ID, g.ID); err != nil {
			t.Fatalf("DeleteGrant: %v", err)
		}
		if err := st.DeleteGrant(ctx, a.ID, g.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete twice: %v", err)
		}
		if _, err := st.PutGrant(ctx, scopeGrant(a.ID, "p3", GrantRead), 3); err != nil {
			t.Errorf("after a delete: %v", err)
		}
		// DeleteGrant never removes a link.
		lg, _, _ := st.ResolveLink(ctx, LinkTokenHash("t"), time.Now())
		if lg == nil {
			t.Fatal("link missing")
		}
		_, linkRow, _ := st.ResolveLink(ctx, LinkTokenHash("t"), time.Now())
		if err := st.DeleteGrant(ctx, a.ID, linkRow.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeleteGrant on a link: %v", err)
		}
		if err := st.DeleteGrant(ctx, uuid.NewString(), home.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete via another artifact: %v", err)
		}
		// Malformed grants.
		for _, bad := range []*Grant{nil, {ArtifactID: a.ID, SubjectKind: SubjectLink, SubjectRef: "h", Permission: GrantRead},
			{ArtifactID: a.ID, SubjectKind: SubjectScope, Permission: GrantRead},
			{ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: "p", Permission: "owner"}} {
			if _, err := st.PutGrant(ctx, bad, 10); err == nil {
				t.Errorf("PutGrant(%+v) accepted", bad)
			}
		}
		// A deleted artifact takes no grants.
		s := st.(*sqlStore)
		if _, err := db.Exec(s.rebind("UPDATE artifact SET deleted_at = ? WHERE id = ?"), s.timeArg(time.Now()), a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutGrant(ctx, scopeGrant(a.ID, "p9", GrantRead), 10); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleted artifact: %v", err)
		}

		// Concurrent adds never exceed the cap.
		b, _, _, _ := seedArtifact(t, st, "")
		const workers, limit = 8, 4 // the home grant takes one slot
		var wg sync.WaitGroup
		errs := make([]error, workers)
		for i := range workers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				other := NewStore(reopen(), driverOf(st))
				_, errs[i] = other.PutGrant(ctx, scopeGrant(b.ID, uuid.NewString(), GrantRead), limit)
			}(i)
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else if !errors.Is(err, ErrTooManyGrants) {
				t.Errorf("concurrent put: %v", err)
			}
		}
		if ok != limit-1 {
			t.Errorf("%d concurrent puts succeeded, want %d", ok, limit-1)
		}
	})
}

// TestStoreExpiryAndRehome: SetExpiry and Rehome on both dialects.
func TestStoreExpiryAndRehome(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "k")
		exp := time.Now().Add(time.Hour)
		got, err := st.SetExpiry(ctx, a.ID, &exp)
		if err != nil || got.ExpiresAt == nil || !got.ExpiresAt.Equal(dbRounded(exp)) {
			t.Fatalf("SetExpiry: %+v %v", got, err)
		}
		if got, err = st.SetExpiry(ctx, a.ID, nil); err != nil || got.ExpiresAt != nil {
			t.Fatalf("clear: %+v %v", got, err)
		}
		if _, err := st.SetExpiry(ctx, uuid.NewString(), nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetExpiry missing: %v", err)
		}
		moved, err := st.Rehome(ctx, a.ID, scopeGrant(a.ID, "project-2", GrantRead))
		if err != nil || moved.ScopeRef != "project-2" {
			t.Fatalf("Rehome: %+v %v", moved, err)
		}
		grants, _ := st.ListGrants(ctx, a.ID)
		have := map[string]bool{}
		for _, g := range grants {
			have[g.SubjectRef] = true
		}
		if !have["project-1"] || !have["project-2"] || len(grants) != 2 {
			t.Errorf("grants after rehome %+v", grants)
		}
		// Moving back keeps the existing grant rather than adding one.
		if _, err := st.Rehome(ctx, a.ID, scopeGrant(a.ID, "project-1", GrantRead)); err != nil {
			t.Fatal(err)
		}
		if grants, _ := st.ListGrants(ctx, a.ID); len(grants) != 2 {
			t.Errorf("moving back added a grant: %d", len(grants))
		}
		// Key clash with another live artifact of the owner in the target.
		c, _, _, _ := seedArtifactIn(t, st, "k2", "project-3")
		d, _, _, _ := seedArtifactIn(t, st, "k2", "project-4")
		if _, err := st.Rehome(ctx, c.ID, scopeGrant(c.ID, "project-4", GrantRead)); !errors.Is(err, ErrConflict) {
			t.Errorf("key clash: %v", err)
		}
		_ = d
		if _, err := st.Rehome(ctx, c.ID, scopeGrant(c.ID, "project-5", GrantRead)); err != nil {
			t.Errorf("no clash: %v", err)
		}
		if _, err := st.Rehome(ctx, uuid.NewString(), scopeGrant("x", "p", GrantRead)); err == nil {
			t.Errorf("Rehome with a grant of another artifact accepted")
		}
	})
}

// TestStoreSweepExpired: expired artifacts are soft-deleted with their
// grants and links; others stay.
func TestStoreSweepExpired(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		now := time.Now()
		expired, _, _, _ := seedArtifact(t, st, "")
		keep, _, _, _ := seedArtifact(t, st, "")
		forever, _, _, _ := seedArtifact(t, st, "")
		past, future := now.Add(-time.Minute), now.Add(time.Hour)
		if _, err := st.SetExpiry(ctx, expired.ID, &past); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetExpiry(ctx, keep.ID, &future); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateLink(ctx, linkGrant(expired.ID, LinkTokenHash("e"), future), 5, now.Add(-2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		n, err := st.SweepExpired(ctx, now, 10)
		if err != nil || n != 1 {
			t.Fatalf("SweepExpired: %d %v", n, err)
		}
		if _, err := st.GetArtifact(ctx, expired.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("expired artifact still live: %v", err)
		}
		if grants, _ := st.ListGrants(ctx, expired.ID); len(grants) != 0 {
			t.Errorf("expired artifact kept %d grants", len(grants))
		}
		for _, id := range []string{keep.ID, forever.ID} {
			if _, err := st.GetArtifact(ctx, id); err != nil {
				t.Errorf("unexpired artifact swept: %v", err)
			}
		}
		if n, _ := st.SweepExpired(ctx, now, 10); n != 0 {
			t.Errorf("second sweep: %d", n)
		}
		// The limit bounds one pass.
		for range 3 {
			x, _, _, _ := seedArtifact(t, st, "")
			if _, err := st.SetExpiry(ctx, x.ID, &past); err != nil {
				t.Fatal(err)
			}
		}
		if n, _ := st.SweepExpired(ctx, now, 2); n != 2 {
			t.Errorf("limited sweep: %d", n)
		}
		if n, _ := st.SweepExpired(ctx, now, 0); n != 0 {
			t.Errorf("zero limit: %d", n)
		}
	})
}

// seedArtifactIn is seedArtifact homed in scope.
func seedArtifactIn(t *testing.T, st Store, key, scope string) (*Artifact, *Version, File, Grant) {
	t.Helper()
	now := time.Now()
	a := &Artifact{ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: scope, OwnerKind: PrincipalKindAgent,
		OwnerRef: "agent-1", Key: key, Title: "T", CreatedAt: now, UpdatedAt: now}
	v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: "a.md",
		TotalBytes: 1, FileCount: 1, CreatedAt: now, State: VersionStateReady}
	f := File{VersionID: v.ID, Path: "a.md", Size: 1, SHA256: sha([]byte(a.ID)), MediaType: "text/markdown"}
	g := Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: scope, Permission: GrantRead, CreatedAt: now}
	if err := st.CreatePublished(context.Background(), a, v, []File{f}, []Grant{g}); err != nil {
		t.Fatal(err)
	}
	return a, v, f, g
}

// TestStoreSweepRechecksExpiry: an artifact whose expiry moved after it
// was found is not deleted.
func TestStoreSweepRechecksExpiry(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		future := time.Now().Add(time.Hour)
		if _, err := st.SetExpiry(ctx, a.ID, &future); err != nil {
			t.Fatal(err)
		}
		s := st.(*sqlStore)
		ok, err := s.sweepOne(ctx, a.ID, s.timeArg(time.Now()))
		if err != nil || ok {
			t.Fatalf("sweepOne on an unexpired artifact: %v %v", ok, err)
		}
		if _, err := st.GetArtifact(ctx, a.ID); err != nil {
			t.Errorf("unexpired artifact deleted: %v", err)
		}
	})
}

// TestStoreCandidatesSharedWithScope: HomeScope keeps rows homed in the
// scope or shared with it; SharedOnly keeps the shared ones.
func TestStoreCandidatesSharedWithScope(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		home, _, _, _ := seedArtifactIn(t, st, "", "p1")
		away, _, _, _ := seedArtifactIn(t, st, "", "p2")
		alone, _, _, _ := seedArtifactIn(t, st, "", "p3")
		if _, err := st.PutGrant(ctx, scopeGrant(away.ID, "p1", GrantRead), 10); err != nil {
			t.Fatal(err)
		}
		_ = alone
		q := CandidateQuery{PrincipalKind: PrincipalKindAgent, PrincipalRef: "agent-1", HomeScope: "p1", Limit: 10, Now: time.Now()}
		ids := func(q CandidateQuery) map[string]bool {
			rows, err := st.ListCandidates(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]bool{}
			for _, r := range rows {
				out[r.ID] = true
			}
			return out
		}
		if got := ids(q); len(got) != 2 || !got[home.ID] || !got[away.ID] {
			t.Errorf("HomeScope: %v", got)
		}
		q.SharedOnly = true
		if got := ids(q); len(got) != 1 || !got[away.ID] {
			t.Errorf("SharedOnly: %v", got)
		}
	})
}
