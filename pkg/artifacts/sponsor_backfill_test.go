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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// backfillRow seeds one artifact row for the sponsor backfill tests.
type backfillRow struct {
	scopeKind, ownerKind, ownerRef, sponsor string
	deleted                                 bool
}

func seedBackfillRow(t *testing.T, db *sql.DB, st Store, r backfillRow) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	scopeRef := "project-1"
	if r.scopeKind == ScopeKindUpload {
		scopeRef = PrincipalRef(r.ownerKind, r.ownerRef)
	}
	a := &Artifact{ID: uuid.NewString(), ScopeKind: r.scopeKind, ScopeRef: scopeRef, OwnerKind: r.ownerKind,
		OwnerRef: r.ownerRef, Title: "T", CreatedAt: now, UpdatedAt: now, SponsorRef: r.sponsor}
	v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: "a.md",
		TotalBytes: 1, FileCount: 1, CreatedAt: now, State: VersionStateReady}
	fl := File{VersionID: v.ID, Path: "a.md", Size: 1, SHA256: sha([]byte(a.ID)), MediaType: "text/markdown"}
	if err := st.CreatePublished(ctx, a, v, []File{fl}, nil); err != nil {
		t.Fatal(err)
	}
	if r.deleted {
		if _, err := st.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: &retentionActor, Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	return a.ID
}

// countingResolver answers from a table and counts the questions per agent.
type countingResolver struct {
	mu    sync.Mutex
	users map[string]string
	errs  map[string]error
	asked map[string]int
}

func newCountingResolver(users map[string]string) *countingResolver {
	return &countingResolver{users: users, errs: map[string]error{}, asked: map[string]int{}}
}

func (r *countingResolver) resolve(_ context.Context, agentID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked[agentID]++
	if err := r.errs[agentID]; err != nil {
		return "", err
	}
	return r.users[agentID], nil
}

func (r *countingResolver) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.asked {
		n += c
	}
	return n
}

// TestBackfillSponsors: the backfill records the resolved delegating user
// on the live, project-scope artifacts of agents with none recorded, and
// on nothing else; it asks once per agent, pages, records the ledger name
// only when complete, and then does nothing.
func TestBackfillSponsors(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		ids := map[string]string{}
		seed := func(name string, r backfillRow) { ids[name] = seedBackfillRow(t, db, st, r) }
		// Five agents with unrecorded artifacts, so a batch of 2 pages three
		// times; agent-c has two artifacts.
		for _, a := range []string{"agent-a", "agent-b", "agent-c", "agent-d", "agent-e"} {
			seed(a, backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: a})
		}
		seed("agent-c second", backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-c"})
		// agent-a also has a recorded artifact (never overwritten) and a
		// deleted unrecorded one (never touched).
		seed("agent-a recorded", backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-a", sponsor: "user-old"})
		seed("agent-a deleted", backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-a", deleted: true})
		// Not candidates: a user-owned artifact, an agent's attachment, and
		// the deleted artifact of an agent with nothing live.
		seed("user-owned", backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindUser, ownerRef: "user-x"})
		seed("upload", backfillRow{scopeKind: ScopeKindUpload, ownerKind: PrincipalKindAgent, ownerRef: "agent-a"})
		seed("deleted only", backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-z", deleted: true})

		res := newCountingResolver(map[string]string{
			"agent-a": "user-a", "agent-b": "user-b", "agent-c": "user-c", "agent-e": "user-e",
			// agent-d resolves to no one; the others must never be asked.
			"user-x": "user-wrong", "agent-z": "user-wrong",
		})
		got, err := BackfillSponsors(ctx, st, res.resolve, 2, time.Now())
		if err != nil {
			t.Fatalf("BackfillSponsors: %v", err)
		}
		if !got.Done || got.AlreadyDone || got.Agents != 5 || got.Resolved != 4 || got.Artifacts != 5 {
			t.Errorf("result = %+v, want Done, 5 agents, 4 resolved, 5 artifacts", got)
		}
		want := map[string]sql.NullString{
			"agent-a":          {String: "user-a", Valid: true},
			"agent-b":          {String: "user-b", Valid: true},
			"agent-c":          {String: "user-c", Valid: true},
			"agent-c second":   {String: "user-c", Valid: true},
			"agent-d":          {},
			"agent-e":          {String: "user-e", Valid: true},
			"agent-a recorded": {String: "user-old", Valid: true},
			"agent-a deleted":  {},
			"user-owned":       {},
			"upload":           {},
			"deleted only":     {},
		}
		for name, w := range want {
			if g := sponsorColumn(t, db, ids[name]); g != w {
				t.Errorf("%s: sponsor_ref = %+v, want %+v", name, g, w)
			}
		}
		for agent, n := range res.asked {
			if n != 1 {
				t.Errorf("resolver asked %d times for %s, want once", n, agent)
			}
		}
		for _, never := range []string{"user-x", "agent-z"} {
			if res.asked[never] != 0 {
				t.Errorf("resolver asked for %s, which owns no candidate", never)
			}
		}
		done, err := st.SponsorBackfillDone(ctx)
		if err != nil || !done {
			t.Fatalf("SponsorBackfillDone = %v, %v; want true", done, err)
		}
		// A second call reads the ledger only.
		before := res.total()
		again, err := BackfillSponsors(ctx, st, res.resolve, 2, time.Now())
		if err != nil || !again.AlreadyDone || again.Done || again.Agents != 0 {
			t.Errorf("second call = %+v, %v; want AlreadyDone only", again, err)
		}
		if res.total() != before {
			t.Errorf("second call asked the resolver")
		}
	})
}

// TestBackfillSponsorsStopsOnError: a resolver error stops the pass with
// no ledger row, keeps what it recorded, and the next call resumes and
// completes.
func TestBackfillSponsorsStopsOnError(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		first := seedBackfillRow(t, db, st, backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-1"})
		second := seedBackfillRow(t, db, st, backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-2"})
		fault := errors.New("lookup fault")
		res := newCountingResolver(map[string]string{"agent-1": "user-1", "agent-2": "user-2"})
		res.errs["agent-2"] = fault

		got, err := BackfillSponsors(ctx, st, res.resolve, 10, time.Now())
		if !errors.Is(err, fault) {
			t.Fatalf("err = %v, want the resolver's", err)
		}
		if got.Done {
			t.Errorf("Done after an error")
		}
		if done, err := st.SponsorBackfillDone(ctx); err != nil || done {
			t.Fatalf("ledger recorded after an error: %v, %v", done, err)
		}
		if g := sponsorColumn(t, db, first); g.String != "user-1" {
			t.Errorf("progress before the error lost: %+v", g)
		}
		if g := sponsorColumn(t, db, second); g.Valid {
			t.Errorf("failed agent recorded %+v", g)
		}

		delete(res.errs, "agent-2")
		got, err = BackfillSponsors(ctx, st, res.resolve, 10, time.Now())
		if err != nil || !got.Done {
			t.Fatalf("retry = %+v, %v", got, err)
		}
		if g := sponsorColumn(t, db, second); g.String != "user-2" {
			t.Errorf("retry did not record agent-2: %+v", g)
		}
		if res.asked["agent-1"] != 1 {
			t.Errorf("agent-1 asked %d times; its recorded artifacts are no longer candidates", res.asked["agent-1"])
		}
	})
}

// TestSetAgentSponsorScope pins SetAgentSponsor's filter: only that
// agent's live, project-scope, unrecorded artifacts.
func TestSetAgentSponsorScope(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		target := seedBackfillRow(t, db, st, backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-1"})
		others := []string{
			seedBackfillRow(t, db, st, backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-1", deleted: true}),
			seedBackfillRow(t, db, st, backfillRow{scopeKind: ScopeKindUpload, ownerKind: PrincipalKindAgent, ownerRef: "agent-1"}),
			seedBackfillRow(t, db, st, backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindUser, ownerRef: "agent-1"}),
			seedBackfillRow(t, db, st, backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-2"}),
		}
		recorded := seedBackfillRow(t, db, st, backfillRow{scopeKind: ScopeKindProject, ownerKind: PrincipalKindAgent, ownerRef: "agent-1", sponsor: "user-old"})
		n, err := st.SetAgentSponsor(ctx, "agent-1", "user-1")
		if err != nil || n != 1 {
			t.Fatalf("SetAgentSponsor = %d, %v; want 1", n, err)
		}
		if g := sponsorColumn(t, db, target); g.String != "user-1" {
			t.Errorf("target = %+v", g)
		}
		for i, id := range others {
			if g := sponsorColumn(t, db, id); g.Valid {
				t.Errorf("non-candidate %d recorded %+v", i, g)
			}
		}
		if g := sponsorColumn(t, db, recorded); g.String != "user-old" {
			t.Errorf("recorded sponsor overwritten: %+v", g)
		}
		if _, err := st.SetAgentSponsor(ctx, "", "user-1"); err == nil {
			t.Errorf("SetAgentSponsor accepted an empty agent")
		}
		if _, err := st.SetAgentSponsor(ctx, "agent-1", ""); err == nil {
			t.Errorf("SetAgentSponsor accepted an empty user")
		}
	})
}

// TestSponsorBackfillLedgerName: the backfill's ledger name is not a DDL
// migration, Init never records it, and recording it twice is fine.
func TestSponsorBackfillLedgerName(t *testing.T) {
	for _, m := range migrations {
		if m.name == migrationSponsorBackfill {
			t.Fatalf("DDL migration %q reuses the sponsor backfill's ledger name", m.name)
		}
	}
	names := make([]string, 0, len(migrations))
	for _, m := range migrations {
		names = append(names, m.name)
	}
	if !slices.IsSorted(names) {
		t.Errorf("migration names out of order: %v", names)
	}
	// Compare the numeric prefixes: a DDL step numbered 0012 under any
	// name would reuse the backfill's number.
	if last := names[len(names)-1]; last[:4] >= migrationSponsorBackfill[:4] {
		t.Errorf("latest DDL migration %q is at or past the backfill's number %s; the next DDL is 0013", last, migrationSponsorBackfill[:4])
	}
	for _, n := range names {
		if n[:4] == migrationSponsorBackfill[:4] {
			t.Errorf("DDL migration %q uses the backfill's number %s", n, migrationSponsorBackfill[:4])
		}
	}
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		if done, err := st.SponsorBackfillDone(ctx); err != nil || done {
			t.Fatalf("after Init: SponsorBackfillDone = %v, %v; want false", done, err)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM artifact_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != len(migrations) {
			t.Errorf("Init recorded %d ledger rows, want %d", n, len(migrations))
		}
		for i := 0; i < 2; i++ {
			if err := st.MarkSponsorBackfillDone(ctx, time.Now()); err != nil {
				t.Fatalf("MarkSponsorBackfillDone #%d: %v", i+1, err)
			}
		}
		if done, err := st.SponsorBackfillDone(ctx); err != nil || !done {
			t.Fatalf("after marking: %v, %v", done, err)
		}
		// Init on a database where the backfill is done leaves it done.
		if err := st.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if done, _ := st.SponsorBackfillDone(ctx); !done {
			t.Errorf("Init cleared the backfill's ledger row")
		}
	})
}

// TestBackfillSponsorsArguments: the backfill refuses a missing store or
// resolver and a non-positive batch.
func TestBackfillSponsorsArguments(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		r := newCountingResolver(nil)
		for name, call := range map[string]func() error{
			"no store":    func() error { _, err := BackfillSponsors(ctx, nil, r.resolve, 1, time.Now()); return err },
			"no resolver": func() error { _, err := BackfillSponsors(ctx, st, nil, 1, time.Now()); return err },
			"zero batch":  func() error { _, err := BackfillSponsors(ctx, st, r.resolve, 0, time.Now()); return err },
		} {
			if err := call(); err == nil {
				t.Errorf("%s: no error", name)
			}
		}
		if _, err := st.ListUnsponsoredAgents(ctx, "", 0); err == nil {
			t.Errorf("ListUnsponsoredAgents accepted limit 0")
		}
	})
}
