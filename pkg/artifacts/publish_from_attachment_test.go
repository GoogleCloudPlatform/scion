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
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/google/uuid"
)

// uploadCountingStorage wraps storage.Storage and counts Upload calls so tests can
// prove publish-by-reference copies zero blob bytes.
type uploadCountingStorage struct {
	storage.Storage
	uploads atomic.Int64
}

func (c *uploadCountingStorage) Upload(ctx context.Context, path string, r io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	c.uploads.Add(1)
	return c.Storage.Upload(ctx, path, r, opts)
}

func TestParseAttachmentRef(t *testing.T) {
	id := "5f1c2d3e-0000-4000-8000-000000000001"
	upper := "5F1C2D3E-0000-4000-8000-000000000001"

	if got := FormatAttachmentRef(id); got != "scion://attachment/"+id {
		t.Fatalf("FormatAttachmentRef = %q", got)
	}
	if !IsAttachmentRef("  scion://attachment/" + id) {
		t.Error("IsAttachmentRef should be true for scion://attachment/...")
	}
	if IsAttachmentRef("scion://artifact/" + id) {
		t.Error("IsAttachmentRef should be false for scion://artifact/...")
	}

	got, err := ParseAttachmentRef("scion://attachment/" + id)
	if err != nil || got != id {
		t.Fatalf("ParseAttachmentRef(lower) = %q, %v", got, err)
	}
	got, err = ParseAttachmentRef("  scion://attachment/" + upper + "  ")
	if err != nil || got != id {
		t.Fatalf("ParseAttachmentRef(upper) = %q, %v", got, err)
	}
	for _, bad := range []string{
		"",
		id, // bare UUID is not an attachment ref
		"scion://artifact/" + id,
		"scion://attachment/",
		"scion://attachment/not-a-uuid",
		"scion://attachment/" + id + "@1",
	} {
		if _, err := ParseAttachmentRef(bad); !errors.Is(err, ErrBadAttachmentRef) {
			t.Errorf("ParseAttachmentRef(%q) = %v, want ErrBadAttachmentRef", bad, err)
		}
	}
}

// TestStoreVersionProvenanceAndAppendPublished verifies source_attachment_id,
// source_message_id, CreatePublished key conflict, and AppendPublishedVersion
// on both SQLite and Postgres dialects.
func TestStoreVersionProvenanceAndAppendPublished(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)

		attID := uuid.NewString()
		msgID := "msg-prov-1"
		a := &Artifact{
			ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: "project-1",
			OwnerKind: PrincipalKindAgent, OwnerRef: "agent-a", Key: "k1", Title: "shot.png",
			CurrentSeq: 1, CreatedAt: now, UpdatedAt: now,
		}
		v1 := &Version{
			ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish,
			EntryPath: "shot.png", Note: "v1", TotalBytes: 4, FileCount: 1,
			CreatedByKind: PrincipalKindAgent, CreatedByRef: "agent-a", CreatedAt: now,
			State: VersionStateReady, SourceAttachmentID: attID, SourceMessageID: msgID,
		}
		f1 := File{VersionID: v1.ID, Path: "shot.png", Size: 4, SHA256: sha([]byte("body")), MediaType: "image/png"}
		if err := st.CreatePublished(ctx, a, v1, []File{f1}, nil); err != nil {
			t.Fatalf("CreatePublished: %v", err)
		}

		// Duplicate key returns ErrConflict.
		dup := &Artifact{
			ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: "project-1",
			OwnerKind: PrincipalKindAgent, OwnerRef: "agent-a", Key: "k1", Title: "dup",
			CurrentSeq: 1, CreatedAt: now, UpdatedAt: now,
		}
		dupV := &Version{
			ID: uuid.NewString(), ArtifactID: dup.ID, Seq: 1, Kind: VersionKindPublish,
			EntryPath: "shot.png", TotalBytes: 4, FileCount: 1, CreatedAt: now, State: VersionStateReady,
		}
		if err := st.CreatePublished(ctx, dup, dupV, nil, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("CreatePublished duplicate key = %v, want ErrConflict", err)
		}

		gotV1, err := st.GetVersion(ctx, a.ID, 1)
		if err != nil {
			t.Fatalf("GetVersion(1): %v", err)
		}
		if gotV1.SourceAttachmentID != attID || gotV1.SourceMessageID != msgID {
			t.Errorf("v1 provenance = (%q, %q), want (%q, %q)", gotV1.SourceAttachmentID, gotV1.SourceMessageID, attID, msgID)
		}

		// AppendPublishedVersion assigns seq=2, advances current_seq, preserves provenance.
		attID2 := uuid.NewString()
		msgID2 := "msg-prov-2"
		v2 := &Version{
			ID: uuid.NewString(), ArtifactID: a.ID, Kind: VersionKindPublish,
			EntryPath: "shot2.png", Note: "v2", TotalBytes: 5, FileCount: 1,
			CreatedByKind: PrincipalKindAgent, CreatedByRef: "agent-a", CreatedAt: now.Add(time.Second),
			State: VersionStateReady, SourceAttachmentID: attID2, SourceMessageID: msgID2,
		}
		f2 := File{VersionID: v2.ID, Path: "shot2.png", Size: 5, SHA256: sha([]byte("body2")), MediaType: "image/png"}
		updated, err := st.AppendPublishedVersion(ctx, v2, []File{f2})
		if err != nil {
			t.Fatalf("AppendPublishedVersion: %v", err)
		}
		if v2.Seq != 2 || updated.CurrentSeq != 2 {
			t.Errorf("seq/currentSeq = %d/%d, want 2/2", v2.Seq, updated.CurrentSeq)
		}

		list, err := st.ListVersions(ctx, a.ID, 0, 10)
		if err != nil || len(list) != 2 {
			t.Fatalf("ListVersions = %d, %v", len(list), err)
		}
		if list[0].SourceAttachmentID != attID2 || list[0].SourceMessageID != msgID2 {
			t.Errorf("listed v2 provenance = (%q, %q), want (%q, %q)", list[0].SourceAttachmentID, list[0].SourceMessageID, attID2, msgID2)
		}
		if list[1].SourceAttachmentID != attID || list[1].SourceMessageID != msgID {
			t.Errorf("listed v1 provenance = (%q, %q), want (%q, %q)", list[1].SourceAttachmentID, list[1].SourceMessageID, attID, msgID)
		}

		// AppendPublishedVersion refuses attachment-scoped artifacts.
		att := seedUpload(t, st, "user-1")
		badV := &Version{
			ID: uuid.NewString(), ArtifactID: att.ID, Kind: VersionKindPublish,
			EntryPath: "x.png", TotalBytes: 1, FileCount: 1, CreatedAt: now, State: VersionStateReady,
		}
		if _, err := st.AppendPublishedVersion(ctx, badV, nil); !errors.Is(err, ErrAttachmentNotShareable) {
			t.Errorf("AppendPublishedVersion on attachment = %v, want ErrAttachmentNotShareable", err)
		}
	})
}

func TestPublishFromAttachment_NewArtifactAndBlobReuse(t *testing.T) {
	f := newAttachmentFixture(t, false)
	cs := &uploadCountingStorage{Storage: f.blobs}
	f.blobs = cs
	f.svc.SetBlobStorage(cs, "hub-1")

	body := []byte("png-image-bytes-shared-blob")
	attID := f.send(userU, "msg-42", body)
	f.mh.allowMessage(agentA, "msg-42")
	f.mh.allowMessage(userU, "msg-42")
	// agentB is in project-1 (can read artifacts in project-1), but NOT msg-42.

	uploadsBefore := cs.uploads.Load()
	if uploadsBefore != 1 {
		t.Fatalf("uploads after attachment create = %d, want 1", uploadsBefore)
	}

	// Agent publishes using default home scope (project-1).
	reqBody, _ := json.Marshal(map[string]any{
		"fromAttachment": attID,
		"title":          "Saved Screenshot",
		"note":           "from DM",
	})
	rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts", reqBody, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish from attachment status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp ArtifactResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Artifact.OwnerKind != PrincipalKindAgent || resp.Artifact.OwnerRef != agentA.ref {
		t.Errorf("owner = %s:%s, want agent:%s", resp.Artifact.OwnerKind, resp.Artifact.OwnerRef, agentA.ref)
	}
	if resp.Artifact.ScopeRef != "project-1" || resp.Artifact.Title != "Saved Screenshot" {
		t.Errorf("artifact = %+v", resp.Artifact)
	}
	if resp.Version == nil || resp.Version.Seq != 1 || resp.Version.Note != "from DM" {
		t.Fatalf("version = %+v", resp.Version)
	}
	if resp.Version.SourceAttachmentID != attID || resp.Version.SourceMessageID != "msg-42" {
		t.Errorf("provenance = (%q, %q), want (%q, msg-42)", resp.Version.SourceAttachmentID, resp.Version.SourceMessageID, attID)
	}
	if len(resp.Version.Files) != 1 || resp.Version.Files[0].SHA256 != sha(body) {
		t.Errorf("files = %+v, want sha %s", resp.Version.Files, sha(body))
	}

	// Zero new blob uploads; still exactly 1 blob object in storage.
	if got := cs.uploads.Load(); got != uploadsBefore {
		t.Errorf("blob uploads after publish-by-ref = %d, want %d (zero copy)", got, uploadsBefore)
	}
	listed, err := f.blobs.List(context.Background(), storage.ListOptions{Prefix: "hubs/hub-1/artifacts/blobs/sha256/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Objects) != 1 {
		t.Errorf("blob object count = %d, want 1", len(listed.Objects))
	}

	// Artifact bytes are readable.
	fileRec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+resp.Artifact.ID+"/files/shot.png", nil, nil)
	if fileRec.Code != http.StatusOK || !bytes.Equal(fileRec.Body.Bytes(), body) {
		t.Fatalf("artifact file read = %d %q, want 200 %q", fileRec.Code, fileRec.Body.String(), body)
	}

	// Provenance visibility: userU (reads msg-42) sees sourceMessageId;
	// agentB (reads project-1 artifact, does NOT read msg-42) sees sourceAttachmentId
	// but NOT sourceMessageId.
	getU := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+resp.Artifact.ID, nil, nil)
	var respU ArtifactResponse
	_ = json.Unmarshal(getU.Body.Bytes(), &respU)
	if respU.Version.SourceAttachmentID != attID || respU.Version.SourceMessageID != "msg-42" {
		t.Errorf("userU GET provenance = (%q, %q), want (%q, msg-42)", respU.Version.SourceAttachmentID, respU.Version.SourceMessageID, attID)
	}

	getB := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+resp.Artifact.ID, nil, nil)
	var respB ArtifactResponse
	_ = json.Unmarshal(getB.Body.Bytes(), &respB)
	if respB.Version.SourceAttachmentID != attID || respB.Version.SourceMessageID != "" {
		t.Errorf("agentB GET provenance = (%q, %q), want (%q, \"\")", respB.Version.SourceAttachmentID, respB.Version.SourceMessageID, attID)
	}

	// Attachment itself stays unchanged and downloadable.
	outcome, attRec := f.serve(&userU, "/att", attID)
	if outcome != ServeWritten || attRec.Code != http.StatusOK || !bytes.Equal(attRec.Body.Bytes(), body) {
		t.Fatalf("ServeAttachment after publish = outcome %d status %d", outcome, attRec.Code)
	}

	// User without scopeRef gets 400; user with scopeRef succeeds.
	noScope, _ := json.Marshal(map[string]any{"fromAttachment": attID})
	recNoScope := f.do(&userU, http.MethodPost, "/api/v1/artifacts", noScope, map[string]string{"Content-Type": "application/json"})
	if recNoScope.Code != http.StatusBadRequest {
		t.Errorf("user without scopeRef status = %d, want 400", recNoScope.Code)
	}
	withScope, _ := json.Marshal(map[string]any{"fromAttachment": attID, "scopeRef": "project-1"})
	recWithScope := f.do(&userU, http.MethodPost, "/api/v1/artifacts", withScope, map[string]string{"Content-Type": "application/json"})
	if recWithScope.Code != http.StatusCreated {
		t.Errorf("user with scopeRef status = %d: %s", recWithScope.Code, recWithScope.Body.String())
	}
}

func TestPublishFromAttachment_KeyedAppend(t *testing.T) {
	f := newAttachmentFixture(t, false)
	cs := &uploadCountingStorage{Storage: f.blobs}
	f.blobs = cs
	f.svc.SetBlobStorage(cs, "hub-1")

	b1 := []byte("first-attachment-bytes")
	b2 := []byte("second-attachment-bytes")
	att1 := f.send(userU, "msg-1", b1)
	att2 := f.send(userU, "msg-2", b2)
	f.mh.allowMessage(userU, "msg-1")
	f.mh.allowMessage(userU, "msg-2")

	uploadsBefore := cs.uploads.Load()

	p1Body, _ := json.Marshal(map[string]any{
		"fromAttachment": att1,
		"scopeRef":       "project-1",
		"key":            "screenshots",
		"title":          "Screenshots",
		"note":           "initial",
	})
	rec1 := f.do(&userU, http.MethodPost, "/api/v1/artifacts", p1Body, map[string]string{"Content-Type": "application/json"})
	if rec1.Code != http.StatusCreated {
		t.Fatalf("v1 status = %d: %s", rec1.Code, rec1.Body.String())
	}
	var r1 ArtifactResponse
	_ = json.Unmarshal(rec1.Body.Bytes(), &r1)

	p2Body, _ := json.Marshal(map[string]any{
		"fromAttachment": att2,
		"scopeRef":       "project-1",
		"key":            "screenshots",
		"note":           "updated",
	})
	rec2 := f.do(&userU, http.MethodPost, "/api/v1/artifacts", p2Body, map[string]string{"Content-Type": "application/json"})
	if rec2.Code != http.StatusCreated {
		t.Fatalf("v2 status = %d: %s", rec2.Code, rec2.Body.String())
	}
	var r2 ArtifactResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &r2)

	if r2.Artifact.ID != r1.Artifact.ID {
		t.Fatalf("artifact IDs differ: v1=%s v2=%s", r1.Artifact.ID, r2.Artifact.ID)
	}
	if r2.Artifact.CurrentSeq != 2 || r2.Version.Seq != 2 || r2.Version.Note != "updated" {
		t.Errorf("v2 = %+v", r2.Version)
	}
	if r2.Version.SourceAttachmentID != att2 || r2.Version.SourceMessageID != "msg-2" {
		t.Errorf("v2 provenance = (%q, %q), want (%q, msg-2)", r2.Version.SourceAttachmentID, r2.Version.SourceMessageID, att2)
	}
	if cs.uploads.Load() != uploadsBefore {
		t.Errorf("uploads changed from %d to %d", uploadsBefore, cs.uploads.Load())
	}

	// Both versions readable.
	g1 := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+r1.Artifact.ID+"/versions/1/files/shot.png", nil, nil)
	g2 := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+r1.Artifact.ID+"/versions/2/files/shot.png", nil, nil)
	if !bytes.Equal(g1.Body.Bytes(), b1) || !bytes.Equal(g2.Body.Bytes(), b2) {
		t.Errorf("version bytes mismatch: v1=%q v2=%q", g1.Body.String(), g2.Body.String())
	}
}

func TestPublishFromAttachment_Refusals(t *testing.T) {
	f := newAttachmentFixture(t, false)
	attID := f.send(userU, "msg-secret", []byte("secret-bytes"))
	f.mh.allowMessage(userU, "msg-secret")
	f.mh.allowMessage(outside, "msg-secret") // outside can read msg-secret, but has NO publish rights in project-1

	// 1. Caller who cannot read the message (agentA can publish in project-1,
	//    cannot read msg-secret) gets the exact same 404 body as unknown attachment.
	unknownID := uuid.NewString()
	reqUnknown, _ := json.Marshal(map[string]any{"fromAttachment": unknownID})
	recUnknown := f.do(&agentA, http.MethodPost, "/api/v1/artifacts", reqUnknown, map[string]string{"Content-Type": "application/json"})

	reqUnreadable, _ := json.Marshal(map[string]any{"fromAttachment": attID})
	recUnreadable := f.do(&agentA, http.MethodPost, "/api/v1/artifacts", reqUnreadable, map[string]string{"Content-Type": "application/json"})

	if recUnknown.Code != http.StatusNotFound || recUnreadable.Code != http.StatusNotFound {
		t.Fatalf("status unknown=%d unreadable=%d, want 404/404", recUnknown.Code, recUnreadable.Code)
	}
	if recUnknown.Body.String() != recUnreadable.Body.String() {
		t.Errorf("404 bodies differ:\nunknown:    %s\nunreadable: %s", recUnknown.Body.String(), recUnreadable.Body.String())
	}

	// 2. Reader without publish rights in target scope gets 403.
	reqNoPublish, _ := json.Marshal(map[string]any{"fromAttachment": attID, "scopeRef": "project-1"})
	recNoPublish := f.do(&outside, http.MethodPost, "/api/v1/artifacts", reqNoPublish, map[string]string{"Content-Type": "application/json"})
	if recNoPublish.Code != http.StatusForbidden {
		t.Fatalf("reader without publish rights status = %d: %s, want 403", recNoPublish.Code, recNoPublish.Body.String())
	}

	// 3. Conflicting manifest fields return 400.
	reqConflict, _ := json.Marshal(map[string]any{"fromAttachment": attID, "scopeRef": "project-1", "entry": "a.md"})
	recConflict := f.do(&userU, http.MethodPost, "/api/v1/artifacts", reqConflict, map[string]string{"Content-Type": "application/json"})
	if recConflict.Code != http.StatusBadRequest {
		t.Errorf("conflicting entry status = %d, want 400", recConflict.Code)
	}

	// 4. fromAttachment on POST /api/v1/artifacts/{id}/versions returns 400.
	art := f.publish(userU, "a.md", []byte("hi"), "scope=project-1").Artifact
	recVer := f.do(&userU, http.MethodPost, "/api/v1/artifacts/"+art.ID+"/versions", reqNoPublish, map[string]string{"Content-Type": "application/json"})
	if recVer.Code != http.StatusBadRequest {
		t.Errorf("fromAttachment on /{id}/versions status = %d, want 400", recVer.Code)
	}
}

func TestPublishFromAttachment_MarkdownRemoteImages(t *testing.T) {
	f := newAttachmentFixture(t, false)
	imgURL := "https://images.example/chart.png"
	f.svc.fetcherFactory = func(RemoteImageLimits) ImageFetcher {
		return &fakeFetcher{bodies: map[string][]byte{imgURL: testPNG}}
	}
	f.svc.SetLimits(func(context.Context) Limits {
		return Limits{
			MaxFileBytes:   DefaultMaxFileBytes,
			MaxBundleBytes: DefaultMaxBundleBytes,
			MaxFiles:       DefaultMaxFiles,
			RemoteImages:   DefaultRemoteImageLimits(),
		}
	})

	md := []byte("# Report\n\n![chart](" + imgURL + ")\n")
	attID := f.upload(userU, "report.md", "text/markdown", md)
	if err := f.svc.BindAttachment(context.Background(), attID, userU.kind, userU.ref, "msg-md", ""); err != nil {
		t.Fatal(err)
	}
	f.mh.allowMessage(userU, "msg-md")

	reqBody, _ := json.Marshal(map[string]any{"fromAttachment": attID, "scopeRef": "project-1"})
	rec := f.do(&userU, http.MethodPost, "/api/v1/artifacts", reqBody, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp ArtifactResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Version.Files) != 2 {
		t.Fatalf("files = %+v, want markdown + fetched remote image", resp.Version.Files)
	}
	foundRemote := false
	for _, fi := range resp.Version.Files {
		if fi.Origin == FileOriginRemote && fi.SourceURL == imgURL && fi.FetchStatus == FetchStatusOK {
			foundRemote = true
		}
	}
	if !foundRemote {
		t.Errorf("expected fetched remote image in %+v", resp.Version.Files)
	}
}

func TestPublishFromAttachment_BlobGCSurvivesEitherDeletion(t *testing.T) {
	ctx := context.Background()

	// Case 1: Deleting the message leaves the artifact readable and its blob alive across GC.
	t.Run("delete message keeps artifact alive", func(t *testing.T) {
		f := newAttachmentFixture(t, false)
		body := []byte("shared-bytes-case-1")
		attID := f.send(userU, "msg-del-1", body)
		f.mh.allowMessage(userU, "msg-del-1")

		reqBody, _ := json.Marshal(map[string]any{"fromAttachment": attID, "scopeRef": "project-1"})
		rec := f.do(&userU, http.MethodPost, "/api/v1/artifacts", reqBody, map[string]string{"Content-Type": "application/json"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("publish status = %d: %s", rec.Code, rec.Body.String())
		}
		var resp ArtifactResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		// Delete the message (retire attachment).
		n, err := f.svc.RetireScope(ctx, ScopeKindMessage, "msg-del-1")
		if err != nil || n != 1 {
			t.Fatalf("RetireScope = %d, %v", n, err)
		}

		// Sweep well past grace period.
		g := &BlobSweeper{}
		t0 := time.Now().Add(DefaultGCGrace)
		f.sweepAt(g, t0, DefaultGCGrace)
		if del := f.sweepAt(g, t0.Add(DefaultGCGrace+time.Hour), DefaultGCGrace); del != 0 {
			t.Fatalf("GC deleted %d blobs while artifact still referenced it", del)
		}
		if !f.blobExists(sha(body)) {
			t.Fatal("shared blob missing after message deletion")
		}

		// Artifact is still readable with its bytes.
		fileRec := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+resp.Artifact.ID+"/files/shot.png", nil, nil)
		if fileRec.Code != http.StatusOK || !bytes.Equal(fileRec.Body.Bytes(), body) {
			t.Fatalf("artifact read after message delete = %d %q", fileRec.Code, fileRec.Body.String())
		}

		// Now delete the artifact too -> GC reclaims the blob.
		delRec := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+resp.Artifact.ID, nil, nil)
		if delRec.Code != http.StatusNoContent {
			t.Fatalf("delete artifact status = %d", delRec.Code)
		}
		t1 := t0.Add(2 * DefaultGCGrace)
		f.sweepAt(g, t1, DefaultGCGrace)
		if del := f.sweepAt(g, t1.Add(DefaultGCGrace+time.Hour), DefaultGCGrace); del != 1 {
			t.Fatalf("GC deleted %d blobs after both retired, want 1", del)
		}
		if f.blobExists(sha(body)) {
			t.Fatal("blob still exists after both attachment and artifact retired")
		}
	})

	// Case 2: Retiring the artifact leaves the attachment readable and its blob alive across GC.
	t.Run("retire artifact keeps attachment alive", func(t *testing.T) {
		f := newAttachmentFixture(t, false)
		body := []byte("shared-bytes-case-2")
		attID := f.send(userU, "msg-del-2", body)
		f.mh.allowMessage(userU, "msg-del-2")

		reqBody, _ := json.Marshal(map[string]any{"fromAttachment": attID, "scopeRef": "project-1"})
		rec := f.do(&userU, http.MethodPost, "/api/v1/artifacts", reqBody, map[string]string{"Content-Type": "application/json"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("publish status = %d: %s", rec.Code, rec.Body.String())
		}
		var resp ArtifactResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		// Retire the artifact.
		delRec := f.do(&userU, http.MethodDelete, "/api/v1/artifacts/"+resp.Artifact.ID, nil, nil)
		if delRec.Code != http.StatusNoContent {
			t.Fatalf("delete artifact status = %d", delRec.Code)
		}

		// Sweep well past grace period.
		g := &BlobSweeper{}
		t0 := time.Now().Add(DefaultGCGrace)
		f.sweepAt(g, t0, DefaultGCGrace)
		if del := f.sweepAt(g, t0.Add(DefaultGCGrace+time.Hour), DefaultGCGrace); del != 0 {
			t.Fatalf("GC deleted %d blobs while attachment still referenced it", del)
		}
		if !f.blobExists(sha(body)) {
			t.Fatal("shared blob missing after artifact deletion")
		}

		// Attachment is still readable with its bytes.
		outcome, attRec := f.serve(&userU, "/att", attID)
		if outcome != ServeWritten || attRec.Code != http.StatusOK || !bytes.Equal(attRec.Body.Bytes(), body) {
			t.Fatalf("attachment read after artifact delete = outcome %d status %d %q", outcome, attRec.Code, attRec.Body.String())
		}
	})
}
