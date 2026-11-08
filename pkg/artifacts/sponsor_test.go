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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// sponsorHost is fakeHost plus SponsorRecorder: it answers DelegatingUser
// from a table of agent id to user id, and records every question.
type sponsorHost struct {
	*fakeHost
	smu     sync.Mutex
	users   map[string]string
	err     error
	queries []string
}

func (h *sponsorHost) DelegatingUser(_ context.Context, agentID string) (string, error) {
	h.smu.Lock()
	defer h.smu.Unlock()
	h.queries = append(h.queries, agentID)
	if h.err != nil {
		return "", h.err
	}
	return h.users[agentID], nil
}

func (h *sponsorHost) setUser(agentID, userID string) {
	h.smu.Lock()
	defer h.smu.Unlock()
	h.users[agentID] = userID
}

// withSponsorHost replaces the fixture's service with one over a
// sponsorHost wrapping the fixture's fake host, and returns that host.
func withSponsorHost(f *fixture) *sponsorHost {
	h := &sponsorHost{fakeHost: f.host, users: map[string]string{}}
	svc := NewService(h)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	f.svc = svc
	return h
}

func (f *fixture) sponsorOf(id string) string {
	f.t.Helper()
	a, err := f.store.GetArtifact(context.Background(), id)
	if err != nil {
		f.t.Fatalf("GetArtifact %s: %v", id, err)
	}
	return a.SponsorRef
}

// sponsorColumn reads artifact.sponsor_ref directly, distinguishing NULL,
// on a SQLite or (pgx) Postgres database.
func sponsorColumn(t *testing.T, db *sql.DB, id string) sql.NullString {
	t.Helper()
	q := `SELECT sponsor_ref FROM artifact WHERE id = ?`
	if strings.Contains(fmt.Sprintf("%T", db.Driver()), "stdlib") {
		q = `SELECT sponsor_ref FROM artifact WHERE id = $1`
	}
	var v sql.NullString
	if err := db.QueryRow(q, id).Scan(&v); err != nil {
		t.Fatalf("read sponsor_ref of %s: %v", id, err)
	}
	return v
}

// TestSponsorRecordedOnAgentCreate: both creation routes record the
// host's DelegatingUser for an agent creating in a project scope, asked
// for that agent (ptone/scion#6028).
func TestSponsorRecordedOnAgentCreate(t *testing.T) {
	f := newFixture(t, false)
	h := withSponsorHost(f)
	h.setUser(agentA.ref, "user-delegator")

	pub := f.publish(agentA, "a.md", []byte("# a"), "")
	if got := f.sponsorOf(pub.Artifact.ID); got != "user-delegator" {
		t.Errorf("single-file publish recorded sponsor %q, want user-delegator", got)
	}
	pend := f.createPending(agentA, "/api/v1/artifacts", site.manifest("index.html"))
	if got := f.sponsorOf(pend.Artifact.ID); got != "user-delegator" {
		t.Errorf("manifest create recorded sponsor %q, want user-delegator", got)
	}
	h.smu.Lock()
	defer h.smu.Unlock()
	if len(h.queries) != 2 || h.queries[0] != agentA.ref || h.queries[1] != agentA.ref {
		t.Errorf("DelegatingUser asked %v, want the publishing agent twice", h.queries)
	}
}

// TestSponsorNotRecorded: nothing is recorded (NULL) for a user creating
// an artifact, for an agent's attachment (upload scope), for a host
// without SponsorRecorder, for a recorder that names no one, and for a
// recorder lookup failure, which does not fail the publish.
func TestSponsorNotRecorded(t *testing.T) {
	t.Run("user owner", func(t *testing.T) {
		f := newFixture(t, false)
		h := withSponsorHost(f)
		h.setUser(userU.ref, "someone")
		pub := f.publish(userU, "u.md", []byte("u"), "scope=project-1")
		if v := sponsorColumn(t, f.db, pub.Artifact.ID); v.Valid {
			t.Errorf("user-owned artifact recorded sponsor %q", v.String)
		}
		h.smu.Lock()
		defer h.smu.Unlock()
		if len(h.queries) != 0 {
			t.Errorf("DelegatingUser asked %v for a user publish", h.queries)
		}
	})
	t.Run("agent attachment", func(t *testing.T) {
		f := newFixture(t, false)
		h := withSponsorHost(f)
		h.setUser(agentA.ref, "user-delegator")
		info, err := f.svc.CreateAttachment(ctxAs(agentA), "a.txt", "text/plain", bytes.NewReader([]byte("x")))
		if err != nil {
			t.Fatal(err)
		}
		if v := sponsorColumn(t, f.db, info.ID); v.Valid {
			t.Errorf("attachment recorded sponsor %q", v.String)
		}
	})
	t.Run("host without recorder", func(t *testing.T) {
		f := newFixture(t, false)
		pub := f.publish(agentA, "a.md", []byte("a"), "")
		if v := sponsorColumn(t, f.db, pub.Artifact.ID); v.Valid {
			t.Errorf("recorded sponsor %q without a recorder", v.String)
		}
	})
	t.Run("no delegating user", func(t *testing.T) {
		f := newFixture(t, false)
		withSponsorHost(f)
		pub := f.publish(agentA, "a.md", []byte("a"), "")
		if v := sponsorColumn(t, f.db, pub.Artifact.ID); v.Valid {
			t.Errorf("recorded sponsor %q when none resolved", v.String)
		}
	})
	t.Run("lookup failure", func(t *testing.T) {
		f := newFixture(t, false)
		h := withSponsorHost(f)
		h.err = errors.New("store down")
		pub := f.publish(agentA, "a.md", []byte("a"), "")
		if v := sponsorColumn(t, f.db, pub.Artifact.ID); v.Valid {
			t.Errorf("recorded sponsor %q after a lookup failure", v.String)
		}
	})
}

// TestSponsorRequestCannotSupply pins the single write path at the
// service boundary: a create request that names a sponsor, in any channel
// the routes read, records only what the host's recorder answers, and
// nothing without a recorder. A manifest body field is refused outright
// by the strict manifest decoder.
func TestSponsorRequestCannotSupply(t *testing.T) {
	forged := "user-forged"
	query := "sponsor_ref=" + forged + "&sponsorRef=" + forged + "&sponsor=" + forged
	headers := map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Sponsor":    forged, "X-Sponsor-Ref": forged, "Sponsor-Ref": forged,
	}
	for _, recorder := range []bool{true, false} {
		name := "no recorder"
		want := sql.NullString{}
		if recorder {
			name, want = "recorder", sql.NullString{String: "user-delegator", Valid: true}
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, false)
			if recorder {
				withSponsorHost(f).setUser(agentA.ref, "user-delegator")
			}
			// Single-file publish: query and headers.
			rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=a.md&"+query, []byte("a"), headers)
			if rec.Code != http.StatusCreated {
				t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
			}
			pub := decodeInto[ArtifactResponse](t, rec)
			if got := sponsorColumn(t, f.db, pub.Artifact.ID); got != want {
				t.Errorf("publish with a forged sponsor recorded %+v, want %+v", got, want)
			}
			// Manifest create: query and headers.
			rec = f.do(&agentA, http.MethodPost, "/api/v1/artifacts?"+query, mustJSON(t, site.manifest("index.html")),
				map[string]string{"Content-Type": "application/json", "X-Sponsor": forged, "X-Sponsor-Ref": forged, "Sponsor-Ref": forged})
			if rec.Code != http.StatusCreated {
				t.Fatalf("manifest create: %d %s", rec.Code, rec.Body.String())
			}
			pend := decodeInto[PendingVersionResponse](t, rec)
			if got := sponsorColumn(t, f.db, pend.Artifact.ID); got != want {
				t.Errorf("manifest create with a forged sponsor recorded %+v, want %+v", got, want)
			}
			// Manifest create: a body field is refused, and nothing is created.
			var before int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM artifact`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"sponsorRef", "sponsor_ref", "sponsor"} {
				body := []byte(`{"entry":"a.md","files":[{"path":"a.md","size":1,"sha256":"` + sha([]byte("a")) + `"}],"` + field + `":"` + forged + `"}`)
				rec = f.do(&agentA, http.MethodPost, "/api/v1/artifacts", body, map[string]string{"Content-Type": "application/json"})
				if rec.Code != http.StatusBadRequest {
					t.Errorf("manifest with body field %s: %d, want 400", field, rec.Code)
				}
			}
			var after int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM artifact`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Errorf("refused manifests created %d artifacts", after-before)
			}
			var forgedRows int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM artifact WHERE sponsor_ref = ?`, forged).Scan(&forgedRows); err != nil {
				t.Fatal(err)
			}
			if forgedRows != 0 {
				t.Errorf("%d artifacts carry the forged sponsor", forgedRows)
			}
		})
	}
}

// TestSponsorCreationValueStands: appending versions never changes the
// recorded sponsor, even when the agent's delegating user has changed
// since (the creation-time value stands).
func TestSponsorCreationValueStands(t *testing.T) {
	f := newFixture(t, false)
	h := withSponsorHost(f)
	h.setUser(agentA.ref, "user-first")
	pub := f.publish(agentA, "a.md", []byte("a"), "key=k1")
	h.setUser(agentA.ref, "user-second")
	// The owner appends by key (single-file publish) and through the
	// versions route.
	f.publish(agentA, "a.md", []byte("a2"), "key=k1")
	f.createPending(agentA, "/api/v1/artifacts/"+pub.Artifact.ID+"/versions", CreateVersionRequest{
		Entry: "b.md", Files: []ManifestFile{{Path: "b.md", Size: 1, SHA256: sha([]byte("b"))}}})
	if got := f.sponsorOf(pub.Artifact.ID); got != "user-first" {
		t.Errorf("sponsor after appends = %q, want user-first", got)
	}
}

// TestStoreSponsorRoundTrip: the store writes Artifact.SponsorRef on
// create and reads it back on GetArtifact and ListCandidates; "" is
// stored as NULL.
func TestStoreSponsorRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		now := time.Now()
		mk := func(sponsor string) *Artifact {
			a := &Artifact{ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: "project-1", OwnerKind: PrincipalKindAgent,
				OwnerRef: "agent-1", Title: "T", CreatedAt: now, UpdatedAt: now, SponsorRef: sponsor}
			v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: "a.md",
				TotalBytes: 1, FileCount: 1, CreatedAt: now, State: VersionStateReady}
			fl := File{VersionID: v.ID, Path: "a.md", Size: 1, SHA256: sha([]byte(a.ID)), MediaType: "text/markdown"}
			if err := st.CreatePublished(ctx, a, v, []File{fl}, nil); err != nil {
				t.Fatal(err)
			}
			return a
		}
		with, without := mk("user-9"), mk("")
		for _, c := range []struct {
			a    *Artifact
			want string
		}{{with, "user-9"}, {without, ""}} {
			got, err := st.GetArtifact(ctx, c.a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.SponsorRef != c.want {
				t.Errorf("GetArtifact SponsorRef = %q, want %q", got.SponsorRef, c.want)
			}
		}
		var nulls int
		if err := db.QueryRow(`SELECT COUNT(*) FROM artifact WHERE sponsor_ref IS NULL`).Scan(&nulls); err != nil {
			t.Fatal(err)
		}
		if nulls != 1 {
			t.Errorf("%d rows with NULL sponsor_ref, want 1", nulls)
		}
		cands, err := st.ListCandidates(ctx, CandidateQuery{PrincipalKind: PrincipalKindAgent, PrincipalRef: "agent-1", Limit: 10, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]string{}
		for _, c := range cands {
			seen[c.ID] = c.SponsorRef
		}
		if len(seen) != 2 || seen[with.ID] != "user-9" || seen[without.ID] != "" {
			t.Errorf("ListCandidates sponsors = %v", seen)
		}
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSponsorForOnlyAgentsInProjectScope pins sponsorFor's own conditions,
// which the routes cannot reach on their own: only an agent creating in a
// project scope is looked up.
func TestSponsorForOnlyAgentsInProjectScope(t *testing.T) {
	f := newFixture(t, false)
	h := withSponsorHost(f)
	h.setUser(agentA.ref, "user-delegator")
	h.setUser(userU.ref, "user-wrong")
	ctx := context.Background()
	for _, c := range []struct {
		name, kind, ref, scopeKind, want string
	}{
		{"agent, project scope", PrincipalKindAgent, agentA.ref, ScopeKindProject, "user-delegator"},
		{"agent, upload scope", PrincipalKindAgent, agentA.ref, ScopeKindUpload, ""},
		{"agent, message scope", PrincipalKindAgent, agentA.ref, ScopeKindMessage, ""},
		{"user, project scope", PrincipalKindUser, userU.ref, ScopeKindProject, ""},
		{"agent without id", PrincipalKindAgent, "", ScopeKindProject, ""},
	} {
		if got := f.svc.sponsorFor(ctx, c.kind, c.ref, c.scopeKind); got != c.want {
			t.Errorf("%s: sponsorFor = %q, want %q", c.name, got, c.want)
		}
	}
	h.smu.Lock()
	defer h.smu.Unlock()
	if len(h.queries) != 1 || h.queries[0] != agentA.ref {
		t.Errorf("DelegatingUser asked %v, want only the agent in a project scope", h.queries)
	}
}
