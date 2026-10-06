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
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// listSeed describes one artifact for the candidate-query tests.
type listSeed struct {
	title, key  string
	ownerKind   string
	ownerRef    string
	scope       string
	updated     time.Time
	kind        string // current version kind; "" = publish
	expires     *time.Time
	deleted     bool
	grants      []Grant // ArtifactID filled in
}

// seedList writes s and returns its id.
func seedList(t *testing.T, db *sql.DB, st Store, s listSeed) string {
	t.Helper()
	ctx := context.Background()
	if s.ownerKind == "" {
		s.ownerKind = PrincipalKindUser
	}
	if s.kind == "" {
		s.kind = VersionKindPublish
	}
	a := &Artifact{ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: s.scope,
		OwnerKind: s.ownerKind, OwnerRef: s.ownerRef, Key: s.key, Title: s.title, ExpiresAt: s.expires,
		CreatedAt: s.updated, UpdatedAt: s.updated}
	v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: s.kind, EntryPath: "a.md",
		TotalBytes: 1, FileCount: 1, CreatedAt: s.updated, State: VersionStateReady}
	grants := []Grant{{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: s.scope,
		Permission: GrantRead, CreatedAt: s.updated}}
	for _, g := range s.grants {
		g.ID, g.ArtifactID, g.CreatedAt = uuid.NewString(), a.ID, s.updated
		grants = append(grants, g)
	}
	if err := st.CreatePublished(ctx, a, v, nil, grants); err != nil {
		t.Fatalf("seed %q: %v", s.title, err)
	}
	ss := st.(*sqlStore)
	if s.deleted {
		if _, err := db.Exec(ss.rebind("UPDATE artifact SET deleted_at = ? WHERE id = ?"), ss.timeArg(s.updated), a.ID); err != nil {
			t.Fatal(err)
		}
	}
	return a.ID
}

func candidateTitles(t *testing.T, st Store, q CandidateQuery) []string {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Now.IsZero() {
		q.Now = time.Now()
	}
	rows, err := st.ListCandidates(context.Background(), q)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}

func TestStoreListCandidates(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
		past, future := at(-100), at(10000)
		alice := PrincipalRef(PrincipalKindUser, "alice")

		seedList(t, db, st, listSeed{title: "own", key: "own-key", ownerRef: "alice", scope: "p-other", updated: at(9)})
		seedList(t, db, st, listSeed{title: "granted", ownerRef: "bob", scope: "p-bob", updated: at(8),
			grants: []Grant{{SubjectKind: SubjectPrincipal, SubjectRef: alice, Permission: GrantRead}}})
		seedList(t, db, st, listSeed{title: "member project", ownerKind: PrincipalKindAgent, ownerRef: "agent-1", scope: "p-alice", updated: at(7), kind: VersionKindReview})
		seedList(t, db, st, listSeed{title: "someone else", ownerRef: "bob", scope: "p-bob", updated: at(6)})
		seedList(t, db, st, listSeed{title: "expired grant", ownerRef: "bob", scope: "p-bob", updated: at(5),
			grants: []Grant{{SubjectKind: SubjectPrincipal, SubjectRef: alice, Permission: GrantRead, ExpiresAt: &past}}})
		seedList(t, db, st, listSeed{title: "link only", ownerRef: "bob", scope: "p-bob", updated: at(4),
			grants: []Grant{{SubjectKind: SubjectLink, SubjectRef: alice, Permission: GrantRead}}})
		seedList(t, db, st, listSeed{title: "expired artifact", ownerRef: "alice", scope: "p-alice", updated: at(3), expires: &past})
		seedList(t, db, st, listSeed{title: "deleted", ownerRef: "alice", scope: "p-alice", updated: at(2), deleted: true})
		seedList(t, db, st, listSeed{title: "future expiry", ownerRef: "alice", scope: "p-x", updated: at(1), expires: &future,
			key: "100%_done"})
		seedList(t, db, st, listSeed{title: "same user id, agent kind", ownerKind: PrincipalKindAgent, ownerRef: "alice", scope: "p-x", updated: at(0)})

		q := CandidateQuery{PrincipalKind: PrincipalKindUser, PrincipalRef: "alice", ScopeRefs: []string{"p-alice"}}
		want := []string{"own", "granted", "member project", "future expiry"}
		if got := candidateTitles(t, st, q); !slices.Equal(got, want) {
			t.Errorf("candidates = %q, want %q", got, want)
		}

		noScopes := q
		noScopes.ScopeRefs = nil
		if got, want := candidateTitles(t, st, noScopes), []string{"own", "granted", "future expiry"}; !slices.Equal(got, want) {
			t.Errorf("without scopes = %q, want %q", got, want)
		}

		owned := q
		owned.OwnedOnly = true
		if got, want := candidateTitles(t, st, owned), []string{"own", "future expiry"}; !slices.Equal(got, want) {
			t.Errorf("owned only = %q, want %q", got, want)
		}

		review := q
		review.ReviewPending = true
		if got, want := candidateTitles(t, st, review), []string{"member project"}; !slices.Equal(got, want) {
			t.Errorf("review pending = %q, want %q", got, want)
		}

		for search, want := range map[string][]string{
			"GRANT":   {"granted"},       // title, case-insensitive
			"own-k":   {"own"},           // key
			"%":       {"future expiry"}, // literal, not a wildcard
			"_":       {"future expiry"},
			"0%_d":    {"future expiry"},
			"nothing": {},
		} {
			sq := q
			sq.Search = search
			if got := candidateTitles(t, st, sq); !slices.Equal(got, want) {
				t.Errorf("search %q = %q, want %q", search, got, want)
			}
		}
	})
}

func TestStoreListCandidatesPaging(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		// Two rows share a timestamp, so the id breaks the tie.
		var all []string
		for i, h := range []int{5, 4, 4, 3, 2, 1} {
			id := seedList(t, db, st, listSeed{title: string(rune('a' + i)), ownerRef: "alice", scope: "p",
				updated: base.Add(time.Duration(h) * time.Hour)})
			all = append(all, id)
		}
		q := CandidateQuery{PrincipalKind: PrincipalKindUser, PrincipalRef: "alice", Limit: 2, Now: time.Now()}
		var walked []string
		for range 10 {
			rows, err := st.ListCandidates(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				walked = append(walked, r.ID)
			}
			if len(rows) < q.Limit {
				break
			}
			last := rows[len(rows)-1]
			q.After = &Position{UpdatedAt: last.UpdatedAt, ID: last.ID}
		}
		if len(walked) != len(all) {
			t.Fatalf("walked %d rows, want %d", len(walked), len(all))
		}
		seen := map[string]bool{}
		for i, id := range walked {
			if seen[id] {
				t.Fatalf("row %s returned twice", id)
			}
			seen[id] = true
			if i > 0 {
				// Order: updated desc, then id desc.
				prev, cur := walked[i-1], id
				pa, _ := st.GetArtifact(context.Background(), prev)
				ca, _ := st.GetArtifact(context.Background(), cur)
				if ca.UpdatedAt.After(pa.UpdatedAt) || (ca.UpdatedAt.Equal(pa.UpdatedAt) && cur > prev) {
					t.Errorf("rows %d and %d out of order", i-1, i)
				}
			}
		}
	})
}

func TestStoreListCandidatesValidates(t *testing.T) {
	st := NewStore(nil, "sqlite")
	ctx := context.Background()
	if _, err := st.ListCandidates(ctx, CandidateQuery{Limit: 1}); err == nil {
		t.Error("no principal: want error")
	}
	if _, err := st.ListCandidates(ctx, CandidateQuery{PrincipalKind: "user", PrincipalRef: "a"}); err == nil {
		t.Error("no limit: want error")
	}
	if _, err := st.ListCandidates(ctx, CandidateQuery{PrincipalKind: "user", PrincipalRef: "a", Limit: 1,
		ScopeRefs: make([]string, maxCandidateScopes+1)}); err == nil {
		t.Error("too many scopes: want error")
	}
}
