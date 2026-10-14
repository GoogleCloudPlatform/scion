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

// seedAttachment writes an attachment artifact owned by kind:ref in scope
// (scopeKind, scopeRef), created at created, with one ready file.
func seedAttachment(t *testing.T, st Store, kind, ref, scopeKind, scopeRef string, created time.Time) *Artifact {
	t.Helper()
	a := &Artifact{ID: uuid.NewString(), ScopeKind: scopeKind, ScopeRef: scopeRef, OwnerKind: kind,
		OwnerRef: ref, Title: "shot.png", CreatedAt: created, UpdatedAt: created}
	v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindAttachment, EntryPath: "shot.png",
		TotalBytes: 3, FileCount: 1, CreatedByKind: kind, CreatedByRef: ref, CreatedAt: created, State: VersionStateReady}
	f := File{VersionID: v.ID, Path: "shot.png", Size: 3, SHA256: sha([]byte(a.ID)), MediaType: "image/png"}
	if err := st.CreatePublished(context.Background(), a, v, []File{f}, nil); err != nil {
		t.Fatal(err)
	}
	return a
}

// seedUpload writes an unsent upload of user ref, created now.
func seedUpload(t *testing.T, st Store, ref string) *Artifact {
	t.Helper()
	return seedAttachment(t, st, PrincipalKindUser, ref, ScopeKindUpload, PrincipalRef(PrincipalKindUser, ref), time.Now())
}

// TestStoreMigratesAttachments: a database at 0008 is upgraded by Init to
// the attachment columns; legacy_attachment_id is unique where set, the
// version source columns accept values, existing rows keep theirs, and
// Init stays idempotent.
func TestStoreMigratesAttachments(t *testing.T) {
	if i := migrationIndex(migrationAttachments); i < 1 || migrations[i-1].name != migrationVersionFailReason {
		t.Fatalf("migration %q is not right after %q", migrationAttachments, migrationVersionFailReason)
	}
	if got := migrationAttachments; got != "0009_attachments" {
		t.Fatalf("migrationAttachments = %q, want 0009_attachments", got)
	}
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
				if m.name == migrationAttachments {
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
			ctx := context.Background()
			existing := seedOwned(t, st, PrincipalKindUser, "user-old")
			if err := st.Init(ctx); err != nil {
				t.Fatalf("Init over 0008: %v", err)
			}
			got, err := st.GetArtifact(ctx, existing.ID)
			if err != nil || got.ScopeKind != ScopeKindProject || got.OwnerRef != "user-old" {
				t.Fatalf("existing row after migration = %+v, %v", got, err)
			}
			a1 := seedUpload(t, st, "user-1")
			a2 := seedUpload(t, st, "user-1")
			if _, err := db.Exec(st.rebind(`UPDATE artifact SET legacy_attachment_id = ? WHERE id = ?`), "legacy-1", a1.ID); err != nil {
				t.Fatalf("set legacy id: %v", err)
			}
			if _, err := db.Exec(st.rebind(`UPDATE artifact SET legacy_attachment_id = ? WHERE id = ?`), "legacy-1", a2.ID); err == nil {
				t.Error("a second row took the same legacy_attachment_id")
			}
			if _, err := db.Exec(st.rebind(`UPDATE artifact_version SET source_attachment_id = ?, source_message_id = ? WHERE artifact_id = ?`),
				a1.ID, "msg-1", a1.ID); err != nil {
				t.Errorf("version source columns: %v", err)
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

// TestStoreBindAttachment: an upload binds to a message for its uploader
// only, once; the row then is in the message scope.
func TestStoreBindAttachment(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		up := seedUpload(t, st, "user-1")

		if err := st.BindAttachment(ctx, up.ID, PrincipalKindUser, "user-2", "msg-1", time.Now()); !errors.Is(err, ErrAttachmentNotOwned) {
			t.Errorf("bind by another user = %v, want ErrAttachmentNotOwned", err)
		}
		if err := st.BindAttachment(ctx, up.ID, PrincipalKindAgent, "user-1", "msg-1", time.Now()); !errors.Is(err, ErrAttachmentNotOwned) {
			t.Errorf("bind by a principal of another kind = %v, want ErrAttachmentNotOwned", err)
		}
		got, err := st.GetAttachment(ctx, up.ID)
		if err != nil || got.ScopeKind != ScopeKindUpload {
			t.Fatalf("after refused binds: %+v, %v; want still an upload", got, err)
		}

		if err := st.BindAttachment(ctx, up.ID, PrincipalKindUser, "user-1", "msg-1", time.Now()); err != nil {
			t.Fatalf("bind by the uploader: %v", err)
		}
		got, err = st.GetAttachment(ctx, up.ID)
		if err != nil || got.ScopeKind != ScopeKindMessage || got.ScopeRef != "msg-1" || got.OwnerRef != "user-1" {
			t.Fatalf("bound row = %+v, %v; want message:msg-1 owned by user-1", got, err)
		}

		if err := st.BindAttachment(ctx, up.ID, PrincipalKindUser, "user-1", "msg-2", time.Now()); !errors.Is(err, ErrAttachmentBound) {
			t.Errorf("second bind = %v, want ErrAttachmentBound", err)
		}
		if got, _ := st.GetAttachment(ctx, up.ID); got == nil || got.ScopeRef != "msg-1" {
			t.Errorf("after the second bind the row is %+v; want still message msg-1", got)
		}

		if err := st.BindAttachment(ctx, uuid.NewString(), PrincipalKindUser, "user-1", "msg-1", time.Now()); !errors.Is(err, ErrNotFound) {
			t.Errorf("bind of an unknown id = %v, want ErrNotFound", err)
		}
		project := seedOwned(t, st, PrincipalKindUser, "user-1")
		if err := st.BindAttachment(ctx, project.ID, PrincipalKindUser, "user-1", "msg-1", time.Now()); !errors.Is(err, ErrNotFound) {
			t.Errorf("bind of a project artifact = %v, want ErrNotFound", err)
		}
		if got, err := st.GetArtifact(ctx, project.ID); err != nil || got.ScopeKind != ScopeKindProject {
			t.Errorf("project artifact after a bind attempt = %+v, %v", got, err)
		}
	})
}

// TestStoreBindAttachmentConcurrent: two sends of one upload, on separate
// connections, bind it exactly once.
func TestStoreBindAttachmentConcurrent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		up := seedUpload(t, st, "user-1")
		driver := "sqlite"
		if st.(*sqlStore).dialect == dialectPostgres {
			driver = "postgres"
		}
		stores := []Store{st, NewStore(reopen(), driver)}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range stores {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = stores[i].BindAttachment(ctx, up.ID, PrincipalKindUser, "user-1", []string{"msg-a", "msg-b"}[i], time.Now())
			}(i)
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, ErrAttachmentBound):
				t.Errorf("losing bind = %v, want ErrAttachmentBound", err)
			}
		}
		if won != 1 {
			t.Fatalf("binds that succeeded = %d (%v), want 1", won, errs)
		}
		got, err := st.GetAttachment(ctx, up.ID)
		if err != nil || got.ScopeKind != ScopeKindMessage || (got.ScopeRef != "msg-a" && got.ScopeRef != "msg-b") {
			t.Errorf("bound row = %+v, %v", got, err)
		}
	})
}

// TestStoreAttachmentRowsHidden: attachment rows are found only by
// GetAttachment (by id or legacy id); GetArtifact, the list candidates and
// share-link resolution never return them.
func TestStoreAttachmentRowsHidden(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		s := st.(*sqlStore)
		up := seedUpload(t, st, "user-1")
		sent := seedAttachment(t, st, PrincipalKindUser, "user-1", ScopeKindMessage, "msg-1", time.Now())
		visible := seedOwned(t, st, PrincipalKindUser, "user-1")

		for _, a := range []*Artifact{up, sent} {
			if _, err := st.GetArtifact(ctx, a.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetArtifact(%s %s) = %v, want ErrNotFound", a.ScopeKind, a.ID, err)
			}
			if got, err := st.GetAttachment(ctx, a.ID); err != nil || got.ID != a.ID {
				t.Errorf("GetAttachment(%s) = %+v, %v", a.ID, got, err)
			}
		}
		if _, err := st.GetAttachment(ctx, visible.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetAttachment of a project artifact = %v, want ErrNotFound", err)
		}
		if _, err := db.Exec(s.rebind(`UPDATE artifact SET legacy_attachment_id = ? WHERE id = ?`), "legacy-7", sent.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := st.GetAttachment(ctx, "legacy-7"); err != nil || got.ID != sent.ID {
			t.Errorf("GetAttachment by legacy id = %+v, %v; want %s", got, err, sent.ID)
		}

		rows, err := st.ListCandidates(ctx, CandidateQuery{PrincipalKind: PrincipalKindUser, PrincipalRef: "user-1",
			Limit: 50, Now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != visible.ID {
			ids := []string{}
			for _, r := range rows {
				ids = append(ids, r.ScopeKind+":"+r.ID)
			}
			t.Errorf("ListCandidates = %v; want only the project artifact %s", ids, visible.ID)
		}

		// A link row written behind the store's back still resolves to
		// nothing.
		exp := time.Now().Add(time.Hour)
		if _, err := db.Exec(s.rebind(`INSERT INTO artifact_grant
			(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			uuid.NewString(), sent.ID, SubjectLink, "hash-1", GrantRead, s.timeArg(exp), s.timeArg(time.Now())); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.ResolveLink(ctx, "hash-1", time.Now()); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveLink to an attachment = %v, want ErrNotFound", err)
		}
	})
}

// TestStoreAttachmentRefusals: every store write that would share, version
// or change an attachment refuses with ErrAttachmentNotShareable under the
// lock, and writes nothing.
func TestStoreAttachmentRefusals(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		now := time.Now()
		for _, a := range []*Artifact{
			seedUpload(t, st, "user-1"),
			seedAttachment(t, st, PrincipalKindUser, "user-1", ScopeKindMessage, "msg-1", now),
		} {
			principalGrant := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal,
				SubjectRef: "user:user-2", Permission: GrantRead, CreatedAt: now}
			if _, err := st.PutGrant(ctx, principalGrant, 10, true); !errors.Is(err, ErrAttachmentNotShareable) {
				t.Errorf("%s: PutGrant = %v, want ErrAttachmentNotShareable", a.ScopeKind, err)
			}
			review := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal,
				SubjectRef: "user:user-2", Permission: GrantReview, CreatedAt: now}
			if _, err := st.PutReviewGrant(ctx, review, 10, a.ScopeRef); !errors.Is(err, ErrAttachmentNotShareable) {
				t.Errorf("%s: PutReviewGrant = %v, want ErrAttachmentNotShareable", a.ScopeKind, err)
			}
			exp := now.Add(time.Hour)
			link := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectLink, SubjectRef: "hash-" + a.ID,
				Permission: GrantRead, ExpiresAt: &exp, CreatedAt: now}
			if err := st.CreateLink(ctx, link, 10, now); !errors.Is(err, ErrAttachmentNotShareable) {
				t.Errorf("%s: CreateLink = %v, want ErrAttachmentNotShareable", a.ScopeKind, err)
			}
			v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Kind: VersionKindPublish, EntryPath: "x.md",
				TotalBytes: 1, FileCount: 1, CreatedAt: now, State: VersionStatePending}
			if err := st.CreateVersion(ctx, v, []File{{VersionID: v.ID, Path: "x.md", Size: 1, SHA256: sha([]byte("x")),
				MediaType: "text/markdown", Pending: true}}, 5); !errors.Is(err, ErrAttachmentNotShareable) {
				t.Errorf("%s: CreateVersion = %v, want ErrAttachmentNotShareable", a.ScopeKind, err)
			}
			if _, err := st.SetExpiry(ctx, a.ID, &exp, testActor); !errors.Is(err, ErrAttachmentNotShareable) {
				t.Errorf("%s: SetExpiry = %v, want ErrAttachmentNotShareable", a.ScopeKind, err)
			}
			home := &Grant{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: "project-9",
				Permission: GrantRead, CreatedAt: now}
			if _, err := st.UpdateArtifact(ctx, a.ID, ArtifactUpdate{HomeGrant: home, MaxGrants: 10, Actor: testActor}); !errors.Is(err, ErrAttachmentNotShareable) {
				t.Errorf("%s: move = %v, want ErrAttachmentNotShareable", a.ScopeKind, err)
			}
			grants, err := st.ListGrants(ctx, a.ID)
			if err != nil || len(grants) != 0 {
				t.Errorf("%s: grants after refusals = %+v, %v; want none", a.ScopeKind, grants, err)
			}
			if _, err := st.GetVersion(ctx, a.ID, 2); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: a second version was written (%v)", a.ScopeKind, err)
			}
			got, err := st.GetAttachment(ctx, a.ID)
			if err != nil || got.ExpiresAt != nil || got.ScopeKind != a.ScopeKind || got.ScopeRef != a.ScopeRef {
				t.Errorf("%s: row after refusals = %+v, %v; want unchanged", a.ScopeKind, got, err)
			}
		}
	})
}

// TestStoreRetireAttachments: RetireScope retires the attachments of one
// message, RetireStaleUploads only uploads created before the cutoff, and
// ListLiveScopeRefs pages the live message refs; retired rows count as
// deleted, with their event.
func TestStoreRetireAttachments(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		now := time.Now()
		m1a := seedAttachment(t, st, PrincipalKindUser, "user-1", ScopeKindMessage, "msg-1", now)
		m1b := seedAttachment(t, st, PrincipalKindUser, "user-1", ScopeKindMessage, "msg-1", now)
		m2 := seedAttachment(t, st, PrincipalKindUser, "user-1", ScopeKindMessage, "msg-2", now.Add(-48*time.Hour))
		oldUpload := seedAttachment(t, st, PrincipalKindUser, "user-1", ScopeKindUpload, "user:user-1", now.Add(-25*time.Hour))
		freshUpload := seedAttachment(t, st, PrincipalKindUser, "user-1", ScopeKindUpload, "user:user-1", now.Add(-time.Hour))
		project := seedOwned(t, st, PrincipalKindUser, "user-1")

		refs, err := st.ListLiveScopeRefs(ctx, ScopeKindMessage, "", 1)
		if err != nil || len(refs) != 1 || refs[0] != "msg-1" {
			t.Fatalf("first page = %v, %v; want [msg-1]", refs, err)
		}
		refs, err = st.ListLiveScopeRefs(ctx, ScopeKindMessage, "msg-1", 10)
		if err != nil || len(refs) != 1 || refs[0] != "msg-2" {
			t.Fatalf("second page = %v, %v; want [msg-2]", refs, err)
		}

		n, err := st.RetireScope(ctx, ScopeKindMessage, "msg-1", &messageGoneActor, 10)
		if err != nil || n != 2 {
			t.Fatalf("RetireScope(msg-1) = %d, %v; want 2", n, err)
		}
		for _, a := range []*Artifact{m1a, m1b} {
			if _, err := st.GetAttachment(ctx, a.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("retired attachment %s still found (%v)", a.ID, err)
			}
			onlyEvent(t, st, a.ID, EventDeleted, &messageGoneActor)
		}
		if _, err := st.GetAttachment(ctx, m2.ID); err != nil {
			t.Errorf("the attachment of another message was retired: %v", err)
		}
		if n, err := st.RetireScope(ctx, ScopeKindMessage, "msg-1", &messageGoneActor, 10); err != nil || n != 0 {
			t.Errorf("RetireScope again = %d, %v; want 0", n, err)
		}

		n, err = st.RetireStaleUploads(ctx, now.Add(-UploadTTL), 10)
		if err != nil || n != 1 {
			t.Fatalf("RetireStaleUploads = %d, %v; want 1", n, err)
		}
		if _, err := st.GetAttachment(ctx, oldUpload.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("the old upload was not retired (%v)", err)
		}
		onlyEvent(t, st, oldUpload.ID, EventDeleted, &unsentActor)
		for _, a := range []*Artifact{freshUpload, m2} {
			if _, err := st.GetAttachment(ctx, a.ID); err != nil {
				t.Errorf("%s %s was retired by the upload sweep: %v", a.ScopeKind, a.ID, err)
			}
		}
		if _, err := st.GetArtifact(ctx, project.ID); err != nil {
			t.Errorf("the project artifact was retired: %v", err)
		}
	})
}

// TestStoreListMessageAttachments: the live attachments of each message,
// with their one file; retired ones and uploads are absent.
func TestStoreListMessageAttachments(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		now := time.Now()
		a := seedAttachment(t, st, PrincipalKindUser, "user-1", ScopeKindMessage, "msg-1", now)
		gone := seedAttachment(t, st, PrincipalKindUser, "user-1", ScopeKindMessage, "msg-2", now)
		seedUpload(t, st, "user-1")
		if _, err := st.RetireScope(ctx, ScopeKindMessage, "msg-2", &messageGoneActor, 10); err != nil {
			t.Fatal(err)
		}
		got, err := st.ListMessageAttachments(ctx, []string{"msg-1", "msg-2", "msg-3"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || len(got["msg-1"]) != 1 {
			t.Fatalf("ListMessageAttachments = %+v; want one attachment of msg-1 (retired %s)", got, gone.ID)
		}
		row := got["msg-1"][0]
		if row.Artifact.ID != a.ID || row.File.Path != "shot.png" || row.File.MediaType != "image/png" || row.File.Size != 3 ||
			row.File.SHA256 != sha([]byte(a.ID)) {
			t.Errorf("row = %+v", row)
		}
	})
}

// TestStoreReconcilerSkipsAttachments: the owner liveness reconciler never
// counts or marks attachment rows: their message is their custodian.
func TestStoreReconcilerSkipsAttachments(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		up := seedUpload(t, st, "user-gone")
		sent := seedAttachment(t, st, PrincipalKindUser, "user-gone", ScopeKindMessage, "msg-1", time.Now())
		owners, err := st.ListLiveOwners(ctx, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(owners) != 0 {
			t.Errorf("ListLiveOwners = %+v; want none (attachments only)", owners)
		}
		if n, err := st.CountLiveOwners(ctx, nil); err != nil || n != 0 {
			t.Errorf("CountLiveOwners = %d, %v; want 0", n, err)
		}
		owned := seedOwned(t, st, PrincipalKindUser, "user-gone")
		n, err := st.SetOwnerOrphaned(ctx, Owner{Kind: PrincipalKindUser, Ref: "user-gone"}, true, time.Now(), 10)
		if err != nil || n != 1 {
			t.Fatalf("SetOwnerOrphaned = %d, %v; want 1 (the project artifact only)", n, err)
		}
		for _, a := range []*Artifact{up, sent} {
			got, err := st.GetAttachment(ctx, a.ID)
			if err != nil || got.OrphanedAt != nil {
				t.Errorf("attachment %s = %+v, %v; want unmarked", a.ID, got, err)
			}
		}
		if got, err := st.GetArtifact(ctx, owned.ID); err != nil || got.OrphanedAt == nil {
			t.Errorf("project artifact = %+v, %v; want marked", got, err)
		}
	})
}
