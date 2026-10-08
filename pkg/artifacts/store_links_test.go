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

func linkGrant(artifactID, hash string, exp time.Time) *Grant {
	return &Grant{
		ID: uuid.NewString(), ArtifactID: artifactID, SubjectKind: SubjectLink, SubjectRef: hash,
		Permission: GrantRead, ExpiresAt: &exp, CreatedByRef: "user:u", CreatedAt: time.Now(),
	}
}

// TestStoreLinks: create, resolve, check, revoke, on both dialects.
func TestStoreLinks(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		now := time.Now()
		g := linkGrant(a.ID, LinkTokenHash("tok"), now.Add(time.Hour))
		if err := st.CreateLink(ctx, g, 5, now); err != nil {
			t.Fatalf("CreateLink: %v", err)
		}
		ra, rg, err := st.ResolveLink(ctx, LinkTokenHash("tok"), now)
		if err != nil {
			t.Fatalf("ResolveLink: %v", err)
		}
		if ra.ID != a.ID || ra.CurrentSeq != 1 || ra.ScopeRef != a.ScopeRef || rg.ID != g.ID ||
			rg.SubjectRef != g.SubjectRef || rg.ExpiresAt == nil || !rg.ExpiresAt.Equal(dbRounded(*g.ExpiresAt)) ||
			rg.CreatedByRef != "user:u" || rg.SubjectKind != SubjectLink || rg.ArtifactID != a.ID {
			t.Errorf("resolved %+v %+v", ra, rg)
		}
		if _, _, err := st.ResolveLink(ctx, LinkTokenHash("other"), now); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown hash: %v", err)
		}
		// Expired at the instant asked about.
		if _, _, err := st.ResolveLink(ctx, LinkTokenHash("tok"), now.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
			t.Errorf("at expiry: %v", err)
		}
		if ok, err := st.LinkActive(ctx, a.ID, g.ID, now); err != nil || !ok {
			t.Errorf("LinkActive: %v %v", ok, err)
		}
		if ok, _ := st.LinkActive(ctx, a.ID, g.ID, now.Add(time.Hour)); ok {
			t.Errorf("LinkActive at expiry")
		}
		if ok, _ := st.LinkActive(ctx, uuid.NewString(), g.ID, now); ok {
			t.Errorf("LinkActive for another artifact")
		}
		// A hash names one link across the hub.
		b, _, _, _ := seedArtifact(t, st, "")
		if err := st.CreateLink(ctx, linkGrant(b.ID, LinkTokenHash("tok"), now.Add(time.Hour)), 5, now); err == nil {
			t.Errorf("a second link with the same hash was accepted")
		}
		if err := st.RevokeLink(ctx, b.ID, g.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoke via another artifact: %v", err)
		}
		if err := st.RevokeLink(ctx, a.ID, g.ID); err != nil {
			t.Fatalf("RevokeLink: %v", err)
		}
		if err := st.RevokeLink(ctx, a.ID, g.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoke twice: %v", err)
		}
		if _, _, err := st.ResolveLink(ctx, LinkTokenHash("tok"), now); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoked: %v", err)
		}
		if ok, _ := st.LinkActive(ctx, a.ID, g.ID, now); ok {
			t.Errorf("LinkActive after revoke")
		}
		// Malformed grants are refused before the database.
		for _, bad := range []*Grant{nil, {ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: "x", Permission: GrantRead, ExpiresAt: &now},
			{ArtifactID: a.ID, SubjectKind: SubjectLink, SubjectRef: "x", Permission: GrantRead},
			{ArtifactID: a.ID, SubjectKind: SubjectLink, SubjectRef: "x", Permission: GrantAdmin, ExpiresAt: &now},
			{ArtifactID: a.ID, SubjectKind: SubjectLink, Permission: GrantRead, ExpiresAt: &now}} {
			if err := st.CreateLink(ctx, bad, 5, now); err == nil {
				t.Errorf("CreateLink(%+v) accepted", bad)
			}
		}
	})
}

// TestStoreResolveLinkArtifactState: the artifact must be live,
// unexpired and published for its link to resolve.
func TestStoreResolveLinkArtifactState(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		s := st.(*sqlStore)
		now := time.Now()
		past := s.timeArg(now.Add(-time.Minute))
		for _, tc := range []struct {
			name, sql string
			arg       any
		}{
			{"deleted", "UPDATE artifact SET deleted_at = ? WHERE id = ?", past},
			{"expired", "UPDATE artifact SET expires_at = ? WHERE id = ?", past},
			{"unpublished", "UPDATE artifact SET current_seq = ? WHERE id = ?", nil},
		} {
			a, _, _, _ := seedArtifact(t, st, "")
			hash := LinkTokenHash(tc.name)
			if err := st.CreateLink(ctx, linkGrant(a.ID, hash, now.Add(time.Hour)), 5, now); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.ResolveLink(ctx, hash, now); err != nil {
				t.Fatalf("%s: before: %v", tc.name, err)
			}
			if _, err := db.Exec(s.rebind(tc.sql), tc.arg, a.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.ResolveLink(ctx, hash, now); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s artifact: %v, want ErrNotFound", tc.name, err)
			}
		}
		// A link cannot be added to a deleted artifact.
		a, _, _, _ := seedArtifact(t, st, "")
		if _, err := db.Exec(s.rebind("UPDATE artifact SET deleted_at = ? WHERE id = ?"), s.timeArg(now), a.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateLink(ctx, linkGrant(a.ID, LinkTokenHash("d"), now.Add(time.Hour)), 5, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("link on a deleted artifact: %v", err)
		}
	})
}

// TestStoreLinkCap: the cap counts unexpired links, expired ones are
// dropped, and concurrent creates never exceed it.
func TestStoreLinkCap(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		now := time.Now()
		short := linkGrant(a.ID, LinkTokenHash("short"), now.Add(time.Minute))
		if err := st.CreateLink(ctx, short, 2, now); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateLink(ctx, linkGrant(a.ID, LinkTokenHash("long"), now.Add(time.Hour)), 2, now); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateLink(ctx, linkGrant(a.ID, LinkTokenHash("third"), now.Add(time.Hour)), 2, now); !errors.Is(err, ErrTooManyLinks) {
			t.Fatalf("over the cap: %v", err)
		}
		later := now.Add(2 * time.Minute)
		if err := st.CreateLink(ctx, linkGrant(a.ID, LinkTokenHash("third"), later.Add(time.Hour)), 2, later); err != nil {
			t.Fatalf("after one expired: %v", err)
		}
		grants, err := st.ListGrants(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range grants {
			if g.ID == short.ID {
				t.Errorf("the expired link was kept")
			}
		}
		// Other grant kinds are neither counted nor dropped.
		if len(grants) != 3 {
			t.Errorf("%d grants, want the scope grant and two links", len(grants))
		}

		b, _, _, _ := seedArtifact(t, st, "")
		const workers, limit = 8, 3
		var wg sync.WaitGroup
		errs := make([]error, workers)
		for i := range workers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				other := NewStore(reopen(), driverOf(st))
				errs[i] = other.CreateLink(ctx, linkGrant(b.ID, LinkTokenHash(uuid.NewString()), now.Add(time.Hour)), limit, now)
			}(i)
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrTooManyLinks):
			default:
				t.Errorf("concurrent create: %v", err)
			}
		}
		if ok != limit {
			t.Errorf("%d concurrent creates succeeded, want %d", ok, limit)
		}
	})
}
